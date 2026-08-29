// Package action defines the versioned Pod-local Action Handler protocol.
package action

const APIVersion = "sandbox.fast.io/actions/v1"

type Operation string

const (
	OperationSetBinding    Operation = "SET_BINDING"
	OperationLifecycleHook Operation = "LIFECYCLE_HOOK"
	OperationRemoveBinding Operation = "REMOVE_BINDING"
)

type LifecycleHook string

const (
	LifecycleHookRuntimeReady   LifecycleHook = "sandbox.runtime-ready"
	LifecycleHookDataPlaneReady LifecycleHook = "sandbox.data-plane-ready"
)

type SandboxIdentity struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type Revision struct {
	SpecGeneration    int64  `json:"specGeneration"`
	RuntimeInstanceID string `json:"runtimeInstanceId,omitempty"`
	AttachmentID      string `json:"attachmentId,omitempty"`
	RouteGeneration   int64  `json:"routeGeneration,omitempty"`
}

type NetworkAttachment struct {
	IP          string `json:"ip,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	PrivateCIDR string `json:"privateCidr,omitempty"`
	HostVeth    string `json:"hostVeth,omitempty"`
}

type Attachment struct {
	Network NetworkAttachment `json:"network"`
}

// BindingPayload uses pointer presence deliberately. A non-nil Input sets the
// exact opaque value (including "" or the literal "null"); nil removes a
// Binding from a still-live Sandbox.
type BindingPayload struct {
	Input *string `json:"input"`
}

type LifecycleHookPayload struct {
	Name     LifecycleHook `json:"name"`
	Sequence int64         `json:"sequence"`
}

type Request struct {
	APIVersion   string                `json:"apiVersion"`
	Operation    Operation             `json:"operation"`
	InvocationID string                `json:"invocationId"`
	Sandbox      SandboxIdentity       `json:"sandbox"`
	Revision     Revision              `json:"revision"`
	Attachment   Attachment            `json:"attachment"`
	Binding      *BindingPayload       `json:"binding,omitempty"`
	Hook         *LifecycleHookPayload `json:"hook,omitempty"`
}

type HandlerStatus struct {
	APIVersion string `json:"apiVersion"`
	Ready      bool   `json:"ready"`
	InstanceID string `json:"instanceId"`
	Message    string `json:"message,omitempty"`
}
