package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

// seedRunningSandbox writes a durable Running Sandbox state plus its
// instance rootfs, pointing the API socket at the fake VMM.
func seedRunningSandbox(t *testing.T, fixture *driverFixture, phase VMPhase) string {
	t.Helper()
	state := &SandboxState{
		Config:     fixture.sandboxSpec,
		Phase:      phase,
		APIAddress: fixture.server.socket,
		CreatedAt:  1720000000,
	}
	dir := filepath.Join(fixture.stateRoot, sandboxStateDir, state.Config.Identity.SandboxUID)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, instanceRootfsName), []byte("instance-rootfs-data"), 0o640))
	require.NoError(t, saveState(dir, state))
	return state.Config.Identity.SandboxUID
}

// snapshotAgentFake records the publication and inspects the staging set at
// call time (the driver removes staging right after).
type snapshotAgentFake struct {
	fakeAgentClient
	kind        string
	key         string
	manifest    map[string]any
	sums        string
	missingFile string
}

func (f *snapshotAgentFake) PublishImage(_ context.Context, _, kind, key, dir string) (PublishOutcome, error) {
	payload, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return PublishOutcome{}, err
	}
	if err := json.Unmarshal(payload, &f.manifest); err != nil {
		return PublishOutcome{}, err
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return PublishOutcome{}, err
	}
	f.sums, f.kind, f.key = string(sums), kind, key
	for _, name := range []string{"rootfs.ext4", "vmstate.snap", "memory.snap"} {
		if info, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil || info.Size() == 0 {
			f.missingFile = name
			return PublishOutcome{}, errors.New("incomplete staging set: " + name)
		}
	}
	return PublishOutcome{ManifestRef: "s3://bucket/publish/0123456789abcdef/manifest.json", ArtifactDigest: "digest-" + key}, nil
}

// withSnapshotSpill points the driver at a per-test spill root.
func withSnapshotSpill(t *testing.T) string {
	t.Helper()
	spill := t.TempDir()
	t.Setenv("FAST_SANDBOX_SNAPSHOT_SPILL_DIR", spill)
	return spill
}

func TestCreateSnapshotSpillsDumpOutsideStateRoot(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	spill := withSnapshotSpill(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Equal(t, "digest-app-v2", result.ArtifactDigest)

	// The dump targeted the spill area (absolute paths in direct mode).
	require.Len(t, fixture.server.snapshotDumps, 1)
	require.Equal(t, filepath.Join(spill, "snap-1", jailedDumpVMStateName), fixture.server.snapshotDumps[0].SnapshotPath)
	require.Equal(t, filepath.Join(spill, "snap-1", jailedDumpMemoryName), fixture.server.snapshotDumps[0].MemFilePath)

	// The relocation landed the artifacts in the publish staging: the
	// agent-side manifest assembly saw all three files.
	require.Len(t, agent.manifest["files"].(map[string]any), 3)

	// The spill area is empty again and no dump leaked into the staging
	// placement the driver itself controls (staging is fully removed after
	// publication by CreateSnapshot).
	entries, readErr := os.ReadDir(spill)
	require.NoError(t, readErr)
	require.Empty(t, entries, "the per-snapshot spill directory is removed")
}

func TestCreateSnapshotRecordsActionBindingsInManifest(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
		ActionBindings: []runtimecontract.SnapshotActionBinding{
			{Handler: "egress", Input: `{"egressPolicy":"deny-all"}`},
		},
	})
	require.NoError(t, err)
	recorded, ok := agent.manifest["actionBindings"].([]any)
	require.True(t, ok, "manifest lacks actionBindings")
	require.Len(t, recorded, 1)
	entry := recorded[0].(map[string]any)
	require.Equal(t, "egress", entry["handler"])
	require.Equal(t, `{"egressPolicy":"deny-all"}`, entry["input"])
}

func TestCreateCheckpointRecordsActionBindingsInManifest(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint,
		ActionBindings: []runtimecontract.SnapshotActionBinding{
			{Handler: "egress", Input: `{"defaultAction":"deny"}`},
		},
	})
	require.NoError(t, err)
	recorded, ok := agent.manifest["actionBindings"].([]any)
	require.True(t, ok, "checkpoint manifest lacks actionBindings")
	require.Len(t, recorded, 1)
	entry := recorded[0].(map[string]any)
	require.Equal(t, "egress", entry["handler"])
	require.Equal(t, `{"defaultAction":"deny"}`, entry["input"])
}

