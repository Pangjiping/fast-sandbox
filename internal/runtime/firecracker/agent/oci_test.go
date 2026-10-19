package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
