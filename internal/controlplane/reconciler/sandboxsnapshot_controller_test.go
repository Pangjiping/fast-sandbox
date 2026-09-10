package reconciler

import (
	"context"
	"testing"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type snapshotHarness struct {
	reconciler *SandboxSnapshotReconciler
	fastlet    *controllerFastlet
	k8sClient  client.Client
}

func newSnapshotReconcilerHarness(t *testing.T) (*snapshotHarness, *apiv1alpha2.SandboxSnapshot) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	sandbox := snapshotTargetSandbox(t)
	snapshot := &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
		Spec: apiv1alpha2.SandboxSnapshotSpec{
			SandboxRef:   apiv1alpha2.SandboxRef{Name: "sandbox-a", Namespace: "default", UID: "sandbox-uid-a"},
			TemplateName: "app-v2",
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha2.Sandbox{}, &apiv1alpha2.SandboxSnapshot{}).
		WithObjects(sandbox, snapshot).Build()
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1", NodeName: "node-a",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-hash",
		ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash", InfraReady: true,
	}
	registry := &controllerRegistry{
		candidates: []placement.FastletInfo{candidate},
		fastlets:   map[placement.FastletID]placement.FastletInfo{"fastlet-a": candidate},
	}
	fastlet := &controllerFastlet{runtimes: make(map[string]string)}
	orchestrator := &orchestration.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet}
	reconciler := &SandboxSnapshotReconciler{Client: k8sClient, Scheme: scheme, Orchestrator: orchestrator}
	return &snapshotHarness{reconciler: reconciler, fastlet: fastlet, k8sClient: k8sClient}, snapshot
}

func snapshotTargetSandbox(t *testing.T) *apiv1alpha2.Sandbox {
	t.Helper()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("sandbox-uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	envelope := assignment.AssignmentEnvelope{
		Version: assignment.AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a", NodeName: "node-a",
		Attempt: 1, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-a",
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	sandbox.Status = apiv1alpha2.SandboxStatus{
		Placement: apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1},
		Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady, Generation: 1},
		DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: 1},
	}
	return sandbox
}

func snapshotRequestFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getSnapshot(t *testing.T, harness *snapshotHarness, name string) *apiv1alpha2.SandboxSnapshot {
	t.Helper()
	var current apiv1alpha2.SandboxSnapshot
	require.NoError(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &current))
	return &current
}

func snapshotCompletedCondition(t *testing.T, snapshot *apiv1alpha2.SandboxSnapshot) *metav1.Condition {
	t.Helper()
	for index := range snapshot.Status.Conditions {
		if snapshot.Status.Conditions[index].Type == apiv1alpha2.SandboxSnapshotConditionCompleted {
			return &snapshot.Status.Conditions[index]
		}
	}
	t.Fatalf("Completed condition is missing")
	return nil
}

func TestSnapshotReconcileAddsFinalizerThenConverges(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)

	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.True(t, result.Requeue)
	first := getSnapshot(t, harness, "snap-a")
	require.Contains(t, first.Finalizers, SandboxSnapshotFinalizerName)

	result, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Equal(t, SnapshotObservationPollInterval, result.RequeueAfter)
	second := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseCreating, second.Status.Phase)
	require.Equal(t, "fastlet-a", second.Status.FastletName)
	require.NotEmpty(t, second.Status.SnapshotID)

	result, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Zero(t, result.Requeue, "terminal snapshots stop reconciling")
	final := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, final.Status.Phase)
	require.Equal(t, "s3://bucket/publish/abc123/manifest.json", final.Status.ManifestRef)
	require.Equal(t, int64(42), final.Status.SizeBytes)
	require.Equal(t, types.UID("sandbox-uid-a"), final.Status.SandboxUID)
	require.NotNil(t, final.Status.CompletedAt)
	require.Equal(t, metav1.ConditionTrue, snapshotCompletedCondition(t, final).Status)
}

func TestSnapshotReconcileFailsWhenTargetMissing(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	require.NoError(t, harness.k8sClient.Delete(context.Background(), snapshotTargetSandbox(t)))

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, current.Status.Phase)
	require.Equal(t, "SandboxNotFound", snapshotCompletedCondition(t, current).Reason)
}

func TestSnapshotReconcileDeterministicRejectionFails(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotCreateErr = &fastletapi.FastletError{Code: fastletapi.ErrorSnapshotInProgress, Message: "busy"}
	harness.fastlet.mu.Unlock()

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	current := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, current.Status.Phase)
	require.Equal(t, "SnapshotInProgress", snapshotCompletedCondition(t, current).Reason)
}

func TestSnapshotReconcileLostTaskFails(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)

	current := getSnapshot(t, harness, "snap-a")
	current.Status.SnapshotID = "snap-known"
	current.Status.Phase = apiv1alpha2.SandboxSnapshotPhaseCreating
	require.NoError(t, harness.reconciler.Status().Update(context.Background(), current))

	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotInspectErr = &fastletapi.FastletError{Code: fastletapi.ErrorNotFound, Message: "gone"}
	harness.fastlet.mu.Unlock()

	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	final := getSnapshot(t, harness, "snap-a")
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, final.Status.Phase)
	require.Equal(t, "SnapshotLost", snapshotCompletedCondition(t, final).Reason)
}

func TestSnapshotDeletionWaitsForRunningDump(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	harness.fastlet.mu.Lock()
	harness.fastlet.snapshotInspectPhase = fastletapi.SnapshotPhaseCreating
	harness.fastlet.mu.Unlock()

	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.NoError(t, harness.k8sClient.Delete(context.Background(), &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
	}))

	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Equal(t, SnapshotObservationPollInterval, result.RequeueAfter, "deletion waits for the running dump")
	current := getSnapshot(t, harness, "snap-a")
	require.Contains(t, current.Finalizers, SandboxSnapshotFinalizerName)
}

func TestSnapshotDeletionCleansUpAfterTerminal(t *testing.T) {
	harness, _ := newSnapshotReconcilerHarness(t)
	_, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	_, err = harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.NoError(t, harness.k8sClient.Delete(context.Background(), &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: types.UID("snapshot-uid-a")},
	}))

	result, err := harness.reconciler.Reconcile(context.Background(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	require.Zero(t, result.Requeue)

	harness.fastlet.mu.Lock()
	deleteCalls := harness.fastlet.snapshotDeleteCall
	harness.fastlet.mu.Unlock()
	require.Equal(t, 1, deleteCalls, "node-local artifacts are cleaned up best-effort")

	var current apiv1alpha2.SandboxSnapshot
	require.Error(t, harness.reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &current),
		"finalizer removal lets the object disappear")
}
