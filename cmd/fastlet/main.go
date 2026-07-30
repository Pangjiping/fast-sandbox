package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/dataplane/fastletproxy"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletsandbox "fast-sandbox/internal/fastlet/sandbox"
	"fast-sandbox/internal/fastlet/server"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	runtimefactory "fast-sandbox/internal/runtime/factory"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

func main() {
	flag.Parse()
	klog.Info("starting sandbox fastlet")
	traceShutdown, err := observability.Configure(context.Background(), "fast-sandbox-fastlet")
	if err != nil {
		klog.ErrorS(err, "Configure OpenTelemetry")
		os.Exit(1)
	}
	defer shutdownTracing(traceShutdown)

	podName := getEnv("POD_NAME", "")
	podUID := getEnv("POD_UID", "")
	podIP := getEnv("POD_IP", "")
	nodeName := getEnv("NODE_NAME", "")
	namespace := getEnv("NAMESPACE", "")
	fastletPort := getEnv("FASTLET_CONTROL_PORT", ":5758")
	runtimeName := getEnv("FAST_SANDBOX_RUNTIME", "container")
	runtimeSocket := getEnv("RUNTIME_SOCKET", "")
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeName(runtimeName))
	if err != nil {
		klog.ErrorS(err, "Failed to resolve runtime profile")
		os.Exit(1)
	}
	injectedRuntimeHash := getEnv("FAST_SANDBOX_RUNTIME_PROFILE_HASH", runtimeProfile.ProfileHash)
	if injectedRuntimeHash != runtimeProfile.ProfileHash {
		klog.ErrorS(runtimecontract.ErrSandboxProfileMismatch, "Injected runtime profile hash does not match built-in catalog", "injected", injectedRuntimeHash, "expected", runtimeProfile.ProfileHash)
		os.Exit(1)
	}
	resourceProfile, err := resourceProfileFromEnvironment()
	if err != nil {
		klog.ErrorS(err, "Failed to resolve Sandbox resource profile")
		os.Exit(1)
	}
	warmImages, err := warmImagesFromEnvironment()
	if err != nil {
		klog.ErrorS(err, "Failed to parse warmImages")
		os.Exit(1)
	}

	klog.InfoS("Fastlet Info", "PodName", podName, "PodIP", podIP, "NodeName", nodeName, "Namespace", namespace)
	klog.InfoS("Runtime", "Name", runtimeName, "Socket", runtimeSocket)

	ctx := context.Background()
	var rt runtimecontract.Driver

	rt, _, err = runtimefactory.New(runtimecatalog.Builtin(), runtimefactory.NewHostCapabilityProber()).Create(ctx, runtimeProfile.Name, runtimeSocket)

	if err != nil {
		klog.ErrorS(err, "Failed to initialize runtime")
		os.Exit(1)
	}
	defer rt.Close()

	rt.SetNamespace(namespace)
	registryProvider := registryconfig.NewFileProvider(getEnv("FAST_SANDBOX_REGISTRY_CONFIG_PATH", registryconfig.MountPath))
	if revision, err := registryProvider.Refresh(); err != nil {
		klog.ErrorS(err, "Failed to load Registry configuration")
		os.Exit(1)
	} else {
		klog.InfoS("Registry configuration loaded", "revision", revision)
	}
	if configurable, ok := rt.(registryConfigurable); ok {
		configurable.SetRegistryProvider(registryProvider)
	}
	if runtimeProfile.UsesFastletNetNS() {
		networkManager, err := newNetworkManager(capacityFromEnvironment(), podUID)
		if err != nil {
			klog.ErrorS(err, "Failed to configure Fastlet-owned network")
			os.Exit(1)
		}
		if err := networkManager.Initialize(ctx); err != nil {
			klog.ErrorS(err, "Failed to initialize Fastlet-owned network")
			os.Exit(1)
		}
		configurable, ok := rt.(networkConfigurable)
		if !ok {
			klog.ErrorS(runtimecontract.ErrUnsupportedRuntime, "Runtime profile requires Linux netns but driver is not network configurable")
			os.Exit(1)
		}
		configurable.SetNetworkManager(networkManager)
		klog.InfoS("Fastlet-owned network initialized", "capacity", networkManager.Snapshot().Capacity, "cleanSlots", networkManager.Snapshot().Clean)
	}
	infraRevision := getEnv("FAST_SANDBOX_INFRA_REVISION", "")
	infraManager, err := newInfraManager(
		podUID,
		runtimeProfile,
		getEnv("FAST_SANDBOX_INFRA_PLAN_PATH", "/etc/fast-sandbox/infra/plan.json"),
		infraRevision,
		runtimeSocket,
		registryProvider,
	)
	if err != nil {
		klog.ErrorS(err, "Failed to configure Infra Components")
		os.Exit(1)
	}
	infraConfigurable, ok := rt.(infraConfigurable)
	if !ok {
		klog.ErrorS(runtimecontract.ErrUnsupportedRuntime, "Runtime driver cannot accept an Infra Component plan")
		os.Exit(1)
	}
	infraConfigurable.SetInfraManager(infraManager)

	klog.InfoS("Runtime initialized successfully", "name", runtimeName)

	proxyControlClient := fastletproxy.NewControlClient(getEnv("FASTLET_PROXY_CONTROL_SOCKET", fastletproxy.DefaultControlSocket))
	sandboxManager, err := fastletsandbox.NewSandboxManagerWithConfig(rt, fastletsandbox.SandboxManagerConfig{
		Capacity: capacityFromEnvironment(), RuntimeName: runtimeProfile.Name, RuntimeProfileHash: runtimeProfile.ProfileHash, ResourceProfile: &resourceProfile,
		FastletPodUID: podUID, RecoverOnStart: true,
		WarmImages:     warmImages,
		RoutePublisher: fastletproxy.NewRoutePublisher(proxyControlClient),
		InfraRevision:  infraManager.Revision(), InfraManager: infraManager,
		RegistryProvider: registryProvider,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to initialize Sandbox manager")
		os.Exit(1)
	}
	defer sandboxManager.Close()
	go recoverUntilReady(ctx, sandboxManager, proxyControlClient)

	fastletServer := server.NewFastletServer(fastletPort, sandboxManager)
	klog.InfoS("Starting Fastlet HTTP Server", "port", fastletPort)

	if err := fastletServer.Start(); err != nil {
		klog.ErrorS(err, "Fastlet server failed")
		os.Exit(1)
	}
}

