package firecracker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	infracontract "fast-sandbox/internal/infra/contract"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

// bootPollInterval is the VM state polling interval after InstanceStart.
const bootPollInterval = 250 * time.Millisecond

// defaultBootArgs is the minimal Firecracker guest kernel command line. The
// guest network ip= argument is appended per Sandbox by buildBootArgs.
const defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"

// Driver boots one Firecracker microVM on demand per Sandbox create request.
// The VM runs in the Fastlet Pod; nothing is pre-warmed.
type Driver struct {
	mu             sync.RWMutex
	profile        runtimecatalog.RuntimeProfile
	config         runtimecatalog.FirecrackerConfig
	namespace      string
	podUID         string
	initialized    bool
	runner         fastletnetwork.CommandRunner
	launcher       ProcessRunner
	newClient      func(socketPath string) *Client
	stat           func(string) (os.FileInfo, error)
	killProcess    func(pid int) error
	probeProcess   func(pid int) error
	waitSocket     func(ctx context.Context, socketPath string, timeout time.Duration) error
	networkManager *fastletnetwork.Manager
	infraMgr       *fastletinfra.Manager
	prepareInfra   func(ctx context.Context, spec *fastletapi.SandboxSpec) (fastletinfra.PreparedInstance, error)
	processes      map[string]Process
	// agentSocket, newAgentClient, and agentClient wire the node-level
	// runtime-agent (agent_wiring.go): an empty socket means local mode, a
	// nil newAgentClient disables the agent client, and agentClient is the
	// lazily built cached client.
	agentSocket    string
	newAgentClient func(socketPath string) (AgentClient, error)
	agentClient    AgentClient
	// sandboxLeases records the runtime-agent lease of each Sandbox that
	// requested one (populated by LeaseDevices; empty in the native stage).
	sandboxLeases map[string]string
	// imageGCInterval is the period of the independent cache GC loop; it is a
	// field (not a constant) so tests can shorten it.
	imageGCInterval time.Duration
	// imageCacheLimitBytes is the cache size the GC enforces by evicting
	// unreferenced images in least-frequently-used order.
	imageCacheLimitBytes int64
	gcStop               chan struct{}
	gcTrigger            chan struct{}
	// imageUseCount records how often each cached image was pulled or booted,
	// in memory: the GC evicts by this instead of filesystem timestamps.
	// Guarded by mu.
	imageUseCount map[string]int64
}

// defaultImageGCInterval bounds the image cache by usage without coupling GC
// to Sandbox lifecycle events.
const defaultImageGCInterval = time.Hour

// New validates the runtime profile configuration and returns the driver.
func New(profile runtimecatalog.RuntimeProfile) (*Driver, error) {
	if profile.Firecracker == nil {
		return nil, fmt.Errorf("firecracker runtime profile %q has no private configuration", profile.Name)
	}
	return &Driver{
		profile: profile, config: *profile.Firecracker,
		runner: fastletnetwork.ExecRunner{}, launcher: ExecProcessRunner{},
		newClient: NewClient, stat: os.Stat, killProcess: killPID, probeProcess: pidAlive,
		waitSocket: waitForAPISocket, processes: make(map[string]Process),
		imageGCInterval:      defaultImageGCInterval,
		imageCacheLimitBytes: defaultImageCacheLimitBytes,
	}, nil
}

// Initialize validates the boot configuration, prepares the StateRoot, and
// starts the independent image cache GC loop.
func (d *Driver) Initialize(_ context.Context, _ string) error {
	d.mu.Lock()
	if d.initialized {
		d.mu.Unlock()
		return nil
	}
	if err := validateConfig(d.config); err != nil {
		d.mu.Unlock()
		return err
	}
	if err := os.MkdirAll(d.config.StateRoot, 0o750); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("prepare Firecracker StateRoot: %w", err)
	}
	d.initialized = true
	interval := d.imageGCInterval
	d.gcStop = make(chan struct{})
	d.gcTrigger = make(chan struct{}, 1)
	stop := d.gcStop
	// Cache entries already present at startup start with one recorded use so
	// the LFU eviction does not single them out before any pull or boot.
	d.imageUseCount = make(map[string]int64)
	if cached, err := listCachedImages(d.config.StateRoot); err == nil {
		for _, digest := range cached {
			d.imageUseCount[digest] = 1
		}
	}
	d.mu.Unlock()
	go d.imageGCLoop(interval, stop)
	return nil
}

