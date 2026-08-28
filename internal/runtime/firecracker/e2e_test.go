//go:build firecracker

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestFirecrackerDriverE2E boots a real Firecracker microVM through the
// driver on the local machine with a real Infra plan (GuestCopy delivery).
// It requires root (netns, tap, iptables), /dev/kvm, /dev/net/tun, a
// firecracker binary, a kernel image, and a converted rootfs.
// See scripts/firecracker-e2e.sh.
func TestFirecrackerDriverE2E(t *testing.T) {
	runE2EOnce(t, true)
}

// TestFirecrackerDriverE2ENoInfra exercises the same lifecycle without Infra
// delivery: no SetInfraManager, no infra.json, no components in the guest.
func TestFirecrackerDriverE2ENoInfra(t *testing.T) {
	runE2EOnce(t, false)
}

// TestFirecrackerDriverE2EConcurrent pre-provisions 5 network slots and boots
// 5 microVMs in parallel through the same driver and slot manager, verifying
// per-sandbox trace correlation, distinct processes, Infra delivery, and
// guest reachability for every instance.
func TestFirecrackerDriverE2EConcurrent(t *testing.T) {
	const vmCount = 5
	env := newE2EEnvironment(t, vmCount, true)
	defer env.teardown()

	snapshot := env.manager.Snapshot()
	require.Equal(t, vmCount, snapshot.Clean, "all slots must be pre-provisioned before boot")
	require.Zero(t, snapshot.Bound)

	type bootResult struct {
		metadata *SandboxMetadata
		err      error
		traceID  trace.TraceID
	}
	results := make([]bootResult, vmCount)
	var wg sync.WaitGroup
	started := time.Now()
	for index := 0; index < vmCount; index++ {
		spec := env.spec(index + 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, end := env.traceContext(spec.SandboxID)
			defer end()
			result := &results[index]
			result.traceID = trace.SpanContextFromContext(ctx).TraceID()
			result.metadata, result.err = env.driver.EnsureSandbox(ctx, spec)
		}()
	}
	wg.Wait()
	t.Logf("%d VMs booted concurrently in %s", vmCount, time.Since(started))

	snapshot = env.manager.Snapshot()
	require.Zero(t, snapshot.Clean, "all pre-provisioned slots must be bound")
	require.Equal(t, vmCount, snapshot.Bound)

	pids := make(map[int]string, vmCount)
	for index := 0; index < vmCount; index++ {
		spec := env.spec(index + 1)
		result := results[index]
		require.NoErrorf(t, result.err, "sandbox %s", spec.SandboxID)
		env.assertBooted(result.metadata, spec, result.traceID)
		if owner, exists := pids[result.metadata.PID]; exists {
			t.Fatalf("VMs must be distinct processes: pid %d used by both %s and %s", result.metadata.PID, owner, spec.SandboxID)
		}
		pids[result.metadata.PID] = spec.SandboxID
		env.assertInfra(result.metadata, spec.SandboxID)
		env.assertGuestReachable(spec.SandboxID)
	}
}

