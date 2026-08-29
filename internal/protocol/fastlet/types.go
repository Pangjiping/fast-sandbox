package fastlet

import "time"

type UserProcessStartSource string

const (
	UserProcessStartRuntimeDirect         UserProcessStartSource = "runtime_direct"
	UserProcessStartSandboxInitUnreported UserProcessStartSource = "sandbox_init_unreported"
	UserProcessStartExistingRuntime       UserProcessStartSource = "existing_runtime"
	UserProcessStartUnknown               UserProcessStartSource = "unknown"
)

// SandboxSpec describes the desired state of a sandbox on a fastlet.
type SandboxSpec struct {
	SandboxID           string            `json:"sandboxId"`
	RequestID           string            `json:"requestId,omitempty"`
	ClaimUID            string            `json:"claimUid"`
	ClaimNamespace      string            `json:"claimNamespace,omitempty"`
	ClaimName           string            `json:"claimName"`
	InstanceGeneration  int64             `json:"instanceGeneration,omitempty"`
	RuntimeInstanceID   string            `json:"runtimeInstanceId,omitempty"`
	AssignmentAttempt   int64             `json:"assignmentAttempt,omitempty"`
	RouteGeneration     int64             `json:"routeGeneration,omitempty"`
	FastletPodUID       string            `json:"fastletPodUid,omitempty"`
	Image               string            `json:"image"`
	CPU                 string            `json:"cpu,omitempty"`
	Memory              string            `json:"memory,omitempty"`
	PIDs                int64             `json:"pids,omitempty"`
	RuntimeProfileHash  string            `json:"runtimeProfileHash,omitempty"`
	ResourceProfileHash string            `json:"resourceProfileHash,omitempty"`
	InfraRevision       string            `json:"infraRevision,omitempty"`
	Command             []string          `json:"command,omitempty"`
	Args                []string          `json:"args,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	WorkingDir          string            `json:"workingDir,omitempty"`
	// Network fields are Fastlet-local runtime material. They are populated
	// only after admission and are never accepted from the public/RPC API.
	NetworkSlotID        string `json:"-"`
	NetworkNamespacePath string `json:"-"`
	NetworkIP            string `json:"-"`
	NetworkGateway       string `json:"-"`
	NetworkDNSPath       string `json:"-"`
	NetworkPrivateCIDR   string `json:"-"`
	NetworkHostVeth      string `json:"-"`
}

type RuntimeState string

const (
	RuntimeStateUnknown     RuntimeState = "Unknown"
	RuntimeStatePending     RuntimeState = "Pending"
	RuntimeStateCreating    RuntimeState = "Creating"
	RuntimeStateReady       RuntimeState = "Ready"
	RuntimeStateStopping    RuntimeState = "Stopping"
	RuntimeStateStopped     RuntimeState = "Stopped"
	RuntimeStateFailed      RuntimeState = "Failed"
	RuntimeStateUnavailable RuntimeState = "Unavailable"
)

type DataPlaneState string

const (
	DataPlaneStateUnknown     DataPlaneState = "Unknown"
	DataPlaneStatePending     DataPlaneState = "Pending"
	DataPlaneStatePublishing  DataPlaneState = "Publishing"
	DataPlaneStateReady       DataPlaneState = "Ready"
	DataPlaneStateDraining    DataPlaneState = "Draining"
	DataPlaneStateFailed      DataPlaneState = "Failed"
	DataPlaneStateUnavailable DataPlaneState = "Unavailable"
)

type RuntimeObservation struct {
	State   RuntimeState `json:"state"`
	Message string       `json:"message,omitempty"`
}

type DataPlaneObservation struct {
	State   DataPlaneState `json:"state"`
	Message string         `json:"message,omitempty"`
}

// SandboxStatus is a structured Fastlet observation. Phase remains an
// implementation detail of SandboxManager and never crosses the wire.
type SandboxStatus struct {
	SandboxID          string                     `json:"sandboxId"`
	ClaimUID           string                     `json:"claimUid"`
	InstanceGeneration int64                      `json:"instanceGeneration,omitempty"`
	RuntimeInstanceID  string                     `json:"runtimeInstanceId,omitempty"`
	AssignmentAttempt  int64                      `json:"assignmentAttempt,omitempty"`
	RouteGeneration    int64                      `json:"routeGeneration,omitempty"`
	AcceptedGeneration int64                      `json:"acceptedGeneration,omitempty"`
	AppliedGeneration  int64                      `json:"appliedGeneration,omitempty"`
	Runtime            RuntimeObservation         `json:"runtime"`
	DataPlane          DataPlaneObservation       `json:"dataPlane"`
	InfraComponents    []InfraComponentDiagnostic `json:"infraComponents,omitempty"`
	ActionBindings     []ActionBindingStatus      `json:"actionBindings,omitempty"`
	CreatedAt          int64                      `json:"createdAt"` // Unix timestamp for orphan cleanup
}

// ActionBindingStatus is Fastlet-local observed state. The digest and fence
// fields are intentionally internal and are not projected to the public
// FastPath or Sandbox CRD status.
type ActionBindingStatus struct {
	Handler                    string    `json:"handler"`
	State                      string    `json:"state"`
	ObservedSpecGeneration     int64     `json:"observedSpecGeneration,omitempty"`
	DesiredInputDigest         string    `json:"desiredInputDigest,omitempty"`
	AppliedInputDigest         string    `json:"appliedInputDigest,omitempty"`
	ObservedAssignmentAttempt  int64     `json:"observedAssignmentAttempt,omitempty"`
	ObservedInstanceGeneration int64     `json:"observedInstanceGeneration,omitempty"`
	ObservedNetworkGeneration  int64     `json:"observedNetworkGeneration,omitempty"`
	LastTransitionTime         time.Time `json:"lastTransitionTime,omitempty"`
	Message                    string    `json:"message,omitempty"`
}

// SandboxActionStatus is retained as an internal source compatibility alias
// while the controller and runtime drivers migrate to ActionBindingStatus.
type SandboxActionStatus = ActionBindingStatus

type InfraComponentDiagnostic struct {
	Component               string `json:"component"`
	Protocol                string `json:"protocol,omitempty"`
	Port                    uint32 `json:"port,omitempty"`
	State                   string `json:"state"`
	ObservedRouteGeneration int64  `json:"observedRouteGeneration,omitempty"`
	Message                 string `json:"message,omitempty"`
}

// FastletStatus represents the current status of a fastlet (internal use).
type FastletStatus struct {
	FastletID           string           `json:"fastletId"`
	NodeName            string           `json:"nodeName"`
	Capacity            int              `json:"capacity"`
	Images              []string         `json:"images,omitempty"`
	SandboxStatuses     []SandboxStatus  `json:"sandboxStatuses"`
	Admission           AdmissionStatus  `json:"admission"`
	RuntimeReady        bool             `json:"runtimeReady"`
	Recovering          bool             `json:"recovering"`
	Draining            bool             `json:"draining"`
	FastletPodUID       string           `json:"fastletPodUid,omitempty"`
	ResourceProfileHash string           `json:"resourceProfileHash,omitempty"`
	InfraRevision       string           `json:"infraRevision,omitempty"`
	InfraReady          bool             `json:"infraReady"`
	PreparedArtifacts   []string         `json:"preparedArtifacts,omitempty"`
	RegistryRevision    string           `json:"registryRevision,omitempty"`
	WarmImages          []WarmImageState `json:"warmImages,omitempty"`
}

type WarmImageState struct {
	Image   string `json:"image"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type FastletErrorCode string

const (
	ErrorCapacityRejected   FastletErrorCode = "CapacityRejected"
	ErrorDraining           FastletErrorCode = "Draining"
	ErrorInProgress         FastletErrorCode = "InProgress"
	ErrorConflict           FastletErrorCode = "Conflict"
	ErrorStaleGeneration    FastletErrorCode = "StaleGeneration"
	ErrorStaleAssignment    FastletErrorCode = "StaleAssignment"
	ErrorRuntimeUnavailable FastletErrorCode = "RuntimeUnavailable"
	ErrorNetworkUnavailable FastletErrorCode = "NetworkUnavailable"
	ErrorInfraUnavailable   FastletErrorCode = "InfraUnavailable"
	ErrorActionUnavailable  FastletErrorCode = "ActionUnavailable"
	ErrorUnknownOutcome     FastletErrorCode = "UnknownOutcome"
	ErrorNotFound           FastletErrorCode = "NotFound"
	ErrorGenerationFenced   FastletErrorCode = "GenerationFenced"
	ErrorProfileMismatch    FastletErrorCode = "ProfileMismatch"
)

type FastletOutcome string

const (
	OutcomeRejectedBeforeSideEffects FastletOutcome = "RejectedBeforeSideEffects"
	OutcomeInProgress                FastletOutcome = "InProgress"
	OutcomeCreated                   FastletOutcome = "Created"
	OutcomeFailedNeedsCleanup        FastletOutcome = "FailedNeedsCleanup"
	OutcomeUnknown                   FastletOutcome = "Unknown"
	OutcomeGenerationFenced          FastletOutcome = "GenerationFenced"
)

type FastletError struct {
	Code      FastletErrorCode `json:"code"`
	Message   string           `json:"message"`
	Retryable bool             `json:"retryable"`
	Outcome   FastletOutcome   `json:"outcome"`
	Cause     error            `json:"-"`
}

func (e *FastletError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *FastletError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Code) + ": " + e.Message
}

