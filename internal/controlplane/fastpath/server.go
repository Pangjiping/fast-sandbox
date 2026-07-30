package fastpath

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	routeauth "fast-sandbox/internal/dataplane/auth"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/pkg/util/idgen"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	fastpathv2.UnimplementedFastPathServiceServer
	K8sClient         client.Client
	RouteCache        client.Client
	Orchestrator      *orchestration.Orchestrator
	DiagnosticsClient interface {
		SandboxDiagnostics(context.Context, string, *fastletapi.SandboxDiagnosticsRequest) (*fastletapi.SandboxDiagnosticsResponse, error)
		WaitSandboxReady(context.Context, string, *fastletapi.WaitSandboxReadyRequest) (*fastletapi.WaitSandboxReadyResponse, error)
	}
	CredentialIssuer    *routeauth.Issuer
	SandboxProxyBaseURL string
	DefaultNamespace    string
}

var _ fastpathv2.FastPathServiceServer = &Server{}

func (s *Server) ResolveEndpoint(ctx context.Context, request *fastpathv2.ResolveEndpointRequest) (*fastpathv2.ResolveEndpointResponse, error) {
	if request == nil || request.Sandbox == nil || request.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference and endpoint target are required")
	}
	sandbox, err := s.sandboxFromReference(ctx, request.Sandbox)
	if err != nil {
		return nil, err
	}
	port, componentName, protocol, err := s.resolveEndpointTarget(ctx, sandbox, request.Target)
	if err != nil {
		return nil, err
	}
	if componentName != "" {
		wait := time.Duration(0)
		if request.WaitUntilReady {
			wait = time.Duration(request.WaitTimeoutMillis) * time.Millisecond
			if wait <= 0 {
				wait = 10 * time.Second
			}
		}
		if _, err := s.waitFastletReady(ctx, sandbox, componentName, false, wait); err != nil {
			return nil, err
		}
	}
	credential, claims, err := s.issueRouteCredential(ctx, sandbox, port, componentName, protocol)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(s.SandboxProxyBaseURL, "/")
	if request.AccessMode == fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY {
		envelope, assignmentErr := assignment.EffectiveAssignment(sandbox)
		if assignmentErr != nil || envelope == nil || s.Orchestrator == nil || s.Orchestrator.Registry == nil {
			return nil, status.Error(codes.Unavailable, "assigned Fastlet is unavailable")
		}
		fastlet, found := s.Orchestrator.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
		if !found || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" {
			return nil, status.Error(codes.Unavailable, "assigned Fastlet is unavailable")
		}
		baseURL = "http://" + fastlet.PodIP + ":5780"
	}
	if baseURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "Sandbox Proxy base URL is not configured")
	}
	path := dataplane.RoutePath(string(sandbox.UID), port)
	if componentName != "" {
		path = dataplane.ComponentRoutePath(string(sandbox.UID), componentName)
	}
	return &fastpathv2.ResolveEndpointResponse{
		SandboxUid: string(sandbox.UID),
		Target:     request.Target, ComponentName: componentName, Protocol: protocol, ResolvedPort: port,
		ProxyEndpoint:   baseURL + path,
		RequiredHeaders: map[string]string{dataplane.HeaderRouteCredential: credential},
		RouteGeneration: claims.RouteGeneration, ExpiresAtUnixSeconds: claims.ExpiresAt,
	}, nil
}

func (s *Server) WaitSandboxReady(ctx context.Context, request *fastpathv2.WaitSandboxReadyRequest) (*fastpathv2.SandboxInfo, error) {
	if request == nil || request.Sandbox == nil || request.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference and readiness target are required")
	}
	sandbox, err := s.sandboxFromReference(ctx, request.Sandbox)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(request.WaitTimeoutMillis) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if timeout > 5*time.Minute {
		return nil, status.Error(codes.InvalidArgument, "wait_timeout_millis cannot exceed 300000")
	}
	switch target := request.Target.(type) {
	case *fastpathv2.WaitSandboxReadyRequest_ComponentName:
		if target.ComponentName == "" {
			return nil, status.Error(codes.InvalidArgument, "component_name is required")
		}
		return s.waitFastletReady(ctx, sandbox, target.ComponentName, false, timeout)
	case *fastpathv2.WaitSandboxReadyRequest_DataPlane:
		if !target.DataPlane {
			return nil, status.Error(codes.InvalidArgument, "data_plane must be true")
		}
		return s.waitFastletReady(ctx, sandbox, "", true, timeout)
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown readiness target")
	}
}

func (s *Server) sandboxFromReference(ctx context.Context, reference *fastpathv2.SandboxReference) (*apiv1alpha2.Sandbox, error) {
	if reference == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox reference is required")
	}
	switch value := reference.Reference.(type) {
	case *fastpathv2.SandboxReference_SandboxUid:
		if value.SandboxUid == "" {
			return nil, status.Error(codes.InvalidArgument, "sandbox_uid is required")
		}
		return s.findSandboxByUID(ctx, value.SandboxUid)
	case *fastpathv2.SandboxReference_NamespacedName:
		if value.NamespacedName == nil || value.NamespacedName.Name == "" {
			return nil, status.Error(codes.InvalidArgument, "sandbox namespaced_name.name is required")
		}
		namespace := value.NamespacedName.Namespace
		if namespace == "" {
			namespace = s.defaultNamespace()
		}
		var sandbox apiv1alpha2.Sandbox
		if err := s.K8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: value.NamespacedName.Name}, &sandbox); err != nil {
			return nil, grpcKubernetesError(err)
		}
		return &sandbox, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown sandbox reference")
	}
}

