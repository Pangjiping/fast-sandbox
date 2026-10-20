package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestDisabledOCIRejectsCreateResumeAndWarmPull(t *testing.T) {
	t.Setenv("FAST_SANDBOX_OCI_DISABLED", "true")
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const image = "oci://r.example/ns/template@sha256:" + digest
	for _, cached := range []bool{false, true} {
		for _, resume := range []bool{false, true} {
			fixture := newDriverFixture(t)
			fixture.sandboxSpec.Spec.Image = image
			restore := &fastletapi.RestoreSpec{ManifestRef: image, ArtifactDigest: digest}
			if resume {
				fixture.sandboxSpec.Spec.Image = "native:template"
				fixture.sandboxSpec.Spec.Restore = restore
			}
			if cached {
				file := cachedManifestPath(fixture.stateRoot, image)
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0750))
				require.NoError(t, os.WriteFile(file, []byte(`{"oci":{"rootfs":"r.example/ns/t@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","snapfiles":"r.example/ns/t@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`), 0600))
				require.NoError(t, verifyRestorableImage(fixture.stateRoot, image))
			}
			_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.ErrorContains(t, err, "FAST_SANDBOX_OCI_DISABLED=true")
			if resume {
				_, err = fixture.driver.DeliverCheckpoint(context.Background(), restore)
			} else {
				_, err = fixture.driver.DeliverImage(context.Background(), image)
			}
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.ErrorContains(t, err, "FAST_SANDBOX_OCI_DISABLED=true")
			err = fixture.driver.PullImage(context.Background(), image)
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.ErrorContains(t, err, "FAST_SANDBOX_OCI_DISABLED=true")
			require.Empty(t, fixture.driver.imageDeliveries)
			require.Empty(t, fixture.launcher.started)
			require.Zero(t, fixture.manager.Snapshot().Bound)
		}
	}
}

func TestOCISupportDefaultsAndNativeOptOut(t *testing.T) {
	for _, disabled := range []string{"", "false", "true"} {
		t.Run(disabled, func(t *testing.T) {
			t.Setenv("FAST_SANDBOX_OCI_DISABLED", disabled)
			err := validateOCISupport("oci://r.example/ns/template:build")
			if disabled == "true" {
				require.ErrorIs(t, err, ErrInvalidConfig)
			} else {
				require.NoError(t, err)
			}
			fixture := newDriverFixture(t)
			fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
			status, err := fixture.driver.DeliverImage(context.Background(), fixture.sandboxSpec.Spec.Image)
			require.NoError(t, err)
			require.Equal(t, runtimecontract.ImageDelivered, status)
			require.NoError(t, fixture.driver.PullImage(context.Background(), fixture.sandboxSpec.Spec.Image))
			require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
			_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.NoError(t, err)
			require.Len(t, fixture.launcher.started, 1)
		})
	}
}

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
