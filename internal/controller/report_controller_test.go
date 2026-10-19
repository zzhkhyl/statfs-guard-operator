package controller

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/zzhkhyl/statfs-guard-operator/api/v1alpha1"
	"github.com/zzhkhyl/statfs-guard-operator/internal/kube"
)

const (
	testMiB = int64(1) << 20
	testGiB = int64(1) << 30
)

// TestTolerance pins the rule that decides when two readings are "the same":
// max(driftMinBytes, driftMinRatio * truth). Getting this wrong makes the
// operator either silent (floor too high) or noisy (ratio too tight).
func TestTolerance(t *testing.T) {
	cases := []struct {
		name      string
		minBytes  int64
		minRatio  float64
		trueBytes int64
		want      int64
	}{
		{"empty volume keeps the absolute floor", 64 * testMiB, 0.05, 0, 64 * testMiB},
		{"small volume keeps the absolute floor", 64 * testMiB, 0.05, 100 * testMiB, 64 * testMiB},
		{"large volume is governed by the ratio", 64 * testMiB, 0.05, 10 * testGiB, testGiB / 2},
		{"ratio wins over a tiny floor", 1 * testMiB, 0.5, 10 * testMiB, 5 * testMiB},
		{"floor wins when the ratio is small", 8 * testMiB, 0.01, 10 * testMiB, 8 * testMiB},
		{"negative truth cannot produce a negative tolerance", 64 * testMiB, 0.05, -1, 64 * testMiB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{DriftMinBytes: tc.minBytes, DriftMinRatio: tc.minRatio}
			if got := r.tolerance(tc.trueBytes); got != tc.want {
				t.Fatalf("tolerance(%d) = %d, want %d", tc.trueBytes, got, tc.want)
			}
		})
	}
}

func TestRatio(t *testing.T) {
	cases := []struct {
		name  string
		part  int64
		whole int64
		want  float64
	}{
		{"zero whole is reported as zero, not NaN", 1, 0, 0},
		{"negative whole is reported as zero", 1, -5, 0},
		{"half", 5, 10, 0.5},
		{"negative part keeps its sign: statfs can over-report", -5, 10, -0.5},
		{"empty", 0, 10, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ratio(tc.part, tc.whole); got != tc.want {
				t.Fatalf("ratio(%d, %d) = %v, want %v", tc.part, tc.whole, got, tc.want)
			}
		})
	}
}

func TestAbs64(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want int64
	}{
		{0, 0},
		{5, 5},
		{-5, 5},
		{1 << 40, 1 << 40},
		{-(1 << 40), 1 << 40},
	} {
		if got := abs64(tc.in); got != tc.want {
			t.Fatalf("abs64(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMax64(t *testing.T) {
	for _, tc := range []struct {
		a, b int64
		want int64
	}{
		{1, 2, 2},
		{2, 1, 2},
		{-1, -2, -1},
		{0, 0, 0},
		{5, 5, 5},
	} {
		if got := max64(tc.a, tc.b); got != tc.want {
			t.Fatalf("max64(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestConditionStatus guards the fix for conditions that used to be reported
// True while the volume was actually unverifiable.
func TestConditionStatus(t *testing.T) {
	if got := conditionStatus(true); got != metav1.ConditionTrue {
		t.Fatalf("conditionStatus(true) = %q, want %q", got, metav1.ConditionTrue)
	}
	if got := conditionStatus(false); got != metav1.ConditionFalse {
		t.Fatalf("conditionStatus(false) = %q, want %q", got, metav1.ConditionFalse)
	}
}

func TestPodPVCIndexFunc(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				pvcVolume("data", "data"),
				pvcVolume("logs", "logs"),
				// A claim reference with no name must not produce a key.
				pvcVolume("broken", ""),
				// Non-PVC volumes are not indexed at all.
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
			},
		},
	}

	got := PodPVCIndexFunc(pod)
	want := []string{"team-a/data", "team-a/logs"}
	if !slices.Equal(got, want) {
		t.Fatalf("PodPVCIndexFunc(pod) = %v, want %v", got, want)
	}

	if keys := PodPVCIndexFunc(&corev1.Pod{}); len(keys) != 0 {
		t.Fatalf("pod without volumes produced %v, want none", keys)
	}

	// Anything that is not a pod must be skipped rather than panic.
	var notAPod client.Object = &corev1.PersistentVolume{}
	if keys := PodPVCIndexFunc(notAPod); keys != nil {
		t.Fatalf("non-pod object produced %v, want nil", keys)
	}
}

// TestNodeChanged pins when the previous placement's series get cleaned up.
// The empty-previous case is the important one: on the first reconcile every
// field is zero, and deleting the series for node "" would be meaningless while
// looking like it did something.
func TestNodeChanged(t *testing.T) {
	status := func(node, class string) storagev1alpha1.CephFSStatfsReportStatus {
		return storagev1alpha1.CephFSStatfsReportStatus{Node: node, StorageClass: class}
	}

	cases := []struct {
		name     string
		previous storagev1alpha1.CephFSStatfsReportStatus
		next     storagev1alpha1.CephFSStatfsReportStatus
		want     bool
	}{
		{"first observation", status("", ""), status("node-1", "cephfs"), false},
		{"first observation, previous class only", status("", "cephfs"), status("node-1", "cephfs"), false},
		{"same node and class", status("node-1", "cephfs"), status("node-1", "cephfs"), false},
		{"moved to another node", status("node-1", "cephfs"), status("node-2", "cephfs"), true},
		{"same node, different class", status("node-1", "cephfs"), status("node-1", "cephfs-slow"), true},
		{"volume lost its node", status("node-1", "cephfs"), status("", "cephfs"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeChanged(tc.previous, tc.next); got != tc.want {
				t.Fatalf("nodeChanged(%+v, %+v) = %v, want %v", tc.previous, tc.next, got, tc.want)
			}
		})
	}
}

// TestInitialSweepUsesLiveReader pins the fix for a sweep that reported
// "cephfs_pvs: 0" on a cluster with 20 volumes: the cached client can be empty
// at that moment because the informers have not served their first list yet.
// The sweep must read from the API server, not the cache.
func TestInitialSweepUsesLiveReader(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add statfsguard scheme: %v", err)
	}

	pv := func(name, driver string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: driver},
				},
			},
		}
	}

	// Client stands in for an informer cache that has not listed anything yet.
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	live := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pv("pv-ceph", kube.CephDriver), pv("pv-other", "other.csi.example.com")).
		Build()

	r := &Reconciler{
		Client:    cached,
		APIReader: live,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := r.initialSweep(context.Background()); err != nil {
		t.Fatalf("initialSweep: %v", err)
	}

	var report storagev1alpha1.CephFSStatfsReport
	err := cached.Get(context.Background(), types.NamespacedName{Name: "pv-ceph"}, &report)
	if err != nil {
		t.Fatalf("no report created for the CephFS volume: %v", err)
	}
	if report.Spec.PersistentVolume != "pv-ceph" {
		t.Fatalf("report points at %q, want pv-ceph", report.Spec.PersistentVolume)
	}

	err = cached.Get(context.Background(), types.NamespacedName{Name: "pv-other"}, &report)
	if err == nil {
		t.Fatal("a report was created for a non-CephFS volume")
	}
}

