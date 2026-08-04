package v1alpha2

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

const InitialInstanceGeneration int64 = 1

var (
	ErrRuntimeImmutable       = errors.New("spec.runtime is immutable")
	ErrResourcesImmutable     = errors.New("spec.sandboxResources is immutable")
	ErrInfraComponentsInvalid = errors.New("spec.infraComponents is invalid")
)

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// IsRuntimeName reports whether name identifies a built-in runtime profile.
func IsRuntimeName(name RuntimeName) bool {
	switch name {
	case RuntimeContainer, RuntimeGVisor, RuntimeKataQemu, RuntimeKataClh, RuntimeKataFc, RuntimeKataDragonball, RuntimeBoxLite:
		return true
	default:
		return false
	}
}

// ValidateRuntime verifies that the Pool selects one built-in runtime profile.
func (s *SandboxPoolSpec) ValidateRuntime() error {
	if !IsRuntimeName(s.Runtime) {
		return fmt.Errorf("unsupported runtime %q", s.Runtime)
	}
	return nil
}

// ValidateInfraComponents verifies the cross-field constraints that the CRD
// structural schema cannot express, including component-scoped target paths
// and endpoint uniqueness.
func (s *SandboxPoolSpec) ValidateInfraComponents() error {
	if s == nil {
		return fmt.Errorf("%w: pool spec is required", ErrInfraComponentsInvalid)
	}
	names := make(map[string]struct{}, len(s.InfraComponents))
	ports := make(map[int32]string, len(s.InfraComponents))
	for index := range s.InfraComponents {
		component := &s.InfraComponents[index]
		if problems := k8svalidation.IsDNS1123Label(component.Name); len(problems) != 0 ||
			strings.HasPrefix(component.Name, "fast-sandbox-") {
			return fmt.Errorf("%w: component %d has invalid or reserved name %q", ErrInfraComponentsInvalid, index, component.Name)
		}
		if _, exists := names[component.Name]; exists {
			return fmt.Errorf("%w: duplicate component name %q", ErrInfraComponentsInvalid, component.Name)
		}
		names[component.Name] = struct{}{}

		source := component.Artifact.Source
		if (source.Image == nil) == (source.Archive == nil) {
			return fmt.Errorf("%w: component %s must declare exactly one artifact source", ErrInfraComponentsInvalid, component.Name)
		}
		if source.Image != nil {
			parts := strings.Split(source.Image.Reference, "@sha256:")
			if len(parts) != 2 || parts[0] == "" || !sha256Pattern.MatchString(parts[1]) {
				return fmt.Errorf("%w: component %s image reference must be digest-pinned", ErrInfraComponentsInvalid, component.Name)
			}
		}
		if source.Archive != nil {
			parsed, err := url.Parse(source.Archive.URL)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
				return fmt.Errorf("%w: component %s archive URL must be absolute HTTPS", ErrInfraComponentsInvalid, component.Name)
			}
			if !sha256Pattern.MatchString(source.Archive.SHA256) {
				return fmt.Errorf("%w: component %s archive SHA-256 is invalid", ErrInfraComponentsInvalid, component.Name)
			}
		}
		if len(component.Artifact.Mappings) == 0 {
			return fmt.Errorf("%w: component %s requires at least one artifact mapping", ErrInfraComponentsInvalid, component.Name)
		}
		targetRoot := "/.fast/components/" + component.Name
		targets := make([]string, 0, len(component.Artifact.Mappings))
		for mappingIndex := range component.Artifact.Mappings {
			mapping := &component.Artifact.Mappings[mappingIndex]
			if !cleanAbsolutePath(mapping.SourcePath) {
				return fmt.Errorf("%w: component %s mapping sourcePath %q is not a clean absolute path", ErrInfraComponentsInvalid, component.Name, mapping.SourcePath)
			}
			if !cleanAbsolutePath(mapping.TargetPath) ||
				(mapping.TargetPath != targetRoot && !strings.HasPrefix(mapping.TargetPath, targetRoot+"/")) {
				return fmt.Errorf("%w: component %s mapping targetPath %q must be under %s", ErrInfraComponentsInvalid, component.Name, mapping.TargetPath, targetRoot)
			}
			for _, existing := range targets {
				if pathsOverlap(existing, mapping.TargetPath) {
					return fmt.Errorf("%w: component %s has overlapping target paths %q and %q", ErrInfraComponentsInvalid, component.Name, existing, mapping.TargetPath)
				}
			}
			targets = append(targets, mapping.TargetPath)
		}

		if len(component.Process.Command) == 0 || strings.TrimSpace(component.Process.Command[0]) == "" {
			return fmt.Errorf("%w: component %s process command is required", ErrInfraComponentsInvalid, component.Name)
		}
		for name := range component.Process.Env {
			if strings.HasPrefix(name, "FAST_SANDBOX_") {
				return fmt.Errorf("%w: component %s environment key %q is reserved", ErrInfraComponentsInvalid, component.Name, name)
			}
		}
		switch component.Process.RestartPolicy {
		case "", InfraRestartNever, InfraRestartOnFailure, InfraRestartAlways:
		default:
			return fmt.Errorf("%w: component %s restart policy %q is invalid", ErrInfraComponentsInvalid, component.Name, component.Process.RestartPolicy)
		}
		health := component.Process.HealthCheck
		if (health.HTTPGet == nil) == (health.TCPConnect == nil) {
			return fmt.Errorf("%w: component %s requires exactly one health check", ErrInfraComponentsInvalid, component.Name)
		}
		if health.HTTPGet != nil && !cleanAbsolutePath(health.HTTPGet.Path) {
			return fmt.Errorf("%w: component %s health path %q is invalid", ErrInfraComponentsInvalid, component.Name, health.HTTPGet.Path)
		}
		if health.TimeoutSeconds < 0 || health.TimeoutSeconds > 300 {
			return fmt.Errorf("%w: component %s health timeout must be between 1 and 300 seconds", ErrInfraComponentsInvalid, component.Name)
		}
		if component.Endpoint.Protocol != "HTTP" {
			return fmt.Errorf("%w: component %s endpoint protocol %q is unsupported", ErrInfraComponentsInvalid, component.Name, component.Endpoint.Protocol)
		}
		if component.Endpoint.Port < 1 || component.Endpoint.Port > 65535 {
			return fmt.Errorf("%w: component %s endpoint port is invalid", ErrInfraComponentsInvalid, component.Name)
		}
		if existing, found := ports[component.Endpoint.Port]; found {
			return fmt.Errorf("%w: components %s and %s use duplicate endpoint port %d", ErrInfraComponentsInvalid, existing, component.Name, component.Endpoint.Port)
		}
		ports[component.Endpoint.Port] = component.Name
	}
	return nil
}

