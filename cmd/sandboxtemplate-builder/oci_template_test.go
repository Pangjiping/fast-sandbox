package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/guestnetwork"
)

func TestOCIBuildUsesRemoteSourceWithoutDownloadingSourceLayers(t *testing.T) {
	for _, test := range []struct {
		private bool
		kernel  string
		version string
	}{
		{false, "/kernels/vmlinux-6.1.0", "6.1.0"},
		{true, "/kernels/vmlinux-6.1.0", "6.1.0"},
		{false, "/kernels/vmlinux.bin", "vmlinux.bin"},
		{true, "/kernels/vmlinux.bin", "vmlinux.bin"},
	} {
		t.Run(map[bool]string{false: "anonymous", true: "basic-auth"}[test.private]+"/"+filepath.Base(test.kernel), func(t *testing.T) {
			private := test.private
			dir := t.TempDir()
			t.Setenv("DOCKER_CONFIG", dir)
			t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", "")
			amd64 := ociTestImage(t, "amd64")
			arm64 := ociTestImage(t, "arm64")
			index := mutate.AppendManifests(empty.Index,
				mutate.IndexAddendum{Add: arm64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
				mutate.IndexAddendum{Add: amd64, Descriptor: v1.Descriptor{Platform: &linuxAMD64}},
			)
			layers, err := amd64.Layers()
			require.NoError(t, err)
			layerDigest, err := layers[0].Digest()
			require.NoError(t, err)
			var sourceLayerReads atomic.Int32
			handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if private {
					user, password, ok := r.BasicAuth()
					if !ok || user != "reader" || password != "secret" {
						w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v2/base/blobs/"+layerDigest.String() {
					sourceLayerReads.Add(1)
					http.Error(w, "builder must not download source layers", http.StatusInternalServerError)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			tag, err := name.NewTag(host + "/base:mutable")
			require.NoError(t, err)
			credential := authn.Anonymous
			config := map[string]any{"auths": map[string]any{}}
			if private {
				credential = authn.FromConfig(authn.AuthConfig{Username: "reader", Password: "secret"})
				config["auths"] = map[string]any{host: authn.AuthConfig{Username: "reader", Password: "secret"}}
			}
			require.NoError(t, remote.WriteIndex(tag, index, remote.WithAuth(credential)))
			execdTag, err := name.NewTag(host + "/execd:build")
			require.NoError(t, err)
			require.NoError(t, remote.Write(execdTag, ociTestExecd(t), remote.WithAuth(credential)))
			raw, err := json.Marshal(config)
			require.NoError(t, err)
			t.Setenv(publishCredentialsDirEnv, dir)
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerconfigjson"), raw, 0600))

			bin := t.TempDir()
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_OCI_COMMANDS", dir)
			require.NoError(t, os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_OCI_COMMANDS/ip.log\"\n"), 0755))
			require.NoError(t, os.WriteFile(filepath.Join(bin, "fsb-vm"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_OCI_COMMANDS/fsb-args\"\nexit 9\n"), 0755))
			spec := apiv1alpha2.SandboxTemplateSpec{
				Image: tag.String(), Execd: execdTag.String(),
				Machine: apiv1alpha2.MachineSpec{VCPU: "1", Memory: "512Mi"},
				Envs:    []corev1.EnvVar{{Name: "OVERRIDE", Value: "from-spec"}},
				Output:  apiv1alpha2.OutputSpec{Format: apiv1alpha2.ArtifactFormatOverlayBD, RootfsSize: "1Gi", Publish: "oci://" + host + "/template:build"},
			}
			spec.Readiness.WarmupSeconds = 600
			err = buildOCITemplate(context.Background(), spec, dir, test.kernel)
			require.ErrorContains(t, err, "fsb-vm build: exit status 9")
			raw, err = os.ReadFile(filepath.Join(dir, "fsb-args"))
			require.NoError(t, err)
			args := strings.Split(strings.TrimSpace(string(raw)), "\n")
			value := func(flag string) string {
				for i, arg := range args {
					if arg == flag && i+1 < len(args) {
						return args[i+1]
					}
				}
				t.Fatalf("missing flag %s in %q", flag, args)
				return ""
			}
			sum, err := amd64.Digest()
			require.NoError(t, err)
			require.Equal(t, tag.Context().Digest(sum.String()).Name(), value("--source"))
			require.NotContains(t, args, "--plain-http")
			require.Equal(t, filepath.Join(dir, ".dockerconfigjson"), value("--auth-file"))
			require.Equal(t, test.version, value("--kernel-version"))
			require.Equal(t, "11m0s", value("--ready-timeout"))
			require.Equal(t, buildTap, value("--tap-name"))
			require.Zero(t, sourceLayerReads.Load())
			require.NoDirExists(t, filepath.Join(dir, "oci-layout"))
			raw, err = os.ReadFile(filepath.Join(value("--inject-dir"), "etc/sandbox-init.env"))
			require.NoError(t, err)
			require.Contains(t, string(raw), "export SOURCE_ARCH='amd64'")
			require.Contains(t, string(raw), "export OVERRIDE='from-spec'")
			for _, asset := range []string{execdAssetName, bootstrapScriptName, prepareScriptName, "bwrap"} {
				raw, err = os.ReadFile(filepath.Join(value("--inject-dir"), "opt/opensandbox", asset))
				require.NoError(t, err)
				require.Equal(t, "test-runtime", string(raw))
			}
			raw, err = os.ReadFile(filepath.Join(dir, "ip.log"))
			require.NoError(t, err)
			commands := strings.Split(strings.TrimSpace(string(raw)), "\n")
			require.Equal(t, "link set dev "+buildTap+" address "+guestnetwork.GatewayMAC, commands[1])
			require.Equal(t, "link set dev "+buildTap+" up", commands[2])
			require.Equal(t, "link del "+buildTap, commands[3])
		})
	}
}

func ociTestImage(t *testing.T, architecture string) v1.Image {
	t.Helper()
	image, err := random.Image(1024, 1)
	require.NoError(t, err)
	config, err := image.ConfigFile()
	require.NoError(t, err)
	config.OS, config.Architecture = "linux", architecture
	config.Config.Env = []string{"SOURCE_ARCH=" + architecture, "OVERRIDE=from-image"}
	image, err = mutate.ConfigFile(image, config)
	require.NoError(t, err)
	return image
}

func ociTestExecd(t *testing.T) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, file := range []string{execdAssetName, bootstrapScriptName, prepareScriptName, "usr/local/bin/bwrap"} {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: file, Mode: 0755, Size: int64(len("test-runtime"))}))
		_, err := writer.Write([]byte("test-runtime"))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	layer, err := tarball.LayerFromReader(&buf)
	require.NoError(t, err)
	image, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	return image
}

