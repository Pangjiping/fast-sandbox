# BoxLite runtime

BoxLite is a strategic RuntimeDriver for Fast Sandbox. The repository contains
an experimental lifecycle adapter, but the built-in `boxlite` profile remains
fail closed. The current code proves a control boundary; it does not yet provide
a supported runtime or a low-latency BoxLite architecture.

This document separates the implemented state from the target design and
defines the evidence required before BoxLite can become a supported runtime.

## Status

| Area | Current state |
|---|---|
| Runtime selection | `SandboxPool.spec.runtime: boxlite` is accepted by the API |
| Deployment model | Pod-local `boxlite-runtime` sidecar is defined |
| Native integration | BoxLite Go SDK and native library v0.9.7 |
| Lifecycle adapter | Ensure, Inspect, List, Delete, and Recover are implemented |
| Identity | Sandbox UID, Fastlet Pod UID, assignment, and immutable spec fences |
| Infra delivery | Artifact volume, preinstalled artifact, and template modes |
| Network | Compatibility `LocalForward` path through `sandbox-tunnel` |
| Cache | Image list and pull interfaces |
| Cleanup | Sidecar deletion plus owner-fenced NodeJanitor state cleanup |
| Positive Kubernetes E2E | Not implemented |
| Production capability | Unsupported: `BoxLiteResourceEnforcementIncomplete` |

The existing BoxLite E2E verifies that the runtime fails closed. It does not
create a Box or prove Create, Execd, networking, deletion, or recovery.

## Current architecture

```text
SandboxPool(runtime=boxlite)
  -> Pool Controller
  -> Fastlet Pod
       |- fastlet
       |- fastlet-proxy
       `- boxlite-runtime
            -> BoxLite Go SDK + CGO
            -> libboxlite
            -> one Box per Sandbox UID
```

Fastlet stays pure Go. Native libraries, CGO, KVM access, gvproxy, and BoxLite
state are isolated in the `boxlite-runtime` container. Fastlet calls a versioned
HTTP/JSON API over the Pod-local Unix socket:

```text
/run/fast-sandbox/boxlite/runtime.sock
```

The sidecar is the Kubernetes resource-owning container. Its CPU and memory
request is:

```text
per-Sandbox resource profile * maxSandboxesPerPod + runtime overhead
```

This preserves Kubernetes scheduling and aggregate Pod accounting while the
sidecar creates multiple Boxes.

BoxLite is not a containerd runtime handler. It is an independent implementation
of the runtime-neutral Fastlet `RuntimeDriver` contract.

## Current Create path

The current path is a cold lifecycle adapter:

```text
Fastlet admission
  -> prepare Infra artifacts
  -> PUT /v1/boxes/{sandboxUID}
  -> persist Fast Sandbox BoxLite record
  -> BoxLite GetOrCreate
  -> Box Start
  -> execute sandbox-tunnel in the Box
  -> wait for tunnel readiness
  -> return RuntimeReady and LocalForward access
  -> initialize required Infra Components asynchronously
  -> publish Fastlet Proxy route
  -> DataPlaneReady
```

One Box is named by Sandbox UID. The immutable request hash prevents the same
UID from being reused with a different runtime, resource, Infra, image, command,
or assignment identity.

Infra artifacts are copied into a per-Sandbox bundle and mounted read-only at
`/.fast`. When `sandbox-init` is needed, it replaces the image entrypoint and
starts the user process plus injected Infra Components. The current adapter
requires an explicit user command in this mode because it does not introspect
the image's default entrypoint and command.

## Current network path

BoxLite owns the guest network, so the current adapter cannot use Fastlet's
Linux network namespace driver. It creates a compatibility path:

```text
Sandbox Proxy
  -> Fastlet Proxy
  -> Pod-local TCP port
  -> BoxLite/gvproxy port mapping
  -> sandbox-tunnel inside the Box
  -> requested private target port