// touchImage records a use of a cached image for the LFU eviction order. It
// is called when an image is pulled or booted.
func (d *Driver) touchImage(image string) {
	d.mu.Lock()
	if d.imageUseCount != nil {
		d.imageUseCount[imageKey(image)]++
	}
	d.mu.Unlock()
}

// TriggerImageGC requests an out-of-band collection from the independent GC
// loop. It is non-blocking; a collection already in flight coalesces the
// request. The loop remains independent of Sandbox lifecycle events.
func (d *Driver) TriggerImageGC() {
	d.mu.RLock()
	trigger := d.gcTrigger
	d.mu.RUnlock()
	if trigger == nil {
		return
	}
	select {
	case trigger <- struct{}{}:
	default:
	}
}

// imageGCLoop periodically collects unreferenced cached images. It runs
// independently of Sandbox lifecycle events and stops when Close closes the
// stop channel.
func (d *Driver) imageGCLoop(interval time.Duration, stop <-chan struct{}) {
	d.gcImageCache()
	for {
		d.mu.RLock()
		trigger := d.gcTrigger
		d.mu.RUnlock()
		select {
		case <-stop:
			return
		case <-trigger:
			d.gcImageCache()
		case <-time.After(interval):
			d.gcImageCache()
		}
	}
}

// gcImageCache drops cached rootfs images that no managed Sandbox references
// and that have been idle beyond the grace period. Failures are logged and
// never fail the surrounding operation.
func (d *Driver) gcImageCache() {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	// Snapshot the use counts under the lock: garbageCollectImages runs
	// outside it while PullImage and boot (touchImage) keep writing the
	// map, so the iteration must not touch the live map.
	useCount := make(map[string]int64, len(d.imageUseCount))
	for digest, uses := range d.imageUseCount {
		useCount[digest] = uses
	}
	limitBytes := d.imageCacheLimitBytes
	d.mu.RUnlock()
	if limitBytes <= 0 {
		limitBytes = defaultImageCacheLimitBytes
	}
	removed, err := garbageCollectImages(stateRoot, limitBytes, useCount)
	if err != nil {
		klog.V(2).InfoS("firecracker image cache GC skipped", "err", err)
		return
	}
	if len(removed) > 0 {
		klog.InfoS("firecracker image cache GC removed unreferenced images", "digests", removed)
	}
}

func validateConfig(config runtimecatalog.FirecrackerConfig) error {
	if config.BinaryPath == "" || config.KernelPath == "" || config.RootfsPath == "" || config.StateRoot == "" {
		return fmt.Errorf("%w: firecracker binary, kernel, rootfs, and state root are required", ErrInvalidConfig)
	}
	if config.DefaultVCPUs < 1 || config.DefaultMemory == "" || config.BootTimeoutSeconds < 1 {
		return fmt.Errorf("%w: firecracker boot profile requires vCPUs, memory, and boot timeout", ErrInvalidConfig)
	}
	return nil
}

// SetNamespace records the Fastlet namespace that owns the managed Sandboxes.
func (d *Driver) SetNamespace(namespace string) {
	d.mu.Lock()
	d.namespace = namespace
	d.mu.Unlock()
}

// SetNetworkManager wires the Fastlet-owned slot manager. Each slot carries
// the pod-side IP and the guest tap prepared by GuestVMNetNSDriver.
func (d *Driver) SetNetworkManager(manager *fastletnetwork.Manager) {
	d.mu.Lock()
	d.networkManager = manager
	d.mu.Unlock()
}

// SetInfraManager wires the prepared Infra Component plan. Artifacts are
// copied into the per-instance guest rootfs before boot (GuestCopy delivery).
func (d *Driver) SetInfraManager(manager *fastletinfra.Manager) {
	d.mu.Lock()
	d.infraMgr = manager
	d.prepareInfra = manager.PrepareInstance
	d.mu.Unlock()
}

