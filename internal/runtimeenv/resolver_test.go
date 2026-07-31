package runtimeenv

import (
	"bytes"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"

	"github.com/stretchr/testify/require"
)

func TestResolveDefaultProducesCompleteImmutablePlan(t *testing.T) {
	plan, err := ResolveDefault(runtimecatalog.Builtin(), apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, DefaultEnvironment, plan.Environment)
	require.Equal(t, "k8s.io", plan.Profile.ContainerdNamespace())
	require.Equal(t, "/run/containerd/containerd.sock", plan.Containerd.Socket)
	require.Equal(t, "/var/lib/kubelet", plan.Kubelet.Root)
	require.NotEmpty(t, plan.Profile.ProfileHash)
	require.Equal(t, "sha256:"+plan.Profile.ProfileHash, plan.Revision)
	require.True(t, hasHostPath(plan.Profile, "/run/containerd"))
	require.True(t, hasHostPath(plan.Profile, "/var/lib/containerd"))

	payload, err := plan.Marshal()
	require.NoError(t, err)
	decoded, err := DecodePlan(bytes.NewReader(payload))
	require.NoError(t, err)
	require.Equal(t, plan, decoded)
}

func TestParseMovesRuntimeToConfiguredEnvironment(t *testing.T) {
	config, err := Parse([]byte(`
environments:
  secure:
    nodeSelector:
      example.com/secure-runtime: "true"
    containerd:
      namespace: tenant-runtime
      root: /srv/containerd/root
    kubelet:
      root: /srv/kubelet
    runtimes:
      container:
        handler: io.containerd.custom.v2
`))
	require.NoError(t, err)
	_, stillDefault := config.Environments[DefaultEnvironment].Runtimes[apiv1alpha2.RuntimeContainer]
	require.False(t, stillDefault)

	plan, err := Resolve(runtimecatalog.Builtin(), config, apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	require.Equal(t, "secure", plan.Environment)
	require.Equal(t, "tenant-runtime", plan.Profile.Containerd.Namespace)
	require.Equal(t, "io.containerd.custom.v2", plan.Profile.Containerd.Handler)
	require.Equal(t, "/srv/kubelet", plan.Kubelet.Root)
	require.Equal(t, "true", plan.Profile.Deployment.NodeSelector["example.com/secure-runtime"])
	require.True(t, hasHostPath(plan.Profile, "/srv/containerd/root"))
}

func TestParseRejectsInvalidPlatformPaths(t *testing.T) {
	_, err := Parse([]byte(`
environments:
  invalid:
    containerd:
      root: relative/root
    runtimes:
      container: {}
`))
	require.ErrorContains(t, err, "containerd.root must be an absolute path")
}

func TestDecodePlanRejectsMutation(t *testing.T) {
	plan, err := ResolveDefault(runtimecatalog.Builtin(), apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	plan.Containerd.Namespace = "other"
	payload, err := plan.Marshal()
	require.NoError(t, err)
	_, err = DecodePlan(bytes.NewReader(payload))
	require.ErrorContains(t, err, "does not match payload")
}

func hasHostPath(profile runtimecatalog.RuntimeProfile, hostPath string) bool {
	for _, requirement := range profile.Deployment.HostPaths {
		if requirement.HostPath == hostPath {
			return true
		}
	}
	return false
}