```

Each Box receives a random credential. Fastlet Proxy writes the credential and
target port in a fixed-size preamble before forwarding application bytes.
Application protocols remain transparent.

The compatibility path preserves the external "any private target port"
contract, including the same target port in different Boxes, but it adds:

- one Pod-local port allocation per Box;
- a guest helper process;
- a credential and listener recovery contract;
- an extra readiness wait in the synchronous runtime path;
- another capacity, conntrack, and failure dimension.

## Recovery and ownership

The sidecar stores state below:

```text
/var/lib/fast-sandbox/boxlite/{fastlet-pod-uid}/
```

The Fastlet Pod UID is an ownership fence. A sidecar container restart in the
same Pod can reload records, reattach Boxes, and recreate LocalForward tunnels.
A replacement Fastlet Pod has a different UID and does not adopt the old
Sandboxes. This matches the Fast Sandbox Pod-bound lifecycle: when a Fastlet Pod
disappears, its Sandboxes become invalid and are cleaned rather than moved.

NodeJanitor checks both Kubernetes ownership and the BoxLite state lock before
removing records. The current cleanup backend removes Fast Sandbox metadata and
artifact bundles, but it does not prove that every detached Box, shim, VMM,
cgroup, disk, and network resource was removed through BoxLite. Production
cleanup requires a runtime-aware, idempotent `ForceRemove`.

## Why the profile is fail closed

Three independent gates intentionally prevent positive use:

1. RuntimeCatalog marks the profile `Unsupported`, so the Pool Controller does
   not create a Fastlet Pod.
2. The Fastlet runtime factory rejects an unsupported profile before creating
   the BoxLite driver.
3. The native sidecar advertises `resource-limits-v1: false` and
   `Ready: false`.

Removing only one gate cannot enable the runtime.

### Resource contract gap

Every Sandbox in a Pool has one immutable CPU, memory, and PID profile. Fastlet
is responsible for rejecting a runtime that cannot enforce it.

The v0.9.7 adapter currently:

- rounds fractional Kubernetes CPU upward to an integer vCPU count;
- converts memory to MiB;
- does not pass the Sandbox PID limit;
- cannot inspect effective per-Box limits;
- cannot prove that limits were applied before the user process started;
- cannot prove strict failure when host enforcement is unavailable.

BoxLite upstream has since added cgroup v2 resource controls. As of
[upstream commit 71ea6b3](https://github.com/boxlite-ai/boxlite/commit/71ea6b327f501f2eb65912ada48aab0503ccdaaa),
the implementation contains `memory.max`, `cpu.max`, and `pids.max`, but
[cgroup setup can still warn and continue](https://github.com/boxlite-ai/boxlite/blob/71ea6b327f501f2eb65912ada48aab0503ccdaaa/src/boxlite/src/jailer/sandbox/bwrap.rs)
when setup fails. In addition, a host-side Box cgroup PID limit covers the
shim/VMM process tree; it is not automatically the same contract as limiting
user processes inside the guest.

Fast Sandbox requires:

- fractional CPU quota, not only an integer vCPU topology;
- a guest workload PID limit that workload root cannot weaken;
- host-enforced memory and aggregate Pod accounting;
- strict create-time failure if any required control is unavailable;
- effective values and enforcement state through Inspect or Stats;
- recovery and negative tests that retain the same limits.

The profile must remain fail closed until this contract is demonstrated.

### Concurrency gap

The native adapter holds one global mutex while performing `GetOrCreate`,
`Start`, tunnel execution, and tunnel readiness. Concurrent Sandbox Creates in
one Fastlet Pod are therefore serialized.

The runtime needs:

- a short registry lock for shared maps;
- a per-Box lifecycle lock;
- bounded concurrency for image, VM, and tunnel operations;
- idempotency independent of global serialization;
- load tests at the configured Fastlet capacity.

### Version and supply-chain gap

The image build pins the Go SDK and native library to v0.9.7 with checksums.
That is reproducible, but the integration now trails upstream resource,
networking, snapshot, clone, and lifecycle work.

Upgrading must select a released, signed, mutually compatible set of:

- Go SDK;
- native library;
- shim and jailer;
- guest kernel and guest agent;
- gvproxy or replacement network backend.

Tracking upstream `main` directly is not an acceptable production supply chain.

## Upstream capabilities to validate

BoxLite's current architecture is an embedded stateful runtime with one
micro-VM and shim per Box. See the
[BoxLite architecture](https://docs.boxlite.ai/architecture).

Recent upstream code provides capabilities that can simplify this integration,
but they must be validated against Fast Sandbox semantics and selected from a
released version.

### Native tunnel

The upstream Go SDK on the reviewed commit has a Box-scoped
[`Network().Tunnel(port)` API](https://github.com/boxlite-ai/boxlite/blob/main/sdks/go/tunnel.go)
that returns a URI or file descriptor and can produce a raw `net.Conn`.

If the selected release supports this contract reliably, Fast Sandbox can
remove the Pod-local port lease and `sandbox-tunnel`. Fastlet Proxy should
request an identity-bound stream from the Runtime Engine for each connection:

```text
Fastlet Proxy
  -> Runtime Engine stream socket
  -> BoxLite native tunnel
  -> requested guest port
