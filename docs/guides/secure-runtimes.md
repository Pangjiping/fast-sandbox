# Secure runtimes

Fast Sandbox supports runtime profiles with different isolation boundaries. The profile selects a complete platform contract; users do not configure individual RuntimeClass names or containerd handlers.

## Runtime profiles

| Runtime | Isolation | Node requirement | Fast Sandbox status |
|---|---|---|---|
| `container` | Host kernel namespaces and cgroups | containerd | Validated |
| `gvisor` | runsc user-space kernel | runsc shim and configuration | Validated |
| `kata-qemu` | QEMU virtual machine | KVM and Kata installation | Validated |
| `kata-clh` | Cloud Hypervisor virtual machine | KVM and Kata installation | Validated |
| `kata-fc` | Firecracker virtual machine | KVM, compatible kernel, block snapshotter | Validated; environment-sensitive latency |
| `kata-dragonball` | Dragonball microVM | KVM and Kata Rust runtime installation | Validated; compatibility binding required |

See [Runtime support](../reference/runtime-support.md) for the canonical capability matrix.

## gVisor prerequisites

Eligible nodes must provide:

- `/usr/local/bin/runsc`;
- `/usr/local/bin/containerd-shim-runsc-v1`;
- `/etc/containerd/runsc.toml`;
- a working `io.containerd.runsc.v1` handler;
- the Fastlet host paths and Linux network prerequisites.

The Pool Controller injects these platform-owned paths from the RuntimeProfile. A Pool cannot override them.

Prepare the development environment:

```bash
make env PROFILE=gvisor
```

Run the interactive walkthrough:

```bash
make quickstart RUNTIME=gvisor
```

## Kata prerequisites

Eligible nodes must provide:

- `/dev/kvm`;
- a compatible Kata installation under `/opt/kata`;
- `containerd-shim-kata-v2`;
- runtime-specific Kata configuration;
- nested virtualization when running inside a development VM;
- Fastlet network host paths.

Prepare and run:

```bash
make env PROFILE=kata-qemu
make quickstart RUNTIME=kata-qemu

make env PROFILE=kata-clh
make quickstart RUNTIME=kata-clh

make env PROFILE=kata-fc
make quickstart RUNTIME=kata-fc

make env PROFILE=kata-dragonball
make quickstart RUNTIME=kata-dragonball
```

Kata uses a Fastlet-owned network slot. The Kata shim carries its interface and supported OCI mounts into the guest.

## Firecracker storage contract

Firecracker cannot use the default Kata kernel and overlayfs combination used
by the other development profiles. The `kata-fc` binding in the node runtime
environment therefore overrides the profile with:

- a Kata guest kernel built with `CONFIG_VIRTIO_MMIO=y`;
- `block_device_driver = "virtio-mmio"` in the Firecracker configuration;
- containerd's `blockfile` snapshotter, backed by an ext4 scratch image;
- a longer Fastlet Create transport deadline while heartbeat and observation
  calls retain their short deadline.

Runtime environment resolution carries the snapshotter into the Fastlet's
containerd driver. Image unpack, workload snapshots, and cleanup all use that
resolved snapshotter. The development manager creates the scratch image and
loop devices, configures the handler, restarts containerd when required, and
fails setup unless the blockfile plugin reports `ok`.

The positive E2E creates a real Firecracker Sandbox through FastPath, verifies
the isolated guest kernel and private network, reaches the workload through
the proxy, restarts the Fastlet, and executes a command through the injected
OpenSandbox Execd after recovery.

Delete is not considered successful until containerd resources and the exact
Firecracker process for that Sandbox are absent. NodeJanitor exposes a
node-local cleanup service for this residual-process check, so Fastlet does not
need host PID visibility. The network slot's durable owner record also carries
the runtime process kind. If a Fastlet Pod disappears after containerd has
already removed the task and container, NodeJanitor uses that record to remove
only the VMM belonging to the orphaned Sandbox before it releases the slot.
The E2E covers both ordinary deletion and Fastlet Pod loss, and repeats
create/delete on one slot to catch accumulating VMM processes.

### Firecracker performance characteristics

`kata-fc` is functionally validated, including private networking, Infra
delivery, Fastlet recovery, repeated deletion, and orphaned-VMM cleanup. It is
not assigned a universal startup number: the current blockfile and cold-VM
path is especially sensitive to storage and nested virtualization.

With a warm image and concurrency 1, a 20-sample engineering run measured a
mean of 560.6 ms to `RuntimeReady` on a non-nested KVM host, but 5,474.6 ms in
a resource-constrained nested-KVM VM with a nearly full root disk. The latter
spent about 4.34 s in `NewContainer` and blockfile preparation, compared with
about 21.7 ms on the non-nested host. Neither result includes asynchronous
`DataPlaneReady` or Execd health.

These values are regression evidence, not a production performance promise.
No Firecracker VM template, snapshot restore, or prepared-VM mechanism is
enabled. See [Performance](performance.md#kata-331-secure-runtime-comparison)
for the complete contract and all runtime results.

## Dragonball compatibility contract

Kata 3.31.0 includes the Dragonball VMM and its Rust shim. Its upstream
experimental guest kernel did not reach kata-agent in the validated nested-KVM
environment, while dynamically adding vCPUs with the unified kernel raced the
Dragonball upcall server. Fast Sandbox therefore uses a separate compatibility
configuration with the unified Kata kernel, static Sandbox resource management,
and two boot vCPUs.

The upstream configuration is never edited. Development setup creates
`configuration-dragonball-fast-sandbox.toml`, and production installation must
create the same independent file before the runtime environment is rolled out.
See [Runtime node installation](runtime-node-installation.md).

## Capability validation

A RuntimeClass object is not sufficient evidence. Fastlet probes actual backend capability, and Pool readiness must fail closed if the handler, binary, configuration, KVM device, network mode, or resource contract is unavailable.

Run:

```bash
make e2e SUITE=runtime RUNTIME=gvisor
make e2e SUITE=runtime RUNTIME=kata
```

A skipped runtime test is not a passing capability gate.
