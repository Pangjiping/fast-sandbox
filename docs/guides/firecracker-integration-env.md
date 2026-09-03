# Firecracker Integration Environment — Deployment Guide & Production Notes

> Scope: the single-host integration environment built by
> `scripts/integration-env.sh`, and the operational issues that matter when
> the same chain is deployed to a real cluster.
>
> Companion diagram: `docs/assets/firecracker-integration-environment.svg`
> (deployment forms, node-shared cache, XFS reflink layer).

## 1. What this environment is

A bare-metal KVM host running the full fast-sandbox Firecracker chain with
one command:

```
SandboxTemplate (builder Pod, in-cluster)
  → golden snapshot published to MinIO
  → node runtime-agent (DaemonSet) pulls + caches
  → SandboxPool schedules fastlet Pods (×2, 5 slots each)
  → per-sandbox Firecracker VM restore (jailer --netns, shared snapshot)
  → execd :44772 /ping verified end-to-end (direct fastlet-proxy route)
```

One-liners:

```bash
./scripts/integration-env.sh up                    # full environment (tasks 1-8)
CHAIN_E2E=1 ./scripts/integration-env.sh up        # task 0 (host-level component chain E2E) + full environment
./scripts/integration-env.sh verify    # 2 + 5 sandboxes, proxy-chain probe, teardown asserts
./scripts/integration-env.sh status    # component / template / pool health
./scripts/integration-env.sh down      # host left clean
```

`up` runs the optional **task 0** first when `CHAIN_E2E=1`: the host-level
component chain E2E (`scripts/firecracker-chain-e2e.sh`, builder → MinIO →
runtime-agent → driver restore with **no Kubernetes**) gates the cluster run
on a green component chain. Off by default: it rebuilds the builder image
and re-downloads the firecracker set (~10 min). It self-cleans on exit and
its leftover resources are also detected and purged by `down`.

`up` and the plain `verify` only prove execd `/ping` delivery. For execd
**protocol** usability in the guest (OpenSandbox issue #1695: `POST
/command` hangs on the snapshot while `/ping` works) run:

```bash
./scripts/integration-env.sh verify-execd-api   # execd API battery + verdict
```

It creates one sandbox, drives the execd HTTP API over the same
DIRECT_FASTLET_PROXY route the `/ping` probes use (`/ping`, `POST /command`
SSE across echo/pipe/sleep/false/missing command classes, malformed-JSON
400), then curls the guest `172.30.0.3:44772` straight from the slot netns
(no proxy at all) when the fastlet image ships busybox wget. Rows classify
each case PASS / RESPONDED / HANG / FAIL and the stage prints an explicit
verdict: `REPRODUCED #1695` (hang, with an init-stall vs stdout-tail
root-cause read) or `NOT REPRODUCED` (API fully usable).

## 2. Deployment topology (deployment forms)

| Component | Form | Node | Key mounts / wiring |
|---|---|---|---|
| MinIO | host Docker container | host | on the kind network (container IP = endpoint); publish + pull credentials |
| controller | Deployment (1 replica) | control plane | CRDs + RBAC + route-keys; Fast-Path gRPC :9090 |
| builder Pod | Job (on-demand) | KVM-labeled node | /dev/kvm, /dev/net/tun, self-mknod loop devices, publish creds |
| runtime installer | DaemonSet (per-node) | firecracker-node | installs firecracker v1.16.1 + jailer + kernel → hostPath |
| runtime-agent | DaemonSet (per-node) | firecracker-node | UDS socket + StateRoot shared with fastlets; MinIO pull creds |
| fastlet Pod | pool-managed Pod ×2 | firecracker-node | profile hostPaths auto-injected; agent socket; registry plan |
| firecracker VM | per-sandbox microVM | inside fastlet netns | golden restore, guest eth0 172.30.0.3, execd :44772 |
| verify CLI | host binaries | host | fastctl + gen-endpoint + port-forwards |

Node labels: `sandbox.fast.io/kvm=true` (builder, hardcoded by the
SandboxTemplate controller) and `fast-sandbox.io/firecracker-node=true`
(installer / agent / fastlet affinity).

## 3. What `up` actually does (stage order matters)

