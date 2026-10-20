package firecracker

// snapshot_driver.go implements the optional Snapshotter contract
// (runtimecontract.Snapshotter) for the live firecracker driver: pause the
// running microVM, dump the writable instance rootfs plus the vmstate/memory
// pair, resume the VM on every path, assemble a restore-compatible manifest
// (byte-format shared with the golden-image builder via internal/artifacts),
// and publish the set through the node runtime-agent. Local mode (no agent)
// refuses snapshots: publication requires the agent's store credentials.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

// snapshotStagingDir holds per-snapshot staging directories:
//
//	<StateRoot>/snapshots/<snapshotID>/{rootfs.ext4, vmstate.snap, memory.snap, manifest.json, SHA256SUMS}
const snapshotStagingDir = "snapshots"

// publishedRootfsName is the rootfs file name in a published artifact set.
// The instance drive file is rootfs.img; consumers (the agent pull layer)
// rename it back on arrival, exactly as for builder-produced sets.
const publishedRootfsName = "rootfs.ext4"

// jailerSnapshotDumpDir is the directory (inside the jail root) the jailed
// VMM dumps its snapshot files into: a chrooted VMM cannot write outside its
// jail, so the driver addresses the dump files CHROOT-RELATIVELY and moves
// them into the staging directory afterwards. The dump uses dedicated file
// names: the same directory also holds the hard-linked restore snapshots,
// and truncating those through the shared inodes would corrupt the cache.
const (
	jailerSnapshotDumpDir = "snapshots"
	jailedDumpVMStateName = "dump-vmstate.snap"
	jailedDumpMemoryName  = "dump-memory.snap"
	// jailerSpillDirName is the chroot-relative mount point of the snapshot
	// spill root inside a jailed VMM (bind-mounted at instance creation —
	// the jailer clones its mount namespace at process start, so a dump
	// window bind would never be visible to the running VMM).
	jailerSpillDirName = "spill"
)

// snapshotManifestName is the commit-point document of the artifact set.
const snapshotManifestName = "manifest.json"

// snapshotSpillDirEnv names a fast local staging area (tmpfs, an emptyDir
// with medium: Memory, or a local NVMe path) mounted into the fastlet. When
// set (and roomy enough), the paused VMM dumps vmstate/memory THERE and the
// driver moves the files to the publish staging after the resume — the
// memory dump then pays tmpfs bandwidth instead of the (often
// network-backed) StateRoot filesystem, shrinking the business-visible
// pause window from seconds to sub-second. Empty (default) keeps the
// direct-to-staging dump.
const snapshotSpillDirEnv = "FAST_SANDBOX_SNAPSHOT_SPILL_DIR"

// Driver implements the optional snapshot extension.
var _ runtimecontract.Snapshotter = (*Driver)(nil)

// snapshotMu serializes the pause/dump/resume window across all Sandboxes of
// this driver: only one VM on the node is ever paused at a time. Manifest
// assembly and publication run outside the lock.
var snapshotMu sync.Mutex

