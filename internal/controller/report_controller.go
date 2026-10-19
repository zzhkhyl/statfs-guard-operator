// Package controller implements the statfs-guard reconcile loop.
//
// For every in-scope CephFS PersistentVolume the operator keeps one
// CephFSStatfsReport holding two numbers side by side:
//
//	TrueBytes     ceph.dir.rbytes  — what the MDS says is in the subvolume
//	ReportedBytes statfs(2)        — what kubelet turns into
//	                                 kubelet_volume_stats_used_bytes
//
// When they disagree the monitoring stack is blind to that volume's real
// occupancy, and the volume can fill up without a single alert firing. The
// operator records the episode, asks the node agent to revalidate the kernel
// client's cached inode, and verifies on the next pass that kubelet's figure
// caught up.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/zzhkhyl/statfs-guard-operator/api/v1alpha1"
	"github.com/zzhkhyl/statfs-guard-operator/internal/agent"
	"github.com/zzhkhyl/statfs-guard-operator/internal/kube"
	"github.com/zzhkhyl/statfs-guard-operator/internal/metrics"
)

// PodPVCIndex is the field index name used to find the pods that mount a claim.
const PodPVCIndex = "spec.volumes.pvc"

// PodPVCIndexFunc indexes a pod by "<namespace>/<claimName>" for every PVC it
// mounts, which is how the existing dashboards correlate pods with volumes.
func PodPVCIndexFunc(raw client.Object) []string {
	pod, ok := raw.(*corev1.Pod)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName == "" {
			continue
		}
		keys = append(keys, pod.Namespace+"/"+v.PersistentVolumeClaim.ClaimName)
	}
	return keys
}

// Reconciler keeps the reports in sync with cluster state.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Log      *slog.Logger
	Agents   *kube.AgentClient

	// DriftMinBytes and DriftMinRatio define what counts as drift. The
	// effective tolerance is the larger of the two, so a small volume is not
	// flagged over a rounding-sized gap and a huge volume is not flagged over
	// a few stray bytes.
	DriftMinBytes int64
	DriftMinRatio float64

	// AutoHeal issues the revalidating read automatically. On by default:
	// the read is harmless, it never touches data.
	AutoHeal bool

	// ResyncInterval is how often healthy volumes are re-checked.
	ResyncInterval time.Duration

	// VerifyInterval is how quickly a volume with an open episode is
	// re-checked, so convergence can be confirmed.
	VerifyInterval time.Duration

	// EventCooldown throttles the drift warning event. A volume that swings in
	// and out of an episode every cycle would otherwise spam the event stream.
	EventCooldown time.Duration

	// StorageClasses restricts the operator to these classes. Empty means all.
	StorageClasses map[string]bool

	// ExcludeNamespaces skips claims in these namespaces.
	ExcludeNamespaces map[string]bool

	// APIReader reads straight from the API server, bypassing the informer
	// cache. initialSweep must use it: the manager starts runnables
	// concurrently, and a cache-backed List issued before the PV informer has
	// served its first list returns an empty result with no error
	// (WaitForCacheSync only waits for informers that have already started).
	// That made the sweep log "cephfs_pvs: 0" on a cluster with 20 PVs, and
	// would have skipped every pre-existing volume had the PV watch not
	// replayed them as Add events.
	APIReader client.Reader
}

// SetupWithManager registers the reconciler, its watches and an initial sweep
// so volumes that already existed at install time are picked up.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.applyDefaults()
	r.APIReader = mgr.GetAPIReader()

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, PodPVCIndex, PodPVCIndexFunc); err != nil {
		return fmt.Errorf("index pods by claim: %w", err)
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.CephFSStatfsReport{}).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(pvToRequest)).
		Named("statfsguard").
		Complete(r); err != nil {
		return err
	}

	return mgr.Add(leaderOnly{r.initialSweep})
}

// leaderOnly runs a startup function on the elected leader only.
//
// manager.RunnableFunc does not implement manager.LeaderElectionRunnable, so
// controller-runtime starts it on every replica. With replicas: 1 that is
// invisible; at 2 it means two concurrent initial sweeps.
type leaderOnly struct{ fn func(context.Context) error }

