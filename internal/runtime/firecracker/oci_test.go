package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOCIReadyNeedsMetadataButNoNativeArtifacts(t *testing.T) {
	root := t.TempDir()
	image := "oci://r.example/ns/template@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.ErrorIs(t, verifyRestorableImage(root, image), ErrImageNotReady)
	file := cachedManifestPath(root, image)
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0750))
	require.NoError(t, os.WriteFile(file, []byte(`{"oci":{"rootfs":"r.example/ns/t@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","snapfiles":"r.example/ns/t@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`), 0640))
	require.NoError(t, verifyRestorableImage(root, image))
	_, err := os.Stat(filepath.Join(filepath.Dir(file), "rootfs.img"))
	require.True(t, os.IsNotExist(err))
}
