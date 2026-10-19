// Package metrics declares the Prometheus collectors exported by statfs-guard.
//
// Two collectors exist because the series have different label sets and live
// in different processes:
//
//	Agent  (DaemonSet, one per node)  -> cephfs_node_*   labels: pv, node
//	Manager (Deployment, cluster-wide) -> cephfs_guard_*  labels: pvc, namespace, node
//
// The headline series is cephfs_guard_volume_true_used_bytes: it is the
// MDS-authoritative figure and is the one capacity alerts should use, because
// kubelet_volume_stats_used_bytes can under-report a CephFS volume by up to
// 100% and stay wrong for months.
package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ---------------------------------------------------------------------------
// Agent-side: raw node-local observations.
// ---------------------------------------------------------------------------

var (
	// NodeTruthBytes is ceph.dir.rbytes — how much is really in the subvolume.
	NodeTruthBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_volume_true_used_bytes",
		Help: "MDS-authoritative bytes inside the CephFS subvolume (ceph.dir.rbytes).",
	}, []string{"pv", "node"})

	// NodeTruthFiles is ceph.dir.rfiles.
	NodeTruthFiles = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_volume_true_files",
		Help: "MDS-authoritative file count inside the CephFS subvolume (ceph.dir.rfiles).",
	}, []string{"pv", "node"})

	// NodeQuotaBytes is ceph.quota.max_bytes, the per-volume ceiling.
	NodeQuotaBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_volume_quota_bytes",
		Help: "ceph.quota.max_bytes on the subvolume root; 0 when the volume has no quota.",
	}, []string{"pv", "node"})

	// NodeStatfsUsedBytes is what statfs(2) — and therefore kubelet — reports.
	NodeStatfsUsedBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_volume_statfs_used_bytes",
		Help: "Used bytes from the local statfs(2) call, the same source kubelet uses.",
	}, []string{"pv", "node"})

	// NodeStatfsCapacityBytes is statfs f_blocks*f_bsize.
	NodeStatfsCapacityBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_volume_statfs_capacity_bytes",
		Help: "Capacity reported by the local statfs(2) call.",
	}, []string{"pv", "node"})

	// NodeDriftBytes is truth minus statfs, measured on the same node at the
	// same instant, so it isolates the kernel cache staleness from any
	// aggregation error.
	NodeDriftBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_volume_drift_bytes",
		Help: "true_used - statfs_used measured locally. Positive means statfs under-reports.",
	}, []string{"pv", "node"})

	// NodeProbeSuccess is 1 when the rstat was readable.
	NodeProbeSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_volume_probe_success",
		Help: "1 when the last rstat probe succeeded, 0 otherwise.",
	}, []string{"pv", "node"})

	// NodeRevalidateTotal counts rstat reads. Each successful read is also a
	// client-cache revalidation (CEPH_STAT_RSTAT round trip to the auth MDS),
	// which is what makes the kubelet metric correct again.
	NodeRevalidateTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cephfs_node_revalidate_total",
		Help: "rstat reads issued on this node, by outcome. Reads are non-destructive.",
	}, []string{"pv", "node", "result"})

	// NodeProbeDuration observes single-probe latency (MDS round trip).
	NodeProbeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cephfs_node_probe_duration_seconds",
		Help:    "Latency of one rstat probe.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"node"})

	// NodeMounts is how many CephFS volume mounts the agent can see on this node.
	NodeMounts = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cephfs_node_mounts",
		Help: "CephFS volume mounts visible to the agent on this node.",
	})

	// NodeDriftEpisodesTotal counts how many separate times the volume was
	// found reporting a stale figure. A nonzero value means the monitoring
	// stack was blind to this volume's growth for a while.
	NodeDriftEpisodesTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_drift_episodes_total",
		Help: "Number of distinct stale-reading episodes observed on this node.",
	}, []string{"pv", "node"})

	// NodeDriftBytesLast is the drift at the start of the current episode,
	// i.e. how wrong kubelet was before this agent corrected it.
	NodeDriftBytesLast = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_drift_bytes_last_episode",
		Help: "Worst drift measured during the current or most recent episode.",
	}, []string{"pv", "node"})

	// NodeReportedStaleSeconds is how long the kubelet-facing figure has been
	// known wrong for the current episode.
	NodeReportedStaleSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_node_reported_stale_seconds",
		Help: "Seconds the kubelet-facing used figure has been known stale (0 when in sync).",
	}, []string{"pv", "node"})

	// NodeEpisodeSeconds observes completed episode lengths. Without an agent
	// running these episodes never end: that is the whole failure mode.
	NodeEpisodeSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cephfs_node_drift_episode_seconds",
		Help:    "Length of completed stale-reading episodes.",
		Buckets: []float64{60, 300, 900, 3600, 4 * 3600, 24 * 3600, 7 * 24 * 3600, 30 * 24 * 3600},
	}, []string{"pv", "node"})
)

