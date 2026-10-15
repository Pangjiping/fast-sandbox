package contract

import (
	"context"
	"errors"
)

// ErrVaultAbsent reports that egress holds no vault for the Sandbox — there
// is nothing to carry across pause/resume. It is a result, not a failure.
var ErrVaultAbsent = errors.New("egress holds no vault for the Sandbox")

// VaultMover carries the egress-owned vault file across pause/resume. The
// driver invokes ExportVault inside the pause flow — strictly before the
// runtime teardown purges the egress-local file (REMOVE_BINDING) — and
// ImportVault at restore, before the restored Sandbox's data plane becomes
// ready. Implementations live on the Fastlet side and deliver over the Pod
// loopback to the Pool's vault-capable Action Handler (OSEP-0022: export /
// import via driver, egress re-stamps the incarnation at load).
//
// Failure semantics: an export failure fails the checkpoint (the control
// plane only releases the runtime after the checkpoint is durable, so the
// egress-local file survives for the retry); an import failure degrades the
// restored subject's vault (fail-closed interception, observable) and must
// not fail the restore.
type VaultMover interface {
	// ExportVault returns the subject's egress-held vault file bytes.
	// ErrVaultAbsent means the subject holds no vault.
	ExportVault(ctx context.Context, sandboxUID string) ([]byte, error)
	// ImportVault delivers a bundled vault file to egress for the subject.
	ImportVault(ctx context.Context, sandboxUID string, data []byte) error
}
