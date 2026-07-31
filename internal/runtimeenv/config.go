package runtimeenv

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"

	"sigs.k8s.io/yaml"
)

var containerdNamespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func Parse(raw []byte) (Config, error) {
	config := DefaultConfig()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return config, nil
	}
	var override Config
	if err := yaml.UnmarshalStrict(raw, &override); err != nil {
		return Config{}, fmt.Errorf("decode runtime environments: %w", err)
	}
	mergeConfig(&config, override)
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if len(c.Environments) == 0 {
		return errors.New("at least one runtime environment is required")
	}
	owners := make(map[apiv1alpha2.RuntimeName]string)
	for name, environment := range c.Environments {
		if strings.TrimSpace(name) == "" {
			return errors.New("runtime environment name is required")
		}
		if err := validateAbsolute("containerd.socket", environment.Containerd.Socket); err != nil {
			return fmt.Errorf("environment %s: %w", name, err)
		}
		if err := validateAbsolute("containerd.root", environment.Containerd.Root); err != nil {
			return fmt.Errorf("environment %s: %w", name, err)
		}
		if err := validateAbsolute("kubelet.root", environment.Kubelet.Root); err != nil {
			return fmt.Errorf("environment %s: %w", name, err)
		}
		if !containerdNamespacePattern.MatchString(environment.Containerd.Namespace) {
			return fmt.Errorf("environment %s: invalid containerd namespace %q", name, environment.Containerd.Namespace)
		}
		if environment.Containerd.Snapshotter == "" {
			return fmt.Errorf("environment %s: containerd snapshotter is required", name)
		}
		if len(environment.Runtimes) == 0 {
			return fmt.Errorf("environment %s: at least one runtime binding is required", name)
		}
		if err := validateHostPaths(environment.HostPaths); err != nil {
			return fmt.Errorf("environment %s: %w", name, err)
		}
		for runtimeName, binding := range environment.Runtimes {
			if owner, exists := owners[runtimeName]; exists {
				return fmt.Errorf("runtime %s is bound to both %s and %s", runtimeName, owner, name)
			}
			owners[runtimeName] = name
			for _, value := range []struct{ field, path string }{
				{"runtimePath", binding.RuntimePath}, {"configPath", binding.ConfigPath},
			} {
				if value.path != "" && !filepath.IsAbs(value.path) {
					return fmt.Errorf("environment %s runtime %s: %s must be absolute", name, runtimeName, value.field)
				}
			}
			if err := validateHostPaths(binding.HostPaths); err != nil {
				return fmt.Errorf("environment %s runtime %s: %w", name, runtimeName, err)
			}
		}
	}
	return nil
}

func (c Config) ContainerdNamespaces() []string {
	seen := make(map[string]struct{})
	for _, environment := range c.Environments {
		seen[environment.Containerd.Namespace] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for namespace := range seen {
		result = append(result, namespace)
	}
	sort.Strings(result)
	return result
}

func (c Config) ContainerdEndpoints() []ContainerdEndpoint {
	bySocket := make(map[string]map[string]struct{})
	for _, environment := range c.Environments {
		if bySocket[environment.Containerd.Socket] == nil {
			bySocket[environment.Containerd.Socket] = make(map[string]struct{})
		}
		bySocket[environment.Containerd.Socket][environment.Containerd.Namespace] = struct{}{}
	}
	sockets := make([]string, 0, len(bySocket))
	for socket := range bySocket {
		sockets = append(sockets, socket)
	}
	sort.Strings(sockets)
	result := make([]ContainerdEndpoint, 0, len(sockets))
	for _, socket := range sockets {
		namespaces := make([]string, 0, len(bySocket[socket]))
		for namespace := range bySocket[socket] {
			namespaces = append(namespaces, namespace)
		}
		sort.Strings(namespaces)
		result = append(result, ContainerdEndpoint{Socket: socket, Namespaces: namespaces})
	}
	return result
}

func mergeConfig(base *Config, override Config) {
	if base.Environments == nil {
		base.Environments = make(map[string]NodeRuntimeEnvironment)
	}
	for name, incoming := range override.Environments {
		current, exists := base.Environments[name]
		if !exists {
			current = inheritedEnvironment(base.Environments[DefaultEnvironment])
		}
		for runtimeName := range incoming.Runtimes {
			for environmentName, environment := range base.Environments {
				if environmentName == name {
					continue
				}
				delete(environment.Runtimes, runtimeName)
				base.Environments[environmentName] = environment
			}
		}
		mergeEnvironment(&current, incoming)
		base.Environments[name] = current
	}
	for name, environment := range base.Environments {
		if len(environment.Runtimes) == 0 {
			delete(base.Environments, name)
		}
	}
}

func inheritedEnvironment(source NodeRuntimeEnvironment) NodeRuntimeEnvironment {
	return NodeRuntimeEnvironment{
		Containerd: source.Containerd,
		Kubelet:    source.Kubelet,
		Runtimes:   make(map[apiv1alpha2.RuntimeName]RuntimeBinding),
	}
}

func mergeEnvironment(base *NodeRuntimeEnvironment, incoming NodeRuntimeEnvironment) {
	if incoming.Containerd.Socket != "" {
		base.Containerd.Socket = incoming.Containerd.Socket
	}
	if incoming.Containerd.Namespace != "" {
		base.Containerd.Namespace = incoming.Containerd.Namespace
	}
	if incoming.Containerd.Snapshotter != "" {
		base.Containerd.Snapshotter = incoming.Containerd.Snapshotter
	}
	if incoming.Containerd.Root != "" {
		base.Containerd.Root = incoming.Containerd.Root
	}
	if incoming.Kubelet.Root != "" {
		base.Kubelet.Root = incoming.Kubelet.Root
	}
	if base.NodeSelector == nil {
		base.NodeSelector = make(map[string]string)
	}
	for key, value := range incoming.NodeSelector {
		base.NodeSelector[key] = value
	}
	base.HostPaths = append(base.HostPaths, incoming.HostPaths...)
	if base.Runtimes == nil {
		base.Runtimes = make(map[apiv1alpha2.RuntimeName]RuntimeBinding)
	}
	for name, binding := range incoming.Runtimes {
		base.Runtimes[name] = binding
	}
}

func validateAbsolute(field, value string) error {
	if value == "" || !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path", field)
	}
	return nil
}

func validateHostPaths(paths []runtimecatalog.HostPathRequirement) error {
	for _, requirement := range paths {
		if requirement.Name == "" || !filepath.IsAbs(requirement.HostPath) || !filepath.IsAbs(requirement.MountPath) {
			return fmt.Errorf("hostPaths require a name and absolute hostPath/mountPath")
		}
	}
	return nil
}