```

The Runtime Engine can return an FD over a Unix socket or splice the two streams.
It must define cancellation, half-close, backpressure, long-lived connections,
Box identity fencing, and sidecar restart behavior.

### Snapshot, clone, pause, and resume

BoxLite advertises snapshots and clones, but the current public API describes
them as copies of a stopped Box's disk state. Disk snapshots can accelerate
rootfs preparation and preserve state; they do not by themselves restore a
running guest in tens of milliseconds.

The upstream state machine also uses `SIGSTOP` and `SIGCONT` to quiesce a live
VM. Fast Sandbox must not signal BoxLite internals directly. A low-latency
integration needs a supported, versioned Pause/Resume API with ownership,
metrics, crash recovery, and resource behavior.

Local AutoPause is not yet a general local-runtime primitive. The
[upstream local runtime](https://github.com/boxlite-ai/boxlite/blob/main/src/boxlite/src/runtime/rt_impl.rs)
currently treats AutoPause as a REST-runtime feature. Fast Sandbox should
therefore measure available APIs instead of inheriting an unqualified startup
claim.

## Target architecture

The recommended architecture preserves the Pod-local native boundary and
evolves `boxlite-runtime` into a BoxLite Runtime Engine:

```text
Fastlet Pod
  |- fastlet
  |    |- admission and capacity
  |    |- Sandbox identity
  |    `- lifecycle orchestration
  |
  |- fastlet-proxy
  |    `- transparent data forwarding
  |
  `- BoxLite Runtime Engine
       |- versioned control API
       |- per-Box lifecycle manager
       |- strict resource verifier
       |- image and template cache
       |- Prepared Runtime Pool
       |- native stream endpoint
       |- state and recovery
       `- metrics and garbage collection
```

The Unix-socket boundary is not a material startup bottleneck. It keeps CGO and
native runtime failures out of Fastlet, permits independent BoxLite upgrades,
and makes the sidecar the Kubernetes resource owner.

Embedding libboxlite directly into Fastlet is not recommended. A node-wide
shared daemon is also not the first target because VM processes would no longer
naturally belong to the resource-reserving Fastlet Pod. It would add node-wide
failure, authorization, ownership, and cgroup-accounting problems.

A shared immutable node image/template cache can be considered later if
upstream supports concurrent, owner-fenced access. Runtime lifecycle ownership
should remain Pod-local until measurements prove that boundary is inadequate.

## Runtime protocol v2

The current monolithic `Ensure` call is sufficient for cold lifecycle
compatibility but cannot express preparation and claim. A future protocol should
separate:

```text
ProbeCapabilities
PrepareImage
PrepareTemplate
ClaimPreparedRuntime
StartWorkload
Dial
Inspect
Stats
Release
ForceRemove
List
Recover
```

Control calls use a versioned Unix-socket API. `Dial` uses an FD-capable or raw
stream Unix-socket protocol so the runtime does not translate application
protocols.

Every mutating call must carry:

- namespace and Sandbox UID;
- Fastlet Pod UID;
- instance generation;
- runtime instance ID;
- assignment attempt;
- route generation where applicable;
- immutable runtime, resource, image, and Infra identities.

## Prepared Runtime Pool

The performance direction is to move expensive work before the user request:

```text
Background:
  prepare image/template
    -> create and boot Box
    -> wait for guest agent
    -> pause clean instance
    -> add prepared slot

Create request:
  atomically claim slot
    -> bind Sandbox identity
    -> resume
    -> deliver command, environment, and Infra configuration
    -> start user workload
    -> RuntimeReady