func TestCreateSnapshotFallsBackWhenSpillUnavailable(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	// A spill root that does not exist: the capacity guard fails and the
	// dump falls back to the staging directory (legacy absolute paths).
	t.Setenv("FAST_SANDBOX_SNAPSHOT_SPILL_DIR", filepath.Join(t.TempDir(), "missing"))
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Len(t, fixture.server.snapshotDumps, 1)
	require.Equal(t, filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1", vmstateSnapshotName),
		fixture.server.snapshotDumps[0].SnapshotPath)
	require.Len(t, agent.manifest["files"].(map[string]any), 3)
}

func newSnapshotFixture(t *testing.T) (*driverFixture, *snapshotAgentFake) {
	t.Helper()
	fixture := newSnapshotDriverFixture(t)
	agent := &snapshotAgentFake{}
	fixture.driver.agentSocket = "/run/test/agent.sock"
	fixture.driver.newAgentClient = func(string) (AgentClient, error) { return agent, nil }
	return fixture, agent
}

// newSnapshotDriverFixture prepares a driver with a cached image (including
// its manifest facts) and a running fake VMM, without wiring an agent.
func newSnapshotDriverFixture(t *testing.T) *driverFixture {
	t.Helper()
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	fixture.server.running = true
	return fixture
}

func TestCreateSnapshotDumpsPublishesAndResumes(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Equal(t, "snap-1", result.SnapshotID)
	require.Equal(t, "digest-app-v2", result.ArtifactDigest)
	require.Contains(t, result.ManifestRef, "s3://bucket/publish/")
	require.Greater(t, result.SizeBytes, int64(0))

	// The dump paused and resumed the VM and wrote both snapshot files.
	require.Len(t, fixture.server.snapshotDumps, 1)
	require.Equal(t, "Full", fixture.server.snapshotDumps[0].SnapshotType)
	require.True(t, fixture.server.running, "the VM must be resumed")
	require.Contains(t, fixture.server.recordedCalls(), "PATCH /vm")

	// The published manifest copies the source facts verbatim and describes
	// this dump.
	require.Equal(t, "app-v2", agent.key)
	require.Equal(t, fixture.sandboxSpec.Spec.Image,
		agent.manifest["lineage"].(map[string]any)["image"])
	require.Equal(t, map[string]any{
		"vcpu": "2", "memory": "1Gi", "rootfs": "1Gi",
	}, agent.manifest["machine"])
	require.Equal(t, map[string]any{
		"iface": "eth0", "mac": "02:00:00:00:00:01", "ip": "172.30.0.3",
		"gateway": "172.30.0.1", "netmask": "255.255.255.0",
	}, agent.manifest["guestNetwork"])
	require.Equal(t, "native", agent.manifest["format"])
	require.Equal(t, map[string]any{"booted": true, "restored": false}, agent.manifest["validation"])
	files := agent.manifest["files"].(map[string]any)
	require.Len(t, files, 3)
	require.Contains(t, agent.sums, "rootfs.ext4")
	require.Contains(t, agent.sums, "memory.snap")

	// Staging is removed after the publication.
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1"))
	require.True(t, os.IsNotExist(statErr))
}

func TestCreateSnapshotResumesAfterFailedDump(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	fixture.server.mu.Lock()
	fixture.server.failSnapshot = true
	fixture.server.mu.Unlock()

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.Error(t, err)
	require.True(t, fixture.server.running, "the VM must resume even after a failed dump")
	require.Empty(t, agent.fakeAgentClient.publishes, "nothing is published on failure")
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1"))
	require.True(t, os.IsNotExist(statErr), "failed staging is discarded")
}

func TestCreateSnapshotRequiresRunningSandbox(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseStopped)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Empty(t, agent.fakeAgentClient.publishes)
}

func TestCreateSnapshotRequiresAgentForPublication(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	fixture.server.running = true
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.True(t, fixture.server.running, "the VM is resumed even when publication is impossible")
}

func TestCreateSnapshotRequiresSourceManifestFacts(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	fixture.server.running = true
	// A hand-seeded cache without machine/guestNetwork facts.
	dir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(fixture.sandboxSpec.Spec.Image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schemaVersion":1}`), 0o640))
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "machine")
}

