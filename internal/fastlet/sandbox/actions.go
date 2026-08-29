package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletaction "fast-sandbox/internal/fastlet/action"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// ReconcileBindings applies Controller-owned desired state after the
// runtime is ready. Identity validation and the metadata pointer fence prevent
// a delayed request from mutating a replacement runtime.
func (m *SandboxManager) ReconcileBindings(ctx context.Context, req *fastletapi.ReconcileBindingsRequest) (*fastletapi.ReconcileBindingsResponse, error) {
	if req == nil || req.SpecGeneration <= 0 {
		failure := fastletError(fastletapi.ErrorConflict, "positive specGeneration is required", false)
		return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
	}
	if failure := m.validateIdentityTarget(&req.Identity); failure != nil {
		return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
	}
	m.mu.Lock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata == nil {
		m.mu.Unlock()
		failure := fastletError(fastletapi.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
		return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
	}
	if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
		m.mu.Unlock()
		return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
	}
	if metadata.Phase == "creating" || metadata.Phase == "terminating" || metadata.Phase == "deleting" {
		m.mu.Unlock()
		failure := fastletError(fastletapi.ErrorInProgress, "runtime is not ready for Sandbox Actions", true)
		return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
	}
	actionManager := m.actionManager
	attachment := actionAttachment(metadata)
	m.mu.Unlock()
	if actionManager == nil {
		if len(req.ActionBindings) == 0 {
			m.mu.Lock()
			if m.sandboxes[metadata.SandboxID] != metadata {
				m.mu.Unlock()
				failure := fastletError(fastletapi.ErrorStaleGeneration, "Sandbox changed while Actions were reconciling", true)
				return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
			}
			metadata.AcceptedGeneration = req.SpecGeneration
			metadata.AppliedGeneration = req.SpecGeneration
			status := m.sandboxStatusLocked(metadata)
			m.mu.Unlock()
			return &fastletapi.ReconcileBindingsResponse{Sandbox: &status}, nil
		}
		failure := fastletError(fastletapi.ErrorActionUnavailable, "Fastlet has no Sandbox Action configuration", false)
		return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
	}
	desired := make([]fastletaction.DesiredInput, 0, len(req.ActionBindings))
	for _, input := range req.ActionBindings {
		desired = append(desired, fastletaction.DesiredInput{Handler: input.Handler, Input: input.Input, Digest: input.InputDigest})
	}
	statuses, appliedGeneration, reconcileErr := actionManager.Reconcile(ctx, attachment, req.SpecGeneration, desired)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sandboxes[metadata.SandboxID] != metadata {
		failure := fastletError(fastletapi.ErrorStaleGeneration, "Sandbox changed while Actions were reconciling", true)
		return &fastletapi.ReconcileBindingsResponse{Error: failure}, failure
	}
	metadata.ActionBindingStatuses = append(metadata.ActionBindingStatuses[:0], statuses...)
	metadata.AcceptedGeneration = req.SpecGeneration
	metadata.AppliedGeneration = appliedGeneration
	if reconcileErr != nil {
		if metadata.Phase == "running" || metadata.Phase == "action-pending" || metadata.Phase == "action-unavailable" {
			metadata.Phase = "action-unavailable"
		}
		m.recordDiagnosticLocked(metadata.SandboxID, "error", "action", "action-unavailable", reconcileErr.Error())
	} else if (len(statuses) == 0 || actionStatusesReady(statuses)) && (metadata.Phase == "action-pending" || metadata.Phase == "action-unavailable") {
		metadata.Phase = "running"
		m.recordDiagnosticLocked(metadata.SandboxID, "info", "action", "running", "all Sandbox Actions are ready")
	}
	status := m.sandboxStatusLocked(metadata)
	m.signalReadinessChangedLocked()
	if reconcileErr != nil {
		failure := fastletErrorWithCause(fastletapi.ErrorActionUnavailable, fmt.Sprintf("reconcile Sandbox Actions: %v", reconcileErr), true, reconcileErr)
		return &fastletapi.ReconcileBindingsResponse{Sandbox: &status, Error: failure}, failure
	}
	return &fastletapi.ReconcileBindingsResponse{Sandbox: &status}, nil
}