// ProbeCapabilities reports host dependencies (KVM, tap device, binary,
// kernel) and the runtime-agent health when one is configured. The profile
// gate keeps the runtime fail-closed until the KVM E2E suite passes.
func (d *Driver) ProbeCapabilities(ctx context.Context) CapabilityReport {
	d.mu.RLock()
	profile := d.profile
	config := d.config
	stat := d.stat
	d.mu.RUnlock()

	report := CapabilityReport{Runtime: profile.Name, ProfileHash: profile.ProfileHash, State: runtimecatalog.CapabilityReady}
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityUnsupported {
		report.State = runtimecatalog.CapabilityUnsupported
		report.Reason = profile.Capabilities.Reason
		report.Message = "firecracker runtime profile is registered but its production capability gate is not enabled"
		return report
	}
	checks := []struct {
		path   string
		reason string
	}{
		{"/dev/kvm", "KVMUnavailable"},
		{"/dev/net/tun", "TapDeviceUnavailable"},
		{config.BinaryPath, "RuntimeBinaryUnavailable"},
		{config.KernelPath, "RuntimeKernelUnavailable"},
	}
	for _, check := range checks {
		if _, err := stat(check.path); err != nil {
			report.Missing = append(report.Missing, check.path)
			if report.Reason == "" {
				report.Reason = check.reason
			}
		}
	}
	if len(report.Missing) > 0 {
		report.State = runtimecatalog.CapabilityDegraded
		report.Message = fmt.Sprintf("firecracker runtime dependencies are unavailable: %v", report.Missing)
		return report
	}
	agent, err := d.agentClientOrNil()
	if err != nil {
		report.State = runtimecatalog.CapabilityDegraded
		report.Reason = "AgentUnavailable"
		report.Message = fmt.Sprintf("firecracker runtime-agent client error: %v", err)
		return report
	}
	if agent != nil {
		healthCtx, cancel := context.WithTimeout(ctx, agentHealthTimeout)
		defer cancel()
		if err := agent.Health(healthCtx); err != nil {
			report.State = runtimecatalog.CapabilityDegraded
			report.Reason = "AgentUnavailable"
			report.Message = err.Error()
			return report
		}
	}
	report.Reason = "RuntimeDriverReady"
	report.Message = "firecracker runtime host dependencies are ready"
	return report
}

