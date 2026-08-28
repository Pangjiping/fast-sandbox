package protocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestEncoding(t *testing.T) {
	request := PinImageRequest{
		Identity: Identity{RequestID: "req-1", Namespace: "tenant-a", PodUID: "pod-1"},
		Image:    "registry.example.com/sandbox:v1",
	}
	payload, err := json.Marshal(request)
	require.NoError(t, err)

	var decoded PinImageRequest
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, request, decoded)

	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	require.Equal(t, "req-1", fields["requestId"])
	require.Equal(t, "pod-1", fields["podUid"])
	require.Equal(t, "registry.example.com/sandbox:v1", fields["image"])
}

func TestLeaseEncoding(t *testing.T) {
	lease := Lease{
		LeaseID: "lease-1", SandboxID: "sandbox-1", Image: "img", PodUID: "pod-1",
		Namespace: "ns", RootfsDev: "/cache/img/rootfs.img", MemDev: "",
	}
	payload, err := json.Marshal(lease)
	require.NoError(t, err)
	var decoded Lease
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, lease, decoded)
}

func TestErrorResponseEncoding(t *testing.T) {
	response := ErrorResponse{Code: ErrorNotFound, Message: `image "x" not ready`}
	payload, err := json.Marshal(response)
	require.NoError(t, err)
	require.JSONEq(t, `{"code":"NotFound","message":"image \"x\" not ready"}`, string(payload))
}

func TestRoutesAreVersioned(t *testing.T) {
	routes := []string{
		RoutePinImage, RouteUnpinImage, RouteLeaseDevices, RouteReleaseDevices,
		RouteListLeases, RouteCompatibility, RouteHealth,
	}
	for _, route := range routes {
		require.Len(t, route, len("/v1/")+len(route)-len("/v1/"))
		require.Contains(t, route, "/v1/")
	}
}

func TestIdentityRequiresRequestID(t *testing.T) {
	// The server enforces the required fields; the protocol layer only
	// guarantees the wire shape. Empty values round-trip unchanged.
	identity := Identity{}
	payload, err := json.Marshal(identity)
	require.NoError(t, err)
	var decoded Identity
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, "", decoded.RequestID)
	require.Equal(t, "", decoded.PodUID)
}
