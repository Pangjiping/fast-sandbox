package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"

	"github.com/stretchr/testify/require"
)

var fixedTime = time.Unix(1720000000, 0)

func newTestState(t *testing.T) *State {
	t.Helper()
	state, err := New(t.TempDir(), WithNow(func() time.Time { return fixedTime }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func identity(requestID, podUID string) agentprotocol.Identity {
	return agentprotocol.Identity{RequestID: requestID, Namespace: "tenant-a", PodUID: podUID}
}

func pinDigest(image string) string { return "sha256:" + image }

func TestPinImageLifecycleAndReplay(t *testing.T) {
	state := newTestState(t)

	calls := 0
	digest, err := state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		calls++
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	require.Equal(t, pinDigest("img-a"), digest)

	// Replay of the same request id returns the recorded digest without
	// re-executing the side effect.
	digest, err = state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		calls++
		return "", errors.New("must not run")
	})
	require.NoError(t, err)
	require.Equal(t, pinDigest("img-a"), digest)
	require.Equal(t, 1, calls)

	snapshot := state.Snapshot()
	require.Equal(t, 1, snapshot.PinCount)
	require.Equal(t, 1, snapshot.ImageCount)

	require.NoError(t, state.UnpinImage(identity("req-2", "pod-1"), "img-a"))
	require.Equal(t, 0, state.Snapshot().PinCount)
}

func TestPinImageIdempotentReplayAcrossRestart(t *testing.T) {
	root := t.TempDir()
	first, err := New(root, WithNow(func() time.Time { return fixedTime }))
	require.NoError(t, err)
	digest, err := first.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	require.NoError(t, first.Close())

	recovered, err := New(root, WithNow(func() time.Time { return fixedTime }))
	require.NoError(t, err)
	defer recovered.Close()
	calls := 0
	replayed, err := recovered.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		calls++
		return "", errors.New("must not run")
	})
	require.NoError(t, err)
	require.Equal(t, digest, replayed)
	require.Equal(t, 0, calls)
	require.Equal(t, 1, recovered.Snapshot().PinCount)
}

func TestPinImageReplayCrossPodConflict(t *testing.T) {
	state := newTestState(t)
	_, err := state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)

	_, err = state.PinImage(identity("req-1", "pod-2"), "img-a", func() (string, error) {
		return "", errors.New("must not run")
	})
	require.ErrorIs(t, err, ErrConflict)
}

func TestPinImageReplayDifferentOpConflict(t *testing.T) {
	state := newTestState(t)
	_, err := state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	require.ErrorIs(t, state.UnpinImage(identity("req-1", "pod-1"), "img-a"), ErrConflict)
}

func TestPinImageConcurrentDedup(t *testing.T) {
	state := newTestState(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var callsMu sync.Mutex
	var wg sync.WaitGroup
	results := make([]string, 8)
	errorsSeen := make([]error, 8)
	for index := 0; index < len(results); index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errorsSeen[index] = state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
				callsMu.Lock()
				calls++
				callsMu.Unlock()
				close(started)
				<-release
				return pinDigest("img-a"), nil
			})
		}(index)
	}
	<-started
	close(release)
	wg.Wait()

	for index := 0; index < len(results); index++ {
		require.NoError(t, errorsSeen[index])
		require.Equal(t, pinDigest("img-a"), results[index])
	}
	require.Equal(t, 1, calls)
	require.Equal(t, 1, state.Snapshot().PinCount)
}

func TestUnpinImageClampsAtZero(t *testing.T) {
	root := t.TempDir()
	state, err := New(root)
	require.NoError(t, err)
	require.NoError(t, state.UnpinImage(identity("req-1", "pod-1"), "img-a"))
	require.NoError(t, state.UnpinImage(identity("req-2", "pod-1"), "img-a"))
	require.Equal(t, 0, state.Snapshot().PinCount)
	require.NoError(t, state.Close())

	recovered, err := New(root)
	require.NoError(t, err)
	defer recovered.Close()
	require.Equal(t, 0, recovered.Snapshot().PinCount)
}

