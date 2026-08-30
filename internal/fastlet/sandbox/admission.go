package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/observability"
	actionapi "fast-sandbox/internal/protocol/action"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type createReservation struct {
	placeholder     *SandboxMetadata
	cleanupExisting *SandboxMetadata
	existing        *fastletapi.CreateSandboxResponse
	admission       fastletapi.AdmissionStatus
	disposition     fastletapi.CreateDisposition
}

func (m *SandboxManager) CreateSandbox(ctx context.Context, req *fastletapi.CreateSandboxRequest) (_ *fastletapi.CreateSandboxResponse, resultErr error) {
	ctx = withCreateIdentity(ctx, req)
	ctx, span := observability.Start(ctx, "fastlet.create Sandbox")
	started := time.Now()
	defer func() {
		observability.End(span, resultErr)
		recordAdmission("create", resultErr)
	}()
	_, finishValidation := startFastletCreateStage(ctx, m.runtimeName, "validation")
	spec, validationFailure := m.prepareCreateSpec(req)
	finishValidation(validationFailure)
	if validationFailure != nil {
		return createFailure(validationFailure, fastletapi.AdmissionStatus{})
	}

	_, finishAdmission := startFastletCreateStage(ctx, m.runtimeName, "admission")
	reservation, admissionErr := m.reserveSandboxForCreate(req, spec)
	finishAdmission(admissionErr)
	if admissionErr != nil {
		if reservation.existing != nil {
			return reservation.existing, admissionErr
		}
		failure, ok := admissionErr.(*fastletapi.FastletError)
		if !ok {
			failure = fastletErrorWithCause(fastletapi.ErrorUnknownOutcome, admissionErr.Error(), true, admissionErr)
		}
		return createFailureWithDisposition(failure, reservation.admission, reservation.disposition)
	}
	if reservation.cleanupExisting != nil {
		return m.retryFailedCreateCleanup(ctx, req, &spec, reservation.cleanupExisting)
	}
	if reservation.existing != nil {
		return m.finishCreate(ctx, req, reservation.existing)
	}
	placeholder := reservation.placeholder
	admission := reservation.admission
	m.recordDiagnostic(spec.SandboxID, "info", "admission", "creating", "Fastlet admission accepted; atomic runtime creation started")
	if bindingFailure, currentAdmission := m.registerDesiredBindings(placeholder, req); bindingFailure != nil {
		return createFailureWithDisposition(bindingFailure, currentAdmission, fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	metadata, err := m.ensureRuntimeForCreate(ctx, started, &spec)
	if err != nil {
		return m.handleRuntimeCreateFailure(ctx, &spec, placeholder, err)
	}
	status, admission, dataPlaneReady, commitFailure := m.commitRuntimeCreate(req, spec, placeholder, metadata)
	if commitFailure != nil {
		return createFailure(commitFailure, admission)
	}
	m.recordRuntimeReadyAndDispatchHooks(metadata, req, dataPlaneReady)
	m.continueDataPlaneCreation(metadata, started, dataPlaneReady)
	return m.finishCreate(ctx, req, &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionCreated, Sandbox: &status, Admission: admission})
}

func withCreateIdentity(ctx context.Context, req *fastletapi.CreateSandboxRequest) context.Context {
	if req == nil {
		return ctx
	}
	return observability.WithIdentity(ctx, observability.Identity{
		RequestID: req.RequestID, Namespace: req.Identity.Namespace, SandboxName: req.Identity.Name,
		SandboxUID: req.Identity.SandboxUID, FastletPodUID: req.Identity.FastletPodUID,
		InstanceGeneration: req.Identity.InstanceGeneration, AssignmentAttempt: req.Identity.AssignmentAttempt,
		RouteGeneration: req.Identity.RouteGeneration,
	})
}

func (m *SandboxManager) prepareCreateSpec(req *fastletapi.CreateSandboxRequest) (fastletapi.RuntimeSandboxSpec, *fastletapi.FastletError) {
	if failure := m.validateCreateRequest(req); failure != nil {
		return fastletapi.RuntimeSandboxSpec{}, failure
	}
	spec := fastletapi.RuntimeSandboxSpec{
		SandboxSpec:    req.Sandbox,
		SandboxID:      req.Identity.SandboxUID,
		RequestID:      req.RequestID,
		ClaimNamespace: req.Identity.Namespace,
		ClaimName:      req.Identity.Name,
	}
	spec.InstanceGeneration, spec.RuntimeInstanceID = req.Identity.InstanceGeneration, req.Identity.RuntimeInstanceID
	spec.AssignmentAttempt, spec.RouteGeneration = req.Identity.AssignmentAttempt, req.Identity.RouteGeneration
	if spec.RouteGeneration <= 0 {
		spec.RouteGeneration = 1
	}
	spec.FastletPodUID = req.Identity.FastletPodUID
	if err := m.validateProfiles(&spec); err != nil {
		return fastletapi.RuntimeSandboxSpec{}, fastletError(fastletapi.ErrorProfileMismatch, err.Error(), false)
	}
	return spec, nil
}

func (m *SandboxManager) reserveSandboxForCreate(req *fastletapi.CreateSandboxRequest, spec fastletapi.RuntimeSandboxSpec) (createReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := createReservation{admission: m.admissionStatusLocked()}
	reject := func(failure *fastletapi.FastletError, disposition fastletapi.CreateDisposition) (createReservation, error) {
		result.admission = m.admissionStatusLocked()
		result.disposition = disposition
		return result, failure
	}
	if m.recovering || !m.runtimeReady {
		return reject(fastletError(fastletapi.ErrorRuntimeUnavailable, "Fastlet runtime recovery/capability probe is incomplete", true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if !m.infraReady {
		message := m.infraMessage
		if message == "" {
			message = "required Infra Component artifacts are still preparing"
		}
		return reject(fastletError(fastletapi.ErrorInfraUnavailable, message, true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if len(req.ActionBindings) > 0 && m.actionManager == nil {
		return reject(fastletError(fastletapi.ErrorActionUnavailable, "Fastlet has no Action Handler configuration", false), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if existing := m.sandboxes[spec.SandboxID]; existing != nil {
		if existing.Phase == "create-cleanup-failed" {
			result.cleanupExisting = existing
			return result, nil
		}
		response, err := m.createExistingLocked(existing, &spec)
		result.existing = response
		return result, err
	}
	if tombstone, found := m.tombstones[spec.SandboxID]; found && identityAtOrBefore(spec.InstanceGeneration, spec.AssignmentAttempt, tombstone) {
		return reject(fastletError(fastletapi.ErrorGenerationFenced, "Sandbox generation was already deleted", false), fastletapi.CreateDispositionGenerationFenced)
	}
	if m.draining {
		return reject(fastletError(fastletapi.ErrorDraining, m.drainReason, true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if len(m.sandboxes) >= m.capacity {
		return reject(fastletError(fastletapi.ErrorCapacityRejected, "Fastlet admission capacity is exhausted", true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	if !m.runtimeResourceAvailable() {
		return reject(fastletError(fastletapi.ErrorNetworkUnavailable, "Fastlet has no clean runtime/network resource available", true), fastletapi.CreateDispositionRejectedBeforeSideEffects)
	}
	result.placeholder = &SandboxMetadata{
		RuntimeSandboxSpec: spec, Phase: "creating", CreatedAt: m.clock.Now().Unix(), AcceptedGeneration: req.SpecGeneration,
		ActionBindingStatuses: pendingActionBindingStatuses(req.ActionBindings),
	}
	m.sandboxes[spec.SandboxID] = result.placeholder
	result.admission = m.admissionStatusLocked()
	return result, nil
}

func (m *SandboxManager) registerDesiredBindings(placeholder *SandboxMetadata, req *fastletapi.CreateSandboxRequest) (*fastletapi.FastletError, fastletapi.AdmissionStatus) {
	if err := m.registerDesiredActions(placeholder, req.SpecGeneration, req.ActionBindings); err != nil {
		m.mu.Lock()
		if m.sandboxes[placeholder.SandboxID] == placeholder {
			delete(m.sandboxes, placeholder.SandboxID)
		}
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return fastletErrorWithCause(fastletapi.ErrorActionUnavailable, err.Error(), false, err), admission
	}
	return nil, fastletapi.AdmissionStatus{}
}

func (m *SandboxManager) ensureRuntimeForCreate(ctx context.Context, createStarted time.Time, spec *fastletapi.RuntimeSandboxSpec) (*SandboxMetadata, error) {
	runtimeStarted := time.Now()
	runtimeContext, finishRuntime := startFastletCreateStage(ctx, m.runtimeName, "runtime_ensure")
	metadata, err := m.runtime.EnsureSandbox(runtimeContext, spec)
	finishRuntime(err)
	observeRuntimeCreate(m.runtimeName, runtimeStarted, err)
	observeUserProcessStart(m.runtimeName, m.infraRevision, createStarted, metadata)
	return metadata, err
}

func (m *SandboxManager) handleRuntimeCreateFailure(ctx context.Context, spec *fastletapi.RuntimeSandboxSpec, placeholder *SandboxMetadata, runtimeErr error) (*fastletapi.CreateSandboxResponse, error) {
	m.cacheProtection.ProtectHotUntil(spec.Image, m.clock.Now().Add(time.Hour))
	cleanupErr := m.runtime.DeleteSandbox(ctx, spec.SandboxID)
	if m.actionManager != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleanupErr = errors.Join(cleanupErr, m.actionManager.Delete(cleanupCtx, spec.SandboxID))
		cancel()
	}
	m.mu.Lock()
	disposition := fastletapi.CreateDispositionRejectedBeforeSideEffects
	if cleanupErr == nil && m.sandboxes[spec.SandboxID] == placeholder {
		delete(m.sandboxes, spec.SandboxID)
	} else if m.sandboxes[spec.SandboxID] == placeholder {
		placeholder.Phase = "create-cleanup-failed"
		disposition = fastletapi.CreateDispositionFailedNeedsCleanup
	}
	admission := m.admissionStatusLocked()
	m.mu.Unlock()
	code := fastletapi.ErrorRuntimeUnavailable
	if errors.Is(runtimeErr, ErrNetworkUnavailable) {
		code = fastletapi.ErrorNetworkUnavailable
	} else if errors.Is(runtimeErr, ErrInfraUnavailable) {
		code = fastletapi.ErrorInfraUnavailable
	}
	message := runtimeErr.Error()
	if cleanupErr != nil {
		message = fmt.Sprintf("%s; cleanup failed: %v", message, cleanupErr)
	}
	m.recordDiagnostic(spec.SandboxID, "error", "runtime", string(disposition), message)
	return createFailureWithDisposition(fastletErrorWithCause(code, message, true, errors.Join(runtimeErr, cleanupErr)), admission, disposition)
}

func (m *SandboxManager) commitRuntimeCreate(req *fastletapi.CreateSandboxRequest, spec fastletapi.RuntimeSandboxSpec, placeholder, metadata *SandboxMetadata) (fastletapi.SandboxStatus, fastletapi.AdmissionStatus, bool, *fastletapi.FastletError) {
	runtimeSpec := metadata.RuntimeSandboxSpec
	metadata.Phase, metadata.RuntimeSandboxSpec = "infra-pending", spec
	metadata.NetworkSlotID, metadata.NetworkNamespacePath = runtimeSpec.NetworkSlotID, runtimeSpec.NetworkNamespacePath
	metadata.NetworkIP, metadata.NetworkGateway = runtimeSpec.NetworkIP, runtimeSpec.NetworkGateway
	metadata.NetworkDNSPath, metadata.NetworkPrivateCIDR = runtimeSpec.NetworkDNSPath, runtimeSpec.NetworkPrivateCIDR
	metadata.NetworkHostVeth = runtimeSpec.NetworkHostVeth
	metadata.ActionBindingStatuses = pendingActionBindingStatuses(req.ActionBindings)
	metadata.AcceptedGeneration = req.SpecGeneration
	if len(req.ActionBindings) == 0 {
		metadata.AppliedGeneration = req.SpecGeneration
	}
	m.cacheProtection.Protect(spec.Image, fastletcache.ProtectActive)
	m.mu.Lock()
	defer m.mu.Unlock()
	if placeholder.Phase == "terminating" {
		metadata.Phase = "terminating"
		m.sandboxes[spec.SandboxID] = metadata
		admission := m.admissionStatusLocked()
		go m.asyncDelete(spec.SandboxID, metadata)
		return fastletapi.SandboxStatus{}, admission, false, fastletError(fastletapi.ErrorConflict, "Sandbox was deleted while creation was in progress", false)
	}
	m.sandboxes[spec.SandboxID] = metadata
	if m.infraManager == nil && m.routePublisher == nil {
		if len(req.ActionBindings) > 0 {
			metadata.Phase = "action-pending"
		} else {
			metadata.Phase = "running"
		}
		m.recordDiagnosticLocked(spec.SandboxID, "info", "fastlet", "running", "runtime is ready; no asynchronous data-plane initialization is required")
	}
	status := m.sandboxStatusLocked(metadata)
	return status, m.admissionStatusLocked(), metadata.Phase == "running", nil
}

func (m *SandboxManager) recordRuntimeReadyAndDispatchHooks(metadata *SandboxMetadata, req *fastletapi.CreateSandboxRequest, dataPlaneReady bool) {
	if err := m.registerDesiredActions(metadata, req.SpecGeneration, req.ActionBindings); err != nil {
		m.recordDiagnostic(metadata.SandboxID, "error", "action", "binding-pending", err.Error())
	}
	m.recordActionHook(metadata, actionapi.LifecycleHookRuntimeReady, 1)
	if dataPlaneReady {
		m.recordActionHook(metadata, actionapi.LifecycleHookDataPlaneReady, 2)
	}
}

func (m *SandboxManager) continueDataPlaneCreation(metadata *SandboxMetadata, started time.Time, dataPlaneReady bool) {
	if dataPlaneReady {
		observeDataPlaneReady(m.runtimeName, m.infraRevision, started, nil)
	} else if dataPlaneWorkPending(metadata.Phase) {
		m.recordDiagnostic(metadata.SandboxID, "info", "runtime", "infra-pending", "runtime and private network are ready; Infra Component initialization continues asynchronously")
		m.startDataPlaneReconcile(metadata, started)
	} else {
		m.recordDiagnostic(metadata.SandboxID, "info", "action", "action-pending", "runtime and private network are ready; Sandbox Actions are pending")
	}
}

func pendingActionBindingStatuses(bindings []fastletapi.ActionBindingInput) []fastletapi.ActionBindingStatus {
	statuses := make([]fastletapi.ActionBindingStatus, 0, len(bindings))
	for _, binding := range bindings {
		statuses = append(statuses, fastletapi.ActionBindingStatus{Handler: binding.Handler, State: "Pending"})
	}
	return statuses
}

// finishCreate implements the caller-selected completion boundary entirely on
// Fastlet-local state. The public FastPath normalizes its unspecified enum to
// READY. The internal zero value keeps Controller recovery calls non-blocking.
func (m *SandboxManager) finishCreate(ctx context.Context, req *fastletapi.CreateSandboxRequest, response *fastletapi.CreateSandboxResponse) (*fastletapi.CreateSandboxResponse, error) {
	if len(req.ActionBindings) == 0 {
		m.mu.Lock()
		if metadata := m.sandboxes[req.Identity.SandboxUID]; metadata != nil {
			metadata.AppliedGeneration = req.SpecGeneration
			status := m.sandboxStatusLocked(metadata)
			response.Sandbox = &status
		}
		m.mu.Unlock()
	}
	if req.Completion != fastletapi.CreateCompletionReady {
		return response, nil
	}
	ready, err := m.waitUntilSandboxReady(ctx, req.Identity, req.SpecGeneration)
	if ready != nil {
		response.Sandbox = ready
	}
	if err != nil {
		if failure, ok := err.(*fastletapi.FastletError); ok {
			response.Error = failure
		}
		return response, err
	}
	return response, nil
}

// retryFailedCreateCleanup resumes only cleanup that belongs to a failed
// Create attempt. A user-requested delete uses the distinct delete-failed
// phase and can never be resurrected by a delayed Create retry.
func (m *SandboxManager) retryFailedCreateCleanup(ctx context.Context, req *fastletapi.CreateSandboxRequest, requested *fastletapi.RuntimeSandboxSpec, existing *SandboxMetadata) (*fastletapi.CreateSandboxResponse, error) {
	m.mu.Lock()
	if m.sandboxes[requested.SandboxID] != existing {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox changed before failed Create cleanup retry", true), admission)
	}
	identity := fastletapi.SandboxIdentity{
		SandboxUID: requested.SandboxID, Namespace: requested.ClaimNamespace, Name: requested.ClaimName,
		InstanceGeneration: requested.InstanceGeneration, RuntimeInstanceID: requested.RuntimeInstanceID,
		AssignmentAttempt: requested.AssignmentAttempt, RouteGeneration: requested.RouteGeneration,
		FastletPodUID: requested.FastletPodUID,
	}
	if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
		response, err := createFailure(failure, m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	if !sameSandboxClaim(existing, requested) {
		response, err := createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
		m.mu.Unlock()
		return response, err
	}
	existing.Phase = "create-cleanup"
	m.mu.Unlock()

	cleanupErr := m.runtime.DeleteSandbox(ctx, requested.SandboxID)
	m.mu.Lock()
	if m.sandboxes[requested.SandboxID] != existing {
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox changed while failed Create cleanup was retried", true), admission)
	}
	if cleanupErr != nil {
		existing.Phase = "create-cleanup-failed"
		admission := m.admissionStatusLocked()
		m.mu.Unlock()
		message := fmt.Sprintf("retry failed Create cleanup: %v", cleanupErr)
		m.recordDiagnostic(requested.SandboxID, "error", "runtime", string(fastletapi.CreateDispositionFailedNeedsCleanup), message)
		return createFailureWithDisposition(fastletErrorWithCause(fastletapi.ErrorRuntimeUnavailable, message, true, cleanupErr), admission, fastletapi.CreateDispositionFailedNeedsCleanup)
	}
	delete(m.sandboxes, requested.SandboxID)
	m.mu.Unlock()
	m.recordDiagnostic(requested.SandboxID, "info", "runtime", "cleanup-recovered", "failed Create cleanup converged; retrying the same runtime identity")
	return m.CreateSandbox(ctx, req)
}

func (m *SandboxManager) InspectSandbox(req *fastletapi.InspectSandboxRequest) (*fastletapi.InspectSandboxResponse, error) {
	if failure := m.validateIdentityTarget(reqIdentity(req)); failure != nil {
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	m.mu.RLock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	if metadata == nil {
		m.mu.RUnlock()
		failure := fastletError(fastletapi.ErrorNotFound, "Sandbox is not managed by this Fastlet", false)
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
		m.mu.RUnlock()
		return &fastletapi.InspectSandboxResponse{Error: failure}, failure
	}
	status := m.sandboxStatusLocked(metadata)
	actionManager := m.actionManager
	m.mu.RUnlock()

	// The Action Manager owns the freshest Handler observation. In particular,
	// its local probe can invalidate a Binding immediately when a Handler is
	// unavailable or its instance ID changes, before the Controller has had time
	// to project that transition back into SandboxMetadata and CRD status.
	if actionManager != nil {
		if bindings, generation := actionManager.Statuses(req.Identity.SandboxUID); bindings != nil {
			status.ActionBindings = bindings
			status.AppliedGeneration = generation
		}
	}
	return &fastletapi.InspectSandboxResponse{Sandbox: &status}, nil
}

func (m *SandboxManager) DeleteSandbox(req *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	return m.DeleteSandboxContext(context.Background(), req)
}

// DeleteSandboxContext keeps the bounded, best-effort Action Handler cleanup
// attempt ahead of runtime and network teardown. Handler failure is diagnostic
// only and cannot block deletion or finalizer completion.
func (m *SandboxManager) DeleteSandboxContext(ctx context.Context, req *fastletapi.DeleteSandboxRequest) (*fastletapi.DeleteSandboxResponse, error) {
	if failure := m.validateIdentityTarget(deleteIdentity(req)); failure != nil {
		return &fastletapi.DeleteSandboxResponse{Error: failure}, failure
	}
	m.mu.Lock()
	metadata := m.sandboxes[req.Identity.SandboxUID]
	wasCreating := false
	if metadata != nil {
		if failure := validateIdentityFence(m.fastletPodUID, metadata, req.Identity); failure != nil {
			m.mu.Unlock()
			return &fastletapi.DeleteSandboxResponse{Error: failure}, failure
		}
		if metadata.Phase == "terminating" || metadata.Phase == "deleting" {
			m.mu.Unlock()
			return &fastletapi.DeleteSandboxResponse{}, nil
		}
		wasCreating = metadata.Phase == "creating"
		m.cancelDataPlaneReconcileLocked(metadata)
		metadata.Phase = "terminating"
	}
	m.recordTombstoneLocked(req.Identity)
	m.recordDiagnosticLocked(req.Identity.SandboxUID, "info", "admission", "terminating", "declarative deletion accepted; new Binding and Hook work is fenced")
	m.signalReadinessChangedLocked()
	m.mu.Unlock()
	if metadata != nil && m.actionManager != nil {
		deleteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := m.actionManager.Delete(deleteCtx, req.Identity.SandboxUID)
		cancel()
		if err != nil {
			m.recordDiagnostic(req.Identity.SandboxUID, "error", "action", "delete-best-effort-failed", fmt.Sprintf("best-effort delete of Sandbox Action Bindings failed: %v", err))
		}
	}
	if metadata != nil && !wasCreating {
		go m.asyncDelete(req.Identity.SandboxUID, metadata)
	}
	return &fastletapi.DeleteSandboxResponse{}, nil
}

func (m *SandboxManager) Recover(ctx context.Context) error {
	m.mu.Lock()
	m.recovering = true
	m.runtimeReady = false
	m.routeReady = m.routePublisher == nil
	m.mu.Unlock()

	managed, err := m.runtime.ListManagedSandboxes(ctx)
	if err != nil {
		return err
	}
	report := m.runtime.ProbeCapabilities(ctx)
	if report.State != runtimecatalog.CapabilityReady {
		return fmt.Errorf("runtime capability is not ready: %s: %s", report.Reason, report.Message)
	}
	if recoverer, ok := m.runtime.(RuntimeResourceRecoverer); ok {
		if err := recoverer.RecoverRuntimeResources(ctx, managed); err != nil {
			return fmt.Errorf("recover runtime resources: %w", err)
		}
	}
	recovered := make(map[string]*SandboxMetadata, len(managed))
	for _, metadata := range managed {
		if metadata == nil || metadata.SandboxID == "" {
			continue
		}
		if m.fastletPodUID != "" && metadata.FastletPodUID != "" && metadata.FastletPodUID != m.fastletPodUID {
			continue
		}
		if metadata.InstanceGeneration <= 0 {
			metadata.InstanceGeneration = 1
		}
		if metadata.RuntimeInstanceID == "" {
			metadata.RuntimeInstanceID = "legacy-" + metadata.SandboxID
		}
		if metadata.AssignmentAttempt <= 0 {
			metadata.AssignmentAttempt = 1
		}
		if metadata.RouteGeneration <= 0 {
			metadata.RouteGeneration = 1
		}
		if metadata.Phase == "" {
			metadata.Phase = "unknown"
		}
		if m.infraManager != nil {
			if metadata.InfraRevision != m.infraRevision {
				return fmt.Errorf("recovered Sandbox %s Infra revision does not match Fastlet", metadata.SandboxID)
			}
			metadata.Phase = "infra-pending"
		} else if m.actionManager != nil && m.actionManager.Required() {
			metadata.Phase = "action-pending"
		}
		recovered[metadata.SandboxID] = metadata
	}
	if len(recovered) > m.capacity {
		return fmt.Errorf("recovered %d Sandboxes exceeds Fastlet capacity %d", len(recovered), m.capacity)
	}
	publications := make([]RoutePublication, 0, len(recovered))
	pendingInfra := false
	for _, metadata := range recovered {
		if m.infraManager != nil {
			pendingInfra = true
			continue
		}
		publication, err := m.routePublication(metadata)
		if err != nil {
			return fmt.Errorf("recover route for Sandbox %s: %w", metadata.SandboxID, err)
		}
		if m.routePublisher != nil {
			publications = append(publications, publication)
		}
	}
	if m.routePublisher != nil && !pendingInfra {
		if err := m.routePublisher.ReconcileRoutes(ctx, publications); err != nil {
			return fmt.Errorf("reconcile Fastlet Proxy routes: %w", err)
		}
	}
	activeImages := make([]string, 0, len(recovered))
	for _, metadata := range recovered {
		activeImages = append(activeImages, metadata.Image)
	}
	m.cacheProtection.Replace(fastletcache.ProtectActive, activeImages)
	m.mu.Lock()
	m.sandboxes = recovered
	m.recovering = false
	m.runtimeReady = true
	m.routeReady = m.routePublisher == nil || !pendingInfra
	m.mu.Unlock()
	for _, metadata := range recovered {
		m.recordActionHook(metadata, actionapi.LifecycleHookRuntimeReady, 1)
		if m.infraManager == nil {
			m.recordActionHook(metadata, actionapi.LifecycleHookDataPlaneReady, 2)
		}
	}
	return nil
}

func (m *SandboxManager) runtimeResourceAvailable() bool {
	admission, ok := m.runtime.(RuntimeResourceAdmission)
	return !ok || admission.RuntimeResourceAvailable()
}

func (m *SandboxManager) SetDraining(draining bool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.draining = draining
	m.drainReason = reason
}

func (m *SandboxManager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.recovering && m.runtimeReady && m.routeReady && !m.draining && (m.actionManager == nil || m.actionManager.Ready())
}

func (m *SandboxManager) RuntimeReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.recovering && m.runtimeReady
}

func (m *SandboxManager) State() (fastletapi.AdmissionStatus, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admissionStatusLocked(), m.recovering, m.draining
}

func (m *SandboxManager) createExistingLocked(existing *SandboxMetadata, requested *fastletapi.RuntimeSandboxSpec) (*fastletapi.CreateSandboxResponse, error) {
	identity := fastletapi.SandboxIdentity{
		SandboxUID: requested.SandboxID, Namespace: requested.ClaimNamespace, Name: requested.ClaimName,
		InstanceGeneration: requested.InstanceGeneration, RuntimeInstanceID: requested.RuntimeInstanceID, AssignmentAttempt: requested.AssignmentAttempt,
		RouteGeneration: requested.RouteGeneration, FastletPodUID: requested.FastletPodUID,
	}
	if failure := validateIdentityFence(m.fastletPodUID, existing, identity); failure != nil {
		return createFailure(failure, m.admissionStatusLocked())
	}
	if !sameSandboxClaim(existing, requested) {
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox UID is already bound to a different claim/profile", false), m.admissionStatusLocked())
	}
	status := m.sandboxStatusLocked(existing)
	if existing.Phase == "creating" {
		failure := fastletError(fastletapi.ErrorInProgress, "Sandbox creation is already in progress", true)
		return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionInProgress, Sandbox: &status, Admission: m.admissionStatusLocked(), Error: failure}, failure
	}
	if existing.Phase == "terminating" || existing.Phase == "deleting" {
		return createFailure(fastletError(fastletapi.ErrorConflict, "Sandbox deletion is already in progress", true), m.admissionStatusLocked())
	}
	switch existing.Phase {
	case "infra-pending", "initializing-infra", "infra-unavailable", "route-pending", "publishing-route", "route-unavailable", "action-pending", "action-unavailable":
		return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionExisting, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
	}
	if existing.Phase != "running" {
		return createFailure(fastletError(fastletapi.ErrorRuntimeUnavailable, fmt.Sprintf("managed Sandbox runtime is %s, not running", existing.Phase), true), m.admissionStatusLocked())
	}
	return &fastletapi.CreateSandboxResponse{Disposition: fastletapi.CreateDispositionExisting, Sandbox: &status, Admission: m.admissionStatusLocked()}, nil
}

func (m *SandboxManager) validateIdentityTarget(identity *fastletapi.SandboxIdentity) *fastletapi.FastletError {
	if identity == nil || identity.SandboxUID == "" || identity.InstanceGeneration <= 0 || identity.RuntimeInstanceID == "" || identity.AssignmentAttempt <= 0 {
		return fastletError(fastletapi.ErrorConflict, "sandboxUid, runtimeInstanceId, positive instanceGeneration, and positive assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && identity.FastletPodUID != m.fastletPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	return nil
}

func reqIdentity(req *fastletapi.InspectSandboxRequest) *fastletapi.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}

func deleteIdentity(req *fastletapi.DeleteSandboxRequest) *fastletapi.SandboxIdentity {
	if req == nil {
		return nil
	}
	return &req.Identity
}

func validateIdentityFence(expectedPodUID string, existing *SandboxMetadata, requested fastletapi.SandboxIdentity) *fastletapi.FastletError {
	if expectedPodUID != "" && requested.FastletPodUID != expectedPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	if requested.InstanceGeneration < existing.InstanceGeneration ||
		(requested.InstanceGeneration == existing.InstanceGeneration && requested.AssignmentAttempt < existing.AssignmentAttempt) {
		return fastletError(fastletapi.ErrorStaleGeneration, "request generation/assignment attempt is older than the managed Sandbox", false)
	}
	if requested.InstanceGeneration > existing.InstanceGeneration || requested.AssignmentAttempt > existing.AssignmentAttempt {
		return fastletError(fastletapi.ErrorConflict, "newer generation/assignment requires the old runtime to be deleted first", true)
	}
	if requested.RuntimeInstanceID != existing.RuntimeInstanceID {
		return fastletError(fastletapi.ErrorConflict, "runtimeInstanceId conflicts with the managed Sandbox", false)
	}
	requestedRouteGeneration := requested.RouteGeneration
	if requestedRouteGeneration <= 0 {
		requestedRouteGeneration = existing.RouteGeneration
	}
	if requestedRouteGeneration < existing.RouteGeneration {
		return fastletError(fastletapi.ErrorStaleGeneration, "request route generation is older than the managed Sandbox", false)
	}
	if requestedRouteGeneration > existing.RouteGeneration {
		return fastletError(fastletapi.ErrorConflict, "newer route generation requires the old runtime to be deleted first", true)
	}
	return nil
}

func sameSandboxClaim(existing *SandboxMetadata, requested *fastletapi.RuntimeSandboxSpec) bool {
	return existing.ClaimNamespace == requested.ClaimNamespace && existing.ClaimName == requested.ClaimName &&
		existing.RuntimeProfileHash == requested.RuntimeProfileHash && existing.ResourceProfileHash == requested.ResourceProfileHash &&
		existing.InfraRevision == requested.InfraRevision
}

func identityAtOrBefore(generation, attempt int64, highWater fastletapi.SandboxIdentity) bool {
	return generation < highWater.InstanceGeneration ||
		(generation == highWater.InstanceGeneration && attempt <= highWater.AssignmentAttempt)
}

func (m *SandboxManager) recordTombstoneLocked(identity fastletapi.SandboxIdentity) {
	current, found := m.tombstones[identity.SandboxUID]
	if !found || current.InstanceGeneration < identity.InstanceGeneration ||
		(current.InstanceGeneration == identity.InstanceGeneration && current.AssignmentAttempt < identity.AssignmentAttempt) {
		m.tombstones[identity.SandboxUID] = identity
	}
}

func (m *SandboxManager) validateCreateRequest(req *fastletapi.CreateSandboxRequest) *fastletapi.FastletError {
	if req == nil || req.SpecGeneration <= 0 || req.Identity.SandboxUID == "" || req.Identity.Namespace == "" || req.Identity.Name == "" || req.Identity.InstanceGeneration <= 0 || req.Identity.RuntimeInstanceID == "" || req.Identity.AssignmentAttempt <= 0 {
		return fastletError(fastletapi.ErrorConflict, "sandboxUid, namespace, name, runtimeInstanceId, positive specGeneration, instanceGeneration, and assignmentAttempt are required", false)
	}
	if m.fastletPodUID != "" && req.Identity.FastletPodUID != m.fastletPodUID {
		return fastletError(fastletapi.ErrorStaleAssignment, "request targets a different Fastlet Pod UID", false)
	}
	return nil
}

func (m *SandboxManager) admissionStatusLocked() fastletapi.AdmissionStatus {
	status := fastletapi.AdmissionStatus{Capacity: m.capacity}
	for _, metadata := range m.sandboxes {
		switch metadata.Phase {
		case "creating", "infra-pending", "initializing-infra", "infra-unavailable", "route-pending", "publishing-route", "route-unavailable", "action-pending", "action-unavailable":
			status.Creating++
		case "terminating", "deleting", "delete-failed", "create-cleanup", "create-cleanup-failed":
			status.Deleting++
		default:
			status.Running++
		}
	}
	status.Used = status.Creating + status.Running + status.Deleting
	recordAdmissionStatus(status)
	return status
}

// sandboxStatusLocked returns the point-in-time Fastlet observation. Callers
// hold m.mu so the global proxy connection state and Sandbox phase form one
// coherent data-plane result.
func (m *SandboxManager) sandboxStatusLocked(metadata *SandboxMetadata) fastletapi.SandboxStatus {
	dataPlaneReady := (m.routePublisher == nil || m.routeReady) && routeReadyForPhase(metadata.Phase)
	runtime, dataPlane := observationsForPhase(metadata.Phase, dataPlaneReady)
	return fastletapi.SandboxStatus{
		SandboxID:          metadata.SandboxID,
		InstanceGeneration: metadata.InstanceGeneration, RuntimeInstanceID: metadata.RuntimeInstanceID, AssignmentAttempt: metadata.AssignmentAttempt,
		RouteGeneration: metadata.RouteGeneration, AcceptedGeneration: metadata.AcceptedGeneration, AppliedGeneration: metadata.AppliedGeneration,
		Runtime: runtime, DataPlane: dataPlane, CreatedAt: metadata.CreatedAt,
		InfraComponents: apiInfraDiagnostics(metadata.InfraDiagnostics, metadata.InfraServices, metadata.RouteGeneration, dataPlaneReady),
		ActionBindings:  append([]fastletapi.ActionBindingStatus(nil), metadata.ActionBindingStatuses...),
	}
}

func observationsForPhase(phase string, routeReady bool) (fastletapi.RuntimeObservation, fastletapi.DataPlaneObservation) {
	runtime := fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateUnknown}
	dataPlane := fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateUnknown}
	switch phase {
	case "creating":
		runtime.State, dataPlane.State = fastletapi.RuntimeStateCreating, fastletapi.DataPlaneStatePending
	case "infra-pending", "initializing-infra", "route-pending", "publishing-route":
		runtime.State, dataPlane.State = fastletapi.RuntimeStateReady, fastletapi.DataPlaneStatePublishing
	case "infra-unavailable", "route-unavailable":
		runtime.State, dataPlane.State = fastletapi.RuntimeStateReady, fastletapi.DataPlaneStateUnavailable
	case "action-pending", "action-unavailable", "running":
		runtime.State, dataPlane.State = fastletapi.RuntimeStateReady, fastletapi.DataPlaneStateReady
	case "terminating", "deleting", "create-cleanup":
		runtime.State, dataPlane.State = fastletapi.RuntimeStateStopping, fastletapi.DataPlaneStateDraining
	case "delete-failed", "create-cleanup-failed":
		runtime.State, dataPlane.State = fastletapi.RuntimeStateFailed, fastletapi.DataPlaneStateFailed
	default:
		// An unrecognized internal phase is a Fastlet state-machine defect, not
		// a normal observation gap. Surface it explicitly instead of silently
		// projecting Unknown and retrying forever without a useful diagnosis.
		runtime.State, dataPlane.State = fastletapi.RuntimeStateFailed, fastletapi.DataPlaneStateFailed
		runtime.Message = "unrecognized internal Fastlet phase: " + phase
		dataPlane.Message = runtime.Message
	}
	if dataPlane.State == fastletapi.DataPlaneStateReady && !routeReady {
		dataPlane.State = fastletapi.DataPlaneStateUnavailable
		dataPlane.Message = "Fastlet proxy route is unavailable"
	}
	return runtime, dataPlane
}

func routeReadyForPhase(phase string) bool {
	return phase == "running" || phase == "action-pending" || phase == "action-unavailable"
}

func apiInfraDiagnostics(
	diagnostics []fastletinfra.ComponentDiagnostic,
	services []fastletinfra.ServiceEndpoint,
	routeGeneration int64,
	routeReady bool,
) []fastletapi.InfraComponentDiagnostic {
	serviceByComponent := make(map[string]fastletinfra.ServiceEndpoint, len(services))
	for _, service := range services {
		serviceByComponent[service.Component] = service
	}
	result := make([]fastletapi.InfraComponentDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		service := serviceByComponent[diagnostic.Component]
		observedRouteGeneration := int64(0)
		if routeReady && diagnostic.State == "Ready" {
			observedRouteGeneration = routeGeneration
		}
		result = append(result, fastletapi.InfraComponentDiagnostic{
			Component: diagnostic.Component, Protocol: service.Protocol, Port: service.Port,
			State: diagnostic.State, ObservedRouteGeneration: observedRouteGeneration, Message: diagnostic.Message,
		})
	}
	return result
}

func fastletError(code fastletapi.FastletErrorCode, message string, retryable bool) *fastletapi.FastletError {
	return &fastletapi.FastletError{Code: code, Message: message, Retryable: retryable}
}

func fastletErrorWithCause(code fastletapi.FastletErrorCode, message string, retryable bool, cause error) *fastletapi.FastletError {
	return &fastletapi.FastletError{Code: code, Message: message, Retryable: retryable, Cause: cause}
}

func defaultCreateDisposition(code fastletapi.FastletErrorCode) fastletapi.CreateDisposition {
	switch code {
	case fastletapi.ErrorCapacityRejected, fastletapi.ErrorDraining, fastletapi.ErrorProfileMismatch:
		return fastletapi.CreateDispositionRejectedBeforeSideEffects
	case fastletapi.ErrorInProgress:
		return fastletapi.CreateDispositionInProgress
	case fastletapi.ErrorGenerationFenced, fastletapi.ErrorStaleGeneration:
		return fastletapi.CreateDispositionGenerationFenced
	default:
		return fastletapi.CreateDispositionUnknown
	}
}

func createFailure(failure *fastletapi.FastletError, admission fastletapi.AdmissionStatus) (*fastletapi.CreateSandboxResponse, error) {
	return createFailureWithDisposition(failure, admission, defaultCreateDisposition(failure.Code))
}

func createFailureWithDisposition(failure *fastletapi.FastletError, admission fastletapi.AdmissionStatus, disposition fastletapi.CreateDisposition) (*fastletapi.CreateSandboxResponse, error) {
	if disposition == "" {
		disposition = fastletapi.CreateDispositionUnknown
	}
	return &fastletapi.CreateSandboxResponse{Disposition: disposition, Admission: admission, Error: failure}, failure
}
