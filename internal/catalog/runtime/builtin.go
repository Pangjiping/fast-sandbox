package runtime

import (
	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const builtinProfileVersion = "v1"

func builtinProfiles() map[apiv1alpha2.RuntimeName]RuntimeProfile {
	linuxNetworkPaths := []HostPathRequirement{
		{Name: "fast-sandbox-netns", HostPath: "/run/fast-sandbox/netns", MountPath: "/run/netns", Type: corev1.HostPathDirectoryOrCreate, MountPropagation: corev1.MountPropagationBidirectional},
		{Name: "fast-sandbox-network", HostPath: "/run/fast-sandbox/network", MountPath: "/run/fast-sandbox/network", Type: corev1.HostPathDirectoryOrCreate},
	}
	containerPaths := append([]HostPathRequirement{}, linuxNetworkPaths...)
	gvisorPaths := append([]HostPathRequirement{}, containerPaths...)
	gvisorPaths = append(gvisorPaths,
		HostPathRequirement{Name: "gvisor-runsc", HostPath: "/usr/local/bin/runsc", MountPath: "/usr/local/bin/runsc", Type: corev1.HostPathFile, ReadOnly: true},
		HostPathRequirement{Name: "gvisor-shim", HostPath: "/usr/local/bin/containerd-shim-runsc-v1", MountPath: "/usr/local/bin/containerd-shim-runsc-v1", Type: corev1.HostPathFile, ReadOnly: true},
		HostPathRequirement{Name: "gvisor-config", HostPath: "/etc/containerd/runsc.toml", MountPath: "/etc/containerd/runsc.toml", Type: corev1.HostPathFile, ReadOnly: true},
	)
	kataPaths := append([]HostPathRequirement{}, linuxNetworkPaths...)
	kataPaths = append(kataPaths,
		HostPathRequirement{Name: "dev-kvm", HostPath: "/dev/kvm", MountPath: "/dev/kvm", Type: corev1.HostPathCharDev},
		HostPathRequirement{Name: "kata-runtime", HostPath: "/opt/kata", MountPath: "/opt/kata", Type: corev1.HostPathDirectory, ReadOnly: true},
	)

	return map[apiv1alpha2.RuntimeName]RuntimeProfile{
		apiv1alpha2.RuntimeContainer: {
			Name: apiv1alpha2.RuntimeContainer, Version: builtinProfileVersion, Driver: DriverKindContainerd,
			Containerd:         &ContainerdConfig{Namespace: DefaultContainerdNamespace, Handler: "io.containerd.runc.v2"},
			Deployment:         DeploymentRequirements{Privileged: true, HostPaths: containerPaths, Overhead: overhead("100m", "128Mi")},
			Capabilities:       Capabilities{DefaultState: CapabilityConfigured, SupportsNetwork: true, SupportsCache: true, SupportsRecovery: true},
			NetworkMode:        NetworkModeLinuxNetNS,
			InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryBindMount, InfraDeliveryImageLayer, InfraDeliveryPreinstalled},
		},
		apiv1alpha2.RuntimeGVisor: {
			Name: apiv1alpha2.RuntimeGVisor, Version: builtinProfileVersion, Driver: DriverKindContainerd,
			Containerd:         &ContainerdConfig{Namespace: DefaultContainerdNamespace, Handler: "io.containerd.runsc.v1", ConfigPath: "/etc/containerd/runsc.toml", OptionsType: "io.containerd.runsc.v1.options", NeedsTTY: true},
			Deployment:         DeploymentRequirements{Privileged: true, HostPaths: gvisorPaths, Overhead: overhead("200m", "256Mi")},
			Capabilities:       Capabilities{DefaultState: CapabilityConfigured, SupportsNetwork: true, SupportsCache: true, SupportsRecovery: true},
			NetworkMode:        NetworkModeLinuxNetNS,
			InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryBindMount, InfraDeliveryImageLayer},
		},
		apiv1alpha2.RuntimeKataQemu: kataProfile(apiv1alpha2.RuntimeKataQemu, "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml", kataPaths),
		apiv1alpha2.RuntimeKataClh:  kataProfile(apiv1alpha2.RuntimeKataClh, "/opt/kata/share/defaults/kata-containers/configuration-clh.toml", kataPaths),
		apiv1alpha2.RuntimeKataFc: withResidualProcess(
			kataProfile(apiv1alpha2.RuntimeKataFc, "/opt/kata/share/defaults/kata-containers/configuration-fc.toml", kataPaths),
			ResidualProcessFirecracker,
		),
		apiv1alpha2.RuntimeKataDragonball: kataRustProfile(
			apiv1alpha2.RuntimeKataDragonball,
			"/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball.toml",
			kataPaths,
		),
		apiv1alpha2.RuntimeBoxLite: {
			Name: apiv1alpha2.RuntimeBoxLite, Version: builtinProfileVersion, Driver: DriverKindBoxLite,
			BoxLite: &BoxLiteConfig{
				StateRoot: "/var/lib/fast-sandbox/boxlite", BinaryPath: "/usr/local/bin/boxlite", ProxyBinary: "gvproxy",
				ControlSocket: "/run/fast-sandbox/boxlite/runtime.sock", ProtocolVersion: "v1", TunnelGuestPort: 19090,
				DefaultVCPUs: 1, DefaultMemory: "1Gi",
			},
			Deployment: DeploymentRequirements{
				Privileged: true, RequiresKVM: true, Sidecar: "boxlite-runtime", ResourceOwner: "boxlite-runtime", Overhead: overhead("200m", "256Mi"),
				HostPaths: []HostPathRequirement{
					{Name: "dev-kvm", HostPath: "/dev/kvm", MountPath: "/dev/kvm", Type: corev1.HostPathCharDev},
					{Name: "boxlite-state", HostPath: "/var/lib/fast-sandbox/boxlite", MountPath: "/var/lib/fast-sandbox/boxlite", Type: corev1.HostPathDirectoryOrCreate},
				},
			},
			Capabilities:       Capabilities{DefaultState: CapabilityUnsupported, SupportsNetwork: true, SupportsRecovery: true, Reason: "BoxLiteResourceEnforcementIncomplete"},
			NetworkMode:        NetworkModeBoxLite,
			InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryTemplateBake, InfraDeliveryPreinstalled, InfraDeliveryArtifactVolume},
		},
		apiv1alpha2.RuntimeFirecracker: withResidualProcess(
			RuntimeProfile{
				Name: apiv1alpha2.RuntimeFirecracker, Version: builtinProfileVersion, Driver: DriverKindFirecracker,
				Firecracker: &FirecrackerConfig{
					BinaryPath: "/usr/local/bin/firecracker", KernelPath: "/opt/fast-sandbox/firecracker/vmlinux.bin",
					RootfsPath: "/var/lib/fast-sandbox/firecracker/rootfs", StateRoot: "/var/lib/fast-sandbox/firecracker",
					DefaultVCPUs: 1, DefaultMemory: "512Mi", BootTimeoutSeconds: 30,
				},
				Deployment: DeploymentRequirements{
					Privileged: true, RequiresKVM: true, Overhead: overhead("250m", "256Mi"),
					HostPaths: append([]HostPathRequirement{
						{Name: "dev-kvm", HostPath: "/dev/kvm", MountPath: "/dev/kvm", Type: corev1.HostPathCharDev},
						{Name: "dev-net-tun", HostPath: "/dev/net/tun", MountPath: "/dev/net/tun", Type: corev1.HostPathCharDev},
						{Name: "firecracker-bin", HostPath: "/usr/local/bin/firecracker", MountPath: "/usr/local/bin/firecracker", Type: corev1.HostPathFile, ReadOnly: true},
						{Name: "firecracker-kernel", HostPath: "/opt/fast-sandbox/firecracker/vmlinux.bin", MountPath: "/opt/fast-sandbox/firecracker/vmlinux.bin", Type: corev1.HostPathFile, ReadOnly: true},
						{Name: "firecracker-rootfs", HostPath: "/var/lib/fast-sandbox/firecracker/rootfs", MountPath: "/var/lib/fast-sandbox/firecracker/rootfs", Type: corev1.HostPathDirectoryOrCreate},
						{Name: "firecracker-state", HostPath: "/var/lib/fast-sandbox/firecracker", MountPath: "/var/lib/fast-sandbox/firecracker", Type: corev1.HostPathDirectoryOrCreate},
					}, linuxNetworkPaths...),
				},
				// The on-demand loading chain is implemented and E2E-verified
				// (builder publish -> agent pull -> golden restore), so the
				// runtime is configured (the pool gate only rejects
				// Unsupported/Degraded); the driver probes node assets at
				// fastlet start.
				Capabilities:       Capabilities{DefaultState: CapabilityConfigured, SupportsNetwork: true, SupportsCache: true, SupportsRecovery: true},
				NetworkMode:        NetworkModeFirecracker,
				InfraDeliveryModes: []InfraDeliveryMode{InfraDeliveryTemplateBake, InfraDeliveryPreinstalled, InfraDeliveryGuestCopy},
			},
			ResidualProcessFirecracker,
		),
	}
}