0. **task 0 — host-level component chain E2E** (only with `CHAIN_E2E=1`):
   `firecracker-chain-e2e.sh` validates the bare chain (builder publish →
   real MinIO SigV4 pull → PinImage idempotency → driver restore → lease
   lifecycle) before any cluster resource exists. Its exit leaves the host
   clean (own MinIO container, fsb bridge/netns/MASQUERADE, agent, jails all
   removed); its `chain-e2e-minio` container / fsb leftovers are also part
   of `up`'s leftover detection and `down`'s cleanup. Workspace and
   re-downloadable artifacts stay under `$WORK/chain-e2e` for reuse.
1. **preflight + tooling** — installs kind/kubectl/jq/xfsprogs when missing.
2. **sysctl** — `fs.inotify.max_user_instances=8192`.
3. **images** — `make images` for 7 images + host `fastctl`/`gen-endpoint`.
4. **XFS StateRoot** — sparse loop image mounted at `/var/lib/fast-sandbox`,
   reflink probe, **before** `kind create` (the node bind-mounts it).
5. **kind cluster** — KVM/tun/shm passthrough; node labels.
6. **MinIO** — on the kind network (container IP), not the host gateway:
   docker-proxy reachability from the kind bridge is unreliable.
7. **credentials** — publish secret (builder), agent registry.json (pull),
   pool registry (fast-sandbox-registry), agent endpoint ConfigMap.
8. **CRDs + controller** — `config/all-in-one`, images loaded into kind.
9. **installer** — binaries land before fastlet pods start (hostPath File
   mounts fail otherwise).
10. **agent** — readiness = POST /v1/health with a real podUID (read routes
    are POST-only and validate caller identity).
11. **template build** — builder Pod → publish → manifest assertions
    (index/manifest/artifactDigest/sizeBytes/guestNetwork).
12. **pool** — 2 fastlet pods (poolMin=2), warmImages Cached on both.

## 4. Verification and the delivery baseline

`verify` creates 2 probe sandboxes, then a 5-sandbox batch (7 total across
two fastlets). Every sandbox is probed **end-to-end** by resolving the port
route with `DIRECT_FASTLET_PROXY` (the assigned fastlet-proxy, resolved from
the durable assignment annotation) and curling execd /ping straight to it —
no central sandbox-proxy, no dependency on the eventually-consistent
`status.placement` projection. Finally everything is deleted and leases
drained + jails cleaned are asserted.

Key timing nodes (all in ms, logged to `$WORK/run.log`):

```
key node: golden restore of '<sbx>'            (driver-internal, precise)
  total=33ms  rootfs=0.6ms (reflink)  launch=20ms (VMM exec)
key node: end-to-end latency (run → execd /ping)
  t(run)/t(run-done)/t(probe)/t(ping) epoch-ms
  run RPC = 55ms   queue-to-probe = ...   first-200 = ...   total = ...
key node: proxy-chain /ping latency (same route reused)
  cold /ping / warm /ping    (chain setup vs steady state)
```

Baseline on the reference node (XFS StateRoot):

- **run RPC ≈ 55 ms** (control plane create + VM restore 33 ms), flat under
  concurrency.
- **first-200 ≈ 0.2-0.8 s on a cold sandbox**: the guest's first request
  after a snapshot resume. The VM and execd resume instantly (33 ms), but
  the guest's network stack takes roughly a second to serve — this is a
  guest-side post-restore settle window, not control plane, proxy, or host
  behavior (a gateway-ARP refresh attempt was measured and falsified). The
  second request is single-digit ms.
- **warm chain ≈ 7-9 ms** (direct fastlet-proxy path).
- `queue-to-probe` grows under sequential batch probing: it is the probe
  order, not system latency.

## 5. Key implementation decisions (learned the hard way)

- **MinIO joins the kind network**; endpoint = container IP. The kind
  gateway + docker-proxy path fails on some hosts (firewalld/hairpin).
- **No vhost-vsock passthrough**: the driver only probes /dev/kvm and
  /dev/net/tun; mounting /sys/.../vhost-vsock breaks kind create on hosts
  without the device.
- **CRI pods lack the host /dev**: the builder and the firecracker driver
  `mknod` /dev/loop* themselves (privileged, CAP_MKNOD) before loop mounts.
- **fastctl `get -o json`** encodes the full `GetSandboxResponse` with
  standard encoding/json (underscore keys, numeric enums): state lives at
  `.sandbox.runtime.state` / `.sandbox.data_plane.state` (READY == 4).
