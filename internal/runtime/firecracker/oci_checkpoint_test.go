package firecracker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestCheckpointDestination(t *testing.T) {
	dest, err := checkpointDestination("r.example/opensandbox-snapshot/checkpoints", "abc")
	require.NoError(t, err)
	require.Equal(t, "r.example/opensandbox-snapshot/checkpoints:abc", dest)
	for _, value := range []string{"", "r.example/ns/r:mutable", "r.example/ns/r@sha256:abc", "http://r.example/ns/r"} {
		_, err = checkpointDestination(value, "abc")
		require.Error(t, err, value)
	}
}
func TestOCIResumeRejectsAnotherSandboxAndTemplateUse(t *testing.T) {
	root := t.TempDir()
	ref := "oci://r.example/ns/r@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	config := &fastletapi.RuntimeSandboxConfig{}
	config.Identity.Namespace = "test"
	config.Identity.SandboxUID = "owner"
	config.Spec.Restore = &fastletapi.RestoreSpec{ManifestRef: ref, ArtifactDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	file := cachedManifestPath(root, ref)
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0750))
	raw, err := json.Marshal(map[string]any{"checkpoint": map[string]string{"sandboxUID": "owner", "namespace": "test"}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, raw, 0640))
	require.NoError(t, validateOCIResumeOwner(root, ref, config))
	config.Identity.SandboxUID = "other"
	require.ErrorIs(t, validateOCIResumeOwner(root, ref, config), ErrInvalidConfig)
	config.Identity.SandboxUID = "owner"
	config.Spec.Restore = nil
	require.ErrorIs(t, validateOCIResumeOwner(root, ref, config), ErrInvalidConfig)
}
func TestOCICheckpointRestoreReferenceBoundToDigest(t *testing.T) {
	sum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := "oci://r.example/ns/r@sha256:" + sum
	spec := fastletapi.SandboxSpec{Restore: &fastletapi.RestoreSpec{ManifestRef: ref, ArtifactDigest: sum}}
	got, err := restoreReference(spec)
	require.NoError(t, err)
	require.Equal(t, ref, got)
	spec.Restore.ArtifactDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, err = restoreReference(spec)
	require.ErrorIs(t, err, ErrInvalidConfig)
	spec.Restore.ManifestRef = "oci://r.example/ns/r:tag"
	_, err = restoreReference(spec)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestPrepareOCICheckpointKeepsNativeFlowIndependentOfRegistry(t *testing.T) {
	dir := t.TempDir()
	state := &SandboxState{}
	state.Config.Identity.SandboxUID = "sandbox-1"
	state.Config.Identity.Namespace = "test"
	state.Phase = PhaseRunning
	state.Config.Spec.Image = "native:golden"
	require.NoError(t, saveState(dir, state))
	plan := dumpPlan{sandboxDir: dir, snapshotID: "checkpoint-id"}
	enabled, err := prepareOCICheckpoint(plan, fastletapi.SnapshotKindCheckpoint)
	require.NoError(t, err)
	require.False(t, enabled)
	state.Config.Spec.Image = "oci://registry.example/templates/alpine:build"
	require.NoError(t, saveState(dir, state))
	_, err = prepareOCICheckpoint(plan, fastletapi.SnapshotKindTemplate)
	require.ErrorIs(t, err, runtimecontract.ErrSnapshotUnsupported)
	t.Setenv(ociSnapshotRepositoryEnv, "registry.example/snapshots/checkpoints")
	enabled, err = prepareOCICheckpoint(plan, fastletapi.SnapshotKindCheckpoint)
	require.NoError(t, err)
	require.True(t, enabled)
	t.Setenv(ociSnapshotRepositoryEnv, "registry.example/snapshots/checkpoints:mutable")
	_, err = prepareOCICheckpoint(plan, fastletapi.SnapshotKindCheckpoint)
	require.ErrorIs(t, err, ErrInvalidConfig)
}
