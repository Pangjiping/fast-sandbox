package containerd

import (
	"context"
	"strings"
	"time"

	"fast-sandbox/internal/observability"

	containerdclient "github.com/containerd/containerd/v2/client"
	containerdcontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
)

var createStageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_containerd_create_stage_latency_seconds",
	Help:    "Latency of bounded synchronous containerd Sandbox creation stages.",
	Buckets: prometheus.ExponentialBuckets(.00025, 2, 16),
}, []string{"runtime", "stage", "result"})

var createRPCLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_containerd_create_rpc_latency_seconds",
	Help:    "Latency of containerd RPCs issued by the synchronous Sandbox creation path.",
	Buckets: prometheus.ExponentialBuckets(.00025, 2, 16),
}, []string{"runtime", "rpc", "result"})

type createRPCMetricsContextKey struct{}

type createRPCMetricsContext struct {
	runtimeName string
}

func startContainerdCreateStage(ctx context.Context, runtimeName, stage string) (context.Context, func(error)) {
	runtimeName = normalizedRuntimeName(runtimeName)
	started := time.Now()
	stageContext, span := observability.Start(ctx, "runtime.containerd.create."+stage)
	return stageContext, func(err error) {
		result := metricResult(err)
		createStageLatency.WithLabelValues(runtimeName, stage, result).Observe(time.Since(started).Seconds())
		observability.End(span, err)
	}
}

func instrumentContainerOption(runtimeName, stage string, option containerdclient.NewContainerOpts) containerdclient.NewContainerOpts {
	return func(ctx context.Context, client *containerdclient.Client, container *containerdcontainers.Container) error {
		stageContext, finish := startContainerdCreateStage(ctx, runtimeName, stage)
		err := option(stageContext, client, container)
		finish(err)
		return err
	}
}

func withContainerdCreateRPCMetrics(ctx context.Context, runtimeName string) context.Context {
	return context.WithValue(ctx, createRPCMetricsContextKey{}, createRPCMetricsContext{
		runtimeName: normalizedRuntimeName(runtimeName),
	})
}

func containerdCreateRPCInterceptor(
	ctx context.Context,
	method string,
	request any,
	reply any,
	connection *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	options ...grpc.CallOption,
) error {
	metricsContext, ok := ctx.Value(createRPCMetricsContextKey{}).(createRPCMetricsContext)
	if !ok {
		return invoker(ctx, method, request, reply, connection, options...)
	}

	rpcName := normalizedCreateRPCName(method)
	started := time.Now()
	rpcContext, span := observability.StartClient(ctx, "runtime.containerd.rpc."+rpcName)
	err := invoker(rpcContext, method, request, reply, connection, options...)
	createRPCLatency.WithLabelValues(metricsContext.runtimeName, rpcName, metricResult(err)).
		Observe(time.Since(started).Seconds())
	observability.End(span, err)
	return err
}

func normalizedCreateRPCName(method string) string {
	switch {
	case strings.HasSuffix(method, ".leases.v1.Leases/Create"):
		return "lease_create"
	case strings.HasSuffix(method, ".leases.v1.Leases/Delete"):
		return "lease_delete"
	case strings.HasSuffix(method, ".snapshots.v1.Snapshots/Prepare"):
		return "snapshot_prepare"
	case strings.HasSuffix(method, ".snapshots.v1.Snapshots/Mounts"):
		return "snapshot_mounts"
	case strings.HasSuffix(method, ".containers.v1.Containers/Create"):
		return "container_create"
	case strings.HasSuffix(method, ".containers.v1.Containers/Get"):
		return "container_get"
	case strings.HasSuffix(method, ".tasks.v1.Tasks/Create"):
		return "task_create"
	case strings.HasSuffix(method, ".tasks.v1.Tasks/Start"):
		return "task_start"
	case strings.HasSuffix(method, ".images.v1.Images/Get"):
		return "image_get"
	default:
		return "other"
	}
}

func normalizedRuntimeName(runtimeName string) string {
	if runtimeName == "" {
		return "unknown"
	}
	return runtimeName
}

func metricResult(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}