// TestFirecrackerDriverE2EImageGC verifies the independent LFU image cache
// GC on a real StateRoot: unreferenced low-frequency images are evicted when
// the cache is over its limit, a running Sandbox pins its image, and deleting
// the Sandbox releases it for eviction.
func TestFirecrackerDriverE2EImageGC(t *testing.T) {
	env := newE2EEnvironment(t, 1, false)
	defer env.teardown()

	waitGone := func(t *testing.T, path string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		require.Failf(t, "image was not collected: %s", path)
	}
	waitPresent := func(t *testing.T, path string) {
		t.Helper()
		time.Sleep(500 * time.Millisecond)
		_, err := os.Stat(path)
		require.NoError(t, err, "image must be pinned: %s", path)
	}
	seedImage := func(t *testing.T, image string) string {
		t.Helper()
		path, err := imageCachePath(env.stateRoot, image)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte("unused rootfs"), 0o640))
		return path
	}

	// The cache limit only fits the real e2e:ubuntu image: any unreferenced
	// extra image is over the limit and gets evicted by the independent loop.
	usedPath, err := imageCachePath(env.stateRoot, env.imageRef)
	require.NoError(t, err)
	info, err := os.Stat(usedPath)
	require.NoError(t, err)
	env.driver.imageCacheLimitBytes = info.Size() + 1
	unusedPath := seedImage(t, "e2e:unused")
	env.driver.TriggerImageGC()
	waitGone(t, unusedPath)

	// A live Sandbox pins its cached image even when the cache is over the
	// limit; booting also records a use for it.
	spec := env.spec(1)
	ctx, end := env.traceContext(spec.SandboxID)
	defer end()
	metadata := env.boot(ctx, spec)
	env.assertGuestReachable(spec.SandboxID)
	env.driver.TriggerImageGC()
	waitPresent(t, usedPath)

	// Deleting the Sandbox releases the reference; with the cache over its
	// limit the now-unreferenced image is evicted on the next collection.
	env.delete(spec.SandboxID)
	env.driver.imageCacheLimitBytes = 1
	env.driver.TriggerImageGC()
	waitGone(t, usedPath)
	require.Equal(t, string(PhaseRunning), metadata.Phase)
}

// runE2EOnce drives one full create/inspect/delete lifecycle.
func runE2EOnce(t *testing.T, useInfra bool) {
	env := newE2EEnvironment(t, 1, useInfra)
	defer env.teardown()
	t.Logf("infra=%t", useInfra)

	spec := env.spec(1)
	ctx, end := env.traceContext(spec.SandboxID)
	defer end()

	started := time.Now()
	metadata := env.boot(ctx, spec)
	t.Logf("VM running in %s (pid=%d)", time.Since(started), metadata.PID)

	if useInfra {
		env.assertInfra(metadata, spec.SandboxID)
	} else {
		require.Empty(t, metadata.InfraServices)
		require.Empty(t, metadata.InfraDiagnostics)
	}

	env.assertGuestReachable(spec.SandboxID)

	// Inspect reports the running VM.
	inspected, err := env.driver.InspectSandbox(ctx, spec.SandboxID)
	require.NoError(t, err)
	require.Equal(t, string(PhaseRunning), inspected.Phase)

	// Delete stops the VM, releases the slot, and removes the state.
	env.delete(spec.SandboxID)
}

