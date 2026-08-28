# Firecracker runtime driver E2E environment

Reference environment and expected behavior for running the real Firecracker
runtime driver E2E (`scripts/firecracker-e2e.sh`). Use this document when
reproducing the suite on a fresh host, comparing results, or triaging
failures.

## Host environment

| Item | Value |
|------|-------|
| Machine | Dedicated bare-metal server (cloud-hosted), `/dev/kvm` passed through |
| CPU | 2 x Intel Xeon Platinum 8163 @ 2.50GHz, 96 logical CPUs (24 cores x 2 sockets x 2 threads), 2 NUMA nodes |
| Memory | 504 GiB total, no swap |
| OS | Alibaba Cloud Linux 3 (Soaring Falcon), kernel 5.10.134-18.al8.x86_64 |
| KVM | `/dev/kvm` and `/dev/net/tun` available (VT-x) |
| Hostname | `agent-sandbox033067064046.sg52` (internal ALITest network) |
| Workdir | `/home/gaoran/fast-sandbox` (git clone, branch `feature/firecracker-driver-lifecycle`) |
| Runner | root (login directly as root; the script re-invokes itself with `sudo` only when not root) |

The host also carries other networking (docker0, `tap-external-sk`,
`slot0XXX` ENI-style interfaces) that coexists with the E2E bridge; the suite
only touches resources it creates (`fsb*` netns, `fc*`/`fh*` devices, the
`fsb0` bridge, one MASQUERADE rule) and restores them on exit.

## VM baseline

Restore is the only startup path: the runtime asset is the **golden
snapshot set**, and the kernel/rootfs are **snapshot-prep assets** (the
preparation VM uses them once to produce the snapshot set; restored
Sandboxes never boot a kernel).

| Item | Value |
|------|-------|
| Firecracker | v1.16.1, vanilla upstream release binary |
| Snapshot-prep kernel | `vmlinux.bin`, 4.14.174 (firecracker quickstart image) |
| Snapshot-prep rootfs | `bionic.rootfs.ext4`, Ubuntu 18.04.5 LTS minimized, ~300 MiB |
| Golden snapshot set | `<StateRoot>/images/<sha256(image)>/{rootfs.img, vmstate.snap, memory.snap, manifest.json}` |
| Guest spec | 1 vCPU, 512 MiB (manifest machine tuple, restore machine-config) |

Artifacts are downloaded on first run:

- `https://github.com/firecracker-microvm/firecracker/releases/download/v1.16.1/firecracker-v1.16.1-x86_64.tgz`
- `https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin` (snapshot-prep only)
- `https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/rootfs/bionic.rootfs.ext4` (snapshot-prep input)

The golden snapshot set is produced once per StateRoot (`方式 B` self-
bootstrap: cold boot the prep VM -> Pause -> `PUT /snapshot/create`) and
cached; a rerun with the same `FC_STATE_ROOT` skips the prep boot.

## StateRoot and reflink

The driver StateRoot lives on a reflink-capable filesystem so per-Sandbox
rootfs copies are CoW instead of paying the full dirty writeback:

- Loop-backed sparse xfs image (default 64 GiB) mounted at
  `/var/lib/fast-sandbox` (see `scripts/firecracker-xfs-stateroot.sh`, size
  selectable; `--grow <size>` extends it online)
- E2E runs with `FC_STATE_ROOT=/var/lib/fast-sandbox/e2e`
- Instance rootfs copies use `cp --reflink=always` (plain copy fallback);
  reflink is verified by the xfs script before use

## Network topology

- Shared bridge `fsb0` in the host netns: `172.30.0.1/24`
- Each slot gets its own netns (`fsb<64 hex>`): veth `eth0` owns the slot IP
  (172.30.0.2 onwards), default route via the bridge gateway
- A host-side tap (`fc<13 hex>`) per slot is attached to the bridge; the VM
  runs in the host netns and attaches to its tap
