package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeFastPath struct {
	mu         sync.Mutex
	requests   []*fastpathv2.CreateSandboxRequest
	deleted    []string
	deletedSet map[string]bool
	failAt     map[string]bool
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func (f *fakeFastPath) CreateSandbox(_ context.Context, request *fastpathv2.CreateSandboxRequest, _ ...grpc.CallOption) (*fastpathv2.CreateSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.failAt[request.RequestId] {
		return nil, status.Error(codes.ResourceExhausted, "full")
	}
	return &fastpathv2.CreateSandboxResponse{Sandbox: &fastpathv2.SandboxInfo{Identity: &fastpathv2.SandboxIdentity{
		Uid: "uid-" + request.RequestId, Name: request.RequestId,
	}}}, nil
}

func (f *fakeFastPath) DeleteSandbox(_ context.Context, request *fastpathv2.DeleteRequest, _ ...grpc.CallOption) (*fastpathv2.DeleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := request.GetSandbox().GetNamespacedName().GetName()
	f.deleted = append(f.deleted, name)
	if f.deletedSet == nil {
		f.deletedSet = make(map[string]bool)
	}
	f.deletedSet[name] = true
	return &fastpathv2.DeleteResponse{}, nil
}

func (f *fakeFastPath) ListSandboxes(_ context.Context, request *fastpathv2.ListSandboxesRequest, _ ...grpc.CallOption) (*fastpathv2.ListSandboxesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	response := &fastpathv2.ListSandboxesResponse{}
	for _, create := range f.requests {
		if create.Namespace == request.Namespace && !f.failAt[create.RequestId] && !f.deletedSet[create.RequestId] {
			response.Items = append(response.Items, &fastpathv2.SandboxSummary{Identity: &fastpathv2.SandboxIdentity{Name: create.RequestId}})
		}
	}
	return response, nil
}

func TestRunLoadReportsBoundedResultsAndCleanup(t *testing.T) {
	client := &fakeFastPath{failAt: map[string]bool{"load-2": true}}
	cfg := config{
		Endpoint: "fastpath:9090", Namespace: "load", Pool: "pool-a", Image: "alpine:latest",
		Command: "/bin/sh", Args: []string{"-c", "sleep 1"}, Requests: 5, Concurrency: 3,
		RequestTimeout: time.Second, RequestIDPrefix: "load", Cleanup: true, CleanupTimeout: time.Second, CleanupPollInterval: time.Millisecond,
		Runtime: "container", InfraRevision: "sha256:test", ImageState: "warm", ImageAffinity: "hit",
		NetworkSlotState: "clean", CreatePath: "fastpath", FastPathReplicas: 3, ControllerReplicas: 2, ProxyReplicas: 2,
	}
	report := runLoad(context.Background(), client, cfg)

	require.Equal(t, 4, report.Succeeded)
	require.Equal(t, 1, report.Failed)
	require.Equal(t, 5, report.Attempted)
	require.Zero(t, report.NotAttempted)
	require.Equal(t, 4, report.GRPCCodes[codes.OK.String()])
	require.Equal(t, 1, report.GRPCCodes[codes.ResourceExhausted.String()])
	require.Equal(t, 5, report.CreateRPCLatency.Samples)
	require.Equal(t, 4, report.SuccessfulCreateRPCLatency.Samples)
	require.Len(t, report.CreateRPCLatencySamples, 5)
	require.Len(t, report.SuccessfulCreateRPCLatencySamples, 4)
	require.IsNonDecreasing(t, report.CreateRPCLatencySamples)
	require.IsNonDecreasing(t, report.SuccessfulCreateRPCLatencySamples)
	require.Equal(t, 4, report.Identity.UniqueSandboxUIDs)
	require.Zero(t, report.Identity.DuplicateSandboxUIDs)
	require.Zero(t, report.Identity.MissingSandboxUIDs)
	require.NotNil(t, report.Cleanup)
	require.Equal(t, 5, report.Cleanup.Succeeded)
	require.True(t, report.Cleanup.Converged)
	require.Len(t, client.deleted, 5)

	seen := make(map[string]bool)
	for _, request := range client.requests {
		require.False(t, seen[request.RequestId])
		seen[request.RequestId] = true
		require.Equal(t, cfg.Namespace, request.Namespace)
		require.Equal(t, cfg.Pool, request.PoolRef)
	}
}

func TestParseConfigProvidesSafeExplicitDefaults(t *testing.T) {
	cfg, err := parseConfig(nil, &bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, "fastpath", cfg.CreatePath)
	require.Equal(t, "unspecified", cfg.ImageState)
	require.Equal(t, []string{"-c", "sleep 3600"}, cfg.Args)
	require.Equal(t, 100*time.Millisecond, cfg.CleanupPollInterval)
	require.Zero(t, cfg.CleanupRegistrySettle)
	require.NotEmpty(t, cfg.RequestIDPrefix)
}

func TestRunLoadSeparatesCanceledBeforeAttemptFromRPCLatency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := runLoad(ctx, &fakeFastPath{}, config{
		Requests: 3, Concurrency: 2, Rate: 1, RequestTimeout: time.Second,
		RequestIDPrefix: "cancelled", Namespace: "default", Pool: "pool-a", Image: "alpine:latest", CleanupPollInterval: time.Millisecond,
	})
	require.Zero(t, report.Attempted)
	require.Equal(t, 3, report.NotAttempted)
	require.Zero(t, report.CreateRPCLatency.Samples)
	require.Empty(t, report.CreateRPCLatencySamples)
	require.Equal(t, 3, report.GRPCCodes[codes.Canceled.String()])
}