// prepareE2EGoldenSnapshot produces (or reuses) the golden snapshot set of
// the E2E image (方式 B self-bootstrap, golden-restore plan §5): a
// preparation VM cold-boots the kernel once, pauses, and dumps a Full
// snapshot; the golden set is assembled into the driver cache layout:
//
//	<StateRoot>/images/<sha256(image)>/{rootfs.img, vmstate.snap, memory.snap, manifest.json}
//
// A complete set is reused so a persistent FC_STATE_ROOT skips the prep
// boot on reruns. The golden rootfs also carries a udev hook
// (fast-sandbox/net-up.sh) that applies the per-instance static network
// config the driver injects at restore time: a restored guest never boots
// a kernel, so the boot-arg ip= configuration does not apply (design §4.2).
func prepareE2EGoldenSnapshot(t *testing.T, binary, kernel, rootfs, stateRoot, imageRef, bootArgs string) {
	t.Helper()
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(imageRef))
	rootfsImg := filepath.Join(dir, rootfsImageName)
	vmstate := filepath.Join(dir, vmstateSnapshotName)
	memory := filepath.Join(dir, memorySnapshotName)
	manifestPath := filepath.Join(dir, "manifest.json")
	complete := func() bool {
		for _, path := range []string{rootfsImg, vmstate, memory, manifestPath} {
			if info, err := os.Stat(path); err != nil || info.IsDir() {
				return false
			}
		}
		return true
	}
	if complete() {
		t.Logf("reusing prepared golden snapshot set %s", dir)
		return
	}

	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, copyFile(rootfs, rootfsImg))
	bakeE2EGuestNetworkHook(t, rootfsImg)
	// Mark the image so the driver injects the per-instance network config
	// for it (restore.go: deliverGuestNetworkConfig).
	require.NoError(t, os.WriteFile(filepath.Join(dir, guestNetHookMarker), nil, 0o640))

	prepDir := t.TempDir()
	apiSock := filepath.Join(prepDir, "api.sock")
	logPath := filepath.Join(prepDir, "firecracker.log")
	process, err := launch(context.Background(), ExecProcessRunner{}, launchConfig{
		BinaryPath: binary, SandboxID: "e2e-prep", APIAddress: apiSock, LogPath: logPath,
	})
	require.NoError(t, err)
	defer process.Kill()

	client := NewClient(apiSock)
	defer client.Close()
	require.NoError(t, waitForAPISocket(context.Background(), apiSock, firecrackerSocketWaitTimeout))

	// The preparation machine must match the manifest machine tuple that
	// restore uses (vmstate pins mem_size_mib): 1 vCPU / 512 MiB, the same
	// as the e2e Sandbox spec.
	require.NoError(t, client.ConfigureMachine(context.Background(), MachineConfigRequest{VCPUs: 1, MemSizeMiB: 512}))
	require.NoError(t, client.ConfigureBootSource(context.Background(), BootSourceRequest{
		KernelImagePath: kernel, BootArgs: bootArgs,
	}))
	require.NoError(t, client.AttachDrive(context.Background(), DriveRequest{
		// The prep VM boots the golden rootfs read-write exactly like a
		// restored instance: the memory snapshot's filesystem state must
		// match the rootfs the instances reflink-copy from.
		DriveID: "root", PathOnHost: rootfsImg, IsRootDevice: true, IsReadOnly: false,
	}))
	// bootVM performs the InstanceStart and the Running poll; the VM must
	// not be started before it (a second InstanceStart is rejected).
	_, err = bootVM(context.Background(), client, 90)
	require.NoError(t, err)
	require.NoError(t, client.Pause(context.Background()))
	require.NoError(t, waitVMState(context.Background(), client, "Paused", 30*time.Second))

	require.NoError(t, client.CreateSnapshot(context.Background(), SnapshotCreateRequest{
		SnapshotType: "Full", SnapshotPath: vmstate, MemFilePath: memory,
	}))
	require.NoError(t, process.Kill())

	// The manifest records the machine tuple restore reads and the artifact
	// digests of the golden set (commit point: manifest last).
	rootfsDigest, err := fileSHA256(rootfsImg)
	require.NoError(t, err)
	vmstateDigest, err := fileSHA256(vmstate)
	require.NoError(t, err)
	memoryDigest, err := fileSHA256(memory)
	require.NoError(t, err)
	manifest, err := json.Marshal(map[string]any{
		"machine": map[string]any{"vcpu": "1", "memory": "512Mi"},
		"files": map[string]any{
			"rootfs.ext4":  map[string]any{"sha256": rootfsDigest, "sizeBytes": fileSize(t, rootfsImg)},
			"vmstate.snap": map[string]any{"sha256": vmstateDigest, "sizeBytes": fileSize(t, vmstate)},
			"memory.snap":  map[string]any{"sha256": memoryDigest, "sizeBytes": fileSize(t, memory)},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, manifest, 0o640))
	t.Logf("prepared golden snapshot set %s (rootfs=%s)", dir, filepath.Base(rootfs))
}

