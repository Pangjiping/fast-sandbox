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

User metadata is stored as ordinary Kubernetes labels. Labels under
`sandbox.fast.io/` are reserved for the platform.

### Status

| Field | Meaning |
| --- | --- |
| `assignment` | Fastlet name, Pod UID, node, attempt, and admitted Infra revision |
| `assignmentAttempt` | Monotonic assignment fence |
| `instanceGeneration` | Reset/recreate fence |
| `routeGeneration` | Data-plane route fence |
| `runtimeState` | Runtime observation |
| `dataPlaneState` | Complete Infra and route observation |
| `userProcessState` | User process observation |
| `components` | Named component state, protocol, port, and observed route generation |
| `recovery` | Persisted Fastlet-loss detection time and deadline |
| `conditions` | Canonical `RuntimeReady` and `DataPlaneReady` conditions |

Observed subsystem states are `Unknown`, `Pending`, `Creating`, `Ready`,
`Draining`, `Stopped`, `Failed`, and `Unavailable`. Component states are
`Starting`, `Ready`, and `Failed`.

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
| `sandboxResources` | Yes | Immutable per-Sandbox CPU, memory, and PID limits |
| `warmImages` | No | Asynchronous, GC-protected cache inputs |
| `infraComponents` | No | Inline immutable artifact/process/health/endpoint definitions |
| `fastletTemplate` | Yes | Kubernetes Pod template with platform-owned fields protected |

Runtime names are `container`, `gvisor`, `kata-qemu`, `kata-clh`, `kata-fc`,
`kata-dragonball`, and `boxlite`.

Pool status exposes Fastlet capacity, the deterministic Infra revision,
prepared Fastlet counts, safe component summaries, Registry rollout status, and
per-image warm-cache aggregation. Conditions are `RuntimeReady`, `InfraReady`,
and `RegistryReady`.

See the [Infra Components reference](infra-components.md) for the complete
artifact, process, health, endpoint, validation, and revision contract.

## FastPath v2

The protobuf contract is
[`api/proto/v2/fastpath.proto`](../../api/proto/v2/fastpath.proto).

| RPC | Semantics |
| --- | --- |
| `CreateSandbox` | Atomic durable intent followed by Fastlet admission; returns at `RuntimeReady` |
| `GetSandbox`, `ListSandboxes` | Complete metadata, expiry, states, components, and bounded metadata filtering |
| `UpdateSandbox` | Expiry/reset/failure settings plus explicit metadata upsert/delete |
| `DeleteSandbox` | Submit declarative deletion |
| `GetSandboxDiagnostics` | Lifecycle and Fastlet diagnostics, not process stdout |
| `WaitSandboxReady` | Event-driven wait on the assigned Fastlet for one component or all components |
| `ResolveEndpoint` | Resolve a named component or raw user port in central or direct mode |
| `GetPool`, `ListPools` | Runtime, fixed resources, components, capacity, Registry, and warm-image discovery |

### Atomic Create

`CreateRequest` includes:

- `request_id`, namespace, image, Pool, command, args, environment, and working directory;
- absolute `expires_at_unix_seconds`;
- initial `metadata`;
- failure policy and recovery timeout.

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

`SandboxReference` accepts a CRD UID or namespace/name. `EndpointTarget` is
exactly one of:

- `component_name`; or
- raw `port`.

A component route can wait directly on Fastlet readiness. The response includes
the resolved protocol/port, route generation, expiry, proxy URL, and
`X-Fast-Sandbox-Route-Credential`.

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