func TestLeaseDevicesLifecycle(t *testing.T) {
	state := newTestState(t)
	do := func(leaseID string) (agentprotocol.Lease, error) {
		return agentprotocol.Lease{
			LeaseID: leaseID, SandboxID: "sandbox-1", Image: "img-a", PodUID: "pod-1",
			Namespace: "tenant-a", RootfsDev: "/cache/img-a/rootfs.img", CreatedAt: fixedTime,
		}, nil
	}
	lease, err := state.LeaseDevices(identity("req-1", "pod-1"), "sandbox-1", "img-a", 4096, false, do)
	require.NoError(t, err)
	require.NotEmpty(t, lease.LeaseID)
	require.Equal(t, "sandbox-1", lease.SandboxID)

	// Business idempotency: a different request id for the same sandbox
	// returns the existing lease without creating a second one.
	replayed, err := state.LeaseDevices(identity("req-2", "pod-1"), "sandbox-1", "img-a", 4096, false, do)
	require.NoError(t, err)
	require.Equal(t, lease, replayed)
	require.Equal(t, 1, state.Snapshot().LeaseCount)
	require.Equal(t, 1, state.Snapshot().PinCount+1) // leaseCount contributes 1

	require.NoError(t, state.ReleaseDevices(identity("req-3", "pod-1"), lease.LeaseID))
	require.Equal(t, 0, state.Snapshot().LeaseCount)
}

func TestLeaseDevicesCrossPodSandboxConflict(t *testing.T) {
	state := newTestState(t)
	do := func(leaseID string) (agentprotocol.Lease, error) {
		return agentprotocol.Lease{
			LeaseID: leaseID, SandboxID: "sandbox-1", Image: "img-a", PodUID: "pod-1", RootfsDev: "/r", CreatedAt: fixedTime,
		}, nil
	}
	_, err := state.LeaseDevices(identity("req-1", "pod-1"), "sandbox-1", "img-a", 4096, false, do)
	require.NoError(t, err)
	_, err = state.LeaseDevices(identity("req-2", "pod-2"), "sandbox-1", "img-a", 4096, false, do)
	require.ErrorIs(t, err, ErrConflict)
}

func TestReleaseDevicesOwnershipConflict(t *testing.T) {
	state := newTestState(t)
	do := func(leaseID string) (agentprotocol.Lease, error) {
		return agentprotocol.Lease{
			LeaseID: leaseID, SandboxID: "sandbox-1", Image: "img-a", PodUID: "pod-1", RootfsDev: "/r", CreatedAt: fixedTime,
		}, nil
	}
	lease, err := state.LeaseDevices(identity("req-1", "pod-1"), "sandbox-1", "img-a", 4096, false, do)
	require.NoError(t, err)
	require.ErrorIs(t, state.ReleaseDevices(identity("req-2", "pod-2"), lease.LeaseID), ErrConflict)
	require.Equal(t, 1, state.Snapshot().LeaseCount)
}

func TestReleaseDevicesUnknownLeaseIsNoOp(t *testing.T) {
	state := newTestState(t)
	require.NoError(t, state.ReleaseDevices(identity("req-1", "pod-1"), "missing"))
	require.NoError(t, state.ReleaseDevices(identity("req-1", "pod-1"), "missing"))
	require.Equal(t, 0, state.Snapshot().LeaseCount)
}