Asynchronous:
  initialize required Infra services
    -> publish route
    -> DataPlaneReady
    -> replenish prepared slot
```

Prepared instances are keyed by at least:

- runtime profile hash;
- resource profile hash;
- resolved cached image identity;
- Infra profile hash;
- template format and guest artifact versions.

Fast Sandbox does not add a registry digest lookup to the Create request.
Warm-image preparation resolves and records immutable cache identity in the
background. Images without a matching prepared slot use a measured cold
fallback.

A mutable instance must never be reset and assigned to another tenant unless
the runtime can prove complete isolation. The safe initial model is an immutable
template plus copy-on-write clone, followed by deletion of the claimed Box.

The main upstream design requirement is workload late binding. A prepared guest
must reach a platform agent before the user image entrypoint is launched, then
accept the claimed command, environment, mounts, and Infra configuration. A
snapshot that already ran a previous tenant's process is not reusable.

## Performance contract

BoxLite measurements must report separate milestones:

| Milestone | Meaning |
|---|---|
| Image ready | OCI data is cached and usable |
| Box configured | Persistent Box metadata exists; no claim of VM readiness |
| Guest ready | Guest agent accepts control operations |
| RuntimeReady | Claimed user process was started |
| Infra ready | Required injected services passed readiness |
| DataPlaneReady | Proxy route is published and usable |

Report cold Create, cached cold Create, template clone, prepared claim, and
resume as different workloads. Do not compare BoxLite's API return, guest boot,
or resume number directly with Fast Sandbox's end-to-end `RuntimeReady`.

Record p50, p95, p99, errors, concurrency, cache state, image, resource profile,
hardware, KVM nesting, and all component versions. Use a production-like
bare-metal or first-level KVM host in addition to nested-KVM environments,
because nested virtualization can dominate VM startup.

## Delivery roadmap

### Phase 0: standalone capability evidence

Validate a selected current BoxLite release directly in a Linux KVM environment
before changing Kubernetes capability gates:

- cached cold Create through first exec;
- native tunnel streaming and arbitrary target ports;
- fractional CPU, memory, and guest PID enforcement;
- strict failure when cgroup setup is unavailable;
- image cache and template behavior;
- stop, restart, detached recovery, and ForceRemove;
- concurrent Boxes at one Fastlet's target capacity;
- runtime and guest version observability.

### Phase 1: positive lifecycle integration

- upgrade the pinned BoxLite SDK and native artifact;
- replace the global lifecycle lock with per-Box locking;
- complete runtime-aware orphan cleanup;
- add an explicit experimental capability gate;
- run positive Create, Inspect, Delete, idempotency, and recovery E2E;
- retain production fail closed until the resource contract passes.

### Phase 2: native data path and Infra integration

- replace the compatibility port mapping and `sandbox-tunnel` with native Dial;
- validate long-lived HTTP, SSE, WebSocket, and file streams;
- inject `sandbox-init` and OpenSandbox Execd;
- prove Execd initialization, readiness, routing, and deletion;
- validate source addresses, NAT, DNS, NetworkPolicy expectations, and conntrack.

### Phase 3: prepared runtime

- agree on supported Pause/Resume and late-binding APIs with BoxLite upstream;
- implement template preparation and atomic claim;
- add cold fallback and asynchronous replenishment;
- expose inventory and cache affinity in Fastlet heartbeat;
- benchmark Create through RuntimeReady under concurrency and failure.

## Support completion criteria

The built-in profile can become supported only when:

1. CPU, memory, and guest PID limits are strict, inspectable, and bypass-tested.
2. The selected artifacts form a pinned and verifiable supply chain.
3. Positive Create, Execd, networking, deletion, and recovery E2E pass on Linux
   KVM.
4. Fastlet, sidecar, Pod, node, and partial-create failures have idempotent
   cleanup.
5. Native stream forwarding has identity fencing and streaming stress evidence.
6. Concurrent Create does not serialize globally or exceed Fastlet capacity.
7. Image and template inventory integrates with heartbeat and scheduling hints.
8. Every advertised capability has both positive and negative tests.

Until those conditions are met:

```bash
make e2e SUITE=runtime RUNTIME=boxlite
```

proves the fail-closed boundary, not BoxLite availability.