// bakeE2EGuestNetworkHook writes the restore network hook into the golden
// rootfs: a udev rule that runs fast-sandbox/net-up.sh when eth0 is
// hot-added. The rule is loaded by the preparation VM's udev at prep boot,
// so the snapshot's running udev can apply the driver-injected
// net.conf (design §4.2, "guest 内配置注入"). Baked before the prep VM
// boots so the running guest already carries the rule.
func bakeE2EGuestNetworkHook(t *testing.T, rootfsImg string) {
	t.Helper()
	if _, err := exec.LookPath("mount"); err != nil {
		t.Log("mount unavailable; skipping guest network hook bake")
		return
	}
	mountpoint, err := os.MkdirTemp("", "fc-e2e-hook")
	require.NoError(t, err)
	defer os.RemoveAll(mountpoint)
	if output, err := exec.Command("mount", "-o", "loop", rootfsImg, mountpoint).CombinedOutput(); err != nil {
		require.Failf(t, "mount golden rootfs for network hook: %v\n%s", err.Error(), output)
	}
	mounted := true
	defer func() {
		if mounted {
			_, _ = exec.Command("umount", mountpoint).CombinedOutput()
		}
	}()
	hookDir := filepath.Join(mountpoint, strings.TrimPrefix(guestNetConfigDir, "/"))
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	script := `#!/bin/sh
# Fast-sandbox restore network hook: apply the per-instance static config
# injected by the runtime driver before snapshot restore.
[ -r /etc/fast-sandbox/net.conf ] || exit 0
. /etc/fast-sandbox/net.conf
ip link set eth0 up 2>/dev/null
ip addr flush dev eth0 2>/dev/null
[ -n "$GUEST_IP" ] && ip addr add "$GUEST_IP/$GUEST_PREFIX" dev eth0 2>/dev/null
[ -n "$GATEWAY" ] && ip route add default via "$GATEWAY" 2>/dev/null
exit 0
`
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "net-up.sh"), []byte(script), 0o755))
	udevDir := filepath.Join(mountpoint, "etc", "udev", "rules.d")
	require.NoError(t, os.MkdirAll(udevDir, 0o755))
	rule := `SUBSYSTEM=="net", ACTION=="add", KERNEL=="eth0", RUN+="/bin/sh /etc/fast-sandbox/net-up.sh"
`
	require.NoError(t, os.WriteFile(filepath.Join(udevDir, "99-fast-sandbox-net.rules"), []byte(rule), 0o644))
	if output, err := exec.Command("umount", mountpoint).CombinedOutput(); err != nil {
		require.Failf(t, "unmount golden rootfs after network hook: %v\n%s", err.Error(), output)
	}
	mounted = false
}

// waitVMState polls the Firecracker machine state until it matches want.
func waitVMState(ctx context.Context, client *Client, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state, err := client.VMState(ctx)
		if err != nil {
			return err
		}
		if state == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Firecracker VM did not reach %s within %s (state %q)", want, timeout, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootPollInterval):
		}
	}
}

