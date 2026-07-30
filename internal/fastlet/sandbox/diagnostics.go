package sandbox

import (
	"context"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const (
	maxDiagnosticSandboxes = 1024
	maxDiagnosticEvents    = 128
	defaultDiagnosticLimit = 50
)

func (m *SandboxManager) recordDiagnostic(sandboxID, level, source, phase, message string) {
	if sandboxID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordDiagnosticLocked(sandboxID, level, source, phase, message)
}

func (m *SandboxManager) recordDiagnosticLocked(sandboxID, level, source, phase, message string) {
	events, found := m.diagnostics[sandboxID]
	if !found {
		if len(m.diagnostics) >= maxDiagnosticSandboxes && len(m.diagnosticOrder) > 0 {
			oldest := m.diagnosticOrder[0]
			m.diagnosticOrder = m.diagnosticOrder[1:]
			delete(m.diagnostics, oldest)
		}
		m.diagnosticOrder = append(m.diagnosticOrder, sandboxID)
	}
	events = append(events, fastletapi.SandboxDiagnosticEvent{
		Timestamp: m.clock.Now(), Level: level, Source: source, Phase: phase, Message: message,
	})
	if len(events) > maxDiagnosticEvents {
		events = append([]fastletapi.SandboxDiagnosticEvent(nil), events[len(events)-maxDiagnosticEvents:]...)
	}
	m.diagnostics[sandboxID] = events
	m.signalReadinessChangedLocked()
}

func (m *SandboxManager) signalReadinessChangedLocked() {
	if m.readinessChanged == nil {
		m.readinessChanged = make(chan struct{})
		return
	}
	close(m.readinessChanged)
	m.readinessChanged = make(chan struct{})
}

// WaitSandboxReady waits on manager state transitions rather than sampling
// diagnostics. A named component is ready only after its health check passed
// and the instance-fenced Fastlet Proxy route was published.
func (m *SandboxManager) WaitSandboxReady(ctx context.Context, req *fastletapi.WaitSandboxReadyRequest) (*fastletapi.WaitSandboxReadyResponse, error) {
	if req == nil || (req.ComponentName == "" && !req.DataPlane) || (req.ComponentName != "" && req.DataPlane) {
		failure := fastletError(fastletapi.ErrorConflict, "exactly one readiness target is required", false)
		return &fastletapi.WaitSandboxReadyResponse{Error: failure}, failure
	}
	if failure := m.validateIdentityTarget(&req.Identity); failure != nil {
		return &fastletapi.WaitSandboxReadyResponse{Error: failure}, failure
	}
	for {
		m.mu.Lock()
		metadata := m.sandboxes[req.Identity.SandboxUID]
		if metadata == nil {
			m.mu.Unlock()
			failure := fastletError(fastletapi.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
			return &fastletapi.WaitSandboxReadyResponse{Error: failure}, failure
		}
		if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
			m.mu.Unlock()
			return &fastletapi.WaitSandboxReadyResponse{Error: failure}, failure
		}
		status := sandboxStatus(metadata)
		ready := metadata.Phase == "running"
		if req.ComponentName != "" {
			ready = false
			for _, component := range status.InfraDiagnostics {
				if component.Component == req.ComponentName &&
					component.State == string(apiv1alpha2.InfraComponentReady) &&
					component.ObservedRouteGeneration == metadata.RouteGeneration {
					ready = metadata.Phase == "running"
					break
				}
			}
		}
		if ready {
			m.mu.Unlock()
			return &fastletapi.WaitSandboxReadyResponse{Sandbox: &status, Ready: true}, nil
		}
		switch metadata.Phase {
		case "terminating", "deleting", "delete-failed", "create-cleanup", "create-cleanup-failed":
			m.mu.Unlock()
			failure := fastletError(fastletapi.ErrorConflict, "Sandbox stopped before the requested data plane became ready", false)
			return &fastletapi.WaitSandboxReadyResponse{Sandbox: &status, Error: failure}, failure
		case "infra-unavailable", "route-unavailable":
			m.mu.Unlock()
			failure := fastletError(fastletapi.ErrorInfraUnavailable, "Sandbox data plane is temporarily unavailable", true)
			return &fastletapi.WaitSandboxReadyResponse{Sandbox: &status, Error: failure}, failure
		}
		if req.NoWait {
			m.mu.Unlock()
			return &fastletapi.WaitSandboxReadyResponse{Sandbox: &status, Ready: false}, nil
		}
		changed := m.readinessChanged
		if changed == nil {
			m.readinessChanged = make(chan struct{})
			changed = m.readinessChanged
		}
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (m *SandboxManager) SandboxDiagnostics(req *fastletapi.SandboxDiagnosticsRequest) (*fastletapi.SandboxDiagnosticsResponse, error) {
	if failure := m.validateIdentityTarget(diagnosticsIdentity(req)); failure != nil {
		return &fastletapi.SandboxDiagnosticsResponse{Error: failure}, failure
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	identity := req.Identity
	metadata := m.sandboxes[identity.SandboxUID]
	if metadata != nil {
		if failure := validateIdentityFence(m.fastletPodUID, metadata, identity); failure != nil {
			return &fastletapi.SandboxDiagnosticsResponse{Error: failure}, failure
		}
	} else {
		tombstone, deleted := m.tombstones[identity.SandboxUID]
		if !deleted || tombstone.InstanceGeneration != identity.InstanceGeneration ||
			tombstone.AssignmentAttempt != identity.AssignmentAttempt || tombstone.RuntimeInstanceID != identity.RuntimeInstanceID {
			failure := fastletError(fastletapi.ErrorNotFound, "Sandbox diagnostics are not retained by this Fastlet", false)
			return &fastletapi.SandboxDiagnosticsResponse{Error: failure}, failure
		}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultDiagnosticLimit
	}
	if limit > maxDiagnosticEvents {
		limit = maxDiagnosticEvents
	}
	events := m.diagnostics[identity.SandboxUID]
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	response := &fastletapi.SandboxDiagnosticsResponse{Events: append([]fastletapi.SandboxDiagnosticEvent(nil), events...)}
	if metadata != nil {
		status := sandboxStatus(metadata)
		response.Sandbox = &status
	}
	return response, nil
}

func diagnosticsIdentity(req *fastletapi.SandboxDiagnosticsRequest) *fastletapi.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}
