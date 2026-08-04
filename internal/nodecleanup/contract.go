// Package nodecleanup provides the narrow node-local control channel used by
// Fastlet to ask NodeJanitor to clean runtime processes which are outside the
// Fastlet Pod PID namespace.
package nodecleanup

import (
	"context"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

const (
	DefaultSocketPath = "/run/fast-sandbox/janitor/control.sock"
	EnsureAbsentPath  = "/v1/runtime-processes/ensure-absent"
)

type EnsureAbsentRequest struct {
	Kind      runtimecatalog.ResidualProcessKind `json:"kind"`
	SandboxID string                             `json:"sandboxID"`
}

type RuntimeProcessCleaner interface {
	EnsureRuntimeProcessesAbsent(context.Context, runtimecatalog.ResidualProcessKind, string) error
}
