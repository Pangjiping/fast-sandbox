package containerd

import (
	"context"
	"time"

	"fast-sandbox/internal/observability"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var createStageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_containerd_create_stage_latency_seconds",
	Help:    "Latency of bounded synchronous containerd Sandbox creation stages.",
	Buckets: prometheus.ExponentialBuckets(.00025, 2, 16),
}, []string{"runtime", "stage", "result"})

func startContainerdCreateStage(ctx context.Context, runtimeName, stage string) (context.Context, func(error)) {
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	started := time.Now()
	stageContext, span := observability.Start(ctx, "runtime.containerd.create."+stage)
	return stageContext, func(err error) {
		result := "success"
		if err != nil {
			result = "error"
		}
		createStageLatency.WithLabelValues(runtimeName, stage, result).Observe(time.Since(started).Seconds())
		observability.End(span, err)
	}
}
