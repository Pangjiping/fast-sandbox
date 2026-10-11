//go:build unix

package janitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	taskapi "github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

func TestCleanupFIFOs(t *testing.T) {
	t.Run("removes nested FIFOs and is idempotent", func(t *testing.T) {
		root := t.TempDir()
		dir, err := os.MkdirTemp(root, "")
		require.NoError(t, err)
		for _, stream := range []string{"stdin", "stdout", "stderr"} {
			require.NoError(t, unix.Mkfifo(filepath.Join(dir, "container-a-"+stream), 0600))
		}
		empty := filepath.Join(root, "unrelated-empty")
		require.NoError(t, os.Mkdir(empty, 0700))

		require.NoError(t, cleanupFIFOs(root, "container-a"))
		require.NoDirExists(t, dir)
		require.DirExists(t, empty)
		require.DirExists(t, root)
		require.NoError(t, cleanupFIFOs(root, "container-a"))
		require.NoError(t, cleanupFIFOs(filepath.Join(root, "missing"), "container-a"))
		require.Error(t, cleanupFIFOs(root, ""))
	})

	t.Run("preserves unrelated resources and non-FIFO paths", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		dir, err := os.MkdirTemp(root, "")
		require.NoError(t, err)
		stdout := filepath.Join(dir, "container-a-stdout")
		require.NoError(t, unix.Mkfifo(stdout, 0600))
		otherFIFO := filepath.Join(dir, "container-ab-stdout")
		require.NoError(t, unix.Mkfifo(otherFIFO, 0600))
		regular := filepath.Join(dir, "container-a-stderr")
		require.NoError(t, os.WriteFile(regular, []byte("not a FIFO"), 0600))
		otherStream := filepath.Join(dir, "container-a-extra")
		require.NoError(t, unix.Mkfifo(otherStream, 0600))
		stdinDir := filepath.Join(dir, "container-a-stdin")
		require.NoError(t, os.Mkdir(stdinDir, 0700))
		deeperFIFO := filepath.Join(stdinDir, "container-a-stdout")
		require.NoError(t, unix.Mkfifo(deeperFIFO, 0600))
		outsideFIFO := filepath.Join(outside, "container-a-stdout")
		require.NoError(t, unix.Mkfifo(outsideFIFO, 0600))
		linkedDir := filepath.Join(root, "linked")
		require.NoError(t, os.Symlink(outside, linkedDir))
		links, err := os.MkdirTemp(root, "")
		require.NoError(t, err)
		linkedFIFO := filepath.Join(links, "container-a-stdout")
		require.NoError(t, os.Symlink(outsideFIFO, linkedFIFO))

		require.NoError(t, cleanupFIFOs(root, "container-a"))
		require.NoFileExists(t, stdout)
		for _, path := range []string{otherFIFO, regular, otherStream, stdinDir, deeperFIFO, linkedDir, linkedFIFO, outsideFIFO} {
			_, err := os.Lstat(path)
			require.NoError(t, err)
		}
		require.DirExists(t, dir)
		require.Error(t, cleanupFIFOs(regular, "container-a"))
	})
}

func TestContainerdCleanupRetainsFIFOsOnRuntimeError(t *testing.T) {
	root := t.TempDir()
	dir, err := os.MkdirTemp(root, "")
	require.NoError(t, err)
	path := filepath.Join(dir, "container-a-stdout")
	require.NoError(t, unix.Mkfifo(path, 0600))
	client, err := containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(&fifoCleanupContainerStore{}),
		containerd.WithTaskClient(&fifoCleanupUnavailableTasks{}),
	))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	backend := NewContainerdBackend(client, root)
	err = backend.Cleanup(context.Background(), ResourceIdentity{
		Backend: BackendContainerd, ResourceID: "container-a", ContainerdNamespace: "k8s.io",
	})
	require.ErrorContains(t, err, "task service unavailable")
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeNamedPipe)
}

type fifoCleanupContainerStore struct{ containers.Store }

func (*fifoCleanupContainerStore) Get(_ context.Context, id string) (containers.Container, error) {
	return containers.Container{ID: id}, nil
}

func (*fifoCleanupContainerStore) Delete(context.Context, string) error { return nil }

type fifoCleanupUnavailableTasks struct{ taskapi.TasksClient }

func (*fifoCleanupUnavailableTasks) Get(context.Context, *taskapi.GetRequest, ...grpc.CallOption) (*taskapi.GetResponse, error) {
	return nil, errors.New("task service unavailable")
}