// EnsureSandbox boots one Firecracker microVM on demand. The call is
// idempotent and emits an OTel span tree correlated by the Sandbox identity.
func (d *Driver) EnsureSandbox(ctx context.Context, config *fastletapi.SandboxSpec) (_ *SandboxMetadata, resultErr error) {
	if config == nil || config.SandboxID == "" || config.FastletPodUID == "" ||
		config.InstanceGeneration <= 0 || config.RuntimeInstanceID == "" || config.AssignmentAttempt <= 0 {
		return nil, fmt.Errorf("%w: complete Firecracker Sandbox identity is required", ErrInvalidConfig)
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{
		RequestID: config.RequestID, Namespace: config.ClaimNamespace, SandboxName: config.ClaimName,
		SandboxUID: config.SandboxID, FastletPodUID: config.FastletPodUID,
		InstanceGeneration: config.InstanceGeneration, AssignmentAttempt: config.AssignmentAttempt,
	})
	ctx, createSpan := observability.Start(ctx, "fastlet.firecracker.create")
	infraPrepared := false
	defer func() {
		observability.End(createSpan, resultErr)
		if resultErr != nil && infraPrepared && d.infraMgr != nil {
			_ = d.infraMgr.RemoveInstance(config)
		}
	}()

	d.mu.RLock()
	stateRoot := d.config.StateRoot
	manager := d.networkManager
	d.mu.RUnlock()
	if manager == nil {
		return nil, fmt.Errorf("%w: firecracker requires the Fastlet network manager", ErrNetworkUnavailable)
	}

	directory, err := ensureSandboxDir(stateRoot, config.SandboxID)
	if err != nil {
		return nil, err
	}

	if existing, err := loadState(directory); err == nil {
		if alive, probeErr := d.probeVM(ctx, existing); probeErr == nil && alive {
			if err := validateExistingRuntimeProfile(existingMetadata(existing), config); err != nil {
				return nil, err
			}
			return existingMetadata(existing), nil
		}
		if err := d.cleanupStale(ctx, directory, existing); err != nil {
			return nil, err
		}
		// The stale state directory was removed; recreate it for the fresh boot.
		directory, err = ensureSandboxDir(stateRoot, config.SandboxID)
		if err != nil {
			return nil, err
		}
	}

	owner := d.networkOwner(config)
	createStarted := time.Now()
	acquireCtx, acquireSpan := observability.Start(ctx, "fastlet.firecracker.acquire")
	slot, err := manager.Acquire(acquireCtx, owner)
	observability.End(acquireSpan, err)
	if err != nil {
		return nil, fmt.Errorf("%w: acquire Firecracker network slot: %v", ErrNetworkUnavailable, err)
	}
	acquireDur := time.Since(createStarted)
	releaseSlot := func() {
		_ = manager.Release(context.Background(), owner)
	}

	rootfsStarted := time.Now()
	_, rootfsSpan := observability.Start(ctx, "fastlet.firecracker.rootfs")
	vmstatePath, memoryPath, err := resolveRestoreSnapshotFiles(stateRoot, config.Image)
	// The machine tuple of the golden snapshot is baked in the vmstate
	// (v1.16 restores it from the snapshot); the manifest values are only
	// validated here, not applied via the API (any machine-config call
	// before snapshot/load is rejected).
	if err == nil {
		_, err = resolveRestoreMachineConfig(*config, d.config, stateRoot, config.Image)
	}
	var instanceRootfs string
	if err == nil {
		// The instance copy is placed at <stateDir>/rootfs.img, the same
		// relative path baked in the vmstate, so the Firecracker process
		// (started with cwd=<stateDir>) resolves it to this instance's own
		// reflink copy instead of the shared cache base.
		instanceRootfs, err = prepareInstanceRootfs(stateRoot, config.Image, directory)
	}
	observability.End(rootfsSpan, err)
	if err != nil {
		releaseSlot()
		return nil, err
	}
	d.touchImage(config.Image)
	rootfsDur := time.Since(rootfsStarted)
	rootfsMiB := float64(0)
	if info, statErr := os.Stat(instanceRootfs); statErr == nil {
		rootfsMiB = float64(info.Size()) / (1024 * 1024)
	}

	// Infra Components: prepare the instance plan and GuestCopy the artifacts
	// into the per-instance rootfs before boot.
	var infraServices []infracontract.ServiceEndpoint
	var infraDiagnostics []infracontract.ComponentDiagnostic
	infraDur := time.Duration(0)
	if d.infraMgr != nil {
		infraStarted := time.Now()
		infraCtx, infraSpan := observability.Start(ctx, "fastlet.firecracker.infra")
		instance, prepareErr := d.prepareInfra(infraCtx, config)
		if prepareErr == nil {
			prepareErr = deliverGuestInfra(infraCtx, d.runner, instanceRootfs, instance)
		}
		observability.End(infraSpan, prepareErr)
		if prepareErr != nil {
			_ = d.infraMgr.RemoveInstance(config)
			releaseSlot()
			return nil, fmt.Errorf("%w: prepare Infra Components: %v", ErrInfraUnavailable, prepareErr)
		}
		infraServices = instance.Services
		infraDiagnostics = instance.Diagnostics
		infraPrepared = true
		infraDur = time.Since(infraStarted)
		klog.V(4).InfoS("firecracker Infra Components delivered", "sandboxId", config.SandboxID, "services", len(instance.Services), "duration", infraDur.String())
	}

	// Restore-only startup: the golden snapshot carries the guest network
	// configuration (the preparation VM's static IP is baked into the guest
	// state), and the NIC host tap is replaced per instance via the load
	// request's network_overrides. Nothing else is injected into the rootfs.
	state := &SandboxState{
		Spec: *config, Phase: PhaseStarting,
		APIAddress: filepath.Join(directory, "api.sock"), CreatedAt: time.Now().Unix(),
		InfraServices: infraServices, InfraDiagnostics: infraDiagnostics,
	}
	if err := saveState(directory, state); err != nil {
		releaseSlot()
		return nil, err
	}

	launchCtx, launchSpan := observability.Start(ctx, "fastlet.firecracker.launch")
	launchStarted := time.Now()
	spawnStarted := time.Now()
	process, err := d.launchVM(launchCtx, config.SandboxID, state.APIAddress, directory)
	spawnDur := time.Since(spawnStarted)
	if err == nil {
		state.PID = process.PID()
		d.rememberProcess(config.SandboxID, process)
		if saveErr := saveState(directory, state); saveErr != nil {
			d.killAndForget(config.SandboxID, process.PID())
			releaseSlot()
			observability.End(launchSpan, saveErr)
			return nil, saveErr
		}
		socketStarted := time.Now()
		err = d.waitSocket(launchCtx, state.APIAddress, firecrackerSocketWaitTimeout)
		socketWaitDur := time.Since(socketStarted)
		if err != nil {
			detail := readProcessLog(directory)
			d.killAndForget(config.SandboxID, process.PID())
			releaseSlot()
			observability.End(launchSpan, err)
			return nil, fmt.Errorf("%w: firecracker API socket did not appear: %v%s", ErrRuntimeNotInitialized, err, detail)
		}
		klog.V(4).InfoS("firecracker API socket ready", "sandboxId", config.SandboxID, "spawn", spawnDur.String(), "socketWait", socketWaitDur.String())
	}
	observability.End(launchSpan, err)
	if err != nil {
		releaseSlot()
		return nil, err
	}
	launchDur := time.Since(launchStarted)

	client := d.newClient(state.APIAddress)
	defer client.Close()
	configureStarted := time.Now()
	configureCtx, configureSpan := observability.Start(ctx, "fastlet.firecracker.configure")
	err = configureRestoreVM(configureCtx, client, slot, vmstatePath, memoryPath)
	observability.End(configureSpan, err)
	if err != nil {
		d.killAndForget(config.SandboxID, process.PID())
		releaseSlot()
		return nil, err
	}
	configureDur := time.Since(configureStarted)

	bootStarted := time.Now()
	bootCtx, bootSpan := observability.Start(ctx, "fastlet.firecracker.boot")
	polls, err := bootVM(bootCtx, client, d.config.BootTimeoutSeconds)
	observability.End(bootSpan, err)
	if err != nil {
		d.killAndForget(config.SandboxID, process.PID())
		releaseSlot()
		return nil, err
	}
	bootDur := time.Since(bootStarted)

	state.Phase = PhaseRunning
	if err := saveState(directory, state); err != nil {
		d.killAndForget(config.SandboxID, process.PID())
		releaseSlot()
		return nil, err
	}
	klog.InfoS("firecracker sandbox created",
		"sandboxId", config.SandboxID,
		"total", time.Since(createStarted).String(),
		"acquire", acquireDur.String(),
		"rootfs", rootfsDur.String(), "rootfsMiB", fmt.Sprintf("%.1f", rootfsMiB),
		"infra", infraDur.String(),
		"launch", launchDur.String(),
		"configure", configureDur.String(),
		"boot", bootDur.String(), "vmStatePolls", polls,
	)
	return existingMetadata(state), nil
}