// The value gauges are exported into prometheus' default registry through
// promauto, so these tests read it directly. Each test uses its own pv label so
// cases cannot pollute each other; there is no registry to rebuild.

type metricSample struct {
	labels map[string]string
	value  float64
}

func gatherSamples(t *testing.T, name, pv string) []metricSample {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []metricSample
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["pv"] != pv {
				continue
			}
			v := 0.0
			if m.GetGauge() != nil {
				v = m.GetGauge().GetValue()
			}
			out = append(out, metricSample{labels: labels, value: v})
		}
	}
	return out
}

// TestExportMetricsSkippedDropsValueSeries pins the rule that an unmounted
// volume disappears from the value gauges instead of reading 0: a dashboards
// gap is honest, a zero is a false "this volume is empty".
func TestExportMetricsSkippedDropsValueSeries(t *testing.T) {
	const (
		pv    = "test-skip-pv"
		ns    = "test-ns"
		claim = "test-claim"
		node  = "node-a"
		class = "cephfs"
	)
	report := &storagev1alpha1.CephFSStatfsReport{ObjectMeta: metav1.ObjectMeta{Name: pv}}
	report.Status = storagev1alpha1.CephFSStatfsReportStatus{
		Phase:         storagev1alpha1.PhaseOK,
		Node:          node,
		StorageClass:  class,
		TrueBytes:     1 << 30,
		ReportedBytes: 512 << 20,
		DriftBytes:    512 << 20,
		QuotaBytes:    2 << 30,
		DriftEpisodes: 1,
	}
	exportMetrics(report, storagev1alpha1.CephFSStatfsReportStatus{}, ns, claim)
	if got := gatherSamples(t, "cephfs_guard_volume_true_used_bytes", pv); len(got) != 1 {
		t.Fatalf("precondition: want one true_used series, got %v", got)
	}

	previous := report.Status
	report.Status = storagev1alpha1.CephFSStatfsReportStatus{
		Phase:        storagev1alpha1.PhaseSkipped,
		StorageClass: class,
	}
	exportMetrics(report, previous, ns, claim)

	for _, name := range []string{
		"cephfs_guard_volume_true_used_bytes",
		"cephfs_guard_volume_reported_used_bytes",
		"cephfs_guard_volume_drift_bytes",
		"cephfs_guard_volume_drift_ratio",
		"cephfs_guard_volume_drift_episodes",
		"cephfs_guard_volume_quota_bytes",
		"cephfs_guard_volume_used_ratio",
	} {
		if got := gatherSamples(t, name, pv); len(got) != 0 {
			t.Fatalf("%s survived Skipped with %v; it must be deleted, not zeroed", name, got)
		}
	}

	// probe_success is the one series that must survive: "not verified" is
	// real information, and it stays on the previous placement's node.
	got := gatherSamples(t, "cephfs_guard_volume_probe_success", pv)
	if len(got) != 1 {
		t.Fatalf("want exactly one probe_success series after Skipped, got %v", got)
	}
	if got[0].labels["node"] != node {
		t.Fatalf("probe_success node = %q, want %q", got[0].labels["node"], node)
	}
	if got[0].value != 0 {
		t.Fatalf("probe_success = %v, want 0 for a Skipped volume", got[0].value)
	}

	// The phase label is still published for Skipped. The outgoing phase is
	// explicitly zeroed rather than deleted, so exactly one series should be 1.
	var active []metricSample
	for _, s := range gatherSamples(t, "cephfs_guard_volume_phase", pv) {
		if s.value == 1 {
			active = append(active, s)
		}
	}
	if len(active) != 1 {
		t.Fatalf("want exactly one active phase series, got %v", active)
	}
	if active[0].labels["phase"] != string(storagev1alpha1.PhaseSkipped) {
		t.Fatalf("active phase = %q, want Skipped", active[0].labels["phase"])
	}
}

