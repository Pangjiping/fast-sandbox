# Runtime support

This matrix describes validation in Fast Sandbox, not the general capability
of each upstream runtime.

| Runtime | Driver | Network access | Inline Infra Components | Quick Start | Status |
| --- | --- | --- | --- | ---: | --- |
| `container` | containerd/runc | private Linux netns | read-only artifact mapping | Yes | Validated |
| `gvisor` | containerd/runsc | private Linux netns | read-only artifact mapping | Yes | Validated |
| `kata-qemu` | containerd/Kata | slot netns to guest NIC | guest-visible artifact mapping | Yes | Validated |
| `kata-clh` | containerd/Kata | slot netns to guest NIC | guest-visible artifact mapping | Yes | Validated |
| `kata-fc` | containerd/Kata | slot netns to guest NIC | guest-visible artifact mapping | Yes | Validated |
| `kata-dragonball` | containerd/Kata Rust | slot netns to guest NIC | guest-visible artifact mapping | Yes | Validated |
| `boxlite` | BoxLite sidecar | authenticated LocalForward | artifact volume | No | Unsupported; fail closed |

## Validation meaning

A validated runtime has remote Linux/Kubernetes evidence for:

- runtime capability detection;
- fixed CPU, memory, and PID enforcement;
- create, inspect, recovery, and idempotent delete;
- private networking and proxy routing;
- inline artifact injection and named component readiness;
- assignment/generation fencing and orphan cleanup.

Validation does not imply snapshot, pause/resume, persistent storage, or live
migration.

## Runtime notes

`container` uses `io.containerd.runc.v2`. `gvisor` uses
`io.containerd.runsc.v1`. Kata QEMU and Cloud Hypervisor use
`containerd-shim-kata-v2` with distinct platform-owned configuration.

Kata Firecracker uses a runtime-specific binding in the node environment. The
binding selects the `blockfile` snapshotter and a Firecracker configuration
backed by a guest kernel with `CONFIG_VIRTIO_MMIO=y`. The development setup validates the
runtime, private networking, inline Execd delivery, named routing, Fastlet
recovery, and cleanup under nested KVM.

Kata Dragonball uses the Kata Rust shim with an independent Fast Sandbox
compatibility configuration. The validation covers runtime creation, resource
limits, private networking, proxy access, Fastlet restart recovery, and
idempotent cleanup under nested KVM.

BoxLite is integrated through a versioned Pod-local UDS sidecar and
runtime-owned LocalForward tunnel. The profile remains unavailable because
host-enforced resource semantics are incomplete. BoxLite 0.9.7 accepts Registry
credentials only when its runtime is initialized, so projected credential
rotation is not hot-applied to an existing BoxLite runtime. Containerd workload
pulls and OCI Infra artifact pulls do hot-reload the namespace Registry
projection.

See [Secure runtimes](../guides/secure-runtimes.md) and
[BoxLite runtime](../concepts/boxlite-runtime.md).
