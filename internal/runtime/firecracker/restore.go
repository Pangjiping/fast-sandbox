package firecracker

// restore.go implements the golden snapshot restore startup path
// (implementation plan §3.3): restore is the only startup path, the cold
// boot branch is removed. EnsureSandbox configures the machine from the
// cached manifest machine tuple (the snapshot was created with it),
// attaches the per-instance rootfs copy and the NIC, then loads the golden
// snapshot (vmstate + file-backed memory) and starts the instance.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
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

// configureRestoreVM drives the Firecracker API for a snapshot restore:
// machine config (from the manifest machine tuple), the root drive (the
// per-instance reflink copy), the guest network interface, and
// PUT /snapshot/load with the golden vmstate + file-backed memory. The
// boot source is deliberately absent: the snapshot already contains the
// guest state, no kernel is booted.
func configureRestoreVM(ctx context.Context, client *Client, spec fastletapi.SandboxSpec, machine MachineConfigRequest, instanceRootfs, vmstatePath, memoryPath string, slot *fastletnetwork.Slot) error {
	if err := client.ConfigureMachine(ctx, machine); err != nil {
		return fmt.Errorf("configure Firecracker machine: %w", err)
	}
	if err := client.AttachDrive(ctx, DriveRequest{
		DriveID: "root", PathOnHost: instanceRootfs, IsRootDevice: true, IsReadOnly: false,
	}); err != nil {
		return fmt.Errorf("attach Firecracker root drive: %w", err)
	}
	tapDevice := slot.GuestTap
	if tapDevice == "" {
		return fmt.Errorf("%w: slot %s has no pre-provisioned guest tap", ErrNetworkUnavailable, slot.ID)
	}
	if err := client.AttachNetworkInterface(ctx, NetworkInterfaceRequest{
		IfaceID: "eth0", HostDevName: tapDevice, GuestMAC: guestMAC(spec.SandboxID),
	}); err != nil {
		return fmt.Errorf("attach Firecracker network interface: %w", err)
	}
	if err := client.LoadSnapshot(ctx, SnapshotLoadRequest{
		SnapshotPath: vmstatePath,
		MemBackend: SnapshotMemBackend{
			BackendType: "File", BackendPath: memoryPath,
		},
		ResumeVM: false,
	}); err != nil {
		return fmt.Errorf("load Firecracker snapshot: %w", err)
	}
	return nil
}

// guest network config injection (design §4.2, "guest 内配置注入"): a
// restored guest never boots a kernel, so the boot-arg ip= static network
// configuration never applies. The golden rootfs carries a udev hook
// (fast-sandbox/net-up.sh) that runs when the virtio-net device is
// hot-added at restore time and applies the per-instance config written
// here.
const (
	// guestNetConfigDir is the guest-side directory of the injected config
	// and the golden network hook.
	guestNetConfigDir = "/etc/fast-sandbox"
	// guestNetConfigName is the per-instance config file the driver writes.
	guestNetConfigName = "net.conf"
	// guestNetHookPath is the golden rootfs hook that applies the config.
	guestNetHookPath = guestNetConfigDir + "/net-up.sh"
	// guestNetHookMarker marks a cache image whose golden rootfs carries the
	// network hook (baked by the snapshot preparation path). The driver
	// only injects the per-instance config for such images; without the
	// marker the config would be inert.
	guestNetHookMarker = ".net-hook"
)

// deliverGuestNetworkConfig injects the per-instance static guest network
// configuration into the instance rootfs before snapshot restore. The
// golden base's udev hook (baked at snapshot preparation time, marked by
// <images>/<key>/.net-hook) applies it when the restored NIC is hot-added;
// without the hook the config is inert, so a missing marker is not an
// error. The write is a plain loop mount: the instance rootfs is a fresh
// reflink copy that only the driver has mounted before the VM starts.
func deliverGuestNetworkConfig(ctx context.Context, runner fastletnetwork.CommandRunner, stateRoot, image, instanceRootfs string, slot *fastletnetwork.Slot) error {
	if runner == nil || slot == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(stateRoot, imageCacheDir, imageKey(image), guestNetHookMarker)); err != nil {
		// The golden base carries no network hook; the config would be inert.
		return nil
	}
	guestIP, err := fastletnetwork.GuestVMIP(slot)
	if err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(slot.PrivateCIDR)
	if err != nil {
		return fmt.Errorf("%w: invalid private CIDR %q", ErrInvalidConfig, slot.PrivateCIDR)
	}
	mountpoint, err := os.MkdirTemp("", "fc-guestnet")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountpoint)
	if _, err := runner.Run(ctx, "mount", "-o", "loop", instanceRootfs, mountpoint); err != nil {
		return fmt.Errorf("mount instance rootfs for guest network config: %w", err)
	}
	mounted := true
	defer func() {
		if mounted {
			_, _ = runner.Run(context.Background(), "umount", mountpoint)
		}
	}()

	configFile := filepath.Join(mountpoint, guestNetConfigDir, guestNetConfigName)
	dir := filepath.Dir(configFile)
	if _, err := runner.Run(ctx, "mkdir", "-p", dir); err != nil {
		return err
	}
	// Write the config via a host temp file + cp, mirroring the GuestCopy
	// delivery pattern so the runner records the write in tests.
	source, err := os.CreateTemp("", "fc-netconf")
	if err != nil {
		return err
	}
	sourcePath := source.Name()
	defer os.Remove(sourcePath)
	if _, err := source.WriteString(fmt.Sprintf("GUEST_IP=%s\nGUEST_PREFIX=%d\nGATEWAY=%s\n", guestIP, prefix.Bits(), slot.Gateway)); err != nil {
		_ = source.Close()
		return err
	}
	if err := source.Chmod(0o600); err != nil {
		_ = source.Close()
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "cp", "-a", sourcePath, configFile); err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "umount", mountpoint); err != nil {
		return err
	}
	mounted = false
	return nil
}