func (l leaderOnly) Start(ctx context.Context) error { return l.fn(ctx) }

func (leaderOnly) NeedLeaderElection() bool { return true }

func (r *Reconciler) applyDefaults() {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.DriftMinBytes <= 0 {
		r.DriftMinBytes = 64 << 20 // 64 MiB
	}
	if r.DriftMinRatio <= 0 {
		r.DriftMinRatio = 0.05
	}
	if r.ResyncInterval <= 0 {
		r.ResyncInterval = 5 * time.Minute
	}
	if r.VerifyInterval <= 0 {
		r.VerifyInterval = 60 * time.Second
	}
	if r.EventCooldown <= 0 {
		r.EventCooldown = 30 * time.Minute
	}
}

func pvToRequest(_ context.Context, obj client.Object) []reconcile.Request {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok || !kube.IsCephFSVolume(pv) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: pv.Name}}}
}

// initialSweep creates reports for volumes that existed before the operator.
//
// It reads through APIReader (a live API call) rather than the cached client on
// purpose: a cached List before the informers started returns an empty result,
// silently leaving every existing volume unreported. The sweep runs once per
// process start, so one direct List is cheap.
func (r *Reconciler) initialSweep(ctx context.Context) error {
	reader := r.APIReader
	if reader == nil {
		// Only reachable from tests that construct a Reconciler by hand.
		reader = r.Client
	}

	var pvs corev1.PersistentVolumeList
	if err := reader.List(ctx, &pvs); err != nil {
		return fmt.Errorf("list persistent volumes: %w", err)
	}
	listed, cephfs, created, skipped := len(pvs.Items), 0, 0, 0
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if !kube.IsCephFSVolume(pv) {
			skipped++
			continue
		}
		cephfs++
		var existing storagev1alpha1.CephFSStatfsReport
		err := r.Get(ctx, types.NamespacedName{Name: pv.Name}, &existing)
		if err == nil {
			continue
		}
		if !kube.NotFound(err) {
			return err
		}
		report := &storagev1alpha1.CephFSStatfsReport{
			ObjectMeta: metav1.ObjectMeta{Name: pv.Name},
			Spec:       storagev1alpha1.CephFSStatfsReportSpec{PersistentVolume: pv.Name},
		}
		if err := r.Create(ctx, report); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create report for %s: %w", pv.Name, err)
		}
		created++
	}
	// listed/cephfs are what the API returned; created is only the reports that
	// did not exist yet. Without the first two numbers a sweep that saw nothing
	// and a sweep where every report already existed look identical -- which is
	// exactly the failure this sweep is supposed to make visible.
	r.Log.Info("initial sweep complete",
		"listed", listed, "cephfs_pvs", cephfs, "created", created, "other_pvs", skipped)
	return nil
}

