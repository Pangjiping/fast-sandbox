package fastpath

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	routeauth "fast-sandbox/internal/dataplane/auth"
	dataplane "fast-sandbox/internal/dataplane/contract"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolveEndpointIssuesInstanceFencedCredential(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, func() time.Time { return now })
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	envelope := assignment.AssignmentEnvelope{
		Version: assignment.AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a",
		NodeName: "node-a", Attempt: 3, InstanceGeneration: 1, RouteGeneration: 5,
		RuntimeInstanceID: "runtime-a", RuntimeProfileHash: "runtime-hash",
		ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
	projected := envelope.StatusAssignment()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{PoolRef: "pool-a"},
		Status: apiv1alpha2.SandboxStatus{
			RuntimeState: apiv1alpha2.ObservedStateReady, DataPlaneState: apiv1alpha2.ObservedStateReady, RouteGeneration: 5,
			Assignment: &projected, AssignmentAttempt: 3, InstanceGeneration: 1,
		},
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a"}}
	server := &Server{
		K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pool).Build(), CredentialIssuer: issuer,
		SandboxProxyBaseURL: "https://proxy.example.test",
	}
	response, err := server.ResolveEndpoint(context.Background(), &fastpathv2.ResolveEndpointRequest{
		Sandbox: &fastpathv2.SandboxReference{Reference: &fastpathv2.SandboxReference_SandboxUid{SandboxUid: "uid-a"}},
		Target:  &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: 8080}},
	})
	require.NoError(t, err)
	require.Equal(t, "https://proxy.example.test/v1/sandboxes/uid-a/ports/8080", response.ProxyEndpoint)
	require.Equal(t, int64(5), response.RouteGeneration)
	token := response.RequiredHeaders[dataplane.HeaderRouteCredential]
	claims, err := verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: "tenant-a", SandboxUID: "uid-a", TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: 8080, FastletPodUID: "pod-a",
		AssignmentAttempt: 3, RouteGeneration: 5,
	})
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute).Unix(), claims.ExpiresAt)

	_, err = verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: "tenant-a", SandboxUID: "uid-a", TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: 8080, FastletPodUID: "pod-a",
		AssignmentAttempt: 4, RouteGeneration: 6,
	})
	require.ErrorIs(t, err, routeauth.ErrClaimMismatch)
}

func TestResolveEndpointRequiresDataPlaneReady(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{PoolRef: "pool-a"},
		Status:     apiv1alpha2.SandboxStatus{Assignment: &apiv1alpha2.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1, NodeName: "node-a"}},
	}
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	server := &Server{K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pool).Build(), CredentialIssuer: issuer, SandboxProxyBaseURL: "http://proxy"}
	_, err = server.ResolveEndpoint(context.Background(), &fastpathv2.ResolveEndpointRequest{
		Sandbox: &fastpathv2.SandboxReference{Reference: &fastpathv2.SandboxReference_SandboxUid{SandboxUid: "uid-a"}},
		Target:  &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: 80}},
	})
	require.Equal(t, codes.Unavailable, status.Code(err))
}
