package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SandboxSnapshotPhase is the lifecycle of one snapshot attempt. A snapshot
// is one-shot and non-reentrant: a Sandbox (or template name) with a
// non-terminal snapshot rejects new snapshot requests until it terminates.
// +kubebuilder:validation:Enum=Pending;Creating;Publishing;Succeeded;Failed
type SandboxSnapshotPhase string

const (
	// SandboxSnapshotPhasePending means the intent is persisted and the
	// snapshot has not been accepted by a fastlet yet.
	SandboxSnapshotPhasePending SandboxSnapshotPhase = "Pending"
	// SandboxSnapshotPhaseCreating means the fastlet is executing the
	// pause/dump/resume window on the target Sandbox.
	SandboxSnapshotPhaseCreating SandboxSnapshotPhase = "Creating"
	// SandboxSnapshotPhasePublishing means local artifacts are complete and
	// the artifact set is being uploaded to the artifact store.
	SandboxSnapshotPhasePublishing SandboxSnapshotPhase = "Publishing"
	// SandboxSnapshotPhaseSucceeded means the artifact set is published and
	// addressable under spec.templateName.
	SandboxSnapshotPhaseSucceeded SandboxSnapshotPhase = "Succeeded"
	// SandboxSnapshotPhaseFailed means the attempt terminated; local
	// staging is discarded and nothing was published. Retry with a new
	// request (new object name).
	SandboxSnapshotPhaseFailed SandboxSnapshotPhase = "Failed"
)

// Terminal reports whether the phase is final: the Sandbox and template name
// accept new snapshot requests again.
func (p SandboxSnapshotPhase) Terminal() bool {
	return p == SandboxSnapshotPhaseSucceeded || p == SandboxSnapshotPhaseFailed
}

const (
	// SandboxSnapshotConditionCompleted reflects whether the snapshot
	// reached Succeeded.
	SandboxSnapshotConditionCompleted = "Completed"
)

// SandboxRef identifies the target Sandbox of a snapshot.
type SandboxRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// UID fences the reference against a Sandbox recreated under the same
	// name. Empty accepts the current Sandbox; fastpath fills it from the
	// live object it validated.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// SandboxSnapshotSpec defines the desired snapshot. The spec is immutable:
// a snapshot is one-shot; re-snapshotting requires a new object.
type SandboxSnapshotSpec struct {
	// SandboxRef is the running Sandbox to snapshot. The Sandbox must be
	// Ready and assigned when the snapshot is triggered.
	// +kubebuilder:validation:Required
	SandboxRef SandboxRef `json:"sandboxRef"`

	// TemplateName is the artifact-store index key the artifact set is
	// published under: publish writes index/<sha256(templateName)>.json, and
	// a later Sandbox create with image=TemplateName boots from the
	// snapshot through the standard pull/restore chain. Unlike a
	// SandboxTemplate build, no default sha256(image) index is written.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(/[a-zA-Z0-9._-]+)*(:[a-zA-Z0-9._-]{1,127})?$`
	// +kubebuilder:validation:MaxLength=255
	TemplateName string `json:"templateName"`
}

// SandboxSnapshotStatus reports the observed state of one snapshot attempt.
// All fields are projections written by the control plane; the artifact
// facts come from the fastlet that executed the snapshot.
type SandboxSnapshotStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the snapshot lifecycle.
	// +optional
	Phase SandboxSnapshotPhase `json:"phase,omitempty"`
	// Message is the latest progress or failure reason.
	// +optional
	Message string `json:"message,omitempty"`

	// SandboxUID is the UID of the snapshotted Sandbox incarnation.
	// +optional
	SandboxUID types.UID `json:"sandboxUID,omitempty"`

	// FastletName/FastletPodUID locate the fastlet the snapshot was
	// triggered on, resolved from the target Sandbox's assignment at
	// trigger time. The assignment annotation stays authoritative; these
	// fields are a projection.
	// +optional
	FastletName   string    `json:"fastletName,omitempty"`
	FastletPodUID types.UID `json:"fastletPodUID,omitempty"`

	// SnapshotID is the fastlet-side identity of the snapshot task.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// ManifestRef is the s3:// URI of the published manifest.json
	// (Succeeded only).
	// +optional
	ManifestRef string `json:"manifestRef,omitempty"`
	// ArtifactDigest is the sha256 of the published manifest document.
	// +optional
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	// SizeBytes is the total logical size of the published artifact set.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// StartedAt is when the fastlet accepted the snapshot.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the snapshot reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SandboxSnapshot is a one-shot snapshot of a running Sandbox: the artifacts
// (rootfs, vmstate, memory) are published to the artifact store in the
// SandboxTemplate layout, addressable under spec.templateName. The object is
// created by fastpath (CreateSandboxSnapshot) and driven to completion by
// the controller; a Sandbox (or template name) with a non-terminal snapshot
// rejects further snapshot requests.
type SandboxSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSnapshotSpec   `json:"spec,omitempty"`
	Status SandboxSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxSnapshotList contains a list of SandboxSnapshot.
type SandboxSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxSnapshot{}, &SandboxSnapshotList{})
}
