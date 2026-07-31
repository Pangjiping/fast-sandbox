# Runtime model

A SandboxPool selects one immutable runtime name. Fast Sandbox combines the
platform-owned runtime definition with the node runtime environment selected by
the operator. The Pool Controller publishes the result as an immutable runtime
plan consumed by Fastlet.

## Public and internal abstractions

```text
SandboxPool.spec.runtime
  -> RuntimeCatalog
  -> RuntimeDefinition
  +  NodeRuntimeEnvironment
  -> ResolvedRuntimePlan
  -> RuntimeDriver
  -> backend runtime
```

- `RuntimeName` is the public Pool value.
- `RuntimeDefinition` is the code-owned behavior and capability contract.
- `NodeRuntimeEnvironment` records the installed containerd, kubelet, node
  selector, host path, and optional runtime binding.
- `ResolvedRuntimePlan` is the immutable merged contract mounted into one
  Fastlet generation.
- `RuntimeDriver` is Fastlet's runtime-neutral lifecycle interface.

Pool users cannot override containerd handlers, shim paths, runtime
configuration paths, network modes, or platform mounts independently. Runtime
environment configuration is restricted to platform administrators.

## Canonical runtime names

| Runtime name | Driver | Backend |
|---|---|---|
| `container` | containerd | `io.containerd.runc.v2` |
| `gvisor` | containerd | `io.containerd.runsc.v1` |
| `kata-qemu` | containerd | Kata shim with QEMU configuration |
| `kata-clh` | containerd | Kata shim with Cloud Hypervisor configuration |
| `kata-fc` | containerd | Kata shim with Firecracker configuration |
| `boxlite` | BoxLite | Pod-local BoxLite runtime sidecar |

The names define stable profiles, not unconditional production support. See [Runtime support](../reference/runtime-support.md).

## Runtime plan

A resolved runtime plan fixes:

- driver kind and backend configuration;
- privileged mode and host paths;
- KVM and sidecar requirements;
- runtime overhead;
- network mode;
- Infra delivery modes;
- cache, recovery, and network capabilities;
- containerd socket, namespace, snapshotter and root;
- kubelet state root;
- a deterministic revision/profile hash.

The Pool Controller writes the complete plan into an immutable ConfigMap and
mounts it into Fastlet. Fastlet does not independently resolve the built-in
catalog. The revision lets Controllers, placement and Fastlets reject
incompatible generations instead of interpreting the same Pool differently.

See [Runtime environments](../guides/runtime-environments.md) for the operator
configuration and rollout behavior.

## RuntimeDriver

The runtime contract is defined in `internal/runtime/contract`. Its `Driver`
interface contains lifecycle operations:

```text
Initialize
ProbeCapabilities
EnsureSandbox
InspectSandbox
DeleteSandbox
ListManagedSandboxes
Close
```

Optional interfaces add image cache, recovery, resource admission, and access
descriptors. Fastlet assembles network and Infra dependencies around the
selected concrete driver; route publication belongs to the separate data-plane
contract.

Exec, file transfer, PTY, and user protocol operations are deliberately absent. They belong to Infra Components and their upstream SDKs.

## Container and gVisor

The container and gVisor profiles use the containerd driver and a Fastlet-owned Linux network namespace.

- `container` uses the runc v2 handler and host kernel namespace/cgroup isolation.
- `gvisor` uses the runsc handler and a user-space kernel boundary.

Both receive fixed CPU, memory, and PID limits from the Pool's Sandbox resource profile.

## Kata

Kata profiles use the containerd Kata shim. Fastlet still owns the network slot, while the shim exposes the interface to the guest.

QEMU and Cloud Hypervisor are validated profiles. Firecracker remains capability-gated because the validation environment lacks the complete kernel and block-snapshotter contract required by that profile.

Kata supports Infra delivery through OCI bind mounts, image/template baking, preinstalled artifacts, or runtime-specific guest copy.

## BoxLite

BoxLite does not use a containerd runtime handler. Fastlet talks to a `boxlite-runtime` sidecar over a versioned Pod-local Unix socket. The sidecar contains native/CGO integration and owns BoxLite state.

The implementation is grouped under `internal/runtime/boxlite`: `protocol`
owns the versioned DTOs, `driver` is the pure-Go Fastlet adapter, `server`
serves the Pod-local API, and `state` owns durable recovery records.

BoxLite networking produces a local-forward access descriptor rather than a Fastlet-managed netns. The current profile remains fail closed because the upstream API cannot yet prove the required host-enforced per-Box resource contract.

See [BoxLite runtime](boxlite-runtime.md) for the implemented adapter, capability
gaps, and proposed Prepared Runtime architecture.

## Fixed Pool resources

Every Sandbox in one Pool uses the same immutable CPU, memory, and PID limits. Fastlet passes those values to the selected RuntimeDriver and is the enforcement boundary.

The Pool Controller sizes the resource-owning Fastlet or runtime sidecar from:

```text
per-Sandbox resources * maxSandboxesPerPod + runtime overhead
```

A runtime that cannot enforce the requested profile must reject the Pool capability instead of silently weakening isolation.

## Capability probe

Fastlet probes actual node/runtime capability before reporting readiness. A Kubernetes RuntimeClass object alone is not proof that its shim, configuration, host paths, KVM device, network mode, and resource enforcement work.

Capability states distinguish configured, available, ready, degraded, and unsupported profiles. Pool Conditions expose the resulting reason.