func newNetworkManager(capacity int, podUID string) (*fastletnetwork.Manager, error) {
	config := fastletnetwork.DefaultConfig(capacity, podUID)
	config.PodName = os.Getenv("POD_NAME")
	config.PodNamespace = os.Getenv("NAMESPACE")
	config.PrivateCIDR = getEnv("FAST_SANDBOX_NETWORK_CIDR", config.PrivateCIDR)
	config.Bridge = getEnv("FAST_SANDBOX_NETWORK_BRIDGE", config.Bridge)
	config.EgressDevice = getEnv("FAST_SANDBOX_NETWORK_EGRESS_DEVICE", "")
	config.StateRoot = getEnv("FAST_SANDBOX_NETWORK_STATE_ROOT", config.StateRoot)
	config.NetNSRoot = getEnv("FAST_SANDBOX_NETWORK_NETNS_ROOT", config.NetNSRoot)
	config.HostNetNSRoot = getEnv("FAST_SANDBOX_NETWORK_HOST_NETNS_ROOT", config.HostNetNSRoot)
	mtu, err := strconv.Atoi(getEnv("FAST_SANDBOX_NETWORK_MTU", strconv.Itoa(config.MTU)))
	if err != nil || mtu <= 0 {
		return nil, runtimecontract.ErrInvalidConfig
	}
	config.MTU = mtu
	store := fastletnetwork.NewFileStateStore(filepath.Join(config.StateRoot, podUID))
	return fastletnetwork.NewManager(config, fastletnetwork.NewLinuxNetNSDriver(fastletnetwork.LinuxDriverConfig{}), store)
}

type networkConfigurable interface {
	SetNetworkManager(*fastletnetwork.Manager)
}

type infraConfigurable interface {
	SetInfraManager(*fastletinfra.Manager)
}

type registryConfigurable interface {
	SetRegistryProvider(registryconfig.Provider)
}