type SandboxIdentity struct {
	RequestID          string `json:"requestId,omitempty"`
	SandboxUID         string `json:"sandboxUid"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	RuntimeInstanceID  string `json:"runtimeInstanceId"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
	RouteGeneration    int64  `json:"routeGeneration,omitempty"`
	FastletPodUID      string `json:"fastletPodUid"`
}

type AdmissionStatus struct {
	Capacity int `json:"capacity"`
	Creating int `json:"creating"`
	Running  int `json:"running"`
	Deleting int `json:"deleting"`
	Used     int `json:"used"`
}

type CreateSandboxRequest struct {
	Identity       SandboxIdentity      `json:"identity"`
	Sandbox        SandboxSpec          `json:"sandbox"`
	SpecGeneration int64                `json:"specGeneration,omitempty"`
	ActionBindings []ActionBindingInput `json:"actionBindings,omitempty"`
	Completion     CreateCompletion     `json:"completion,omitempty"`
}

type CreateCompletion string

const (
	CreateCompletionReady        CreateCompletion = "READY"
	CreateCompletionRuntimeReady CreateCompletion = "RUNTIME_READY"
)

type CreateSandboxResponse struct {
	Accepted   bool            `json:"accepted"`
	Created    bool            `json:"created"`
	InProgress bool            `json:"inProgress"`
	Sandbox    *SandboxStatus  `json:"sandbox,omitempty"`
	Admission  AdmissionStatus `json:"admission"`
	Error      *FastletError   `json:"error,omitempty"`
}

