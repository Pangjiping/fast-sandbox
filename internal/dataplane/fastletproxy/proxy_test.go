package fastletproxy

import (
	"crypto/ed25519"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	routeauth "fast-sandbox/internal/dataplane/auth"

	"github.com/stretchr/testify/require"
)

func TestProxyForwardsArbitraryPortPreservesApplicationAuthorizationAndStripsRouteAuthority(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/command/run", request.URL.Path)
		require.Equal(t, "large=true", request.URL.RawQuery)
		require.Equal(t, "Bearer application-token", request.Header.Get("Authorization"))
		require.Empty(t, request.Header.Get(dataplane.HeaderRouteCredential))
		require.Empty(t, request.Header.Get(dataplane.HeaderRouteGeneration))
		require.Contains(t, request.Header.Get("traceparent"), "4bf92f3577b34da6a3ce929d0e0e4736")
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: ready\n\n")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	portNumber, err := parseTestPort(upstreamURL.Port())
	require.NoError(t, err)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := Route{
		RouteKey: RouteKey{SandboxUID: "uid-a", RouteGeneration: 7},
		RouteSpec: RouteSpec{
			Namespace: "default", FastletPodUID: "pod-a", AssignmentAttempt: 4,
			Access: dataplane.AccessDescriptor{Kind: dataplane.AccessKindDirectIP, Address: upstreamURL.Hostname()}, State: RouteReady,
		},
	}
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: portNumber,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	require.NoError(t, err)

	proxy := httptest.NewServer(&Proxy{Store: store, Verifier: verifier})
	defer proxy.Close()
	request, err := http.NewRequest(http.MethodPost, proxy.URL+dataplane.RoutePath("uid-a", portNumber)+"/command/run?large=true", strings.NewReader("payload"))
	require.NoError(t, err)
	request.Header.Set(dataplane.HeaderRouteCredential, token)
	request.Header.Set("Authorization", "Bearer application-token")
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	for name, values := range RouteHeaders(route) {
		request.Header[name] = values
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "data: ready\n\n", string(body))
}

func TestProxyDoesNotTreatCallerAuthorizationAsRouteCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer application-token", request.Header.Get("Authorization"))
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	userPort, err := parseTestPort(upstreamURL.Port())
	require.NoError(t, err)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := Route{
		RouteKey: RouteKey{SandboxUID: "uid-a", RouteGeneration: 7},
		RouteSpec: RouteSpec{
			Namespace: "default", FastletPodUID: "pod-a", AssignmentAttempt: 4,
			Access: dataplane.AccessDescriptor{Kind: dataplane.AccessKindDirectIP, Address: upstreamURL.Hostname()}, State: RouteReady,
		},
	}
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: userPort,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, dataplane.RoutePath(route.SandboxUID, userPort), nil)
	request.Header.Set(dataplane.HeaderRouteCredential, token)
	request.Header.Set("Authorization", "Bearer application-token")
	response := httptest.NewRecorder()
	(&Proxy{Store: store, Verifier: verifier}).ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestProxyRejectsStaleCredentialWithoutSeparateFenceHeaders(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := testRoute(2)
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: 8080,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: 1,
	})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, dataplane.RoutePath(route.SandboxUID, 8080), nil)
	request.Header.Set(dataplane.HeaderRouteCredential, token)
	response := httptest.NewRecorder()
	(&Proxy{Store: store, Verifier: verifier}).ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Equal(t, dataplane.ProxyErrorCredentialRejected, response.Header().Get(dataplane.HeaderProxyError))
}

func TestProxyMarksPlatformFailuresAndStripsApplicationErrorHeader(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, time.Now)
	require.NoError(t, err)
	route := testRoute(2)
	store := NewStore()
	_, err = store.Apply(route)
	require.NoError(t, err)

	staleRequest := httptest.NewRequest(http.MethodGet, dataplane.RoutePath(route.SandboxUID, 8080), nil)
	staleRequest.Header.Set(dataplane.HeaderRouteCredential, "invalid")
	staleResponse := httptest.NewRecorder()
	(&Proxy{Store: store, Verifier: verifier}).ServeHTTP(staleResponse, staleRequest)
	require.Equal(t, http.StatusForbidden, staleResponse.Code)
	require.Equal(t, dataplane.ProxyErrorCredentialRejected, staleResponse.Header().Get(dataplane.HeaderProxyError))

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set(dataplane.HeaderProxyError, "forged-by-application")
		writer.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	upstreamPort, err := parseTestPort(upstreamURL.Port())
	require.NoError(t, err)
	appRoute := route
	appRoute.Access.Address = upstreamURL.Hostname()
	appStore := NewStore()
	_, err = appStore.Apply(appRoute)
	require.NoError(t, err)
	token, _, err := issuer.Issue(routeauth.Claims{
		Namespace: appRoute.Namespace, SandboxUID: appRoute.SandboxUID, TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: upstreamPort, FastletPodUID: appRoute.FastletPodUID,
		AssignmentAttempt: appRoute.AssignmentAttempt, RouteGeneration: appRoute.RouteGeneration,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodGet, dataplane.RoutePath(appRoute.SandboxUID, upstreamPort), nil)
	request.Header.Set(dataplane.HeaderRouteCredential, token)
	response := httptest.NewRecorder()
	(&Proxy{Store: appStore, Verifier: verifier}).ServeHTTP(response, request)
	require.Equal(t, http.StatusTeapot, response.Code)
	require.Empty(t, response.Header().Get(dataplane.HeaderProxyError))
}

func parseTestPort(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint32(parsed), nil
}