// CreateSnapshot snapshots a running Sandbox in place and publishes the
// artifact set. Template snapshots publish under the template name so later
// creates boot from them; checkpoint snapshots publish an instance-private
// set (no index) that only the owning Sandbox resumes from. The VM is always
// resumed; a failed dump leaves the Sandbox running and discards the staging
// directory without publishing anything.
func (d *Driver) CreateSnapshot(ctx context.Context, input *runtimecontract.SnapshotInput) (*runtimecontract.SnapshotResult, error) {
	if input == nil || input.SandboxID == "" || input.SnapshotID == "" {
		return nil, fmt.Errorf("%w: sandboxId and snapshotId are required", ErrInvalidConfig)
	}
	kind := input.Kind
	if kind == "" {
		kind = fastletapi.SnapshotKindTemplate
	}
	switch kind {
	case fastletapi.SnapshotKindTemplate:
		if input.TemplateName == "" {
			return nil, fmt.Errorf("%w: templateName is required for a template snapshot", ErrInvalidConfig)
		}
	case fastletapi.SnapshotKindCheckpoint:
		if input.TemplateName != "" {
			return nil, fmt.Errorf("%w: checkpoint snapshots publish no template index; templateName must be empty", ErrInvalidConfig)
		}
	default:
		return nil, fmt.Errorf("%w: unknown snapshot kind %q", ErrInvalidConfig, kind)
	}
	if err := validateSandboxID(input.SnapshotID); err != nil {
		return nil, fmt.Errorf("%w: invalid snapshot id %q", ErrInvalidConfig, input.SnapshotID)
	}
	d.mu.RLock()
	plan := dumpPlan{
		stateRoot:   d.config.StateRoot,
		binaryName:  filepath.Base(d.config.BinaryPath),
		sandboxID:   input.SandboxID,
		snapshotID:  input.SnapshotID,
		jailed:      d.config.JailerPath != "",
		bootTimeout: d.config.BootTimeoutSeconds,
		spillRoot:   d.snapshotSpillRoot(),
	}
	d.mu.RUnlock()
	plan.sandboxDir = filepath.Join(plan.stateRoot, sandboxStateDir, plan.sandboxID)
	plan.staging = filepath.Join(plan.stateRoot, snapshotStagingDir, plan.snapshotID)
	if err := os.RemoveAll(plan.staging); err != nil {
		return nil, fmt.Errorf("clear stale snapshot staging %s: %w", plan.staging, err)
	}

	// Capacity gate BEFORE any pause: a dump that cannot land wastes the
	// business interruption. Self-heal once through the image cache GC.
	if err := d.ensureDumpCapacity(plan); err != nil {
		return nil, err
	}

	dumpErr := d.dumpRunningSandbox(ctx, &plan)
	if dumpErr != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, dumpErr
	}
	klog.InfoS("firecracker sandbox dumped",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID,
		"pauseWindow", plan.pauseWindow.String(), "spillMove", plan.spillMove.String(),
		"spilled", plan.spilled, "rootfsClone", plan.rootfsClone.String(), "rootfsCopy", plan.rootfsCopy.String(),
		"pauseAPI", plan.pauseAPI.String(), "dumpAPI", plan.dumpAPI.String(), "resumeAPI", plan.resumeAPI.String())

	// Checkpoints carry the egress-owned vault file in the bundle: the export
	// runs here — inside the pause flow, strictly before the control plane
	// releases the runtime (the release is what sends REMOVE_BINDING and
	// purges the egress-local file). An export failure fails the checkpoint,
	// so the egress-local file survives for the retry. Template snapshots
	// NEVER export: the vault belongs to one sandbox instance, and a shared
	// boot image would leak it to every sandbox booted from the template.
	if kind == fastletapi.SnapshotKindCheckpoint {
		if err := d.stageCheckpointVault(ctx, input.SandboxID, plan.staging); err != nil {
			_ = os.RemoveAll(plan.staging)
			return nil, err
		}
	}
	// The recorded egress policy rides EVERY snapshot bundle (checkpoint and
	// template alike) — the deliberate asymmetry vs the vault: the policy is
	// desired state rather than a secret, and a template clone must inherit
	// the relayed policy, not just the bootstrap binding input. Same failure
	// posture as the vault: an export failure fails the snapshot so the
	// egress-local file survives for the retry.
	if err := d.stageCheckpointPolicy(ctx, input.SandboxID, plan.staging); err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}

	sizeBytes, err := assembleSnapshotManifest(plan.stateRoot, plan.staging, plan.sandboxDir, input.ActionBindings, d.policyBindingHandlerOf())
	if err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}
	klog.InfoS("firecracker checkpoint manifest assembled",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "kind", kind,
		"sizeBytes", sizeBytes, "staging", plan.staging)

	client, err := d.agentClientOrNil()
	if err != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, err
	}
	if client == nil {
		_ = os.RemoveAll(plan.staging)
		return nil, fmt.Errorf("%w: live snapshots require the firecracker runtime-agent for artifact publication", ErrInvalidConfig)
	}
	// The pause window is closed and the staged set is complete: report
	// Publishing before the (potentially long) upload — the caller may
	// admit the next snapshot of this Sandbox from here on.
	if input.OnPublishing != nil {
		input.OnPublishing()
	}
	publishStarted := time.Now()
	outcome, publishErr := client.PublishImage(ctx, "snapshot-"+plan.snapshotID, publishKind(kind), input.TemplateName, plan.staging)
	if publishErr != nil {
		_ = os.RemoveAll(plan.staging)
		return nil, fmt.Errorf("publish snapshot artifacts: %w", publishErr)
	}
	result := &runtimecontract.SnapshotResult{
		SnapshotID:     input.SnapshotID,
		ManifestRef:    outcome.ManifestRef,
		ArtifactDigest: outcome.ArtifactDigest,
		SizeBytes:      sizeBytes,
	}
	// A checkpoint keeps the published set in the node-local image cache: a
	// resume that still lands on this node restores locally (~ms) instead of
	// pulling the full set back from the store (~16s per 3.5GiB). The commit
	// is best-effort — the store copy is authoritative, and any miss (cache
	// evicted by the image GC, or a resume on another node) falls back to the
	// normal pull path. Template snapshots keep discarding staging; their
	// same-node prewarm is issue #57.
	if kind == fastletapi.SnapshotKindCheckpoint {
		if cacheErr := d.commitCheckpointCache(result, plan.staging); cacheErr != nil {
			klog.ErrorS(cacheErr, "checkpoint local cache commit failed; a resume will pull from the store",
				"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "manifestRef", outcome.ManifestRef)
		}
	} else {
		_ = os.RemoveAll(plan.staging)
	}
	klog.InfoS("firecracker snapshot published",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "kind", kind, "templateName", input.TemplateName,
		"manifestRef", outcome.ManifestRef, "sizeBytes", sizeBytes, "publish", time.Since(publishStarted).String())
	return result, nil
}

// commitCheckpointCache moves a published checkpoint artifact set from its
// staging directory into the node-local image cache, under the canonical
// checkpoint reference (artifacts.CheckpointReference), in the standard cache
// layout (rootfs.img/vmstate.snap/memory.snap/manifest.json). The set is
// assembled in a non-digest staging subdirectory and renamed into place
// atomically, so the image GC — which only considers 64-hex cache names —
// observes either a complete entry or nothing at all. The staging directory
// is consumed on every path.
func (d *Driver) commitCheckpointCache(result *runtimecontract.SnapshotResult, staging string) error {
	if result == nil || result.ArtifactDigest == "" {
		return fmt.Errorf("%w: checkpoint result carries no artifact digest", ErrInvalidConfig)
	}
	reference := artifacts.CheckpointReference(result.ArtifactDigest)
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	cacheDir := filepath.Join(stateRoot, imageCacheDir, imageKey(reference))
	if verifyRestorableImage(stateRoot, reference) == nil {
		// A concurrent replay already committed the identical set.
		_ = os.RemoveAll(staging)
		d.touchImage(reference)
		return nil
	}
	tmpDir := filepath.Join(stateRoot, imageCacheDir, ".staging-"+filepath.Base(staging))
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("prepare checkpoint cache staging: %w", err)
	}
	mappings := map[string]string{
		publishedRootfsName:  rootfsImageName,
		vmstateSnapshotName:  vmstateSnapshotName,
		memorySnapshotName:   memorySnapshotName,
		snapshotManifestName: snapshotManifestName,
	}
	// The vault and policy files are optional bundle members; they ride
	// into the cache so the restore path finds them next to the snapshot
	// files. Mirror the assembly/publish stat contract: only absence skips
	// a file — dropping it on a transient error would corrupt the
	// node-local cache asymmetrically against the published set.
	for _, bundleName := range []string{vaultBundleName, policyBundleName} {
		if _, err := os.Stat(filepath.Join(staging, bundleName)); err == nil {
			mappings[bundleName] = bundleName
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = os.RemoveAll(tmpDir)
			_ = os.RemoveAll(staging)
			return fmt.Errorf("stat staged %s for the cache commit: %w", bundleName, err)
		}
	}
	for from, to := range mappings {
		if err := os.Rename(filepath.Join(staging, from), filepath.Join(tmpDir, to)); err != nil {
			_ = os.RemoveAll(tmpDir)
			_ = os.RemoveAll(staging)
			return fmt.Errorf("stage checkpoint cache entry: %w", err)
		}
	}
	if err := os.Rename(tmpDir, cacheDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		_ = os.RemoveAll(staging)
		// A concurrent commit won the rename; the identical set is already
		// addressable.
		if verifyRestorableImage(stateRoot, reference) == nil {
			d.touchImage(reference)
			return nil
		}
		return fmt.Errorf("commit checkpoint cache entry: %w", err)
	}
	_ = os.RemoveAll(staging)
	d.touchImage(reference)
	return nil
}