// ---------------------------------------------------------------------------
// Manager-side: cluster-scoped, Kubernetes-labelled view.
// ---------------------------------------------------------------------------

var (
	// VolumeTrueBytes joins node truth with PVC identity.
	VolumeTrueBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_true_used_bytes",
		Help: "Real bytes in the PVC's CephFS subvolume. Use this for capacity alerts.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "storageclass"})

	// VolumeReportedBytes is the kubelet figure, for building drift dashboards.
	VolumeReportedBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_reported_used_bytes",
		Help: "Used bytes kubelet believes, i.e. kubelet_volume_stats_used_bytes.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "storageclass"})

	// VolumeDriftBytes is the gap between the two.
	VolumeDriftBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_drift_bytes",
		Help: "true_used - reported_used. Alert on this to catch lying volumes.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "storageclass"})

	// VolumeDriftRatio expresses drift as a fraction of the truth.
	VolumeDriftRatio = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_drift_ratio",
		Help: "drift / true_used. 1 means the volume was reported as empty.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "storageclass"})

	// VolumeQuotaBytes is the per-volume ceiling.
	VolumeQuotaBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_quota_bytes",
		Help: "Quota on the PVC's subvolume; 0 when unset.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "storageclass"})

	// VolumeUsedRatio is the real fullness of the volume.
	VolumeUsedRatio = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_used_ratio",
		Help: "true_used / quota. The number a 'volume almost full' alert wants.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "storageclass"})

	// VolumePhase reports the guard phase (1 on the active phase label).
	VolumePhase = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_phase",
		Help: "1 on the label matching the current phase of the volume.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "phase"})

	// VolumeStaleSeconds is how long the reported value has been known wrong.
	VolumeStaleSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_stale_seconds",
		Help: "Seconds since drift was first observed for this volume.",
	}, []string{"pv", "pvc_namespace", "pvc", "node"})

	// VolumeProbeSuccess mirrors the agent signal for the manager's view.
	VolumeProbeSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_volume_probe_success",
		Help: "1 when the operator could obtain ground truth for this volume.",
	}, []string{"pv", "pvc_namespace", "pvc", "node"})

	// HealTotal counts revalidations issued by the manager.
	HealTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cephfs_guard_heal_total",
		Help: "Client-cache revalidations issued by the operator, by outcome.",
	}, []string{"pv", "pvc_namespace", "pvc", "node", "result"})

	// ReconcileErrors counts failed reconciles.
	ReconcileErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cephfs_guard_reconcile_errors_total",
		Help: "Total reconcile failures.",
	})

	// AgentUp marks whether the agent on a node answered.
	AgentUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cephfs_guard_agent_up",
		Help: "1 when the node agent answered the operator.",
	}, []string{"node"})

	// AgentRequestSeconds observes agent calls.
	AgentRequestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cephfs_guard_agent_request_seconds",
		Help:    "Latency of operator -> agent requests.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"node", "endpoint"})
)

// Register adds the manager-side collectors (cephfs_guard_*) to r. Calling it
// more than once for the same registry is harmless.
//
// This is not optional plumbing. controller-runtime's metrics server gathers
// sigs.k8s.io/controller-runtime/pkg/metrics.Registry, which is a *different*
// registry from prometheus' default one. promauto registers into the default
// registry, so without this call the manager serves controller-runtime's own
// metrics and not a single cephfs_guard_* series -- every alert rule built on
// them silently matches nothing. The agent is unaffected: it serves the
// default registry through promhttp.Handler().
func Register(r prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		VolumeTrueBytes,
		VolumeReportedBytes,
		VolumeDriftBytes,
		VolumeDriftRatio,
		VolumeQuotaBytes,
		VolumeUsedRatio,
		VolumePhase,
		VolumeStaleSeconds,
		VolumeProbeSuccess,
		HealTotal,
		ReconcileErrors,
		AgentUp,
		AgentRequestSeconds,
	}
	for _, c := range collectors {
		if err := r.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if errors.As(err, &already) {
				continue
			}
			return err
		}
	}
	return nil
}
