package fastletproxy

import (
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
)

func BenchmarkStoreLookupParallel(b *testing.B) {
	const routeCount = 128
	store := NewStore()
	for routeIndex := 0; routeIndex < routeCount; routeIndex++ {
		route := testRoute(int64(routeIndex + 1))
		route.SandboxUID = "sandbox-" + strconv.Itoa(routeIndex)
		if _, err := store.Apply(route); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.SetParallelism(4)
	var workerSequence atomic.Uint64
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		routeIndex := int(workerSequence.Add(1)-1) % routeCount
		var observed int64
		for parallel.Next() {
			route, err := store.Lookup("sandbox-" + strconv.Itoa(routeIndex))
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

func BenchmarkStoreApplyExistingRoute(b *testing.B) {
	const routeCount = 128
	store := NewStore()
	for routeIndex := 0; routeIndex < routeCount; routeIndex++ {
		route := testRoute(int64(routeIndex + 1))
		route.SandboxUID = "sandbox-" + strconv.Itoa(routeIndex)
		if _, err := store.Apply(route); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		route := testRoute(int64(routeCount + iteration + 1))
		route.SandboxUID = "sandbox-0"
		if _, err := store.Apply(route); err != nil {
			b.Fatal(err)
		}
	}
}