// publishKind maps the Fastlet-side snapshot kind onto the runtime-agent's
// publish protocol. The two enums are deliberately separate layers (the
// Fastlet kind travels FastPath->Fastlet as JSON; the publish kind travels
// driver->agent), so the mapping is explicit: a raw string cast here would
// silently send "Checkpoint" where the agent expects "checkpoint".
func publishKind(kind fastletapi.SnapshotKind) string {
	if kind == fastletapi.SnapshotKindCheckpoint {
		return agentprotocol.PublishKindCheckpoint
	}
	return agentprotocol.PublishKindTemplate
}

// DeleteSnapshot discards the staging directory of a snapshot. It never
// unpublishes stored objects. A missing directory is a successful no-op.
func (d *Driver) DeleteSnapshot(_ context.Context, snapshotID string) error {
	if err := validateSandboxID(snapshotID); err != nil {
		return fmt.Errorf("%w: invalid snapshot id %q", ErrInvalidConfig, snapshotID)
	}
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	staging := filepath.Join(stateRoot, snapshotStagingDir, snapshotID)
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove snapshot staging %s: %w", staging, err)
	}
	return nil
}

// defaultSnapshotCapacityWait bounds the best-effort GC + recheck window
// when the staging filesystem cannot hold the artifact set (a field so
// tests can shorten it).
const defaultSnapshotCapacityWait = 15 * time.Second

// ensureDumpCapacity verifies the staging filesystem can hold the full
// artifact set (instance rootfs + memory + margin), once after triggering
// the image-cache GC. It must run before the VM is paused: an ENOSPC
// mid-dump would burn the pause window for nothing. Returns the
// retryable ErrInsufficientStorage sentinel when space stays short.
func (d *Driver) ensureDumpCapacity(plan dumpPlan) error {
	need, err := d.dumpFootprintBytes(plan)
	if err != nil {
		// The dump itself will report a precise error; do not block on
		// unmeasurable inputs (e.g. a vanished state directory).
		return nil //nolint:nilerr // unmeasurable footprint must not block the dump; the dump itself reports the precise error
	}
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	if stagingFree(stateRoot) >= need {
		return nil
	}
	d.TriggerImageGC()
	wait := d.snapshotCapacityWait
	if wait <= 0 {
		wait = defaultSnapshotCapacityWait
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if stagingFree(stateRoot) >= need {
			return nil
		}
	}
	free := stagingFree(stateRoot)
	klog.InfoS("firecracker snapshot staging space insufficient; parking the task",
		"stateRoot", stateRoot, "needBytes", need, "freeBytes", free)
	return fmt.Errorf("%w: staging needs %d bytes, %d free", runtimecontract.ErrInsufficientStorage, need, free)
}

// dumpFootprintBytes computes the full artifact set size: the instance
// rootfs logical size plus the guest memory with margin.
func (d *Driver) dumpFootprintBytes(plan dumpPlan) (int64, error) {
	state, err := loadState(plan.sandboxDir)
	if err != nil {
		return 0, err
	}
	if state.Config.Spec.Memory == "" {
		return 0, fmt.Errorf("sandbox spec carries no memory size")
	}
	mib, err := parseMemMiB(state.Config.Spec.Memory)
	if err != nil {
		return 0, err
	}
	rootfs := filepath.Join(plan.sandboxDir, instanceRootfsName)
	if plan.jailed {
		rootfs = filepath.Join(plan.jailRoot(), rootfsImageName)
	}
	info, err := d.stat(rootfs)
	if err != nil {
		return 0, err
	}
	// vmstate is small; the margin covers filesystem overhead.
	return info.Size() + int64(mib)<<20 + (64 << 20) + int64(mib)<<20/8, nil
}

