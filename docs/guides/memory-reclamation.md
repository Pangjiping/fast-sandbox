---
title: Firecracker memory reclamation
description: Default free-page reporting with existing Fastlet memory observation and admission.
---

# Firecracker memory reclamation

Free-page reporting (FPR) returns unused guest pages to the host while the sandbox keeps running. It does not change configured RAM, static resource commitments, or checkpoint pause/resume. New object-store golden snapshots enable FPR by default without a SandboxTemplate field. Existing snapshots retain their saved device state; rebuild a template to acquire reporting. OCI templates currently use the separate fsb-vm build path, which does not configure FPR.

The object-store builder installs a zero-sized balloon with statistics before boot. The Firecracker binary must support `free_page_reporting`; an unsupported VMM fails the build. The guest needs built-in VIRTIO_BALLOON and PAGE_REPORTING. A custom kernel without either interface can still boot: its console reports `SANDBOX_FREE_PAGE_REPORTING_UNAVAILABLE`, and the manifest records `freePageReporting: false`.

The builder records the observed guest capability as the optional `freePageReporting` boolean in the immutable manifest. Snapshots and checkpoints inherit it along with the saved balloon device and guest state. Older manifests omit the field, which does not establish reporting support. Reporting cannot be disabled on a live VM; rollback selects an older template.

## Feedback to Fastlet and FastPath

FPR releases backing pages from the Firecracker process, so its effect is reflected in the runtime owner's cgroup memory usage. Fastlet's existing resource admission observer reads `memory.current`, `memory.max`, memory PSI, OOM events, and host available memory. It uses the existing watermarks and recovery hysteresis to update resource admission state. Fastlet reports that state and its revision to FastPath through the existing heartbeat and Create responses.

This feature adds no separate pressure policy, admission gate, or Fastlet-to-FastPath protocol. Configure ResourceAware Pools and the existing memory observer as described in [Resource-aware overcommit tuning](resource-aware-overcommit-tuning.md). LegacySlot Pools retain their existing admission behavior. Guest free-memory statistics and the `freePageReporting` capability flag are not admission headroom and do not change the static commitment ledger.

Compare old snapshots and reporting snapshots at identical density. Measure cgroup usage, PSI, OOMs, aggregate VMM PSS, CPU cost, and tail latency after idle. Verify process and file state through snapshot and cross-host resume. Do not raise overcommit merely because a guest reports free pages or a single reclaim sample succeeds.

## Compatibility and limits

Upgrade the object-store builder and rebuild templates; no new CRD field is required. Existing Firecracker runtimes that support reporting restore the saved balloon device state. Verify the deployed Firecracker version and storage backend before rollout. Existing Fastlet memory admission and FastPath feedback remain responsible for placement and admission decisions.

Only pages already freed by the guest are reported. FPR does not actively evict cold file cache, reclaim live anonymous working sets, configure DAMON, or automatically pause sandboxes. A resident kernel, browser, or JVM can retain its working set during inactivity. Shared pmem/DAX layers are a separate storage optimization.

Native pause removes the VMM but can leave checkpoint file cache charged to the Fastlet cgroup. FPR does not evict host checkpoint cache, so a fall in VMM PSS alone does not establish a fall in total cgroup memory. Idle detection and automatic pause scheduling remain the caller's responsibility.
