package sandboxproxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	routeauth "fast-sandbox/internal/dataplane/auth"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fast-sandbox/internal/observability"

	"go.opentelemetry.io/otel/attribute"
)

const DefaultAddress = ":8080"

type Proxy struct {
	Resolver    Resolver
	Verifier    *routeauth.Verifier
	Transport   http.RoundTripper
	FastletPort int
}

func (p *Proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	request, span := observability.StartHTTPServer(request, "sandbox-proxy")
	started := time.Now()
	metricResult := "success"
	defer func() {
		span.SetAttributes(attribute.String("fast_sandbox.proxy_result", metricResult))
		observability.End(span, nil)
		observeSandboxProxy(metricResult, started)
	}()
	sandboxUID, targetPort, componentName, err := parseSandboxTarget(request.URL.Path)
	if err != nil {
		metricResult = "invalid_route"
		writeProxyError(writer, http.StatusBadRequest, dataplane.ProxyErrorRouteUnavailable, err.Error())
		return
	}
	if p.Resolver == nil || p.Verifier == nil {
		metricResult = "unconfigured"
		writeProxyError(writer, http.StatusServiceUnavailable, dataplane.ProxyErrorRouteUnavailable, "Sandbox Proxy is not configured")
		return
	}
	token, err := routeCredential(request.Header.Get(dataplane.HeaderRouteCredential))
	if err != nil {
		metricResult = "missing_credential"
		writeProxyError(writer, http.StatusUnauthorized, dataplane.ProxyErrorCredentialRejected, err.Error())
		return
	}
	if componentName != "" {
		claims, verifyErr := p.Verifier.Verify(token)
		if verifyErr != nil || claims.SandboxUID != sandboxUID ||
			claims.TargetKind != routeauth.TargetKindComponent || claims.ComponentName != componentName {
			metricResult = "credential_rejected"
			writeProxyError(writer, http.StatusForbidden, dataplane.ProxyErrorCredentialRejected, "route credential rejected")
			return
		}
		targetPort = claims.TargetPort
	}
	route, err := p.Resolver.Resolve(request.Context(), sandboxUID)
	if err != nil {
		metricResult = "resolve_error"
		writeResolveError(writer, err)
		return
	}
	request = request.WithContext(observability.WithIdentity(request.Context(), observability.Identity{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, FastletPodUID: route.FastletPodUID,
		AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration, TargetPort: targetPort,
	}))
	if _, err = p.verify(token, targetPort, componentName, route); err != nil {
		// Watch delivery may lag behind a freshly issued credential. One direct
		// API-server read distinguishes temporary cache lag from a stale token.
		route, err = p.Resolver.ResolveFresh(request.Context(), sandboxUID)
		if err != nil {
			metricResult = "resolve_error"
			writeResolveError(writer, err)
			return
		}
		request = request.WithContext(observability.WithIdentity(request.Context(), observability.Identity{
			Namespace: route.Namespace, SandboxUID: route.SandboxUID, FastletPodUID: route.FastletPodUID,
			AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration, TargetPort: targetPort,
		}))
		if _, err = p.verify(token, targetPort, componentName, route); err != nil {
			metricResult = "credential_rejected"
			writeProxyError(writer, http.StatusForbidden, dataplane.ProxyErrorCredentialRejected, "route credential rejected")
			return
		}
	}
	port := p.FastletPort
	if port <= 0 {
		port = 5780
	}
	upstream := "http://" + route.FastletPodIP + ":" + strconv.Itoa(port)
	transport := p.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: false, DisableCompression: true,
			MaxIdleConns: 512, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second,
		}
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(mustParseURL(upstream))
			proxyRequest.Out.Host = proxyRequest.In.Host
			stripForwardingAuthority(proxyRequest.Out.Header)
			proxyRequest.Out.Header.Set(dataplane.HeaderRouteCredential, token)
			observability.InjectHTTP(proxyRequest.Out.Context(), proxyRequest.Out.Header)
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
			metricResult = "upstream_error"
			writeProxyError(response, http.StatusBadGateway, dataplane.ProxyErrorUpstreamUnavailable, "assigned Fastlet Proxy unavailable: "+proxyErr.Error())
		},
	}
	proxy.ServeHTTP(writer, request)
}

func parseSandboxTarget(path string) (sandboxUID string, port uint32, component string, err error) {
	if strings.HasPrefix(path, "/v2/sandboxes/") {
		sandboxUID, component, _, err = dataplane.ParseComponentRoutePath(path)
		return
	}
	sandboxUID, port, _, err = dataplane.ParseRoutePath(path)
	return
}

func (p *Proxy) verify(token string, targetPort uint32, componentName string, route Route) (routeauth.Claims, error) {
	targetKind := routeauth.TargetKindPort
	if componentName != "" {
		targetKind = routeauth.TargetKindComponent
	}
	claims, err := p.Verifier.Verify(token)
	if err != nil {
		return routeauth.Claims{}, err
	}
	return p.Verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: route.Namespace, SandboxUID: route.SandboxUID, TargetPort: targetPort,
		TargetKind: targetKind, ComponentName: componentName, Protocol: claims.Protocol,
		FastletPodUID: route.FastletPodUID, AssignmentAttempt: route.AssignmentAttempt, RouteGeneration: route.RouteGeneration,
	})
}

func routeCredential(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("route credential is required")
	}
	return strings.TrimSpace(value), nil
}

func stripForwardingAuthority(headers http.Header) {
	headers.Del(dataplane.HeaderForwardedNamespace)
	headers.Del(dataplane.HeaderRouteCredential)
	headers.Del(dataplane.HeaderFastletPodUID)
	headers.Del(dataplane.HeaderAssignmentAttempt)
	headers.Del(dataplane.HeaderRouteGeneration)
}

func writeResolveError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSandboxNotFound):
		writeProxyError(writer, http.StatusNotFound, dataplane.ProxyErrorRouteUnavailable, err.Error())
	case errors.Is(err, ErrSandboxNotReady), errors.Is(err, ErrFastletUnavailable):
		writer.Header().Set("Retry-After", "1")
		writeProxyError(writer, http.StatusServiceUnavailable, dataplane.ProxyErrorRouteUnavailable, err.Error())
	default:
		writeProxyError(writer, http.StatusBadGateway, dataplane.ProxyErrorRouteUnavailable, err.Error())
	}
}

func writeProxyError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set(dataplane.HeaderProxyError, code)
	http.Error(writer, message, status)
}

func mustParseURL(value string) *url.URL {
	parsed, err := url.Parse(value)
	if err != nil {
		panic(fmt.Sprintf("invalid internal proxy URL %q: %v", value, err))
	}
	return parsed
}
