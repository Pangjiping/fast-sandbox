package ocitemplate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func TestDecodeRequiresImmutablePair(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, test := range []struct {
		name, root, snap string
		valid            bool
	}{
		{"digests", "r.example/ns/t@" + digest, "r.example/ns/t@" + digest, true},
		{"mutable root", "r.example/ns/t:rootfs", "r.example/ns/t@" + digest, false},
		{"missing snapfiles", "r.example/ns/t@" + digest, "", false},
		{"invalid digest", "r.example/ns/t@sha256:bad", "r.example/ns/t@" + digest, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"oci": Pair{test.root, test.snap}})
			require.NoError(t, err)
			pair, err := Decode(raw)
			if test.valid {
				require.NoError(t, err)
				require.NotNil(t, pair)
			} else {
				require.Error(t, err)
			}
		})
	}
	pair, err := Decode([]byte(`{"files":{}}`))
	require.NoError(t, err)
	require.Nil(t, pair)
}

// Full registry round trip checks commit ordering, immutable refs and that
// resolving a template never fetches its rootfs or snapshot layer blobs.
func TestPublishResolveMetadataOnly(t *testing.T) {
	var blockReads atomic.Int32
	handler := registrytest.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/blobs/") && r.URL.Path != "" {
			// Layer digest is distinct from all JSON config blobs.
			if strings.HasSuffix(r.URL.Path, digest.FromString("layer").String()) {
				blockReads.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	old := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = old }()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifest"), 0750))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "blobs/sha256"), 0750))
	config := content.NewDescriptorFromBytes(oci.MediaTypeImageConfig, []byte(`{}`))
	layer := content.NewDescriptorFromBytes(oci.MediaTypeImageLayer, []byte("layer"))
	for _, blob := range []struct {
		desc oci.Descriptor
		data []byte
	}{{config, []byte(`{}`)}, {layer, []byte("layer")}} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blobs/sha256", blob.desc.Digest.Encoded()), blob.data, 0640))
	}
	manifest := oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: config, Layers: []oci.Descriptor{layer}}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest/pair.json"), raw, 0640))
	index := map[string]any{"images": []map[string]string{{"name": "template:x_rootfs", "manifest_file": "pair.json", "manifest_digest": digest.FromBytes(raw).String()}, {"name": "template:x_snapfiles", "manifest_file": "pair.json", "manifest_digest": digest.FromBytes(raw).String()}}}
	raw, err = json.Marshal(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest/index.json"), raw, 0640))
	dest := strings.TrimPrefix(server.URL, "https://") + "/ns/template:build"
	ref, sum, err := Push(context.Background(), dir, dest, []byte(`{"machine":{"vcpu":"1","memory":"512Mi"}}`), nil)
	require.NoError(t, err)
	require.Contains(t, ref, "@sha256:"+sum)
	payload, err := Resolve(context.Background(), ref, nil)
	require.NoError(t, err)
	pair, err := Decode(payload)
	require.NoError(t, err)
	require.Contains(t, pair.Rootfs, "@sha256:")
	require.Zero(t, blockReads.Load(), "metadata resolution must not download data layers")
	// An incremental export carries its parent descriptor without a local
	// layer file. Copy it into another repository before publishing the pair.
	sources, err := json.Marshal(map[string]string{layer.Digest.String(): strings.Split(dest, ":build")[0]})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, RemoteSourcesName), sources, 0600))
	require.NoError(t, os.Remove(filepath.Join(dir, "blobs/sha256", layer.Digest.Encoded())))
	checkpoint := strings.TrimPrefix(server.URL, "https://") + "/snapshots/checkpoint:delta"
	checkpointRef, _, err := Push(context.Background(), dir, checkpoint, []byte(`{"machine":{"vcpu":"1"}}`), nil)
	require.NoError(t, err)
	_, err = Resolve(context.Background(), checkpointRef, nil)
	require.NoError(t, err)
	copied, _, err := Repository(checkpointRef, nil)
	require.NoError(t, err)
	present, err := copied.Blobs().Exists(context.Background(), layer)
	require.NoError(t, err)
	require.True(t, present)
	// A parent blob missing in both repositories cannot be inherited.
	missing := content.NewDescriptorFromBytes(oci.MediaTypeImageLayer, []byte("missing parent"))
	err = inheritBlob(context.Background(), copied, missing, strings.TrimPrefix(server.URL, "https://")+"/absent/base", nil)
	require.Error(t, err)
	// A corrupted local blob must not publish a new template commit point.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blobs/sha256", layer.Digest.Encoded()), []byte("broken"), 0640))
	_, _, err = Push(context.Background(), dir, strings.TrimSuffix(dest, "build")+"broken", []byte(`{}`), nil)
	require.Error(t, err)
	repo, parsed, err := Repository(strings.TrimSuffix(dest, "build")+"broken", nil)
	require.NoError(t, err)
	_, err = repo.Resolve(context.Background(), parsed.Reference)
	require.Error(t, err)
}