// stagingFree reports the free bytes of the filesystem holding the
// snapshot staging directory (the StateRoot).
func stagingFree(stateRoot string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(stateRoot, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}

// dumpPlan carries one serialized pause/dump/resume execution. spilled
// reports whether the dump landed on the fast spill area; pauseWindow
// covers pause→resume (the business-visible interruption), spillMove the
// post-resume relocation into the publish staging.
type dumpPlan struct {
	stateRoot   string
	binaryName  string
	sandboxID   string
	snapshotID  string
	sandboxDir  string
	staging     string
	jailed      bool
	bootTimeout int32
	spillRoot   string
	spilled     bool
	pauseWindow time.Duration
	spillMove   time.Duration
	rootfsClone time.Duration
	rootfsCopy  time.Duration
	pauseAPI    time.Duration
	dumpAPI     time.Duration
	resumeAPI   time.Duration
}

// jailRoot returns the jail root of the plan's Sandbox (jailer mode only).
func (p dumpPlan) jailRoot() string {
	return jailerRoot(filepath.Join(p.stateRoot, jailerChrootBaseDir), p.binaryName, truncatedSandboxID(p.sandboxID))
}

// snapshotSpillRoot returns the configured spill root ("" disables
// spilling). It is read per call so tests can inject without racing the
// driver lock.
func (d *Driver) snapshotSpillRoot() string {
	return strings.TrimSpace(os.Getenv(snapshotSpillDirEnv))
}

// spillDirFor resolves the per-snapshot spill directory and applies the
// capacity guard. It returns "" when spilling is disabled or the area
// cannot hold the dump (the caller falls back to the
// direct-to-staging dump). The guard needs the VM's memory size: unknown
// sizes fall back rather than risk a mid-dump ENOSPC.
func (d *Driver) spillDirFor(snapshotID, memoryQuantity string) string {
	root := d.snapshotSpillRoot()
	if root == "" {
		return ""
	}
	if err := validateSandboxID(snapshotID); err != nil {
		return ""
	}
	memBytes := int64(512 << 20)
	if memoryQuantity != "" {
		if mib, err := parseMemMiB(memoryQuantity); err == nil && mib > 0 {
			memBytes = int64(mib) << 20
		} else {
			return ""
		}
	}
	// vmstate is small; the margin covers filesystem overhead growth.
	need := memBytes + memBytes/8 + (64 << 20)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return ""
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free < need {
		klog.InfoS("firecracker snapshot spill area too small, falling back to staging dump",
			"spillRoot", root, "freeBytes", free, "needBytes", need)
		return ""
	}
	dir := filepath.Join(root, snapshotID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ""
	}
	return dir
}

// dumpRunningSandbox performs the pause/dump/resume window. The rootfs copy
// and the vmstate/memory dump both happen inside the pause window for
// consistency; when a spill area is available the dump lands THERE and the
// relocation into the publish staging runs after the resume, so the pause
// window pays the spill area's bandwidth (tmpfs/local NVMe) instead of the
// StateRoot filesystem. The Sandbox keeps running on every failure path,
// with a failed resume joined after (and never masking) the dump error.
func (d *Driver) dumpRunningSandbox(ctx context.Context, plan *dumpPlan) error { //nolint:gocognit // pre-existing pause/dump/resume window; refactor tracked separately
	snapshotMu.Lock()
	defer snapshotMu.Unlock()

	state, err := loadState(plan.sandboxDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", runtimecontract.ErrSandboxNotFound, plan.sandboxID)
		}
		return err
	}
	if state.Phase != PhaseRunning {
		return fmt.Errorf("%w: sandbox runtime is %q, must be Running", ErrInvalidConfig, state.Phase)
	}
	if state.APIAddress == "" {
		return fmt.Errorf("%w: sandbox state carries no API socket", ErrInvalidConfig)
	}
	if err := os.MkdirAll(plan.staging, 0o750); err != nil {
		return err
	}

	// Resolve the spill area BEFORE pausing: an unspillable dump falls back
	// up front, never mid-window.
	spillDir := d.spillDirFor(plan.snapshotID, state.Config.Spec.Memory)
	plan.spilled = spillDir != ""
	klog.InfoS("firecracker sandbox dump starting",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID,
		"staging", plan.staging, "jailed", plan.jailed, "spilled", plan.spilled)

	// The instance root drive: the state-directory copy in direct mode, the
	// jail-root copy in jailer mode.
	rootfs := filepath.Join(plan.sandboxDir, instanceRootfsName)
	// Snapshot paths as the (possibly jailed) VMM sees them:
	//   - direct + no spill: absolute staging paths;
	//   - direct + spill: absolute paths inside the spill area;
	//   - jailed (either way): chroot-relative under snapshots/, with the
	//     spill area bind-mounted over the jail's snapshots/ when spilled —
	//     the restore hard links in that directory are never touched
	//     (dedicated dump file names, and hidden behind the bind anyway).
	vmstateTarget := filepath.Join(plan.staging, vmstateSnapshotName)
	memoryTarget := filepath.Join(plan.staging, memorySnapshotName)
	if plan.spilled && !plan.jailed {
		vmstateTarget = filepath.Join(spillDir, jailedDumpVMStateName)
		memoryTarget = filepath.Join(spillDir, jailedDumpMemoryName)
	}
	jailDumpDir := ""
	if plan.jailed {
		rootfs = filepath.Join(plan.jailRoot(), rootfsImageName)
		jailDumpDir = filepath.Join(plan.jailRoot(), jailerSnapshotDumpDir)
		if err := os.MkdirAll(jailDumpDir, 0o750); err != nil {
			return err
		}
		if plan.spilled {
			// The spill root was bind-mounted into the jail at instance
			// creation; the per-snapshot directory is reachable behind it.
			// A sandbox created before the spill was configured (or whose
			// bind failed) falls back to the in-jail dump.
			if _, err := os.Stat(filepath.Join(plan.jailRoot(), jailerSpillDirName, plan.snapshotID)); err != nil {
				plan.spilled = false
				_ = os.RemoveAll(spillDir)
			}
		}
		if plan.spilled {
			vmstateTarget = filepath.ToSlash(filepath.Join("/", jailerSpillDirName, plan.snapshotID, jailedDumpVMStateName))
			memoryTarget = filepath.ToSlash(filepath.Join("/", jailerSpillDirName, plan.snapshotID, jailedDumpMemoryName))
		} else {
			vmstateTarget = filepath.ToSlash(filepath.Join("/", jailerSnapshotDumpDir, jailedDumpVMStateName))
			memoryTarget = filepath.ToSlash(filepath.Join("/", jailerSnapshotDumpDir, jailedDumpMemoryName))
		}
	}

	cleanupSpill := func() {
		if !plan.spilled {
			return
		}
		_ = os.RemoveAll(spillDir)
	}

	// Clone the instance rootfs BEFORE pausing when the filesystem supports
	// reflinks: the clone is atomic at the extent level (concurrent guest
	// writes COW into new extents and never touch the copy), and
	// disk-behind-memory is the crash-consistent direction — so a slow
	// reflink (seconds on a loaded host) no longer sits in the
	// business-visible pause window. Filesystems without reflinks keep the
	// in-window full copy: it is not atomic and must happen while paused.
	stagedRootfs := filepath.Join(plan.staging, publishedRootfsName)
	rootfsCloned := false
	reflinkStarted := time.Now()
	if err := reflinkOnly(rootfs, stagedRootfs); err == nil {
		rootfsCloned = true
	}
	plan.rootfsClone = time.Since(reflinkStarted)

	client := d.newClient(state.APIAddress)
	defer client.Close()
	pauseStarted := time.Now()
	if err := client.Pause(ctx); err != nil {
		plan.pauseWindow = time.Since(pauseStarted)
		cleanupSpill()
		return fmt.Errorf("pause microVM: %w", err)
	}
	plan.pauseAPI = time.Since(pauseStarted)
	klog.InfoS("firecracker sandbox paused for dump",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "spilled", plan.spilled,
		"rootfsCloned", rootfsCloned)
	dumpErr := func() error {
		if !rootfsCloned {
			copyStarted := time.Now()
			if err := copyReflinkOrCopy(rootfs, stagedRootfs); err != nil {
				return fmt.Errorf("copy instance rootfs: %w", err)
			}
			plan.rootfsCopy = time.Since(copyStarted)
		}
		dumpStarted := time.Now()
		err := client.CreateSnapshot(ctx, SnapshotCreateRequest{
			SnapshotType: "Full",
			SnapshotPath: vmstateTarget,
			MemFilePath:  memoryTarget,
		})
		plan.dumpAPI = time.Since(dumpStarted)
		if err != nil {
			return fmt.Errorf("create Firecracker snapshot: %w", err)
		}
		return nil
	}()
	// The VM resumes regardless of the dump outcome: the business-visible
	// pause window closes here.
	resumeStarted := time.Now()
	if _, resumeErr := resumeVM(ctx, client, plan.bootTimeout); resumeErr != nil {
		plan.pauseWindow = time.Since(pauseStarted)
		cleanupSpill()
		if dumpErr != nil {
			return errors.Join(dumpErr, fmt.Errorf("resume microVM after failed dump: %w", resumeErr))
		}
		return fmt.Errorf("resume microVM after snapshot: %w", resumeErr)
	}
	plan.resumeAPI = time.Since(resumeStarted)
	plan.pauseWindow = time.Since(pauseStarted)
	if dumpErr != nil {
		cleanupSpill()
		return dumpErr
	}
	klog.InfoS("firecracker sandbox resumed after dump",
		"sandboxID", plan.sandboxID, "snapshotId", plan.snapshotID, "pauseWindow", plan.pauseWindow.String(),
		"rootfsClone", plan.rootfsClone.String(), "rootfsCopy", plan.rootfsCopy.String(),
		"pauseAPI", plan.pauseAPI.String(), "dumpAPI", plan.dumpAPI.String(), "resumeAPI", plan.resumeAPI.String())
	if !plan.spilled {
		if plan.jailed {
			// Move the chroot-local dump into the staging directory: the
			// jailed VMM wrote under its own credentials, but the files are
			// regular and movable by this (privileged) driver.
			for dumped, staged := range map[string]string{
				jailedDumpVMStateName: vmstateSnapshotName,
				jailedDumpMemoryName:  memorySnapshotName,
			} {
				if err := os.Rename(filepath.Join(jailDumpDir, dumped), filepath.Join(plan.staging, staged)); err != nil {
					return fmt.Errorf("move dumped %s out of the jail root: %w", dumped, err)
				}
			}
		}
		return nil
	}

	// Spilled: relocate the dump into the publish staging OUTSIDE the pause
	// window (cross-device tmpfs→StateRoot is a plain copy). The jail's
	// spill bind persists with the sandbox and is released at its deletion.
	moveStarted := time.Now()
	for dumped, staged := range map[string]string{
		jailedDumpVMStateName: vmstateSnapshotName,
		jailedDumpMemoryName:  memorySnapshotName,
	} {
		if err := copyFile(filepath.Join(spillDir, dumped), filepath.Join(plan.staging, staged)); err != nil {
			_ = os.RemoveAll(spillDir)
			return fmt.Errorf("move spilled %s into the staging directory: %w", dumped, err)
		}
	}
	plan.spillMove = time.Since(moveStarted)
	_ = os.RemoveAll(spillDir)
	return nil
}

