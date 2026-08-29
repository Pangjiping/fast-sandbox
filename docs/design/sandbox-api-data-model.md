# Sandbox API Data Model

Status: implemented for `sandbox.fast.io/v1alpha2`.

This document owns the public CRD and FastPath data model. The Pod-local
Handler protocol and lifecycle ordering are defined in
[sandbox-actions.md](sandbox-actions.md).

## 1. Ownership and consistency

| Data | Owner | Source of truth |
| --- | --- | --- |
| Available Action Handlers and Hook subscriptions | Pool administrator | `SandboxPool.spec.actionHandlers` |
| Ordered Handler bindings and opaque input | Sandbox user | `Sandbox.spec.actionBindings` |
| Placement, recovery and durable observed state | Sandbox Controller | `Sandbox.status` |
| Live runtime, data-plane and Binding state | Fastlet | Fastlet-local state |
| Per-Hook progress, invocation IDs, retry journal and Handler incarnation | Fastlet | Fastlet-local private state |
| Handler-specific applied policy | Action Handler | Handler-owned state |

CRD Status and FastPath `SandboxInfo` are intentionally not identical:

- CRD Status is the durable Controller projection needed for reconciliation,
  scheduling and `kubectl` observation.
- FastPath `SandboxInfo` is a smaller, real-time view returned by the assigned
  Fastlet. It contains only information Fastlet can determine directly.

FastPath never manufactures live state from a partially projected CRD Status.

## 2. SandboxPool CRD

### 2.1 Handler declaration

The Pool declares which Pod-local HTTP receivers a Sandbox may bind:

```go
type LifecycleHook string

const (
	LifecycleHookRuntimeReady   LifecycleHook = "sandbox.runtime-ready"
	LifecycleHookDataPlaneReady LifecycleHook = "sandbox.data-plane-ready"
)

type ActionHandler struct {
	Name           string          `json:"name"`
	TargetHTTPPort int32           `json:"targetHTTPPort"`
	Hooks          []LifecycleHook `json:"hooks,omitempty"`
}
```

`TargetHTTPPort` is reached at `127.0.0.1` inside the Fastlet Pod. The actual
Handler container or process is declared in `spec.fastletTemplate`.

`hooks` is a subscription set. An empty set defines a config-only Handler: it
receives Binding synchronization and terminal removal, but no lifecycle Hook.
The type remains a string alias for future extension, while the current
validation rejects names unsupported by the running platform version.

The Pool does not configure operation names, URL paths, methods, timeouts,
retry policies or failure policies. Those are fixed platform behavior.

### 2.2 Pool Status

```go
type SandboxPoolStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	CurrentPods   int32 `json:"currentPods,omitempty"`
	ReadyPods     int32 `json:"readyPods,omitempty"`
	TotalFastlets int32 `json:"totalFastlets,omitempty"`
	IdleFastlets  int32 `json:"idleFastlets,omitempty"`
	BusyFastlets  int32 `json:"busyFastlets,omitempty"`

	RuntimeRevision  string `json:"runtimeRevision,omitempty"`
	InfraRevision    string `json:"infraRevision,omitempty"`
	FastletRevision  string `json:"fastletRevision,omitempty"`
	PreparedFastlets int32  `json:"preparedFastlets,omitempty"`

	WarmImages []WarmImageStatus  `json:"warmImages,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

There is no Action Handler summary in Pool Status. A Handler is useful only in
the context of a particular Sandbox Binding, so readiness belongs in
`Sandbox.status.actionBindings`. Pod/container health is already represented
by `ReadyPods` and Pool Conditions.

There is also no Registry nested status. Registry configuration is a
Namespace-level dependency rather than Pool Spec. Compilation or propagation
problems use the Pool `RegistryReady` Condition and its `message`.

`FastletRevision` is the compatibility revision used for rollout and
placement. `RuntimeRevision` and `InfraRevision` are Pool-level diagnostics;
none of them is copied into every Sandbox Status.

## 3. Sandbox CRD Spec

```go
type SandboxSpec struct {
	Image      string          `json:"image"`
	Command    []string        `json:"command,omitempty"`
	Args       []string        `json:"args,omitempty"`
	Envs       []corev1.EnvVar `json:"envs,omitempty"`
	WorkingDir string          `json:"workingDir,omitempty"`

	ExpireTime             *metav1.Time `json:"expireTime,omitempty"`
	FailurePolicy          FailurePolicy `json:"failurePolicy,omitempty"`
	RecoveryTimeoutSeconds int32         `json:"recoveryTimeoutSeconds,omitempty"`
	ResetRevision          *metav1.Time  `json:"resetRevision,omitempty"`
	PoolRef                string        `json:"poolRef"`

	// Atomic because declaration order is Handler execution order.
	ActionBindings []ActionBinding `json:"actionBindings,omitempty"`
}

type ActionBinding struct {
	Handler string `json:"handler"`
	Input   string `json:"input"`
}
```

