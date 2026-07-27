package fastpath

import (
	"context"
	"testing"

	fsbtest "fast-sandbox/internal/testutil"

	"github.com/stretchr/testify/require"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestCreateStageMetricIsCollectable(t *testing.T) {
	labels := map[string]string{"stage": "candidate_selection", "result": "success"}
	before, _ := fsbtest.HistogramSampleCountFrom(controllermetrics.Registry, "fast_sandbox_create_stage_latency_seconds", labels)
	_, finish := startCreateStage(context.Background(), "candidate_selection")
	finish(nil)
	after, err := fsbtest.HistogramSampleCountFrom(controllermetrics.Registry, "fast_sandbox_create_stage_latency_seconds", labels)
	require.NoError(t, err)
	require.Equal(t, before+1, after)
}