// vaultBundleName / policyBundleName are the staged state bundle files of a
// snapshot set. The names are owned by the shared protocol package: the
// publisher uploads and the pull layer materializes the same names, so a
// rename anywhere must go through there.
const (
	vaultBundleName  = agentprotocol.VaultBundleName
	policyBundleName = agentprotocol.PolicyBundleName
)

// stageCheckpointVault exports the subject's egress vault into the checkpoint
// staging directory (no-op without a mover). The Handler copies the file
// itself: a successful export with no file at the target simply means the
// subject held no vault; any transfer failure fails the checkpoint so the
// egress-local file survives for the retry.
func (d *Driver) stageCheckpointVault(ctx context.Context, sandboxUID, staging string) error {
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return nil //nolint:nilerr // no mover wired: state transfers disabled
	}
	if err := mover.ExportState(ctx, sandboxUID, runtimecontract.StateKindVault, filepath.Join(staging, vaultBundleName)); err != nil {
		return fmt.Errorf("export egress vault for the checkpoint bundle: %w", err)
	}
	return nil
}

// stageCheckpointPolicy exports the subject's egress-recorded network policy
// into the snapshot staging directory (no-op without a mover). Exported for
// BOTH checkpoint and template snapshots (the caller stages it outside the
// kind gate): the policy is desired state, not a secret, and a template
// clone must inherit the relayed policy. Same failure posture as the vault:
// a transfer failure fails the snapshot so the egress-local file survives
// for the retry.
func (d *Driver) stageCheckpointPolicy(ctx context.Context, sandboxUID, staging string) error {
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return nil //nolint:nilerr // no mover wired: state transfers disabled
	}
	if err := mover.ExportState(ctx, sandboxUID, runtimecontract.StateKindPolicy, filepath.Join(staging, policyBundleName)); err != nil {
		return fmt.Errorf("export egress policy for the snapshot bundle: %w", err)
	}
	return nil
}

// policyBindingHandlerOf resolves the Pool-declared handler name the
// dump-recorded policy binding must address in the manifest: the same
// handler that serves the state transfers (StateHandlerName, implemented by
// the Fastlet action manager). An unknown name ("") skips the binding track
// rather than guessing — a hardcoded name the target Pool may not declare
// would be dropped at clone-create, silently reverting the clone to the
// stale bootstrap while the resume path keeps the relayed policy.
func (d *Driver) policyBindingHandlerOf() string {
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return ""
	}
	namer, ok := mover.(interface{ StateHandlerName() string })
	if !ok {
		return ""
	}
	return namer.StateHandlerName()
}