func desiredActionInputs(bindings []fastletapi.ActionBindingInput) []fastletaction.DesiredInput {
	desired := make([]fastletaction.DesiredInput, 0, len(bindings))
	for _, input := range bindings {
		desired = append(desired, fastletaction.DesiredInput{Handler: input.Handler, Input: input.Input, Digest: input.InputDigest})
	}
	return desired
}

func actionAttachment(metadata *SandboxMetadata) fastletaction.Attachment {
	return fastletaction.Attachment{
		ID: actionAttachmentID(metadata), InstanceGeneration: metadata.InstanceGeneration,
		AssignmentAttempt: metadata.AssignmentAttempt, RuntimeInstanceID: metadata.RuntimeInstanceID, RouteGeneration: metadata.RouteGeneration,
		SandboxUID: metadata.SandboxID, SandboxName: metadata.ClaimName, Namespace: metadata.ClaimNamespace,
		IP: metadata.NetworkIP, Gateway: metadata.NetworkGateway, PrivateCIDR: metadata.NetworkPrivateCIDR, HostVeth: metadata.NetworkHostVeth,
	}
}

func (m *SandboxManager) registerDesiredActions(metadata *SandboxMetadata, generation int64, bindings []fastletapi.ActionBindingInput) error {
	if m.actionManager == nil {
		if len(bindings) != 0 {
			return errors.New("Fastlet has no Action Handler configuration")
		}
		return nil
	}
	return m.actionManager.RegisterDesired(actionAttachment(metadata), generation, desiredActionInputs(bindings))
}

func (m *SandboxManager) recordActionHook(metadata *SandboxMetadata, hook actionapi.LifecycleHook, sequence int64) {
	if m.actionManager == nil {
		return
	}
	if err := m.actionManager.RecordHook(metadata.SandboxID, actionAttachment(metadata), hook, sequence); err != nil {
		m.recordDiagnostic(metadata.SandboxID, "error", "action", "hook-pending", fmt.Sprintf("record lifecycle Hook %s: %v", hook, err))
	}
}

// actionAttachmentID is deliberately opaque outside Fastlet. It changes when
// the concrete runtime, assignment, or published route is replaced.
func actionAttachmentID(metadata *SandboxMetadata) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%s",
		metadata.FastletPodUID, metadata.AssignmentAttempt, metadata.InstanceGeneration,
		metadata.RouteGeneration, metadata.RuntimeInstanceID)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// actionStateChanged projects the Action Manager's lock-free snapshot into the
// Sandbox observation and wakes local READY waiters. It is called after
// background retries and Handler-incarnation changes, so Create completion
// never depends on a Controller/CRD status round trip.
func (m *SandboxManager) actionStateChanged(sandboxUID string) {
	if m.actionManager == nil {
		return
	}
	statuses, generation := m.actionManager.Statuses(sandboxUID)
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata := m.sandboxes[sandboxUID]
	if metadata == nil || metadata.Phase == "terminating" || metadata.Phase == "deleting" || metadata.Phase == "delete-failed" {
		return
	}
	metadata.ActionBindingStatuses = append(metadata.ActionBindingStatuses[:0], statuses...)
	metadata.AppliedGeneration = generation
	ready := generation > 0 && actionStatusesReady(statuses)
	dataPlaneReady := routeReadyForPhase(metadata.Phase)
	if dataPlaneReady && ready {
		metadata.Phase = "running"
	} else if dataPlaneReady {
		metadata.Phase = "action-pending"
		for _, status := range statuses {
			if status.State == string(apiv1alpha2.ActionFailed) {
				metadata.Phase = "action-unavailable"
				break
			}
		}
	}
	m.signalReadinessChangedLocked()
}