func (s *Server) resolveEndpointTarget(
	ctx context.Context,
	sandbox *apiv1alpha2.Sandbox,
	target *fastpathv2.EndpointTarget,
) (uint32, string, string, error) {
	var pool apiv1alpha2.SandboxPool
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Spec.PoolRef}, &pool); err != nil {
		return 0, "", "", grpcKubernetesError(err)
	}
	switch value := target.Target.(type) {
	case *fastpathv2.EndpointTarget_ComponentName:
		for _, component := range pool.Spec.InfraComponents {
			if component.Name == value.ComponentName {
				return uint32(component.Endpoint.Port), component.Name, component.Endpoint.Protocol, nil
			}
		}
		return 0, "", "", status.Errorf(codes.NotFound, "Infra Component %q is not declared by Pool %s", value.ComponentName, pool.Name)
	case *fastpathv2.EndpointTarget_Port:
		if value.Port == 0 || value.Port > 65535 {
			return 0, "", "", status.Error(codes.InvalidArgument, "port must be between 1 and 65535")
		}
		for _, component := range pool.Spec.InfraComponents {
			if uint32(component.Endpoint.Port) == value.Port {
				return 0, "", "", status.Errorf(codes.FailedPrecondition, "port %d is reserved by Infra Component %s; resolve by component name", value.Port, component.Name)
			}
		}
		return value.Port, "", "HTTP", nil
	default:
		return 0, "", "", status.Error(codes.InvalidArgument, "unknown endpoint target")
	}
}

func (s *Server) waitFastletReady(
	ctx context.Context,
	sandbox *apiv1alpha2.Sandbox,
	componentName string,
	fullDataPlane bool,
	timeout time.Duration,
) (*fastpathv2.SandboxInfo, error) {
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil || envelope == nil {
		return nil, status.Error(codes.Unavailable, "Sandbox assignment is not ready")
	}
	if s.Orchestrator == nil || s.Orchestrator.Registry == nil || s.DiagnosticsClient == nil {
		return nil, status.Error(codes.FailedPrecondition, "Fastlet readiness client is not configured")
	}
	fastlet, found := s.Orchestrator.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !found || fastlet.PodUID != envelope.FastletPodUID || fastlet.PodIP == "" {
		return nil, status.Error(codes.Unavailable, "assigned Fastlet is unavailable")
	}
	var pool apiv1alpha2.SandboxPool
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Spec.PoolRef}, &pool); err != nil {
		return nil, grpcKubernetesError(err)
	}
	desired := make(map[string]apiv1alpha2.InfraComponent, len(pool.Spec.InfraComponents))
	for _, component := range pool.Spec.InfraComponents {
		desired[component.Name] = component
	}
	if componentName != "" {
		if _, exists := desired[componentName]; !exists {
			return nil, status.Errorf(codes.NotFound, "Infra Component %q is not declared", componentName)
		}
	}
	waitCtx := ctx
	cancel := func() {}
	noWait := timeout <= 0
	if !noWait {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	response, callErr := s.DiagnosticsClient.WaitSandboxReady(waitCtx, fastlet.PodIP, &fastletapi.WaitSandboxReadyRequest{
		Identity: fastletapi.SandboxIdentity{
			RequestID: sandbox.Annotations[assignment.AnnotationRequestID], SandboxUID: string(sandbox.UID),
			FastletPodUID: envelope.FastletPodUID, InstanceGeneration: envelope.InstanceGeneration,
			RuntimeInstanceID: envelope.RuntimeInstanceID, AssignmentAttempt: envelope.Attempt,
			RouteGeneration: envelope.RouteGeneration,
		},
		ComponentName: componentName,
		DataPlane:     fullDataPlane,
		NoWait:        noWait,
	})
	if callErr != nil {
		if errors.Is(callErr, context.DeadlineExceeded) || errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return nil, status.Error(codes.DeadlineExceeded, "timed out waiting for Fastlet readiness")
		}
		if errors.Is(callErr, context.Canceled) {
			return nil, status.FromContextError(callErr).Err()
		}
		return nil, status.Errorf(codes.Unavailable, "wait for Fastlet readiness: %v", callErr)
	}
	if response == nil || !response.Ready || response.Sandbox == nil {
		return nil, status.Error(codes.Unavailable, "requested Sandbox endpoint is not ready")
	}
	info := sandboxInfoWithEnvelope(sandbox, envelope, true)
	observed := make(map[string]fastletapi.InfraComponentDiagnostic, len(response.Sandbox.InfraDiagnostics))
	for _, diagnostic := range response.Sandbox.InfraDiagnostics {
		observed[diagnostic.Component] = diagnostic
	}
	info.Components = nil
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		component := desired[name]
		diagnostic := observed[name]
		state := diagnostic.State
		if state == "" {
			state = string(apiv1alpha2.InfraComponentStarting)
		}
		info.Components = append(info.Components, &fastpathv2.ComponentInfo{
			Name: name, State: state, Protocol: component.Endpoint.Protocol,
			Port: uint32(component.Endpoint.Port), ObservedRouteGeneration: diagnostic.ObservedRouteGeneration,
			Message: diagnostic.Message,
		})
	}
	info.DataPlaneState = string(apiv1alpha2.ObservedStateReady)
	return info, nil
}