func cleanAbsolutePath(value string) bool {
	return strings.HasPrefix(value, "/") && value != "/" && path.Clean(value) == value
}

func pathsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// ValidateSandboxPoolUpdate enforces the immutable scheduling and resource
// boundary shared by admission and reconciliation.
func ValidateSandboxPoolUpdate(oldSpec, newSpec *SandboxPoolSpec) error {
	if err := oldSpec.ValidateRuntime(); err != nil {
		return fmt.Errorf("old pool runtime: %w", err)
	}
	if err := newSpec.ValidateRuntime(); err != nil {
		return fmt.Errorf("new pool runtime: %w", err)
	}
	if err := oldSpec.ValidateInfraComponents(); err != nil {
		return fmt.Errorf("old pool Infra Components: %w", err)
	}
	if err := newSpec.ValidateInfraComponents(); err != nil {
		return fmt.Errorf("new pool Infra Components: %w", err)
	}
	if oldSpec.Runtime != newSpec.Runtime {
		return ErrRuntimeImmutable
	}
	if oldSpec.SandboxResources.CPU.Cmp(newSpec.SandboxResources.CPU) != 0 ||
		oldSpec.SandboxResources.Memory.Cmp(newSpec.SandboxResources.Memory) != 0 ||
		oldSpec.SandboxResources.PIDs != newSpec.SandboxResources.PIDs {
		return ErrResourcesImmutable
	}
	return nil
}

// NextInstanceGeneration advances a generation fence. A newly created Sandbox
// has no status yet, so its first runtime instance starts at generation one.
func NextInstanceGeneration(current int64) int64 {
	if current < InitialInstanceGeneration {
		return InitialInstanceGeneration
	}
	return current + 1
}

// Validate verifies the assignment identity required for fencing.
func (a *SandboxAssignment) Validate() error {
	if a == nil {
		return errors.New("assignment is required")
	}
	if a.FastletName == "" {
		return errors.New("fastletName is required")
	}
	if a.FastletPodUID == "" {
		return errors.New("fastletPodUID is required")
	}
	if a.Attempt < 1 {
		return errors.New("attempt must be at least 1")
	}
	if a.InfraRevision == "" {
		return errors.New("infraRevision is required")
	}
	return nil
}