The Handler name must be unique in the ordered list and must exist in the
referenced Pool. `input` is an opaque UTF-8 string with a 64 KiB limit. Fast
Sandbox does not parse JSON, canonicalize whitespace, reorder keys or otherwise
interpret it. `""` and the literal string `"null"` are valid values.

Kubernetes `metadata.labels` stores the public FastPath `metadata` map. It is
user-defined control-plane metadata for filtering and ownership; it is not
implicitly injected into the Sandbox process or forwarded to Handlers.

## 4. Complete Sandbox CRD Status

```go
type SandboxStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Placement PlacementStatus `json:"placement,omitempty"`
	Runtime   RuntimeStatus   `json:"runtime,omitempty"`
	DataPlane DataPlaneStatus `json:"dataPlane,omitempty"`

	InfraComponents []InfraComponentStatus `json:"infraComponents,omitempty"`
	ActionBindings  []ActionBindingStatus  `json:"actionBindings,omitempty"`
	Conditions      []metav1.Condition     `json:"conditions,omitempty"`
}

type PlacementStatus struct {
	Attempt       int64           `json:"attempt,omitempty"`
	FastletName   string          `json:"fastletName,omitempty"`
	FastletPodUID types.UID       `json:"fastletPodUID,omitempty"`
	Recovery      *RecoveryStatus `json:"recovery,omitempty"`
}

type RecoveryStatus struct {
	DetectedAt metav1.Time `json:"detectedAt"`
	Deadline   metav1.Time `json:"deadline"`
}

type RuntimeStatus struct {
	State      RuntimeState `json:"state,omitempty"`
	Generation int64        `json:"generation,omitempty"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`

	AcceptedResetRevision *metav1.Time `json:"acceptedResetRevision,omitempty"`
}

type DataPlaneStatus struct {
	State           DataPlaneState `json:"state,omitempty"`
	RouteGeneration int64          `json:"routeGeneration,omitempty"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}

