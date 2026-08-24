package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
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

	if _, err := runner.Run(ctx, "mount", "-o", "loop", rootfsImage, mountpoint); err != nil {
		return fmt.Errorf("mount guest rootfs for Infra delivery: %w", err)
	}
	mounted := true
	defer func() {
		if mounted {
			_, _ = runner.Run(context.Background(), "umount", mountpoint)
		}
	}()

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
	if _, err := runner.Run(ctx, "umount", mountpoint); err != nil {
		return err
	}
	mounted = false
	return nil
}