func fileSHA256(path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

// e2eEnvironment bundles the host checks, StateRoot, slot manager, driver,
// and per-sandbox helpers shared by the E2E tests.
type e2eEnvironment struct {
	t            *testing.T
	manager      *fastletnetwork.Manager
	driver       *Driver
	infraMgr     *fastletinfra.Manager // nil when Infra delivery is disabled
	imageRef     string
	stateRoot    string
	podUID       string
	infraEnabled bool
	created      []string
}

// newE2EEnvironment validates the host, prepares the golden snapshot set,
// pre-provisions capacity network slots, and wires the driver (with a real
// Infra plan when infraEnabled).
func newE2EEnvironment(t *testing.T, capacity int, infraEnabled bool) *e2eEnvironment {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root: netns, tap, and iptables setup")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("requires /dev/kvm")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("requires /dev/net/tun")
	}

	binary := envOr("FC_BINARY", "")
	kernel := envOr("FC_KERNEL", "")
	rootfs := envOr("FC_ROOTFS", "")
	imageRef := envOr("FC_IMAGE_REF", "e2e:ubuntu")
	for name, path := range map[string]string{"FC_BINARY": binary, "FC_KERNEL": kernel, "FC_ROOTFS": rootfs} {
		require.FileExists(t, path, name)
	}

	ctx := context.Background()
	stateRoot := os.Getenv("FC_STATE_ROOT")
	if stateRoot == "" {
		stateRoot = t.TempDir()
	} else {
		require.NoError(t, os.MkdirAll(stateRoot, 0o750))
	}
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off net.ifnames=0 biosdevname=0"
	// The golden snapshot set is the runtime asset: a preparation VM boots
	// the kernel once, pauses, and dumps vmstate + memory; subsequent
	// Sandboxes restore from it (no kernel at runtime). A complete set is
	// reused, so a persistent FC_STATE_ROOT skips the prep boot on reruns.
	prepareE2EGoldenSnapshot(t, binary, kernel, rootfs, stateRoot, imageRef, bootArgs)

	// Unique Pod UID per run keeps the resourceName-derived netns/bridge names
	// from colliding with leftovers of earlier runs.
	podUID := "e2e-pod-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	// In-memory slot store: the manager's async Replenish keeps writing
	// durable files, which would race with t.TempDir cleanup.
	netStateRoot, err := os.MkdirTemp("", "fc-e2e-netstate")
	require.NoError(t, err)
	t.Cleanup(func() {
		for attempt := 0; attempt < 20; attempt++ {
			if removeErr := os.RemoveAll(netStateRoot); removeErr == nil || os.IsNotExist(removeErr) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.RemoveAll(netStateRoot)
	})
	var slotCounter atomic.Int64
	manager, err := fastletnetwork.NewManager(fastletnetwork.Config{
		Capacity: capacity, PodUID: podUID,
		StateRoot: netStateRoot, NetNSRoot: "/run/netns", HostNetNSRoot: "/run/fast-sandbox/netns",
		IDGenerator:      func() (string, error) { return fmt.Sprintf("e2e-slot-%d", slotCounter.Add(1)), nil },
		Now:              time.Now,
		ReplenishTimeout: time.Minute,
	}, fastletnetwork.NewGuestVMNetNSDriver(fastletnetwork.LinuxDriverConfig{}),
		newMemoryStateStore())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll("/run/netns", 0o755))
	require.NoError(t, os.MkdirAll("/run/fast-sandbox/netns", 0o755))
	require.NoError(t, manager.Initialize(ctx))

	profile := runtimecatalog.RuntimeProfile{
		Name: apiv1alpha2.RuntimeFirecracker, ProfileHash: "e2e-profile",
		Firecracker: &runtimecatalog.FirecrackerConfig{
			BinaryPath: binary, KernelPath: kernel, RootfsPath: rootfs, StateRoot: stateRoot,
			DefaultVCPUs: 1, DefaultMemory: "512Mi", BootTimeoutSeconds: 90,
			BootArgs: bootArgs,
		},
		Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady},
	}
	driver, err := New(profile)
	require.NoError(t, err)
	driver.SetNetworkManager(manager)

	env := &e2eEnvironment{
		t: t, manager: manager, driver: driver, imageRef: imageRef,
		stateRoot: stateRoot, podUID: podUID, infraEnabled: infraEnabled,
	}
	if infraEnabled {
		// Real Infra plan: EnsureSandbox performs a genuine GuestCopy delivery
		// (loop mount + copy) of sandbox-init and an "execd" component into the
		// instance rootfs, verified later with debugfs.
		sandboxInit := filepath.Join(t.TempDir(), "sandbox-init")
		require.NoError(t, os.WriteFile(sandboxInit, []byte("#!/bin/sh\n"), 0o755))
		env.infraMgr = newE2EInfraManager(t, profile, sandboxInit)
		driver.SetInfraManager(env.infraMgr)
		require.NoError(t, driver.Initialize(ctx, ""))
	}
	require.NoError(t, driver.Initialize(ctx, ""))

	// Local sampled provider so spans carry real trace IDs without an OTLP
	// endpoint; the root trace ID is derived from the sandbox id.
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	t.Logf("restoring Firecracker microVMs from golden snapshot set (kernel=%s)", kernel)
	for index := 1; index <= capacity; index++ {
		env.created = append(env.created, fmt.Sprintf("e2e-sandbox-%d", index))
	}
	return env
}