// maxVaultBundleBytes bounds the vault bundle file buffered for provenance
// verification. The vault is decoupled from the binding-input cap, so the
// bound is its own (generous) limit; nothing larger is ever legitimate.
const maxVaultBundleBytes = 4 << 20

// maxPolicyBundleBytes bounds the policy bundle file buffered for provenance
// verification (≈1 MiB): the egress caps recorded policy files at
// MaxEncodedPolicyFileBytes (≈68 KiB), so nothing larger is ever legitimate.
const maxPolicyBundleBytes = 1 << 20

// importRestoreVault delivers a restored checkpoint's vault bundle file to
// egress — before the data plane becomes ready. Only a resumed checkpoint
// carries the file (golden-image caches never do); a nil mover is a no-op.
// The committed manifest's egressVault entry is the enforced provenance:
// an authorized-but-missing file, a read failure, a marker/size/digest
// mismatch, or an import failure all degrade the subject's vault
// fail-closed and observably — mirroring the corrupt vault file posture at
// registration. The Handler reads the bundle file itself; the driver only
// verifies it and hands over the path.
func (d *Driver) importRestoreVault(ctx context.Context, spec fastletapi.SandboxSpec, restoreRef, vmstatePath, sandboxUID string) {
	if spec.Restore == nil {
		return
	}
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return
	}
	bundleDir := filepath.Dir(vmstatePath)
	vaultPath := filepath.Join(bundleDir, vaultBundleName)

	marker, markerPresent, markerErr := vaultManifestMarker(bundleDir)
	if markerErr != nil {
		klog.ErrorS(markerErr, "read the checkpoint manifest for the vault provenance failed; the restored Sandbox vault degrades",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", vaultPath)
		return
	}

	importRestoreBundle(ctx, importPlan{
		sandboxUID: sandboxUID,
		restoreRef: restoreRef, path: vaultPath, marker: marker, markerPresent: markerPresent,
		maxBytes:         maxVaultBundleBytes,
		subject:          "vault",
		degrade:          "the restored Sandbox vault degrades",
		importIt:         func() error { return mover.ImportState(ctx, sandboxUID, runtimecontract.StateKindVault, vaultPath) },
		sentinelMissing:  errVaultBundleMissing,
		sentinelUnlisted: errVaultBundleUnlisted,
		sentinelMismatch: errVaultBundleMismatch,
	})
}

// importRestorePolicy delivers a restored checkpoint's recorded-policy bundle
// file to egress — before the data plane becomes ready, so the imported
// record is authoritative over the SET_BINDING replay that seeds the
// bootstrap binding input. Only a resumed checkpoint carries the file; a nil
// mover is a no-op. The committed manifest's policy.json files entry is the
// enforced provenance (sha256 + size, stat-first bounded): anything
// unverified is never delivered, and the restored subject keeps the
// bootstrap binding policy — the pre-transfer semantics, observably.
func (d *Driver) importRestorePolicy(ctx context.Context, spec fastletapi.SandboxSpec, restoreRef, vmstatePath, sandboxUID string) {
	if spec.Restore == nil {
		return
	}
	d.mu.RLock()
	mover := d.stateMover
	d.mu.RUnlock()
	if mover == nil {
		return
	}
	bundleDir := filepath.Dir(vmstatePath)
	policyPath := filepath.Join(bundleDir, policyBundleName)

	marker, markerPresent, markerErr := policyManifestMarker(bundleDir)
	if markerErr != nil {
		klog.ErrorS(markerErr, "read the snapshot manifest for the policy provenance failed; the restored Sandbox keeps the bootstrap binding policy",
			"sandboxID", sandboxUID, "checkpoint", restoreRef, "file", policyPath)
		return
	}

	importRestoreBundle(ctx, importPlan{
		sandboxUID: sandboxUID,
		restoreRef: restoreRef, path: policyPath, marker: marker, markerPresent: markerPresent,
		maxBytes:         maxPolicyBundleBytes,
		subject:          "policy",
		degrade:          "the restored Sandbox keeps the bootstrap binding policy",
		importIt:         func() error { return mover.ImportState(ctx, sandboxUID, runtimecontract.StateKindPolicy, policyPath) },
		sentinelMissing:  errPolicyBundleMissing,
		sentinelUnlisted: errPolicyBundleUnlisted,
		sentinelMismatch: errPolicyBundleMismatch,
	})
}

// importPlan carries one restore-side state bundle delivery: the verified
// hand-over of a manifest-provenanced bundle file to the mover.
type importPlan struct {
	sandboxUID    string
	restoreRef    string
	path          string
	marker        *fileMarker
	markerPresent bool
	maxBytes      int64
	// subject names the state slice in log lines ("vault", "policy");
	// degrade names the observable consequence of a failed delivery.
	subject string
	degrade string
	// importIt performs the handler hand-over once provenance holds (the
	// caller's closure captures the mover and the state kind).
	importIt func() error
	// sentinels mark the degradation cause in the log line.
	sentinelMissing, sentinelUnlisted, sentinelMismatch error
}