func TestCreateSnapshotRejectsInsufficientStagingBeforePause(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	fixture.driver.snapshotCapacityWait = 0 // no GC self-heal wait
	// An absurd memory size makes the footprint exceed any tmpdir's free
	// space without touching the disk.
	fixture.sandboxSpec.Spec.Memory = "99999Gi"
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.ErrorIs(t, err, runtimecontract.ErrInsufficientStorage)
	require.Contains(t, err.Error(), "staging needs")
	// The VM was never paused and nothing was published.
	require.True(t, fixture.server.running)
	require.Empty(t, fixture.server.snapshotDumps)
	require.Empty(t, agent.fakeAgentClient.publishes)
}

func TestDeleteSnapshotDiscardsStaging(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	staging := filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1")
	require.NoError(t, os.MkdirAll(staging, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "manifest.json"), []byte("{}"), 0o644))

	require.NoError(t, fixture.driver.DeleteSnapshot(context.Background(), "snap-1"))
	_, statErr := os.Stat(staging)
	require.True(t, os.IsNotExist(statErr))
	// Idempotent for unknown snapshots.
	require.NoError(t, fixture.driver.DeleteSnapshot(context.Background(), "snap-ghost"))
}

func TestCreateCheckpointPublishesWithoutTemplateIndex(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint,
	})
	require.NoError(t, err)
	require.Equal(t, agentprotocol.PublishKindCheckpoint, agent.kind,
		"the driver must send the agent-protocol kind, not the Fastlet enum string")
	require.Empty(t, agent.key, "a checkpoint publishes no template index key")
	require.Len(t, agent.manifest["files"].(map[string]any), 3)
	require.Contains(t, result.ManifestRef, "/manifest.json")
	require.NotEmpty(t, result.ArtifactDigest)
}

func TestCreateCheckpointRejectsTemplateName(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint, TemplateName: "app-v2",
	})
	require.ErrorContains(t, err, "templateName must be empty")
}

func TestAssembleSnapshotManifestPrefersCheckpointLineage(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	// The Sandbox is itself a resume: its durable spec carries the restore
	// reference, and the source image is NOT cached on this node.
	dir := filepath.Join(fixture.stateRoot, sandboxStateDir, sandboxID)
	state, err := loadState(dir)
	require.NoError(t, err)
	restore := &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/publish/abcdef/manifest.json", ArtifactDigest: "lineage-digest"}
	state.Config.Spec.Restore = restore
	require.NoError(t, saveState(dir, state))

	cacheDir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(artifacts.CheckpointReference(restore.ArtifactDigest)))
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	lineage, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "runtime": "firecracker",
		"machine":      map[string]any{"vcpu": "1", "memory": "512Mi"},
		"guestNetwork": map[string]any{"ip": "10.0.0.2", "gateway": "10.0.0.1"},
		"kernel":       map[string]any{"name": "vmlinux"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "manifest.json"), lineage, 0o644))

	staging := t.TempDir()
	for _, name := range []string{"rootfs.ext4", "vmstate.snap", "memory.snap"} {
		require.NoError(t, os.WriteFile(filepath.Join(staging, name), []byte("data-"+name), 0o640))
	}
	size, err := assembleSnapshotManifest(fixture.stateRoot, staging, dir, nil)
	require.NoError(t, err)
	require.Positive(t, size)

	payload, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(payload, &document))
	require.Equal(t, "10.0.0.2", document["guestNetwork"].(map[string]any)["ip"],
		"the checkpoint lineage facts ride forward into the new checkpoint")
	require.Equal(t, fixture.sandboxSpec.Spec.Image,
		document["lineage"].(map[string]any)["image"])
}