// TestExportMetricsNodeMoveDropsOldSeries pins the fix for double counting:
// after a volume moves from node-a to node-b, `sum by (pv)` must not see both.
func TestExportMetricsNodeMoveDropsOldSeries(t *testing.T) {
	const (
		pv    = "test-move-pv"
		ns    = "test-ns"
		claim = "test-claim"
		class = "cephfs"
	)
	report := &storagev1alpha1.CephFSStatfsReport{ObjectMeta: metav1.ObjectMeta{Name: pv}}
	report.Status = storagev1alpha1.CephFSStatfsReportStatus{
		Phase:        storagev1alpha1.PhaseOK,
		Node:         "node-a",
		StorageClass: class,
		TrueBytes:    100,
		QuotaBytes:   1000,
	}
	exportMetrics(report, storagev1alpha1.CephFSStatfsReportStatus{}, ns, claim)

	previous := report.Status
	report.Status = storagev1alpha1.CephFSStatfsReportStatus{
		Phase:        storagev1alpha1.PhaseOK,
		Node:         "node-b",
		StorageClass: class,
		TrueBytes:    200,
		QuotaBytes:   1000,
	}
	exportMetrics(report, previous, ns, claim)

	for _, name := range []string{
		"cephfs_guard_volume_true_used_bytes",
		"cephfs_guard_volume_drift_bytes",
		"cephfs_guard_volume_drift_episodes",
		"cephfs_guard_volume_quota_bytes",
		"cephfs_guard_volume_used_ratio",
	} {
		got := gatherSamples(t, name, pv)
		if len(got) != 1 {
			t.Fatalf("%s: want exactly the node-b series, got %v", name, got)
		}
		if got[0].labels["node"] != "node-b" {
			t.Fatalf("%s still carries node=%q", name, got[0].labels["node"])
		}
	}
}

// TestExportMetricsQuotaRemovedDropsUsedRatio pins that removing a quota does
// not leave used_ratio frozen at its last value, where it would keep matching
// the almost-full alert on a denominator that no longer exists.
func TestExportMetricsQuotaRemovedDropsUsedRatio(t *testing.T) {
	const (
		pv    = "test-quota-pv"
		ns    = "test-ns"
		claim = "test-claim"
		node  = "node-a"
		class = "cephfs"
	)
	report := &storagev1alpha1.CephFSStatfsReport{ObjectMeta: metav1.ObjectMeta{Name: pv}}
	report.Status = storagev1alpha1.CephFSStatfsReportStatus{
		Phase:        storagev1alpha1.PhaseOK,
		Node:         node,
		StorageClass: class,
		TrueBytes:    900 << 20,
		QuotaBytes:   1 << 30,
	}
	exportMetrics(report, storagev1alpha1.CephFSStatfsReportStatus{}, ns, claim)
	if got := gatherSamples(t, "cephfs_guard_volume_used_ratio", pv); len(got) != 1 {
		t.Fatalf("precondition: want a used_ratio series, got %v", got)
	}

	previous := report.Status
	report.Status = storagev1alpha1.CephFSStatfsReportStatus{
		Phase:        storagev1alpha1.PhaseOK,
		Node:         node,
		StorageClass: class,
		TrueBytes:    900 << 20,
		QuotaBytes:   0,
	}
	exportMetrics(report, previous, ns, claim)

	if got := gatherSamples(t, "cephfs_guard_volume_used_ratio", pv); len(got) != 0 {
		t.Fatalf("used_ratio survived quota removal: %v", got)
	}
	got := gatherSamples(t, "cephfs_guard_volume_quota_bytes", pv)
	if len(got) != 1 || got[0].value != 0 {
		t.Fatalf("quota_bytes = %v, want a single series at 0", got)
	}
}

func pvcVolume(name, claim string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		},
	}
}
