// Package firecracker-runtime-agent is the node-level runtime-agent entry
// point (stage 1): a thin assembly of the pull client, the durable lease
// state, and the UDS management server. Deployment carrier (DaemonSet vs
// co-located container) is a pending decision; the binary is a standalone
// process either way.
package main