type InspectSandboxRequest struct {
	Identity SandboxIdentity `json:"identity"`
}

type InspectSandboxResponse struct {
	Sandbox *SandboxStatus `json:"sandbox,omitempty"`
	Error   *FastletError  `json:"error,omitempty"`
}

type DeleteSandboxRequest struct {
	Identity SandboxIdentity `json:"identity"`
}

type DeleteSandboxResponse struct {
	Accepted bool          `json:"accepted"`
	Error    *FastletError `json:"error,omitempty"`
}

type ActionBindingInput struct {
	Handler     string `json:"handler"`
	Input       string `json:"input"`
	InputDigest string `json:"inputDigest"`
}

// ReconcileBindingsRequest carries persisted desired Binding state from
// the Sandbox Controller. Lifecycle Hooks are always dispatched by Fastlet.
type ReconcileBindingsRequest struct {
	Identity       SandboxIdentity      `json:"identity"`
	SpecGeneration int64                `json:"specGeneration"`
	ActionBindings []ActionBindingInput `json:"actionBindings"`
}

type ReconcileBindingsResponse struct {
	Sandbox *SandboxStatus `json:"sandbox,omitempty"`
	Error   *FastletError  `json:"error,omitempty"`
}

type SetDrainingRequest struct {
	Draining bool   `json:"draining"`
	Reason   string `json:"reason,omitempty"`
}

type SetDrainingResponse struct {
	Draining bool `json:"draining"`
}

type RuntimeDiagnostics struct {
	RuntimeProfileHash string `json:"runtimeProfileHash"`
	InfraRevision      string `json:"infraRevision,omitempty"`
	InfraState         string `json:"infraState,omitempty"`
	InfraMessage       string `json:"infraMessage,omitempty"`
	State              string `json:"state"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// SandboxDiagnosticEvent is a bounded Fastlet-side lifecycle record. It is
// platform diagnostics, not stdout/stderr from a process inside the Sandbox.
type SandboxDiagnosticEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	Phase     string    `json:"phase,omitempty"`
	Message   string    `json:"message"`
}

type SandboxDiagnosticsRequest struct {
	Identity SandboxIdentity `json:"identity"`
	Limit    int             `json:"limit,omitempty"`
}

type SandboxDiagnosticsResponse struct {
	Sandbox *SandboxStatus           `json:"sandbox,omitempty"`
	Events  []SandboxDiagnosticEvent `json:"events,omitempty"`
	Error   *FastletError            `json:"error,omitempty"`
}

type CacheCursor struct {
	Epoch     string `json:"epoch,omitempty"`
	Revision  uint64 `json:"revision,omitempty"`
	ForceFull bool   `json:"forceFull,omitempty"`
}

type CacheSnapshot struct {
	Epoch    string   `json:"epoch"`
	Revision uint64   `json:"revision"`
	Full     bool     `json:"full"`
	Complete bool     `json:"complete"`
	Images   []string `json:"images,omitempty"`
}

type HeartbeatRequest struct {
	Cache CacheCursor `json:"cache"`
}

type HeartbeatResponse struct {
	FastletStatus
	Sequence    uint64             `json:"sequence"`
	ObservedAt  time.Time          `json:"observedAt"`
	Cache       CacheSnapshot      `json:"cache"`
	Diagnostics RuntimeDiagnostics `json:"diagnostics"`
}
