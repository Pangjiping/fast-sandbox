package agent

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

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote/retry"

	"fast-sandbox/internal/ocitemplate"
	"fast-sandbox/internal/registryconfig"
)

func TestOCICacheCommitsMetadataWithoutNativeArtifacts(t *testing.T) {
	root := t.TempDir()
	image := "oci://registry.example/ns/template@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir := imageDir(root, image)
	require.NoError(t, os.MkdirAll(dir, 0750))
	payload := []byte(`{"oci":{"rootfs":"registry.example/ns/template@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","snapfiles":"registry.example/ns/template@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`)
	require.NoError(t, commitManifest(dir, payload))
	complete, err := cacheComplete(dir)
	require.NoError(t, err)
	require.True(t, complete)
	// A committed OCI cache hit must not need any provider or network access.
	require.NoError(t, PullOCIImage(context.Background(), root, image, nil))
	for _, name := range []string{"rootfs.img", "vmstate.snap", "memory.snap"} {
		_, err = os.Stat(filepath.Join(dir, name))
		require.True(t, os.IsNotExist(err))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"oci":{"rootfs":"registry.example/ns/template:mutable","snapfiles":"registry.example/ns/template:mutable"}}`), 0600))
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)
}

func TestReadOCIUsesLiveRepositoryCredentials(t *testing.T) {
	documents := map[string][]byte{}
	var password atomic.Value
	password.Store("original")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		if !ok || user != "reader" || secret != password.Load().(string) {
			w.Header().Set("WWW-Authenticate", `Basic realm="node-registry"`)
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
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
	}))
	defer server.Close()
	old := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = old }()
	host := strings.TrimPrefix(server.URL, "https://")
	root := []byte(`{"schemaVersion":2,"layers":[]}`)
	rootSum := digest.FromBytes(root).String()
	cfg, err := json.Marshal(map[string]any{"oci": ocitemplate.Pair{Rootfs: host + "/team/t@" + rootSum, Snapfiles: host + "/team/t@" + rootSum}})
	require.NoError(t, err)
	config := content.NewDescriptorFromBytes(ocitemplate.ConfigMediaType, cfg)
	top, err := json.Marshal(oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: config})
	require.NoError(t, err)
	topSum := digest.FromBytes(top).String()
	documents["/v2/team/t/manifests/"+topSum] = top
	documents["/v2/team/t/manifests/"+rootSum] = root
	documents["/v2/team/t/blobs/"+config.Digest.String()] = cfg
	file := filepath.Join(t.TempDir(), "registries.json")
	provider := registryconfig.NewFileProvider(file)
	image := "oci://" + host + "/team/t@" + topSum
	for _, secret := range []string{"original", "rotated"} {
		password.Store(secret)
		compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{Host: host, Username: "wrong", Password: "wrong"}, {Host: host, RepositoryPrefix: "team", Username: "reader", Password: secret}})
		require.NoError(t, err)
		raw, err := compiled.Marshal()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, raw, 0600))
		got, err := readOCI(context.Background(), image, provider)
		require.NoError(t, err)
		require.JSONEq(t, string(cfg), string(got))
	}
	require.NoError(t, os.WriteFile(file, []byte(`{"revision":"invalid"}`), 0600))
	_, err = readOCI(context.Background(), image, provider)
	require.Error(t, err, "invalid live credentials must not reuse a prior successful read")
	require.NoError(t, os.Remove(file))
	_, err = readOCI(context.Background(), image, provider)
	require.Error(t, err, "removed credentials must fail closed")
}

func TestReadOCIRejectsTokenOnlyBeforeRegistryAccess(t *testing.T) {
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{Host: "registry.invalid", IdentityToken: "private-token"}})
	require.NoError(t, err)
	raw, err := compiled.Marshal()
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "registries.json")
	require.NoError(t, os.WriteFile(file, raw, 0600))
	_, err = readOCI(context.Background(), "oci://registry.invalid/team/template:build", registryconfig.NewFileProvider(file))
	require.ErrorContains(t, err, "identitytoken-only credentials are not supported")
}
