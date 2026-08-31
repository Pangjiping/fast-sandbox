package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"

	"k8s.io/klog/v2"
)

// ensureLoopDevices creates the loop device nodes the rootfs loop mount
// needs. A CRI Pod's /dev is not populated from the host even when
// privileged (unlike docker --privileged), so the driver creates the nodes
// itself with CAP_MKNOD; losetup attaches them through the shared kernel.
// Existing nodes (e.g. mounted via hostPath) are left untouched.
func ensureLoopDevices() error {
	devices := []struct {
		path  string
		kind  string
		major int
		minor int
	}{
		{"/dev/loop-control", "c", 10, 237},
		{"/dev/loop0", "b", 7, 0},
		{"/dev/loop1", "b", 7, 1},
		{"/dev/loop2", "b", 7, 2},
		{"/dev/loop3", "b", 7, 3},
		{"/dev/loop4", "b", 7, 4},
		{"/dev/loop5", "b", 7, 5},
		{"/dev/loop6", "b", 7, 6},
		{"/dev/loop7", "b", 7, 7},
	}
	for _, device := range devices {
		if _, err := os.Stat(device.path); err == nil {
			continue
		}
		// mknod must not fail the mount on read-only /dev: the mount below
		// reports the authoritative error if a loop device is missing.
		_ = exec.Command("mknod", device.path, device.kind,
			fmt.Sprintf("%d", device.major), fmt.Sprintf("%d", device.minor)).Run()
	}
	return nil
}

// deliverGuestInfra performs the GuestCopy Infra delivery by loop-mounting the
// per-instance rootfs and copying every prepared artifact to its guest
// destination (sandbox-init, sandbox-tunnel, infra.json, component mappings).
// Kernel-journaled writes survive the guest's later read-write mount, which
// non-journaled debugfs writes do not. The mount costs nothing extra because
// the instance image is a reflink copy with no dirty pages.
func deliverGuestInfra(ctx context.Context, runner fastletnetwork.CommandRunner, rootfsImage string, instance fastletinfra.PreparedInstance) error {
	if runner == nil {
		return fmt.Errorf("%w: command runner is required for Infra delivery", ErrInvalidConfig)
	}
	if len(instance.Mounts) == 0 {
		return nil
	}
	mountpoint, err := os.MkdirTemp("", "fc-infra")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountpoint)
	if err := ensureLoopDevices(); err != nil {
		return err
	}

	// Per-step timing isolates the GuestCopy cost: mounting a multi-GiB
	// sparse ext4 image read-write can initialize metadata/journal regions
	// (allocating blocks over the holes), which is where the delivery
	// latency tends to hide.
	mountStarted := time.Now()
	if _, err := runner.Run(ctx, "mount", "-o", "loop", rootfsImage, mountpoint); err != nil {
		return fmt.Errorf("mount guest rootfs for Infra delivery: %w", err)
	}
	klog.V(4).InfoS("infra delivery: rootfs mounted", "rootfs", rootfsImage, "mountMs", time.Since(mountStarted).Milliseconds())
	mounted := true
	defer func() {
		if mounted {
			_, _ = runner.Run(context.Background(), "umount", mountpoint)
		}
	}()

	copyStarted := time.Now()
	for _, mount := range instance.Mounts {
		if mount.Source == "" || mount.Destination == "" {
			continue
		}
		target := filepath.Join(mountpoint, mount.Destination)
		if _, err := runner.Run(ctx, "mkdir", "-p", filepath.Dir(target)); err != nil {
			return err
		}
		if _, err := runner.Run(ctx, "cp", "-a", mount.Source, target); err != nil {
			return fmt.Errorf("copy Infra artifact to %s: %w", mount.Destination, err)
		}
	}
	klog.V(4).InfoS("infra delivery: artifacts copied", "files", len(instance.Mounts), "copyMs", time.Since(copyStarted).Milliseconds())
	umountStarted := time.Now()
	if _, err := runner.Run(ctx, "umount", mountpoint); err != nil {
		return err
	}
	klog.V(4).InfoS("infra delivery: rootfs unmounted", "umountMs", time.Since(umountStarted).Milliseconds())
	mounted = false
	return nil
}