func grpcKubernetesError(err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return status.Error(codes.NotFound, err.Error())
	case apierrors.IsAlreadyExists(err):
		return status.Error(codes.AlreadyExists, err.Error())
	case apierrors.IsConflict(err):
		return status.Error(codes.Aborted, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (s *Server) issueRouteCredential(
	ctx context.Context,
	sandbox *apiv1alpha2.Sandbox,
	targetPort uint32,
	componentName string,
	protocol string,
) (string, routeauth.Claims, error) {
	if s.CredentialIssuer == nil {
		return "", routeauth.Claims{}, status.Error(codes.FailedPrecondition, "route credential issuer is not configured")
	}
	if sandbox == nil || sandbox.UID == "" {
		return "", routeauth.Claims{}, status.Error(codes.InvalidArgument, "Sandbox is required")
	}
	envelope, err := assignment.EffectiveAssignment(sandbox)
	if err != nil || envelope == nil {
		return "", routeauth.Claims{}, status.Error(codes.Unavailable, "Sandbox assignment is not ready")
	}
	ctx = observability.WithIdentity(ctx, identityFromSandbox(sandbox, targetPort))
	routeGeneration := envelope.RouteGeneration
	if routeGeneration <= 0 {
		routeGeneration = 1
	}
	claims := routeauth.Claims{
		Namespace: sandbox.Namespace, SandboxUID: string(sandbox.UID), TargetPort: targetPort,
		TargetKind: routeauth.TargetKindPort, Protocol: protocol,
		FastletPodUID: envelope.FastletPodUID, AssignmentAttempt: envelope.Attempt,
		RouteGeneration: routeGeneration,
	}
	if componentName != "" {
		claims.TargetKind = routeauth.TargetKindComponent
		claims.ComponentName = componentName
	}
	credential, claims, err := s.CredentialIssuer.Issue(claims)
	if err != nil {
		return "", routeauth.Claims{}, status.Errorf(codes.Internal, "issue route credential: %v", err)
	}
	return credential, claims, nil
}

func (s *Server) findSandboxByUID(ctx context.Context, sandboxUID string) (*apiv1alpha2.Sandbox, error) {
	if s.RouteCache != nil {
		var cached apiv1alpha2.SandboxList
		if err := s.RouteCache.List(ctx, &cached, client.MatchingFields{SandboxUIDIndexField: sandboxUID}); err != nil {
			return nil, status.Errorf(codes.Internal, "read Sandbox UID index: %v", err)
		}
		if len(cached.Items) == 1 {
			return cached.Items[0].DeepCopy(), nil
		}
		if len(cached.Items) > 1 {
			return nil, status.Error(codes.Internal, "Sandbox UID index returned duplicate objects")
		}
	}
	var list apiv1alpha2.SandboxList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, status.Errorf(codes.Internal, "list Sandboxes: %v", err)
	}
	for index := range list.Items {
		if string(list.Items[index].UID) == sandboxUID {
			return list.Items[index].DeepCopy(), nil
		}
	}
	return nil, status.Error(codes.NotFound, "Sandbox UID not found")
}

const SandboxUIDIndexField = "metadata.uid"

func (s *Server) CreateSandbox(ctx context.Context, request *fastpathv2.CreateRequest) (_ *fastpathv2.SandboxInfo, resultErr error) {
	started := time.Now()
	acceptedObserved := false
	defer func() {
		success := "true"
		if resultErr != nil {
			success = "false"
		}
		createSandboxDuration.WithLabelValues("v2", success).Observe(time.Since(started).Seconds())
		createRuntimeReadyLatency.WithLabelValues(grpcMetricResult(resultErr)).Observe(time.Since(started).Seconds())
		if !acceptedObserved {
			observeCreateAccepted("rejected", started, resultErr)
		}
	}()

	if request == nil || request.Image == "" || request.PoolRef == "" {
		return nil, status.Error(codes.InvalidArgument, "image and pool_ref are required")
	}
	if request.Namespace == "" {
		request.Namespace = s.defaultNamespace()
	}
	if request.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required")
	}
	if err := ValidateRequestID(request.RequestId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateCreateOptions(request, time.Now()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{
		RequestID: request.RequestId, Namespace: request.Namespace, SandboxName: request.RequestId,
	})
	createSpecHash, err := CreateSpecHash(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hash create request: %v", err)
	}
	orchestrator, err := s.orchestrator()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	sandbox := sandboxFromCreateRequest(request, createSpecHash)
	ctx = observability.WithIdentity(ctx, observability.Identity{SandboxName: sandbox.Name})
	_, finishCandidates := startCreateStage(ctx, "candidate_selection")
	candidates, err := orchestrator.FastPathCandidates(sandbox, request.RequestId)
	finishCandidates(err)
	if err != nil {
		if errors.Is(err, orchestration.ErrNoCandidate) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	runtimeInstanceID, err := idgen.GenerateRequestID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate runtime instance ID: %v", err)
	}
	envelope, err := orchestration.AssignmentForCandidate(candidates[0], 1, apiv1alpha2.InitialInstanceGeneration, 1, runtimeInstanceID)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid Fastlet candidate: %v", err)
	}
	if err := assignment.SetAssignmentAnnotation(sandbox, envelope); err != nil {
		return nil, status.Errorf(codes.Internal, "encode assignment: %v", err)
	}

	// IO 1: CRD Create. The happy path does not preflight with a Get/List.
	crdContext, finishCRDCreate := startCreateStage(ctx, "crd_create")
	createErr := s.K8sClient.Create(crdContext, sandbox)
	finishCRDCreate(createErr)
	if createErr != nil {
		var existing apiv1alpha2.Sandbox
		getContext, finishIdempotencyRead := startCreateStage(ctx, "idempotency_read")
		getErr := s.K8sClient.Get(getContext, client.ObjectKeyFromObject(sandbox), &existing)
		finishIdempotencyRead(getErr)
		if getErr != nil {
			if apierrors.IsNotFound(getErr) && !apierrors.IsAlreadyExists(createErr) {
				return nil, createErr
			}
			return nil, errors.Join(createErr, getErr)
		}
		if existing.Annotations[assignment.AnnotationRequestID] != request.RequestId || existing.Annotations[assignment.AnnotationCreateSpecHash] != createSpecHash {
			return nil, status.Errorf(codes.AlreadyExists, "Sandbox name %q belongs to another create intent", sandbox.Name)
		}
		sandbox = existing.DeepCopy()
		existingEnvelope, envelopeErr := assignment.AssignmentFromAnnotation(sandbox)
		if envelopeErr != nil || existingEnvelope == nil {
			return nil, status.Errorf(codes.Unavailable, "existing Sandbox assignment is not ready: %v", envelopeErr)
		}
		envelope = *existingEnvelope
		selected, ok := orchestrator.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
		if !ok {
			return nil, status.Error(codes.Unavailable, orchestration.ErrAssignedFastletUnavailable.Error())
		}
		candidates = []placement.FastletInfo{selected}
		observeCreateAccepted("idempotent", started, nil)
	} else {
		observeCreateAccepted("crd", started, nil)
	}
	acceptedObserved = true
	ctx = observability.WithIdentity(ctx, observability.Identity{
		RequestID: request.RequestId, Namespace: sandbox.Namespace, SandboxName: sandbox.Name, SandboxUID: string(sandbox.UID),
		FastletPodUID: envelope.FastletPodUID, InstanceGeneration: envelope.InstanceGeneration,
		AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration,
	})

	for index, candidate := range candidates {
		if index > 0 {
			orchestration.RecordTopKRetry("attempt")
			runtimeInstanceID, err = idgen.GenerateRequestID()
			if err != nil {
				return nil, status.Errorf(codes.Internal, "generate runtime instance ID: %v", err)
			}
			next, nextErr := orchestration.AssignmentForCandidate(candidate, envelope.Attempt+1, envelope.InstanceGeneration, envelope.RouteGeneration+1, runtimeInstanceID)
			if nextErr != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "invalid Fastlet candidate: %v", nextErr)
			}
			casContext, finishAssignmentCAS := startCreateStage(ctx, "assignment_cas")
			sandbox, err = assignment.CASAssignmentAnnotation(casContext, s.K8sClient, client.ObjectKeyFromObject(sandbox), envelope, next)
			finishAssignmentCAS(err)
			if err != nil {
				return nil, status.Errorf(codes.Aborted, "assignment changed concurrently: %v", err)
			}
			envelope = next
		}

		// IO 2 on the happy path: one atomic Fastlet admission/create call.
		fastletContext, finishFastletCreate := startCreateStage(ctx, "fastlet_create")
		_, createErr := orchestrator.CreateRuntimeOnCandidate(fastletContext, sandbox, candidate, envelope)
		finishFastletCreate(createErr)
		if createErr == nil {
			if index > 0 {
				orchestration.RecordTopKRetry("accepted")
			}
			return sandboxInfoWithEnvelope(sandbox, &envelope, true), nil
		}
		if orchestration.IsCandidateRejection(createErr) && index+1 < len(candidates) {
			orchestration.RecordTopKRetry("candidate_rejected")
			orchestrator.RecordCandidateFeedback(candidate.ID, createErr)
			continue
		}
		if orchestration.IsCandidateRejection(createErr) {
			return nil, status.Errorf(codes.ResourceExhausted, "all Fastlet candidates rejected admission: %v", createErr)
		}
		return nil, status.Errorf(codes.Unavailable, "Sandbox intent is persisted and Controller will retry the same runtime identity: %v", createErr)
	}
	return nil, status.Error(codes.ResourceExhausted, orchestration.ErrNoCandidate.Error())
}

