package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	infracontract "fast-sandbox/internal/infra/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

// firecrackerConfigForTest returns the built-in Firecracker configuration with
// the StateRoot redirected to a test directory.
func firecrackerConfigForTest(t *testing.T, root string) runtimecatalog.FirecrackerConfig {
	t.Helper()
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	profile.Firecracker.StateRoot = root
	return *profile.Firecracker
}

// fakeNetworkDriver no-ops the host netns preparation steps.
type fakeNetworkDriver struct{}

func (fakeNetworkDriver) Prepare(context.Context, *fastletnetwork.Slot) error  { return nil }
func (fakeNetworkDriver) Validate(context.Context, *fastletnetwork.Slot) error { return nil }
func (fakeNetworkDriver) Destroy(context.Context, *fastletnetwork.Slot) error  { return nil }

// memoryStateStore keeps slots in memory for the manager fixture.
type memoryStateStore struct {
	mu    sync.Mutex
	slots map[string]*fastletnetwork.Slot
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{slots: make(map[string]*fastletnetwork.Slot)}
}

func (s *memoryStateStore) LoadAll(context.Context) ([]*fastletnetwork.Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots := make([]*fastletnetwork.Slot, 0, len(s.slots))
	for _, slot := range s.slots {
		slots = append(slots, cloneSlotForTest(slot))
	}
	return slots, nil
}

func (s *memoryStateStore) Save(_ context.Context, slot *fastletnetwork.Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots[slot.ID] = cloneSlotForTest(slot)
	return nil
}

func (s *memoryStateStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.slots, id)
	return nil
}

func cloneSlotForTest(slot *fastletnetwork.Slot) *fastletnetwork.Slot {
	clone := *slot
	return &clone
}

func newNetworkManagerForTest(t *testing.T) *fastletnetwork.Manager {
	t.Helper()
	root := t.TempDir()
	manager, err := fastletnetwork.NewManager(fastletnetwork.Config{
		Capacity: 1, PodUID: "pod-1", PrivateCIDR: "172.30.0.0/24",
		StateRoot: root, NetNSRoot: filepath.Join(root, "netns"), HostNetNSRoot: filepath.Join(root, "host-netns"),
		IDGenerator: func() (string, error) { return "slot-1", nil },
		Now:         func() time.Time { return time.Unix(1720000000, 0) },
	}, fakeNetworkDriver{}, newMemoryStateStore())
	require.NoError(t, err)
	require.NoError(t, manager.Initialize(context.Background()))
	return manager
}

// statefulFakeServer scripts the Firecracker API and records calls.
type statefulFakeServer struct {
	mu            sync.Mutex
	calls         []string
	snapshotLoads []SnapshotLoadRequest
	running       bool
	socket        string
}

func newStatefulFakeServer(t *testing.T) *statefulFakeServer {
	server := &statefulFakeServer{}
	server.socket = startFakeFirecracker(t, server.handle)
	return server
}

