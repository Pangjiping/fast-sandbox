# Sandbox Actions

> Status: detailed design draft
>
> Scope: a Fastlet-native mechanism for delivering per-Sandbox lifecycle,
> network identity, and opaque desired input to trusted sidecars in a Fastlet
> Pod.

## Summary

A Sandbox Action is a named handler declared by a `SandboxPool` and running in
the Fastlet Pod. At fixed Sandbox lifecycle points, Fastlet invokes these
handlers through a uniform local HTTP protocol. Each Sandbox may provide one
opaque JSON input per Action name. Fastlet combines that input with
authoritative instance and network metadata, then delivers `CREATE`, `UPDATE`,
`DELETE`, and `SYNC` operations.

The primary use case is a shared OpenSandbox Egress daemon, but the abstraction
is not specific to Egress. Audit, QoS, and other trusted Pod-local agents can
implement the same protocol. Each handler continues to own its
application-specific schema and behavior.

## Background and motivation

An `InfraComponent` describes an artifact or process injected into each
Sandbox and managed by `sandbox-init`. A Fastlet Pod sidecar has different
ownership and lifecycle semantics:

- its container, image, process, resources, and privileges come from
  `fastletTemplate`;
- one physical process serves multiple Sandboxes;
- it needs authoritative metadata such as Sandbox UID, network generation,
  IP, host veth, and netns path;
- some handlers must finish preparing per-Sandbox security state before the
  runtime can start;
- desired per-Sandbox input must survive Fastlet and handler restarts.

Modeling such a sidecar as a Pod-placement `InfraComponent` would duplicate
the container definition already owned by `fastletTemplate`, while still not
solving lifecycle metadata delivery. Exposing a Pod port solves reachability
only; it does not define state, ordering, a creation barrier, reconciliation,
or recovery.

Sandbox Actions fill those gaps without taking ownership of the sidecar
workload. Administrators continue to deploy sidecars through
`fastletTemplate`.

## Goals

- Define a small, Fastlet-native lifecycle extension protocol.
- Deliver authoritative instance and network metadata in a versioned
  envelope.
- Carry opaque, per-Sandbox input whose schema is owned by the Action.
- Apply the initial Action input before starting the runtime.
- Support declarative online updates through `UpdateSandbox`.
- Fence stale assignment, runtime instance, Network Slot, and update requests.
- Recover from failure through idempotency and full-set synchronization.
- Keep hook placement, endpoint scope, deadlines, and failure semantics under
  Fastlet control instead of exposing them as Pool configuration.

## Non-goals

- Managing or injecting sidecar containers.
- Replacing Infra Components inside a Sandbox.
- Providing an arbitrary script runner, webhook, or external event bus.
- Defining the schema of any Action's opaque input.
- Providing distributed transactions across Actions.
- Persisting credentials or other secrets in Action input.
- Translating an Action's application-specific protocol.

## Example: Egress daemon

This section shows how an Egress daemon uses Sandbox Actions. Egress is an
example consumer of the generic protocol; it is not a component managed or
implemented by Fastlet.

### Pod layout and ownership

One Egress daemon runs as a sidecar in a Fastlet Pod and manages isolated
policy state for all Sandboxes assigned to that Pod:

```text
Fastlet Pod
├── fastlet
├── fastlet-proxy
├── egress-daemon
│   ├── Sandbox A: Policy A / nftables state A
│   └── Sandbox B: Policy B / nftables state B
├── Sandbox A
└── Sandbox B
```

The sidecar is part of `fastletTemplate`. Sandbox Actions do not create the
container or own its image, process, resources, or privileges:

```yaml
spec:
  fastletTemplate:
    spec:
      containers:
        - name: fastlet
          image: fast-sandbox/fastlet:v1

        - name: egress-daemon
          image: opensandbox/egress:v1
          args:
            - --profile=pod
            - --listen=127.0.0.1:18080
          securityContext:
            capabilities:
              add: [NET_ADMIN]

  sandboxActions:
    - name: egress
      operations: [CREATE, UPDATE, DELETE, SYNC]
      localPort: 18080
```

The Egress daemon shares the Fastlet Pod network namespace and listens only on
Pod loopback. Each Sandbox has a separate network namespace and cannot reach
that listener. Fastlet calls the fixed internal endpoint:

```text
POST http://127.0.0.1:18080/_fastlet/v1/actions
```

