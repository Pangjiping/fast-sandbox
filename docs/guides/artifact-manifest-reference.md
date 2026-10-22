# Artifact manifest reference

`manifest.json` is the single metadata contract for both OCI snapfiles and native object-store artifacts. The shared Go model is `artifacts.VMManifest` in `internal/artifacts/manifest.go`; Builder, fsb-vm, Fastlet and runtime-agent use its types and validation. `schemaVersion` remains `1`; historical layouts are not supported and must be rebuilt. No `metadata.json` is produced or read.

## Core VM contract

```json
{
  "schemaVersion": 1,
  "runtime": "firecracker",
  "machine": {
    "vcpuCount": 2,
    "memoryMiB": 1024,
    "rootfsSizeMiB": 20480,
    "rootfsDriveID": "rootfs",
    "rootfsDrivePath": "rootfs.dev"
  },
  "kernel": { "version": "6.1.177" },
  "compatibility": {
    "cpuArch": "x86_64",
    "firecrackerVersion": "1.16.1",
    "cpuTemplate": "none",
    "vendor": "GenuineIntel",
    "cpuFamily": 6,
    "cpuModel": 85,
    "cpuStepping": 7
  }
}
```

CPU count is a positive integer; memory and disk capacity are positive integer MiB, recorded exactly once. The rootfs drive path is `rootfs.dev` for fsb-vm/OCI and `rootfs.img` for native snapshots, matching the relative path baked into vmstate. `rootfsDriveID` records the logical device ID. `kernel.version`, CPU architecture and a known Firecracker version are required. `cpuTemplate=none` requires a resolved vendor/family/model/stepping identity; `T2` and `T2A` use the supported Firecracker-version allowlists. Restore validates architecture and CPU compatibility; the independent CLI's `--force` only downgrades VMM version mismatches.

The fixed OCI snapshot paths are `/vmstate.bin`, `/memfile` and `/manifest.json`. Native object storage publishes `rootfs.ext4`, `vmstate.snap`, `memory.snap`, `manifest.json` and `SHA256SUMS`. VM state and memory reside in those binary files, not in the JSON document.

## Optional shared records

`guestNetwork` describes a NIC baked into vmstate: `iface` and `mac`, plus optional `ip`, `gateway`, `netmask` and `mtu`. A standalone VM without a NIC omits it. Fastlet's static guest networking requires the complete network record. Host tap names are allocated on restore and are not persisted here.

`lineage` records optional `image`, `imageDigest` and `createdAt`. Template builds record their source and creation time. Snapshots preserve the original digest and capture compatibility, updating only the source reference for the running Sandbox. `compatibility.cpuModelName` and `hostKernel` are optional diagnostics.

## Platform extension

The entire `platform` object is optional for independent fsb-vm operation. It contains:

- `execd`, `entrypoint`, `init` and literal `envs`: build provenance. Do not put secrets in these published fields.
- `egressMitmCA`: optional public certificate fingerprints (`certificates[].certSha256`, `spkiSha256`, `notAfter`).
- `checkpoint`: `{ "sandboxUID": "...", "namespace": "..." }` for a private checkpoint. Reusable template snapshots omit inherited ownership. OCI restores also cross-check the Registry owner annotations resolved by fsb-vm.
- `stateFiles`: entries for `policy.json` and `vault.json`, each containing `sha256` and `sizeBytes`. Each state file is described exactly once here; there is no separate `egressVault` marker. Fastlet verifies size and digest before handing the opaque bytes to handlers. Native downloads use the same entries to fetch state files.

OCI `manifest.json` does not contain a S3-style VM file list: Registry manifests describe the image layers. Native `files` lists only `rootfs.ext4`, `vmstate.snap` and `memory.snap`, with `sha256` and `sizeBytes`. Native state files appear only under `platform.stateFiles`. The native image index points at this manifest and its byte digest; artifacts upload before the manifest, and the image index uploads last.

## Independent build and additional input

Without additional input, `fsb-vm build` generates the full core VM contract and optional source lineage itself. It does not need Kubernetes or Fastlet.

```bash
fsb-vm build --source registry.example/base@sha256:... \
  --output template:build --kernel /opt/kernels/vmlinux-6.1.177
```

`--manifest-extra-file` accepts an optional JSON object containing only `guestNetwork`, `lineage` and `platform`. It supplements the generated document rather than replacing it. Core keys such as `schemaVersion`, `runtime`, `machine`, `kernel` and `compatibility` are rejected. Baked NIC identity, source digest and creation time cannot be changed by supplemental input. Optional source `lineage.image` may retain the original image tag while its digest remains pinned.

```json
{
  "guestNetwork": {
    "ip": "172.30.0.3",
    "gateway": "172.30.0.1",
    "netmask": "255.255.255.0",
    "mtu": 1500
  },
  "platform": { "execd": "registry.example/execd:v1" }
}
```

The guest network supplement requires a NIC configured by build's `--tap-name` and `--guest-mac`. Producers write the final manifest before sealing snapfiles. `pack` validates machine sizes and preserves the complete input manifest, including unknown optional extensions; snapshot updates preserve unknown metadata while replacing this capture's state-file entries and ownership.

## Version policy

Readers explicitly accept only `schemaVersion: 1`. Missing or unsupported versions fail before admission. New ignorable optional fields may retain the version; changing required fields, units, types or restore semantics requires a future version and an explicit decoder. Unknown optional metadata is carried through repackaging; it must not change required restore behavior. Core and platform metadata share the same version.

Deploy matching Builder/fsb-vm/Fastlet/runtime-agent versions and rebuild existing OCI and S3 templates before using this format. Existing snapshots using the historical structure must also be recreated; changing their version number alone does not convert them. This change deliberately provides no legacy-format decoder.