func sandboxFromCreateRequest(request *fastpathv2.CreateRequest, createSpecHash string) *apiv1alpha2.Sandbox {
	environment := make([]corev1.EnvVar, 0, len(request.Envs))
	for name, value := range request.Envs {
		environment = append(environment, corev1.EnvVar{Name: name, Value: value})
	}
	labels := map[string]string{assignment.LabelCreatedBy: "fastpath"}
	for name, value := range request.Metadata {
		labels[metadataLabelKey(name)] = value
	}
	var expiresAt *metav1.Time
	if request.ExpiresAtUnixSeconds > 0 {
		value := metav1.NewTime(time.Unix(request.ExpiresAtUnixSeconds, 0).UTC())
		expiresAt = &value
	}
	recoveryTimeout := request.RecoveryTimeoutSeconds
	if recoveryTimeout == 0 {
		recoveryTimeout = 60
	}
	return &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: request.RequestId, Namespace: request.Namespace,
			Annotations: map[string]string{
				assignment.AnnotationRequestID: request.RequestId, assignment.AnnotationCreateSpecHash: createSpecHash,
			},
			Labels: labels,
		},
		Spec: apiv1alpha2.SandboxSpec{
			Image: request.Image, PoolRef: request.PoolRef,
			Command: request.Command, Args: request.Args, Envs: environment, WorkingDir: request.WorkingDir,
			ExpireTime: expiresAt, FailurePolicy: toFailurePolicy(request.FailurePolicy),
			RecoveryTimeoutSeconds: recoveryTimeout,
		},
	}
}