The Action name `egress` is a logical key connecting Pool declaration,
per-Sandbox input, status, and protocol calls. It does not need to equal the
sidecar container name.

### Creating a Sandbox with initial policy

The Sandbox supplies policy input under the `egress` Action name:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: agent-a
spec:
  poolRef: egress-pool
  image: python:3.13
  actions:
    egress:
      apiVersion: opensandbox.io/egress/v1alpha1
      policy:
        defaultAction: deny
        allow:
          - domain: api.openai.com
            ports: [443]
```

Fastlet does not interpret this policy. It validates only that `egress` is
declared by the selected Pool and that its input is valid, bounded JSON. The
Egress daemon owns the input schema, versioning, defaulting, and validation.

Creation follows a synchronous, fail-closed sequence:

```text
CreateSandbox(initial Egress input)
-> Fastlet prepares and persists the Sandbox Network Slot
-> Fastlet sends Egress CREATE(network identity + input)
-> Egress creates the Subject and installs deny-first rules and initial policy
-> Egress acknowledges the applied input digest
-> Fastlet starts the Sandbox runtime
-> CreateSandbox completes
```

There is no interval in which the runtime is running without its initial
Egress state. If the daemon is unavailable, the input is invalid, or rule
installation fails, Fastlet does not start the runtime.

### Updating and recovering policy

`UpdateSandbox` completely replaces `Sandbox.spec.actions.egress`. Fastlet
then sends `UPDATE`, and the Egress daemon atomically applies the complete new
policy. `UpdateSandbox` success means desired state was committed; a caller
that requires application confirmation waits for the `egress` Action to
become Ready.

The Egress handler implements the four generic operations as follows:

| Operation | Egress behavior |
| --- | --- |
| `CREATE` | Create the Subject and install deny-first rules plus initial policy |
| `UPDATE` | Atomically replace the complete policy input |
| `DELETE` | Remove Subject, policy, DNS state, and nftables resources |
| `SYNC` | Reconcile all live Sandboxes and remove stale state |

Requests are idempotent and fenced by the Sandbox assignment, runtime
instance, and network generations. After either Fastlet or the daemon
restarts, `SYNC` reconstructs the full desired Egress state. The remaining
sections define these generic API and protocol guarantees in detail.

## API model

### Pool declaration

```yaml
spec:
  sandboxActions:
    - name: egress
      operations: [CREATE, UPDATE, DELETE, SYNC]
      localPort: 18080
```

Conceptual Go types:

```go
type SandboxActionOperation string

const (
    SandboxActionCreate SandboxActionOperation = "CREATE"
    SandboxActionUpdate SandboxActionOperation = "UPDATE"
    SandboxActionDelete SandboxActionOperation = "DELETE"
    SandboxActionSync   SandboxActionOperation = "SYNC"
)

type SandboxAction struct {
    Name       string                   `json:"name"`
    Operations []SandboxActionOperation `json:"operations"`
    LocalPort  int32                    `json:"localPort"`
}
```

Validation requirements:

- `name` is a unique DNS label;
- `localPort` is unique, valid, and not reserved by the platform;
- `operations` is non-empty and contains no duplicates;
- every operation is supported by Fastlet;
- the number of Actions in one Pool is bounded.

The handler address is fixed:

```text
http://127.0.0.1:<port>/_fastlet/v1/actions
```

`localPort` is intentionally flat. Version 1 has exactly one transport: HTTP
over Fastlet Pod loopback, with a fixed path and protocol. A nested
`localHTTP` object would imply configurable HTTP properties or transport
polymorphism that the API does not provide. If a materially different
transport such as a Unix socket is needed later, it should be introduced as an
explicit new API shape based on concrete requirements.

The Pool administrator cannot choose another host or path, enable redirects,
inject custom headers, or change operation timing and failure policy. Changes
to `sandboxActions` or the corresponding `fastletTemplate` roll the Fastlet
Pods through the existing Pool revision mechanism.

An Action name is a logical capability key. It does not need to match the
Kubernetes sidecar container name. Fastlet only requires a conforming local
handler on the declared port.

### Sandbox desired input

```yaml
spec:
  actions:
    egress:
      apiVersion: opensandbox.io/egress/v1alpha1
      policy:
        defaultAction: deny
