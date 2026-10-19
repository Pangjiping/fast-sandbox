package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/guestnetwork"
)

func TestStagedTarSourceIsReadableWithoutOriginalRegistry(t *testing.T) {
	image, err := random.Image(1024, 2)
	require.NoError(t, err)
	tag, err := name.NewTag("unavailable.invalid/team/image:mutable")
	require.NoError(t, err)
	dir := t.TempDir()
	tar := filepath.Join(dir, "source.tar")
	require.NoError(t, tarball.WriteToFile(tar, tag, image))
	t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", tar)
	sum, err := pullOCILayout(context.Background(), apiv1alpha2.SandboxTemplateSpec{Image: tag.String()}, dir)
	require.NoError(t, err)
	require.NoError(t, os.Remove(tar), "the staged layout must be self-contained")
	source, closeSource, err := serveOCILayout(dir)
	require.NoError(t, err)
	defer closeSource()
	require.True(t, strings.HasPrefix(source, "127.0.0.1:"))
	require.Contains(t, source, "@"+sum)
	reference, err := name.ParseReference(source, name.Insecure)
	require.NoError(t, err)
	got, err := remote.Image(reference, remote.WithAuth(authn.Anonymous))
	require.NoError(t, err)
	gotSum, err := got.Digest()
	require.NoError(t, err)
	require.Equal(t, sum, gotSum.String())
	layers, err := got.Layers()
	require.NoError(t, err)
	require.Len(t, layers, 2)
	for _, layer := range layers {
		reader, err := layer.Uncompressed()
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
	}
	base := "http://" + strings.Split(source, "/source@")[0]
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/v2/source/blobs/uploads/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/source/manifests/mutable", http.StatusNotFound},
		{http.MethodGet, "/v2/other/blobs/sha256:bad", http.StatusNotFound},
		{http.MethodGet, "/v2/source/blobs/sha256:bad", http.StatusNotFound},
		{http.MethodGet, "/v2/source/blobs/not-a-digest", http.StatusNotFound},
	} {
		req, err := http.NewRequest(test.method, base+test.path, nil)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, test.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
}

func TestStagedRegistryRejectsInvalidLayout(t *testing.T) {
	for _, raw := range []string{`{broken`, `{"manifests":[]}`, `{"manifests":[{}]}`, `{"manifests":[{"digest":"sha256:../escape"}]}`} {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "oci-layout"), 0750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "oci-layout/index.json"), []byte(raw), 0600))
		_, _, err := serveOCILayout(dir)
		require.Error(t, err)
	}
}

func TestOCIBuildUsesStagedSourceAndNativeBootContract(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	image, err := random.Image(1024, 1)
	require.NoError(t, err)
	tag, err := name.NewTag("unavailable.invalid/base:mutable")
	require.NoError(t, err)
	tar := filepath.Join(dir, "source.tar")
	require.NoError(t, tarball.WriteToFile(tar, tag, image))
	t.Setenv("SANDBOX_TEMPLATE_IMAGE_TAR", tar)
	t.Setenv(publishCredentialsDirEnv, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerconfigjson"), []byte(`{"auths":{}}`), 0600))
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_OCI_COMMANDS", dir)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_OCI_COMMANDS/ip.log\"\n"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "fsb-vm"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_OCI_COMMANDS/fsb-args\"\necho build-intercepted >&2\nexit 9\n"), 0755))
	spec := apiv1alpha2.SandboxTemplateSpec{Image: tag.String(), Machine: apiv1alpha2.MachineSpec{VCPU: "1", Memory: "512Mi"}, Output: apiv1alpha2.OutputSpec{Format: apiv1alpha2.ArtifactFormatOverlayBD, RootfsSize: "1Gi", Publish: "oci://r.example/template:build"}}
	spec.Readiness.WarmupSeconds = 600
	err = buildOCITemplate(context.Background(), spec, dir, "/kernels/vmlinux-6.1.0")
	require.ErrorContains(t, err, "fsb-vm build: exit status 9")
	raw, err := os.ReadFile(filepath.Join(dir, "fsb-args"))
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
	require.Contains(t, args, "--plain-http")
	require.True(t, strings.HasPrefix(value("--source"), "127.0.0.1:"))
	require.NotContains(t, value("--source"), "unavailable.invalid")
	require.Equal(t, "6.1.0", value("--kernel-version"))
	require.Equal(t, "11m0s", value("--ready-timeout"))
	require.Equal(t, buildTap, value("--tap-name"))
	raw, err = os.ReadFile(filepath.Join(dir, "ip.log"))
	require.NoError(t, err)
	commands := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.Equal(t, "link set dev "+buildTap+" address "+guestnetwork.GatewayMAC, commands[1])
	require.Equal(t, "link set dev "+buildTap+" up", commands[2])
	require.Equal(t, "link del "+buildTap, commands[3])
}
