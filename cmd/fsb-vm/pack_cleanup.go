package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
)

const (
	packActiveMarker  = ".pack-resources-active"
	packPhasePrepared = "prepared"
	packPhaseAdding   = "adding"
	packPhaseAttached = "attached"
	packPhaseReleased = "released"
)

type packResources struct {
	Work  string `json:"work"`
	Phase string `json:"phase"`
}

func writePackResources(marker, work, phase string) error {
	raw, err := json.Marshal(packResources{Work: work, Phase: phase})
	if err != nil {
		return err
	}
	return writeFileAtomic(marker, raw, 0600)
}

// The lock lives outside the bundle, so removing staging cannot unlink a live lock.
func lockPackBundle(bundle string) (func(), error) {
	absolute, err := filepath.Abs(bundle)
	if err != nil {
		return nil, err
	}
	return tryLock(filepath.Dir(absolute), "pack-"+digest.FromString(absolute).Encoded())
}

func newPackCleanupCommand() *cobra.Command {
	var bundle string
	command := &cobra.Command{Use: "cleanup-pack", Short: "Release retained checkpoint pack devices before removing staging", RunE: func(cmd *cobra.Command, _ []string) error {
		release, err := lockPackBundle(bundle)
		if err != nil {
			return err
		}
		defer release()
		return recoverPack(cmd.Context(), bundle, newUblkdClient(global.ublkdSocket), unmount)
	}}
	command.Flags().StringVar(&bundle, "bundle-dir", "", "checkpoint directory passed to pack")
	_ = command.MarkFlagRequired("bundle-dir")
	return command
}

func recoverPack(ctx context.Context, bundle string, devices deviceReleaser, unmount func(string) error) error {
	marker := filepath.Join(bundle, packActiveMarker)
	raw, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Never let a corrupted marker select another build's devices or directories.
	record := packResources{Work: string(raw), Phase: packPhaseAdding}
	if strings.HasPrefix(string(raw), "{") {
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
	}
	switch record.Phase {
	case packPhasePrepared, packPhaseAdding, packPhaseAttached, packPhaseReleased:
	default:
		return fmt.Errorf("unknown retained pack phase %q", record.Phase)
	}
	root, err := filepath.EvalSymlinks(global.buildWorkDir)
	if err != nil {
		return err
	}
	work, err := filepath.EvalSymlinks(record.Work)
	if err != nil {
		return fmt.Errorf("resolve retained pack work: %w", err)
	}
	if filepath.Dir(work) != root || !strings.HasPrefix(filepath.Base(work), "snapshot_") {
		return fmt.Errorf("retained pack work %q is outside the configured pack work root", work)
	}
	if err := unmount(filepath.Join(work, "mnt-snapfiles")); err != nil {
		return err
	}
	matches, err := releaseOwnedDevicesByConfig(ctx, devices, filepath.Join(record.Work, snapfilesConfigName))
	if err != nil {
		return err
	}
	if record.Phase == packPhaseAdding && matches == 0 {
		return fmt.Errorf("pack device add may still be in flight; retaining backing files")
	}

	if err := os.RemoveAll(work); err != nil {
		return err
	}
	return os.Remove(marker)
}
