package reconciler

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newSandboxTemplate(namespace, name string) *apiv1alpha2.SandboxTemplate {
	return &apiv1alpha2.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: apiv1alpha2.SandboxTemplateSpec{
			Image:  "registry.example.com/sandbox:v1.0.0",
			Kernel: "vmlinux.bin",
			Machine: apiv1alpha2.MachineSpec{
				VCPU:   "4",
				Memory: "8Gi",
			},
			Readiness: apiv1alpha2.ReadinessSpec{WarmupSeconds: 60},
			Output: apiv1alpha2.OutputSpec{
				RootfsSize: "30Gi",
				Format:     apiv1alpha2.ArtifactFormatOverlayBD,
				Publish:    "s3://bucket/sandbox-images/",
			},
		},
	}
}

func newPublishSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"accessKeyId":     []byte("AKID"),
			"secretAccessKey": []byte("SKEY"),
			"endpoint":        []byte("https://s3.example.com"),
			"region":          []byte("cn-hangzhou"),
		},
	}
}

func newSandboxTemplateReconciler(t *testing.T, objects ...client.Object) *SandboxTemplateReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha2 scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return &SandboxTemplateReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&apiv1alpha2.SandboxTemplate{}).
			WithObjects(objects...).Build(),
		Scheme:       scheme,
		BuilderImage: "fast-sandbox/sandboxtemplate-builder:dev",
		BuildTTL:     time.Hour,
	}
}

func reconcileOnce(t *testing.T, r *SandboxTemplateReconciler, key client.ObjectKey) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v (%T) isNotFound=%v", err, err, apierrors.IsNotFound(err))
	}
	return result
}

func listBuilderPods(t *testing.T, r *SandboxTemplateReconciler) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := r.List(context.Background(), &pods, client.InNamespace(builderNamespace)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return pods.Items
}

