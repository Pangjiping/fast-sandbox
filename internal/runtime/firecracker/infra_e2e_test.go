package firecracker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletinfra "fast-sandbox/internal/fastlet/infra"

	"github.com/stretchr/testify/require"
)

// newE2EInfraManager builds a real Infra Manager with one archive component
// ("execd") served from a local tar.gz, so EnsureSandbox performs a genuine
// GuestCopy delivery into the instance rootfs.
func newE2EInfraManager(t *testing.T, profile runtimecatalog.RuntimeProfile, sandboxInitPath string) *fastletinfra.Manager {
	t.Helper()
	payload := filepath.Join(t.TempDir(), "execd")
	require.NoError(t, os.WriteFile(payload, []byte("#!/bin/sh\necho fake-execd\n"), 0o755))
	archive := createE2EComponentArchive(t, payload, "execd")
	digest := sha256.Sum256(mustReadFile(t, archive))

	component := infracatalog.Component{
		Name: "execd",
		Artifact: infracatalog.Artifact{
			Source: infracatalog.ArtifactSource{
				Type: infracatalog.SourceArchive, Reference: "https://e2e.local/component.tar.gz",
				Digest: "sha256:" + hex.EncodeToString(digest[:]),
			},
			Mappings: []infracatalog.ArtifactMapping{{SourcePath: "/execd", TargetPath: "/.fast/components/execd/execd"}},
		},
		Process: infracatalog.Process{
			Command: []string{"/.fast/components/execd/execd"}, RestartPolicy: infracatalog.RestartNever,
			Readiness: infracatalog.ReadinessProbe{Type: infracatalog.ProbeTCP, Timeout: 5 * time.Second},
		},
		Endpoint: infracatalog.Endpoint{Protocol: "HTTP", Port: 44772},
		Delivery: runtimecatalog.InfraDeliveryGuestCopy,
	}
	storeRoot := t.TempDir()
	store, err := fastletinfra.NewArtifactStore(storeRoot, storeRoot)
	require.NoError(t, err)
	manager, err := fastletinfra.NewManagerWithConfig(fastletinfra.ManagerConfig{
		Plan:           infracatalog.Plan{Components: []infracatalog.Component{component}},
		RuntimeProfile: profile, Store: store,
		Resolver:        e2eFileResolver{archivePath: archive},
		SandboxInitPath: sandboxInitPath,
	})
	require.NoError(t, err)
	require.NoError(t, manager.Prepare(context.Background()))
	return manager
}

// e2eFileResolver serves a local tar.gz for the archive component source,
// bypassing the HTTPS download used by the platform resolver.
type e2eFileResolver struct {
	archivePath string
}

func (r e2eFileResolver) Prepare(_ context.Context, source infracatalog.ArtifactSource, store *fastletinfra.ArtifactStore) (fastletinfra.PreparedSource, error) {
	return store.StageTree(context.Background(), source.Digest, true, true, func() (io.ReadCloser, error) {
		return os.Open(r.archivePath)
	})
}

func createE2EComponentArchive(t *testing.T, payload, payloadName string) string {
	t.Helper()
	archivePath := filepath.Join(t.TempDir(), "component.tar.gz")
	file, err := os.Create(archivePath)
	require.NoError(t, err)
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	body, err := os.ReadFile(payload)
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: payloadName, Mode: 0o755, Size: int64(len(body))}))
	_, err = tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, file.Close())
	return archivePath
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	require.NoError(t, err)
	return payload
}

// assertGuestFile verifies a file inside the instance rootfs image using
// debugfs (userspace ext4 reader, no mount conflicts with the running VM).
func assertGuestFile(t *testing.T, imagePath, guestPath, want string) {
	t.Helper()
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Logf("debugfs not installed; skipping guest file assertion for %s", guestPath)
		return
	}
	output, err := exec.Command("debugfs", "-R", "cat "+guestPath, imagePath).CombinedOutput()
	require.NoErrorf(t, err, "guest file %s missing from %s: %s", guestPath, imagePath, output)
	require.Contains(t, string(output), want)
}
