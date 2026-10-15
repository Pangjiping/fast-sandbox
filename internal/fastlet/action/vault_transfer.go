package action

// vault_transfer.go implements the Fastlet side of the driver-invoked vault
// transfer (runtimecontract.VaultMover): the egress-owned vault file moves
// across pause/resume over the same Pod-loopback delivery path as Sandbox
// Actions. The transfer endpoints are the driver-facing slice of the
// OpenSandbox-owned egress vault surface:
//
//	GET  /vault/{sandboxUid}/export -> 200 + vault file bytes; 404 = absent
//	POST /vault/{sandboxUid}/import <- vault file bytes; 2xx = accepted
//
// The egress re-stamps the imported file with the new incarnation at load —
// the transfer never interprets the bytes.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// vaultTransferEnvVar optionally overrides the name of the Pool-declared
// Action Handler that serves the egress vault surface. The default matches
// the OSEP-0022 fast-sandbox profile convention ("egress"); pools without
// that handler never stage vault files (ExportVault reports ErrVaultAbsent).
const vaultTransferEnvVar = "FAST_SANDBOX_VAULT_HANDLER"

// defaultVaultHandler is the conventional name of the egress Action Handler.
const defaultVaultHandler = "egress"

// maxVaultTransferBytes bounds one vault file transfer. The vault is
// deliberately decoupled from the 64 KiB binding-input cap, so the transfer
// carries its own (generous) bound instead of reusing the 32 KiB action
// response limit.
const maxVaultTransferBytes = 4 << 20

// vaultHTTPClient is the transfer client: loopback only, bounded timeout,
// redirects surfaced instead of followed — the same delivery posture as the
// Sandbox Actions caller.
var vaultHTTPClient = &http.Client{
	Timeout: defaultTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// SetVaultHandler configures the Pool-declared Action Handler name that
// serves the egress vault surface. An empty name disables vault transfer.
func (m *Manager) SetVaultHandler(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vaultHandler = name
}

// VaultHandler returns the configured vault handler name.
func (m *Manager) VaultHandler() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.vaultHandler
}

// VaultHandlerFromEnv resolves the vault handler name from the environment,
// defaulting to the egress convention.
func VaultHandlerFromEnv() string {
	if name := os.Getenv(vaultTransferEnvVar); name != "" {
		return name
	}
	return defaultVaultHandler
}

var _ runtimecontract.VaultMover = (*Manager)(nil)

// vaultPort resolves the declared loopback port of the vault handler. An
// unset or undeclared handler means the Pool cannot hold vault state at all,
// which is the ErrVaultAbsent result — not a failure.
func (m *Manager) vaultPort() (int32, error) {
	m.mu.RLock()
	name := m.vaultHandler
	handler, found := m.byName[name]
	m.mu.RUnlock()
	if name == "" || !found {
		return 0, runtimecontract.ErrVaultAbsent
	}
	return handler.TargetHTTPPort, nil
}

// ExportVault returns the subject's egress-held vault file bytes. A 404 from
// the handler maps to ErrVaultAbsent (the subject holds no vault); any other
// non-2xx answer or transport failure is a transfer failure.
func (m *Manager) ExportVault(ctx context.Context, sandboxUID string) ([]byte, error) {
	port, err := m.vaultPort()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(port, fmt.Sprintf("/vault/%s/export", sandboxUID)), nil)
	if err != nil {
		return nil, err
	}
	response, err := vaultHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("export vault from handler: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, runtimecontract.ErrVaultAbsent
	}
	body, err := readVaultTransferBody(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("export vault from handler: HTTP %d", response.StatusCode)
	}
	return body, nil
}

// ImportVault delivers a bundled vault file to egress. A non-2xx answer or
// transport failure is returned to the driver, which logs it and proceeds —
// the restored subject's vault then degrades (fail-closed, observable).
func (m *Manager) ImportVault(ctx context.Context, sandboxUID string, data []byte) error {
	port, err := m.vaultPort()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(port, fmt.Sprintf("/vault/%s/import", sandboxUID)), bytes.NewReader(data))
	if err != nil {
		return err
	}
	if len(data) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := vaultHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("import vault to handler: %w", err)
	}
	defer response.Body.Close()
	if _, err := readVaultTransferBody(response.Body); err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("import vault to handler: HTTP %d", response.StatusCode)
	}
	return nil
}

// readVaultTransferBody reads a bounded transfer payload.
func readVaultTransferBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxVaultTransferBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read vault transfer body: %w", err)
	}
	if len(body) > maxVaultTransferBytes {
		return nil, fmt.Errorf("vault transfer exceeds %d bytes", maxVaultTransferBytes)
	}
	return body, nil
}