// The pair is constrained to the metadata repository, so its descriptor checks
// must share the metadata client's bearer token while still checking both refs.
func TestResolveSharesRepositoryAuthAndValidatesPair(t *testing.T) {
	for _, mode := range []string{"valid", "missing-rootfs", "missing-snapfiles", "cross-repository", "cross-host"} {
		t.Run(mode, func(t *testing.T) {
			var tokens atomic.Int32
			documents := map[string][]byte{}
			heads := map[string]int{}
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"token":"test-token","expires_in":300}`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-token" {
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="repository:team/template:pull"`, server.URL))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				raw, ok := documents[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", oci.MediaTypeImageManifest)
				w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
				w.Header().Set("Docker-Content-Digest", digest.FromBytes(raw).String())
				if r.Method == http.MethodHead {
					heads[r.URL.Path]++
					return
				}
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			old := retry.DefaultClient
			retry.DefaultClient = server.Client()
			defer func() { retry.DefaultClient = old }()
			repo := strings.TrimPrefix(server.URL, "https://") + "/team/template"
			root, snap := []byte(`{"schemaVersion":2,"annotations":{"kind":"rootfs"}}`), []byte(`{"schemaVersion":2,"annotations":{"kind":"snapfiles"}}`)
			rootSum, snapSum := digest.FromBytes(root).String(), digest.FromBytes(snap).String()
			pair := Pair{Rootfs: repo + "@" + rootSum, Snapfiles: repo + "@" + snapSum}
			switch mode {
			case "cross-repository":
				pair.Rootfs = strings.Replace(pair.Rootfs, "/team/template", "/other/template", 1)
			case "cross-host":
				pair.Rootfs = "other.example/team/template@" + rootSum
			}
			cfg, err := json.Marshal(map[string]any{"oci": pair})
			require.NoError(t, err)
			config := content.NewDescriptorFromBytes(ConfigMediaType, cfg)
			top, err := json.Marshal(oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: config})
			require.NoError(t, err)
			topSum := digest.FromBytes(top).String()
			documents["/v2/team/template/manifests/"+topSum] = top
			documents["/v2/team/template/blobs/"+config.Digest.String()] = cfg
			rootPath, snapPath := "/v2/team/template/manifests/"+rootSum, "/v2/team/template/manifests/"+snapSum
			if mode != "missing-rootfs" {
				documents[rootPath] = root
			}
			if mode != "missing-snapfiles" {
				documents[snapPath] = snap
			}
			got, err := Resolve(context.Background(), repo+"@"+topSum, nil)
			if mode == "valid" {
				require.NoError(t, err)
				require.JSONEq(t, string(cfg), string(got))
				require.EqualValues(t, 1, tokens.Load(), "metadata and both pair checks must share one token cache")
				require.Equal(t, 1, heads[rootPath])
				require.Equal(t, 1, heads[snapPath])
			} else {
				require.Error(t, err)
			}
		})
	}
}