func TestOCIBuilderRejectsLocalTarSource(t *testing.T) {
	t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", "/unused/source.tar")
	spec := apiv1alpha2.SandboxTemplateSpec{Output: apiv1alpha2.OutputSpec{Format: apiv1alpha2.ArtifactFormatOverlayBD}}
	require.ErrorContains(t, buildOCITemplate(context.Background(), spec, t.TempDir(), ""), "supported only for object-store output")
}

func TestObjectStoreSourceStillStagesLocalTar(t *testing.T) {
	image := ociTestImage(t, "amd64")
	tag, err := name.NewTag("unavailable.invalid/base:build")
	require.NoError(t, err)
	dir := t.TempDir()
	archive := filepath.Join(dir, "source.tar")
	require.NoError(t, tarball.WriteToFile(archive, tag, image))
	t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", archive)
	sum, err := pullOCILayout(context.Background(), apiv1alpha2.SandboxTemplateSpec{Image: tag.String()}, dir)
	require.NoError(t, err)
	want, err := image.Digest()
	require.NoError(t, err)
	require.Equal(t, want.String(), sum)
	require.FileExists(t, filepath.Join(dir, "oci-layout/index.json"))
	raw, err := os.ReadFile(filepath.Join(dir, imageEnvFileName))
	require.NoError(t, err)
	require.Contains(t, string(raw), "SOURCE_ARCH=amd64")
}
