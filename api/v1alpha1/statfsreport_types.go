package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase summarises what the operator currently believes about one volume.
type Phase string

const (
	// PhaseOK means the kubelet-reported usage matches the MDS ground truth.
	PhaseOK Phase = "OK"
	// PhaseDrifted means the two disagree beyond the configured tolerance:
	// kubelet_volume_stats_used_bytes cannot be trusted for this volume.
	PhaseDrifted Phase = "Drifted"
	// PhaseHealing means a revalidation has just been issued and the operator
	// is waiting for kubelet to pick up the corrected value.
	PhaseHealing Phase = "Healing"
	// PhaseHealed means drift was observed and later cleared by the operator.
	PhaseHealed Phase = "Healed"
	// PhaseError means the volume could not be probed (agent unreachable,
	// volume not mounted on any node, permission denied, ...).
	PhaseError Phase = "Error"
	// PhaseSkipped means the volume is intentionally out of scope.
	PhaseSkipped Phase = "Skipped"
)

// NamespacedNameRef points at the PersistentVolumeClaim behind a volume.
type NamespacedNameRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// CephFSStatfsReportSpec identifies the volume under observation. Reports are
// created and owned by the operator; they carry no user-tunable knobs so that
// reconciliation stays a pure function of cluster state.
type CephFSStatfsReportSpec struct {
	// PersistentVolume is the PV name this report tracks.
	// +kubebuilder:validation:MinLength=1
	PersistentVolume string `json:"persistentVolume"`
}

// CephFSStatfsReportStatus is the observed state of one CephFS volume.
type CephFSStatfsReportStatus struct {
	// PVCRef is the claim this PV is bound to, when bound.
	PVCRef *NamespacedNameRef `json:"pvcRef,omitempty"`

	// Node is the node whose CephFS kernel client serves this volume.
	Node string `json:"node,omitempty"`

	// StorageClass of the backing PVC.
	StorageClass string `json:"storageClass,omitempty"`

	// SubvolumePath is the CephFS path exported by the CSI volume handle,
	// e.g. /volumes/csi/csi-vol-<id>/<subvol-uuid>.
	SubvolumePath string `json:"subvolumePath,omitempty"`

	// QuotaBytes is ceph.quota.max_bytes on the subvolume root (the number
	// statfs() uses as the filesystem size). Zero or negative means the
	// subvolume carries no quota and the reported used figure is whole-filesystem.
	QuotaBytes int64 `json:"quotaBytes,omitempty"`

	// TrueBytes is the MDS-authoritative content size (ceph.dir.rbytes).
	TrueBytes int64 `json:"trueBytes,omitempty"`

	// ReportedBytes is what kubelet's statfs()-derived metric says.
	ReportedBytes int64 `json:"reportedBytes,omitempty"`

	// DriftBytes is TrueBytes - ReportedBytes. Positive means the volume is
	// fuller than the monitoring stack believes.
	DriftBytes int64 `json:"driftBytes,omitempty"`

	// DriftRatio is DriftBytes / max(TrueBytes, 1).
	DriftRatio float64 `json:"driftRatio,omitempty"`

	// Phase is the rolled-up state.
	Phase Phase `json:"phase,omitempty"`

	// LastProbeTime is when the agent last read the rstat.
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// LastHealTime is when the operator last issued a revalidation.
	LastHealTime *metav1.Time `json:"lastHealTime,omitempty"`

	// StaleSince is when the reported figure was first found wrong for the
	// current episode. Absent while the volume is in sync.
	StaleSince *metav1.Time `json:"staleSince,omitempty"`

	// WorstDriftBytes is the largest drift seen during the current or most
	// recent stale episode.
	WorstDriftBytes int64 `json:"worstDriftBytes,omitempty"`

	// DriftEpisodes counts how many distinct stale-reading episodes the agent
	// has caught for this volume since it started.
	DriftEpisodes int64 `json:"driftEpisodes,omitempty"`

	// HealCount counts revalidations issued for this volume.
	HealCount int32 `json:"healCount,omitempty"`

	// Message is a human-readable explanation of the current phase.
	Message string `json:"message,omitempty"`

	// Conditions follow the usual Kubernetes condition conventions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types reported on every volume.
const (
	// ConditionTruthReadable is True when the agent could read the rstat.
	ConditionTruthReadable = "TruthReadable"
	// ConditionInSync is False while the reported usage is not trustworthy.
	ConditionInSync = "InSync"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cfsr,categories=storage
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.pvcRef.namespace`
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=`.status.pvcRef.name`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.node`
// +kubebuilder:printcolumn:name="True",type=integer,JSONPath=`.status.trueBytes`
// +kubebuilder:printcolumn:name="Reported",type=integer,JSONPath=`.status.reportedBytes`
// +kubebuilder:printcolumn:name="Drift",type=integer,JSONPath=`.status.driftBytes`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CephFSStatfsReport is the per-PersistentVolume drift record maintained by
// statfs-guard. One object exists per in-scope PV.
type CephFSStatfsReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CephFSStatfsReportSpec   `json:"spec,omitempty"`
	Status CephFSStatfsReportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CephFSStatfsReportList lists CephFSStatfsReport objects.
type CephFSStatfsReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CephFSStatfsReport `json:"items"`
}
