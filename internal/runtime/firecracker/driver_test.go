package firecracker

import (
	"context"
	"errors"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"

	"github.com/stretchr/testify/require"
)

func TestNewRequiresProfileConfig(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	require.NotNil(t, profile.Firecracker)

	profile.Firecracker = nil
	_, err = New(profile)
	require.Error(t, err)
}

func TestInitializeValidatesBootConfig(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)

	driver, err := New(profile)
	require.NoError(t, err)
	require.NoError(t, driver.Initialize(context.Background(), ""))
	require.NoError(t, driver.Initialize(context.Background(), ""))

	broken, err := New(profile)
	require.NoError(t, err)
	broken.config.BinaryPath = ""
	require.ErrorIs(t, broken.Initialize(context.Background(), ""), ErrInvalidConfig)

	invalid, err := New(profile)
	require.NoError(t, err)
	invalid.config.BootTimeoutSeconds = 0
	require.ErrorIs(t, invalid.Initialize(context.Background(), ""), ErrInvalidConfig)
}

func TestProbeCapabilitiesFailsClosed(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	driver, err := New(profile)
	require.NoError(t, err)

	report := driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityUnsupported, report.State)
	require.Equal(t, "FirecrackerDriverUnimplemented", report.Reason)
	require.False(t, report.Ready())
}

func TestLifecycleOperationsFailClosed(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	driver, err := New(profile)
	require.NoError(t, err)
	require.NoError(t, driver.Initialize(context.Background(), ""))

	ctx := context.Background()
	_, err = driver.EnsureSandbox(ctx, nil)
	require.True(t, errors.Is(err, errLifecycleUnimplemented))
	_, err = driver.InspectSandbox(ctx, "sandbox-1")
	require.True(t, errors.Is(err, errLifecycleUnimplemented))
	require.True(t, errors.Is(driver.DeleteSandbox(ctx, "sandbox-1"), errLifecycleUnimplemented))
	_, err = driver.ListManagedSandboxes(ctx)
	require.True(t, errors.Is(err, errLifecycleUnimplemented))
	require.True(t, errors.Is(driver.RecoverRuntimeResources(ctx, nil), errLifecycleUnimplemented))
	_, err = driver.GetAccessDescriptor("sandbox-1")
	require.True(t, errors.Is(err, ErrNetworkUnavailable))
	_, err = driver.ListImages(ctx)
	require.True(t, errors.Is(err, errLifecycleUnimplemented))
	require.True(t, errors.Is(driver.PullImage(ctx, "example.com/image:latest"), errLifecycleUnimplemented))
}

func TestCloseResetsDriver(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	driver, err := New(profile)
	require.NoError(t, err)
	require.NoError(t, driver.Initialize(context.Background(), ""))

	require.NoError(t, driver.Close())
	require.NoError(t, driver.Initialize(context.Background(), ""))
}
