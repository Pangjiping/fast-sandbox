package runtimeenv

import (
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func DefaultConfig() Config {
	return Config{Environments: map[string]NodeRuntimeEnvironment{
		DefaultEnvironment: {
			Containerd: ContainerdEnvironment{
				Socket: "/run/containerd/containerd.sock", Namespace: "k8s.io",
				Snapshotter: "overlayfs", Root: "/var/lib/containerd",
			},
			Kubelet: KubeletEnvironment{Root: "/var/lib/kubelet"},
			Runtimes: map[apiv1alpha2.RuntimeName]RuntimeBinding{
				apiv1alpha2.RuntimeContainer: {},
				apiv1alpha2.RuntimeGVisor:    {},
				apiv1alpha2.RuntimeKataQemu:  {},
				apiv1alpha2.RuntimeKataClh:   {},
				apiv1alpha2.RuntimeKataFc:    {},
				apiv1alpha2.RuntimeBoxLite:   {},
			},
		},
	}}
}
