package fastletproxy

import (
	dataplane "fast-sandbox/internal/dataplane/contract"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func testRoute(generation int64) Route {
	return Route{
		Namespace: "default", SandboxUID: "uid-a", FastletPodUID: "pod-a",
		AssignmentAttempt: generation, RouteGeneration: generation,
		Access: dataplane.AccessDescriptor{Kind: dataplane.AccessKindDirectIP, Address: "10.42.0.2"},
		State:  RouteReady,
	}
}

func TestStoreGenerationFencingAndTombstone(t *testing.T) {
	store := NewStore()
	_, err := store.Apply(testRoute(1))
	require.NoError(t, err)
	_, err = store.Apply(testRoute(1))
	require.NoError(t, err)

	conflict := testRoute(1)
	conflict.Access.Address = "10.42.0.3"
	_, err = store.Apply(conflict)
	require.ErrorIs(t, err, ErrRouteConflict)

	_, err = store.Apply(testRoute(2))
	require.NoError(t, err)
	_, err = store.Delete("uid-a", 1)
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = store.Delete("uid-a", 2)
	require.NoError(t, err)
	_, err = store.Apply(testRoute(2))
	require.ErrorIs(t, err, ErrRouteStale)
	_, err = store.Apply(testRoute(3))
	require.NoError(t, err)
}

func TestStoreDrainingRejectsLookup(t *testing.T) {
	store := NewStore()
	_, err := store.Apply(testRoute(1))
	require.NoError(t, err)
	_, err = store.MarkDraining("uid-a", 1)
	require.NoError(t, err)
	_, err = store.Lookup("uid-a")
	require.ErrorIs(t, err, ErrRouteDraining)
}

func TestStoreSnapshotsRouteValues(t *testing.T) {
	store := NewStore()
	route := testRoute(1)
	route.Components = map[string]dataplane.ComponentRoute{
		"execd": {Protocol: "HTTP", Port: 44772},
	}
	_, err := store.Apply(route)
	require.NoError(t, err)

	route.Access.Address = "10.42.0.99"
	route.Components["execd"] = dataplane.ComponentRoute{Protocol: "HTTP", Port: 8080}
	stored, err := store.Lookup(route.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, "10.42.0.2", stored.Access.Address)
	require.Equal(t, uint32(44772), stored.Components["execd"].Port)

	stored.Components["execd"] = dataplane.ComponentRoute{Protocol: "HTTP", Port: 9090}
	storedAgain, err := store.Lookup(route.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, uint32(44772), storedAgain.Components["execd"].Port)
}

func TestStoreConcurrentLookupAndRouteTransitions(t *testing.T) {
	store := NewStore()
	stable := testRoute(1)
	stable.SandboxUID = "stable"
	_, err := store.Apply(stable)
	require.NoError(t, err)

	const iterations = 1000
	errorsChannel := make(chan error, 1)
	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				route, lookupErr := store.Lookup("stable")
				if lookupErr != nil {
					select {
					case errorsChannel <- lookupErr:
					default:
					}
					return
				}
				if route.RouteGeneration != 1 {
					select {
					case errorsChannel <- ErrRouteConflict:
					default:
					}
					return
				}
			}
		}()
	}
	for generation := int64(1); generation <= iterations; generation++ {
		route := testRoute(generation)
		route.SandboxUID = "changing"
		_, err = store.Apply(route)
		require.NoError(t, err)
		_, err = store.MarkDraining(route.SandboxUID, generation)
		require.NoError(t, err)
		_, err = store.Delete(route.SandboxUID, generation)
		require.NoError(t, err)
	}
	readers.Wait()
	select {
	case err := <-errorsChannel:
		require.NoError(t, err)
	default:
	}
}
