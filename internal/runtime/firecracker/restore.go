package firecracker

// restore.go implements the golden snapshot restore startup path
// (implementation plan §3.3): restore is the only startup path, the cold
// boot branch is removed. EnsureSandbox launches the Firecracker process
// with the state directory as its working directory (so the relative
// rootfs.img path baked in the vmstate resolves to this instance's reflink
// copy), validates the request memory against the manifest machine tuple,
// then loads the golden snapshot (vmstate + file-backed memory) as the
// FIRST API call and starts the instance. Devices (root drive, NIC) are
// restored from the snapshot; only the NIC host tap is replaced per
// instance via network_overrides.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"k8s.io/apimachinery/pkg/api/resource"
)

// cachedManifestPath returns the commit-point manifest of a pulled image.
func cachedManifestPath(stateRoot, image string) string {
	return filepath.Join(stateRoot, imageCacheDir, imageKey(image), "manifest.json")
}

// manifestMachine is the machine tuple recorded in the published manifest
// (builder records {vcpu, memory} as resource quantities).
type manifestMachine struct {
	VCPU   string `json:"vcpu"`
	Memory string `json:"memory"`
}

// readCachedManifestMachine loads the machine tuple from the cached
// manifest. It reports false when the manifest is absent or carries no
// machine tuple (hand-seeded local cache).
func readCachedManifestMachine(stateRoot, image string) (manifestMachine, bool, error) {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manifestMachine{}, false, nil
		}
		return manifestMachine{}, false, err
	}
	var document struct {
		Machine manifestMachine `json:"machine"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return manifestMachine{}, false, fmt.Errorf("decode cached manifest: %w", err)
	}
	if document.Machine.VCPU == "" || document.Machine.Memory == "" {
		return manifestMachine{}, false, nil
	}
	return document.Machine, true, nil
}

// resolveRestoreMachineConfig returns the machine configuration of a
// snapshot restore. Restore requires the machine tuple of the golden
// snapshot (Firecracker rejects a mem_size_mib different from the one the
// vmstate was created with), so the manifest machine is authoritative. The
// request cpu/mem only validate: a request memory below the snapshot memory
// is rejected with an explicit error. When the cached manifest carries no
// machine tuple (hand-seeded local cache), the request profile falls back
// to the previous request-based resolution.
func resolveRestoreMachineConfig(spec fastletapi.SandboxSpec, config runtimecatalog.FirecrackerConfig, stateRoot, image string) (MachineConfigRequest, error) {
	machine, ok, err := readCachedManifestMachine(stateRoot, image)
	if err != nil {
		return MachineConfigRequest{}, err
	}
	if !ok {
		return resolveMachineConfig(spec, config)
	}
	vcpus, err := machineVCPUs(machine.VCPU)
	if err != nil {
		return MachineConfigRequest{}, err
	}
	snapshotMiB, err := machineMemMiB(machine.Memory)
	if err != nil {
		return MachineConfigRequest{}, err
	}
	if spec.Memory != "" {
		requestMiB, err := parseMemMiB(spec.Memory)
		if err != nil {
			return MachineConfigRequest{}, err
		}
		if requestMiB < snapshotMiB {
			return MachineConfigRequest{}, fmt.Errorf("%w: requested memory %d MiB is below the template snapshot memory %d MiB", ErrInvalidConfig, requestMiB, snapshotMiB)
		}
	}
	return MachineConfigRequest{VCPUs: vcpus, MemSizeMiB: snapshotMiB}, nil
}

// machineVCPUs parses the manifest vcpu quantity into a vCPU count.
func machineVCPUs(value string) (int, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid manifest vcpu %q", ErrInvalidConfig, value)
	}
	vcpus := int(math.Ceil(float64(quantity.MilliValue()) / 1000.0))
	if vcpus < 1 {
		return 0, fmt.Errorf("%w: manifest vcpu %q yields no vCPU", ErrInvalidConfig, value)
	}
	return vcpus, nil
}

// machineMemMiB parses the manifest memory quantity into MiB.
func machineMemMiB(value string) (int, error) {
	return parseMemMiB(value)
}

// parseMemMiB parses a resource quantity into whole MiB.
func parseMemMiB(value string) (int, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid memory %q", ErrInvalidConfig, value)
	}
	mib := int(math.Ceil(float64(quantity.Value()) / (1024.0 * 1024.0)))
	if mib < 1 {
		return 0, fmt.Errorf("%w: memory %q yields no MiB", ErrInvalidConfig, value)
	}
	return mib, nil
}

// restoreSnapshotPath returns the cache path of a golden snapshot artifact.
func restoreSnapshotPath(stateRoot, image, name string) (string, error) {
	path := filepath.Join(stateRoot, imageCacheDir, imageKey(image), name)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", fmt.Errorf("%w: %q", ErrImageNotReady, image)
	}
	return path, nil
}

// vmstateSnapshotName and memorySnapshotName are the golden snapshot files
// the pull layer commits into the image cache (implementation plan §5).
const (
	vmstateSnapshotName = "vmstate.snap"
	memorySnapshotName  = "memory.snap"
)

// resolveRestoreSnapshotFiles returns the cached vmstate and memory snapshot
// paths of an image, both of which restore requires.
func resolveRestoreSnapshotFiles(stateRoot, image string) (vmstate, memory string, err error) {
	vmstate, err = restoreSnapshotPath(stateRoot, image, vmstateSnapshotName)
	if err != nil {
		return "", "", err
	}
	memory, err = restoreSnapshotPath(stateRoot, image, memorySnapshotName)
	if err != nil {
		return "", "", err
	}
	return vmstate, memory, nil
}

// configureRestoreVM drives the Firecracker API for a snapshot restore.
// v1.16 requires LoadSnapshot to be the FIRST configuration call: any
// machine/drive/network API call before it sets the boot path and is
// rejected ("Loading a microVM snapshot not allowed after configuring
// boot-specific resources"). All devices (root drive, NIC) are restored
// from the vmstate; only the NIC host tap can be replaced via
// network_overrides. The boot source is deliberately absent: the snapshot
// already contains the guest state, no kernel is booted.
//
// The root drive path is baked in the vmstate as a relative path
// ("rootfs.img") so each instance resolves it to its own reflink copy in
// its state directory (the Firecracker process cwd).
func configureRestoreVM(ctx context.Context, client *Client, slot *fastletnetwork.Slot, vmstatePath, memoryPath string) error {
	tapDevice := slot.GuestTap
	if tapDevice == "" {
		return fmt.Errorf("%w: slot %s has no pre-provisioned guest tap", ErrNetworkUnavailable, slot.ID)
	}
	if err := client.LoadSnapshot(ctx, SnapshotLoadRequest{
		SnapshotPath: vmstatePath,
		MemBackend: SnapshotMemBackend{
			BackendType: "File", BackendPath: memoryPath,
		},
		ResumeVM:     false,
		NetworkOverrides: []SnapshotNetworkOverride{{
			IfaceID: "eth0", HostDevName: tapDevice,
		}},
	}); err != nil {
		return fmt.Errorf("load Firecracker snapshot: %w", err)
	}
	return nil
}