func recoverUntilReady(ctx context.Context, manager *fastletsandbox.SandboxManager, proxyClient *fastletproxy.ControlClient) {
	for {
		if err := manager.Recover(ctx); err == nil {
			klog.Info("Fastlet runtime recovery completed")
			go warmCacheUntilReady(ctx, manager)
			go prepareInfraUntilReady(ctx, manager)
			go watchProxyRoutes(ctx, manager, proxyClient)
			return
		} else {
			klog.ErrorS(err, "Fastlet runtime recovery failed; readiness remains false")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func warmCacheUntilReady(ctx context.Context, manager *fastletsandbox.SandboxManager) {
	for ctx.Err() == nil {
		if err := manager.WarmCache(ctx); err == nil {
			klog.Info("Asynchronous warmImages preparation completed")
			return
		} else {
			klog.ErrorS(err, "Asynchronous warmImages preparation failed; retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func prepareInfraUntilReady(ctx context.Context, manager *fastletsandbox.SandboxManager) {
	for ctx.Err() == nil {
		if err := manager.PrepareInfra(ctx); err == nil {
			klog.Info("Fastlet Infra Component preparation completed")
			return
		} else {
			klog.ErrorS(err, "Fastlet Infra Component preparation failed; revision admission remains disabled")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func newInfraManager(
	podUID string,
	runtimeProfile runtimecatalog.RuntimeProfile,
	planPath string,
	expectedRevision string,
	runtimeSocket string,
	registryProvider registryconfig.Provider,
) (*fastletinfra.Manager, error) {
	podRoot, hostRoot, err := fastletinfra.DefaultStorePaths(podUID)
	if err != nil {
		return nil, err
	}
	podRoot = getEnv("FAST_SANDBOX_INFRA_STORE_ROOT", podRoot)
	hostRoot = getEnv("FAST_SANDBOX_INFRA_HOST_ROOT", hostRoot)
	store, err := fastletinfra.NewArtifactStore(podRoot, hostRoot)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(planPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var plan infracatalog.Plan
	if err := json.NewDecoder(file).Decode(&plan); err != nil {
		return nil, err
	}
	if expectedRevision != "" && plan.Revision != expectedRevision {
		return nil, fmt.Errorf("Infra plan revision %s does not match expected %s", plan.Revision, expectedRevision)
	}
	ociOpener := fastletinfra.NewContainerdOCIArtifactOpener(
		runtimeSocket,
		getEnv("FAST_SANDBOX_SNAPSHOTTER", "overlayfs"),
		registryProvider,
	)
	return fastletinfra.NewManagerWithConfig(fastletinfra.ManagerConfig{
		Plan: plan, RuntimeProfile: runtimeProfile, Store: store,
		Resolver: fastletinfra.NewPlatformResolverWithOptions(fastletinfra.PlatformResolverOptions{
			OCI: ociOpener,
		}),
		SandboxInitPath:   getEnv("FAST_SANDBOX_SANDBOX_INIT_PATH", "/opt/fast-sandbox/bin/sandbox-init"),
		SandboxTunnelPath: getEnv("FAST_SANDBOX_SANDBOX_TUNNEL_PATH", "/opt/fast-sandbox/bin/sandbox-tunnel"),
	})
}

func watchProxyRoutes(ctx context.Context, manager *fastletsandbox.SandboxManager, proxyClient *fastletproxy.ControlClient) {
	for ctx.Err() == nil {
		if err := manager.ReconcileProxyRoutes(ctx); err != nil {
			klog.ErrorS(err, "Reconcile Fastlet Proxy routes after control reconnect")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		if err := proxyClient.Watch(ctx, func(fastletproxy.Event) error { return nil }); err != nil && ctx.Err() == nil {
			manager.MarkProxyRouteUnavailable()
			klog.ErrorS(err, "Fastlet Proxy control watch disconnected; route readiness revoked")
		}
	}
}

func warmImagesFromEnvironment() ([]string, error) {
	value := getEnv("FAST_SANDBOX_WARM_IMAGES", "[]")
	var images []string
	if err := json.Unmarshal([]byte(value), &images); err != nil {
		return nil, err
	}
	return images, nil
}

func resourceProfileFromEnvironment() (apiv1alpha2.SandboxResourceProfile, error) {
	cpu, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_CPU", "1"))
	if err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	memory, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_MEMORY", "512Mi"))
	if err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	pids, err := strconv.ParseInt(getEnv("FAST_SANDBOX_RESOURCE_PIDS", "256"), 10, 64)
	if err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	profile := apiv1alpha2.SandboxResourceProfile{CPU: cpu, Memory: memory, PIDs: pids}
	if err := apiv1alpha2.ValidateSandboxResourceProfile(profile); err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	return profile, nil
}

func capacityFromEnvironment() int {
	value, err := strconv.Atoi(getEnv("FASTLET_CAPACITY", "5"))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func shutdownTracing(shutdown observability.Shutdown) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		klog.ErrorS(err, "Flush OpenTelemetry traces")
	}
}
