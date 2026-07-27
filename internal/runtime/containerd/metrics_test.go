package containerd

import (
	"context"
	"errors"
	"testing"

	fsbtest "fast-sandbox/internal/testutil"

	containerdclient "github.com/containerd/containerd/v2/client"
	containerdcontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestContainerdCreateStageMetricIsCollectable(t *testing.T) {
	labels := map[string]string{"runtime": "container", "stage": "task_create", "result": "success"}
	before, _ := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_stage_latency_seconds", labels)
	_, finish := startContainerdCreateStage(context.Background(), "container", "task_create")
	finish(nil)
	after, err := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_stage_latency_seconds", labels)
	require.NoError(t, err)
	require.Equal(t, before+1, after)
}

func TestInstrumentContainerOptionRecordsStage(t *testing.T) {
	labels := map[string]string{"runtime": "container", "stage": "snapshot_prepare_opt", "result": "success"}
	before, _ := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_stage_latency_seconds", labels)
	option := instrumentContainerOption("container", "snapshot_prepare_opt",
		func(context.Context, *containerdclient.Client, *containerdcontainers.Container) error {
			return nil
		},
	)

	err := option(context.Background(), nil, &containerdcontainers.Container{})

	require.NoError(t, err)
	after, collectErr := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_stage_latency_seconds", labels)
	require.NoError(t, collectErr)
	require.Equal(t, before+1, after)
}

func TestContainerdCreateRPCInterceptorRecordsMarkedCreateRPC(t *testing.T) {
	labels := map[string]string{"runtime": "container", "rpc": "task_create", "result": "success"}
	before, _ := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_rpc_latency_seconds", labels)
	ctx := withContainerdCreateRPCMetrics(context.Background(), "container")
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		return nil
	}

	err := containerdCreateRPCInterceptor(
		ctx,
		"/containerd.services.tasks.v1.Tasks/Create",
		nil,
		nil,
		nil,
		invoker,
	)

	require.NoError(t, err)
	after, collectErr := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_rpc_latency_seconds", labels)
	require.NoError(t, collectErr)
	require.Equal(t, before+1, after)
}

func TestContainerdCreateRPCInterceptorIgnoresUnmarkedRPC(t *testing.T) {
	labels := map[string]string{"runtime": "container", "rpc": "task_create", "result": "error"}
	createRPCLatency.WithLabelValues("container", "task_create", "error")
	before, _ := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_rpc_latency_seconds", labels)
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		return errors.New("test error")
	}

	err := containerdCreateRPCInterceptor(
		context.Background(),
		"/containerd.services.tasks.v1.Tasks/Create",
		nil,
		nil,
		nil,
		invoker,
	)

	require.EqualError(t, err, "test error")
	after, collectErr := fsbtest.HistogramSampleCount("fast_sandbox_containerd_create_rpc_latency_seconds", labels)
	require.NoError(t, collectErr)
	require.Equal(t, before, after)
}

func TestNormalizedCreateRPCName(t *testing.T) {
	tests := map[string]string{
		"/containerd.services.leases.v1.Leases/Create":         "lease_create",
		"/containerd.services.leases.v1.Leases/Delete":         "lease_delete",
		"/containerd.services.snapshots.v1.Snapshots/Prepare":  "snapshot_prepare",
		"/containerd.services.snapshots.v1.Snapshots/Mounts":   "snapshot_mounts",
		"/containerd.services.containers.v1.Containers/Create": "container_create",
		"/containerd.services.containers.v1.Containers/Get":    "container_get",
		"/containerd.services.tasks.v1.Tasks/Create":           "task_create",
		"/containerd.services.tasks.v1.Tasks/Start":            "task_start",
		"/containerd.services.images.v1.Images/Get":            "image_get",
		"/containerd.services.content.v1.Content/Info":         "other",
	}
	for method, expected := range tests {
		t.Run(expected, func(t *testing.T) {
			require.Equal(t, expected, normalizedCreateRPCName(method))
		})
	}
}