// InspectSandbox returns the metadata of a managed microVM. An unreachable
// VM is reported with Phase Stopped instead of an error so Fastlet can
// reconcile the real state.
func (d *Driver) InspectSandbox(ctx context.Context, sandboxID string) (_ *SandboxMetadata, resultErr error) {
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxUID: sandboxID})
	ctx, span := observability.Start(ctx, "fastlet.firecracker.inspect")
	defer func() { observability.End(span, resultErr) }()
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	directory, err := sandboxDir(stateRoot, sandboxID)
	if err != nil {
		return nil, err
	}
	state, err := loadState(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrSandboxNotFound
		}
		return nil, err
	}
	if alive, probeErr := d.probeVM(ctx, state); probeErr == nil && !alive {
		state.Phase = PhaseStopped
	}
	return existingMetadata(state), nil
}

// DeleteSandbox stops the microVM, releases its network slot, and removes the
// Sandbox state. The call is idempotent.
func (d *Driver) DeleteSandbox(ctx context.Context, sandboxID string) (resultErr error) {
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxUID: sandboxID})
	ctx, span := observability.Start(ctx, "fastlet.firecracker.delete")
	defer func() { observability.End(span, resultErr) }()
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	manager := d.networkManager
	d.mu.RUnlock()
	directory, err := sandboxDir(stateRoot, sandboxID)
	if err != nil {
		return err
	}
	state, err := loadState(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	d.killAndForget(sandboxID, state.PID)
	if manager != nil && state.Spec.SandboxID != "" {
		if _, exists := manager.Lookup(sandboxID); exists {
			_ = manager.Release(ctx, d.networkOwner(&state.Spec))
		}
	}
	d.mu.RLock()
	infraMgr := d.infraMgr
	d.mu.RUnlock()
	if infraMgr != nil {
		_ = infraMgr.RemoveSandboxInstances(sandboxID)
	}
	d.releaseAgentSandbox(ctx, sandboxID, state.Spec.Image)
	_ = removeSandboxDir(directory)
	klog.Infof("firecracker sandbox %s deleted", sandboxID)
	return nil
}

