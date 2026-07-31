package janitor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizedContainerdNamespaces(t *testing.T) {
	require.Equal(t, []string{"k8s.io"}, normalizedContainerdNamespaces(nil))
	require.Equal(t, []string{"k8s.io", "default"}, normalizedContainerdNamespaces([]string{
		"k8s.io", "default", "k8s.io", "",
	}))
}

func TestContainerdNamespaceIsPartOfCleanupFence(t *testing.T) {
	expected := ResourceIdentity{
		Backend: BackendContainerd, ResourceID: "sandbox-a", ContainerdNamespace: "default",
		FastletPodUID: "fastlet-a", SandboxUID: "sandbox-uid", InstanceGeneration: 1, AssignmentAttempt: 1,
	}
	current := expected
	require.True(t, sameResourceFence(expected, current))

	current.ContainerdNamespace = "k8s.io"
	require.False(t, sameResourceFence(expected, current))
}