// importRestoreBundle verifies one staged bundle file against its manifest
// provenance and hands it to the mover. Every rejected posture (missing
// file, unlisted file, size/digest mismatch, read or import failure) is a
// logged degradation that never fails the restore: the control plane has
// already resumed the Sandbox, and the subject's state on the handler
// degrades fail-closed (vault) or to the binding-input fallback (policy) —
// both observable.
func importRestoreBundle(ctx context.Context, plan importPlan) {
	info, statErr := os.Stat(plan.path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			if plan.markerPresent {
				// The manifest authorizes state but the bundle file is
				// gone (partial cache damage): never a silent skip.
				klog.ErrorS(plan.sentinelMissing, "snapshot manifest authorizes "+plan.subject+" but the bundle file is missing; "+plan.degrade,
					"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
			}
			return //nolint:nilerr // no manifest-listed state: nothing to import
		}
		klog.ErrorS(statErr, "read bundled egress "+plan.subject+" failed; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // already logged
	}
	if !plan.markerPresent {
		klog.ErrorS(plan.sentinelUnlisted, "bundled "+plan.subject+" file is not listed by the snapshot manifest; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // unverified state bytes are never delivered
	}
	// Stat-first bounds: the whole file is never buffered before the size
	// contract holds, so a corrupted/replaced file cannot grow the driver's
	// memory arbitrarily.
	if info.Size() != plan.marker.SizeBytes || info.Size() > plan.maxBytes {
		klog.ErrorS(plan.sentinelMismatch, "bundled "+plan.subject+" size mismatches the manifest entry; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path,
			"stagedBytes", info.Size(), "manifestBytes", plan.marker.SizeBytes)
		return //nolint:nilerr // already logged
	}
	bundleData, readErr := os.ReadFile(plan.path)
	if readErr != nil {
		klog.ErrorS(readErr, "read bundled egress "+plan.subject+" failed; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // already logged
	}
	if plan.marker.SHA256 != artifacts.SHA256Of(bundleData) {
		klog.ErrorS(plan.sentinelMismatch, "bundled "+plan.subject+" digest mismatches the manifest entry; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef, "file", plan.path)
		return //nolint:nilerr // unverified state bytes are never delivered
	}
	if importErr := plan.importIt(); importErr != nil {
		klog.ErrorS(importErr, "egress "+plan.subject+" import failed; "+plan.degrade,
			"sandboxID", plan.sandboxUID, "checkpoint", plan.restoreRef)
	}
}

// fileMarker is a manifest provenance entry: a sha256 + size pair that
// authorizes exactly one bundle file content.
type fileMarker struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// sentinel degradation causes of the restore import; the log line carries
// the specifics.
var (
	errVaultBundleMissing  = errors.New("vault bundle file missing")
	errVaultBundleUnlisted = errors.New("vault bundle file not listed by the manifest")
	errVaultBundleMismatch = errors.New("vault bundle does not match the manifest entry")

	errPolicyBundleMissing  = errors.New("policy bundle file missing")
	errPolicyBundleUnlisted = errors.New("policy bundle file not listed by the manifest")
	errPolicyBundleMismatch = errors.New("policy bundle does not match the manifest entry")
)

// vaultManifestMarker parses the committed checkpoint manifest's egressVault
// entry. A manifest without the marker authorizes nothing.
func vaultManifestMarker(bundleDir string) (*fileMarker, bool, error) {
	payload, err := os.ReadFile(filepath.Join(bundleDir, snapshotManifestName))
	if err != nil {
		return nil, false, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	var document struct {
		EgressVault *fileMarker `json:"egressVault"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, false, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	return document.EgressVault, document.EgressVault != nil, nil
}

// policyManifestMarker returns the committed snapshot manifest's policy.json
// files entry: the recorded policy needs no dedicated top-level marker —
// the checksummed file list IS the provenance. A manifest without the entry
// authorizes nothing.
func policyManifestMarker(bundleDir string) (*fileMarker, bool, error) {
	payload, err := os.ReadFile(filepath.Join(bundleDir, snapshotManifestName))
	if err != nil {
		return nil, false, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var document struct {
		Files map[string]fileMarker `json:"files"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, false, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	marker, listed := document.Files[policyBundleName]
	if !listed {
		return nil, false, nil
	}
	return &marker, true, nil
}

// stagedBundleFiles validates the optional state bundle files of the staged
// set and returns the full bundle file list. Mirrors the publish side
// (PublishImage): only absence skips a file, any other stat failure fails
// the assembly — a transient error here would otherwise produce a manifest
// that omits a staged file while the publish still uploads the orphaned
// bytes. The staged policy content is read stat-first (driver-staged, not
// guest-written) for the actionBindings track; policyBindingOK reports
// whether the dump may ride that track, which requires a resolvable
// transfer handler (a guess could be undeclared by the target Pool and
// silently dropped at clone-create) and CRD-fitting content — either miss
// keeps the files track only, and the resume import still carries the
// relayed policy.
func stagedBundleFiles(staging, policyHandler string) (bundleFiles []string, policyBindingInput string, policyBindingOK bool, resultErr error) {
	bundleFiles = []string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName}
	for _, bundleName := range []string{vaultBundleName, policyBundleName} {
		info, statErr := os.Stat(filepath.Join(staging, bundleName))
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, "", false, fmt.Errorf("stat staged %s: %w", bundleName, statErr)
		}
		bundleFiles = append(bundleFiles, bundleName)
		if bundleName != policyBundleName {
			continue
		}
		// Stat-first bound on the buffered content, mirroring the restore
		// import: the egress caps recorded policy files well below this, so
		// a larger file is corrupt and is never buffered.
		if info.Size() > maxPolicyBundleBytes {
			klog.ErrorS(errPolicyBundleMismatch, "staged policy file exceeds the bundle bound; publishing it unverified (files track only)",
				"file", filepath.Join(staging, bundleName), "stagedBytes", info.Size(), "maxBytes", int64(maxPolicyBundleBytes))
			continue
		}
		policyContent, readErr := os.ReadFile(filepath.Join(staging, policyBundleName))
		if readErr != nil {
			return nil, "", false, fmt.Errorf("read staged %s: %w", policyBundleName, readErr)
		}
		if policyHandler == "" {
			klog.InfoS("staged policy file not recorded as a manifest binding: the transfer handler name is unknown; the resume import still carries it",
				"file", filepath.Join(staging, policyBundleName), "stagedBytes", info.Size())
			continue
		}
		if len(policyContent) > runtimecontract.MaxPolicyBindingInputBytes {
			klog.InfoS("staged policy file exceeds the CRD binding-input bound; not recorded as a manifest binding (the resume import still carries it)",
				"file", filepath.Join(staging, policyBundleName), "stagedBytes", info.Size(),
				"bindingInputBound", runtimecontract.MaxPolicyBindingInputBytes)
			continue
		}
		policyBindingInput, policyBindingOK = string(policyContent), true
	}
	return bundleFiles, policyBindingInput, policyBindingOK, nil
}

// recordActionBindings writes THIS dump's actionBindings record into the
// manifest: the CRD bindings of the snapshot request, with an eligible
// staged policy dump overriding the transfer handler's entry (the relayed
// record is authoritative over the CRD replay — relay policy mutations are
// invisible to the CRD), the remaining CRD bindings staying the graceful
// fallback for handlers that skipped the transfer. The source manifest's
// inherited record is dropped unconditionally first: stale bindings must
// never ride forward — they would publish the previous dump's policy
// (possibly relay-DELETEd since) as this snapshot's authoritative record.
func recordActionBindings(document map[string]any, actionBindings []runtimecontract.SnapshotActionBinding, policyHandler, policyBindingInput string, policyBindingOK bool) {
	delete(document, "actionBindings")
	if len(actionBindings) == 0 && !policyBindingOK {
		return
	}
	recorded := make([]map[string]string, 0, len(actionBindings)+1)
	for _, binding := range actionBindings {
		if policyBindingOK && binding.Handler == policyHandler {
			continue
		}
		recorded = append(recorded, map[string]string{"handler": binding.Handler, "input": binding.Input})
	}
	if policyBindingOK {
		recorded = append(recorded, map[string]string{"handler": policyHandler, "input": policyBindingInput})
	}
	if len(recorded) > 0 {
		document["actionBindings"] = recorded
	}
}

// assembleSnapshotManifest builds the restore-compatible manifest of the
// dumped set. lineage, machine (vcpu/memory), guestNetwork, and
// compatibility ride forward from the SOURCE manifest verbatim — the dumped
// vmstate carries the source snapshot's CPU state, so its compatibility
// identity is the source's. Only files, machine.rootfs, format, and
// validation describe this dump. Returns the total logical artifact size.
// A staged vault bundle file joins files, SHA256SUMS, and the egressVault
// manifest marker. A staged policy file is recorded dual-track (see
// stagedBundleFiles / recordActionBindings).
func assembleSnapshotManifest(stateRoot, staging, sandboxDir string, actionBindings []runtimecontract.SnapshotActionBinding, policyHandler string) (int64, error) {
	state, err := loadState(sandboxDir)
	if err != nil {
		return 0, err
	}
	// The lineage manifest of the dump: the golden image manifest of a fresh
	// boot, or the cached checkpoint manifest when the Sandbox was itself
	// resumed from a checkpoint (the checkpoint carries every lineage field
	// forward, and the original image may not exist on this node at all).
	sourceRef, err := restoreReference(state.Config.Spec)
	if err != nil {
		return 0, err
	}
	document, err := readSourceManifest(stateRoot, sourceRef)
	if err != nil {
		return 0, err
	}

	bundleFiles, policyBindingInput, policyBindingOK, err := stagedBundleFiles(staging, policyHandler)
	if err != nil {
		return 0, err
	}

	cache := map[string]string{}
	files := map[string]any{}
	var sizeBytes, rootfsSize int64
	for _, name := range bundleFiles {
		entry, err := artifacts.FileEntry(filepath.Join(staging, name), cache)
		if err != nil {
			return 0, fmt.Errorf("checksum %s: %w", name, err)
		}
		files[name] = entry
		if entrySize, ok := entry["sizeBytes"].(int64); ok {
			sizeBytes += entrySize
			if name == publishedRootfsName {
				rootfsSize = entrySize
			}
		}
	}
	if err := artifacts.WriteSHA256SUMS(staging, bundleFiles, cache); err != nil {
		return 0, err
	}

	document["schemaVersion"] = 1
	document["runtime"] = "firecracker"
	// The lineage object rides forward from the source manifest; only the
	// image reference is rewritten to the one this Sandbox booted from (the
	// imageDigest stays the original build's anchor).
	lineage, ok := document["lineage"].(map[string]any)
	if !ok {
		lineage = map[string]any{}
		document["lineage"] = lineage
	}
	lineage["image"] = state.Config.Spec.Image
	// compatibility rides forward untouched — see assembleSnapshotManifest.
	document["files"] = files
	// machine.rootfs reflects this dump's actual rootfs size; vcpu/memory
	// ride forward from the source manifest untouched.
	if machine, ok := document["machine"].(map[string]any); ok {
		machine["rootfs"] = fmt.Sprintf("%dGi", artifacts.SizeGiB(rootfsSize))
	}
	document["format"] = "native"
	document["validation"] = map[string]any{"booted": true, "restored": false}
	// The actionBindings record and the egress vault marker describe THIS
	// dump: a source checkpoint's inherited entries must never ride forward
	// (see recordActionBindings) — and a stale vault marker would claim
	// vault state this bundle does not carry and publish the previous
	// subject's vault fingerprint into shared, world-readable template
	// manifests.
	recordActionBindings(document, actionBindings, policyHandler, policyBindingInput, policyBindingOK)
	delete(document, "egressVault")
	if vaultEntry, ok := files[vaultBundleName]; ok {
		document["egressVault"] = vaultEntry
	}

	manifestBytes, err := artifacts.MarshalManifest(document)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(staging, snapshotManifestName), manifestBytes, 0o644); err != nil { //nolint:gosec // published snapshot manifest, world-readable by design
		return 0, err
	}
	return sizeBytes, nil
}

// readSourceManifest loads the cached manifest of the image the Sandbox
// boots from. machine and guestNetwork are required: restore validation
// rejects a snapshot manifest without them, so a hand-seeded cache cannot
// produce a restorable snapshot.
func readSourceManifest(stateRoot, image string) (map[string]any, error) {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		return nil, fmt.Errorf("read source image manifest: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("decode source image manifest: %w", err)
	}
	for _, key := range []string{"machine", "guestNetwork"} {
		if _, ok := document[key]; !ok {
			return nil, fmt.Errorf("%w: source image manifest carries no %s; a restorable snapshot cannot be published", ErrInvalidConfig, key)
		}
	}
	return document, nil
}
