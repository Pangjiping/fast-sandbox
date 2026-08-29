package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

func TestWaitUntilSandboxReadyUsesLocalStateNotification(t *testing.T) {
	identity := fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
		RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 2,
	}
	metadata := &SandboxMetadata{
		SandboxSpec: fastletapi.SandboxSpec{
			SandboxID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
			RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 2,
		},
		Phase:             "route-pending",
		AppliedGeneration: 6,
		InfraDiagnostics: []fastletinfra.ComponentDiagnostic{{
			Component: "execd", State: "Ready",
		}},
		InfraServices: []fastletinfra.ServiceEndpoint{{
			Component: "execd", Protocol: "HTTP", Port: 44772,
		}},
	}
	manager := &SandboxManager{
		fastletPodUID: "pod-a", sandboxes: map[string]*SandboxMetadata{"sandbox-a": metadata},
		readinessChanged: make(chan struct{}), clock: realClock{},
		diagnostics: make(map[string][]fastletapi.SandboxDiagnosticEvent),
	}

	result := make(chan *fastletapi.SandboxStatus, 1)
	errors := make(chan error, 1)
	go func() {
		status, err := manager.waitUntilSandboxReady(context.Background(), identity, 6)
		result <- status
		errors <- err
	}()

	select {
	case <-result:
		t.Fatal("wait returned before route publication")
	case <-time.After(10 * time.Millisecond):
	}

	manager.mu.Lock()
	metadata.Phase = "running"
	manager.recordDiagnosticLocked("sandbox-a", "info", "route", "running", "route published")
	manager.mu.Unlock()

	select {
	case status := <-result:
		require.NoError(t, <-errors)
		require.Equal(t, fastletapi.RuntimeStateReady, status.Runtime.State)
		require.Equal(t, fastletapi.DataPlaneStateReady, status.DataPlane.State)
		require.Equal(t, int64(2), status.InfraComponents[0].ObservedRouteGeneration)
	case <-time.After(time.Second):
		t.Fatal("wait was not released by a local readiness change")
	}
}

func TestUnknownInternalPhaseProjectsExplicitFailure(t *testing.T) {
	runtime, dataPlane := observationsForPhase("unexpected-phase", false)
	require.Equal(t, fastletapi.RuntimeStateFailed, runtime.State)
	require.Equal(t, fastletapi.DataPlaneStateFailed, dataPlane.State)
	require.Contains(t, runtime.Message, "unexpected-phase")
	require.Equal(t, runtime.Message, dataPlane.Message)
}

func TestWaitUntilSandboxReadyWaitsForExpectedGeneration(t *testing.T) {
	identity := fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
		RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 1,
	}
	metadata := &SandboxMetadata{
		SandboxSpec: fastletapi.SandboxSpec{
			SandboxID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
			RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 1,
		},
		Phase: "running", AppliedGeneration: 3,
	}
	manager := &SandboxManager{
		fastletPodUID: "pod-a", sandboxes: map[string]*SandboxMetadata{"sandbox-a": metadata},
		readinessChanged: make(chan struct{}), clock: realClock{}, diagnostics: make(map[string][]fastletapi.SandboxDiagnosticEvent),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := manager.waitUntilSandboxReady(ctx, identity, 4)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitUntilSandboxReadyFailsWhenSandboxTerminates(t *testing.T) {
	identity := fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
		RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 1,
	}
	manager := &SandboxManager{
		fastletPodUID: "pod-a",
		sandboxes: map[string]*SandboxMetadata{"sandbox-a": {
			SandboxSpec: fastletapi.SandboxSpec{
				SandboxID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
				RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 1,
			},
			Phase: "terminating", AppliedGeneration: 1,
		}},
		readinessChanged: make(chan struct{}), clock: realClock{}, diagnostics: make(map[string][]fastletapi.SandboxDiagnosticEvent),
	}
	_, err := manager.waitUntilSandboxReady(context.Background(), identity, 1)
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorConflict, failure.Code)
}

func TestHealthRegressionRevokesRouteAndReadiness(t *testing.T) {
	runtime := newAdmissionRuntime()
	metadata := &SandboxMetadata{
		SandboxSpec: fastletapi.SandboxSpec{
			SandboxID: "sandbox-a", ClaimUID: "claim-a", ClaimNamespace: "fast-sandbox",
			FastletPodUID: "pod-a", InstanceGeneration: 1, RuntimeInstanceID: "runtime-a",
			AssignmentAttempt: 1, RouteGeneration: 2,
		},
		Phase: "running",
		InfraDiagnostics: []fastletinfra.ComponentDiagnostic{{
			Component: "execd", State: "Failed", Message: "connection refused",
		}},
		InfraServices: []fastletinfra.ServiceEndpoint{{
			Component: "execd", Protocol: "HTTP", Port: 44772,
		}},
	}
	runtime.sandboxes["sandbox-a"] = metadata
	publisher := &admissionRoutePublisher{}
	manager, err := NewSandboxManagerWithConfig(runtime, SandboxManagerConfig{
		Capacity: 1, FastletPodUID: "pod-a", RoutePublisher: publisher,
	})
	require.NoError(t, err)
	manager.sandboxes["sandbox-a"] = metadata

	manager.markDataPlaneUnhealthy(metadata, errors.New("connection refused"))

	manager.mu.RLock()
	require.Equal(t, "infra-unavailable", metadata.Phase)
	status := manager.sandboxStatusLocked(metadata)
	manager.mu.RUnlock()
	require.Equal(t, fastletapi.RuntimeStateReady, status.Runtime.State)
	require.Equal(t, fastletapi.DataPlaneStateUnavailable, status.DataPlane.State)
	publisher.mu.Lock()
	require.Len(t, publisher.removed, 1)
	require.Equal(t, int64(2), publisher.removed[0].RouteGeneration)
	publisher.mu.Unlock()
}