// ListManagedSandboxes returns the Sandboxes managed by this Fastlet in the
// configured namespace.
func (d *Driver) ListManagedSandboxes(_ context.Context) ([]*SandboxMetadata, error) {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	namespace := d.namespace
	d.mu.RUnlock()
	directories, err := listSandboxDirs(stateRoot)
	if err != nil {
		return nil, err
	}
	managed := make([]*SandboxMetadata, 0, len(directories))
	for _, directory := range directories {
		state, err := loadState(directory)
		if err != nil || (namespace != "" && state.Spec.ClaimNamespace != namespace) {
			continue
		}
		managed = append(managed, existingMetadata(state))
	}
	return managed, nil
}

// RecoverRuntimeResources cleans up VMs that died with a Fastlet restart:
// Firecracker processes are pod-local, so stale records are removed and
// their slots released; surviving VMs are returned.
func (d *Driver) RecoverRuntimeResources(ctx context.Context, managed []*SandboxMetadata) error {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	manager := d.networkManager
	d.mu.RUnlock()
	directories, err := listSandboxDirs(stateRoot)
	if err != nil {
		return err
	}
	for _, directory := range directories {
		state, err := loadState(directory)
		if err != nil {
			continue
		}
		alive, probeErr := d.probeVM(ctx, state)
		if probeErr != nil || !alive {
			if manager != nil {
				if slot, exists := manager.Lookup(state.Spec.SandboxID); exists {
					_ = slot
					_ = manager.Release(ctx, d.networkOwner(&state.Spec))
				}
			}
			d.killAndForget(state.Spec.SandboxID, state.PID)
			_ = removeSandboxDir(directory)
		}
	}
	return nil
}

// GetAccessDescriptor returns the pod-side DirectIP descriptor of the Sandbox.
func (d *Driver) GetAccessDescriptor(sandboxID string) (dataplane.AccessDescriptor, error) {
	d.mu.RLock()
	manager := d.networkManager
	d.mu.RUnlock()
	if manager == nil {
		return dataplane.AccessDescriptor{}, ErrNetworkUnavailable
	}
	slot, exists := manager.Lookup(sandboxID)
	if !exists {
		return dataplane.AccessDescriptor{}, ErrSandboxNotFound
	}
	if err := slot.Access.Validate(); err != nil {
		return dataplane.AccessDescriptor{}, fmt.Errorf("%w: %v", ErrNetworkUnavailable, err)
	}
	return slot.Access, nil
}

// Close stops every managed microVM and releases driver state.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for sandboxID, process := range d.processes {
		_ = process.Kill()
		delete(d.processes, sandboxID)
	}
	if d.gcStop != nil {
		close(d.gcStop)
		d.gcStop = nil
	}
	d.agentClient = nil
	d.sandboxLeases = nil
	d.initialized = false
	return nil
}

// probeVM reports whether the Firecracker process and its API socket are
// alive; the PID check rejects stale records with recycled identifiers.
func (d *Driver) probeVM(ctx context.Context, state *SandboxState) (bool, error) {
	if state == nil || state.APIAddress == "" {
		return false, ErrInvalidConfig
	}
	d.mu.RLock()
	probeProcess := d.probeProcess
	d.mu.RUnlock()
	if state.PID > 0 {
		if err := probeProcess(state.PID); err != nil {
			return false, nil
		}
	}
	client := d.newClient(state.APIAddress)
	defer client.Close()
	if _, err := client.Version(ctx); err != nil {
		return false, nil
	}
	return true, nil
}

