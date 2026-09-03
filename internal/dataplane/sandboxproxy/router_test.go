package sandboxproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesResolverUsesProjectedAssignmentAndWarmsIndex(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	sandbox := sandboxWithProjectedAssignment(t, "fastlet-a", "pod-a", 3, 4)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	}
	index := NewIndex()
	resolver := &KubernetesResolver{Index: index, Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pod).Build()}

	route, err := resolver.Resolve(context.Background(), "uid-a")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.8", route.FastletPodIP)
	require.Equal(t, int64(4), route.RouteGeneration)

	route, err = index.Resolve("uid-a")
	require.NoError(t, err)
	require.Equal(t, "pod-a", route.FastletPodUID)
}

func TestKubernetesResolverUsesAssignmentAnnotationBeforeStatusProjectionAndWarmsIndex(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	sandbox := sandboxWithAssignment(t, "fastlet-a", "pod-a", 3, 4)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	}
	index := NewIndex()
	resolver := &KubernetesResolver{Index: index, Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pod).Build()}

	route, err := resolver.ResolveFresh(context.Background(), "uid-a")
	require.NoError(t, err)
	require.Equal(t, "fastlet-a", route.FastletName)
	require.Equal(t, "pod-a", route.FastletPodUID)
	require.Equal(t, int64(3), route.AssignmentAttempt)
	require.Equal(t, int64(4), route.RouteGeneration)

	route, err = index.Resolve("uid-a")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.8", route.FastletPodIP)
}

func TestIndexUsesAssignmentAnnotationBeforeStatusProjection(t *testing.T) {
	index := NewIndex()
	index.UpsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	})
	index.UpsertSandbox(sandboxWithAssignment(t, "fastlet-a", "pod-a", 3, 4))

	route, err := index.Resolve("uid-a")
	require.NoError(t, err)
	require.Equal(t, "fastlet-a", route.FastletName)
	require.Equal(t, "pod-a", route.FastletPodUID)
	require.Equal(t, int64(3), route.AssignmentAttempt)
	require.Equal(t, int64(4), route.RouteGeneration)
}

func TestAssignmentProjectionConflictFailsClosed(t *testing.T) {
	sandbox := sandboxWithAssignment(t, "fastlet-a", "pod-a", 3, 4)
	sandbox.Status = readyRouteStatus("fastlet-b", "pod-b", 3, 4)
	sandbox.Status.Runtime.Generation = 1

	index := NewIndex()
	index.UpsertSandbox(sandbox)
	index.UpsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	})
	_, err := index.Resolve("uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	resolver := &KubernetesResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()}
	_, err = resolver.ResolveFresh(context.Background(), "uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.ErrorIs(t, err, assignment.ErrAssignmentProjectionConflict)
}

func TestNewerStatusProjectionFailsClosed(t *testing.T) {
	sandbox := sandboxWithAssignment(t, "fastlet-a", "pod-a", 3, 4)
	sandbox.Status = readyRouteStatus("fastlet-b", "pod-b", 4, 5)
	sandbox.Status.Runtime.Generation = 2

	index := NewIndex()
	index.UpsertSandbox(sandbox)
	index.UpsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	})
	_, err := index.Resolve("uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	resolver := &KubernetesResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()}
	_, err = resolver.ResolveFresh(context.Background(), "uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.ErrorIs(t, err, assignment.ErrAssignmentProjectionConflict)
}

func TestNewerAssignmentWithStaleStatusProjectionFailsClosed(t *testing.T) {
	sandbox := sandboxWithAssignment(t, "fastlet-b", "pod-b", 4, 6)
	sandbox.Status = readyRouteStatus("fastlet-a", "pod-a", 3, 5)

	index := NewIndex()
	index.UpsertSandbox(sandbox)
	index.UpsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-b", Namespace: "tenant-a", UID: types.UID("pod-b")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.9"},
	})
	_, err := index.Resolve("uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	resolver := &KubernetesResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()}
	_, err = resolver.ResolveFresh(context.Background(), "uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.ErrorIs(t, err, assignment.ErrAssignmentProjectionConflict)
}

func TestStatusOnlyProjectionFailsClosed(t *testing.T) {
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
		Status:     readyRouteStatus("fastlet-a", "pod-a", 3, 4),
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	}
	index := NewIndex()
	index.UpsertSandbox(sandbox)
	index.UpsertPod(pod)

	_, err := index.Resolve("uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	resolver := &KubernetesResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pod).Build()}
	_, err = resolver.ResolveFresh(context.Background(), "uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.ErrorIs(t, err, assignment.ErrAssignmentAnnotationMissing)
}

func TestMalformedAssignmentAnnotationDoesNotFallBackToStatus(t *testing.T) {
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a"),
			Annotations: map[string]string{assignment.AnnotationAssignment: "{"},
		},
		Status: readyRouteStatus("fastlet-a", "pod-a", 3, 4),
	}
	index := NewIndex()
	index.UpsertSandbox(sandbox)
	index.UpsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	})

	_, err := index.Resolve("uid-a")
	require.ErrorIs(t, err, ErrSandboxNotReady)
}