func TestAssembleSnapshotManifestDropsInheritedEgressVaultMarker(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	// The Sandbox is itself a resume from a checkpoint whose manifest carries
	// an egressVault entry; this dump stages no vault file of its own.
	dir := filepath.Join(fixture.stateRoot, sandboxStateDir, sandboxID)
	state, err := loadState(dir)
	require.NoError(t, err)
	restore := &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/publish/abcdef/manifest.json", ArtifactDigest: "lineage-digest"}
	state.Config.Spec.Restore = restore
	require.NoError(t, saveState(dir, state))

	cacheDir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(artifacts.CheckpointReference(restore.ArtifactDigest)))
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	lineage, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "runtime": "firecracker",
		"machine":      map[string]any{"vcpu": "1", "memory": "512Mi"},
		"guestNetwork": map[string]any{"ip": "10.0.0.2", "gateway": "10.0.0.1"},
		"egressVault":  map[string]any{"sha256": "stale", "sizeBytes": 7},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "manifest.json"), lineage, 0o644))

	staging := t.TempDir()
	for _, name := range []string{"rootfs.ext4", "vmstate.snap", "memory.snap"} {
		require.NoError(t, os.WriteFile(filepath.Join(staging, name), []byte("data-"+name), 0o640))
	}
	_, err = assembleSnapshotManifest(fixture.stateRoot, staging, dir, nil)
	require.NoError(t, err)

	payload, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(payload, &document))
	require.NotContains(t, document, "egressVault",
		"a source checkpoint's vault marker must not ride forward into a vault-less dump")
}

func TestCreateSnapshotPublishesWithTemplateKind(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Equal(t, agentprotocol.PublishKindTemplate, agent.kind)
	require.Equal(t, "app-v2", agent.key)
}

func TestCreateSnapshotDefaultsToTemplateKind(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	require.Equal(t, agentprotocol.PublishKindTemplate, agent.kind)
}

func TestCreateCheckpointCommitsNodeLocalCache(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint,
	})
	require.NoError(t, err)

	// The published set is addressable locally under the canonical checkpoint
	// reference, so a resume on this node restores without a store pull.
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	require.NoError(t, verifyRestorableImage(fixture.stateRoot, reference))
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(reference), rootfsImageName))
	require.NoError(t, statErr, "checkpoint cache entry missing its rootfs")
	_, statErr = os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "ckpt-1"))
	require.True(t, os.IsNotExist(statErr), "staging must be consumed by the cache commit")
}

func TestCreateTemplateSnapshotDoesNotCommitCheckpointCache(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", TemplateName: "app-v2",
	})
	require.NoError(t, err)
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	require.ErrorIs(t, verifyRestorableImage(fixture.stateRoot, reference), ErrImageNotReady,
		"template snapshots keep discarding staging (same-node prewarm is issue #57)")
}

// fakeStateMover plays the Handler side of the state transfers: a successful
// export writes the per-kind payload to the driver-provided bundle path (as
// the egress copy would); imports record the handed-over path. A kind with
// no staged payload exports nothing (the "subject held no state of this
// kind" answer).
type fakeStateMover struct {
	mu          sync.Mutex
	exportData  map[runtimecontract.StateKind][]byte
	exportErr   error
	exportCalls []string
	imports     []string
}

func (f *fakeStateMover) ExportState(_ context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exportCalls = append(f.exportCalls, sandboxUID+":"+string(kind)+":"+bundlePath)
	if f.exportErr != nil {
		return f.exportErr
	}
	if data, ok := f.exportData[kind]; ok {
		return os.WriteFile(bundlePath, data, 0o600)
	}
	return nil
}

func (f *fakeStateMover) ImportState(_ context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, sandboxUID+":"+string(kind)+":"+bundlePath)
	return nil
}

func (f *fakeStateMover) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.exportCalls...), append([]string(nil), f.imports...)
}

// vaultMoverWith stages vault content; policyMoverWith stages policy content.
func vaultMoverWith(data []byte) *fakeStateMover {
	return &fakeStateMover{exportData: map[runtimecontract.StateKind][]byte{runtimecontract.StateKindVault: data}}
}

func policyMoverWith(data []byte) *fakeStateMover {
	return &fakeStateMover{exportData: map[runtimecontract.StateKind][]byte{runtimecontract.StateKindPolicy: data}}
}

func TestCreateCheckpointStagesAndPublishesVaultBundle(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	mover := vaultMoverWith([]byte(`{"v":1,"revision":3,"vault":{"token":"t"}}`))
	fixture.driver.SetStateMover(mover)

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint,
	})
	require.NoError(t, err)

	// The staged vault file rides the manifest file list, SHA256SUMS, and the
	// egressVault marker.
	files := agent.manifest["files"].(map[string]any)
	require.Len(t, files, 4)
	require.Contains(t, files, "vault.json")
	require.Equal(t, files["vault.json"], agent.manifest["egressVault"], "the egressVault marker names the staged vault entry")
	require.Contains(t, agent.sums, "vault.json")

	// The node-local cache commit carries the vault file next to the
	// snapshot files, so a same-node restore imports it before the data
	// plane becomes ready.
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	cached, statErr := os.Stat(filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(reference), "vault.json"))
	require.NoError(t, statErr)
	require.Equal(t, int64(len(mover.exportData[runtimecontract.StateKindVault])), cached.Size())
}

