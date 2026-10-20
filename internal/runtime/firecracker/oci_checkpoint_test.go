package firecracker

import (
	"encoding/json"
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
	ref := "oci://r.example/ns/r@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	config := &fastletapi.RuntimeSandboxConfig{}
	config.Identity.Namespace = "test"
	config.Identity.SandboxUID = "owner"
	config.Spec.Restore = &fastletapi.RestoreSpec{ManifestRef: ref, ArtifactDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	raw, err := json.Marshal(map[string]any{"checkpoint": map[string]string{"sandboxUID": "owner", "namespace": "test"}})
	require.NoError(t, err)
	var metadata ociRestoreMetadata
	require.NoError(t, json.Unmarshal(raw, &metadata))
	require.NoError(t, validateOCIResumeOwner(metadata, config))
	config.Identity.SandboxUID = "other"
	require.ErrorIs(t, validateOCIResumeOwner(metadata, config), ErrInvalidConfig)
	config.Identity.SandboxUID = "owner"
	config.Spec.Restore = nil
	require.ErrorIs(t, validateOCIResumeOwner(metadata, config), ErrInvalidConfig)
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

func TestPrepareOCISnapshotKeepsNativeFlowIndependentOfRegistry(t *testing.T) {
	dir := t.TempDir()
	state := &SandboxState{}
	state.Config.Identity.SandboxUID = "sandbox-1"
	state.Config.Identity.Namespace = "test"
	state.Phase = PhaseRunning
	state.Config.Spec.Image = "native:golden"
	require.NoError(t, saveState(dir, state))
	plan := dumpPlan{sandboxDir: dir, snapshotID: "checkpoint-id"}
	enabled, err := prepareOCISnapshot(plan)
	require.NoError(t, err)
	require.False(t, enabled)
	state.Config.Spec.Image = "oci://registry.example/templates/alpine:build"
	require.NoError(t, saveState(dir, state))
	t.Setenv(ociSnapshotRepositoryEnv, "")
	destination, err := checkpointDestinationForSource(state.Config.Spec.Image, plan.snapshotID)
	require.NoError(t, err)
	require.Equal(t, "registry.example/templates/alpine:checkpoint-id", destination)
	enabled, err = prepareOCISnapshot(plan)
	require.NoError(t, err)
	require.True(t, enabled, "default OCI pause must not require an extra environment variable")
	t.Setenv(ociSnapshotRepositoryEnv, "registry.example/snapshots/checkpoints")
	destination, err = checkpointDestinationForSource(state.Config.Spec.Image, plan.snapshotID)
	require.NoError(t, err)
	require.Equal(t, "registry.example/snapshots/checkpoints:checkpoint-id", destination)
	enabled, err = prepareOCISnapshot(plan)
	require.NoError(t, err)
	require.True(t, enabled)
	t.Setenv(ociSnapshotRepositoryEnv, "registry.example/snapshots/checkpoints:mutable")
	_, err = prepareOCISnapshot(plan)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestOCISnapshotMetadataRemovesInheritedCheckpointOwner(t *testing.T) {
	raw := []byte(`{"format":"native","oci":{"rootfs":"old"},"checkpoint":{"sandboxUID":"previous","namespace":"old"},"machine":{"vcpu":"2","memory":"1Gi"},"actionBindings":[{"handler":"egress","input":"deny"}]}`)
	owner := fastletapi.SandboxIdentity{SandboxUID: "current", Namespace: "tenant"}
	for _, kind := range []fastletapi.SnapshotKind{fastletapi.SnapshotKindTemplate, fastletapi.SnapshotKindCheckpoint} {
		t.Run(string(kind), func(t *testing.T) {
			data, err := ociSnapshotMetadata(raw, kind, owner)
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, json.Unmarshal(data, &document))
			require.Equal(t, "overlaybd", document["format"])
			require.NotContains(t, document, "oci")
			require.Contains(t, document, "machine")
			require.Contains(t, document, "actionBindings")
			if kind == fastletapi.SnapshotKindTemplate {
				require.NotContains(t, document, "checkpoint")
			} else {
				require.Equal(t, map[string]any{"sandboxUID": "current", "namespace": "tenant"}, document["checkpoint"])
			}
		})
	}
}

func TestDisabledOCIRejectsLiveSnapshotBeforeDump(t *testing.T) {
	t.Setenv("FAST_SANDBOX_OCI_DISABLED", "true")
	fixture := newSnapshotDriverFixture(t)
	fixture.sandboxSpec.Spec.Image = "oci://r.example/ns/template:base"
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	_, err := fixture.driver.CreateSnapshot(t.Context(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "live-snapshot", Kind: fastletapi.SnapshotKindTemplate,
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, "FAST_SANDBOX_OCI_DISABLED=true")
	require.Empty(t, fixture.server.snapshotDumps)
	require.True(t, fixture.server.running)
}