func TestIndexPublishesImmutableRouteProjection(t *testing.T) {
	index := NewIndex()
	sandbox := sandboxWithProjectedAssignment(t, "fastlet-a", "pod-a", 3, 4)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	}
	index.UpsertSandbox(sandbox)
	index.UpsertPod(pod)

	sandbox.Status.Placement.FastletPodUID = "mutated-pod"
	sandbox.Status.DataPlane.RouteGeneration = 99
	pod.Status.PodIP = "10.0.0.99"

	route, err := index.Resolve("uid-a")
	require.NoError(t, err)
	require.Equal(t, "pod-a", route.FastletPodUID)
	require.Equal(t, "10.0.0.8", route.FastletPodIP)
	require.Equal(t, int64(4), route.RouteGeneration)
}

func TestIndexConcurrentUpdatesAndResolves(t *testing.T) {
	index := NewIndex()
	const sandboxUID = "uid-a"
	upsert := func(suffix string, generation int64) {
		podUID := "pod-" + suffix
		index.UpsertPod(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "fastlet-" + suffix, Namespace: "tenant-a", UID: types.UID(podUID)},
			Status:     corev1.PodStatus{PodIP: "10.0.0." + suffix},
		})
		index.UpsertSandbox(sandboxWithProjectedAssignment(t, "fastlet-"+suffix, podUID, generation, generation))
	}
	upsert("1", 1)

	const iterations = 2000
	errorsChannel := make(chan error, 1)
	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				route, err := index.Resolve(sandboxUID)
				if err != nil {
					if errors.Is(err, ErrFastletUnavailable) {
						continue
					}
					select {
					case errorsChannel <- err:
					default:
					}
					return
				}
				expectedIP := "10.0.0." + route.FastletPodUID[len("pod-"):]
				if route.FastletPodIP != expectedIP {
					select {
					case errorsChannel <- fmt.Errorf("route %s resolved mismatched IP %s", route.FastletPodUID, route.FastletPodIP):
					default:
					}
					return
				}
			}
		}()
	}
	for iteration := 2; iteration <= iterations; iteration++ {
		suffix := "1"
		if iteration%2 == 0 {
			suffix = "2"
		}
		upsert(suffix, int64(iteration))
	}
	readers.Wait()
	select {
	case err := <-errorsChannel:
		require.NoError(t, err)
	default:
	}
}

func readyRouteStatus(fastletName, podUID string, attempt, routeGeneration int64) apiv1alpha2.SandboxStatus {
	return apiv1alpha2.SandboxStatus{
		Placement: apiv1alpha2.PlacementStatus{FastletName: fastletName, FastletPodUID: types.UID(podUID), Attempt: attempt},
		Runtime:   apiv1alpha2.RuntimeStatus{Generation: 1},
		DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: routeGeneration},
	}
}

func sandboxWithProjectedAssignment(t *testing.T, fastletName, podUID string, attempt, routeGeneration int64) *apiv1alpha2.Sandbox {
	t.Helper()
	sandbox := sandboxWithAssignment(t, fastletName, podUID, attempt, routeGeneration)
	sandbox.Status = readyRouteStatus(fastletName, podUID, attempt, routeGeneration)
	return sandbox
}

func sandboxWithAssignment(t *testing.T, fastletName, podUID string, attempt, routeGeneration int64) *apiv1alpha2.Sandbox {
	t.Helper()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, assignment.AssignmentEnvelope{
		Version:     assignment.AssignmentEnvelopeVersion,
		FastletName: fastletName, FastletPodUID: podUID, NodeName: "node-a",
		Attempt: attempt, InstanceGeneration: 1, RouteGeneration: routeGeneration,
		RuntimeInstanceID: "runtime-a", RuntimeProfileHash: "runtime-hash",
		ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}))
	return sandbox
}