func TestCreateCheckpointWithoutVaultStagesNothing(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	fixture.driver.SetStateMover(&fakeStateMover{})

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint,
	})
	require.NoError(t, err)
	require.Len(t, agent.manifest["files"].(map[string]any), 3, "ErrVaultAbsent stages no vault file")
	require.NotContains(t, agent.manifest, "egressVault")
}

func TestCreateCheckpointFailsWhenVaultExportFails(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	fixture.driver.SetStateMover(&fakeStateMover{exportErr: errors.New("handler unreachable")})

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint,
	})
	require.ErrorContains(t, err, "export egress vault")
	// The control plane only releases the runtime after the checkpoint is
	// durable, so a failed export leaves the egress-local vault intact for
	// the retry — assert the staging set was discarded without publishing.
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "ckpt-1"))
	require.True(t, os.IsNotExist(statErr))
}

func TestCreateTemplateSnapshotNeverExportsVault(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	mover := vaultMoverWith([]byte(`{"secret":true}`))
	fixture.driver.SetStateMover(mover)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.NoError(t, err)
	exports, _ := mover.snapshot()
	require.NotEmpty(t, exports, "the policy export is attempted for templates too")
	for _, call := range exports {
		require.NotContains(t, call, "vault:", "a shared boot image must never carry one sandbox's credentials")
	}
	require.Len(t, agent.manifest["files"].(map[string]any), 3)
	require.NotContains(t, agent.manifest, "egressVault")
}

// The recorded policy rides EVERY snapshot bundle — template snapshots
// included (the deliberate asymmetry vs the vault) — and the dumped content
// becomes the manifest's egress binding, overriding the CRD-compiled entry.
func TestCreateTemplateSnapshotStagesAndRecordsPolicy(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	policy := []byte(`{"egress":[{"action":"allow","target":"example.com"}],"defaultAction":"deny"}`)
	mover := policyMoverWith(policy)
	fixture.driver.SetStateMover(mover)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
		ActionBindings: []runtimecontract.SnapshotActionBinding{
			{Handler: "egress", Input: `{"defaultAction":"deny"}`},
			{Handler: "audit", Input: `{"audit":true}`},
		},
	})
	require.NoError(t, err)

	// Track 1: policy.json joins files + SHA256SUMS.
	files := agent.manifest["files"].(map[string]any)
	require.Len(t, files, 4)
	require.Contains(t, files, "policy.json")
	require.Contains(t, agent.sums, "policy.json")

	// Track 2: the dump-recorded binding replaces the CRD egress entry and
	// keeps the unrelated audit binding.
	recorded, ok := agent.manifest["actionBindings"].([]any)
	require.True(t, ok, "manifest lacks actionBindings")
	require.Len(t, recorded, 2)
	byHandler := map[string]string{}
	for _, entry := range recorded {
		item := entry.(map[string]any)
		byHandler[item["handler"].(string)] = item["input"].(string)
	}
	require.Equal(t, string(policy), byHandler["egress"], "the dump is authoritative over the CRD binding")
	require.Equal(t, `{"audit":true}`, byHandler["audit"])
}

// A staged policy file rides the checkpoint cache commit like the vault file.
func TestCreateCheckpointStagesPolicyIntoNodeLocalCache(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	policy := []byte(`{"defaultAction":"deny"}`)
	fixture.driver.SetStateMover(policyMoverWith(policy))

	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "ckpt-1", Kind: fastletapi.SnapshotKindCheckpoint,
	})
	require.NoError(t, err)
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	cached, statErr := os.Stat(filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(reference), "policy.json"))
	require.NoError(t, statErr)
	require.Equal(t, int64(len(policy)), cached.Size())
}

func TestCreateSnapshotFailsWhenPolicyExportFails(t *testing.T) {
	fixture, agent := newSnapshotFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	mover := &fakeStateMover{exportErr: errors.New("handler unreachable")}
	fixture.driver.SetStateMover(mover)

	_, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindTemplate, TemplateName: "app-v2",
	})
	require.ErrorContains(t, err, "export egress policy")
	require.Empty(t, agent.fakeAgentClient.publishes, "nothing is published on a failed export")
	_, statErr := os.Stat(filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-1"))
	require.True(t, os.IsNotExist(statErr), "failed staging is discarded")
}

