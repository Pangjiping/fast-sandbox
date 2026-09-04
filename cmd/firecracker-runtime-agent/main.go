package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"fast-sandbox/internal/registryconfig"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentdart "fast-sandbox/internal/runtime/firecracker/agent/dart"
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
	if err := run(); err != nil {
		klog.ErrorS(err, "firecracker-runtime-agent failed")
		os.Exit(1)
	}
}

// run assembles the agent. It returns an error when the agent cannot serve;
// deferred cleanup (lease state close, DART child stop) runs on the way out.
func run() error {
	socketPath := getEnv("FAST_SANDBOX_RUNTIME_AGENT_SOCKET", defaultSocketPath)
	storeRoot := getEnv("FAST_SANDBOX_ARTIFACT_STORE", "")
	stateRoot := getEnv("FAST_SANDBOX_STATE_ROOT", defaultStateRoot)
	registryPath := getEnv("FAST_SANDBOX_REGISTRY_CONFIG_PATH", registryconfig.MountPath)
	// The artifact store endpoint is usually derived from the credential
	// Host; an explicit endpoint overrides the matching key and the
	// connection address (e.g. a local MinIO: http://127.0.0.1:9000).
	endpoint := getEnv("FAST_SANDBOX_ARTIFACT_ENDPOINT", "")

	if storeRoot == "" {
		return errors.New("FAST_SANDBOX_ARTIFACT_STORE is required (s3://bucket/prefix)")
	}

	registryProvider := registryconfig.NewFileProvider(registryPath)
	credential, err := resolveCredential(registryProvider, storeRoot, endpoint)
	if err != nil {
		return fmt.Errorf("resolve the artifact store credential: %w", err)
	}

	// DART P2P gateway (stage 2). FAST_SANDBOX_DART_ADDR empty = local
	// mode: artifact pulls stay on the direct header-signed S3 path.
	// Non-empty = the node-local DART daemon is orchestrated as a child
	// process and artifact bytes route through its prefix API as presigned
	// URLs (with direct-S3 fallback when DART is unreachable).
	dartAddr := getEnv("FAST_SANDBOX_DART_ADDR", "")
	var pullOptions []agentpull.Option
	var dartManager *agentdart.Manager
	if dartAddr != "" {
		listen, err := dartListenAddress(dartAddr)
		if err != nil {
			return err
		}
		// A stable identity anchors the HRW keyspace AND names the per-node
		// block cache: the StateRoot can be shared (multi-node kind mounts
		// one host filesystem into every node container), but two DART
		// arenas must never point at the same directory. The node's
		// hostname is read from /etc/hostname (mounted from the node by the
		// DaemonSet) because a regular pod's own hostname is its pod name,
		// which changes on restart.
		nodeID := getEnv("FAST_SANDBOX_DART_SELF_ID", "")
		if nodeID == "" {
			nodeID = nodeHostID()
		}
		peerPort := "9000"
		config := agentdart.Config{
			Binary:    getEnv("FAST_SANDBOX_DART_BIN", "dart"),
			Listen:    listen,
			Admin:     getEnv("FAST_SANDBOX_DART_ADMIN", "127.0.0.1:8147"),
			CacheDir:  filepath.Join(stateRoot, "cache", "dart-"+strings.ReplaceAll(nodeID, "/", "-")),
			CacheSize: getEnv("FAST_SANDBOX_DART_CACHE_SIZE", "8GiB"),
			Discover:  getEnv("FAST_SANDBOX_DART_DISCOVER", ""),
			SelfID:    nodeID,
			Log:       os.Stderr,
		}
		if nodeIP := getEnv("FAST_SANDBOX_NODE_IP", ""); nodeIP != "" {
			config.PeerAdvertise = net.JoinHostPort(nodeIP, peerPort)
		}
		dartManager = agentdart.New(config)
		pullOptions = append(pullOptions, agentpull.WithDART(dartAddr))
		klog.InfoS("DART P2P gateway enabled", "addr", dartAddr,
			"discover", config.Discover, "cacheDir", config.CacheDir, "peerAdvertise", config.PeerAdvertise)
	}

	pull, err := agentpull.NewClient(storeRoot, credential, pullOptions...)
	if err != nil {
		return fmt.Errorf("build the artifact pull client: %w", err)
	}
	state, err := agentstate.New(stateRoot)
	if err != nil {
		return fmt.Errorf("open the lease state: %w", err)
	}
	defer func() { _ = state.Close() }()

	serviceOptions := []agentserver.ServiceOption{}
	if dartManager != nil {
		serviceOptions = append(serviceOptions, agentserver.WithDARTProbe(dartManager.Healthy))
	}
	service := agentserver.NewService(pull, state, stateRoot, serviceOptions...)
	server := agentserver.New(service, socketPath)
	klog.InfoS("firecracker-runtime-agent starting",
		"socket", socketPath, "store", storeRoot, "stateRoot", stateRoot,
		"registry", registryPath, "dart", dartAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if dartManager != nil {
		go func() {
			if err := dartManager.Run(ctx); err != nil {
				klog.ErrorS(err, "DART supervisor stopped with an error")
			}
		}()
	}
	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	klog.InfoS("firecracker-runtime-agent stopped")
	return nil
}

// dartListenAddress derives the DART client-plane listen address from the
// FAST_SANDBOX_DART_ADDR base (http://127.0.0.1:8145 -> 127.0.0.1:8145).
func dartListenAddress(dartAddr string) (string, error) {
	parsed, err := url.Parse(dartAddr)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid FAST_SANDBOX_DART_ADDR %q: expected http://host:port", dartAddr)
	}
	return parsed.Host, nil
}