- **fastctl `exec`** is a subcommand of `opensandbox` and requires a
  declared `execd` Infra Component. This environment has **no
  infraComponents** (execd is baked into the golden snapshot), so the probe
  uses the **port route** (`/v1/sandboxes/{uid}/ports/44772`) with
  `DIRECT_FASTLET_PROXY` access mode: fastpath resolves the assigned
  fastlet from the durable assignment annotation (written synchronously at
  create), and curl goes straight to that fastlet-proxy — this bypasses the
  sandbox-proxy's `status.placement` dependency entirely.
- **The central sandbox-proxy route resolution depends on the serialized
  controller status projection**: `Index.UpsertSandbox` and
  `ResolveFresh` read `status.placement`, which the sandbox controller
  (single worker at the time) projected one sandbox at a time — batch
  first-requests through the central proxy grew ~0.5 s per sandbox. See
  the integration environment measurements: with DIRECT_FASTLET_PROXY the
  batch first-200 is flat, no serialization.
- **A full pool is rejected before the CR exists**: fastpath
  `CreateSandbox` returns `ResourceExhausted` when no fastlet has capacity,
  so demand-driven scale-out can never trigger through the API — the pool
  must be pre-sized (`poolMin=2`) for the 7-sandbox batch.
- **warm-pull idempotency is per-fastlet**: two fastlets warming the same
  image collided in the agent journal ("request id committed by pod X");
  the key is now `warm-pull-<podUID>-<image-sha>`.
- **The first request after a snapshot resume takes ~0.5-1 s** even though
  the VM and execd resume in ~33 ms: the guest's network stack needs a
  settle window before serving. A gateway-ARP refresh (raw-socket announce)
  was implemented and measured — no effect, the mechanism is guest-kernel
  post-restore processing, not a stale neighbour table. Compressing it
  requires guest-side work, not host data-plane changes.

## 6. Issues to watch in a REAL deployment

### 6.1 Host / kernel prerequisites
- **cgroup v2 is mandatory** for kind (kubelet fails to create the
  `kubepods` cgroup on v1 hosts). Enable via
  `systemd.unified_cgroup_hierarchy=1` + reboot; verify
  `docker info` shows `Cgroup Version: 2`.
- **KVM**: `/dev/kvm` present, `grep -c vmx /proc/cpuinfo` > 0; node kernel
  and the snapshot's `compatibility.hostKernel` should match the bake host.
- **loop devices** on the host for the XFS loop image; the builder/driver
  create their own nodes inside pods (see §5).
- `fs.inotify.max_user_instances` ≥ 8192 (kubelet/containerd watchers).

### 6.2 StateRoot filesystem — the single biggest latency lever
- Put the node StateRoot on **reflink-capable XFS** (or Btrfs). Without it
  every sandbox pays a full multi-GiB rootfs copy (~2.5 s); with it the
  per-instance rootfs is a CoW reflink (~1 ms).
- The cache, the agent journal, and the per-sandbox jails **must share one
  filesystem** (reflink only works within a filesystem) and one StateRoot
  (`/var/lib/fast-sandbox/firecracker`) across agent + all fastlets.
- Size: sparse rootfs (2 GiB declared) × concurrency + snapshot cache;
  monitor real usage (`du`, not `ls` — sparse).

### 6.3 Cold-start measurement pitfalls
- Reusing an already-Ready sandbox makes `run → first-200` meaningless
  (the create is skipped and the warm guest answers in ms). The verify
  deletes leftovers first — a 9 ms run RPC with a 33 ms restore is the
  tell-tale of a reused warm sandbox.
- 2s-poll waits drown sub-second numbers: use the 10 ms `wait_until` for
  baseline waits.
- Distinguish VM cost (driver `total`, ~33 ms) from the guest's
  post-restore settle (~0.5-1 s, first request only) and the steady-state
  chain (~8 ms).
- Sequential probing: report first-200 from each sandbox's own probe start
  (`t(probe)`), not from its create return — the latter accumulates the
  earlier sandboxes' probe/reporting time and fakes linear growth.

### 6.4 Node asset installation (installer)
- The DaemonSet downloads firecracker from GitHub releases and the kernel
  from `s3.amazonaws.com` — fine for the integration env, **not** for
  production: use a private artifact mirror, pre-baked node images, or
  signed hostPath assets, and pin `FC_VERSION`/`KERNEL_URL`.
- The jailer/`vmlinux.bin` **must match the snapshot's baked kernel** and
  firecracker version (restore compatibility), and must exist before any
  fastlet starts (hostPath File mounts fail otherwise).

