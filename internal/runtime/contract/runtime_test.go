package contract

import (
	"testing"

	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

func TestSameRuntimeIdentityIncludesEveryFence(t *testing.T) {
	existing := fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-a", Namespace: "default", Name: "sandbox-a", FastletPodUID: "pod-a",
		InstanceGeneration: 2, RuntimeInstanceID: "runtime-a", AssignmentAttempt: 3, RouteGeneration: 4,
	}
	require.True(t, SameRuntimeIdentity(existing, existing))

	for name, mutate := range map[string]func(*fastletapi.SandboxIdentity){
		"Sandbox UID":         func(identity *fastletapi.SandboxIdentity) { identity.SandboxUID = "sandbox-b" },
		"namespace":           func(identity *fastletapi.SandboxIdentity) { identity.Namespace = "other" },
		"name":                func(identity *fastletapi.SandboxIdentity) { identity.Name = "other" },
		"Fastlet Pod":         func(identity *fastletapi.SandboxIdentity) { identity.FastletPodUID = "pod-b" },
		"instance generation": func(identity *fastletapi.SandboxIdentity) { identity.InstanceGeneration++ },
		"runtime instance":    func(identity *fastletapi.SandboxIdentity) { identity.RuntimeInstanceID = "runtime-b" },
		"assignment attempt":  func(identity *fastletapi.SandboxIdentity) { identity.AssignmentAttempt++ },
		"route generation":    func(identity *fastletapi.SandboxIdentity) { identity.RouteGeneration++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := existing
			mutate(&changed)
			require.False(t, SameRuntimeIdentity(existing, changed))
		})
	}
}