```

Conceptual API type:

```go
type SandboxSpec struct {
    // Actions contains opaque desired input for Pool-declared Actions.
    Actions map[string]runtime.RawExtension `json:"actions,omitempty"`
}
```

Fast Sandbox validates only that:

- the map key is declared by the selected Pool;
- the value is syntactically valid JSON;
- per-Action and per-Sandbox size limits are not exceeded.

The handler owns input schema, validation, defaults, and compatibility. Fast
Sandbox neither declares nor interprets an `inputType`. An Action may carry an
`apiVersion` or another version marker inside its opaque input.

A missing map value is delivered as JSON `null`. A handler that requires input
must reject `CREATE` when it receives `null`; Fastlet then does not start the
runtime. This keeps Action-specific required-field and defaulting semantics out
of the Pool API.

Suggested limits are 16 Actions per Pool, 64 KiB per Action input, and 256 KiB
for all Action inputs on one Sandbox.

### Create API

FastPath `CreateRequest` carries Action input without interpreting it:

```protobuf
message SandboxActionInput {
  string name = 1;
  bytes input_json = 2;
}

message CreateRequest {
  // Existing fields omitted.
  repeated SandboxActionInput actions = 13;
}
```

The normalized Action map is part of the immutable create-intent hash. A retry
that reuses a request ID with different Action input returns a conflict.

### Update API

`UpdateSandbox` remains the generic API for committing typed changes to
Sandbox desired state. It does not become an unrestricted replacement of the
entire Sandbox spec. Action input is the first extensible state domain that can
converge online:

```protobuf
message UpdateSandboxAction {
  string name = 1;
  bytes input_json = 2;
  string expected_input_digest = 3;
}

message UpdateRequest {
  string sandbox_name = 1;
  string namespace = 2;

  oneof update {
    // Existing mutations omitted.
    UpdateSandboxAction action = 9;
  }
}
```

An Action update completely replaces one opaque input. It is not JSON Patch,
Merge Patch, or an imperative command sent to the plugin. The optional
`expected_input_digest` provides compare-and-swap semantics. A mismatch with
the current desired input returns Conflict/Aborted.

FastPath writes the new input to `Sandbox.spec.actions`; clients writing the
CRD directly update the same field. There is no second desired-state copy used
only by FastPath. Consistent with the existing API, `UpdateSandbox` success
means that desired state was committed. Status or a wait operation reports
whether the handler has applied it.

Fields such as image, command, environment, Pool, runtime profile, and resource
shape retain their existing immutable or reset/recreate semantics. Any future
online update must be introduced as an explicit typed mutation rather than
turning `UpdateSandbox` into an unconstrained spec patch.

### Status

```go
type SandboxActionState string

const (
    SandboxActionPending  SandboxActionState = "Pending"
    SandboxActionApplying SandboxActionState = "Applying"
    SandboxActionReady    SandboxActionState = "Ready"
    SandboxActionFailed   SandboxActionState = "Failed"
)

type SandboxActionStatus struct {
    Name                       string             `json:"name"`
    State                      SandboxActionState `json:"state"`
    ObservedSandboxGeneration  int64              `json:"observedSandboxGeneration,omitempty"`
    DesiredInputDigest         string             `json:"desiredInputDigest,omitempty"`
    AppliedInputDigest         string             `json:"appliedInputDigest,omitempty"`
    ObservedAssignmentAttempt  int64              `json:"observedAssignmentAttempt,omitempty"`
    ObservedInstanceGeneration int64              `json:"observedInstanceGeneration,omitempty"`
    ObservedNetworkGeneration  int64              `json:"observedNetworkGeneration,omitempty"`
    LastTransitionTime         *metav1.Time       `json:"lastTransitionTime,omitempty"`
    Message                    string             `json:"message,omitempty"`
}
```

The complete opaque input is never copied to status, diagnostics, events, or
ordinary logs. `DataPlaneReady` is false while any Pool Action for the current
Sandbox instance is not Ready or has not applied the current desired input.

`WaitSandboxReady` may accept an `action_name` target. A client that needs a
synchronous update experience uses:

```text
UpdateSandbox(Action input)
-> WaitSandboxReady(action_name)
```

This preserves the declarative meaning of `UpdateSandbox`.

## Wire protocol

### Common request envelope

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1alpha1",
  "action": "egress",
  "operation": "CREATE",
  "requestId": "pod-a/uid-a/attempt-2/generation-7",
  "sandboxGeneration": 4,
  "inputDigest": "sha256:abc...",
  "attachment": {
    "sandboxUid": "uid-a",
    "sandboxName": "agent-a",
    "namespace": "tenant-a",
    "fastletPodUid": "pod-a",
    "assignmentAttempt": 2,
    "instanceGeneration": 1,
    "networkGeneration": 7,
    "network": {
      "ip": "172.30.0.2",
      "gateway": "172.30.0.1",
      "privateCidr": "172.30.0.0/24",
      "hostVeth": "fh1234567890",
      "hostNetnsPath": "/run/netns/fsb...",
      "dnsPath": "/run/fast-sandbox/network/.../resolv.conf"
    }
  },
  "input": {}
}
```