// firecrackerSocketWaitTimeout bounds the time between process launch and API
// socket readiness.
const firecrackerSocketWaitTimeout = 5 * time.Second

// launchVM starts the Firecracker process for the Sandbox.
func (d *Driver) launchVM(ctx context.Context, sandboxID, apiAddress, stateDir string) (Process, error) {
	d.mu.RLock()
	config := d.config
	launcher := d.launcher
	d.mu.RUnlock()
	return launch(ctx, launcher, launchConfig{
		BinaryPath: config.BinaryPath, SandboxID: sandboxID, APIAddress: apiAddress,
		// The vmstate bakes the root drive as the relative path "rootfs.img";
		// the process cwd selects this instance's own reflink copy.
		WorkingDir: stateDir,
		LogPath:    filepath.Join(stateDir, processLogName),
	})
}

// waitForAPISocket polls until the Firecracker API Unix socket exists.
func waitForAPISocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s not ready within %s", socketPath, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// readProcessLog returns the tail of the firecracker process log, or an empty
// string when no log is available.
func readProcessLog(stateDir string) string {
	payload, err := os.ReadFile(filepath.Join(stateDir, processLogName))
	if err != nil {
		return ""
	}
	tail := payload
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	return "\nfirecracker process log:\n" + string(tail)
}

// guestBootArgs composes the guest kernel command line with the static
// network configuration derived from the network slot. The driver no longer
// boots a kernel (restore is the only startup path), so this is used only
// by the golden snapshot preparation path (E2E self-bootstrap).
func guestBootArgs(config runtimecatalog.FirecrackerConfig, slot *fastletnetwork.Slot) (string, error) {
	base := config.BootArgs
	if base == "" {
		base = defaultBootArgs
	}
	guestIP, err := fastletnetwork.GuestVMIP(slot)
	if err != nil {
		return "", err
	}
	prefix, err := netip.ParsePrefix(slot.PrivateCIDR)
	if err != nil {
		return "", fmt.Errorf("%w: invalid private CIDR %q", ErrInvalidConfig, slot.PrivateCIDR)
	}
	// The kernel ip= parameter expects a dotted-quad netmask, not a prefix
	// length; a bare "24" is parsed as 24.0.0.0 and rejected with EINVAL.
	mask, err := prefixMask(prefix.Bits())
	if err != nil {
		return "", err
	}
	return buildBootArgs(base, guestIP, slot.Gateway, mask), nil
}

// bootVM starts the microVM and waits until the machine state is Running.
// bootVM starts the microVM, waits until the machine state is Running, and
// returns the number of VM state polls performed.
func bootVM(ctx context.Context, client *Client, timeoutSeconds int32) (int, error) {
	if err := client.Start(ctx); err != nil {
		return 0, fmt.Errorf("start Firecracker instance: %w", err)
	}
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	polls := 0
	for {
		state, err := client.VMState(ctx)
		if err != nil {
			return polls, fmt.Errorf("query Firecracker VM state: %w", err)
		}
		polls++
		if state == "Running" {
			return polls, nil
		}
		if time.Now().After(deadline) {
			return polls, fmt.Errorf("%w: Firecracker VM did not reach Running within %ds (state %q)", ErrRuntimeNotInitialized, timeoutSeconds, state)
		}
		select {
		case <-ctx.Done():
			return polls, ctx.Err()
		case <-time.After(bootPollInterval):
		}
	}
}