// spec builds the SandboxSpec for the i-th sandbox ("e2e-sandbox-<i>").
func (env *e2eEnvironment) spec(index int) *fastletapi.SandboxSpec {
	sandboxID := fmt.Sprintf("e2e-sandbox-%d", index)
	spec := &fastletapi.SandboxSpec{
		SandboxID: sandboxID, ClaimUID: fmt.Sprintf("e2e-claim-%d", index), ClaimName: sandboxID, ClaimNamespace: "e2e",
		InstanceGeneration: 1, RuntimeInstanceID: fmt.Sprintf("e2e-ri-%d", index), AssignmentAttempt: 1,
		FastletPodUID: env.podUID, Image: env.imageRef, CPU: "1", Memory: "512Mi",
		RuntimeProfileHash: "e2e-profile", ResourceProfileHash: "e2e-resource",
	}
	if env.infraEnabled {
		spec.InfraRevision = env.infraMgr.Revision()
	}
	return spec
}

// traceContext returns a context carrying a sampled root span whose trace ID
// is derived from the sandbox id, exercising the identity correlation used by
// the production Fastlet.
func (env *e2eEnvironment) traceContext(sandboxID string) (context.Context, func()) {
	digest := sha256.Sum256([]byte(sandboxID))
	rootContext := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID(digest[:16]),
		SpanID:     trace.SpanID(digest[16:24]),
		TraceFlags: trace.FlagsSampled,
	}))
	ctx, span := observability.Start(rootContext, "e2e.firecracker.create",
		attribute.String("sandbox_uid", sandboxID),
		attribute.String("image", env.imageRef),
	)
	return ctx, func() { observability.End(span, nil) }
}

// boot creates one sandbox through the driver and validates the result.
// It must be called from the test goroutine (it uses require).
func (env *e2eEnvironment) boot(ctx context.Context, spec *fastletapi.SandboxSpec) *SandboxMetadata {
	env.t.Helper()
	metadata, err := env.driver.EnsureSandbox(ctx, spec)
	require.NoError(env.t, err)
	env.assertBooted(metadata, spec, trace.SpanContextFromContext(ctx).TraceID())
	return metadata
}

// assertBooted validates the metadata, trace correlation, and process
// identity of one successful boot.
func (env *e2eEnvironment) assertBooted(metadata *SandboxMetadata, spec *fastletapi.SandboxSpec, traceID trace.TraceID) {
	t := env.t
	t.Helper()
	digest := sha256.Sum256([]byte(spec.SandboxID))
	require.True(t, traceID.IsValid(), "trace context must carry a valid trace ID")
	require.Equal(t, trace.TraceID(digest[:16]), traceID, "trace ID must be derived from the sandbox id")
	t.Logf("traceId=%s sandboxId=%s", traceID.String(), spec.SandboxID)
	require.Equal(t, string(PhaseRunning), metadata.Phase)
	require.Positive(t, metadata.PID)
	// The launched process must match the NodeJanitor residual-process
	// matcher: binary "firecracker" and --id equal to the truncated sandbox id.
	cmdline := readProcCmdline(t, metadata.PID)
	require.Equal(t, "firecracker", filepath.Base(cmdline[0]))
	require.Equal(t, spec.SandboxID, cmdline[indexOf(cmdline, "--id")+1])
}

// assertInfra verifies the delivered Infra plan: services for route
// publication and the artifacts inside the instance rootfs image (checked
// with debugfs against the image file).
func (env *e2eEnvironment) assertInfra(metadata *SandboxMetadata, sandboxID string) {
	t := env.t
	t.Helper()
	require.True(t, env.infraEnabled, "infra assertions require an infra environment")
	require.Len(t, metadata.InfraServices, 1)
	require.Equal(t, "execd", metadata.InfraServices[0].Component)
	require.Equal(t, uint32(44772), metadata.InfraServices[0].Port)
	instanceRootfs := filepath.Join(env.stateRoot, "sandboxes", sandboxID, instanceRootfsName)
	assertGuestFile(t, instanceRootfs, "/.fast/run/infra.json", sandboxID)
	assertGuestFile(t, instanceRootfs, "/.fast/components/execd/execd", "fake-execd")
	assertGuestFile(t, instanceRootfs, "/.fast/bin/sandbox-init", "#!/bin/sh")
}

