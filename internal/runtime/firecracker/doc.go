// Package firecracker implements the direct Firecracker runtime driver for
// Fastlet. It is the runtime-neutral Driver contract implementation behind
// the built-in "firecracker" SandboxPool runtime.
//
// The driver boots one Firecracker microVM on demand for every Sandbox create
// request: nothing is pre-warmed and no sidecar daemon stays resident.
// Firecracker is launched as a plain child process with an immutable
// per-Sandbox identity, which matches the NodeJanitor residual-process
// cleanup contract (ResidualProcessFirecracker, binary "firecracker" and
// "--id" matching).
//
// The built-in profile currently fails closed with
// CapabilityUnsupported/FirecrackerDriverUnimplemented. The lifecycle
// implementation lands behind this gate; until the profile gate is enabled,
// the HostCapabilityProber rejects the runtime before any driver is built.
package firecracker