// Reconcile is keyed by PV name: one report object exists per tracked volume.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.With("pv", req.Name)

	var report storagev1alpha1.CephFSStatfsReport
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name}, &report); err != nil {
		if !kube.NotFound(err) {
			metrics.ReconcileErrors.Inc()
			return ctrl.Result{}, err
		}
		return r.ensureReport(ctx, req.Name, log)
	}

	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: report.Spec.PersistentVolume}, &pv); err != nil {
		if kube.NotFound(err) {
			log.Info("persistent volume is gone, removing report")
			ns, claim := "", ""
			if report.Status.PVCRef != nil {
				ns, claim = report.Status.PVCRef.Namespace, report.Status.PVCRef.Name
			}
			forgetMetrics(report.Name, ns, claim, report.Status.Node, report.Status.StorageClass)
			if err := r.Delete(ctx, &report); err != nil && !kube.NotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	next, requeueAfter, err := r.inspect(ctx, &pv, log)
	if err != nil {
		metrics.ReconcileErrors.Inc()
		return ctrl.Result{}, err
	}

	write := r.applyStatus(&report, next, log)
	if write {
		if err := r.Status().Update(ctx, &report); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// ensureReport creates the report for a PV that has just appeared.
func (r *Reconciler) ensureReport(ctx context.Context, pvName string, log *slog.Logger) (ctrl.Result, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvName}, &pv); err != nil {
		if kube.NotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !kube.IsCephFSVolume(&pv) {
		return ctrl.Result{}, nil
	}

	report := &storagev1alpha1.CephFSStatfsReport{
		ObjectMeta: metav1.ObjectMeta{Name: pv.Name},
		Spec:       storagev1alpha1.CephFSStatfsReportSpec{PersistentVolume: pv.Name},
	}
	if err := r.Create(ctx, report); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("create report for %s: %w", pv.Name, err)
	}
	log.Info("started tracking cephfs volume",
		"storageclass", pv.Spec.StorageClassName, "subvolumePath", kube.SubvolumePath(&pv))
	return ctrl.Result{Requeue: true}, nil
}

// inspect gathers ground truth and kubelet's view, then decides the phase.
func (r *Reconciler) inspect(ctx context.Context, pv *corev1.PersistentVolume, log *slog.Logger) (storagev1alpha1.CephFSStatfsReportStatus, time.Duration, error) {
	st := storagev1alpha1.CephFSStatfsReportStatus{Phase: storagev1alpha1.PhaseOK}
	st.StorageClass = pv.Spec.StorageClassName
	st.SubvolumePath = kube.SubvolumePath(pv)

	ns, claim := "", ""
	if pv.Spec.ClaimRef != nil {
		ns, claim = pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
		st.PVCRef = &storagev1alpha1.NamespacedNameRef{Namespace: ns, Name: claim}
	}

	if r.skipNamespace(ns) {
		st.Phase = storagev1alpha1.PhaseSkipped
		st.Message = fmt.Sprintf("namespace %q is excluded by policy", ns)
		return st, 30 * time.Minute, nil
	}
	if len(r.StorageClasses) > 0 && !r.StorageClasses[pv.Spec.StorageClassName] {
		st.Phase = storagev1alpha1.PhaseSkipped
		st.Message = fmt.Sprintf("storage class %q is out of scope", pv.Spec.StorageClassName)
		return st, 30 * time.Minute, nil
	}

	nodes, err := r.nodesForClaim(ctx, ns, claim)
	if err != nil {
		st.Phase = storagev1alpha1.PhaseError
		st.Message = err.Error()
		setCondition(&st, false, false, "ResolveFailed", err.Error())
		return st, 2 * time.Minute, nil
	}
	if len(nodes) == 0 {
		st.Phase = storagev1alpha1.PhaseSkipped
		st.Message = "claim is not mounted by any running pod, so there is no kubelet figure to compare against"
		return st, 30 * time.Minute, nil
	}

	now := metav1.Now()
	st.LastProbeTime = &now

	// A CephFS superblock is per node, so the cache can be stale on one node
	// and fresh on another. Probe every node that mounts the volume and report
	// the worst client.
	worst, worstErr := r.probeNodes(ctx, pv.Name, nodes, log)
	if worst == nil {
		st.Phase = storagev1alpha1.PhaseError
		st.Node = nodes[0]
		st.Message = fmt.Sprintf("no agent could report ground truth for this volume: %v", worstErr)
		metrics.VolumeProbeSuccess.WithLabelValues(pv.Name, ns, claim, nodes[0]).Set(0)
		setCondition(&st, false, false, "NoAgent", st.Message)
		return st, 2 * time.Minute, nil
	}

	s := *worst
	st.Node = s.Node
	st.QuotaBytes = max64(s.Quota, 0)
	st.TrueBytes = s.RBytes
	st.ReportedBytes = s.StatfsUsed
	st.DriftBytes = s.Drift
	st.DriftRatio = ratio(s.Drift, s.RBytes)
	st.DriftEpisodes = s.DriftEpisodes
	st.WorstDriftBytes = s.WorstDriftBytes
	st.StaleSince = toMetaTime(s.StaleSince)
	metrics.VolumeProbeSuccess.WithLabelValues(pv.Name, ns, claim, s.Node).Set(1)

	if s.Error != "" {
		st.Phase = storagev1alpha1.PhaseError
		st.Message = s.Error
		setCondition(&st, false, false, "ProbeFailed", s.Error)
		return st, 2 * time.Minute, nil
	}
	setCondition(&st, true, true, "Read", "MDS rstat read succeeded")

	tolerance := r.tolerance(s.RBytes)
	// An episode is open exactly when this probe measured drift above the
	// tolerance: the agent closes the episode as soon as a probe finds the
	// reported figure caught up. Do not key this off StaleSeconds, which is
	// legitimately ~0 on the very first observation of an episode.
	liveEpisode := abs64(s.Drift) > tolerance
	everDrifted := abs64(s.WorstDriftBytes) > tolerance

	switch {
	case liveEpisode:
		// The read the agent just issued is the repair. Report the volume as
		// healing and come back quickly to confirm kubelet caught up.
		st.Phase = storagev1alpha1.PhaseHealing
		action := "observed"
		if r.AutoHeal {
			action = "revalidated"
		}
		st.Message = fmt.Sprintf(
			"kubelet reports %.2f GiB but the MDS says %.2f GiB (drift %.2f GiB, %.0f%% of the truth); %s the node's kernel client cache. Episode #%d, worst %.2f GiB, open for %d s",
			gib(s.StatfsUsed), gib(s.RBytes), gib(abs64(s.Drift)),
			100*math.Abs(ratio(s.Drift, s.RBytes)), action,
			s.DriftEpisodes, gib(s.WorstDriftBytes), int(s.StaleSeconds))
		setCondition(&st, true, false, "Drift", st.Message)
		log.Info("statfs drift detected", "node", s.Node,
			"true", s.RBytes, "reported", s.StatfsUsed, "drift", s.Drift,
			"episodes", s.DriftEpisodes)
		return st, r.VerifyInterval, nil

	case everDrifted:
		st.Phase = storagev1alpha1.PhaseHealed
		st.Message = fmt.Sprintf(
			"in sync now at %.2f GiB; this node has caught kubelet under-reporting %d time(s), worst %.2f GiB — treat the reported figure as unusable for history",
			gib(s.RBytes), s.DriftEpisodes, gib(s.WorstDriftBytes))
		setCondition(&st, true, true, "Verified", "kubelet figure matches the MDS after revalidation")

	default:
		st.Phase = storagev1alpha1.PhaseOK
		st.Message = fmt.Sprintf("in sync at %.2f GiB (tolerance %d B)", gib(s.RBytes), tolerance)
		setCondition(&st, true, true, "Verified", "kubelet figure matches the MDS")
	}

	return st, r.ResyncInterval, nil
}

// probeNodes probes every node serving the volume and returns the sample with
// the largest absolute drift, plus the last error if none succeeded.
func (r *Reconciler) probeNodes(ctx context.Context, pv string, nodes []string, log *slog.Logger) (*agent.Sample, error) {
	var (
		worst   *agent.Sample
		lastErr error
	)
	for _, node := range nodes {
		s, err := r.Agents.Probe(ctx, node, pv)
		if err != nil {
			log.Warn("probe failed", "node", node, "err", err)
			lastErr = err
			continue
		}
		if s.Error != "" {
			log.Warn("agent reported a probe error", "node", node, "err", s.Error)
			lastErr = fmt.Errorf("node %s: %s", node, s.Error)
		}
		if worst == nil || abs64(s.Drift) > abs64(worst.Drift) {
			cp := s
			worst = &cp
		}
	}
	if worst != nil && len(nodes) > 1 {
		log.Debug("volume mounted on several nodes; reporting the worst client",
			"nodes", nodes, "worst", worst.Node, "drift", worst.Drift)
	}
	return worst, lastErr
}

// tolerance is the drift below which the two numbers are treated as equal.
func (r *Reconciler) tolerance(trueBytes int64) int64 {
	rel := int64(r.DriftMinRatio * float64(max64(trueBytes, 0)))
	if rel > r.DriftMinBytes {
		return rel
	}
	return r.DriftMinBytes
}

// nodesForClaim returns the nodes whose kubelet serves this claim.
func (r *Reconciler) nodesForClaim(ctx context.Context, ns, claim string) ([]string, error) {
	if ns == "" || claim == "" {
		return nil, fmt.Errorf("persistent volume is not bound to a claim")
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(ns),
		client.MatchingFields{PodPVCIndex: ns + "/" + claim},
	); err != nil {
		return nil, fmt.Errorf("list pods mounting %s/%s: %w", ns, claim, err)
	}

	seen := map[string]bool{}
	nodes := make([]string, 0, 2)
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
			continue
		}
		if !mountsClaim(&p, claim) || seen[p.Spec.NodeName] {
			continue
		}
		seen[p.Spec.NodeName] = true
		nodes = append(nodes, p.Spec.NodeName)
	}
	return nodes, nil
}

