package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"

	"k8s.io/klog/v2"
)

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