const metadataLabelPrefix = "metadata.sandbox.fast.io/"

func metadataLabelKey(name string) string { return metadataLabelPrefix + name }

func validateCreateOptions(request *fastpathv2.CreateRequest, now time.Time) error {
	if request.ExpiresAtUnixSeconds < 0 {
		return errors.New("expires_at_unix_seconds cannot be negative")
	}
	if request.ExpiresAtUnixSeconds > 0 && !time.Unix(request.ExpiresAtUnixSeconds, 0).After(now) {
		return errors.New("expires_at_unix_seconds must be in the future")
	}
	if request.RecoveryTimeoutSeconds < 0 || request.RecoveryTimeoutSeconds > 86400 {
		return errors.New("recovery_timeout_seconds must be between 0 and 86400")
	}
	if request.FailurePolicy != fastpathv2.FailurePolicy_MANUAL &&
		request.FailurePolicy != fastpathv2.FailurePolicy_AUTO_RECREATE {
		return errors.New("failure_policy is invalid")
	}
	return validateMetadata(request.Metadata)
}

func validateMetadata(metadata map[string]string) error {
	for name, value := range metadata {
		if problems := kvalidation.IsDNS1123Label(name); len(problems) > 0 {
			return fmt.Errorf("metadata key %q must be a DNS label: %s", name, problems[0])
		}
		if problems := kvalidation.IsValidLabelValue(value); len(problems) > 0 {
			return fmt.Errorf("metadata value for %q is not a Kubernetes label value: %s", name, problems[0])
		}
	}
	return nil
}

func userMetadata(labels map[string]string) map[string]string {
	result := make(map[string]string)
	for name, value := range labels {
		if strings.HasPrefix(name, metadataLabelPrefix) {
			result[strings.TrimPrefix(name, metadataLabelPrefix)] = value
		}
	}
	return result
}

func (s *Server) orchestrator() (*orchestration.Orchestrator, error) {
	if s.Orchestrator == nil {
		return nil, errors.New("Sandbox orchestrator is not configured")
	}
	return s.Orchestrator, nil
}

func (s *Server) ListSandboxes(ctx context.Context, request *fastpathv2.ListRequest) (*fastpathv2.ListResponse, error) {
	if request == nil {
		request = &fastpathv2.ListRequest{}
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	if request.PageSize < 0 || request.PageSize > 500 {
		return nil, status.Error(codes.InvalidArgument, "page_size must be between 0 and 500")
	}
	if err := validateMetadata(request.Metadata); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace})
	var list apiv1alpha2.SandboxList
	options := []client.ListOption{client.InNamespace(namespace)}
	if len(request.Metadata) > 0 {
		selector := make(map[string]string, len(request.Metadata))
		for name, value := range request.Metadata {
			selector[metadataLabelKey(name)] = value
		}
		options = append(options, client.MatchingLabels(selector))
	}
	if request.PageSize > 0 {
		options = append(options, client.Limit(request.PageSize))
	}
	if request.PageToken != "" {
		options = append(options, client.Continue(request.PageToken))
	}
	if err := s.K8sClient.List(ctx, &list, options...); err != nil {
		return nil, err
	}
	response := &fastpathv2.ListResponse{
		Items: make([]*fastpathv2.SandboxInfo, 0, len(list.Items)), NextPageToken: list.Continue,
	}
	for index := range list.Items {
		response.Items = append(response.Items, sandboxInfo(&list.Items[index]))
	}
	return response, nil
}

func (s *Server) GetSandbox(ctx context.Context, request *fastpathv2.GetRequest) (*fastpathv2.SandboxInfo, error) {
	if request == nil || request.SandboxName == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_name is required")
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	var sandbox apiv1alpha2.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: request.SandboxName, Namespace: namespace}, &sandbox); err != nil {
		return nil, err
	}
	return sandboxInfo(&sandbox), nil
}

func (s *Server) GetPool(ctx context.Context, request *fastpathv2.GetPoolRequest) (*fastpathv2.PoolInfo, error) {
	if request == nil || request.PoolName == "" {
		return nil, status.Error(codes.InvalidArgument, "pool_name is required")
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	var pool apiv1alpha2.SandboxPool
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: request.PoolName}, &pool); err != nil {
		return nil, grpcKubernetesError(err)
	}
	return poolInfo(&pool), nil
}

