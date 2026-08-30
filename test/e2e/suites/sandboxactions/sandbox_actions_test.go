package sandboxactions

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestSandboxActionsBindingHooksRecoveryAndDelete(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("sandbox-actions-binding-lifecycle").
		WithLabel("suite", "sandboxactions").WithLabel("tier", "smoke").
		Assess("ordered Action Bindings converge through Pod-local Handlers", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("actions")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatal(err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := actionPool(namespace)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create Pool: %v", err)
			}
			poolCtx, cancelPool := context.WithTimeout(ctx, 90*time.Second)
			_, err := fixture.WaitForReadyFastletPods(poolCtx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, 1)
			cancelPool()
			if err != nil {
				t.Fatalf("wait for Action-ready Fastlet: %v", err)
			}

			sandbox := &apiv1alpha2.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "action-sandbox", Namespace: namespace},
				Spec: apiv1alpha2.SandboxSpec{
					Image: "docker.io/library/alpine:latest", Command: []string{"/bin/sleep", "3600"}, PoolRef: pool.Name,
					ActionBindings: []apiv1alpha2.ActionBinding{
						binding("audit", ""),
						binding("egress", "null"),
					},
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create Sandbox: %v", err)
			}
			key := types.NamespacedName{Namespace: namespace, Name: sandbox.Name}
			ready := waitActions(t, ctx, fixture, key, "audit", "egress")
			restartPodContainerAndWait(t, ctx, k8sClient, namespace, ready.Status.Placement.FastletName, "fastlet")
			assertHandlerLogSequence(t, ctx, namespace, pool.Name, string(ready.UID), []string{
				`targetPort=18081 operation=SET_BINDING input=""`,
				`targetPort=18080 operation=SET_BINDING input="null"`,
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1",
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2",
				`targetPort=18081 operation=SET_BINDING input=""&&duplicate=true`,
				`targetPort=18080 operation=SET_BINDING input="null"&&duplicate=true`,
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1&&duplicate=true",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1&&duplicate=true",
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2&&duplicate=true",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2&&duplicate=true",
			})
			_ = waitActions(t, ctx, fixture, key, "audit", "egress")

			restartPodContainerAndWait(t, ctx, k8sClient, namespace, ready.Status.Placement.FastletName, "sandbox-action-fixture")
			assertHandlerLogSequence(t, ctx, namespace, pool.Name, string(ready.UID), []string{
				`targetPort=18081 operation=SET_BINDING input=""`,
				`targetPort=18080 operation=SET_BINDING input="null"`,
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1",
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2",
			})
			_ = waitActions(t, ctx, fixture, key, "audit", "egress")

			current := &apiv1alpha2.Sandbox{}
			if err := k8sClient.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			current.Spec.ActionBindings = []apiv1alpha2.ActionBinding{
				binding("egress", `{"policy":"allow-api"}`),
				binding("audit", `{"sequence":2}`),
			}
			if err := k8sClient.Update(ctx, current); err != nil {
				t.Fatalf("reorder and update Action Bindings: %v", err)
			}
			_ = waitActions(t, ctx, fixture, key, "egress", "audit")

			if err := k8sClient.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			current.Spec.ActionBindings = []apiv1alpha2.ActionBinding{binding("egress", `{"policy":"allow-api"}`)}
			if err := k8sClient.Update(ctx, current); err != nil {
				t.Fatalf("remove audit Action Binding: %v", err)
			}
			_ = waitActions(t, ctx, fixture, key, "egress")

			if err := k8sClient.Delete(ctx, current); err != nil {
				t.Fatalf("delete Sandbox: %v", err)
			}
			deleteCtx, cancelDelete := context.WithTimeout(ctx, 90*time.Second)
			if err := fixture.WaitForSandboxDeleted(deleteCtx, key); err != nil {
				cancelDelete()
				t.Fatalf("wait delete: %v", err)
			}
			cancelDelete()

			assertHandlerLogSequence(t, ctx, namespace, pool.Name, string(ready.UID), []string{
				`targetPort=18081 operation=SET_BINDING input=""`,
				`targetPort=18080 operation=SET_BINDING input="null"`,
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.runtime-ready/1",
				"targetPort=18081 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2",
				"targetPort=18080 operation=LIFECYCLE_HOOK input=<none> hook=sandbox.data-plane-ready/2",
				`targetPort=18080 operation=SET_BINDING input="{\"policy\":\"allow-api\"}"`,
				`targetPort=18081 operation=SET_BINDING input="{\"sequence\":2}"`,
				"targetPort=18081 operation=SET_BINDING input=<nil>",
				`targetPort=18080 operation=SET_BINDING input="{\"policy\":\"allow-api\"}"`,
				"targetPort=18080 operation=REMOVE_BINDING input=<none>",
			})
			assertRemoveTimeoutDoesNotBlockDeletion(t, ctx, fixture, k8sClient, namespace, pool.Name)
			assertFastPathReadyCreate(t, ctx, fixture, namespace, pool.Name)
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func assertRemoveTimeoutDoesNotBlockDeletion(t *testing.T, ctx context.Context, fixture *fixtures.FixtureClient, k8sClient ctrlclient.Client, namespace, poolName string) {
	t.Helper()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "action-timeout-sandbox", Namespace: namespace},
		Spec: apiv1alpha2.SandboxSpec{
			Image: "docker.io/library/alpine:latest", Command: []string{"/bin/sleep", "3600"}, PoolRef: poolName,
			ActionBindings: []apiv1alpha2.ActionBinding{binding("audit", "terminal-timeout")},
		},
	}
	if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
		t.Fatalf("create terminal-timeout Sandbox: %v", err)
	}
	key := types.NamespacedName{Namespace: namespace, Name: sandbox.Name}
	ready := waitActions(t, ctx, fixture, key, "audit")
	started := time.Now()
	if err := k8sClient.Delete(ctx, ready); err != nil {
		t.Fatalf("delete terminal-timeout Sandbox: %v", err)
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := fixture.WaitForSandboxDeleted(deleteCtx, key); err != nil {
		t.Fatalf("Handler timeout blocked Sandbox teardown: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 25*time.Second {
		t.Fatalf("terminal Handler cleanup blocked Sandbox teardown for %s", elapsed)
	}
	logs := assertHandlerLogSequence(t, ctx, namespace, poolName, string(ready.UID), []string{
		"targetPort=18081 operation=REMOVE_BINDING&&injectedRemoveDelay=true",
		"targetPort=18081 operation=REMOVE_BINDING&&injectedRemoveDelayComplete=true&&cancelled=true",
	})
	assertRemoveDelayDuration(t, logs, string(ready.UID), 4*time.Second, 7*time.Second)
}

func assertFastPathReadyCreate(t *testing.T, ctx context.Context, fixture *fixtures.FixtureClient, namespace, poolName string) {
	t.Helper()
	endpoint, forward, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
	if err != nil {
		t.Fatalf("start FastPath port-forward: %v", err)
	}
	defer forward.Cleanup()
	dialCtx, cancelDial := context.WithTimeout(ctx, 20*time.Second)
	defer cancelDial()
	connection, err := grpc.DialContext(dialCtx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial FastPath: %v", err)
	}
	defer connection.Close()
	client := fastpathv2.NewFastPathServiceClient(connection)

	requestCtx, cancelRequest := context.WithTimeout(ctx, 90*time.Second)
	defer cancelRequest()
	created, err := client.CreateSandbox(requestCtx, &fastpathv2.CreateSandboxRequest{
		RequestId: namespace + "-fastpath-actions", Namespace: namespace, PoolRef: poolName,
		Image: "docker.io/library/alpine:latest", Command: []string{"/bin/sleep", "3600"},
		ActionBindings: []*fastpathv2.ActionBinding{
			{Handler: "audit", Input: `{"source":"fastpath"}`},
			{Handler: "egress", Input: `{"policy":"deny"}`},
		},
	})
	if err != nil {
		t.Fatalf("FastPath CreateSandbox with Action Bindings: %v", err)
	}
	if created.GetCompletion() != fastpathv2.CreateCompletion_CREATE_COMPLETION_READY || created.GetGeneration() <= 0 {
		t.Fatalf("FastPath Create completion=%s generation=%d", created.GetCompletion(), created.GetGeneration())
	}
	assertLiveReadyInfo(t, created.GetSandbox(), created.GetGeneration(), "audit", "egress")

	observed, err := client.GetSandbox(requestCtx, &fastpathv2.GetSandboxRequest{
		Sandbox:            sandboxReference(namespace, created.GetSandbox().GetIdentity().GetName(), created.GetSandbox().GetIdentity().GetUid()),
		ExpectedGeneration: created.GetGeneration(),
	})
	if err != nil {
		t.Fatalf("FastPath GetSandbox at committed generation: %v", err)
	}
	assertLiveReadyInfo(t, observed.GetSandbox(), created.GetGeneration(), "audit", "egress")
	if observed.GetSandbox().GetIdentity().GetUid() != created.GetSandbox().GetIdentity().GetUid() {
		t.Fatalf("GetSandbox identity differs from CreateSandbox")
	}

	name := created.GetSandbox().GetIdentity().GetName()
	updated, err := client.UpdateSandbox(requestCtx, &fastpathv2.UpdateSandboxRequest{
		Sandbox:            sandboxReference(namespace, name, created.GetSandbox().GetIdentity().GetUid()),
		ExpectedGeneration: created.GetGeneration(),
		Update: &fastpathv2.UpdateSandboxRequest_ActionBindings{ActionBindings: &fastpathv2.ReplaceActionBindings{Items: []*fastpathv2.ActionBinding{
			{Handler: "egress", Input: `{"policy":"allow-fastpath"}`},
			{Handler: "audit", Input: `{"source":"fastpath-update"}`},
		}}},
	})
	if err != nil {
		t.Fatalf("FastPath UpdateSandbox Action Bindings: %v", err)
	}
	if updated.GetCommittedGeneration() <= created.GetGeneration() {
		t.Fatalf("FastPath Update committed generation=%d, create generation=%d", updated.GetCommittedGeneration(), created.GetGeneration())
	}
	updateCtx, cancelUpdate := context.WithTimeout(ctx, 90*time.Second)
	defer cancelUpdate()
	for {
		observed, err = client.GetSandbox(updateCtx, &fastpathv2.GetSandboxRequest{
			Sandbox: sandboxReference(namespace, name, created.GetSandbox().GetIdentity().GetUid()), ExpectedGeneration: updated.GetCommittedGeneration(),
		})
		if err == nil && observed.GetSandbox().GetReady() && observed.GetSandbox().GetAppliedGeneration() >= updated.GetCommittedGeneration() {
			break
		}
		select {
		case <-updateCtx.Done():
			t.Fatalf("wait for FastPath Action update: %v; last response=%+v error=%v", updateCtx.Err(), observed, err)
		case <-time.After(250 * time.Millisecond):
		}
	}
	assertLiveReadyInfo(t, observed.GetSandbox(), updated.GetCommittedGeneration(), "egress", "audit")
	resolved, err := client.ResolveEndpoint(requestCtx, &fastpathv2.ResolveEndpointRequest{
		Sandbox:            sandboxReference(namespace, name, created.GetSandbox().GetIdentity().GetUid()),
		Target:             &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: 8080}},
		ExpectedGeneration: updated.GetCommittedGeneration(),
	})
	if err != nil || resolved.GetProxyEndpoint() == "" || resolved.GetRouteGeneration() <= 0 ||
		resolved.GetEndpoint() == nil || resolved.GetEndpoint().GetPort() != 8080 || resolved.GetEndpoint().GetProtocol() == "" {
		t.Fatalf("ResolveEndpoint after aggregate Ready: response=%+v error=%v", resolved, err)
	}

	if _, err := client.DeleteSandbox(requestCtx, &fastpathv2.DeleteRequest{Sandbox: sandboxReference(namespace, name, created.GetSandbox().GetIdentity().GetUid())}); err != nil {
		t.Fatalf("FastPath DeleteSandbox: %v", err)
	}
	deleteCtx, cancelDelete := context.WithTimeout(ctx, 90*time.Second)
	defer cancelDelete()
	if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Namespace: namespace, Name: name}); err != nil {
		t.Fatalf("wait for FastPath-created Sandbox deletion: %v", err)
	}
}