func (s *statefulFakeServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/snapshot/load" {
		var request SnapshotLoadRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err == nil {
			s.snapshotLoads = append(s.snapshotLoads, request)
		}
	}
	s.mu.Unlock()
	switch r.URL.Path {
	case "/version":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.15.1"}`))
	case "/actions":
		var action actionRequest
		_ = json.NewDecoder(r.Body).Decode(&action)
		s.mu.Lock()
		s.running = action.ActionType == "InstanceStart"
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "/":
		s.mu.Lock()
		state := "NotStarted"
		if s.running {
			state = "Running"
		}
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"` + state + `"}`))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *statefulFakeServer) recordedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *statefulFakeServer) recordedSnapshotLoads() []SnapshotLoadRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SnapshotLoadRequest(nil), s.snapshotLoads...)
}

// driverFixture wires a Driver with fake Firecracker, process, and network
// backends.
type driverFixture struct {
	driver      *Driver
	runner      *infraRunner
	launcher    *fakeProcessRunner
	server      *statefulFakeServer
	killCalls   []int
	stateRoot   string
	manager     *fastletnetwork.Manager
	sandboxSpec fastletapi.SandboxSpec
}

func newDriverFixture(t *testing.T) *driverFixture {
	t.Helper()
	stateRoot := t.TempDir()
	config := firecrackerConfigForTest(t, stateRoot)
	profile := runtimecatalog.RuntimeProfile{
		Name: apiv1alpha2.RuntimeFirecracker, ProfileHash: "hash",
		Firecracker:  &config,
		Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady},
	}
	server := newStatefulFakeServer(t)
	launcher := &fakeProcessRunner{}
	runner := &infraRunner{}
	fixture := &driverFixture{
		runner: runner, launcher: launcher, server: server, stateRoot: stateRoot,
		manager: newNetworkManagerForTest(t),
	}
	driver := &Driver{
		profile: profile, config: config,
		runner: runner, launcher: launcher,
		newClient: func(string) *Client { return NewClient(server.socket) },
		stat:      func(string) (os.FileInfo, error) { return fakeFileInfoForTest{}, nil },
		killProcess: func(pid int) error {
			fixture.killCalls = append(fixture.killCalls, pid)
			return nil
		},
		probeProcess:         func(int) error { return nil },
		waitSocket:           func(context.Context, string, time.Duration) error { return nil },
		processes:            make(map[string]Process),
		imageGCInterval:      defaultImageGCInterval,
		imageCacheLimitBytes: defaultImageCacheLimitBytes,
	}
	driver.SetNetworkManager(fixture.manager)
	fixture.driver = driver
	fixture.sandboxSpec = fastletapi.SandboxSpec{
		SandboxID: "sandbox-1", ClaimUID: "claim-1", ClaimName: "sandbox-1", ClaimNamespace: "tenant-a",
		InstanceGeneration: 1, RuntimeInstanceID: "runtime-1", AssignmentAttempt: 1,
		FastletPodUID: "pod-1", Image: "example.com/app:v1", CPU: "2", Memory: "1Gi",
		RuntimeProfileHash: "hash", ResourceProfileHash: "r-hash",
	}
	return fixture
}

func (f *driverFixture) prepareCachedImage(t *testing.T, image string) {
	t.Helper()
	dir := filepath.Join(f.stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs-image-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmstateSnapshotName), []byte("vmstate-snapshot-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, memorySnapshotName), []byte("memory-snapshot-data"), 0o640))
	manifest, err := json.Marshal(map[string]any{
		"machine": map[string]any{"vcpu": "2", "memory": "1Gi"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o640))
}

func TestNewRequiresProfileConfig(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	require.NotNil(t, profile.Firecracker)

	profile.Firecracker = nil
	_, err = New(profile)
	require.Error(t, err)
}

func TestInitializeValidatesBootConfig(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	broken, err := New(fixture.driver.profile)
	require.NoError(t, err)
	broken.config.BinaryPath = ""
	require.ErrorIs(t, broken.Initialize(context.Background(), ""), ErrInvalidConfig)

	invalid, err := New(fixture.driver.profile)
	require.NoError(t, err)
	invalid.config.BootTimeoutSeconds = 0
	require.ErrorIs(t, invalid.Initialize(context.Background(), ""), ErrInvalidConfig)
}

func TestProbeCapabilitiesFailsClosedWithProfileGate(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.driver.profile.Capabilities.DefaultState = runtimecatalog.CapabilityUnsupported
	fixture.driver.profile.Capabilities.Reason = "FirecrackerDriverUnimplemented"
	report := fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityUnsupported, report.State)
	require.Equal(t, "FirecrackerDriverUnimplemented", report.Reason)
}

func TestProbeCapabilitiesReady(t *testing.T) {
	fixture := newDriverFixture(t)
	report := fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityReady, report.State)
	require.Empty(t, report.Missing)

	fixture.driver.stat = func(path string) (os.FileInfo, error) {
		if path == "/dev/kvm" {
			return nil, os.ErrNotExist
		}
		return fakeFileInfoForTest{}, nil
	}
	report = fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "KVMUnavailable", report.Reason)
	require.Contains(t, report.Missing, "/dev/kvm")
}

type fakeFileInfoForTest struct{}

func (fakeFileInfoForTest) Name() string       { return "probe" }
func (fakeFileInfoForTest) Size() int64        { return 0 }
func (fakeFileInfoForTest) Mode() os.FileMode  { return 0 }
func (fakeFileInfoForTest) ModTime() time.Time { return time.Time{} }
func (fakeFileInfoForTest) IsDir() bool        { return false }
func (fakeFileInfoForTest) Sys() any           { return nil }

func TestEnsureSandboxDeliversInfraComponents(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	infraJSON := filepath.Join(t.TempDir(), "infra.json")
	require.NoError(t, os.WriteFile(infraJSON, []byte(`{"version":1}`), 0o400))
	fixture.driver.prepareInfra = func(context.Context, *fastletapi.SandboxSpec) (fastletinfra.PreparedInstance, error) {
		return fastletinfra.PreparedInstance{
			ConfigHostPath: infraJSON,
			Mounts: []fastletinfra.Mount{
				{Source: infraJSON, Destination: "/.fast/run/infra.json", Options: []string{"ro"}},
			},
			Services: []infracontract.ServiceEndpoint{
				{Component: "execd", Protocol: "HTTP", Port: 44772},
			},
			Diagnostics: []infracontract.ComponentDiagnostic{{Component: "execd", State: "Starting"}},
		}, nil
	}
	fixture.driver.infraMgr = &fastletinfra.Manager{} // enable the infra path; prepareInfra is faked

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)
	require.Len(t, metadata.InfraServices, 1)
	require.Equal(t, uint32(44772), metadata.InfraServices[0].Port)
	require.Len(t, metadata.InfraDiagnostics, 1)

	joined := strings.Join(fixture.runner.commands, "\n")
	require.Contains(t, joined, "mount -o loop")
	require.Contains(t, joined, "/.fast/run/infra.json")
	require.Contains(t, joined, "umount ")

	// The persisted state carries the services for route publication.
	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	state, err := loadState(directory)
	require.NoError(t, err)
	require.Len(t, state.InfraServices, 1)
}

func TestEnsureSandboxInfraFailureReleasesResources(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	fixture.driver.prepareInfra = func(context.Context, *fastletapi.SandboxSpec) (fastletinfra.PreparedInstance, error) {
		return fastletinfra.PreparedInstance{}, errors.New("plan revision mismatch")
	}
	root := t.TempDir()
	store, err := fastletinfra.NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	fixture.driver.infraMgr, err = fastletinfra.NewManagerWithConfig(fastletinfra.ManagerConfig{Store: store, Resolver: fakeResolver{}})
	require.NoError(t, err)

	_, err = fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.ErrorIs(t, err, ErrInfraUnavailable)
	require.Empty(t, fixture.launcher.started)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
}

// fakeResolver is a minimal ArtifactResolver used to construct an Infra
// Manager in tests.
type fakeResolver struct{}

func (fakeResolver) Prepare(context.Context, infracatalog.ArtifactSource, *fastletinfra.ArtifactStore) (fastletinfra.PreparedSource, error) {
	return fastletinfra.PreparedSource{}, nil
}

func TestEnsureSandboxBootsVM(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)
	require.Equal(t, "sandbox-1", metadata.ContainerID)
	require.Equal(t, 4242, metadata.PID)
	require.Equal(t, string(PhaseRunning), metadata.Phase)
	require.Empty(t, metadata.InfraServices)

	require.Len(t, fixture.launcher.started, 1)
	require.Equal(t, "/usr/local/bin/firecracker", fixture.launcher.started[0][0])
	require.Contains(t, fixture.launcher.started[0], "--api-sock")

	calls := fixture.server.recordedCalls()
	require.Contains(t, calls, "PUT /machine-config")
	require.NotContains(t, calls, "PUT /boot-source")
	require.Contains(t, calls, "PUT /drives/root")
	require.Contains(t, calls, "PUT /network-interfaces/eth0")
	require.Contains(t, calls, "PUT /snapshot/load")
	require.Contains(t, calls, "PUT /actions")

	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	state, err := loadState(directory)
	require.NoError(t, err)
	require.Equal(t, PhaseRunning, state.Phase)

	instanceRootfs := filepath.Join(directory, instanceRootfsName)
	content, err := os.ReadFile(instanceRootfs)
	require.NoError(t, err)
	require.Equal(t, "rootfs-image-data", string(content))
}

func TestEnsureSandboxRestoresGoldenSnapshot(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)
	require.Equal(t, string(PhaseRunning), metadata.Phase)

	// Restore sequence: machine-config -> drive -> nic -> snapshot/load ->
	// InstanceStart; the kernel boot source is never configured.
	calls := fixture.server.recordedCalls()
	require.Equal(t, []string{
		"PUT /machine-config",
		"PUT /drives/root",
		"PUT /network-interfaces/eth0",
		"PUT /snapshot/load",
		"PUT /actions",
		"GET /",
	}, calls)

	// The snapshot/load payload references the cached golden artifacts and
	// leaves the VM paused for the explicit InstanceStart resume.
	loads := fixture.server.recordedSnapshotLoads()
	require.Len(t, loads, 1)
	imageDir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(fixture.sandboxSpec.Image))
	require.Equal(t, filepath.Join(imageDir, vmstateSnapshotName), loads[0].SnapshotPath)
	require.Equal(t, "File", loads[0].MemBackend.BackendType)
	require.Equal(t, filepath.Join(imageDir, memorySnapshotName), loads[0].MemBackend.BackendPath)
	require.False(t, loads[0].ResumeVM)
}

func TestEnsureSandboxRestoreMachineConfigFromManifest(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	// The fixture manifest records {vcpu: 2, memory: 1Gi}; the fixture spec
	// requests the same so restore succeeds with the manifest machine.
	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)

	// A request memory below the snapshot memory fails restore explicitly.
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), fixture.sandboxSpec.SandboxID))
	small := fixture.sandboxSpec
	small.Memory = "256Mi"
	_, err = fixture.driver.EnsureSandbox(context.Background(), &small)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "below the template snapshot memory")
}

func TestEnsureSandboxMissingSnapshotFilesReleasesSlot(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	// Only the rootfs exists; the golden snapshot set is incomplete.
	dir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(fixture.sandboxSpec.Image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs-image-data"), 0o640))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.ErrorIs(t, err, ErrImageNotReady)
	require.Empty(t, fixture.launcher.started)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
}

func TestEnsureSandboxIsIdempotent(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	first, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)
	second, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)
	require.Equal(t, first.ContainerID, second.ContainerID)
	require.Len(t, fixture.launcher.started, 1)
}

func TestEnsureSandboxValidatesIdentity(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	spec := fixture.sandboxSpec
	spec.AssignmentAttempt = 0
	_, err := fixture.driver.EnsureSandbox(context.Background(), &spec)
	require.ErrorIs(t, err, ErrInvalidConfig)

	_, err = fixture.driver.EnsureSandbox(context.Background(), nil)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestEnsureSandboxMissingImageReleasesSlot(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.ErrorIs(t, err, ErrImageNotReady)
	require.Empty(t, fixture.launcher.started)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
}

func TestEnsureSandboxCleansStaleRecord(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	// Plant a stale record whose process is dead.
	directory, err := ensureSandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{
		Spec: fixture.sandboxSpec, Phase: PhaseRunning, PID: 999, APIAddress: filepath.Join(directory, "dead.sock"),
	}))
	fixture.driver.probeProcess = func(pid int) error {
		if pid == 999 {
			return os.ErrNotExist
		}
		return nil
	}

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)
	require.Equal(t, "sandbox-1", metadata.ContainerID)
	require.Len(t, fixture.launcher.started, 1)
	require.Contains(t, fixture.killCalls, 999)
}

func TestInspectSandbox(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)

	metadata, err := fixture.driver.InspectSandbox(context.Background(), "sandbox-1")
	require.NoError(t, err)
	require.Equal(t, string(PhaseRunning), metadata.Phase)

	_, err = fixture.driver.InspectSandbox(context.Background(), "sandbox-missing")
	require.ErrorIs(t, err, ErrSandboxNotFound)
}

func TestDeleteSandboxIsIdempotent(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-1"))
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-1"))
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-missing"))

	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	_, err = os.Stat(directory)
	require.True(t, os.IsNotExist(err))
}

func TestListManagedSandboxesFiltersNamespace(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	fixture.driver.SetNamespace("tenant-a")
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)

	managed, err := fixture.driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	require.Len(t, managed, 1)
	require.Equal(t, "sandbox-1", managed[0].SandboxID)

	fixture.driver.SetNamespace("tenant-b")
	managed, err = fixture.driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	require.Empty(t, managed)
}

func TestRecoverRuntimeResourcesCleansDeadVM(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.driver.probeProcess = func(int) error { return os.ErrNotExist }
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	directory, err := ensureSandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{
		Spec: fixture.sandboxSpec, Phase: PhaseRunning, PID: 777, APIAddress: filepath.Join(directory, "dead.sock"),
	}))

	require.NoError(t, fixture.driver.RecoverRuntimeResources(context.Background(), nil))
	_, err = os.Stat(directory)
	require.True(t, os.IsNotExist(err))
	require.Contains(t, fixture.killCalls, 777)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
}

func TestRecoverRuntimeResourcesKeepsAliveVM(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)

	require.NoError(t, fixture.driver.RecoverRuntimeResources(context.Background(), nil))
	managed, err := fixture.driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	require.Len(t, managed, 1)
}

func TestGetAccessDescriptor(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)

	access, err := fixture.driver.GetAccessDescriptor("sandbox-1")
	require.NoError(t, err)
	require.Equal(t, "172.30.0.2", access.Address)
	require.NoError(t, access.Validate())

	_, err = fixture.driver.GetAccessDescriptor("sandbox-missing")
	require.ErrorIs(t, err, ErrSandboxNotFound)
}

func TestCloseKillsManagedProcesses(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), &fixture.sandboxSpec)
	require.NoError(t, err)

	fixture.driver.mu.Lock()
	process := fixture.driver.processes["sandbox-1"]
	fixture.driver.mu.Unlock()
	require.NotNil(t, process)

	require.NoError(t, fixture.driver.Close())
	fixture.driver.mu.Lock()
	require.Empty(t, fixture.driver.processes)
	fixture.driver.mu.Unlock()
	require.True(t, process.(*fakeProcess).killed)
}

func TestResolveMachineConfig(t *testing.T) {
	config := firecrackerConfigForTest(t, t.TempDir())

	request, err := resolveMachineConfig(fastletapi.SandboxSpec{}, config)
	require.NoError(t, err)
	require.Equal(t, int(config.DefaultVCPUs), request.VCPUs)
	require.Equal(t, 512, request.MemSizeMiB)

	request, err = resolveMachineConfig(fastletapi.SandboxSpec{CPU: "500m", Memory: "1Gi"}, config)
	require.NoError(t, err)
	require.Equal(t, 1, request.VCPUs)
	require.Equal(t, 1024, request.MemSizeMiB)

	request, err = resolveMachineConfig(fastletapi.SandboxSpec{CPU: "2.5", Memory: "256Mi"}, config)
	require.NoError(t, err)
	require.Equal(t, 3, request.VCPUs)
	require.Equal(t, 256, request.MemSizeMiB)

	_, err = resolveMachineConfig(fastletapi.SandboxSpec{CPU: "not-a-quantity"}, config)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestResolveRestoreMachineConfigUsesManifest(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	manifest, err := json.Marshal(map[string]any{
		"machine": map[string]any{"vcpu": "1", "memory": "512Mi"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o640))
	config := firecrackerConfigForTest(t, stateRoot)

	// The manifest machine tuple is authoritative for the restore machine
	// config; the request profile is not mapped into it.
	request, err := resolveRestoreMachineConfig(fastletapi.SandboxSpec{CPU: "4", Memory: "1Gi"}, config, stateRoot, image)
	require.NoError(t, err)
	require.Equal(t, 1, request.VCPUs)
	require.Equal(t, 512, request.MemSizeMiB)

	// A request memory below the snapshot memory is rejected.
	_, err = resolveRestoreMachineConfig(fastletapi.SandboxSpec{Memory: "256Mi"}, config, stateRoot, image)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "below the template snapshot memory")
}

func TestResolveRestoreMachineConfigFallsBackWithoutManifest(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	config := firecrackerConfigForTest(t, stateRoot)

	request, err := resolveRestoreMachineConfig(fastletapi.SandboxSpec{CPU: "2", Memory: "1Gi"}, config, stateRoot, image)
	require.NoError(t, err)
	require.Equal(t, 2, request.VCPUs)
	require.Equal(t, 1024, request.MemSizeMiB)
}

func TestResolveRestoreSnapshotFilesRequiresBothArtifacts(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmstateSnapshotName), []byte("vmstate"), 0o640))

	_, _, err := resolveRestoreSnapshotFiles(stateRoot, image)
	require.ErrorIs(t, err, ErrImageNotReady)

	require.NoError(t, os.WriteFile(filepath.Join(dir, memorySnapshotName), []byte("memory"), 0o640))
	vmstate, memory, err := resolveRestoreSnapshotFiles(stateRoot, image)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, vmstateSnapshotName), vmstate)
	require.Equal(t, filepath.Join(dir, memorySnapshotName), memory)
}

func TestGuestBootArgs(t *testing.T) {
	config := firecrackerConfigForTest(t, t.TempDir())
	slot := &fastletnetwork.Slot{IP: "172.30.0.2", Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24"}
	args, err := guestBootArgs(config, slot)
	require.NoError(t, err)
	require.Equal(t, "console=ttyS0 reboot=k panic=1 pci=off ip=172.30.0.3::172.30.0.1:255.255.255.0::eth0:off", args)

	config.BootArgs = "console=ttyS0 console=tty1"
	args, err = guestBootArgs(config, slot)
	require.NoError(t, err)
	require.Equal(t, "console=ttyS0 console=tty1 ip=172.30.0.3::172.30.0.1:255.255.255.0::eth0:off", args)
}

func TestCloseResetsDriver(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
	require.NoError(t, fixture.driver.Close())
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
}
