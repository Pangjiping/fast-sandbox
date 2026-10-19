package main

import (
	"context"
	"encoding/json"
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
