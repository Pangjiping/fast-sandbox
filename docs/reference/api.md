# API reference

Fast Sandbox exposes Kubernetes CRDs for declarative lifecycle and a gRPC
FastPath API for latency-sensitive clients.

## Version boundary

```text
CRD group: sandbox.fast.io
CRD version: v1alpha2
FastPath package: fastpath.v2
```

`v1alpha2` is the only canonical runtime representation. It removes the old
`infraProfile` reference in favor of inline Pool components.

## Sandbox

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: example
  namespace: fast-sandbox
  labels:
    owner: team-a
spec:
  image: docker.io/library/alpine:latest
  command: ["/bin/sleep"]
  args: ["3600"]
  poolRef: default-pool
  expireTime: "2026-07-30T00:00:00Z"
  failurePolicy: Manual
  recoveryTimeoutSeconds: 60
  actionBindings:
    - handler: egress
      input: '{"defaultAction":"deny"}'
```

### Spec

| Field | Required | Meaning |
| --- | ---: | --- |
| `image` | Yes | Workload OCI image |
| `command`, `args` | No | User process override |
| `envs` | No | Kubernetes `EnvVar` array |
| `workingDir` | No | User process working directory |
| `expireTime` | No | Absolute declarative expiry |
| `failurePolicy` | No | `Manual` or `AutoRecreate`; default `Manual` |
| `recoveryTimeoutSeconds` | No | Durable delay before recovery action; default 60 |
| `resetRevision` | No | Opaque monotonic reset trigger |
| `poolRef` | Yes | Same-namespace SandboxPool |
| `actionBindings` | No | Ordered atomic Handler/input list; order defines lifecycle invocation order |

User metadata is stored as ordinary Kubernetes labels. Labels under
`sandbox.fast.io/` are reserved for the platform.

### Status

| Field | Meaning |
| --- | --- |
| `observedGeneration` | Sandbox Spec generation represented by this Controller snapshot |
| `placement` | Assignment attempt, Fastlet name/Pod UID, and nested recovery deadline |
| `runtime` | Runtime state, concrete runtime generation, transition time/message, and accepted reset revision |
| `dataPlane` | Route state, route-generation fence, transition time, and message |
| `infraComponents` | Per-component name, `Starting`/`Ready`/`Failed`, transition time, and message |
| `actionBindings` | Per-Binding Handler, `Pending`/`Applying`/`Ready`/`Failed`, transition time, and message |
| `conditions` | One aggregate standard `Ready` Condition |

Runtime and DataPlane use separate state enums. Input digests, invocation IDs,
runtime IDs, and per-Handler fences remain internal and never appear in CRD
Status.

## SandboxPool

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: default-pool
  namespace: fast-sandbox
spec:
  capacity:
    poolMin: 1
    poolMax: 3
    bufferMin: 1
    bufferMax: 2
  maxSandboxesPerPod: 8
  runtime: container
  sandboxResources:
    cpu: "1"
    memory: 512Mi
    pids: 256
  warmImages:
    - docker.io/library/alpine:latest
  actionHandlers:
    - name: egress
      targetHTTPPort: 18080
  infraComponents:
    - name: execd
      artifact:
        source:
          image:
            reference: ghcr.io/opensandbox/execd@sha256:<digest>
        mappings:
          - sourcePath: /execd
            targetPath: /.fast/components/execd/execd
      process:
        command: ["/.fast/components/execd/execd", "--port", "44772"]
        restartPolicy: OnFailure
        healthCheck:
          httpGet:
            path: /ping
          timeoutSeconds: 10
      endpoint:
        protocol: HTTP
        port: 44772
  fastletTemplate:
    spec:
      containers:
        - name: fastlet
          image: fast-sandbox/fastlet:dev
```

### Spec

| Field | Required | Meaning |
| --- | ---: | --- |
| `capacity` | Yes | Pool size and idle-buffer bounds |
| `maxSandboxesPerPod` | Yes | Fastlet-authoritative admission limit |
| `runtime` | Yes | Immutable runtime name |
| `sandboxResources` | Yes | Immutable per-Sandbox CPU, memory, and PID limits; their sum may exceed an explicitly lower Fastlet aggregate limit |
| `warmImages` | No | Asynchronous, GC-protected cache inputs |
| `infraComponents` | No | Inline immutable artifact/process/health/endpoint definitions |
| `actionHandlers` | No | Named Pod-loopback Binding receivers and their lifecycle Hook subscriptions |
| `fastletTemplate` | Yes | Kubernetes Pod template with platform-owned fields protected; runtime-owner limits define the optional aggregate overcommit budget |