Fastlet defines and supplies every field except `input`. `inputDigest` is the
SHA-256 digest of a canonical JSON representation, not the original YAML or
JSON with arbitrary whitespace. The digest uses the `sha256:` prefix.

The identity fencing key is:

```text
Sandbox UID
+ Fastlet Pod UID
+ assignment attempt
+ instance generation
+ network generation
```

`sandboxGeneration` orders observations of Sandbox spec. `inputDigest`
identifies the exact Action input. Fastlet may resend unchanged input under a
new Sandbox generation, and the handler must remain idempotent.

### Response

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1alpha1",
  "requestId": "pod-a/uid-a/attempt-2/generation-7",
  "observedInputDigest": "sha256:abc..."
}
```

Fastlet accepts a successful response only when:

- the HTTP status is 2xx;
- the response arrives before the Fastlet-defined deadline;
- the JSON and API version are valid;
- `requestId` matches;
- for an operation carrying input, `observedInputDigest` matches.

An HTTP error response may carry a size-limited, non-sensitive message.
Fastlet maps the error into Action status but does not log or return the opaque
input.

### Idempotency and ordering

The handler provides at-least-once processing semantics:

- a repeated request with the same operation, identity fencing key, and input
  digest must succeed;
- an older assignment, instance, network generation, or Sandbox generation
  must be rejected without modifying current state;
- a different digest under the same identity and generation must be rejected;
- operations for the same Sandbox and Action execute serially;
- operations for different Sandboxes may execute concurrently.

The protocol does not promise exactly-once delivery. `requestId` identifies a
retry; the identity fencing key and digest define state ownership.

## Fixed operation semantics

### CREATE

Fastlet invokes `CREATE` after the Network Slot has been prepared and persisted
as Bound, but before the runtime or user process starts:

```text
Acquire Network Slot
-> invoke CREATE for each Action in Pool declaration order
-> start the runtime only after every CREATE succeeds
```

A successful response means that the handler completed everything required
before untrusted Sandbox code may run. For Egress, this includes creating the
Subject and atomically installing default-deny rules together with the initial
policy.

If an Action fails, Fastlet:

1. stops invoking subsequent Actions;
2. invokes `DELETE` in reverse order for every Action whose `CREATE` succeeded;
3. releases the prepared Network Slot;
4. does not start the runtime;
5. reports the failing Action in status and create diagnostics.

### UPDATE

When the complete desired input digest for the current instance differs from
the last observed digest, Fastlet sends `UPDATE`. The handler validates and
applies the complete replacement.

The handler must not expose a partially applied state. On failure, it must
retain the last successfully applied input or enter an Action-defined
fail-closed state. Fastlet cannot choose between those behaviors because it
does not understand Action input. An Egress implementation must document
whether a failed policy update keeps the old policy or changes the Subject to
deny-all.

After an apply failure, the new desired input remains in the CRD. Reconcile
keeps retrying the same desired digest. A user corrects or rolls back desired
state with another `UpdateSandbox` call.

### DELETE

Fastlet first stops the runtime, invokes `DELETE` in reverse Action declaration
order, and then releases the Network Slot. The request carries the last known
attachment and input.

`DELETE` must be idempotent and is best effort. Missing local state is success.
A handler failure or timeout is recorded but cannot block runtime/network
cleanup or finalizer completion. `SYNC` removes residual state that was not
cleaned up successfully.

### SYNC

`SYNC` carries the complete live attachment set and current desired input for
one Action. Fastlet sends it:

- after Fastlet starts;
- after a handler recovers from unavailability or its instance identity
  changes;
- periodically at a bounded, low frequency.

Conceptual payload:

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1alpha1",
  "action": "egress",
  "operation": "SYNC",
  "requestId": "pod-a/sync-42",
  "items": [
    {
      "sandboxGeneration": 4,
      "inputDigest": "sha256:abc...",
      "attachment": {},
      "input": {}
    }
  ]
}
```

