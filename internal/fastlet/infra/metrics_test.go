package infra

import (
	"testing"
	"time"

	fsbtest "fast-sandbox/internal/testutil"

	"github.com/stretchr/testify/require"
)

func TestInfraInstanceStageMetricIsCollectable(t *testing.T) {
	labels := map[string]string{"stage": "config_persist", "result": "success"}
	before, _ := fsbtest.HistogramSampleCount("fast_sandbox_infra_instance_stage_latency_seconds", labels)
	observeInstanceStage("config_persist", time.Now(), nil)
	after, err := fsbtest.HistogramSampleCount("fast_sandbox_infra_instance_stage_latency_seconds", labels)
	require.NoError(t, err)
	require.Equal(t, before+1, after)
}
