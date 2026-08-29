# Firecracker full-chain E2E (builder → S3 → runtime-agent → driver restore)

Reference environment and expected behavior for
`scripts/firecracker-chain-e2e.sh` — the stage-1 closeout of the
[Firecracker on-demand loading](../design/firecracker-on-demand-loading.md)
design. Every component runs for real: the builder image publishes through
MinIO, the runtime-agent pulls over the wire (path-style SigV4 + credential
mapping), and the Firecracker driver restores from the pulled artifacts and
reaches the guest.

```text
alpine:3.19 ──builder──▶ s3://sandbox-images/publish (index + digest16 build + SHA256SUMS)
                              │  PinImage (UDS, SigV4 over MinIO)
                              ▼
                       runtime-agent ──pull──▶ <state-root>/images/<sha256(image)>/
                              │  restore (LoadSnapshot, network_overrides)
                              ▼
                       firecracker driver (Go E2E) ──▶ guest reachable at 172.30.0.3
```

## Host environment

Same bare-metal host as
[firecracker-runtime-e2e.md](firecracker-runtime-e2e.md) and
[sandboxtemplate-golden-image-e2e.md](sandboxtemplate-golden-image-e2e.md):

| Item | Value |
|------|-------|
| Machine | Dedicated bare-metal server (cloud-hosted), `/dev/kvm` and `/dev/net/tun` passed through |
| OS | Linux x86_64 (Alibaba Cloud Linux 3 reference host) |
| Runner | root (the script re-invokes itself with sudo) |
| Required tools | `ip`, `iptables`, `sysctl`, `ping`, `curl`, `jq`, `docker`, `go` |
| StateRoot | **xfs (reflink-capable), never ext4** — see [StateRoot MUST be on a reflink-capable filesystem](#stateroot-must-be-on-a-reflink-capable-filesystem) below; provision with `scripts/firecracker-xfs-stateroot.sh --loop` |

## What it verifies

| # | Verification point | Assertion |
|---|--------------------|-----------|
| 1 | builder real publish layout | `index/<sha256(image)>.json` + `digest16/{rootfs.ext4,vmstate.snap,memory.snap,SHA256SUMS,manifest.json}`; `index.artifactDigest` == sha256(manifest); every `files[]` digest matches the uploaded object |
| 2 | SigV4 against MinIO | agent pull succeeds over the wire (signature correctness proven by digest-verified commit) |
| 3 | credential mapping | registry configuration (read-only AK/SK + endpoint) resolves via `FAST_SANDBOX_ARTIFACT_ENDPOINT`; the agent connects to MinIO |
| 4 | builder snapshot ↔ driver restore | driver E2E restores from the pulled artifacts (`FC_SKIP_PREP`: no self-bootstrap); VM `Running`; guest reachable through the slot IP DNAT; single + **concurrent** cases with **execd `/ping` ready on every instance** |
| 5 | idempotency + cleanup | re-PinImage makes zero re-pull (committed cache); driver delete releases the pin through the UDS API; lease lifecycle returns state to zero |

The three scenarios (A: pull chain, B: restore, C: full chain) map to these
points; see the script comments for the exact step order.

## Running

```bash
cd /home/gaoran/fast-sandbox
./scripts/firecracker-chain-e2e.sh
```

The script:

1. downloads the firecracker release, kernel, and bionic rootfs (the driver
   E2E's prep inputs — in `FC_SKIP_PREP` mode the kernel/rootfs are only
   presence checks);
2. starts MinIO (`minio/minio:latest`, ports 9000/9001, AK/SK
   `chain-test`/`chain-test-secret`, bucket `sandbox-images`);
3. exports `alpine:3.19`, builds the builder image, and runs the pipeline
   with `publish: s3://sandbox-images/publish` and machine `1` vCPU / `512Mi`
   (the machine tuple the driver E2E spec validates against);
4. asserts the published layout (verification point 1);
5. builds and starts the real `firecracker-runtime-agent` (UDS socket, state
   root, registry file) and pulls the image twice through `PinImage`
   (points 2/3 + idempotency);
6. runs `TestFirecrackerDriverE2E`, `TestFirecrackerDriverE2ENoInfra`,
   `TestFirecrackerDriverE2EConcurrent` and
   `TestFirecrackerDriverE2EConcurrentSerial` with `FC_SKIP_PREP=1` and
   `FC_AGENT_SOCKET` wired, so the driver restores from the builder artifacts
   and its delete path unpins through the agent (point 4; the Concurrent
   cases are **NoInfra**: 5 VMs restored from the same snapshot set, in
   parallel and sequentially, every instance's execd answers through its own
   slot DNAT — per-clone netns data plane; both print the per-stage load
   breakdown);
7. exercises the lease lifecycle (`LeaseDevices`/`ListLeases`/
   `ReleaseDevices`/`UnpinImage`) and asserts pin/lease state returns to zero
   (point 5);
8. tears everything down (agent, MinIO container, test bridge/netns/rules).

## Overrides

| Variable | Default | Meaning |
|----------|---------|---------|
| `CHAIN_SOURCE_IMAGE` | `alpine:3.19` | OCI image the builder converts |
| `CHAIN_IMAGE` | `chain-test:v1` | Image reference addressed end to end (index key + cache key) |
| `CHAIN_MACHINE_VCPU` / `CHAIN_MACHINE_MEM` | `1` / `512Mi` | Builder machine tuple; must fit the driver E2E spec (request memory ≥ snapshot memory) |
| `CHAIN_ROOTFS_SIZE` | `10Gi` | Builder rootfs logical size |
| `MINIO_IMAGE` / `MINIO_PORT` / `MINIO_AK` / `MINIO_SK` | `minio/minio:latest` / `9000` / `chain-test` / `chain-test-secret` | MinIO deployment |
| `WORK` | `$PWD/.firecracker-chain-e2e` | Workspace (agent binary, state root, logs, MinIO data) |
| `SANDBOX_TEMPLATE_BUILDER_IMAGE` | `sandboxtemplate-builder:chain-e2e` | Builder image name |
| `FC_VERSION` / `FC_BINARY` / `FC_KERNEL` / `FC_ROOTFS` / `FC_STATE_ROOT` | — | Driver E2E overrides (artifacts, state root) |

## Host impact and cleanup

- The driver E2E creates a private `172.30.0.0/24` bridge, one MASQUERADE
  rule, and sets host `ip_forward`; the script restores them on exit (only
  what it created). Per-run netns/taps are always purged.
- MinIO runs as a docker container removed on exit; its data lives under
  `$WORK/minio-data`.
- The builder creates and deletes its own build tap (`fc-build-tap`).
- `./scripts/firecracker-chain-e2e.sh --cleanup` cleans a crashed run.

## Observed results

Green runs on the reference host (`agent-sandbox033067064046.sg52`, xfs
StateRoot, MinIO local, `opensandbox/execd:1.1.0` baked, `native` format,
machine 1 vCPU / 512 MiB, rootfs 2 Gi):

**Builder pipeline** (per run, `builder.log`):

| Phase | 1st | 2nd |
|-------|-----|-----|
| pull / convert | 6.5 s / 0.6 s | 6.5 s / 0.6 s |
| boot → SANDBOX_READY | 2.1 s | 2.1 s |
| snapshot create / restore verify | 5.0 s / 5.1 s | 5.0 s / 5.1 s |
| manifest / publish | 12.4 s / 42.3 s | 12.4 s / 42.3 s |
| **total** | **74 s** | **74 s** |

`bootToReady` dropped from 16.1 s to 2.1 s: with execd injected, readiness
is the execd `/ping` probe instead of the 15 s warmup fallback (~1 s kernel
boot + ~1 s execd readiness).

**Scenario A — cold pull** (first PinImage over MinIO, 3.6 GiB cache):
**~39.5 s**; committed-cache re-pin makes zero requests (verification 5a).

**Scenario B — driver restore** (same snapshot set, all three cases; per-clone
netns data plane, jailer carrier):

| Segment | Infra | NoInfra |
|---------|-------|---------|
| create total | 75-177 ms | 25-39 ms |
| rootfs (reflink copy) | ~1 ms | ~1 ms |
| infra (GuestCopy) | 36-138 ms | — |
| launch (jailer spawn + API socket) | 20.4-21 ms | 20.4-21 ms |
| configure (LoadSnapshot) | 2.3-2.9 ms | 2.3-2.9 ms |
| boot (PATCH /vm resume) | ~0.3 ms | ~0.3 ms |
| guest reachable (ICMP via slot DNAT) | immediate | immediate |
| **execd /ping (VM running + delta)** | **+5.1 ms** | **+5.0 ms** |

The Concurrent case restores 5 VMs in parallel from the same snapshot set
(~106 ms total) without GuestCopy delivery (**NoInfra**: execd is baked into
the snapshot by the builder) and asserts **per-instance reachability +
execd `/ping` ready for every slot** (+4.0-4.5 ms each; per-clone netns +
slot DNAT, the shared baked guest IP/MAC 172.30.0.3 is namespace-isolated,
slot IPs skip the baked address: .2/.4/.5/.6/.7).

The serial baseline (`TestFirecrackerDriverE2EConcurrentSerial`, the
production default path) runs the same batch sequentially and prints the
**per-stage min/avg/max breakdown** for both modes (`load-mode=...` lines in
the driver E2E log). Measured (2026-08-29):

| Stage | serial (avg) | parallel (avg) | Note |
|-------|--------------|----------------|------|
| wall (5 VMs) | **207 ms** | **108 ms** | parallel overlaps VM work (~1.9x) |
| acquire | 22 µs | **16.5 ms** (max 49.9 ms) | **the manager write lock was held by ApplyGuest's netns commands** (~15 ms of `ip netns exec iptables/route` per slot) — ~77% of the parallel wall; fixed by running the guest data plane outside the lock |
| rootfs | 1.03 ms | 1.08 ms | reflink copy |
| launch | 20.4 ms | 20.8 ms | jailer spawn, the largest fixed per-VM cost |
| configure | 2.47 ms | 2.61 ms | LoadSnapshot |
| boot | 321 µs | 304 µs | PATCH /vm resume |

Bottlenecks in order: ① the guest data plane ran under the manager lock
(parallel; fixed — `Manager.ApplyGuest` now clones under the lock, runs the
netns commands outside, and re-validates before committing); ② jailer spawn
~20 ms (per-VM, both modes); ③ ~17 ms/VM of unstaged overhead (ApplyGuest
commands + state persists + idempotency checks). Next optimization
directions: pre-warm the firecracker process (AgentENV warm-pool idea) and
fold the guest data plane into the agent (overlaybd stage).

End-to-end delivery (create → business-ready): **~30 ms** on the driver
layer (VM Running ~25 ms + execd readiness ~5 ms).

### StateRoot MUST be on a reflink-capable filesystem

The 1.8-2.7 s per-create rootfs copy measured earlier on ext4 was the
reflink fallback (full 3 GiB copy). With an **xfs StateRoot**
(`scripts/firecracker-xfs-stateroot.sh --loop`, then
`FC_STATE_ROOT=/var/lib/fast-sandbox`), the copy is COW (~1 ms) and the
NoInfra create drops to ~25 ms. This is a deployment requirement, not an
E2E nicety: **run the StateRoot on xfs/btrfs (reflink-capable), never
ext4**, or every sandbox create pays a full rootfs copy. The chain E2E
probes the StateRoot filesystem at startup and warns when reflink is
unavailable.

## Known gaps

- **~~execd restore flake~~ / stale-netns shadowing (resolved)**: the
  intermittent serial-batch execd failures had two stacked causes. First
  `net.ipv4.conf.all.proxy_arp=1` made every slot netns proxy-answer the
  whole private CIDR (fixed: tap-only proxy ARP). The failure persisted
  with all netns iptables counters at 0 pkts — the probes never entered
  the live netns because a STALE netns from an earlier test still owned
  the slot IP on the shared bridge (its `ip netns del` had failed with
  EBUSY): it answered ARP/pings locally and refused TCP, shadowing the
  new VM. Fixed by deleting the host-side veth before the namespace
  (its peer would otherwise keep the netns busy), a longer delete retry
  (5 x 500 ms), and purging stale fsb netns/bridge devices at the start
  of every E2E environment.
- **OSS vs MinIO**: SigV4 is verified against MinIO here; Aliyun OSS region
  normalization and endpoint-style differences are covered in production
  validation (design details §6.8).
- **Builder NIC dependency**: the restore relies on the snapshot carrying the
  baked NIC (builder `snapshot_stage`); without it, `network_overrides` has
  no iface to replace and the guest is unreachable.
- **Clone networking**: every restored instance resumes with the baked
  guest IP/MAC (172.30.0.3); multi-slot reachability uses the per-clone netns
  data plane (jailer `--netns` + slot DNAT/SNAT), exercised by the
  Concurrent case.
