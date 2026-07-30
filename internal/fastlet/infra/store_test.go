package infra

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArtifactStoreVerifiesDigestAndReusesReadOnlyBlob(t *testing.T) {
	podRoot := t.TempDir()
	store, err := NewArtifactStore(podRoot, "/host/infra")
	require.NoError(t, err)
	payload := []byte("immutable infra artifact")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
	opens := 0
	open := func() (io.ReadCloser, error) {
		opens++
		return io.NopCloser(bytes.NewReader(payload)), nil
	}

	first, err := store.Stage(context.Background(), digest, true, open)
	require.NoError(t, err)
	require.False(t, first.CacheHit)
	require.Equal(t, 1, opens)
	require.Equal(t, filepath.Join("/host/infra", "blobs", "sha256", digest[len("sha256:"):], "executable"), first.HostPath)
	info, err := os.Stat(first.PodPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0555), info.Mode().Perm())

	second, err := store.Stage(context.Background(), digest, true, open)
	require.NoError(t, err)
	require.True(t, second.CacheHit)
	require.Equal(t, 1, opens, "cache hit must not reopen registry/static source")
}

func TestArtifactStoreSeparatesExecutableAndDataVariants(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)
	payload := []byte("same immutable bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }

	data, err := store.Stage(context.Background(), digest, false, open)
	require.NoError(t, err)
	executable, err := store.Stage(context.Background(), digest, true, open)
	require.NoError(t, err)
	require.NotEqual(t, data.PodPath, executable.PodPath)
	require.Equal(t, os.FileMode(0444), fileMode(t, data.PodPath))
	require.Equal(t, os.FileMode(0555), fileMode(t, executable.PodPath))
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

func TestArtifactStoreRejectsMismatchAndCorruption(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)
	expected := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("expected")))
	_, err = store.Stage(context.Background(), expected, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("wrong")), nil
	})
	require.ErrorIs(t, err, ErrDigestMismatch)

	prepared, err := store.Stage(context.Background(), expected, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("expected")), nil
	})
	require.NoError(t, err)
	require.NoError(t, os.Chmod(prepared.PodPath, 0644))
	require.NoError(t, os.WriteFile(prepared.PodPath, []byte("corrupt"), 0444))
	_, _, err = store.Lookup(context.Background(), expected)
	require.ErrorIs(t, err, ErrArtifactCorrupted)
}

func TestArtifactStoreEnforcesBoundAndCancellation(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)
	store.SetMaxBytes(4)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("12345")))
	_, err = store.Stage(context.Background(), digest, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("12345")), nil
	})
	require.ErrorIs(t, err, ErrArtifactTooLarge)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.Stage(ctx, digest, false, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString("12345")), nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestArtifactStoreStagesVerifiedGzipTreeAndMappings(t *testing.T) {
	archive := gzipTar(t, []tarEntry{
		{name: "bin/execd", mode: 0o555, body: "binary"},
		{name: "assets/config.yaml", mode: 0o444, body: "enabled: true"},
	})
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)

	source, err := store.StageTree(context.Background(), digest, true, true, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	})
	require.NoError(t, err)
	require.False(t, source.CacheHit)
	execd, err := source.Resolve("/bin/execd")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o555), fileMode(t, execd.PodPath))
	require.Equal(t, filepath.Join(source.HostRoot, "bin", "execd"), execd.HostPath)
	assets, err := source.Resolve("/assets")
	require.NoError(t, err)
	require.DirExists(t, assets.PodPath)

	cached, err := store.StageTree(context.Background(), digest, true, true, func() (io.ReadCloser, error) {
		t.Fatal("cache hit reopened archive source")
		return nil, nil
	})
	require.NoError(t, err)
	require.True(t, cached.CacheHit)

	wrongDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("wrong")))
	_, err = store.StageTree(context.Background(), wrongDigest, true, true, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	})
	require.ErrorIs(t, err, ErrDigestMismatch)
}

func TestArtifactStoreRejectsUnsafeArchiveEntries(t *testing.T) {
	tests := []struct {
		name  string
		entry tarEntry
	}{
		{name: "path traversal", entry: tarEntry{name: "../../escape", body: "bad"}},
		{name: "absolute path", entry: tarEntry{name: "/escape", body: "bad"}},
		{name: "escaping symlink", entry: tarEntry{name: "bin/link", typeflag: tar.TypeSymlink, linkname: "../../escape"}},
		{name: "escaping hardlink", entry: tarEntry{name: "bin/link", typeflag: tar.TypeLink, linkname: "../../escape"}},
		{name: "device", entry: tarEntry{name: "dev/evil", typeflag: tar.TypeChar}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := gzipTar(t, []tarEntry{test.entry})
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
			store, err := NewArtifactStore(t.TempDir(), "/host/infra")
			require.NoError(t, err)
			_, err = store.StageTree(context.Background(), digest, true, true, func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(archive)), nil
			})
			require.Error(t, err)
		})
	}
}

func TestArtifactStoreRewritesRootfsAbsoluteSymlinks(t *testing.T) {
	archive := gzipTar(t, []tarEntry{
		{name: "usr/bin/tool", mode: 0o555, body: "binary"},
		{name: "bin/tool", typeflag: tar.TypeSymlink, linkname: "/usr/bin/tool"},
		// Real OCI root filesystems may contain unrelated absolute symlinks.
		{name: "bin/arch", typeflag: tar.TypeSymlink, linkname: "/usr/bin/arch"},
	})
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
	store, err := NewArtifactStore(t.TempDir(), "/host/infra")
	require.NoError(t, err)

	source, err := store.StageTree(context.Background(), digest, true, true, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	})
	require.NoError(t, err)

	tool, err := source.Resolve("/bin/tool")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(source.PodRoot, "usr", "bin", "tool"), tool.PodPath)
	require.Equal(t, filepath.Join(source.HostRoot, "usr", "bin", "tool"), tool.HostPath)
	target, err := os.Readlink(filepath.Join(source.PodRoot, "bin", "tool"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join("..", "usr", "bin", "tool"), target)
}

type tarEntry struct {
	name     string
	mode     int64
	body     string
	typeflag byte
	linkname string
}

func gzipTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var result bytes.Buffer
	compressed := gzip.NewWriter(&result)
	writer := tar.NewWriter(compressed)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o444
		}
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		size := int64(0)
		if typeflag == tar.TypeReg || typeflag == tar.TypeRegA {
			size = int64(len(entry.body))
		}
		require.NoError(t, writer.WriteHeader(&tar.Header{
			Name: entry.name, Mode: mode, Size: size, Typeflag: typeflag, Linkname: entry.linkname,
		}))
		if size > 0 {
			_, err := io.WriteString(writer, entry.body)
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())
	require.NoError(t, compressed.Close())
	return result.Bytes()
}