func TestSummarizeLatenciesUsesNearestRank(t *testing.T) {
	values := make([]time.Duration, 100)
	for index := range values {
		values[index] = time.Duration(index+1) * time.Millisecond
	}
	summary := summarizeLatencies(values)
	require.Equal(t, 50.0, summary.P50)
	require.Equal(t, 95.0, summary.P95)
	require.Equal(t, 99.0, summary.P99)
	require.Equal(t, 100.0, summary.Max)
}

func TestValidateConfigRejectsUnsafeOrAmbiguousLoad(t *testing.T) {
	base := config{
		Endpoint: "fastpath:9090", Namespace: "default", Pool: "pool-a", Image: "alpine:latest",
		Requests: 10, Concurrency: 2, RequestTimeout: time.Second, CleanupTimeout: time.Second, RequestIDPrefix: "load-a",
		CleanupPollInterval: time.Millisecond,
		CreatePath:          "fastpath", ImageState: "unspecified", ImageAffinity: "unspecified", NetworkSlotState: "unspecified",
	}
	require.NoError(t, validateConfig(base))
	badConcurrency := base
	badConcurrency.Concurrency = 11
	require.ErrorContains(t, validateConfig(badConcurrency), "concurrency")
	badPrefix := base
	badPrefix.RequestIDPrefix = "unsafe prefix"
	require.ErrorContains(t, validateConfig(badPrefix), "whitespace")
	missingMetricsEndpoint := base
	missingMetricsEndpoint.CleanupNetworkSlots = 10
	require.ErrorContains(t, validateConfig(missingMetricsEndpoint), "fastlet-metrics-url")
	negativeSettle := base
	negativeSettle.CleanupRegistrySettle = -time.Second
	require.ErrorContains(t, validateConfig(negativeSettle), "cleanup-registry-settle")
}

func TestCleanupWaitsForNetworkSlotMetric(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		value := 0
		if calls.Add(1) > 1 {
			value = 2
		}
		body := fmt.Sprintf("# TYPE fast_sandbox_network_slot_available gauge\nfast_sandbox_network_slot_available %d\n", value)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	client := &fakeFastPath{}
	cfg := config{
		Namespace: "default", RequestTimeout: time.Second, CleanupTimeout: time.Second, CleanupPollInterval: time.Millisecond,
		CleanupNetworkSlots: 2, FastletMetricsURLs: []string{"http://fastlet.test/metrics"}, MetricsHTTPClient: httpClient,
	}
	result := cleanup(context.Background(), client, cfg, []string{"load-0"})
	require.True(t, result.Converged)
	require.Equal(t, 2, result.AvailableNetworkSlots)
}

func TestParseNetworkSlotAvailable(t *testing.T) {
	value, err := parseNetworkSlotAvailable(strings.NewReader(`# HELP fast_sandbox_network_slot_available clean slots
fast_sandbox_network_slot_available 7
`))
	require.NoError(t, err)
	require.Equal(t, 7, value)
}
