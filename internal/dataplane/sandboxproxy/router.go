package sandboxproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrSandboxNotFound    = errors.New("sandbox route not found")
	ErrSandboxNotReady    = errors.New("sandbox data plane is not ready")
	ErrFastletUnavailable = errors.New("assigned Fastlet Pod is unavailable")
)

type Route struct {
	Namespace         string
	SandboxUID        string
	FastletName       string
	FastletPodUID     string
	FastletPodIP      string
	AssignmentAttempt int64
	RouteGeneration   int64
}

type Resolver interface {
	Resolve(context.Context, string) (Route, error)
	ResolveFresh(context.Context, string) (Route, error)
}

type Index struct {
	// The informer callbacks publish immutable, routing-only projections.
	// sync.Map is a deliberate fit here: entries are stable and read for every
	// data-plane request, while writes are comparatively rare and normally
	// affect disjoint Sandbox or Pod identities.
	sandboxes sync.Map // Sandbox UID string -> *sandboxRouteState
	pods      sync.Map // Fastlet Pod UID string -> *fastletPodState
}

type sandboxRouteState struct {
	Namespace         string
	SandboxUID        string
	FastletName       string
	FastletPodUID     string
	AssignmentAttempt int64
	RouteGeneration   int64
	DataPlaneReady    bool
}

type fastletPodState struct {
	Namespace string
	Name      string
	UID       string
	IP        string
}

func NewIndex() *Index {
	return &Index{}
}

func (i *Index) UpsertSandbox(sandbox *apiv1alpha1.Sandbox) {
	if sandbox == nil || sandbox.UID == "" {
		return
	}
	routeGeneration := sandbox.Status.RouteGeneration
	if routeGeneration <= 0 {
		routeGeneration = 1
	}
	state := &sandboxRouteState{
		Namespace:       sandbox.Namespace,
		SandboxUID:      string(sandbox.UID),
		DataPlaneReady:  sandbox.Status.DataPlaneState == apiv1alpha1.ObservedStateReady,
		RouteGeneration: routeGeneration,
	}
	if assignment := sandbox.Status.Assignment; assignment != nil {
		state.FastletName = assignment.FastletName
		state.FastletPodUID = assignment.FastletPodUID
		state.AssignmentAttempt = assignment.Attempt
	}
	i.sandboxes.Store(state.SandboxUID, state)
}

func (i *Index) DeleteSandbox(sandbox *apiv1alpha1.Sandbox) {
	if sandbox == nil || sandbox.UID == "" {
		return
	}
	i.sandboxes.Delete(string(sandbox.UID))
}

func (i *Index) UpsertPod(pod *corev1.Pod) {
	if pod == nil || pod.UID == "" {
		return
	}
	state := &fastletPodState{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		UID:       string(pod.UID),
		IP:        pod.Status.PodIP,
	}
	i.pods.Store(state.UID, state)
}

func (i *Index) DeletePod(pod *corev1.Pod) {
	if pod == nil || pod.UID == "" {
		return
	}
	i.pods.Delete(string(pod.UID))
}

func (i *Index) Resolve(sandboxUID string) (Route, error) {
	sandboxValue, exists := i.sandboxes.Load(sandboxUID)
	if !exists {
		return Route{}, ErrSandboxNotFound
	}
	sandbox, valid := sandboxValue.(*sandboxRouteState)
	if !valid || sandbox == nil {
		return Route{}, ErrSandboxNotFound
	}
	if !sandbox.DataPlaneReady || sandbox.FastletName == "" || sandbox.FastletPodUID == "" || sandbox.AssignmentAttempt <= 0 {
		return Route{}, ErrSandboxNotReady
	}
	podValue, exists := i.pods.Load(sandbox.FastletPodUID)
	if !exists {
		return Route{}, ErrFastletUnavailable
	}
	pod, valid := podValue.(*fastletPodState)
	if !valid || pod == nil || pod.UID != sandbox.FastletPodUID ||
		pod.Name != sandbox.FastletName || pod.Namespace != sandbox.Namespace || pod.IP == "" {
		return Route{}, ErrFastletUnavailable
	}
	return Route{
		Namespace: sandbox.Namespace, SandboxUID: sandbox.SandboxUID,
		FastletName: sandbox.FastletName, FastletPodUID: sandbox.FastletPodUID, FastletPodIP: pod.IP,
		AssignmentAttempt: sandbox.AssignmentAttempt, RouteGeneration: sandbox.RouteGeneration,
	}, nil
}

type KubernetesResolver struct {
	Index  *Index
	Client client.Client
}

func (r *KubernetesResolver) Resolve(ctx context.Context, sandboxUID string) (Route, error) {
	if r.Index != nil {
		if route, err := r.Index.Resolve(sandboxUID); err == nil {
			return route, nil
		}
	}
	return r.ResolveFresh(ctx, sandboxUID)
}

// ResolveFresh is the bounded, authoritative read-after-create fallback. It
// runs only on a cache miss/lag or credential-generation mismatch; the steady
// state stays entirely watch-driven.
func (r *KubernetesResolver) ResolveFresh(ctx context.Context, sandboxUID string) (Route, error) {
	if r.Client == nil || sandboxUID == "" {
		return Route{}, ErrSandboxNotFound
	}
	var sandboxes apiv1alpha1.SandboxList
	if err := r.Client.List(ctx, &sandboxes); err != nil {
		return Route{}, fmt.Errorf("list Sandboxes for UID fallback: %w", err)
	}
	var sandbox *apiv1alpha1.Sandbox
	for index := range sandboxes.Items {
		if string(sandboxes.Items[index].UID) == sandboxUID {
			sandbox = sandboxes.Items[index].DeepCopy()
			break
		}
	}
	if sandbox == nil {
		return Route{}, ErrSandboxNotFound
	}
	if sandbox.Status.Assignment == nil || sandbox.Status.DataPlaneState != apiv1alpha1.ObservedStateReady {
		return Route{}, ErrSandboxNotReady
	}
	var pod corev1.Pod
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Status.Assignment.FastletName}, &pod); err != nil {
		return Route{}, fmt.Errorf("%w: %v", ErrFastletUnavailable, err)
	}
	if string(pod.UID) != sandbox.Status.Assignment.FastletPodUID || pod.Status.PodIP == "" {
		return Route{}, ErrFastletUnavailable
	}
	if r.Index != nil {
		r.Index.UpsertSandbox(sandbox)
		r.Index.UpsertPod(&pod)
	}
	return routeFromObjects(sandbox, &pod)
}

func routeFromObjects(sandbox *apiv1alpha1.Sandbox, pod *corev1.Pod) (Route, error) {
	if sandbox.Status.Assignment == nil {
		return Route{}, ErrSandboxNotReady
	}
	generation := sandbox.Status.RouteGeneration
	if generation <= 0 {
		generation = 1
	}
	return Route{
		Namespace: sandbox.Namespace, SandboxUID: string(sandbox.UID),
		FastletName: sandbox.Status.Assignment.FastletName, FastletPodUID: sandbox.Status.Assignment.FastletPodUID,
		FastletPodIP: pod.Status.PodIP, AssignmentAttempt: sandbox.Status.Assignment.Attempt,
		RouteGeneration: generation,
	}, nil
}
