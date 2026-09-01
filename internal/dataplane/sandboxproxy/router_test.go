package sandboxproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesResolverUsesAuthoritativeFallbackAndWarmsIndex(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
		Status:     readyRouteStatus("fastlet-a", "pod-a", 3, 4),
	}
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

func TestIndexPublishesImmutableRouteProjection(t *testing.T) {
	index := NewIndex()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
		Status:     readyRouteStatus("fastlet-a", "pod-a", 3, 4),
	}
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
		index.UpsertSandbox(&apiv1alpha2.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID(sandboxUID)},
			Status:     readyRouteStatus("fastlet-"+suffix, podUID, generation, generation),
		})
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
		DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: routeGeneration},
	}
}
