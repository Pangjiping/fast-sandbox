package infra

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/sandbox/supervisor"

	"github.com/stretchr/testify/require"
)

func TestPrepareAndRecoverInstanceUsesFencedPrivateConfigWithoutComponentToken(t *testing.T) {
	manager, _ := testManager(t, apiv1alpha2.RuntimeContainer)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &fastletapi.RuntimeSandboxSpec{
		SandboxSpec: fastletapi.SandboxSpec{InfraRevision: manager.Revision()},
		SandboxID:   "uid-a", InstanceGeneration: 2, AssignmentAttempt: 3,
	}
	instance, err := manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, instance.WrapperRequired)
	require.Len(t, instance.Services, 1)
	require.Len(t, instance.Mounts, 4, "sandbox-init, two artifact mappings, and private config")
	require.FileExists(t, instance.ConfigPodPath)

	configFile, err := os.Open(instance.ConfigPodPath)
	require.NoError(t, err)
	var initConfig supervisor.Config
	require.NoError(t, json.NewDecoder(configFile).Decode(&initConfig))
	require.NoError(t, configFile.Close())
	require.Len(t, initConfig.Components, 1)
	require.Equal(t, "production", initConfig.Components[0].Env["MODE"])
	require.NotContains(t, initConfig.Components[0].Env, "EXECD_ACCESS_TOKEN")
	require.NotContains(t, initConfig.Components[0].Env, "FAST_SANDBOX_INTERNAL_TOKEN")
	info, err := os.Stat(instance.ConfigPodPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0400), info.Mode().Perm())

	recovered, err := manager.RecoverInstance(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, instance, recovered)
	stale := *spec
	stale.AssignmentAttempt++
	_, err = manager.RecoverInstance(context.Background(), &stale)
	require.Error(t, err)
}

func TestPrepareInstanceRejectsWrongImmutableRevision(t *testing.T) {
	manager, _ := testManager(t, apiv1alpha2.RuntimeContainer)
	require.NoError(t, manager.Prepare(context.Background()))
	_, err := manager.PrepareInstance(context.Background(), &fastletapi.RuntimeSandboxSpec{
		SandboxSpec: fastletapi.SandboxSpec{InfraRevision: "sha256:stale"},
		SandboxID:   "uid-a", InstanceGeneration: 1, AssignmentAttempt: 1,
	})
	require.ErrorContains(t, err, "revision")
}