func assertLiveReadyInfo(t *testing.T, info *fastpathv2.SandboxInfo, generation int64, handlers ...string) {
	t.Helper()
	if info == nil || !info.GetReady() || info.GetAppliedGeneration() != generation {
		t.Fatalf("live SandboxInfo is not current and Ready: %+v", info)
	}
	if info.GetRuntime().GetState() != fastpathv2.RuntimeState_RUNTIME_STATE_READY ||
		info.GetDataPlane().GetState() != fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY {
		t.Fatalf("live runtime/data-plane state is not Ready: runtime=%s dataPlane=%s", info.GetRuntime().GetState(), info.GetDataPlane().GetState())
	}
	if len(info.GetActionBindings()) != len(handlers) {
		t.Fatalf("live Action Binding count=%d, want %d", len(info.GetActionBindings()), len(handlers))
	}
	for index, handler := range handlers {
		binding := info.GetActionBindings()[index]
		if binding.GetHandler() != handler || binding.GetState() != fastpathv2.ActionState_ACTION_STATE_READY {
			t.Fatalf("live Action Binding %d=%s/%s, want %s/Ready", index, binding.GetHandler(), binding.GetState(), handler)
		}
	}
	if info.GetIdentity().GetName() == "" || info.GetIdentity().GetUid() == "" {
		t.Fatal(fmt.Sprintf("live Sandbox identity is incomplete: %+v", info.GetIdentity()))
	}
}

