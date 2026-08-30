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
	path := "/v1/sandboxes/uid-a/ports/44772"
	if componentName != "" {
		port = 44772
		path = "/v2/sandboxes/uid-a/components/" + componentName
	}
	return &fastpathv2.ResolveEndpointResponse{
		SandboxUid:      "uid-a",
		Endpoint:        &fastpathv2.ResolvedEndpoint{ComponentName: componentName, Protocol: "HTTP", Port: port},
		ProxyEndpoint:   "http://sandbox-proxy.svc" + path,
		RequiredHeaders: map[string]string{"X-Fast-Sandbox-Route-Credential": "route-token"}, RouteGeneration: 3,
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
	require.Equal(t, "http://127.0.0.1:18080/proxy/v2/sandboxes/uid-a/components/execd", route.Endpoint.String())
	require.Equal(t, "route-token", route.RequiredHeaders.Get("X-Fast-Sandbox-Route-Credential"))

	requestURL, err := route.RequestURL("/command", nil)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18080/proxy/v2/sandboxes/uid-a/components/execd/command", requestURL.String())
}

func TestEndpointResolverUsesNonBlockingRawPortResolution(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: control}
	_, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, PortTarget(8080))
	require.NoError(t, err)
	require.Equal(t, uint32(8080), control.resolveRequest.GetTarget().GetPort())
}

func TestEndpointResolverRejectsMismatchedRouteIdentity(t *testing.T) {
	control := &fakeEndpointControl{}
	resolver := &EndpointResolver{Control: mismatchedEndpointControl{control}}
	_, err := resolver.Resolve(context.Background(), SandboxRef{Name: "sandbox-a"}, ComponentTarget("execd"))
	require.ErrorContains(t, err, "different Sandbox")
}

type mismatchedEndpointControl struct{ *fakeEndpointControl }

func (c mismatchedEndpointControl) ResolveEndpoint(ctx context.Context, request *fastpathv2.ResolveEndpointRequest, options ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
	response, err := c.fakeEndpointControl.ResolveEndpoint(ctx, request, options...)
	response.Endpoint.ComponentName = "other"
	return response, err
}