// writeCheckpointManifest writes a committed checkpoint manifest with an
// egressVault entry describing content.
func writeCheckpointManifest(t *testing.T, dir string, content []byte, withMarker bool) {
	t.Helper()
	document := map[string]any{"schemaVersion": 1, "runtime": "firecracker"}
	if withMarker {
		document["egressVault"] = map[string]any{"sha256": artifacts.SHA256Of(content), "sizeBytes": len(content)}
	}
	payload, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, snapshotManifestName), payload, 0o644))
}

// writeSnapshotManifestWithFiles writes a committed manifest whose files
// list carries the given entries (name -> content).
func writeSnapshotManifestWithFiles(t *testing.T, dir string, entries map[string][]byte) {
	t.Helper()
	files := map[string]any{}
	for name, content := range entries {
		files[name] = map[string]any{"sha256": artifacts.SHA256Of(content), "sizeBytes": len(content)}
	}
	payload, err := json.Marshal(map[string]any{"schemaVersion": 1, "runtime": "firecracker", "files": files})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, snapshotManifestName), payload, 0o644))
}

func TestImportRestoreVaultDeliversBundleBeforeDataPlane(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	mover := &fakeStateMover{}
	fixture.driver.SetStateMover(mover)

	restore := &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/publish/abcdef/manifest.json", ArtifactDigest: "digest"}
	cacheDir := t.TempDir()
	vmstatePath := filepath.Join(cacheDir, vmstateSnapshotName)
	require.NoError(t, os.WriteFile(vmstatePath, []byte("vmstate"), 0o600))
	vaultContent := []byte(`{"vault":true}`)

	// A resumed checkpoint whose bundle carries a provenance-verified vault
	// file: the import hands the exact bundle path to the mover.
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, vaultBundleName), vaultContent, 0o600))
	writeCheckpointManifest(t, cacheDir, vaultContent, true)
	spec := fixture.sandboxSpec.Spec
	spec.Restore = restore
	fixture.driver.importRestoreVault(context.Background(), spec, "checkpoint:sha256:digest", vmstatePath, sandboxID)
	exports, imports := mover.snapshot()
	require.Empty(t, exports)
	require.Equal(t, []string{sandboxID + ":vault:" + filepath.Join(cacheDir, vaultBundleName)}, imports)

	// A manifest without the egressVault marker never authorizes a delivery,
	// and neither does a digest or size mismatch: unverified credential
	// bytes are never handed to egress.
	mover.imports = nil
	writeCheckpointManifest(t, cacheDir, vaultContent, false)
	fixture.driver.importRestoreVault(context.Background(), spec, "checkpoint:sha256:digest", vmstatePath, sandboxID)
	_, imports = mover.snapshot()
	require.Empty(t, imports)

	mover.imports = nil
	writeCheckpointManifest(t, cacheDir, []byte(`{"vault":"other"}`), true)
	fixture.driver.importRestoreVault(context.Background(), spec, "checkpoint:sha256:digest", vmstatePath, sandboxID)
	_, imports = mover.snapshot()
	require.Empty(t, imports)

	mover.imports = nil
	writeCheckpointManifest(t, cacheDir, vaultContent, true)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, vaultBundleName), append(vaultContent, []byte("x")...), 0o600))
	fixture.driver.importRestoreVault(context.Background(), spec, "checkpoint:sha256:digest", vmstatePath, sandboxID)
	_, imports = mover.snapshot()
	require.Empty(t, imports)

	// A manifest that authorizes a vault whose bundle file went missing is a
	// loud degradation, never a silent skip.
	mover.imports = nil
	writeCheckpointManifest(t, cacheDir, vaultContent, true)
	require.NoError(t, os.Remove(filepath.Join(cacheDir, vaultBundleName)))
	fixture.driver.importRestoreVault(context.Background(), spec, "checkpoint:sha256:digest", vmstatePath, sandboxID)
	_, imports = mover.snapshot()
	require.Empty(t, imports)

	// A bundle without a manifest-listed vault imports nothing (vault-less
	// checkpoints).
	mover.imports = nil
	writeCheckpointManifest(t, cacheDir, vaultContent, false)
	fixture.driver.importRestoreVault(context.Background(), spec, "checkpoint:sha256:digest", vmstatePath, sandboxID)
	_, imports = mover.snapshot()
	require.Empty(t, imports)

	// A fresh boot never imports.
	spec.Restore = nil
	fixture.driver.importRestoreVault(context.Background(), spec, "app-v1", vmstatePath, sandboxID)
	_, imports = mover.snapshot()
	require.Empty(t, imports)
}