func (s *Server) ListPools(ctx context.Context, request *fastpathv2.ListPoolsRequest) (*fastpathv2.ListPoolsResponse, error) {
	namespace := ""
	if request != nil {
		namespace = request.Namespace
	}
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	var pools apiv1alpha2.SandboxPoolList
	if err := s.K8sClient.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return nil, grpcKubernetesError(err)
	}
	sort.Slice(pools.Items, func(i, j int) bool { return pools.Items[i].Name < pools.Items[j].Name })
	response := &fastpathv2.ListPoolsResponse{Items: make([]*fastpathv2.PoolInfo, 0, len(pools.Items))}
	for index := range pools.Items {
		response.Items = append(response.Items, poolInfo(&pools.Items[index]))
	}
	return response, nil
}

func poolInfo(pool *apiv1alpha2.SandboxPool) *fastpathv2.PoolInfo {
	info := &fastpathv2.PoolInfo{
		Namespace: pool.Namespace, Name: pool.Name, Runtime: string(pool.Spec.Runtime),
		SandboxCpu:         pool.Spec.SandboxResources.CPU.String(),
		SandboxMemory:      pool.Spec.SandboxResources.Memory.String(),
		SandboxPids:        pool.Spec.SandboxResources.PIDs,
		MaxSandboxesPerPod: pool.Spec.MaxSandboxesPerPod,
		TotalFastlets:      pool.Status.TotalFastlets, ReadyFastlets: pool.Status.ReadyPods,
		IdleFastlets: pool.Status.IdleFastlets, InfraRevision: pool.Status.InfraRevision,
		PreparedFastlets: pool.Status.PreparedFastlets,
		Registry: &fastpathv2.RegistryInfo{
			TargetGeneration: pool.Status.Registry.TargetGeneration,
			AppliedFastlets:  pool.Status.Registry.AppliedFastlets,
			TotalFastlets:    pool.Status.Registry.TotalFastlets,
			LastError:        pool.Status.Registry.LastError,
		},
	}
	for _, component := range pool.Status.InfraComponents {
		info.Components = append(info.Components, &fastpathv2.ComponentCapability{
			Name: component.Name, Protocol: component.Protocol, Port: uint32(component.Port),
			HealthKind: component.HealthKind,
		})
	}
	for _, image := range pool.Status.WarmImages {
		info.WarmImages = append(info.WarmImages, &fastpathv2.WarmImageInfo{
			Image: image.Image, DesiredFastlets: image.DesiredFastlets,
			CachedFastlets: image.CachedFastlets, PullingFastlets: image.PullingFastlets,
			FailedFastlets: image.FailedFastlets, ObservedGeneration: image.ObservedGeneration,
			LastError: image.LastError,
		})
	}
	return info
}

func (s *Server) GetSandboxDiagnostics(ctx context.Context, request *fastpathv2.SandboxDiagnosticsRequest) (*fastpathv2.SandboxDiagnosticsResponse, error) {
	if request == nil || request.SandboxName == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_name is required")
	}
	if request.Limit < 0 || request.Limit > 128 {
		return nil, status.Error(codes.InvalidArgument, "limit must be between 0 and 128")
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	var sandbox apiv1alpha2.Sandbox
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: request.SandboxName, Namespace: namespace}, &sandbox); err != nil {
		return nil, err
	}
	response := &fastpathv2.SandboxDiagnosticsResponse{Sandbox: sandboxInfo(&sandbox)}
	envelope, annotationErr := assignment.AssignmentFromAnnotation(&sandbox)
	if annotationErr != nil {
		response.AssignmentState = "invalid-annotation"
		response.FastletError = annotationErr.Error()
		return response, nil
	}
	if envelope == nil {
		response.AssignmentState = "unassigned"
		response.FastletError = "Sandbox has no durable assignment annotation"
		return response, nil
	}
	response.AssignmentState = "annotation-authoritative"
	if _, projectionErr := assignment.EffectiveAssignment(&sandbox); projectionErr != nil {
		response.AssignmentState = "status-projection-conflict"
		response.FastletError = projectionErr.Error()
	} else if sandbox.Status.Assignment == nil {
		response.AssignmentState = "status-projection-pending"
	} else {
		response.AssignmentState = "synchronized"
	}
	response.RuntimeInstanceId = envelope.RuntimeInstanceID
	response.AssignmentAttempt = envelope.Attempt

	if s.Orchestrator == nil || s.Orchestrator.Registry == nil || s.DiagnosticsClient == nil {
		response.FastletError = appendDiagnosticError(response.FastletError, "Fastlet diagnostics client is not configured")
		return response, nil
	}
	fastlet, found := s.Orchestrator.Registry.GetFastletByID(placement.FastletID(envelope.FastletName))
	if !found {
		response.FastletError = appendDiagnosticError(response.FastletError, "assigned Fastlet is absent from the local registry")
		return response, nil
	}
	if fastlet.PodUID != envelope.FastletPodUID {
		response.FastletError = appendDiagnosticError(response.FastletError, "assigned Fastlet Pod UID was replaced")
		return response, nil
	}
	identity := fastletapi.SandboxIdentity{
		RequestID: sandbox.Annotations[assignment.AnnotationRequestID], SandboxUID: string(sandbox.UID),
		FastletPodUID: envelope.FastletPodUID, InstanceGeneration: envelope.InstanceGeneration,
		RuntimeInstanceID: envelope.RuntimeInstanceID, AssignmentAttempt: envelope.Attempt, RouteGeneration: envelope.RouteGeneration,
	}
	fastletResponse, err := s.DiagnosticsClient.SandboxDiagnostics(ctx, fastlet.PodIP, &fastletapi.SandboxDiagnosticsRequest{
		Identity: identity, Limit: int(request.Limit),
	})
	if err != nil {
		response.FastletError = appendDiagnosticError(response.FastletError, err.Error())
		return response, nil
	}
	response.FastletReachable = true
	for _, event := range fastletResponse.Events {
		response.Events = append(response.Events, &fastpathv2.SandboxDiagnosticEvent{
			TimestampUnixNano: event.Timestamp.UnixNano(), Level: event.Level,
			Source: event.Source, Phase: event.Phase, Message: event.Message,
		})
	}
	return response, nil
}

