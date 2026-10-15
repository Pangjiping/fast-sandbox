package action

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func newVaultTransferManager(t *testing.T, handlerPort int32) *Manager {
	t.Helper()
	manager, err := NewManager([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: handlerPort}}, &fakeCaller{instanceID: "handler-1"})
	require.NoError(t, err)
	manager.SetVaultHandler("egress")
	return manager
}

func TestExportVaultReturnsFileBytes(t *testing.T) {
	var seenMethod, seenPath string
	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod, seenPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"v":1,"revision":7,"vault":{}}`))
	}))
	t.Cleanup(handlerServer.Close)
	manager := newVaultTransferManager(t, int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port))

	data, err := manager.ExportVault(context.Background(), "uid-a")
	require.NoError(t, err)
	require.Equal(t, []byte(`{"v":1,"revision":7,"vault":{}}`), data)
	require.Equal(t, http.MethodGet, seenMethod)
	require.Equal(t, "/vault/uid-a/export", seenPath)
}

func TestExportVaultMapsAbsentAndFailures(t *testing.T) {
	t.Run("handler answers 404", func(t *testing.T) {
		handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(handlerServer.Close)
		manager := newVaultTransferManager(t, int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port))
		_, err := manager.ExportVault(context.Background(), "uid-a")
		require.ErrorIs(t, err, runtimecontract.ErrVaultAbsent, "no vault is a result, not a failure")
	})
	t.Run("handler answers 500", func(t *testing.T) {
		handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(handlerServer.Close)
		manager := newVaultTransferManager(t, int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port))
		_, err := manager.ExportVault(context.Background(), "uid-a")
		require.ErrorContains(t, err, "HTTP 500")
	})
	t.Run("handler unreachable", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		require.NoError(t, listener.Close())
		manager := newVaultTransferManager(t, int32(listener.Addr().(*net.TCPAddr).Port))
		_, err = manager.ExportVault(context.Background(), "uid-a")
		require.Error(t, err)
	})
	t.Run("handler undeclared", func(t *testing.T) {
		manager, err := NewManager([]apiv1alpha2.ActionHandler{{Name: "audit", TargetHTTPPort: 18081}}, &fakeCaller{instanceID: "handler-1"})
		require.NoError(t, err)
		manager.SetVaultHandler("egress")
		_, err = manager.ExportVault(context.Background(), "uid-a")
		require.ErrorIs(t, err, runtimecontract.ErrVaultAbsent, "a pool without the vault handler can never hold vault state")
	})
	t.Run("transfer disabled", func(t *testing.T) {
		manager, err := NewManager([]apiv1alpha2.ActionHandler{{Name: "egress", TargetHTTPPort: 18080}}, &fakeCaller{instanceID: "handler-1"})
		require.NoError(t, err)
		_, err = manager.ExportVault(context.Background(), "uid-a")
		require.ErrorIs(t, err, runtimecontract.ErrVaultAbsent)
	})
}

func TestImportVaultDeliversFileBytes(t *testing.T) {
	var seenMethod, seenPath, seenContentType string
	var seenBody []byte
	handlerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod, seenPath, seenContentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		seenBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(seenBody)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(handlerServer.Close)
	manager := newVaultTransferManager(t, int32(handlerServer.Listener.Addr().(*net.TCPAddr).Port))

	require.NoError(t, manager.ImportVault(context.Background(), "uid-a", []byte(`{"v":1}`)))
	require.Equal(t, http.MethodPost, seenMethod)
	require.Equal(t, "/vault/uid-a/import", seenPath)
	require.Equal(t, "application/json", seenContentType)
	require.Equal(t, []byte(`{"v":1}`), seenBody)

	// A rejected import is a transfer failure the driver logs and tolerates.
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(rejecting.Close)
	rejectManager := newVaultTransferManager(t, int32(rejecting.Listener.Addr().(*net.TCPAddr).Port))
	require.ErrorContains(t, rejectManager.ImportVault(context.Background(), "uid-a", []byte(`{}`)), "HTTP 409")
}

func TestVaultHandlerFromEnvDefaultsToEgress(t *testing.T) {
	t.Setenv("FAST_SANDBOX_VAULT_HANDLER", "")
	require.Equal(t, "egress", VaultHandlerFromEnv())
	t.Setenv("FAST_SANDBOX_VAULT_HANDLER", "secret-vault")
	require.Equal(t, "secret-vault", VaultHandlerFromEnv())
}
