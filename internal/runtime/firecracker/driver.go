package firecracker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// reasonUnimplemented is the capability gate reason that keeps the driver
// fail-closed until the Firecracker VM lifecycle implementation lands.
const reasonUnimplemented = "FirecrackerDriverUnimplemented"

// errLifecycleUnimplemented is returned by every VM lifecycle operation while
// the driver is still a framework-only skeleton.
var errLifecycleUnimplemented = errors.New("firecracker runtime driver lifecycle is not implemented")

// Driver is the on-demand Firecracker microVM runtime driver. One Firecracker
// process is launched per Sandbox create request with an immutable identity;
// no VM or sidecar is pre-warmed. The VM lifecycle implementation is pending
// behind the FirecrackerDriverUnimplemented capability gate.
type Driver struct {
	mu          sync.RWMutex
	profile     runtimecatalog.RuntimeProfile
	config      runtimecatalog.FirecrackerConfig
	namespace   string
	initialized bool
}

// New validates that the resolved runtime profile carries the private
// Firecracker configuration and returns the driver skeleton.
func New(profile runtimecatalog.RuntimeProfile) (*Driver, error) {
	if profile.Firecracker == nil {
		return nil, fmt.Errorf("firecracker runtime profile %q has no private configuration", profile.Name)
	}
	return &Driver{profile: profile, config: *profile.Firecracker}, nil
}

// Initialize validates the Firecracker boot configuration. The concrete boot
// preparation (kernel/rootfs readiness, API socket placement) is part of the
// lifecycle implementation.
func (d *Driver) Initialize(_ context.Context, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.initialized {
		return nil
	}
	if err := validateConfig(d.config); err != nil {
		return err
	}
	d.initialized = true
	return nil
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

// ProbeCapabilities fails closed until the lifecycle implementation reports a
// concrete runtime capability report.
func (d *Driver) ProbeCapabilities(_ context.Context) CapabilityReport {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return CapabilityReport{
		Runtime: d.profile.Name, ProfileHash: d.profile.ProfileHash,
		State: runtimecatalog.CapabilityUnsupported, Reason: reasonUnimplemented,
		Message: "firecracker runtime driver lifecycle is not implemented yet",
	}
}

// EnsureSandbox boots one Firecracker microVM on demand for the Sandbox. The
// boot implementation is pending.
func (d *Driver) EnsureSandbox(_ context.Context, _ *fastletapi.SandboxSpec) (*SandboxMetadata, error) {
	return nil, errLifecycleUnimplemented
}

// InspectSandbox reports the metadata of a managed Firecracker microVM.
func (d *Driver) InspectSandbox(_ context.Context, _ string) (*SandboxMetadata, error) {
	return nil, errLifecycleUnimplemented
}

// DeleteSandbox stops and removes the Firecracker microVM of the Sandbox.
func (d *Driver) DeleteSandbox(_ context.Context, _ string) error {
	return errLifecycleUnimplemented
}

// ListManagedSandboxes lists the Sandboxes owned by this Fastlet.
func (d *Driver) ListManagedSandboxes(_ context.Context) ([]*SandboxMetadata, error) {
	return nil, errLifecycleUnimplemented
}

// RecoverRuntimeResources reattaches Fastlet to VMs that survived a Fastlet
// restart.
func (d *Driver) RecoverRuntimeResources(_ context.Context, _ []*SandboxMetadata) error {
	return errLifecycleUnimplemented
}

// GetAccessDescriptor returns the data-plane access descriptor for a Sandbox.
func (d *Driver) GetAccessDescriptor(_ string) (dataplane.AccessDescriptor, error) {
	return dataplane.AccessDescriptor{}, fmt.Errorf("%w: firecracker access descriptors are not implemented", ErrNetworkUnavailable)
}

// ListImages lists the images cached by the Firecracker artifact cache.
func (d *Driver) ListImages(_ context.Context) ([]string, error) {
	return nil, errLifecycleUnimplemented
}

// PullImage caches an image for later Firecracker rootfs preparation.
func (d *Driver) PullImage(_ context.Context, _ string) error {
	return errLifecycleUnimplemented
}

// Close resets the driver state.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initialized = false
	return nil
}

var (
	_ RuntimeDriver            = (*Driver)(nil)
	_ RuntimeArtifactCache     = (*Driver)(nil)
	_ RuntimeResourceRecoverer = (*Driver)(nil)
	_ AccessDescriptorProvider = (*Driver)(nil)
)
