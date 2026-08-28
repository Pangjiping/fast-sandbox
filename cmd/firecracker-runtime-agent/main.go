package main

// firecracker-runtime-agent serves the node-level UDS management API of
// the Firecracker on-demand loading design (implementation plan §8): the
// pull chain (agent.Client) plus the durable lease/journal state, exposed
// to fastlet drivers as JSON over a Unix socket.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"fast-sandbox/internal/registryconfig"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentserver "fast-sandbox/internal/runtime/firecracker/agent/server"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"

	"k8s.io/klog/v2"
)

const (
	// defaultSocketPath is the UDS socket served by this agent.
	defaultSocketPath = "/run/fast-sandbox/firecracker/runtime.sock"
	// defaultStateRoot mirrors the fastlet driver's StateRoot, so the
	// shared cache (images/) and the driver's resolveRootfsImage agree.
	defaultStateRoot = "/var/lib/fast-sandbox/firecracker"
)

func main() {
	socketPath := getEnv("FAST_SANDBOX_RUNTIME_AGENT_SOCKET", defaultSocketPath)
	storeRoot := getEnv("FAST_SANDBOX_ARTIFACT_STORE", "")
	stateRoot := getEnv("FAST_SANDBOX_STATE_ROOT", defaultStateRoot)
	registryPath := getEnv("FAST_SANDBOX_REGISTRY_CONFIG_PATH", registryconfig.MountPath)

	if storeRoot == "" {
		klog.ErrorS(errors.New("missing store root"), "FAST_SANDBOX_ARTIFACT_STORE is required (s3://bucket/prefix)")
		os.Exit(1)
	}

	registryProvider := registryconfig.NewFileProvider(registryPath)
	credential, err := resolveCredential(registryProvider, storeRoot)
	if err != nil {
		klog.ErrorS(err, "Failed to resolve the artifact store credential")
		os.Exit(1)
	}
	pull, err := agentpull.NewClient(storeRoot, credential)
	if err != nil {
		klog.ErrorS(err, "Failed to build the artifact pull client")
		os.Exit(1)
	}
	state, err := agentstate.New(stateRoot)
	if err != nil {
		klog.ErrorS(err, "Failed to open the lease state", "stateRoot", stateRoot)
		os.Exit(1)
	}
	defer func() { _ = state.Close() }()

	service := agentserver.NewService(pull, state, stateRoot)
	server := agentserver.New(service, socketPath)
	klog.InfoS("firecracker-runtime-agent starting",
		"socket", socketPath, "store", storeRoot, "stateRoot", stateRoot, "registry", registryPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		klog.ErrorS(err, "firecracker-runtime-agent stopped")
		os.Exit(1)
	}
	klog.InfoS("firecracker-runtime-agent stopped")
}

// resolveCredential matches the store endpoint host against the compiled
// registry configuration; the credential carries the read-only access key
// pair (Username/Password) and the store Host.
func resolveCredential(provider registryconfig.Provider, storeRoot string) (registryconfig.Credential, error) {
	parsed, err := url.Parse(storeRoot)
	if err != nil {
		return registryconfig.Credential{}, fmt.Errorf("parse store root %q: %w", storeRoot, err)
	}
	if parsed.Host == "" {
		return registryconfig.Credential{}, fmt.Errorf("store root %q has no endpoint host", storeRoot)
	}
	credential, ok, err := provider.Credentials(parsed.Host)
	if err != nil {
		return registryconfig.Credential{}, err
	}
	if !ok {
		return registryconfig.Credential{}, fmt.Errorf("no read-only credential configured for store endpoint %q", parsed.Host)
	}
	return credential, nil
}

func getEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
