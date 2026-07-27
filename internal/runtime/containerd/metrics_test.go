package containerd

import (
	"context"
	"testing"

	fsbtest "fast-sandbox/internal/testutil"

	"github.com/stretchr/testify/require"
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