### 6.5 Network & multi-node
- Per-clone netns: each sandbox gets a slot IP in the fastlet pod netns;
  guest eth0 is the baked 172.30.0.3 (clone model) — reachability is
  **per-fastlet-pod**, plan probes/access accordingly.
- On multi-node clusters every node needs: installer + agent DaemonSets,
  the `firecracker-node` label, KVM passthrough (or a device plugin), and
  the XFS StateRoot (per-node local disk, not a shared NAS — reflink and
  jailer chroot are node-local by design).
- The agent is **per-node**; a node's cache is single-copy (PinImage
  dedup). The agent socket + StateRoot hostPath must be mounted
  identically in the agent DaemonSet and every fastlet.

### 6.6 Control plane behavior to know
- **fastpath fast-path rejects, never queues**: a full pool →
  `ResourceExhausted` before the Sandbox CR exists. Either pre-size the
  pool (`poolMin`), or accept a queuing layer in front of the API.
- **Sandbox status schema is API-versioned** (the sandbox-actions refactor
  moved `status.assignment.*` → `status.placement.*`, `runtimeState` →
  `runtime.state`); pin tooling/CLI versions to the control plane.
- The pool controller injects runtime plan / registry / infra into fastlet
  pods; changing `runtime-environments.yaml` rolls fastlets and requires
  re-`up` in this environment.

### 6.7 Credentials & security (production hardening)
- This environment uses dev route-keys and MinIO AK/SK literals; production
  must use a secret manager for: publish creds (builder), pull creds
  (agent), pool registry secrets, and route-signing keys.
- `privileged` fastlet/builder pods + hostPath `/var/lib/fast-sandbox`,
  `/dev/kvm`, `/dev/net/tun`: scope with PodSecurity admission, avoid
  whole-`/dev` mounts, and run node components with read-only root
  filesystems where possible.
- Route credentials are short-lived and signed (Ed25519); keep clocks in
  sync (NTP) — expiry checks are wall-clock based.

### 6.8 Images & artifact store
- Pin image tags (`:dev` in this env) to digests in production; `kind load`
  is a dev-only distribution mechanism.
- The artifact store must be S3-compatible with immutable,
  digest-addressed objects (`index/<sha256(image)>.json`). Publish and
  pull credentials should be separate (write vs read-only).
- The store endpoint must be reachable from: builder Pod, agent DaemonSet
  (both in-cluster) — a host-side MinIO needs the same routing story as the
  kind-network container-IP trick.

### 6.9 Logging & failure forensics
- Every stage writes `$WORK/logs/`; failures dump component logs to
  `failure-<task>-<ts>.txt` before exiting — keep this contract in
  production (collect pod logs + serial consoles on builder/fastlet
  failures; the KVM failure scene is in the serial console, not klog).
- `environment.txt` snapshots tool versions + endpoints at `up` start.

## 7. Environment variables (all optional)

| Variable | Default | Meaning |
|---|---|---|
| `WORK` | `$PWD/.integration-env` | workspace + logs |
| `KIND_CLUSTER` / `KIND_NODE_IMAGE` / `KIND_RETAIN` | firecracker / - / 0 | kind knobs; mirror override, retain-for-debug |
| `MINIO_PORT` / `MINIO_AK` / `MINIO_SK` / `MINIO_ENDPOINT` | 9000 / ... | store credentials; endpoint auto = container IP |
| `SBX_IMAGE` / `EXECD` / `FC_VERSION` | alpine:3.19 / execd:1.1.0 / v1.16.1 | the chain keys |
| `CHAIN_E2E` | 0 | 1 runs task 0: the host-level component chain E2E (`firecracker-chain-e2e.sh`) before the cluster tasks (~10 min; forwards `SBX_IMAGE`/`EXECD`/`ROOTFS_SIZE`/`FC_VERSION`) |
| `CONCURRENCY` | 5 | per-fastlet slot capacity for the batch |
| `EXECD_API_SBX` | sandbox-execd-api | sandbox name used by `verify-execd-api` |
| `EXECD_API_KEEP_SANDBOX` | 0 | 1 keeps the sandbox + jail after the battery and prints the guest-console (firecracker.log) tail command for execd-side diagnosis |
| `DEBUG_PROBE` | 0 | print every probe attempt (resolve/host/curl code) for the batch window |
| `XFS_STATEROOT` / `XFS_SIZE` | 1 / 16G | reflink layer on/off, virtual size |
| `SKIP_TOOL_INSTALL` / `SKIP_LEFTOVER_CLEAN` | 0 | manual tooling / refuse auto-rebuild |