type InfraComponentStatus struct {
	Name  string              `json:"name"`
	State InfraComponentState `json:"state"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}

type ActionBindingStatus struct {
	Handler string      `json:"handler"`
	State   ActionState `json:"state"`

	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Message            string       `json:"message,omitempty"`
}
```

### 4.1 Field semantics

`observedGeneration`
: Highest `metadata.generation` for which the Controller successfully made a
  reconciliation decision. It does not mean Fastlet or every Handler has
  applied that generation. Component progress is represented by the structured
  states and Fastlet-private applied fences.

`placement.attempt`
: Monotonic scheduling fence. It increases whenever Controller assigns a new
  Fastlet and remains a high-water mark after placement is cleared, preventing
  an old Fastlet response from becoming authoritative again.

`placement.fastletName` and `fastletPodUID`
: Identify the active Fastlet process. Pod UID distinguishes a replacement Pod
  that reused the same name. `NodeName` is unnecessary: the Fastlet Pod already
  supplies that relation and routing uses the assigned Pod identity.

`placement.recovery`
: Explicit recovery timer for an unreachable assigned Fastlet. It replaces
  interpreting a Condition `reason` as hidden workflow state.

`runtime.generation`
: Controller-managed generation of the concrete runtime instance under the
  same CRD UID. Runtime recreation, reset and recovery increment it; ordinary
  Spec edits do not. It cannot reuse `metadata.generation`, because a Spec may
  change without recreating the runtime and one Spec generation may be retried
  across more than one concrete runtime.

`runtime.acceptedResetRevision`
: Latest `spec.resetRevision` already consumed. This makes reset idempotent.

`dataPlane.routeGeneration`
: Status-managed fence for the currently published route. The proxy credential
  carries this value, so credentials for a drained/replaced route stop matching.
  It is not desired state and therefore has no Spec counterpart.

`infraComponents`
: Independent per-component observation. It is not nested under DataPlane:
  component process health and proxy route publication have different owners
  and failure modes.

`actionBindings`
: Status counterpart of the ordered Spec list. A Binding is `Ready` only when
  its current input has completed `SetBinding` and every subscribed Hook already
  reached by this Sandbox has succeeded for the current Handler incarnation.
  Per-Hook history and retries stay private to Fastlet.

Every structured state has `lastTransitionTime` and one diagnostic `message`.
There is no duplicate `reason` field. Kubernetes Conditions retain their
standard `reason` because it is required by `metav1.Condition`, but Controller
never reads it back as state-machine input.

### 4.2 State enums

```text
Runtime:       Unknown, Pending, Creating, Ready, Stopping, Stopped,
               Failed, Unavailable
DataPlane:     Unknown, Pending, Publishing, Ready, Draining,
               Failed, Unavailable
InfraComponent: Starting, Ready, Failed
ActionBinding:  Pending, Applying, Ready, Failed
```

### 4.3 Overall Ready

Version 1 defines only one Sandbox Condition type: `Ready`.

```text
Ready =
    status.observedGeneration == metadata.generation
    && Runtime.State == Ready
    && DataPlane.State == Ready
    && every current InfraComponent.State == Ready
    && every current ActionBinding.State == Ready
```

The Condition is the aggregate answer for clients; the structured states show
which prerequisite is still converging. Separate Runtime/DataPlane Conditions
would duplicate those fields without improving the state machine.

### 4.4 Expiration and recovery

Expiration tears down the current runtime and clears placement, but it is not
an irreversible tombstone while the CRD still exists. Extending/removing
`spec.expireTime`, or submitting a newer `spec.resetRevision`, may move the
Sandbox back to Pending and create a new `runtime.generation`.

## 5. FastPath data model

### 5.1 Live SandboxInfo

```proto
message SandboxInfo {
  SandboxIdentity identity = 1;
  int64 applied_generation = 2;
  RuntimeInfo runtime = 3;
  DataPlaneInfo data_plane = 4;
  repeated InfraComponentInfo infra_components = 5;
  repeated ActionBindingInfo action_bindings = 6;
  bool ready = 7;
}
```

`CreateSandbox` and `GetSandbox` obtain this structure from Fastlet, not by
waiting for Controller to fill CRD Status. Runtime and DataPlane carry only
their live state because Fastlet cannot authoritatively provide the durable
Controller timestamps and recovery fields. Infra and Binding entries retain a
diagnostic message; Binding also retains transition time because Fastlet owns
that transition.

`applied_generation` is the highest Sandbox Spec generation for which Fastlet's
current Binding list has fully converged. It is used with
`UpdateSandboxResponse.committed_generation` to observe that an update has
reached Fastlet and all Binding barriers have completed. It is intentionally
different from CRD `status.observedGeneration`, which means only that the
Controller processed the Spec.

### 5.2 Creation completion

```proto
enum CreateCompletion {
  CREATE_COMPLETION_UNSPECIFIED = 0;
  CREATE_COMPLETION_READY = 1;
  CREATE_COMPLETION_RUNTIME_READY = 2;
}
```

`UNSPECIFIED` defaults to `READY`:

- `READY` returns only when the Fastlet-local overall Ready predicate is true.
- `RUNTIME_READY` returns after the concrete runtime is ready; routes, Infra
  Components and Bindings may still be converging.

The wait is local to the assigned Fastlet and does not require a CRD Status
watch round trip. There is no `WaitSandboxReady` RPC.

### 5.3 Reference and concurrency fences

```proto
message SandboxReference {
  reserved 1;
  reserved "sandbox_uid";
  NamespacedName namespaced_name = 2;
  string expected_uid = 3;
}
```

Namespaced name locates the Kubernetes resource. Optional `expected_uid`
protects a caller from accidentally operating on a same-name replacement.
Delete converts it into a Kubernetes UID precondition; Update rechecks it after
every resourceVersion conflict.

`expected_generation` on Get/Update/ResolveEndpoint normally comes from a
previous Create/Get/Update response. It is a lower-bound/read-after-write fence,
not identity. A RouteCache entry below that generation triggers one direct
Kubernetes GET; an entry at or above it needs no apiserver read.

### 5.4 Update, Get and ResolveEndpoint

- `UpdateSandbox` updates the CRD and returns `committed_generation`. It does
  not synchronously wait for Fastlet/Handlers.
- `GetSandbox` resolves the CRD identity/assignment, calls the assigned
  Fastlet's Inspect endpoint, and returns the real-time `SandboxInfo`.
- Clients observe an update by polling Get until
  `applied_generation >= committed_generation` and the relevant structured
  state is Ready.
- `ResolveEndpoint` performs the same live inspection, requires overall Ready
  and the requested component route to be Ready, then issues a route-fenced
  short-lived credential. It never waits internally for readiness.

### 5.5 Expected control-plane IO

On a healthy, synchronized cache:

| RPC | Kubernetes API IO | Fastlet IO |
| --- | ---: | ---: |
| Create | one CRD CREATE | one Create call; Handler calls stay Pod-local |
| Get | zero; one GET only if cache is behind `expected_generation` | one Inspect call |
| Update | one UPDATE, plus conflict retries | asynchronous ReconcileBindings by Controller |
| ResolveEndpoint | zero; one GET only if cache is behind | one Inspect call |
| Delete | one DELETE with UID precondition | asynchronous terminal cleanup |

Controller-to-Fastlet calls and Fastlet-to-Handler HTTP calls are not
Kubernetes apiserver IO. CRD Status writes remain asynchronous Controller
projection and are not on FastPath's successful create/read critical path.

## 6. Handler wire boundary

Binding presence is explicit:

```go
type BindingPayload struct {
	Input *string `json:"input"`
}
```

- non-nil means create/update the Binding; the value may be empty;
- nil means remove one Binding while the Sandbox remains alive;
- terminal `RemoveBinding` has no input.

The full request envelope, ordering, retry fences, Hook replay and deletion
deadline are specified in [sandbox-actions.md](sandbox-actions.md).

## 7. Compatibility

The CRD version remains `v1alpha2`; the feature does not introduce an API
version bump. Removed protobuf field numbers/names remain reserved. Generated
CRDs, Go protobufs and Python protobufs must be regenerated together whenever
these source structures change.
