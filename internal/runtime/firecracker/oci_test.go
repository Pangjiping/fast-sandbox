package firecracker

import (
	"context"
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

func TestEnsureSandboxRejectsInvalidOCICacheBeforeAcquiringSlot(t *testing.T) {
	for _, payload := range []string{`{broken`, `{}`, `{"oci":{"rootfs":"r.example/ns/t:mutable"}}`} {
		t.Run(payload, func(t *testing.T) {
			fixture := newDriverFixture(t)
			require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
			fixture.sandboxSpec.Spec.Image = "oci://r.example/ns/t@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			file := cachedManifestPath(fixture.driver.config.StateRoot, fixture.sandboxSpec.Spec.Image)
			require.NoError(t, os.MkdirAll(filepath.Dir(file), 0750))
			require.NoError(t, os.WriteFile(file, []byte(payload), 0600))
			_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.ErrorIs(t, err, ErrImageNotReady)
			require.Empty(t, fixture.launcher.started)
			require.Equal(t, 0, fixture.manager.Snapshot().Bound)
		})
	}
}
