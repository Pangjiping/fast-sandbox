package infra

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

func TestProbeServiceHTTPAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Empty(t, request.Header.Get("X-EXECD-ACCESS-TOKEN"))
		if request.URL.Path == "/health" {
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	require.NoError(t, probeService(context.Background(), parsed.Host, infracatalog.ReadinessProbe{
		Type: infracatalog.ProbeHTTP, Path: "/health", Timeout: time.Second,
	}))

	_, portValue, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portValue)
	require.NoError(t, err)
	server.Close()
	err = probeService(context.Background(), net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), infracatalog.ReadinessProbe{
		Type: infracatalog.ProbeTCP, Timeout: 30 * time.Millisecond,
	})
	require.Error(t, err)
}

func TestReadinessProbeUsesFastExponentialBackoff(t *testing.T) {
	attempts := 0
	dial := func(context.Context, uint32) (net.Conn, error) {
		attempts++
		return nil, errors.New("not ready")
	}
	started := time.Now()
	err := probeServiceWithDialer(context.Background(), 44772, infracatalog.ReadinessProbe{
		Type: infracatalog.ProbeTCP, Timeout: 25 * time.Millisecond,
	}, dial, nil)
	require.Error(t, err)
	require.GreaterOrEqual(t, attempts, 4)
	require.Less(t, time.Since(started), 100*time.Millisecond)
}

func TestComponentReadinessFailureIsReportedAndFailsDataPlaneReady(t *testing.T) {
	manager, _ := testManager(t, apiv1alpha2.RuntimeContainer)
	require.NoError(t, manager.Prepare(context.Background()))
	spec := &fastletapi.RuntimeSandboxConfig{
		Spec:     fastletapi.SandboxSpec{InfraRevision: manager.Revision()},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "uid-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	}
	_, err := manager.PrepareInstance(context.Background(), spec)
	require.NoError(t, err)
	instance, err := manager.InitializeInstanceWithDialer(context.Background(), spec, func(context.Context, uint32) (net.Conn, error) {
		return nil, errors.New("not listening")
	})
	require.Error(t, err)
	require.Len(t, instance.Diagnostics, 1)
	require.Equal(t, "Failed", instance.Diagnostics[0].State)
}
