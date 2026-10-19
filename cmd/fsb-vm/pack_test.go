package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/ocitemplate"
)

func TestPackRejectsTruncatedCheckpointBeforeKernelResources(t *testing.T) {
	dir := t.TempDir()
	metadata := vmMetadata{VCPUCount: 1, MemoryMB: 1, DiskSizeMB: 1, RootfsDrivePath: rootfsDriveLink}
	raw, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), raw, 0600))
	for _, file := range []string{"rootfs.ext4", "memory.snap", "vmstate.snap", "manifest.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("short"), 0600))
	}
	err = packCheckpoint(context.Background(), dir, "snapshot:test")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match machine metadata")
}

func TestPackedPairPreservesRootfsParentLayers(t *testing.T) {
	layout, _, err := createStagedArtifactLayout(t.TempDir(), "incremental")
	require.NoError(t, err)
	parent := ocispec.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("parent"), Size: 100}
	delta := ocispec.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("delta"), Size: 20}
	snap := ocispec.Descriptor{MediaType: ocitemplate.OverlayBDLayerMediaType, Digest: digest.FromString("memory"), Size: 50}
	names, err := parseOutput("snapshot:test")
	require.NoError(t, err)
	require.NoError(t, writePackedPair(layout, names, []ocispec.Descriptor{parent, delta}, snap))
	var index layoutIndex
	raw, err := os.ReadFile(filepath.Join(layout.root, "manifest/index.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &index))
	require.Len(t, index.Images, 2)
	raw, err = os.ReadFile(layout.manifestPath(index.Images[0].ManifestFile))
	require.NoError(t, err)
	var manifest ocispec.Manifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.Equal(t, []ocispec.Descriptor{parent, delta}, manifest.Layers)
}

func TestRecoverPackReleasesOnlyOwnedDeviceBeforeRemovingFiles(t *testing.T) {
	for _, failure := range []string{"", "unmount", "list", "detach", "outside", "symlink"} {
		t.Run(failure, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			bundle := t.TempDir()
			global.buildWorkDir = filepath.Join(bundle, "pack-work")
			require.NoError(t, os.MkdirAll(global.buildWorkDir, 0750))
			work, err := os.MkdirTemp(global.buildWorkDir, "snapshot_test-")
			require.NoError(t, err)
			config := filepath.Join(work, snapfilesConfigName)
			require.NoError(t, os.WriteFile(config, []byte("backing-files"), 0600))
			marker := filepath.Join(bundle, packActiveMarker)
			markerWork := work
			if failure == "outside" {
				markerWork = t.TempDir()
			}
			if failure == "symlink" {
				markerWork = filepath.Join(global.buildWorkDir, "snapshot_link")
				require.NoError(t, os.Symlink(t.TempDir(), markerWork))
			}
			require.NoError(t, os.WriteFile(marker, []byte(markerWork), 0600))
			devices := &fakeReleaser{devices: []ublkdDevice{{DevID: 1, Config: config}, {DevID: 2, Config: filepath.Join(bundle, "another.config")}}, delErr: map[int]error{}}
			if failure == "list" {
				devices.listErr = errors.New("list failed")
			}
			if failure == "detach" {
				devices.delErr[1] = errors.New("detach failed")
			}
			unmounted := false
			err = recoverPack(context.Background(), bundle, devices, func(path string) error {
				require.Equal(t, filepath.Join(work, "mnt-snapfiles"), path)
				require.FileExists(t, config)
				require.Empty(t, devices.deleted)
				unmounted = true
				if failure == "unmount" {
					return errors.New("unmount failed")
				}
				return nil
			})
			if failure != "" {
				require.Error(t, err)
				require.FileExists(t, marker)
				require.FileExists(t, config)
				require.Empty(t, devices.deleted)
			} else {
				require.NoError(t, err)
				require.True(t, unmounted)
				require.Equal(t, []int{1}, devices.deleted)
				require.NoDirExists(t, work)
				require.NoFileExists(t, marker)
				require.NoError(t, recoverPack(context.Background(), bundle, devices, func(string) error { t.Fatal("already recovered"); return nil }))
			}
		})
	}
}

func TestPackRecoveryRefusesActivePack(t *testing.T) {
	previous := global
	t.Cleanup(func() { global = previous })
	global.stateDir = t.TempDir()
	bundle := t.TempDir()
	release, err := lockPackBundle(bundle)
	require.NoError(t, err)
	_, err = lockPackBundle(bundle)
	require.ErrorIs(t, err, errBusy)
	release()
	release, err = lockPackBundle(bundle)
	require.NoError(t, err)
	release()
}

func TestRecoverPackKeepsUncertainAddBackingFiles(t *testing.T) {
	for _, phase := range []string{"prepared", "adding", "attached", "released"} {
		t.Run(phase, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			bundle := t.TempDir()
			global.buildWorkDir = filepath.Join(bundle, "pack-work")
			work := filepath.Join(global.buildWorkDir, "snapshot_test-work")
			require.NoError(t, os.MkdirAll(work, 0750))
			marker := filepath.Join(bundle, packActiveMarker)
			require.NoError(t, writePackResources(marker, work, phase))
			err := recoverPack(context.Background(), bundle, &fakeReleaser{}, func(string) error { return nil })
			if phase == "adding" {
				require.ErrorContains(t, err, "may still be in flight")
				require.DirExists(t, work)
				require.FileExists(t, marker)
			} else {
				require.NoError(t, err)
				require.NoDirExists(t, work)
				require.NoFileExists(t, marker)
			}
		})
	}
}

func TestValidatePackBundlePreservesMetadataAndChecksMachine(t *testing.T) {
	for _, mode := range []string{"valid", "invalid-cpu", "invalid-drive", "short-memory", "short-rootfs"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			machine := vmMetadata{VCPUCount: 1, MemoryMB: 1, DiskSizeMB: 1, RootfsDrivePath: rootfsDriveLink}
			if mode == "invalid-cpu" {
				machine.VCPUCount = 0
			}
			if mode == "invalid-drive" {
				machine.RootfsDrivePath = "/unrelated"
			}
			raw, err := json.Marshal(machine)
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, json.Unmarshal(raw, &document))
			document["checkpointExtension"] = map[string]string{"keep": "opaque metadata"}
			raw, err = json.MarshalIndent(document, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), raw, 0600))
			for _, name := range []string{"rootfs.ext4", "memory.snap", "vmstate.snap", "manifest.json"} {
				file, err := os.Create(filepath.Join(dir, name))
				require.NoError(t, err)
				size := int64(1)
				if name == "rootfs.ext4" || name == "memory.snap" {
					size = 1 << 20
				}
				if (mode == "short-memory" && name == "memory.snap") || (mode == "short-rootfs" && name == "rootfs.ext4") {
					size--
				}
				require.NoError(t, file.Truncate(size))
				require.NoError(t, file.Close())
			}
			metadata, upper, err := validatePackBundle(dir)
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, raw, metadata)
				require.Nil(t, upper)
			} else {
				require.Error(t, err)
			}
		})
	}
}