func withResidualProcess(profile RuntimeProfile, kind ResidualProcessKind) RuntimeProfile {
	profile.ResidualProcess = kind
	return profile
}

func kataProfile(name apiv1alpha2.RuntimeName, configPath string, paths []HostPathRequirement) RuntimeProfile {
	return kataProfileWithRuntime(name, "/opt/kata/bin/containerd-shim-kata-v2", configPath, paths)
}

func kataRustProfile(name apiv1alpha2.RuntimeName, configPath string, paths []HostPathRequirement) RuntimeProfile {
	return kataProfileWithRuntime(name, "/opt/kata/runtime-rs/bin/containerd-shim-kata-v2", configPath, paths)
}

func kataProfileWithRuntime(name apiv1alpha2.RuntimeName, runtimePath, configPath string, paths []HostPathRequirement) RuntimeProfile {
	return RuntimeProfile{
		Name: name, Version: builtinProfileVersion, Driver: DriverKindContainerd,
		Containerd:   &ContainerdConfig{Namespace: DefaultContainerdNamespace, Handler: "io.containerd.kata.v2", RuntimePath: runtimePath, ConfigPath: configPath},
		Deployment:   DeploymentRequirements{Privileged: true, RequiresKVM: true, HostPaths: paths, Overhead: overhead("250m", "256Mi")},
		Capabilities: Capabilities{DefaultState: CapabilityConfigured, SupportsNetwork: true, SupportsCache: true, SupportsRecovery: true},
		NetworkMode:  NetworkModeGuestNetNS,
		InfraDeliveryModes: []InfraDeliveryMode{
			// Kata/containerd carries OCI bind mounts into the guest through
			// its shared filesystem. Quick Start uses this path for immutable
			// Infra bundles; production images can still bake or preinstall
			// the same logical component.
			InfraDeliveryBindMount,
			InfraDeliveryTemplateBake,
			InfraDeliveryPreinstalled,
			InfraDeliveryGuestCopy,
		},
	}
}

func overhead(cpu, memory string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
	}
}