// resolveMachineConfig maps the Sandbox resource profile to Firecracker
// machine configuration, falling back to runtime defaults.
func resolveMachineConfig(spec fastletapi.SandboxSpec, config runtimecatalog.FirecrackerConfig) (MachineConfigRequest, error) {
	request := MachineConfigRequest{VCPUs: int(config.DefaultVCPUs), MemSizeMiB: defaultMemoryMiB(config.DefaultMemory)}
	if spec.CPU != "" {
		quantity, err := resource.ParseQuantity(spec.CPU)
		if err != nil {
			return MachineConfigRequest{}, fmt.Errorf("%w: invalid CPU %q", ErrInvalidConfig, spec.CPU)
		}
		millis := quantity.MilliValue()
		request.VCPUs = int(math.Ceil(float64(millis) / 1000.0))
		if request.VCPUs < 1 {
			return MachineConfigRequest{}, fmt.Errorf("%w: CPU %q yields no vCPU", ErrInvalidConfig, spec.CPU)
		}
	}
	if spec.Memory != "" {
		quantity, err := resource.ParseQuantity(spec.Memory)
		if err != nil {
			return MachineConfigRequest{}, fmt.Errorf("%w: invalid memory %q", ErrInvalidConfig, spec.Memory)
		}
		request.MemSizeMiB = int(math.Ceil(float64(quantity.Value()) / (1024.0 * 1024.0)))
		if request.MemSizeMiB < 1 {
			return MachineConfigRequest{}, fmt.Errorf("%w: memory %q yields no MiB", ErrInvalidConfig, spec.Memory)
		}
	}
	return request, nil
}

func defaultMemoryMiB(memory string) int {
	quantity, err := resource.ParseQuantity(memory)
	if err != nil {
		return 512
	}
	mib := int(math.Ceil(float64(quantity.Value()) / (1024.0 * 1024.0)))
	if mib < 1 {
		return 512
	}
	return mib
}

func (d *Driver) networkOwner(config *fastletapi.SandboxSpec) fastletnetwork.Owner {
	generation := config.InstanceGeneration
	if generation <= 0 {
		generation = 1
	}
	attempt := config.AssignmentAttempt
	if attempt <= 0 {
		attempt = 1
	}
	return fastletnetwork.Owner{
		SandboxUID: config.SandboxID, SandboxName: config.ClaimName, SandboxNamespace: config.ClaimNamespace,
		InstanceGeneration: generation, RuntimeInstanceID: config.RuntimeInstanceID,
		AssignmentAttempt: attempt, ResidualProcess: runtimecatalog.ResidualProcessFirecracker,
	}
}

func existingMetadata(state *SandboxState) *SandboxMetadata {
	metadata := &SandboxMetadata{SandboxSpec: state.Spec}
	metadata.ContainerID = state.Spec.SandboxID
	metadata.PID = state.PID
	metadata.Phase = string(state.Phase)
	metadata.CreatedAt = state.CreatedAt
	metadata.UserProcessStartSource = fastletapi.UserProcessStartRuntimeDirect
	metadata.InfraServices = append(metadata.InfraServices, state.InfraServices...)
	metadata.InfraDiagnostics = append(metadata.InfraDiagnostics, state.InfraDiagnostics...)
	return metadata
}

func (d *Driver) rememberProcess(sandboxID string, process Process) {
	d.mu.Lock()
	d.processes[sandboxID] = process
	d.mu.Unlock()
}

// killAndForget stops the tracked process of a Sandbox; when no process
// handle exists (Fastlet restart), it falls back to the persisted PID.
func (d *Driver) killAndForget(sandboxID string, pid int) {
	d.mu.Lock()
	process, exists := d.processes[sandboxID]
	delete(d.processes, sandboxID)
	d.mu.Unlock()
	if exists {
		if process.Kill() == nil {
			// Wait for the VMM to exit before the network slot is released;
			// deleting the slot netns races a still-dying firecracker and
			// leaks the namespace (and its private address) on the bridge.
			done := make(chan struct{})
			go func() { _ = process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
		return
	}
	if d.killProcess != nil {
		_ = d.killProcess(pid)
	}
}

// cleanupStale removes the state of a dead VM and its network binding.
func (d *Driver) cleanupStale(ctx context.Context, directory string, state *SandboxState) error {
	d.mu.RLock()
	manager := d.networkManager
	d.mu.RUnlock()
	if manager != nil && state.Spec.SandboxID != "" {
		if _, exists := manager.Lookup(state.Spec.SandboxID); exists {
			_ = manager.Release(ctx, d.networkOwner(&state.Spec))
		}
	}
	d.killAndForget(state.Spec.SandboxID, state.PID)
	return removeSandboxDir(directory)
}

func killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// pidAlive probes process existence with signal 0.
func pidAlive(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.Signal(0))
}

var _ runtimecontract.Driver = (*Driver)(nil)