func TestSandboxTemplateReconcileCreatesPodAndTracksPhase(t *testing.T) {
	namespace, name := "tenant-a", "golden"
	template := newSandboxTemplate(namespace, name)
	template.Spec.Output.PublishSecretRef = &corev1.LocalObjectReference{Name: "publish-creds"}
	secret := newPublishSecret(namespace, "publish-creds")
	reconciler := newSandboxTemplateReconciler(t, template, secret)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	// First reconcile: Pending → Building, and a build Pod is created in
	// the platform namespace with the publish credentials injected.
	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected Building phase, got %s", updated.Status.Phase)
	}
	if !containsString(updated.Finalizers, sandboxTemplateFinalizer) {
		t.Fatalf("expected cleanup finalizer to be set")
	}
	pods := listBuilderPods(t, reconciler)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod in %s, got %d", builderNamespace, len(pods))
	}
	pod := &pods[0]
	if pod.Name != name+"-build-1" {
		t.Fatalf("expected deterministic pod name, got %q", pod.Name)
	}
	if pod.Labels[sandboxTemplateBuildLabel] != name {
		t.Fatalf("build pod missing template label")
	}
	if pod.Labels[sandboxTemplateNamespaceLabel] != namespace {
		t.Fatalf("build pod missing template namespace label")
	}
	if pod.Labels[sandboxTemplateGenerationLabel] != "1" {
		t.Fatalf("build pod missing generation label, got %q", pod.Labels[sandboxTemplateGenerationLabel])
	}

	// Protocol contract: the serialized SANDBOX_TEMPLATE_SPEC round-trips to
	// exactly the template's spec — the builder parses it as
	// SandboxTemplateSpec (not the wrapping CR).
	specJSON := ""
	for _, entry := range pod.Spec.Containers[0].Env {
		if entry.Name == builderImageEnv {
			specJSON = entry.Value
		}
	}
	if specJSON == "" {
		t.Fatalf("build pod missing %s env", builderImageEnv)
	}
	var specInPod apiv1alpha2.SandboxTemplateSpec
	if err := json.Unmarshal([]byte(specJSON), &specInPod); err != nil {
		t.Fatalf("SANDBOX_TEMPLATE_SPEC is not valid SandboxTemplateSpec JSON: %v", err)
	}
	if !reflect.DeepEqual(specInPod, template.Spec) {
		t.Fatalf("spec JSON round-trip mismatch:\n got %+v\nwant %+v", specInPod, template.Spec)
	}
	credentialEnv := map[string]string{}
	credentialRefs := map[string]struct{ name, key string }{}
	for _, entry := range pod.Spec.Containers[0].Env {
		if entry.ValueFrom != nil && entry.ValueFrom.SecretKeyRef != nil {
			credentialRefs[entry.Name] = struct{ name, key string }{entry.ValueFrom.SecretKeyRef.Name, entry.ValueFrom.SecretKeyRef.Key}
			continue
		}
		credentialEnv[entry.Name] = entry.Value
	}
	for name, ref := range map[string]struct{ name, key string }{
		"AWS_ACCESS_KEY_ID":     {"publish-creds", "accessKeyId"},
		"AWS_SECRET_ACCESS_KEY": {"publish-creds", "secretAccessKey"},
		"AWS_ENDPOINT_URL":      {"publish-creds", "endpoint"},
		"AWS_REGION":            {"publish-creds", "region"},
	} {
		got, ok := credentialRefs[name]
		if !ok || got != ref {
			t.Fatalf("expected %s to reference secret %s/%s, got %v", name, ref.name, ref.key, got)
		}
	}
	for name := range credentialEnv {
		switch name {
		case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_ENDPOINT_URL", "AWS_REGION":
			t.Fatalf("credential %s must not be inlined into the pod env", name)
		}
	}

	// A duplicate reconcile must be a no-op (deterministic name + AlreadyExists).
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler); len(pods) != 1 {
		t.Fatalf("expected exactly one build pod after duplicate reconcile, got %d", len(pods))
	}

	// Pod succeeds with reported annotations → phase becomes Succeeded and
	// manifestRef/artifactDigest are consumed before cleanup.
	pod.Annotations = map[string]string{
		sandboxTemplateManifestRefAnnot:  "s3://bucket/sandbox-images/abc123/manifest.json",
		sandboxTemplateArtifactDigestAnn: "deadbeef",
	}
	if err := reconciler.Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod metadata: %v", err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseSucceeded {
		t.Fatalf("expected Succeeded phase, got %s", updated.Status.Phase)
	}
	if updated.Status.LastBuildTime == nil {
		t.Fatalf("expected LastBuildTime to be set")
	}
	if updated.Status.ManifestRef != "s3://bucket/sandbox-images/abc123/manifest.json" {
		t.Fatalf("expected ManifestRef to be consumed, got %q", updated.Status.ManifestRef)
	}
	if updated.Status.ArtifactDigest != "deadbeef" {
		t.Fatalf("expected ArtifactDigest to be consumed, got %q", updated.Status.ArtifactDigest)
	}
	foundSucceeded := false
	for _, condition := range updated.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded && condition.Status == "True" {
			foundSucceeded = true
			if condition.LastTransitionTime == nil {
				t.Fatalf("expected LastTransitionTime on the success condition")
			}
		}
	}
	if !foundSucceeded {
		t.Fatalf("expected a True BuildSucceeded condition on success")
	}

	// Terminal builds are no-ops, and the finished Pod is cleaned up.
	result := reconcileOnce(t, reconciler, key)
	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for terminal build, got %v", result.RequeueAfter)
	}
	if pods := listBuilderPods(t, reconciler); len(pods) != 0 {
		t.Fatalf("expected the finished pod to be cleaned up, got %d", len(pods))
	}
}

func TestSandboxTemplateReconcileFailedPod(t *testing.T) {
	namespace, name := "tenant-a", "broken"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler)
	pod := &pods[0]
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "build",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Message: "boom",
		}},
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseFailed {
		t.Fatalf("expected Failed phase, got %s", updated.Status.Phase)
	}
	found := false
	for _, condition := range updated.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded && condition.Status == "False" {
			found = true
			if condition.Message != "build pod failed (exit code 1): boom" {
				t.Fatalf("expected failure message with exit code, got %q", condition.Message)
			}
		}
	}
	if !found {
		t.Fatalf("expected a Failed BuildSucceeded condition")
	}
}