// nodeHostID derives the stable node identity: the node hostname file
// (FAST_SANDBOX_HOSTNAME_FILE, mounted from the node by the DaemonSet),
// falling back to the process hostname.
func nodeHostID() string {
	path := getEnv("FAST_SANDBOX_HOSTNAME_FILE", "/etc/hostname")
	if payload, err := os.ReadFile(path); err == nil {
		if name := strings.TrimSpace(string(payload)); name != "" {
			return name
		}
	}
	return hostnameOrEmpty()
}

func hostnameOrEmpty() string {
	hostname, err := os.Hostname()
	if err != nil {
		return ""
	}
	return hostname
}

// resolveCredential matches the store endpoint host against the compiled
// registry configuration; the credential carries the read-only access key
// pair (Username/Password) and the store endpoint. When endpointEnv is set,
// its host is the matching key (an explicit artifact store endpoint such as
// a MinIO address); otherwise the store root host (the bucket) is matched.
// The match is a normalized host comparison (CredentialsForHost), not an
// image-reference match: a bare endpoint host like "127.0.0.1:9000" has no
// repository component for the reference-splitting rules to parse.
func resolveCredential(provider registryconfig.Provider, storeRoot, endpointEnv string) (registryconfig.Credential, error) {
	parsed, err := url.Parse(storeRoot)
	if err != nil {
		return registryconfig.Credential{}, fmt.Errorf("parse store root %q: %w", storeRoot, err)
	}
	matchHost := parsed.Host
	if endpointEnv != "" {
		parsedEndpoint, parseErr := url.Parse(endpointEnv)
		if parseErr != nil || parsedEndpoint.Host == "" {
			return registryconfig.Credential{}, fmt.Errorf("invalid artifact store endpoint %q", endpointEnv)
		}
		matchHost = parsedEndpoint.Host
	}
	if matchHost == "" {
		return registryconfig.Credential{}, fmt.Errorf("store root %q has no endpoint host", storeRoot)
	}
	var credential registryconfig.Credential
	var ok bool
	if hostMatcher, supports := provider.(interface {
		CredentialsForHost(string) (registryconfig.Credential, bool, error)
	}); supports {
		credential, ok, err = hostMatcher.CredentialsForHost(matchHost)
	} else {
		credential, ok, err = provider.Credentials(matchHost)
	}
	if err != nil {
		return registryconfig.Credential{}, err
	}
	if !ok {
		return registryconfig.Credential{}, fmt.Errorf("no read-only credential configured for store endpoint %q", matchHost)
	}
	return credential, nil
}

func getEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