Runtime names are `container`, `gvisor`, `kata-qemu`, `kata-clh`, `kata-fc`,
`kata-dragonball`, and `boxlite`.

Pool status exposes Fastlet capacity, runtime/Infra/Fastlet revisions, prepared
Fastlet counts, and per-image warm-cache aggregation. Handler and Registry
rollout details are not duplicated as nested status. Conditions are
`RuntimeReady`, `InfraReady`, and `RegistryReady`.

See the [Infra Components reference](infra-components.md) for the complete
artifact, process, health, endpoint, validation, and revision contract.
See [Sandbox Actions](../concepts/sandbox-actions.md) for Binding, lifecycle
Hook, ordering, readiness, and recovery semantics.

## FastPath v2

The protobuf contract is
[`api/proto/v2/fastpath.proto`](../../api/proto/v2/fastpath.proto).

| RPC | Semantics |
| --- | --- |
| `CreateSandbox` | Atomic durable intent followed by Fastlet admission; omitted completion waits for aggregate `READY`, explicit `RUNTIME_READY` returns early |
| `GetSandbox` | Fenced point-in-time live view from the assigned Fastlet |
| `ListSandboxes` | Lightweight CRD-backed identity/generation summaries with metadata filtering |
| `UpdateSandbox` | Typed desired-state mutation, complete ordered Binding replacement, and metadata upsert/delete |
| `DeleteSandbox` | Submit declarative deletion |
| `GetSandboxDiagnostics` | Lifecycle and Fastlet diagnostics, not process stdout |
| `ResolveEndpoint` | Non-blocking resolution of a named component or raw user port in central/direct mode; requires live aggregate Ready |
| `GetPool`, `ListPools` | Runtime, fixed resources, components, capacity, and warm-image discovery |

### Atomic Create

`CreateRequest` includes:

- `request_id`, namespace, image, Pool, command, args, environment, and working directory;
- absolute `expires_at_unix_seconds`;
- initial `metadata`;
- failure policy and recovery timeout.
- ordered `action_bindings` and a `CreateCompletion` boundary.

The first CRD write contains the complete initial intent. A retry with the same
normalized intent is idempotent; a changed intent under the same request ID is
a conflict. Fast Sandbox does not extend an absolute expiry during retry.

### Metadata update and list

`UpdateRequest` carries `metadata_upsert` and `metadata_delete_keys`. A key is
preserved when absent, replaced when upserted, and removed only when explicitly
listed for deletion.

`ListRequest.metadata` has AND semantics. Filtering happens before bounded
pagination.

### Endpoint targets

`SandboxReference` uses namespace/name for lookup and accepts an optional
`expected_uid` identity fence. `EndpointTarget` is
exactly one of:

- `component_name`; or
- raw `port`.

Resolution never waits. A non-Ready live Sandbox returns
`FailedPrecondition`; stale/missing assignment state returns `Unavailable`.
The response contains one nested `endpoint` with the final component identity
(when applicable), protocol, and port, plus route generation, expiry, proxy URL,
and `X-Fast-Sandbox-Route-Credential`.

Central mode returns:

```text
Sandbox Proxy -> Fastlet Proxy -> Sandbox
```

Direct mode is for a trusted platform ingress:

```text
trusted ingress -> Fastlet Proxy -> Sandbox
```

Application `Authorization` is not consumed by Fast Sandbox route
authentication.

See [OpenSandbox integration](../guides/opensandbox-integration.md) for the
trusted direct-ingress contract.

## Error semantics

- local validation and no-capacity errors occur before CRD creation;
- only an explicit side-effect-free Fastlet rejection permits trying another
  Top-K candidate;
- ambiguous transport failure preserves and retries the durable assignment;
- failures after CRD persistence leave durable intent for Reconciler takeover;
- Delete is accepted before asynchronous finalizer cleanup completes;
- stale Pod UID, instance generation, assignment attempt, route generation, or
  route credential is rejected.