func sandboxReference(namespace, name, expectedUID string) *fastpathv2.SandboxReference {
	return &fastpathv2.SandboxReference{
		NamespacedName: &fastpathv2.NamespacedName{Namespace: namespace, Name: name},
		ExpectedUid:    expectedUID,
	}
}

func binding(handler, input string) apiv1alpha2.ActionBinding {
	return apiv1alpha2.ActionBinding{Handler: handler, Input: input}
}

func actionPool(namespace string) *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "actions-pool", Namespace: namespace}, Spec: apiv1alpha2.SandboxPoolSpec{
		Capacity: apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1}, MaxSandboxesPerPod: 2,
		Runtime: apiv1alpha2.RuntimeContainer, SandboxResources: suiteenv.SmallSandboxResourceProfile(),
		ActionHandlers: []apiv1alpha2.ActionHandler{
			{Name: "audit", TargetHTTPPort: 18081, Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookRuntimeReady, apiv1alpha2.LifecycleHookDataPlaneReady}},
			{Name: "egress", TargetHTTPPort: 18080, Hooks: []apiv1alpha2.LifecycleHook{apiv1alpha2.LifecycleHookRuntimeReady, apiv1alpha2.LifecycleHookDataPlaneReady}},
		},
		FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "fastlet", Image: suiteenv.FastletImage()},
			{Name: "sandbox-action-fixture", Image: suiteenv.ActionFixtureImage(),
				Env: []corev1.EnvVar{
					{Name: "SANDBOX_ACTION_FIXTURE_ADDRESSES", Value: "127.0.0.1:18080,127.0.0.1:18081"},
					{Name: "SANDBOX_ACTION_FIXTURE_REMOVE_DELAY_PORTS", Value: "18081"},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
				}},
		}}},
	}}
}