func mountsClaim(p *corev1.Pod, claim string) bool {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

func (r *Reconciler) skipNamespace(ns string) bool {
	return ns != "" && r.ExcludeNamespaces[ns]
}

// applyStatus folds the new observation into the report, exports metrics and
// announces phase transitions. It returns true when the object needs a write.
func (r *Reconciler) applyStatus(report *storagev1alpha1.CephFSStatfsReport, next storagev1alpha1.CephFSStatfsReportStatus, log *slog.Logger) bool {
	previous := report.Status

	// Counters and stamps that only ever move forward are carried over here so
	// that inspect() stays a pure observation of the cluster.
	if next.Phase == storagev1alpha1.PhaseHealing && previous.Phase != storagev1alpha1.PhaseHealing {
		next.HealCount = previous.HealCount + 1
		next.LastHealTime = next.LastProbeTime
	} else {
		next.HealCount = previous.HealCount
		if next.LastHealTime == nil {
			next.LastHealTime = previous.LastHealTime
		}
	}
	if next.StaleSince == nil {
		next.StaleSince = previous.StaleSince
	}
	switch next.Phase {
	case storagev1alpha1.PhaseHealing, storagev1alpha1.PhaseDrifted:
		// keep the open episode's start time
	default:
		next.StaleSince = nil
	}

	report.Status = next
	ns, claim := "", ""
	if next.PVCRef != nil {
		ns, claim = next.PVCRef.Namespace, next.PVCRef.Name
	}
	exportMetrics(report, previous, ns, claim)

	if previous.Phase != next.Phase {
		log.Info("phase changed", "from", previous.Phase, "to", next.Phase, "node", next.Node)
		r.announce(report, previous, next, ns, claim)
	}

	return previous.Phase != next.Phase ||
		previous.Node != next.Node ||
		previous.DriftEpisodes != next.DriftEpisodes ||
		previous.WorstDriftBytes != next.WorstDriftBytes ||
		previous.QuotaBytes != next.QuotaBytes ||
		previous.LastProbeTime == nil ||
		next.LastProbeTime.Sub(previous.LastProbeTime.Time) > time.Minute
}

// announce emits the Kubernetes events and counters for a phase transition.
//
// Drift events are throttled: with auto-heal enabled the operator deliberately
// detects and fixes drift on a schedule, so "drift happened" is normal
// operation, not an incident. What is worth an event is a drift big enough to
// matter, or a heal that will not converge.
func (r *Reconciler) announce(report *storagev1alpha1.CephFSStatfsReport, previous, next storagev1alpha1.CephFSStatfsReportStatus, ns, claim string) {
	switch next.Phase {
	case storagev1alpha1.PhaseHealing:
		metrics.HealTotal.WithLabelValues(report.Name, ns, claim, next.Node, "issued").Inc()
		if r.Recorder == nil {
			return
		}
		quiet := previous.LastHealTime != nil &&
			next.LastProbeTime.Sub(previous.LastHealTime.Time) <= r.EventCooldown
		if !quiet {
			r.Recorder.Eventf(report, corev1.EventTypeWarning, "StatfsDrift", "%s", next.Message)
		}
	case storagev1alpha1.PhaseHealed:
		metrics.HealTotal.WithLabelValues(report.Name, ns, claim, next.Node, "converged").Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(report, corev1.EventTypeNormal, "ReportedFigureRecovered", "%s", next.Message)
		}
	case storagev1alpha1.PhaseError:
		if r.Recorder != nil {
			r.Recorder.Eventf(report, corev1.EventTypeWarning, "TruthUnavailable", "%s", next.Message)
		}
	}
}

