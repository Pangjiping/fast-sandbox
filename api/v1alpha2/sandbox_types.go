package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "sandbox.fast.io", Version: "v1alpha2"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

// FailurePolicy defines the action to take when the fastlet becomes unreachable.
// +kubebuilder:validation:Enum=Manual;AutoRecreate
type FailurePolicy string

const (
	// FailurePolicyManual means only report the failure in status, do nothing automatically.
	FailurePolicyManual FailurePolicy = "Manual"
	// FailurePolicyAutoRecreate means automatically reschedule the sandbox after timeout.
	FailurePolicyAutoRecreate FailurePolicy = "AutoRecreate"
)

const SandboxConditionReady = "Ready"

// SandboxSpec defines the desired state of Sandbox.
// +kubebuilder:validation:XValidation:rule="!has(self.actionBindings) || self.actionBindings.all(x, self.actionBindings.filter(y, y.handler == x.handler).size() == 1)",message="actionBindings must use unique Handler names"
type SandboxSpec struct {
	// +kubebuilder:validation:MinLength=1
	Image      string          `json:"image"`
	Command    []string        `json:"command,omitempty"`
	Args       []string        `json:"args,omitempty"`
	Envs       []corev1.EnvVar `json:"envs,omitempty"`
	WorkingDir string          `json:"workingDir,omitempty"`

	// ExpireTime specifies when this sandbox should expire and be garbage collected.
	// If not set, the sandbox will not expire automatically.
	ExpireTime *metav1.Time `json:"expireTime,omitempty"`

	// FailurePolicy defines the recovery strategy when the fastlet is lost.
	// Defaults to "Manual".
	// +kubebuilder:default="Manual"
	FailurePolicy FailurePolicy `json:"failurePolicy,omitempty"`

	// RecoveryTimeoutSeconds is the duration to wait before taking action after losing contact with fastlet.
	// Defaults to 60 seconds.
	// +kubebuilder:default=60
	RecoveryTimeoutSeconds int32 `json:"recoveryTimeoutSeconds,omitempty"`

	// ResetRevision is an opaque token (usually a timestamp) used to trigger a manual reset.
	// When Spec.ResetRevision > Status.Runtime.AcceptedResetRevision, the sandbox will be rescheduled.
	ResetRevision *metav1.Time `json:"resetRevision,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// PoolRef specifies which SandboxPool this sandbox should be scheduled to.
	// This field is required.
	PoolRef string `json:"poolRef"`

	// ActionBindings is atomic because its order defines Handler invocation order.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	ActionBindings []ActionBinding `json:"actionBindings,omitempty"`
}

type ActionBinding struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Handler string `json:"handler"`

	// Input is opaque Handler-owned data. Fast Sandbox preserves the UTF-8
	// bytes exactly and never parses or canonicalizes its contents.
	// +kubebuilder:validation:MaxLength=65536
	Input string `json:"input"`
}

// RuntimeState is the lifecycle of the concrete Sandbox runtime.
// +kubebuilder:validation:Enum=Unknown;Pending;Creating;Ready;Stopping;Stopped;Failed;Unavailable
type RuntimeState string

const (
	RuntimeUnknown     RuntimeState = "Unknown"
	RuntimePending     RuntimeState = "Pending"
	RuntimeCreating    RuntimeState = "Creating"
	RuntimeReady       RuntimeState = "Ready"
	RuntimeStopping    RuntimeState = "Stopping"
	RuntimeStopped     RuntimeState = "Stopped"
	RuntimeFailed      RuntimeState = "Failed"
	RuntimeUnavailable RuntimeState = "Unavailable"
)

type RuntimeStatus struct {
	State      RuntimeState `json:"state,omitempty"`
	Generation int64        `json:"generation,omitempty"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`

	AcceptedResetRevision *metav1.Time `json:"acceptedResetRevision,omitempty"`
}

// DataPlaneState is the lifecycle of the Sandbox interaction route.
// +kubebuilder:validation:Enum=Unknown;Pending;Publishing;Ready;Draining;Failed;Unavailable
type DataPlaneState string

const (
	DataPlaneUnknown     DataPlaneState = "Unknown"
	DataPlanePending     DataPlaneState = "Pending"
	DataPlanePublishing  DataPlaneState = "Publishing"
	DataPlaneReady       DataPlaneState = "Ready"
	DataPlaneDraining    DataPlaneState = "Draining"
	DataPlaneFailed      DataPlaneState = "Failed"
	DataPlaneUnavailable DataPlaneState = "Unavailable"
)

type DataPlaneStatus struct {
	State           DataPlaneState `json:"state,omitempty"`
	RouteGeneration int64          `json:"routeGeneration,omitempty"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}

type PlacementStatus struct {
	Attempt int64 `json:"attempt,omitempty"`

	FastletName   string    `json:"fastletName,omitempty"`
	FastletPodUID types.UID `json:"fastletPodUID,omitempty"`

	Recovery *RecoveryStatus `json:"recovery,omitempty"`
}

type RecoveryStatus struct {
	DetectedAt metav1.Time `json:"detectedAt"`
	Deadline   metav1.Time `json:"deadline"`
}

// ActionState is the observed lifecycle of one Action Binding.
// +kubebuilder:validation:Enum=Pending;Applying;Ready;Failed
type ActionState string

const (
	ActionPending  ActionState = "Pending"
	ActionApplying ActionState = "Applying"
	ActionReady    ActionState = "Ready"
	ActionFailed   ActionState = "Failed"
)

type ActionBindingStatus struct {
	Handler string      `json:"handler"`
	State   ActionState `json:"state"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Placement PlacementStatus `json:"placement,omitempty"`
	Runtime   RuntimeStatus   `json:"runtime,omitempty"`
	DataPlane DataPlaneStatus `json:"dataPlane,omitempty"`

	// +listType=map
	// +listMapKey=name
	InfraComponents []InfraComponentStatus `json:"infraComponents,omitempty"`

	// +listType=atomic
	ActionBindings []ActionBindingStatus `json:"actionBindings,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// InfraComponentState is the lifecycle of one named local component route.
// +kubebuilder:validation:Enum=Starting;Ready;Failed
type InfraComponentState string

const (
	InfraComponentStarting InfraComponentState = "Starting"
	InfraComponentReady    InfraComponentState = "Ready"
	InfraComponentFailed   InfraComponentState = "Failed"
)

type InfraComponentStatus struct {
	Name  string              `json:"name"`
	State InfraComponentState `json:"state"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}

// HasCondition reports whether a canonical condition currently has the given
// status and reason.
func (s *SandboxStatus) HasCondition(conditionType string, conditionStatus metav1.ConditionStatus, reason string) bool {
	if s == nil {
		return false
	}
	for index := range s.Conditions {
		condition := &s.Conditions[index]
		if condition.Type == conditionType && condition.Status == conditionStatus && condition.Reason == reason {
			return true
		}
	}
	return false
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Sandbox is the Schema for the sandboxes API.
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSpec   `json:"spec,omitempty"`
	Status SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxList contains a list of Sandbox.
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