The handler creates missing state, updates stale desired input, fences old
generations, and removes objects absent from the set. When restoring an item,
it should establish a fail-closed baseline before acknowledging success.

At large scale, SYNC may be split into versioned batches with an explicit end
marker. An incomplete batch must never imply that absent objects can be
garbage-collected.

## Handler availability

The protocol defines a fixed status endpoint:

```text
GET http://127.0.0.1:<port>/_fastlet/v1/actions/status
```

It returns supported protocol versions, supported operations, readiness, and
a handler instance ID that changes on every restart. Fastlet:

- does not accept new placements while a handler is incompatible or not
  Ready;
- triggers `SYNC` when the instance ID changes;
- sets process-level deadlines on every call;
- limits concurrency per handler;
- opens a circuit breaker after repeated failures so new creates fail fast;
- never follows HTTP redirects.

Timeouts, retry counts, backoff, circuit-breaker thresholds, and body-size
limits are Fastlet implementation or process configuration, not Pool fields.

## Reconciliation and lifecycle

### Create

```text
Persist the complete Sandbox intent, including actions
-> assign a compatible Fastlet
-> prepare networking
-> CREATE every Action
-> create and start the runtime
-> RuntimeReady
```

If Action input is invalid, the persisted Sandbox intent remains Failed or
Pending, but the runtime does not start. After the user corrects the input with
`UpdateSandbox`, reconciliation retries creation.

### Reset and reassignment

Reset increments the instance generation; reassignment increments the
assignment attempt. Both establish a new Action ownership fencing key:

```text
old instance -> DELETE (best effort)
new instance -> CREATE (latest desired input)
```

Fastlet never sends only `UPDATE` for a new runtime or network identity.

### Handler restart

After a handler restart, its status endpoint exposes a new instance ID. The
handler must establish a global safe baseline before reporting Ready. Fastlet
then sends a full `SYNC`. For an Egress daemon that stores state only in
memory, all live Subjects return to deny-first before the daemon reapplies
persisted, non-sensitive desired input.

### Fastlet restart

Fastlet reconstructs live Sandboxes and Network Slots from existing state,
then sends `SYNC`. A continuously running handler process is not evidence that
its internal state is still correct.

## Update consistency

### Single source of truth

`Sandbox.spec.actions` is the authoritative source for persisted,
non-sensitive Action input. FastPath and direct CRD writers update the same
desired state. FastPath never pushes an unrecorded competing value directly to
a handler.

### Update commit versus apply

An `UpdateSandbox` response means the CRD desired state was committed; it does
not mean every handler applied it. Status exposes desired and applied digests.
`WaitSandboxReady(action_name)` provides synchronous waiting when required.

### Cross-Action atomicity

A single CRD write can atomically change multiple Action inputs, but physical
application by independent sidecars is not atomic. Each Action converges and
reports status independently. Version 1 does not implement cross-handler
prepare/commit/abort.

State that requires atomic application belongs in one Action input managed by
one handler. For example, policy elements that must change together should all
live in `actions.egress`, not in separate Actions.

## Security design

### Trust boundary

An Action handler is an administrator-controlled Fastlet Pod sidecar and may
already hold privileges such as `NET_ADMIN`. A required handler that fails or
hangs can reject new Sandbox creation; that is an intentional failure domain.
Fastlet must bound the impact and remain fail-closed rather than bypassing the
handler.

### Endpoint restrictions

- Fastlet connects only to `127.0.0.1` on the declared port.
- Arbitrary hosts, URLs, paths, scripts, shell fragments, headers, and
  redirects are unsupported.
- The handler listens on Pod loopback; a Sandbox netns cannot access the
  listener.
- The Action protocol is independent from any external application or control
  routing.

Containers in one Pod share a network trust domain. If stronger peer identity
is needed later, transport can move to a Fastlet-owned Unix socket or an
authenticated per-Pod channel without changing operation and envelope
semantics.

### Input confidentiality

Action input is persisted in the Sandbox CRD and may appear in API server
storage, audit, watch, and backup paths. It must not contain:

- passwords or API tokens;
- private keys;
- credential-vault contents;
- any value that must exist only in memory.