func appendDiagnosticError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func sandboxInfo(sandbox *apiv1alpha2.Sandbox) *fastpathv2.SandboxInfo {
	fastletName := ""
	if sandbox.Status.Assignment != nil {
		fastletName = sandbox.Status.Assignment.FastletName
	}
	info := &fastpathv2.SandboxInfo{
		SandboxUid: string(sandbox.UID), SandboxName: sandbox.Name,
		Namespace:    sandbox.Namespace,
		RuntimeState: string(sandbox.Status.RuntimeState), DataPlaneState: string(sandbox.Status.DataPlaneState),
		UserProcessState: string(sandbox.Status.UserProcessState), FastletPod: fastletName,
		Image: sandbox.Spec.Image, PoolRef: sandbox.Spec.PoolRef, Metadata: userMetadata(sandbox.Labels),
		CreatedAtUnixSeconds:   sandbox.CreationTimestamp.Unix(),
		FailurePolicy:          toProtoFailurePolicy(sandbox.Spec.FailurePolicy),
		RecoveryTimeoutSeconds: sandbox.Spec.RecoveryTimeoutSeconds,
		AssignmentAttempt:      sandbox.Status.AssignmentAttempt,
		InstanceGeneration:     sandbox.Status.InstanceGeneration,
		RouteGeneration:        sandbox.Status.RouteGeneration,
		InfraRevision:          sandbox.Status.InfraRevision,
	}
	if sandbox.Spec.ExpireTime != nil {
		info.ExpiresAtUnixSeconds = sandbox.Spec.ExpireTime.Unix()
	}
	if sandbox.Status.Assignment != nil {
		info.AssignmentAttempt = sandbox.Status.Assignment.Attempt
		info.InfraRevision = sandbox.Status.Assignment.InfraRevision
	}
	for _, component := range sandbox.Status.Components {
		transition := int64(0)
		if component.LastTransitionTime != nil {
			transition = component.LastTransitionTime.Unix()
		}
		info.Components = append(info.Components, &fastpathv2.ComponentInfo{
			Name: component.Name, State: string(component.State), Protocol: component.Protocol,
			Port: uint32(component.Port), ObservedRouteGeneration: component.ObservedRouteGeneration,
			LastTransitionUnixSeconds: transition, Message: component.Message,
		})
	}
	return info
}

func sandboxInfoWithEnvelope(
	sandbox *apiv1alpha2.Sandbox,
	envelope *assignment.AssignmentEnvelope,
	runtimeReady bool,
) *fastpathv2.SandboxInfo {
	info := sandboxInfo(sandbox)
	if envelope != nil {
		info.FastletPod = envelope.FastletName
		info.AssignmentAttempt = envelope.Attempt
		info.InstanceGeneration = envelope.InstanceGeneration
		info.RouteGeneration = envelope.RouteGeneration
		info.InfraRevision = envelope.InfraRevision
	}
	if runtimeReady {
		info.RuntimeState = string(apiv1alpha2.ObservedStateReady)
		info.UserProcessState = string(apiv1alpha2.ObservedStateReady)
		if info.DataPlaneState == "" {
			info.DataPlaneState = string(apiv1alpha2.ObservedStatePending)
		}
	}
	return info
}

func toProtoFailurePolicy(policy apiv1alpha2.FailurePolicy) fastpathv2.FailurePolicy {
	if policy == apiv1alpha2.FailurePolicyAutoRecreate {
		return fastpathv2.FailurePolicy_AUTO_RECREATE
	}
	return fastpathv2.FailurePolicy_MANUAL
}

func identityFromSandbox(sandbox *apiv1alpha2.Sandbox, targetPort uint32) observability.Identity {
	if sandbox == nil {
		return observability.Identity{TargetPort: targetPort}
	}
	identity := observability.Identity{
		RequestID: sandbox.Annotations[assignment.AnnotationRequestID], Namespace: sandbox.Namespace, SandboxName: sandbox.Name,
		SandboxUID: string(sandbox.UID), InstanceGeneration: sandbox.Status.InstanceGeneration,
		RouteGeneration: sandbox.Status.RouteGeneration, TargetPort: targetPort,
	}
	if sandbox.Status.Assignment != nil {
		identity.FastletPodUID = sandbox.Status.Assignment.FastletPodUID
		identity.AssignmentAttempt = sandbox.Status.Assignment.Attempt
	}
	return identity
}