// setCondition publishes the two conditions every report carries.
//
// truthReadable and inSync are separate on purpose: a probe that could not read
// the MDS rstat leaves the volume unverified, which is *not* the same as
// verified. Reporting ConditionTruthReadable=True in that case made
// `kubectl get cfsr` claim ground truth was available when it was not.
func setCondition(st *storagev1alpha1.CephFSStatfsReportStatus, truthReadable, inSync bool, reason, msg string) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTruthReadable,
		Status:             conditionStatus(truthReadable),
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionInSync,
		Status:             conditionStatus(inSync),
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
}

func conditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// nodeChanged reports whether the volume moved to a different node or storage
// class since the last reconciliation. An empty previous node means this is the
// volume's first observation, and "" must never be used as a label value to
// clean up after.
func nodeChanged(previous, next storagev1alpha1.CephFSStatfsReportStatus) bool {
	if previous.Node == "" {
		return false
	}
	return previous.Node != next.Node || previous.StorageClass != next.StorageClass
}

func exportMetrics(report *storagev1alpha1.CephFSStatfsReport, previous storagev1alpha1.CephFSStatfsReportStatus, ns, claim string) {
	st := report.Status

	// Value gauges only exist while the volume has a placement and a reading
	// worth publishing. The cases are deliberate:
	//
	//	OK/Healing/Healed/Drifted -> publish this observation
	//	Error                    -> keep the last known truth; probe_success=0
	//	                            says it is no longer verified
	//	Skipped                  -> drop the series; a gap is honest, a zero is
	//	                            not ("this volume is empty" is a false read)
	writeValues := st.Phase == storagev1alpha1.PhaseOK ||
		st.Phase == storagev1alpha1.PhaseHealing ||
		st.Phase == storagev1alpha1.PhaseHealed ||
		st.Phase == storagev1alpha1.PhaseDrifted

	// Drop the previous placement only when a new observation replaces it.
	// On Error the old values must survive; on Skipped the cleanup happens in
	// the switch below, against previous.Node.
	if writeValues && nodeChanged(previous, st) {
		volumeSeries(report.Name, ns, claim, previous.Node, previous.StorageClass)
	}

	l := []string{report.Name, ns, claim, st.Node, st.StorageClass}
	short := []string{report.Name, ns, claim, st.Node}

	switch {
	case writeValues:
		metrics.VolumeTrueBytes.WithLabelValues(l...).Set(float64(st.TrueBytes))
		metrics.VolumeReportedBytes.WithLabelValues(l...).Set(float64(st.ReportedBytes))
		metrics.VolumeDriftBytes.WithLabelValues(l...).Set(float64(st.DriftBytes))
		metrics.VolumeDriftRatio.WithLabelValues(l...).Set(st.DriftRatio)
		metrics.VolumeQuotaBytes.WithLabelValues(l...).Set(float64(max64(st.QuotaBytes, 0)))
		metrics.VolumeDriftEpisodes.WithLabelValues(l...).Set(float64(st.DriftEpisodes))
		if st.QuotaBytes > 0 {
			metrics.VolumeUsedRatio.WithLabelValues(l...).Set(float64(st.TrueBytes) / float64(st.QuotaBytes))
		} else {
			// Without a quota the ratio means nothing, and the series must not
			// be left behind: a value published while a quota still existed
			// would go on matching `used_ratio > 0.85` forever, because
			// forgetMetrics only runs when the volume itself disappears.
			metrics.VolumeUsedRatio.DeleteLabelValues(l...)
		}

		metrics.VolumeProbeSuccess.WithLabelValues(short...).Set(1)
		var stale float64
		if st.StaleSince != nil {
			stale = time.Since(st.StaleSince.Time).Seconds()
		}
		metrics.VolumeStaleSeconds.WithLabelValues(short...).Set(stale)

	case st.Phase == storagev1alpha1.PhaseError:
		// Keep the last values on purpose: the last known truth is still the
		// best estimate, and a reader is told it is stale by probe_success=0.
		// Only write that flag when there is a node to attach it to; an empty
		// node label would create a series nothing can join on.
		if st.Node != "" {
			metrics.VolumeProbeSuccess.WithLabelValues(short...).Set(0)
		}

	case st.Phase == storagev1alpha1.PhaseSkipped:
		// st.Node is empty for Skipped, so the series to remove are the ones
		// under the previous placement. Deleting with st.Node would match
		// nothing and leave a frozen "last known" value behind.
		volumeSeries(report.Name, ns, claim, previous.Node, previous.StorageClass)
		if previous.Node != "" {
			// volumeSeries also removed probe_success; recreate it at 0. The
			// "not verified" state is real information, and the alert rule
			// excludes Skipped explicitly.
			metrics.VolumeProbeSuccess.WithLabelValues(report.Name, ns, claim, previous.Node).Set(0)
		}
	}

	// A phase label set that is left behind would keep reporting 1 forever, so
	// zero the outgoing phase (and node) before publishing the new one. Use
	// `count by (phase) (cephfs_guard_volume_phase == 1)` for the fleet view.
	if previous.Phase != "" && (previous.Phase != st.Phase || previous.Node != st.Node) {
		metrics.VolumePhase.WithLabelValues(report.Name, ns, claim, previous.Node, string(previous.Phase)).Set(0)
	}
	metrics.VolumePhase.WithLabelValues(report.Name, ns, claim, st.Node, string(st.Phase)).Set(1)
}