// The recorded-policy import mirrors the vault's provenance gating, with the
// manifest's policy.json files entry as the authorization.
func TestImportRestorePolicyDeliversBundleBeforeDataPlane(t *testing.T) {
	fixture := newSnapshotDriverFixture(t)
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	mover := &fakeStateMover{}
	fixture.driver.SetStateMover(mover)

	fixture.sandboxSpec.Spec.Restore = &fastletapi.RestoreSpec{ManifestRef: "s3://bucket/publish/abcdef/manifest.json", ArtifactDigest: "digest"}
	cacheDir := t.TempDir()
	vmstatePath := filepath.Join(cacheDir, vmstateSnapshotName)
	require.NoError(t, os.WriteFile(vmstatePath, []byte("vmstate"), 0o600))
	policyContent := []byte(`{"egress":[{"action":"allow","target":"example.com"}],"defaultAction":"deny"}`)

	importPolicy := func() []string {
		fixture.driver.importRestorePolicy(context.Background(),
			fixture.sandboxSpec.Spec, "checkpoint:sha256:digest", vmstatePath, sandboxID)
		_, imports := mover.snapshot()
		return imports
	}

	// A provenance-verified policy file is imported: the record lands before
	// the data plane becomes ready and stays authoritative over the
	// SET_BINDING replay.
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, policyBundleName), policyContent, 0o600))
	writeSnapshotManifestWithFiles(t, cacheDir, map[string][]byte{"policy.json": policyContent})
	require.Equal(t,
		[]string{sandboxID + ":policy:" + filepath.Join(cacheDir, policyBundleName)}, importPolicy())

	// A manifest without the policy.json entry authorizes nothing — the
	// restored subject keeps the bootstrap binding policy.
	mover.mu.Lock()
	mover.imports = nil
	mover.mu.Unlock()
	writeSnapshotManifestWithFiles(t, cacheDir, map[string][]byte{"rootfs.ext4": []byte("x")})
	require.Empty(t, importPolicy())

	// Digest mismatch: never delivered.
	mover.mu.Lock()
	mover.imports = nil
	mover.mu.Unlock()
	writeSnapshotManifestWithFiles(t, cacheDir, map[string][]byte{"policy.json": []byte(`{"defaultAction":"deny"}`)})
	require.Empty(t, importPolicy())

	// Size mismatch: never delivered.
	mover.mu.Lock()
	mover.imports = nil
	mover.mu.Unlock()
	writeSnapshotManifestWithFiles(t, cacheDir, map[string][]byte{"policy.json": policyContent})
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, policyBundleName), append(policyContent, 'x'), 0o600))
	require.Empty(t, importPolicy())

	// Over-size bundle (beyond the ≈1 MiB bound): never delivered, even with
	// a matching manifest entry.
	mover.mu.Lock()
	mover.imports = nil
	mover.mu.Unlock()
	huge := make([]byte, maxPolicyBundleBytes+1)
	writeSnapshotManifestWithFiles(t, cacheDir, map[string][]byte{"policy.json": huge})
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, policyBundleName), huge, 0o600))
	require.Empty(t, importPolicy())

	// An authorized-but-missing file is a loud degradation, never a silent
	// skip (it imports nothing either way).
	mover.mu.Lock()
	mover.imports = nil
	mover.mu.Unlock()
	writeSnapshotManifestWithFiles(t, cacheDir, map[string][]byte{"policy.json": policyContent})
	require.NoError(t, os.Remove(filepath.Join(cacheDir, policyBundleName)))
	require.Empty(t, importPolicy())

	// A fresh boot never imports.
	mover.mu.Lock()
	mover.imports = nil
	mover.mu.Unlock()
	spec := fixture.sandboxSpec.Spec
	spec.Restore = nil
	fixture.driver.importRestorePolicy(context.Background(), spec, "app-v1", vmstatePath, sandboxID)
	_, imports := mover.snapshot()
	require.Empty(t, imports)
}
