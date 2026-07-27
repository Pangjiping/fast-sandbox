package network

import (
	"testing"
	"time"

	fsbtest "fast-sandbox/internal/testutil"

	"github.com/stretchr/testify/require"
)

func TestNetworkLatencyMetricsAreCollectable(t *testing.T) {
	acquireLabels := map[string]string{"result": "bound"}
	persistLabels := map[string]string{"result": "success"}
	acquireBefore, _ := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_acquire_latency_seconds", acquireLabels)
	persistBefore, _ := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_persist_latency_seconds", persistLabels)
	observeSlotAcquire("bound", time.Now())
	observeSlotPersist(time.Now(), nil)
	acquireAfter, err := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_acquire_latency_seconds", acquireLabels)
	require.NoError(t, err)
	persistAfter, err := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_persist_latency_seconds", persistLabels)
	require.NoError(t, err)
	require.Equal(t, acquireBefore+1, acquireAfter)
	require.Equal(t, persistBefore+1, persistAfter)
}
