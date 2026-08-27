// Package agent implements the node-level firecracker-runtime-agent pull
// layer (stage 1 of the Firecracker on-demand loading design): the consumer
// side of the addressing chain SandboxSpec.Image -> index -> manifest ->
// content-addressed native artifacts, materialized in the cache shared with
// the driver (<StateRoot>/images/<sha256(image)>/).
//
// Stage 1 scope: no UDS server, no P2P, no overlaybd, and the driver keeps
// its local PullImage until the agent wiring lands.
package agent
