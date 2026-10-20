package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/retry"

	"fast-sandbox/internal/ocitemplate"
)

func TestPushPackedPairWithCheckpointAnnotations(t *testing.T) {
	layout, _, err := createStagedArtifactLayout(t.TempDir(), "checkpoint")
	require.NoError(t, err)
	parent, err := layout.installBytes([]byte("parent layer"), ocitemplate.OverlayBDLayerMediaType)
	require.NoError(t, err)
	delta, err := layout.installBytes([]byte("new delta layer"), ocitemplate.OverlayBDLayerMediaType)
	require.NoError(t, err)
	snap, err := layout.installBytes([]byte("snapshot layer"), ocitemplate.OverlayBDLayerMediaType)
	require.NoError(t, err)
	names, err := parseOutput("snapshot:checkpoint-1")
	require.NoError(t, err)
	require.NoError(t, writePackedPair(layout, names, []oci.Descriptor{parent, delta}, snap))
	server := httptest.NewTLSServer(registrytest.New())
	defer server.Close()
	previous := retry.DefaultClient
	retry.DefaultClient = server.Client()
	defer func() { retry.DefaultClient = previous }()
	metadata, auth := filepath.Join(t.TempDir(), "metadata.json"), filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(metadata, []byte(`{"checkpoint":{"sandboxUID":"sandbox-1","namespace":"tenant"},"machine":{"vcpu":"1"}}`), 0600))
	require.NoError(t, os.WriteFile(auth, []byte(`{"auths":{}}`), 0600))
	cmd := newPushCommand()
	cmd.SetArgs([]string{"--artifact-dir", layout.root, "--destination", strings.TrimPrefix(server.URL, "https://") + "/checkpoint/test:checkpoint-1", "--metadata-file", metadata, "--auth-file", auth})
	require.NoError(t, cmd.Execute())
	// Resolve through the default paired-tag convention, then inspect exactly
	// the snapfiles manifest returned as the checkpoint publication point.
	ref := strings.TrimPrefix(server.URL, "https://") + "/checkpoint/test:checkpoint-1"
	payload, err := ocitemplate.Resolve(context.Background(), ref, nil)
	require.NoError(t, err)
	pair, err := ocitemplate.Decode(payload)
	require.NoError(t, err)
	repo, parsed, err := ocitemplate.Repository(pair.Snapfiles, nil)
	require.NoError(t, err)
	descriptor, err := repo.Resolve(context.Background(), parsed.Reference)
	require.NoError(t, err)
	stream, err := repo.Manifests().Fetch(context.Background(), descriptor)
	require.NoError(t, err)
	defer stream.Close()
	var manifest oci.Manifest
	require.NoError(t, json.NewDecoder(stream).Decode(&manifest))
	require.Equal(t, "sandbox-1", manifest.Annotations[ocitemplate.AnnotationSandboxUID])
	require.Equal(t, "tenant", manifest.Annotations[ocitemplate.AnnotationNamespace])
	require.Equal(t, "checkpoint-1", manifest.Annotations[ocitemplate.AnnotationCheckpointID])
	require.Equal(t, []oci.Descriptor{snap}, manifest.Layers)
}
