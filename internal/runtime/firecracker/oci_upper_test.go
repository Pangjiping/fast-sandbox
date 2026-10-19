package firecracker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/ocitemplate"
)

func TestCaptureOCIUpperPreservesSparsePrivateChangesAndParent(t *testing.T) {
	root, staging := t.TempDir(), t.TempDir()
	home := filepath.Join(root, "oci/sandboxes/test")
	require.NoError(t, os.MkdirAll(filepath.Join(home, "upper"), 0750))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "mnt"), 0750))
	data := filepath.Join(home, "upper/writable_data")
	index := filepath.Join(home, "upper/writable_index")
	file, err := os.Create(data)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(1<<30))
	_, err = file.WriteAt([]byte("changed block"), 128<<20)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.WriteFile(index, []byte("frozen mapping"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "mnt/metadata.json"), []byte(`{"disk_size_mb":1024}`), 0600))
	config := map[string]any{"repoBlobUrl": "https://r.example/v2/templates/base/blobs", "lowers": []map[string]any{{"digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "size": 123}}, "upper": map[string]string{"data": data, "index": index}}
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	path := filepath.Join(home, "rootfs.config.v1.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	require.NoError(t, captureOCIUpper(root, "test", staging))
	upper, err := ocitemplate.ReadRootfsUpper(staging)
	require.NoError(t, err)
	require.Equal(t, "r.example/templates/base", upper.Repository)
	require.Equal(t, int64(1<<30), upper.VirtualSizeBytes)
	require.Len(t, upper.Lowers, 1)
	// A failed upload can leave the original VM running without changing its
	// frozen capture: later guest writes cannot leak into that checkpoint.
	require.NoError(t, os.WriteFile(index, []byte("later mapping"), 0600))
	original, err := os.OpenFile(data, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = original.WriteAt([]byte("later writes!"), 128<<20)
	require.NoError(t, err)
	require.NoError(t, original.Close())
	captured, err := os.Open(filepath.Join(staging, ocitemplate.UpperDataName))
	require.NoError(t, err)
	defer captured.Close()
	buf := make([]byte, len("changed block"))
	_, err = captured.ReadAt(buf, 128<<20)
	require.NoError(t, err)
	require.Equal(t, "changed block", string(buf))
	raw, err = os.ReadFile(filepath.Join(staging, ocitemplate.UpperIndexName))
	require.NoError(t, err)
	require.Equal(t, "frozen mapping", string(raw))
	_, err = os.Stat(filepath.Join(staging, publishedRootfsName))
	require.True(t, os.IsNotExist(err))
	// Invalid parent metadata is rejected before copying anything.
	config["lowers"] = []map[string]any{{"digest": "sha256:bad", "size": 123}}
	raw, err = json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	require.Error(t, captureOCIUpper(root, "test", t.TempDir()))
}
