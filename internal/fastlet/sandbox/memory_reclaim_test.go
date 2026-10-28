package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/fastlet/resourceadmission"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// Reclaimed backing pages lower the runtime cgroup charge. Feed that change
// through the existing reader and observer into Fastlet's reported admission.
func TestReclaimedMemoryUpdatesExistingAdmissionFeedback(t *testing.T) {
	rt := newAdmissionRuntime()
	manager, profile := newResourceAwareAdmissionManager(t, rt, 10, "10", "50Gi")
	request := resourceAwareRequest("running", profile)
	created, err := manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err)
	committed := created.Admission.Resources.Committed

	cgroup := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(cgroup, name), []byte(value), 0o600))
	}
	write("memory.current", "95")
	write("memory.max", "100")
	write("memory.events", "oom_kill 0\n")
	write("memory.pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	write("meminfo", "MemTotal: 1000 kB\nMemAvailable: 500 kB\n")
	reader, err := resourceadmission.NewCgroupPathReader(cgroup, filepath.Join(cgroup, "meminfo"), 100)
	require.NoError(t, err)
	config := resourceadmission.DefaultConfig()
	observer, err := resourceadmission.NewObserver(reader, manager, config)
	require.NoError(t, err)
	for range config.HighSamples {
		observer.ObserveOnce()
	}
	blocked, _, _ := manager.State()
	require.Equal(t, fastletapi.AdmissionStateBlocked, blocked.Resources.State)
	require.Equal(t, fastletapi.AdmissionReasonFastletMemoryHighWatermark, blocked.Resources.Reason)
	require.Equal(t, committed, blocked.Resources.Committed)

	rejected, err := manager.CreateSandbox(context.Background(), resourceAwareRequest("new", profile))
	requireFastletCode(t, err, fastletapi.ErrorCapacityRejected)
	require.Equal(t, fastletapi.CreateDispositionRejectedBeforeSideEffects, rejected.Disposition)
	_, err = manager.CreateSandbox(context.Background(), request)
	require.NoError(t, err, "an existing identity remains replayable under pressure")

	write("memory.current", "70")
	for range config.RecoverySamples - 1 {
		state, _ := observer.ObserveOnce()
		require.Equal(t, fastletapi.AdmissionStateBlocked, state)
	}
	observer.ObserveOnce()
	recovered, _, _ := manager.State()
	require.Equal(t, fastletapi.AdmissionStateOpen, recovered.Resources.State)
	require.Empty(t, recovered.Resources.Reason)
	require.Greater(t, recovered.Resources.Revision, blocked.Resources.Revision)
	require.Equal(t, committed, recovered.Resources.Committed, "reclamation does not change static commitments")
	_, err = manager.CreateSandbox(context.Background(), resourceAwareRequest("new", profile))
	require.NoError(t, err)
}