// Delete only submits desired state. Finalizer reconciliation owns runtime cleanup.
func (s *Server) DeleteSandbox(ctx context.Context, request *fastpathv2.DeleteRequest) (*fastpathv2.DeleteResponse, error) {
	if request == nil || request.SandboxName == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_name is required")
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: request.SandboxName, Namespace: namespace}}
	if err := s.K8sClient.Delete(ctx, sandbox); err != nil && !apierrors.IsNotFound(err) {
		return &fastpathv2.DeleteResponse{Success: false}, err
	}
	return &fastpathv2.DeleteResponse{Success: true}, nil
}

// Update only commits declarative intent; the Controller observes and reconciles it.
func (s *Server) UpdateSandbox(ctx context.Context, request *fastpathv2.UpdateRequest) (*fastpathv2.UpdateResponse, error) {
	if request == nil || request.SandboxName == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_name is required")
	}
	if err := validateMetadata(request.MetadataUpsert); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	for _, name := range request.MetadataDeleteKeys {
		if problems := kvalidation.IsDNS1123Label(name); len(problems) > 0 {
			return nil, status.Errorf(codes.InvalidArgument, "metadata delete key %q is invalid: %s", name, problems[0])
		}
	}
	switch value := request.Update.(type) {
	case *fastpathv2.UpdateRequest_ExpiresAtUnixSeconds:
		if value.ExpiresAtUnixSeconds < 0 {
			return nil, status.Error(codes.InvalidArgument, "expires_at_unix_seconds cannot be negative")
		}
		if value.ExpiresAtUnixSeconds > 0 && !time.Unix(value.ExpiresAtUnixSeconds, 0).After(time.Now()) {
			return nil, status.Error(codes.InvalidArgument, "expires_at_unix_seconds must be in the future")
		}
	case *fastpathv2.UpdateRequest_ResetRevision:
		if _, err := time.Parse(time.RFC3339Nano, value.ResetRevision); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid reset_revision: %v", err)
		}
	case *fastpathv2.UpdateRequest_FailurePolicy:
		if value.FailurePolicy != fastpathv2.FailurePolicy_MANUAL &&
			value.FailurePolicy != fastpathv2.FailurePolicy_AUTO_RECREATE {
			return nil, status.Error(codes.InvalidArgument, "failure_policy is invalid")
		}
	case *fastpathv2.UpdateRequest_RecoveryTimeoutSeconds:
		if value.RecoveryTimeoutSeconds < 0 || value.RecoveryTimeoutSeconds > 86400 {
			return nil, status.Error(codes.InvalidArgument, "recovery_timeout_seconds must be between 0 and 86400")
		}
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = s.defaultNamespace()
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{Namespace: namespace, SandboxName: request.SandboxName})
	key := client.ObjectKey{Name: request.SandboxName, Namespace: namespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var sandbox apiv1alpha2.Sandbox
		if err := s.K8sClient.Get(ctx, key, &sandbox); err != nil {
			return err
		}
		switch value := request.Update.(type) {
		case *fastpathv2.UpdateRequest_ExpiresAtUnixSeconds:
			if value.ExpiresAtUnixSeconds == 0 {
				sandbox.Spec.ExpireTime = nil
			} else {
				expiresAt := metav1.NewTime(time.Unix(value.ExpiresAtUnixSeconds, 0))
				sandbox.Spec.ExpireTime = &expiresAt
			}
		case *fastpathv2.UpdateRequest_ResetRevision:
			parsed, err := time.Parse(time.RFC3339Nano, value.ResetRevision)
			if err != nil {
				return fmt.Errorf("invalid reset_revision: %w", err)
			}
			sandbox.Spec.ResetRevision = &metav1.Time{Time: parsed}
		case *fastpathv2.UpdateRequest_FailurePolicy:
			sandbox.Spec.FailurePolicy = toFailurePolicy(value.FailurePolicy)
		case *fastpathv2.UpdateRequest_RecoveryTimeoutSeconds:
			sandbox.Spec.RecoveryTimeoutSeconds = value.RecoveryTimeoutSeconds
		}
		if sandbox.Labels == nil {
			sandbox.Labels = make(map[string]string)
		}
		for name, value := range request.MetadataUpsert {
			sandbox.Labels[metadataLabelKey(name)] = value
		}
		for _, name := range request.MetadataDeleteKeys {
			delete(sandbox.Labels, metadataLabelKey(name))
		}
		return s.K8sClient.Update(ctx, &sandbox)
	})
	if err != nil {
		return &fastpathv2.UpdateResponse{Success: false, Message: err.Error()}, nil
	}
	var updated apiv1alpha2.Sandbox
	if err := s.K8sClient.Get(ctx, key, &updated); err != nil {
		return nil, err
	}
	return &fastpathv2.UpdateResponse{Success: true, Message: "desired state committed", Sandbox: sandboxInfo(&updated)}, nil
}

func toFailurePolicy(policy fastpathv2.FailurePolicy) apiv1alpha2.FailurePolicy {
	if policy == fastpathv2.FailurePolicy_AUTO_RECREATE {
		return apiv1alpha2.FailurePolicyAutoRecreate
	}
	return apiv1alpha2.FailurePolicyManual
}

func (s *Server) defaultNamespace() string {
	if s.DefaultNamespace != "" {
		return s.DefaultNamespace
	}
	return "fast-sandbox"
}