// volumeSeries drops the gauge series keyed by the volume and its placement.
//
// It deliberately leaves cephfs_guard_volume_phase alone — the phase is zeroed,
// never deleted, so a transition stays visible on dashboards — and it leaves
// cephfs_guard_agent_up alone too, because that series belongs to the node and
// is shared with every other volume there.
func volumeSeries(pv, ns, claim, node, class string) {
	l := []string{pv, ns, claim, node, class}
	for _, v := range []*prometheus.GaugeVec{
		metrics.VolumeTrueBytes, metrics.VolumeReportedBytes, metrics.VolumeDriftBytes,
		metrics.VolumeDriftRatio, metrics.VolumeQuotaBytes, metrics.VolumeUsedRatio,
		metrics.VolumeDriftEpisodes,
	} {
		v.DeleteLabelValues(l...)
	}
	short := []string{pv, ns, claim, node}
	metrics.VolumeProbeSuccess.DeleteLabelValues(short...)
	metrics.VolumeStaleSeconds.DeleteLabelValues(short...)
}

// forgetMetrics drops every series belonging to a volume that no longer exists,
// so a deleted PVC does not linger in the TSDB forever.
func forgetMetrics(pv, ns, claim, node, class string) {
	volumeSeries(pv, ns, claim, node, class)
	for _, phase := range []storagev1alpha1.Phase{
		storagev1alpha1.PhaseOK, storagev1alpha1.PhaseDrifted, storagev1alpha1.PhaseHealing,
		storagev1alpha1.PhaseHealed, storagev1alpha1.PhaseError, storagev1alpha1.PhaseSkipped,
	} {
		metrics.VolumePhase.DeleteLabelValues(pv, ns, claim, node, string(phase))
	}
	metrics.AgentUp.DeleteLabelValues(node)
}

func ratio(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func gib(v int64) float64 { return float64(v) / (1 << 30) }

func toMetaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	mt := metav1.NewTime(*t)
	return &mt
}
