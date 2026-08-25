package sandboxclient

import (
	"context"
	"testing"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type fakeEndpointControl struct {
	resolveRequest *fastpathv2.ResolveEndpointRequest
}

func (c *fakeEndpointControl) ResolveEndpoint(_ context.Context, request *fastpathv2.ResolveEndpointRequest, _ ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
	c.resolveRequest = request
	port := request.GetTarget().GetPort()
	componentName := request.GetTarget().GetComponentName()
	podPortName := request.GetTarget().GetPodPortName()
	path := "/v1/sandboxes/uid-a/ports/44772"
	endpoint := "http://sandbox-proxy.svc" + path
	switch {
	case componentName != "":
		port = 44772
		path = "/v2/sandboxes/uid-a/components/" + componentName
		endpoint = "http://sandbox-proxy.svc" + path
	case podPortName != "":
		port = 9000
		endpoint = "http://10.0.0.8:9000"
	}
	return &fastpathv2.ResolveEndpointResponse{
		SandboxUid:      "uid-a",
		Target:          request.Target,
		ComponentName:   componentName,
		Protocol:        "HTTP",
		ResolvedPort:    port,
		ProxyEndpoint:   endpoint,
		RequiredHeaders: map[string]string{"X-Fast-Sandbox-Route-Credential": "route-token"}, RouteGeneration: 3,
		FastletPod: "fastlet-a",
	}, nil
}

func TestEndpointResolverPreservesRoutePathWhenAuthorityIsOverridden(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: control, DefaultNamespace: "tenant-a", ProxyBaseURL: "http://127.0.0.1:18080/proxy"}

	route, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, ComponentTarget("execd"))
	require.NoError(t, err)
	require.Equal(t, "tenant-a", control.resolveRequest.GetSandbox().GetNamespacedName().GetNamespace())
	require.Equal(t, "sandbox-a", control.resolveRequest.GetSandbox().GetNamespacedName().GetName())
	require.Equal(t, "execd", control.resolveRequest.GetTarget().GetComponentName())
	require.True(t, control.resolveRequest.GetWaitUntilReady())
	require.Equal(t, int32(30_000), control.resolveRequest.GetWaitTimeoutMillis())
	require.Equal(t, "http://127.0.0.1:18080/proxy/v2/sandboxes/uid-a/components/execd", route.Endpoint.String())
	require.Equal(t, "route-token", route.RequiredHeaders.Get("X-Fast-Sandbox-Route-Credential"))

	requestURL, err := route.RequestURL("/command", nil)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18080/proxy/v2/sandboxes/uid-a/components/execd/command", requestURL.String())
}

func TestEndpointResolverDoesNotApplyComponentWaitToRawPort(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: control}
	_, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, PortTarget(8080))
	require.NoError(t, err)
	require.False(t, control.resolveRequest.GetWaitUntilReady())
	require.Zero(t, control.resolveRequest.GetWaitTimeoutMillis())
}

func TestEndpointResolverRejectsMismatchedRouteIdentity(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: mismatchedEndpointControl{control}}
	_, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, ComponentTarget("execd"))
	require.ErrorContains(t, err, "different Sandbox")
}

func TestEndpointResolverResolvesPodPortDirectly(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: control, DefaultNamespace: "tenant-a", ProxyBaseURL: "http://127.0.0.1:18080/proxy"}

	route, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, PodPortTarget("sidecar"))
	require.NoError(t, err)
	require.Equal(t, "sidecar", control.resolveRequest.GetTarget().GetPodPortName())
	require.Equal(t, "http://10.0.0.8:9000", route.Endpoint.String())
	require.Equal(t, uint32(9000), route.TargetPort)
	require.Equal(t, "route-token", route.RequiredHeaders.Get("X-Fast-Sandbox-Route-Credential"))

	// The Pod Port endpoint must not be rewritten to the central proxy base URL.
	require.NotContains(t, route.Endpoint.String(), "127.0.0.1")
}

func TestEndpointResolverRejectsPodPortCombinations(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: control}
	_, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, RouteTarget{ComponentName: "execd", PodPortName: "sidecar"})
	require.ErrorContains(t, err, "pod port name may not be combined")
	_, err = resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, RouteTarget{Port: 8080, PodPortName: "sidecar"})
	require.ErrorContains(t, err, "pod port name may not be combined")
}

type mismatchedEndpointControl struct{ *fakeEndpointControl }

func (c mismatchedEndpointControl) ResolveEndpoint(ctx context.Context, request *fastpathv2.ResolveEndpointRequest, options ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
	response, err := c.fakeEndpointControl.ResolveEndpoint(ctx, request, options...)
	response.ComponentName = "other"
	return response, err
}