func TestSandboxTemplateReconcileIgnoresStaleGenerationPod(t *testing.T) {
	namespace, name := "tenant-a", "stale"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	// First reconcile creates a Pod for generation 1.
	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod, got %d", len(pods))
	}

	// Spec changes: generation 2, status reset. The running Pod for
	// generation 1 is superseded and deleted immediately; a fresh Pod for
	// generation 2 is created instead (no concurrent stale builders).
	if err := reconciler.Get(context.Background(), key, template); err != nil {
		t.Fatalf("get template: %v", err)
	}
	template.Generation = 2
	if err := reconciler.Update(context.Background(), template); err != nil {
		t.Fatalf("update template: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	pods = listBuilderPods(t, reconciler)
	if len(pods) != 1 {
		t.Fatalf("expected the stale pod to be deleted and one fresh pod created, got %d", len(pods))
	}
	if pods[0].Labels[sandboxTemplateGenerationLabel] != "2" {
		t.Fatalf("expected a generation-2 build pod, got %q", pods[0].Labels[sandboxTemplateGenerationLabel])
	}
	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected Building phase after generation bump, got %s", updated.Status.Phase)
	}
}

func TestSandboxTemplateReconcileBuildTTLRetainsPod(t *testing.T) {
	namespace, name := "tenant-a", "ttl"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler)
	pod := &pods[0]
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionFalse,
		Reason:             "PodCompleted",
		LastTransitionTime: metav1.Now(),
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Within BuildTTL the finished Pod is retained (annotations still
	// inspectable); beyond TTL it is cleaned up.
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler); len(pods) != 1 {
		t.Fatalf("expected the finished pod to be retained within BuildTTL, got %d", len(pods))
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionFalse,
		Reason:             "PodCompleted",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler); len(pods) != 0 {
		t.Fatalf("expected the finished pod to be cleaned up after BuildTTL, got %d", len(pods))
	}
}

func TestSandboxTemplateDeletionCleansUpBuildPods(t *testing.T) {
	namespace, name := "tenant-a", "gone"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler); len(pods) != 1 {
		t.Fatalf("expected one build pod, got %d", len(pods))
	}

	// Deleting the template must reap the privileged build Pod (no
	// cross-namespace ownerRef) before the finalizer is removed.
	if err := reconciler.Delete(context.Background(), template); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler); len(pods) != 0 {
		t.Fatalf("expected build pods to be deleted with the template, got %d", len(pods))
	}
	// The finalizer is removed, so the object disappears.
	if err := reconciler.Get(context.Background(), key, &apiv1alpha2.SandboxTemplate{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the template to be gone after finalizer removal, got %v", err)
	}
}

func TestSandboxTemplateReconcileFailsStuckPendingPod(t *testing.T) {
	namespace, name := "tenant-a", "stuck"
	template := newSandboxTemplate(namespace, name)
	// Preset a build Pod that never got scheduled: old and still Pending.
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-build-1",
			Namespace: builderNamespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      name,
				sandboxTemplateNamespaceLabel:  namespace,
				sandboxTemplateGenerationLabel: "1",
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-podPendingTimeout - time.Minute)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}
	stale.Status.Phase = corev1.PodPending
	reconciler := newSandboxTemplateReconciler(t, template, stale)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseFailed {
		t.Fatalf("expected Failed phase for a stuck Pending pod, got %s", updated.Status.Phase)
	}
	found := false
	for _, condition := range updated.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded &&
			condition.Status == "False" && condition.Reason == "PodPendingTimeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a PodPendingTimeout failure condition")
	}
}

func containsString(list []string, want string) bool {
	for _, entry := range list {
		if entry == want {
			return true
		}
	}
	return false
}

func TestSandboxTemplateReconcileSelfHealsPendingPhase(t *testing.T) {
	namespace, name := "tenant-a", "recover"
	template := newSandboxTemplate(namespace, name)
	// Pod exists and runs, but the template status was never flipped to
	// Building (e.g. controller restarted between create and status update).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-build-1",
			Namespace: builderNamespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      name,
				sandboxTemplateNamespaceLabel:  namespace,
				sandboxTemplateGenerationLabel: "1",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}
	reconciler := newSandboxTemplateReconciler(t, template, pod)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected the Pending phase to self-heal to Building, got %s", updated.Status.Phase)
	}
}

func TestSandboxTemplatePendingTimeoutDeletesPod(t *testing.T) {
	namespace, name := "tenant-a", "leak"
	template := newSandboxTemplate(namespace, name)
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-build-1",
			Namespace: builderNamespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      name,
				sandboxTemplateNamespaceLabel:  namespace,
				sandboxTemplateGenerationLabel: "1",
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-podPendingTimeout - time.Minute)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}
	stale.Status.Phase = corev1.PodPending
	reconciler := newSandboxTemplateReconciler(t, template, stale)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler); len(pods) != 0 {
		t.Fatalf("expected the stuck Pending pod to be deleted, got %d", len(pods))
	}
}
