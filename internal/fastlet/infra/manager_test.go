package infra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"

	"github.com/stretchr/testify/require"
)

func TestManagerPreparesInlinePlanAndSupervisorOnce(t *testing.T) {
	manager, resolver := testManager(t, apiv1alpha2.RuntimeContainer)

	require.NoError(t, manager.Prepare(context.Background()))
	plan, err := manager.Plan()
	require.NoError(t, err)
	require.NotNil(t, plan.Supervisor)
	require.Len(t, plan.Components, 1)
	require.Len(t, plan.Components[0].Mappings, 2)
	require.NoError(t, manager.Prepare(context.Background()))
	require.Equal(t, 1, resolver.calls)
	require.Len(t, manager.ArtifactReferences(), 2)
}

func TestManagerRetriesTransientArtifactPreparationFailure(t *testing.T) {
	manager, resolver := testManager(t, apiv1alpha2.RuntimeContainer)
	resolver.failFirst = true

	require.Error(t, manager.Prepare(context.Background()))
	require.NoError(t, manager.Prepare(context.Background()))
	_, err := manager.Plan()
	require.NoError(t, err)
	require.Equal(t, 2, resolver.calls)
}

func TestManagerPreparesTunnelForBoxLite(t *testing.T) {
	manager, _ := testManager(t, apiv1alpha2.RuntimeBoxLite)
	require.NoError(t, manager.Prepare(context.Background()))
	plan, err := manager.Plan()
	require.NoError(t, err)
	require.Equal(t, runtimecatalog.InfraDeliveryArtifactVolume, plan.Components[0].Plan.Delivery)
	require.NotNil(t, plan.Supervisor)
	require.NotNil(t, plan.Tunnel)
	require.FileExists(t, plan.Tunnel.PodPath)
	require.Len(t, manager.ArtifactReferences(), 3)
}

type testResolver struct {
	source    PreparedSource
	calls     int
	failFirst bool
}

func (r *testResolver) Prepare(context.Context, infracatalog.ArtifactSource, *ArtifactStore) (PreparedSource, error) {
	r.calls++
	if r.failFirst && r.calls == 1 {
		return PreparedSource{}, errors.New("temporary registry failure")
	}
	return r.source, nil
}

func testManager(t *testing.T, runtimeName apiv1alpha2.RuntimeName) (*Manager, *testResolver) {
	t.Helper()
	root := t.TempDir()
	podRoot := filepath.Join(root, "pod")
	hostRoot := filepath.Join(root, "host")
	sourcePodRoot := filepath.Join(podRoot, "source")
	sourceHostRoot := filepath.Join(hostRoot, "source")
	require.NoError(t, os.MkdirAll(filepath.Join(sourcePodRoot, "config"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sourcePodRoot, "execd"), []byte("execd"), 0555))
	require.NoError(t, os.WriteFile(filepath.Join(sourcePodRoot, "config", "default.yaml"), []byte("enabled: true"), 0444))
	store, err := NewArtifactStore(podRoot, hostRoot)
	require.NoError(t, err)
	sandboxInit := filepath.Join(root, "sandbox-init")
	require.NoError(t, os.WriteFile(sandboxInit, []byte("sandbox-init"), 0555))
	sandboxTunnel := filepath.Join(root, "sandbox-tunnel")
	require.NoError(t, os.WriteFile(sandboxTunnel, []byte("sandbox-tunnel"), 0555))
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(runtimeName)
	require.NoError(t, err)
	plan := testInfraPlan(t, runtimeProfile)
	resolver := &testResolver{source: PreparedSource{
		Digest: plan.Components[0].Artifact.Source.Digest, PodRoot: sourcePodRoot, HostRoot: sourceHostRoot,
	}}
	manager, err := NewManagerWithConfig(ManagerConfig{
		Plan: plan, RuntimeProfile: runtimeProfile, Store: store, Resolver: resolver,
		SandboxInitPath: sandboxInit, SandboxTunnelPath: sandboxTunnel,
	})
	require.NoError(t, err)
	return manager, resolver
}

func testInfraPlan(t *testing.T, runtimeProfile runtimecatalog.RuntimeProfile) infracatalog.Plan {
	t.Helper()
	component := apiv1alpha2.InfraComponent{
		Name: "execd",
		Artifact: apiv1alpha2.InfraArtifact{
			Source: apiv1alpha2.InfraArtifactSource{Image: &apiv1alpha2.InfraArtifactImage{
				Reference: "registry.example/execd@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
			Mappings: []apiv1alpha2.InfraArtifactMapping{
				{SourcePath: "/execd", TargetPath: "/.fast/components/execd/execd"},
				{SourcePath: "/config", TargetPath: "/.fast/components/execd/config"},
			},
		},
		Process: apiv1alpha2.InfraProcess{
			Command: []string{"/.fast/components/execd/execd", "--port", "44772"},
			Env:     map[string]string{"MODE": "production"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{
				HTTPGet: &apiv1alpha2.InfraHTTPGet{Path: "/ping"},
			},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}
	plan, err := infracatalog.Compile([]apiv1alpha2.InfraComponent{component}, runtimeProfile)
	require.NoError(t, err)
	return plan
}
