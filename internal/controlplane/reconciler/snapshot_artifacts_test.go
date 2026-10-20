package reconciler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"oras.land/oras-go/v2/registry/remote/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
)

func TestOCISnapshotDeletionRetriesAndWaitsForPublication(t *testing.T) {
	for _, sourceGone := range []bool{true, false} {
		t.Run(strconv.FormatBool(sourceGone), func(t *testing.T) {
			testOCISnapshotDeletion(t, sourceGone)
		})
	}
}

func testOCISnapshotDeletion(t *testing.T, sourceGone bool) {
	harness, _ := newSnapshotReconcilerHarness(t)
	root := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"io.fast-sandbox.template.id":"snapshot-id"}}`)
	snap := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"io.fast-sandbox.template.id":"snapshot-id"},"layers":[]}`)
	rootSum, snapSum := digest.FromBytes(root).String(), digest.FromBytes(snap).String()
	var mu sync.Mutex
	blocked := true
	objects := map[string][]byte{rootSum: root, snapSum: snap}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		user, password, _ := r.BasicAuth()
		if user != "writer" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v2/snapshots/test/manifests/")
		if name == "snapshot-id_rootfs" {
			name = rootSum
		}
		body, ok := objects[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			if blocked {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			delete(objects, name)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", oci.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	previous := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = previous }()
	host := strings.TrimPrefix(server.URL, "https://")
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{Host: host, RepositoryPrefix: "snapshots/test", Username: "reader", Password: "read-secret", WriteUsername: "writer", WritePassword: "secret"}})
	require.NoError(t, err)
	payload, err := json.Marshal(compiled)
	require.NoError(t, err)
	require.NoError(t, harness.k8sClient.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: registrySecretName("pool-a")},
		Data:       map[string][]byte{registryconfig.SecretKey: payload},
	}))
	snapshot := getSnapshot(t, harness, "snap-a")
	snapshot.Finalizers = []string{SandboxSnapshotFinalizerName}
	snapshot.Annotations = map[string]string{assignment.AnnotationSnapshotPoolRef: "pool-a"}
	require.NoError(t, harness.k8sClient.Update(t.Context(), snapshot))
	snapshot.Status = apiv1alpha2.SandboxSnapshotStatus{Phase: apiv1alpha2.SandboxSnapshotPhaseSucceeded, SnapshotID: "snapshot-id", ManifestRef: ocitemplate.Prefix + host + "/snapshots/test@" + snapSum, ArtifactDigest: strings.TrimPrefix(snapSum, "sha256:")}
	require.NoError(t, harness.k8sClient.Status().Update(t.Context(), snapshot))
	if sourceGone {
		require.NoError(t, harness.k8sClient.Delete(t.Context(), snapshotTargetSandbox(t)))
	} else {
		harness.fastlet.snapshotInspectStatus = &fastletapi.SnapshotStatus{SnapshotID: snapshot.Status.SnapshotID, Phase: fastletapi.SnapshotPhaseSucceeded, ManifestRef: snapshot.Status.ManifestRef, ArtifactDigest: snapshot.Status.ArtifactDigest}
		snapshot.Status.Phase = apiv1alpha2.SandboxSnapshotPhasePublishing
		snapshot.Status.ManifestRef, snapshot.Status.ArtifactDigest = "", ""
		require.NoError(t, harness.k8sClient.Status().Update(t.Context(), snapshot))
	}
	require.NoError(t, harness.k8sClient.Delete(t.Context(), snapshot))
	result, err := harness.reconciler.Reconcile(t.Context(), snapshotRequestFor("snap-a"))
	require.Error(t, err, "registry failure must not turn into a successful deletion")
	require.Equal(t, SnapshotRetryInterval, result.RequeueAfter)
	require.Contains(t, getSnapshot(t, harness, "snap-a").Finalizers, SandboxSnapshotFinalizerName)
	mu.Lock()
	blocked = false
	mu.Unlock()
	_, err = harness.reconciler.Reconcile(t.Context(), snapshotRequestFor("snap-a"))
	require.NoError(t, err)
	var deleted apiv1alpha2.SandboxSnapshot
	require.True(t, apierrors.IsNotFound(harness.k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "snap-a"}, &deleted)))
	mu.Lock()
	require.Empty(t, objects, "both remote manifests must disappear before the CR")
	mu.Unlock()
}
