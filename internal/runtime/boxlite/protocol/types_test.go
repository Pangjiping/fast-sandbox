package protocol

import (
	"encoding/json"
	"testing"

	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

func TestEnsureRequestSerializesExplicitRuntimeBoundaries(t *testing.T) {
	request := EnsureRequest{
		FastletNamespace: "fastlet-system",
		Input: fastletapi.EnsureSandboxInput{
			RequestID: "invocation-a",
			Sandbox: fastletapi.RuntimeSandboxConfig{
				Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", Namespace: "tenant-a", Name: "sandbox-a"},
				Spec:     fastletapi.SandboxSpec{Image: "alpine:latest"},
			},
		},
		TunnelGuestPort: 19090,
	}
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(payload, &object))
	require.Equal(t, "fastlet-system", object["fastletNamespace"])
	require.NotContains(t, object, "namespace")
	require.NotContains(t, object, "sandbox")

	input := object["input"].(map[string]any)
	require.Equal(t, "invocation-a", input["requestId"])
	sandbox := input["sandbox"].(map[string]any)
	require.Contains(t, sandbox, "identity")
	require.Contains(t, sandbox, "spec")
	require.NotContains(t, sandbox, "allocation")
	require.NotContains(t, sandbox, "network")
}
