package nodecleanup

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"

	"github.com/stretchr/testify/require"
)

func TestClientServerEnsureAbsent(t *testing.T) {
	tempDir, err := os.MkdirTemp("/tmp", "nodecleanup-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(tempDir)) })
	socket := filepath.Join(tempDir, "control.sock")
	cleaner := &recordingCleaner{}
	server, err := NewServer(socket, cleaner)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
		require.NoError(t, server.Close())
	})

	client := NewClient(socket)
	require.Eventually(t, func() bool {
		err = client.EnsureRuntimeProcessesAbsent(context.Background(), runtimecatalog.ResidualProcessFirecracker, "sandbox-a")
		return err == nil
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, runtimecatalog.ResidualProcessFirecracker, cleaner.kind)
	require.Equal(t, "sandbox-a", cleaner.sandboxID)
}

func TestEnsureAbsentHandlerRejectsMalformedRequest(t *testing.T) {
	handler := ensureAbsentHandler(&recordingCleaner{})
	server := &http.Server{Handler: handler}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { _ = server.Close() })

	resp, err := http.Post("http://"+listener.Addr().String()+EnsureAbsentPath, "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

type recordingCleaner struct {
	kind      runtimecatalog.ResidualProcessKind
	sandboxID string
}

func (c *recordingCleaner) EnsureRuntimeProcessesAbsent(_ context.Context, kind runtimecatalog.ResidualProcessKind, sandboxID string) error {
	c.kind = kind
	c.sandboxID = sandboxID
	return nil
}