- The guest owns the next address (slot IP + 1), configured statically
- **Restore 后网络恢复**: v1.16 restores the guest network stack from the
  snapshot — the preparation VM's static eth0 config (guest IP 172.30.0.3,
  MAC 02:00:00:00:00:01) is baked into the vmstate, and every restored
  instance resumes with it (the v1.16 clone networking model, see
  firecracker `docs/snapshotting/network-for-clones.md`). The driver only
  replaces the NIC host tap per instance via the load request's
  `network_overrides`. Per-instance distinct guest addresses would require
  per-clone netns isolation + NAT (upstream clone model) and are out of
  scope for the native stage-1 E2E
- Host `PREROUTING` DNAT maps the slot IP to the guest IP; `FORWARD` ACCEPT
  rules allow ingress/egress; the slot netns MASQUERADEs egress
- Sandbox-to-sandbox isolation: the slot netns rejects direct traffic to the
  private CIDR (except the gateway)

## E2E suites

All four cases run under the `firecracker` build tag in one script
invocation (`-run '^TestFirecrackerDriverE2E'`). Every case restores from the
same golden snapshot set (方式 B 自举, produced once per StateRoot):

| Test | What it verifies |
|------|------------------|
| `TestFirecrackerDriverE2E` | Full lifecycle with real Infra Components delivery (loop-mount GuestCopy of sandbox-init + execd), guest files asserted with debugfs, real guest reachability |
| `TestFirecrackerDriverE2ENoInfra` | Same lifecycle without infra delivery |
| `TestFirecrackerDriverE2EConcurrent` | 5 pre-provisioned slots, 5 VMs restored concurrently from the same snapshot set; per-sandbox trace correlation, distinct processes, memory.snap shared (COW). v1.16 clone networking shares the baked guest MAC/IP, so per-instance reachability is not asserted here (single-VM cases cover it) |
| `TestFirecrackerDriverE2EImageGC` | Independent LFU image-cache GC: unreferenced low-frequency image evicted, live Sandbox pins its image, deletion releases it |

## Observed performance

Restore is the only startup path. The snapshot-set preparation is a one-time
cost per StateRoot (cold boot + pause + snapshot dump, reused across runs and
across the four cases); steady-state Sandbox create is a restore:

| Case | Total | Phase breakdown |
|------|-------|-----------------|
| Single VM with infra | restore + infra delivery | prep (one-time) + acquire ~30 µs, rootfs ~1 ms, infra ~200 ms, launch ~100 ms, configure (incl. `snapshot/load`) ~10 ms, boot ~10 ms |
| Single VM without infra | restore | infra 0, rest identical |
| 5 VMs concurrently | 5 x restore from shared set | 5 x infra ~280-400 ms in parallel, launch ~100 ms each |

Notes:

- `launch` (~100 ms) is the firecracker process spawn constant; `boot`
  (~10 ms) is the VM reaching `Running` (1 state poll) after `snapshot/load`
  + `InstanceStart`
- `snapshot/load` itself is part of the `configure` phase (machine-config,
  drive, nic, load)
- Without reflink (ext4 StateRoot) a create costs ~3.0 s (first-fsync dirty
  writeback); with xfs reflink it drops to ~282 ms (rootfs copy ~1 ms)
- The suite has been run back-to-back many times with all green; stale
  netns/taps from interrupted runs are purged by the script at start and on
  exit, and the host neighbour cache is flushed before guest probes so
  repeated runs do not interfere

## Running

```bash
cd /home/gaoran/fast-sandbox
git pull origin feature/firecracker-driver-lifecycle
FC_STATE_ROOT=/var/lib/fast-sandbox/e2e ./scripts/firecracker-e2e.sh
```

Overrides: `FC_VERSION`, `FC_BINARY`, `FC_KERNEL`, `FC_ROOTFS`,
`FC_STATE_ROOT` (defaults to `$WORK/state-root`; keep it persistent to reuse
the prepared golden snapshot set), `WORK`. `--cleanup` removes leftovers of
an interrupted run. Host resources created by a run (bridge, MASQUERADE,
ip_forward, netns, taps) are restored automatically on exit.