func waitActions(t *testing.T, ctx context.Context, fixture *fixtures.FixtureClient, key types.NamespacedName, handlers ...string) *apiv1alpha2.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	sandbox, err := fixture.WaitForSandbox(waitCtx, key, func(value *apiv1alpha2.Sandbox) bool {
		if value.Status.ObservedGeneration != value.Generation || len(value.Status.ActionBindings) != len(handlers) {
			return false
		}
		for index, handler := range handlers {
			if value.Status.ActionBindings[index].Handler != handler || value.Status.ActionBindings[index].State != apiv1alpha2.ActionReady {
				return false
			}
		}
		return value.Status.Runtime.State == apiv1alpha2.RuntimeReady && value.Status.DataPlane.State == apiv1alpha2.DataPlaneReady
	})
	if err != nil {
		t.Fatalf("wait for Action Bindings Ready: %v", err)
	}
	return sandbox
}

func restartPodContainerAndWait(t *testing.T, ctx context.Context, k8sClient ctrlclient.Client, namespace, podName, containerName string) {
	t.Helper()
	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: namespace, Name: podName}
	if err := k8sClient.Get(ctx, key, pod); err != nil {
		t.Fatalf("get Fastlet Pod before restart: %v", err)
	}
	podUID := string(pod.UID)
	previous := int32(-1)
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == containerName {
			previous = container.RestartCount
		}
	}
	if previous < 0 {
		t.Fatalf("container %s status not found", containerName)
	}
	command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", testSuite.Config().KubeconfigFile(),
		"exec", "-n", namespace, podName, "-c", containerName, "--", "kill", "1")
	_, _ = command.CombinedOutput() // the connection commonly closes with PID 1

	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		current := &corev1.Pod{}
		if err := k8sClient.Get(waitCtx, key, current); err == nil && string(current.UID) == podUID {
			for _, container := range current.Status.ContainerStatuses {
				if container.Name == containerName && container.RestartCount > previous && container.Ready {
					return
				}
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for container %s restart: %v", containerName, waitCtx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func assertHandlerLogSequence(t *testing.T, ctx context.Context, namespace, poolName, sandboxUID string, sequence []string) string {
	t.Helper()
	config, err := conf.New(testSuite.Config().KubeconfigFile())
	if err != nil {
		t.Fatalf("load Kubernetes config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create Kubernetes clientset: %v", err)
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "fast-sandbox.io/pool=" + poolName})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("list Fastlet Pods: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	lastLogs := ""
	for time.Now().Before(deadline) {
		body, readErr := clientset.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: "sandbox-action-fixture"}).DoRaw(ctx)
		if readErr == nil {
			lastLogs = string(body)
			if containsInOrder(lastLogs, sandboxUID, sequence) {
				return lastLogs
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Action Handler log did not contain the expected ordered lifecycle; expected=%q logs=%s", sequence, lastLogs)
	return ""
}

func assertRemoveDelayDuration(t *testing.T, logs, sandboxUID string, minimum, maximum time.Duration) {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "sandboxUid="+sandboxUID) || !strings.Contains(line, "injectedRemoveDelayComplete=true") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "elapsedMillis=") {
				continue
			}
			milliseconds, err := strconv.ParseInt(strings.TrimPrefix(field, "elapsedMillis="), 10, 64)
			if err != nil {
				t.Fatalf("parse injected RemoveBinding delay from %q: %v", line, err)
			}
			elapsed := time.Duration(milliseconds) * time.Millisecond
			if elapsed < minimum || elapsed > maximum {
				t.Fatalf("injected RemoveBinding was cancelled after %s, want %s..%s", elapsed, minimum, maximum)
			}
			return
		}
	}
	t.Fatalf("Action Handler log has no completed injected RemoveBinding delay for Sandbox %s", sandboxUID)
}

func containsInOrder(logs, sandboxUID string, sequence []string) bool {
	position := 0
	for _, line := range strings.Split(logs, "\n") {
		if position < len(sequence) && strings.Contains(line, "sandboxUid="+sandboxUID) && containsAll(line, sequence[position]) {
			position++
		}
	}
	return position == len(sequence)
}

func containsAll(line, pattern string) bool {
	for _, fragment := range strings.Split(pattern, "&&") {
		if !strings.Contains(line, fragment) {
			return false
		}
	}
	return true
}
