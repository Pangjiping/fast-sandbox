package infra

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var infraReadyLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_infra_ready_latency_seconds",
	Help:    "Per-service Infra initialization and readiness latency.",
	Buckets: prometheus.ExponentialBuckets(0.005, 2, 13),
}, []string{"component", "runtime", "result"})

var infraInstanceStageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_infra_instance_stage_latency_seconds",
	Help:    "Latency of bounded synchronous per-Sandbox Infra preparation stages.",
	Buckets: prometheus.ExponentialBuckets(.00025, 2, 15),
}, []string{"stage", "result"})

func (m *Manager) observeInfraReady(component string, started time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	runtimeName := string(m.config.RuntimeProfile.Name)
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	infraReadyLatency.WithLabelValues(component, runtimeName, result).Observe(time.Since(started).Seconds())
}

func observeInstanceStage(stage string, started time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	infraInstanceStageLatency.WithLabelValues(stage, result).Observe(time.Since(started).Seconds())
}
