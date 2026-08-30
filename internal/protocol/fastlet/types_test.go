package fastlet

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateRequestSerializesIdentityDesiredSpecAndRequestMetadataOnce(t *testing.T) {
	request := CreateSandboxRequest{
		RequestID: "request-a",
		Identity: SandboxIdentity{
			SandboxUID: "uid-a", Namespace: "tenant-a", Name: "sandbox-a",
			InstanceGeneration: 2, RuntimeInstanceID: "runtime-a", AssignmentAttempt: 3,
			RouteGeneration: 4, FastletPodUID: "pod-a",
		},
		Sandbox: SandboxSpec{Image: "alpine:latest", CPU: "500m", Memory: "256Mi"},
		ActionBindings: []ActionBindingInput{{Handler: "egress", Input: "allow: example.com"}},
	}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.Equal(t, "request-a", object["requestId"])

	identity := object["identity"].(map[string]any)
	require.Equal(t, "uid-a", identity["sandboxUid"])
	require.Equal(t, "tenant-a", identity["namespace"])
	require.Equal(t, "sandbox-a", identity["name"])
	require.NotContains(t, identity, "requestId")
	require.NotContains(t, identity, "image")

	desired := object["sandbox"].(map[string]any)
	require.Equal(t, "alpine:latest", desired["image"])
	for _, key := range []string{"sandboxUid", "sandboxId", "namespace", "name", "requestId", "instanceGeneration", "runtimeInstanceId", "assignmentAttempt", "routeGeneration", "fastletPodUid"} {
		require.NotContains(t, desired, key)
	}

	binding := object["actionBindings"].([]any)[0].(map[string]any)
	require.Equal(t, map[string]any{"handler": "egress", "input": "allow: example.com"}, binding)
}

func TestHeartbeatUsesAdmissionAndCacheAsSingleAuthorities(t *testing.T) {
	encoded, err := json.Marshal(HeartbeatResponse{
		FastletStatus: FastletStatus{RuntimeProfileHash: "runtime-a", Admission: AdmissionStatus{Capacity: 8, Running: 2, Used: 2}},
		Sequence: 7,
		Cache: CacheSnapshot{Epoch: "epoch-a", Revision: 9, Full: true, Complete: true, Images: []string{"alpine:latest"}},
	})
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.NotContains(t, object, "capacity")
	require.NotContains(t, object, "observedAt")
	require.NotContains(t, object, "diagnostics")
	require.NotContains(t, object, "images")
	require.Equal(t, float64(8), object["admission"].(map[string]any)["capacity"])
	require.Equal(t, []any{"alpine:latest"}, object["cache"].(map[string]any)["images"])
}
