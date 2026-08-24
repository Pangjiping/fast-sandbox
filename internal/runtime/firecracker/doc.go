// Package firecracker implements the direct Firecracker runtime driver for
// Fastlet: one microVM booted on demand per Sandbox create request, launched
// as a plain child process with an immutable per-Sandbox identity (matching
// the NodeJanitor residual-process contract: binary "firecracker" + "--id").
//
// Networking comes from the pre-provisioned Fastlet slot: the slot carries
// the pod-side IP and the guest tap prepared by GuestVMNetNSDriver, so a
// create performs no ip/iptables commands. The guest owns the address after
// the slot IP (DNATed at the host) and is configured via the kernel ip=
// boot argument. Rootfs comes from the content-addressed cache (copied per
// instance), and durable state in StateRoot/sandboxes/<id>/meta.json anchors
// idempotent create, delete, and Fastlet-restart recovery.
//
// The built-in profile fails closed (CapabilityUnsupported) until the KVM
// E2E suite passes; the HostCapabilityProber rejects the runtime before any
// driver is built while the gate is closed.
package firecracker