// assertGuestReachable verifies the guest VM answers ICMP on its address
// inside the private CIDR. The host routes the guest address via the shared
// bridge to the pre-provisioned slot tap, so this exercises the real
// tap/bridge/guest data path (the slot IP itself is owned by the slot netns
// and would answer locally, which is why the guest address is probed).
func (env *e2eEnvironment) assertGuestReachable(sandboxID string) {
	t := env.t
	t.Helper()
	if _, err := exec.LookPath("ping"); err != nil {
		t.Logf("ping not installed; skipping reachability check")
		return
	}
	slot, ok := env.manager.Lookup(sandboxID)
	require.True(t, ok)
	guestIP, err := fastletnetwork.GuestVMIP(slot)
	require.NoError(t, err)
	// Earlier tests in this process reused the same private addresses and
	// left the host neighbour cache pointing at their (now dead) VMs and
	// stale bridge ports; a frame to a stale MAC is flooded to a gone tap and
	// dropped. Flush the bridge neighbours so ARP is re-resolved against the
	// live guest.
	_, _ = exec.Command("ip", "neigh", "flush", "dev", slot.Bridge).CombinedOutput()
	output, err := exec.Command("ping", "-c", "1", "-W", "3", guestIP).CombinedOutput()
	if err != nil {
		t.Logf("ping failed; dumping guest network diagnostics:\n%s", dumpNetworkDiagnostics(t, slot, env.stateRoot))
		require.NoErrorf(t, err, "guest %s not reachable: %s", guestIP, output)
	}
	t.Logf("guest reachable at %s (slot %s, tap %s)", guestIP, slot.IP, slot.GuestTap)
}

// delete stops the VM, releases the slot, and removes the state.
func (env *e2eEnvironment) delete(sandboxID string) {
	t := env.t
	t.Helper()
	require.NoError(t, env.driver.DeleteSandbox(context.Background(), sandboxID))
	directory, err := sandboxDir(env.stateRoot, sandboxID)
	require.NoError(t, err)
	_, err = os.Stat(directory)
	require.True(t, os.IsNotExist(err))
	_, exists := env.manager.Lookup(sandboxID)
	require.False(t, exists)
}

// teardown removes any sandbox that is still around and closes the driver.
// Individual tests call delete() first; teardown is the idempotent safety net.
func (env *e2eEnvironment) teardown() {
	for _, sandboxID := range env.created {
		_ = env.driver.DeleteSandbox(context.Background(), sandboxID)
	}
	_ = env.driver.Close()
}

// dumpNetworkDiagnostics collects the netns and guest state for failure logs.
func dumpNetworkDiagnostics(t *testing.T, slot *fastletnetwork.Slot, stateRoot string) string {
	t.Helper()
	var builder strings.Builder
	for _, args := range [][]string{
		{"addr", "show"},
		{"route", "show"},
		{"netns", "exec", slot.NetNSName, "ip", "addr", "show"},
		{"netns", "exec", slot.NetNSName, "ip", "route", "show"},
		{"netns", "exec", slot.NetNSName, "sysctl", "net.ipv4.ip_forward"},
		{"netns", "exec", slot.NetNSName, "iptables", "-t", "nat", "-L", "-n"},
	} {
		output, err := exec.Command("ip", args...).CombinedOutput()
		fmt.Fprintf(&builder, "$ ip %s\n%s\n", strings.Join(args, " "), output)
		if err != nil {
			fmt.Fprintf(&builder, "(exit %v)\n", err)
		}
	}
	// The firecracker process log carries the guest serial console, which
	// shows the guest-side network configuration.
	directory, err := sandboxDir(stateRoot, slot.Owner.SandboxUID)
	if err == nil {
		if payload, readErr := os.ReadFile(filepath.Join(directory, processLogName)); readErr == nil {
			tail := payload
			if len(tail) > 8192 {
				tail = tail[len(tail)-8192:]
			}
			fmt.Fprintf(&builder, "--- firecracker.log (guest serial) ---\n%s\n", tail)
		}
	}
	return builder.String()
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func readProcCmdline(t *testing.T, pid int) []string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	require.NoError(t, err)
	return strings.Split(strings.TrimRight(string(payload), "\x00"), "\x00")
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