func TestJournalWritesIntentBeforeExecution(t *testing.T) {
	root := t.TempDir()
	state, err := New(root, WithNow(func() time.Time { return fixedTime }))
	require.NoError(t, err)
	defer func() { _ = state.Close() }()
	journalPath := filepath.Join(root, "agent", JournalFileName)
	_, err = state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		payload, readErr := os.ReadFile(journalPath)
		require.NoError(t, readErr)
		require.Contains(t, string(payload), `"op":"pin-image"`)
		require.Contains(t, string(payload), `"requestId":"req-1"`)
		require.NotContains(t, string(payload), `"result"`)
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	payload, err := os.ReadFile(journalPath)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"result"`)
	require.Contains(t, string(payload), pinDigest("img-a"))
}

func TestRecoverRebuildsLeasesAndCounts(t *testing.T) {
	root := t.TempDir()
	state, err := New(root, WithNow(func() time.Time { return fixedTime }), WithUUID(func() (string, error) { return "lease-1", nil }))
	require.NoError(t, err)
	_, err = state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	_, err = state.PinImage(identity("req-2", "pod-1"), "img-b", func() (string, error) {
		return pinDigest("img-b"), nil
	})
	require.NoError(t, err)
	lease, err := state.LeaseDevices(identity("req-3", "pod-1"), "sandbox-1", "img-a", 4096, false, func(leaseID string) (agentprotocol.Lease, error) {
		return agentprotocol.Lease{
			LeaseID: leaseID, SandboxID: "sandbox-1", Image: "img-a", PodUID: "pod-1",
			Namespace: "tenant-a", RootfsDev: "/cache/img-a/rootfs.img", CreatedAt: fixedTime,
		}, nil
	})
	require.NoError(t, err)
	require.NoError(t, state.Close())

	recovered, err := New(root, WithNow(func() time.Time { return fixedTime }))
	require.NoError(t, err)
	defer recovered.Close()

	snapshot := recovered.Snapshot()
	require.Equal(t, 1, snapshot.LeaseCount)
	require.Equal(t, 2, snapshot.PinCount)
	require.Equal(t, 2, snapshot.ImageCount)
	recoveredLease, ok := recovered.GetLease(lease.LeaseID)
	require.True(t, ok)
	require.Equal(t, "sandbox-1", recoveredLease.SandboxID)
	require.Equal(t, "/cache/img-a/rootfs.img", recoveredLease.RootfsDev)
	// The lease pins its image on top of the pin count.
	require.Equal(t, 3, snapshot.PinCount+snapshot.LeaseCount)

	// Release after recovery works against the rebuilt lease.
	require.NoError(t, recovered.ReleaseDevices(identity("req-4", "pod-1"), lease.LeaseID))
	require.Equal(t, 0, recovered.Snapshot().LeaseCount)
	require.Equal(t, 2, recovered.Snapshot().PinCount)
}

func TestRecoverDropsInFlightIntent(t *testing.T) {
	root := t.TempDir()
	state, err := New(root)
	require.NoError(t, err)
	journalPath := filepath.Join(root, "agent", JournalFileName)
	// Simulate a crash after the intent line of a pin that never completed.
	require.NoError(t, os.WriteFile(journalPath, []byte(
		`{"requestId":"req-1","op":"pin-image","podUid":"pod-1","namespace":"tenant-a","args":{"image":"img-a"},"at":"2026-08-27T00:00:00Z"}`+"\n",
	), 0o640))
	require.NoError(t, state.Close())

	recovered, err := New(root)
	require.NoError(t, err)
	defer recovered.Close()
	require.Equal(t, 0, recovered.Snapshot().PinCount)
	// The dropped intent is not idempotent: the op can run again.
	digest, err := recovered.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	require.Equal(t, pinDigest("img-a"), digest)
}

func TestRecoverTruncatesPartialTail(t *testing.T) {
	root := t.TempDir()
	state, err := New(root)
	require.NoError(t, err)
	journalPath := filepath.Join(root, "agent", JournalFileName)
	complete := `{"requestId":"req-1","op":"pin-image","podUid":"pod-1","args":{"image":"img-a"},"at":"2026-08-27T00:00:00Z"}` + "\n" +
		`{"requestId":"req-1","op":"pin-image","podUid":"pod-1","result":{"manifestDigest":"d"},"at":"2026-08-27T00:00:00Z"}` + "\n"
	partial := `{"requestId":"req-2","op":"unpin-image","podUid":"pod-1","args":{"image":"img-`
	require.NoError(t, os.WriteFile(journalPath, []byte(complete+partial), 0o640))
	require.NoError(t, state.Close())

	recovered, err := New(root)
	require.NoError(t, err)
	defer recovered.Close()
	require.Equal(t, 1, recovered.Snapshot().PinCount)
	require.Equal(t, 1, recovered.Snapshot().ImageCount)
	payload, err := os.ReadFile(journalPath)
	require.NoError(t, err)
	require.Equal(t, complete, string(payload))
}

func TestRecoverFailsOnMidFileCorruption(t *testing.T) {
	root := t.TempDir()
	state, err := New(root)
	require.NoError(t, err)
	journalPath := filepath.Join(root, "agent", JournalFileName)
	require.NoError(t, os.WriteFile(journalPath, []byte(
		`{"requestId":"req-1","op":"pin-image","podUid":"pod-1","args":{"image":"img-a"},"at":"2026-08-27T00:00:00Z"}`+"\n"+
			"this is not json\n"+
			`{"requestId":"req-2","op":"pin-image","podUid":"pod-1","args":{"image":"img-b"},"at":"2026-08-27T00:00:00Z"}`+"\n",
	), 0o640))
	require.NoError(t, state.Close())

	_, err = New(root)
	require.Error(t, err)
}

func TestSnapshotHealthCounts(t *testing.T) {
	state := newTestState(t)
	_, err := state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	_, err = state.PinImage(identity("req-2", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	_, err = state.LeaseDevices(identity("req-3", "pod-1"), "sandbox-1", "img-a", 4096, false, func(leaseID string) (agentprotocol.Lease, error) {
		return agentprotocol.Lease{
			LeaseID: leaseID, SandboxID: "sandbox-1", Image: "img-a", PodUID: "pod-1", RootfsDev: "/r", CreatedAt: fixedTime,
		}, nil
	})
	require.NoError(t, err)

	snapshot := state.Snapshot()
	require.Equal(t, 1, snapshot.ImageCount)
	require.Equal(t, 1, snapshot.LeaseCount)
	require.Equal(t, 2, snapshot.PinCount)
	require.Len(t, snapshot.Leases, 1)
}

func TestFailedSideEffectIsRetryable(t *testing.T) {
	state := newTestState(t)
	calls := 0
	_, err := state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		calls++
		return "", fmt.Errorf("transient failure")
	})
	require.Error(t, err)
	require.Equal(t, 0, state.Snapshot().PinCount)

	digest, err := state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		calls++
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	require.Equal(t, pinDigest("img-a"), digest)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, state.Snapshot().PinCount)
}

func TestJournalReplayOrderingAcrossOps(t *testing.T) {
	root := t.TempDir()
	state, err := New(root, WithUUID(func() (string, error) { return "lease-1", nil }))
	require.NoError(t, err)
	_, err = state.PinImage(identity("req-1", "pod-1"), "img-a", func() (string, error) {
		return pinDigest("img-a"), nil
	})
	require.NoError(t, err)
	lease, err := state.LeaseDevices(identity("req-2", "pod-1"), "sandbox-1", "img-a", 4096, false, func(leaseID string) (agentprotocol.Lease, error) {
		return agentprotocol.Lease{
			LeaseID: leaseID, SandboxID: "sandbox-1", Image: "img-a", PodUID: "pod-1", RootfsDev: "/r", CreatedAt: fixedTime,
		}, nil
	})
	require.NoError(t, err)
	require.NoError(t, state.ReleaseDevices(identity("req-3", "pod-1"), lease.LeaseID))
	require.NoError(t, state.Close())

	recovered, err := New(root)
	require.NoError(t, err)
	defer recovered.Close()
	snapshot := recovered.Snapshot()
	require.Equal(t, 0, snapshot.LeaseCount)
	require.Equal(t, 1, snapshot.PinCount)
}
