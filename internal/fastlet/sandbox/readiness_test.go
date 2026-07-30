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

func TestWaitSandboxReadyUsesStateNotification(t *testing.T) {
	identity := fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
		RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 2,
	}
	metadata := &SandboxMetadata{
		SandboxSpec: fastletapi.SandboxSpec{
			SandboxID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
			RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 2,
		},
		Phase: "route-pending",
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

	result := make(chan *fastletapi.WaitSandboxReadyResponse, 1)
	errors := make(chan error, 1)
	go func() {
		response, err := manager.WaitSandboxReady(context.Background(), &fastletapi.WaitSandboxReadyRequest{
			Identity: identity, ComponentName: "execd",
		})
		result <- response
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
	case response := <-result:
		require.NoError(t, <-errors)
		require.True(t, response.Ready)
		require.Equal(t, int64(2), response.Sandbox.InfraDiagnostics[0].ObservedRouteGeneration)
	case <-time.After(time.Second):
		t.Fatal("wait was not released by readiness notification")
	}
}

func TestWaitSandboxReadyNoWaitReturnsCurrentState(t *testing.T) {
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
			Phase: "infra-pending",
		}},
		readinessChanged: make(chan struct{}), clock: realClock{},
		diagnostics: make(map[string][]fastletapi.SandboxDiagnosticEvent),
	}
	response, err := manager.WaitSandboxReady(context.Background(), &fastletapi.WaitSandboxReadyRequest{
		Identity: identity, DataPlane: true, NoWait: true,
	})
	require.NoError(t, err)
	require.False(t, response.Ready)
	require.Equal(t, "infra-pending", response.Sandbox.Phase)
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
	manager.mu.RUnlock()
	publisher.mu.Lock()
	require.Len(t, publisher.removed, 1)
	require.Equal(t, int64(2), publisher.removed[0].RouteGeneration)
	publisher.mu.Unlock()

	_, err = manager.WaitSandboxReady(context.Background(), &fastletapi.WaitSandboxReadyRequest{
		Identity: fastletapi.SandboxIdentity{
			SandboxUID: "sandbox-a", FastletPodUID: "pod-a", InstanceGeneration: 1,
			RuntimeInstanceID: "runtime-a", AssignmentAttempt: 1, RouteGeneration: 2,
		},
		ComponentName: "execd",
	})
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorInfraUnavailable, failure.Code)
	require.True(t, failure.Retryable)
}
