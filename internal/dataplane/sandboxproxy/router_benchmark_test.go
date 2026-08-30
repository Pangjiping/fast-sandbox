package sandboxproxy

import (
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func BenchmarkIndexResolveParallel(b *testing.B) {
	const routeCount = 1024
	index := NewIndex()
	for routeIndex := 0; routeIndex < routeCount; routeIndex++ {
		suffix := strconv.Itoa(routeIndex)
		sandboxUID := "sandbox-" + suffix
		podUID := "pod-" + suffix
		index.UpsertSandbox(&apiv1alpha2.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: sandboxUID, Namespace: "default", UID: types.UID(sandboxUID)},
			Status:     readyRouteStatus(podUID, podUID, int64(routeIndex+1), int64(routeIndex+1)),
		})
		index.UpsertPod(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podUID, Namespace: "default", UID: types.UID(podUID)},
			Status:     corev1.PodStatus{PodIP: fmt.Sprintf("10.42.%d.%d", routeIndex/256, routeIndex%256)},
		})
	}

	b.ReportAllocs()
	b.SetParallelism(4)
	var workerSequence atomic.Uint64
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		routeIndex := int(workerSequence.Add(1)-1) % routeCount
		var observed int64
		for parallel.Next() {
			route, err := index.Resolve("sandbox-" + strconv.Itoa(routeIndex))
			if err != nil {
				b.Fatal(err)
			}
			observed += route.RouteGeneration
			routeIndex++
			if routeIndex == routeCount {
				routeIndex = 0
			}
		}
		runtime.KeepAlive(observed)
	})
}