Sensitive input requires a separate transient authentication channel or a
reference with appropriate Secret semantics. Sandbox Actions v1 does not solve
that problem.

Fastlet must redact input from ordinary logs, status, diagnostics, metric
labels, and error messages.

### Authorization

Updating a Sandbox Action may be more sensitive than updating ordinary
metadata. Where the deployment identity model permits it, FastPath should
authorize the Action mutation of `UpdateSandbox` as a distinct verb or
capability. Kubernetes RBAC deployments that require write isolation may add
a dedicated `actions` subresource in a later API version.

## Failure semantics

| Scenario | Required behavior |
| --- | --- |
| Handler unavailable before placement | Fastlet must not accept new placement |
| `CREATE` fails or times out | Do not start runtime; roll back successful Actions and networking |
| Runtime creation fails after Actions succeed | Run `DELETE` in reverse order; clean up networking |
| `UPDATE` fails | Keep retrying desired state; report desired/applied mismatch |
| `DELETE` fails | Continue deletion; recover cleanup through `SYNC` |
| Handler restarts | Establish a safe baseline, then run full `SYNC` |
| Fastlet restarts | Reconstruct authoritative state, then run full `SYNC` |
| Request has an old generation | Reject it without modifying current state |
| Request is duplicated | Return the existing result idempotently |

## Alternatives considered

### Pod-placement Infra Components

This is not the primary abstraction. Container delivery already belongs to
`fastletTemplate`, while lifecycle metadata and per-Sandbox desired input
would remain unsolved.

### Direct Pod port routing

This can discover a Pod service but does not define lifecycle ordering, the
creation barrier, desired state, retry, fencing, or recovery. Sandbox Actions
also deliberately keep the internal handler endpoint independent from
external routing.

### Registration from sandbox-init

This is not an identity source. `sandbox-init` runs after the runtime starts
and does not own host veth, Pod netns, assignment, or network generation
metadata. Waiting for registration creates an unregistered post-start window
and moves identity authority into the Sandbox trust domain.

### Arbitrary scripts or webhooks

These are unsupported. Inline scripts provide code execution in Fastlet's
privilege domain. Arbitrary URLs introduce SSRF, metadata leakage, remote
latency, and unbounded external dependencies. The local handler and fixed
envelope are intentionally narrow.

### Asynchronous event bus

An event bus is unsuitable for required Actions because `CREATE` needs a
synchronous acknowledgement capable of blocking runtime startup. Asynchronous
audit or observability events can be added independently, but cannot reuse
these lifecycle guarantees.

### Slot files only

Files are useful for observation but cannot provide a deterministic pre-start
barrier, update acknowledgement, desired-input delivery, and handler-restart
recovery without another protocol.

## Implementation phases

### Phase 1

- Add local HTTP handlers through `SandboxPool.spec.sandboxActions`.
- Add the opaque `Sandbox.spec.actions` input map.
- Implement `CREATE`, `DELETE`, and `SYNC`.
- Implement create rollback, fixed deadlines, status handshake, idempotency,
  and fencing.
- Add an Egress end-to-end test proving that the runtime cannot start before
  policy is active.

### Phase 2

- Add `UPDATE` through the existing `UpdateSandbox` desired-state API.
- Add Action status and `WaitSandboxReady(action_name)`.
- Add handler-restart detection and periodic synchronization.
- Add reset and reassignment recovery tests.

### Phase 3

- Optionally add separate authorization for Action updates.
- Add batched SYNC for large Pools.
- Add other trusted handlers such as audit and QoS agents.
- Harden transport if Pod-local trust is insufficient.

## Required tests

- `CREATE` carries initial opaque input and blocks premature runtime startup.
- Missing or invalid Egress input never allows the runtime to start.
- Duplicate `CREATE` is idempotent.
- Partial failure during multi-Action `CREATE` rolls back in reverse order.
- `UPDATE` is a complete replacement and never exposes partial state.
- A desired/applied digest mismatch remains visible until convergence.
- Old assignment, instance, network, and Sandbox generations are rejected.
- `DELETE` is repeatable and cannot block final cleanup.
- `SYNC` recovers after handler and Fastlet restarts.
- `SYNC` removes stale Subjects and restores missing Subjects.
- Input never appears in status, diagnostics, events, or logs.
- Arbitrary hosts, paths, redirects, scripts, and oversized bodies are
  rejected.
- A Sandbox cannot reach the Pod-loopback Action endpoint.
