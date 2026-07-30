package infra

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"testing"

	infracatalog "fast-sandbox/internal/catalog/infra"

	"github.com/stretchr/testify/require"
)

type fakeOCIOpener struct {
	opened  infracatalog.ArtifactSource
	payload []byte
}

func (o *fakeOCIOpener) OpenOCI(_ context.Context, source infracatalog.ArtifactSource) (io.ReadCloser, error) {
	o.opened = source
	return io.NopCloser(bytes.NewReader(o.payload)), nil
}

func TestPlatformResolverStagesOCIImageRootFSAndMappings(t *testing.T) {
	source := infracatalog.ArtifactSource{
		Type: infracatalog.SourceOCIImage, Reference: "registry.example/execd@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	opener := &fakeOCIOpener{payload: testTar(t, map[string]string{
		"execd":               "binary",
		"config/default.yaml": "enabled: true",
	})}
	resolver := NewPlatformResolverWithOptions(PlatformResolverOptions{OCI: opener})
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)

	prepared, err := resolver.Prepare(context.Background(), source, store)
	require.NoError(t, err)
	require.Equal(t, source, opener.opened)
	execd, err := prepared.Resolve("/execd")
	require.NoError(t, err)
	require.FileExists(t, execd.PodPath)
	config, err := prepared.Resolve("/config")
	require.NoError(t, err)
	require.DirExists(t, config.PodPath)

	cached, err := resolver.Prepare(context.Background(), source, store)
	require.NoError(t, err)
	require.True(t, cached.CacheHit)
}

func TestPlatformResolverRejectsUnconfiguredOCIAndInsecureArchive(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)
	_, err = NewPlatformResolver(nil).Prepare(context.Background(), infracatalog.ArtifactSource{
		Type: infracatalog.SourceOCIImage, Reference: "registry.example/execd@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}, store)
	require.ErrorIs(t, err, ErrArtifactSourceUnsupported)

	_, err = NewPlatformResolver(nil).Prepare(context.Background(), infracatalog.ArtifactSource{
		Type: infracatalog.SourceArchive, Reference: "http://example.test/component.tar.gz",
		Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}, store)
	require.ErrorIs(t, err, ErrArtifactSourceUnsupported)
}

func testTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, contents := range files {
		require.NoError(t, writer.WriteHeader(&tar.Header{
			Name: name, Mode: 0555, Size: int64(len(contents)), Typeflag: tar.TypeReg,
		}))
		_, err := io.WriteString(writer, contents)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}
