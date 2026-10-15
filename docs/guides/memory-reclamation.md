---
title: Firecracker memory reclamation
description: Opt-in free-page reporting, bounded cold-file reclamation, and pressure-aware admission.
---

# Firecracker memory reclamation

Reclamation keeps the sandbox running while returning unused guest pages to
the host. It does not change configured RAM or replace checkpoint pause/resume.
Existing templates keep their current behavior; enable the policy on new ones.

```yaml
spec:
  memoryReclaim:
    freePageReporting: true
    damonReclaim: true
    minAgeSeconds: 60
    quotaMiB: 64
```

Reporting installs a zero-sized balloon with statistics before boot. The guest
needs built-in VIRTIO_BALLOON and PAGE_REPORTING. Optional DAMON reclamation
needs built-in DAMON_RECLAIM with `skip_anon`: only cold file pages are reclaimed,
with reclaim work bounded by 10 ms and `quotaMiB` attempted bytes per second.
Monitoring adds separate CPU cost. Guest free-memory watermarks span the full
range (`high=1000`, `mid=1000`, `low=0`, in per-thousand units), so spare guest RAM
does not retain cold cache on an overcommitted host. Setting `high=0` would
deactivate reclamation whenever free memory exists. Missing interfaces fail the
build instead of silently advertising an inactive policy. DAMON requires reporting.

The policy is recorded in the immutable manifest and inherited by checkpoints.
Rollback selects an older template; reporting cannot be disabled on a live VM.

Compare disabled, reporting-only, and reporting plus DAMON at identical density.
Measure aggregate VMM PSS, guest memory, cgroup usage/events and PSI, CPU cost,
OOMs, and tail latency after idle. Separate peak memory from time-integrated
usage. Verify process/file state through snapshot and cross-host resume. Guest
statistics are advisory, not guaranteed admission headroom. Do not raise
overcommit until measurements establish a safe operating envelope.

Shared pmem/DAX layers are a separate storage optimization.

Upgrade the controller and builder together with the additive SandboxTemplate
CRD before using this field: an older controller's typed spec drops unknown
fields when preparing the build Job. Existing templates need rebuilding to
acquire the balloon device and guest policy. A new Fastlet binary is required
only for host telemetry/admission; existing Firecracker runtimes that support
reporting can restore the new golden snapshot. Verify this against the actual
Firecracker version and storage backend before rollout.

## Pressure-aware admission

Set `FAST_SANDBOX_MEMORY_PRESSURE_POLICY` on the Pool's Fastlet container to
an opt-in JSON policy, for example:

```json
{"nodeMinAvailableBytes":1073741824,"cgroupHighPercent":90,"cgroupLowPercent":80,"maxPSISomeAvg10":10,"recoverySeconds":5}
```

The Fastlet samples host MemAvailable, host memory PSI, and every visible
finite cgroup ancestor budget once per second. It rejects new creates before
side effects when the node floor, cgroup high watermark, or PSI threshold is
crossed. Admission recovers after five continuously healthy seconds: below
the cgroup low watermark, below half the PSI threshold, and at least 10%
above the node floor. Missing or more than three-second-old telemetry blocks
new admission. Cgroup v2 with memory/PSI interfaces is required when enabled.

On kernels without PSI, explicitly set `maxPSISomeAvg10` to `0` to disable
only the PSI gate. Node and cgroup checks remain required. The
`psi_gate_enabled` observation distinguishes this from measured zero pressure.

Rejections reuse retryable `CapacityRejected` feedback and do not consume a
slot. Existing instances and idempotent replays remain accessible. Policies
do not automatically change Pool budgets or call pause, kill, or inflate.
Guest DAMON/FPR and host admission run independently; a healthy host does not
disable guest reclamation, and exhausted headroom stops new demand.

`fast_sandbox_memory_pressure{observation="..."}` exports telemetry validity,
admission state, node available bytes, the highest-utilization finite cgroup's
usage/limit, and PSI some avg10 in percent. Set no environment variable to keep
legacy admission. Roll out Fastlets with the variable removed to restore legacy admission.

The same metric also exports `vmm_rss_bytes`, `vmm_pss_bytes`,
`vmm_observed_processes`, and `vmm_expected_processes` every five seconds.
These aggregate only this Fastlet's process handles. Compare observed/expected
counts before interpreting totals; shared golden pages are apportioned in PSS.

## Limits

Anonymous pages that still belong to a live process are retained. A resident
kernel, browser, or JVM can therefore keep its working set during inactivity;
this policy does not force collection or discard application state.

Native pause removes the VMM but can leave checkpoint file cache charged to
the Fastlet cgroup. This feature does not evict host checkpoint cache, so a
fall in VMM PSS alone does not establish a fall in total cgroup memory.

Idle detection and automatic pause scheduling remain the caller's responsibility.
