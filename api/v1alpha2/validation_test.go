package v1alpha2

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestValidateRuntime(t *testing.T) {
	require.NoError(t, (&SandboxPoolSpec{Runtime: RuntimeKataFc}).ValidateRuntime())
	require.NoError(t, (&SandboxPoolSpec{Runtime: RuntimeKataDragonball}).ValidateRuntime())
	require.NoError(t, (&SandboxPoolSpec{Runtime: RuntimeFirecracker}).ValidateRuntime())
	require.Error(t, (&SandboxPoolSpec{}).ValidateRuntime())
	require.Error(t, (&SandboxPoolSpec{Runtime: RuntimeName("unknown")}).ValidateRuntime())
}

func TestValidateSandboxPoolUpdate(t *testing.T) {
	base := SandboxPoolSpec{
		Runtime: RuntimeContainer,
		SandboxResources: SandboxResourceProfile{
			CPU:    resource.MustParse("1"),
			Memory: resource.MustParse("1Gi"),
			PIDs:   256,
		},
	}

	same := *base.DeepCopy()
	require.NoError(t, ValidateSandboxPoolUpdate(&base, &same))

	runtimeChanged := *base.DeepCopy()
	runtimeChanged.Runtime = RuntimeGVisor
	require.ErrorIs(t, ValidateSandboxPoolUpdate(&base, &runtimeChanged), ErrRuntimeImmutable)

	resourcesChanged := *base.DeepCopy()
	resourcesChanged.SandboxResources.Memory = resource.MustParse("2Gi")
	require.ErrorIs(t, ValidateSandboxPoolUpdate(&base, &resourcesChanged), ErrResourcesImmutable)
}

func TestGenerationAndAssignmentValidation(t *testing.T) {
	require.Equal(t, int64(1), NextInstanceGeneration(0))
	require.Equal(t, int64(2), NextInstanceGeneration(1))

	assignment := &SandboxAssignment{
		FastletName: "fastlet-1", FastletPodUID: "pod-uid", Attempt: 1, InfraRevision: "sha256:infra",
	}
	require.NoError(t, assignment.Validate())
	assignment.Attempt = 0
	require.Error(t, assignment.Validate())
}

func TestValidateInfraComponents(t *testing.T) {
	valid := SandboxPoolSpec{InfraComponents: []InfraComponent{{
		Name: "execd",
		Artifact: InfraArtifact{
			Source: InfraArtifactSource{Image: &InfraArtifactImage{
				Reference: "ghcr.io/opensandbox/execd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
			Mappings: []InfraArtifactMapping{{
				SourcePath: "/usr/local/bin/execd", TargetPath: "/.fast/components/execd/execd",
			}},
		},
		Process: InfraProcess{
			Command:     []string{"/.fast/components/execd/execd", "--port", "44772"},
			HealthCheck: InfraHealthCheck{HTTPGet: &InfraHTTPGet{Path: "/ping"}, TimeoutSeconds: 10},
		},
		Endpoint: InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}}}
	require.NoError(t, valid.ValidateInfraComponents())

	duplicatePort := *valid.DeepCopy()
	second := duplicatePort.InfraComponents[0]
	second.Name = "envd"
	second.Artifact.Mappings[0].TargetPath = "/.fast/components/envd/envd"
	duplicatePort.InfraComponents = append(duplicatePort.InfraComponents, second)
	require.ErrorIs(t, duplicatePort.ValidateInfraComponents(), ErrInfraComponentsInvalid)

	escapedTarget := *valid.DeepCopy()
	escapedTarget.InfraComponents[0].Artifact.Mappings[0].TargetPath = "/usr/local/bin/execd"
	require.ErrorIs(t, escapedTarget.ValidateInfraComponents(), ErrInfraComponentsInvalid)

	tokenEnv := *valid.DeepCopy()
	tokenEnv.InfraComponents[0].Process.Env = map[string]string{"FAST_SANDBOX_TOKEN": "bad"}
	require.ErrorIs(t, tokenEnv.ValidateInfraComponents(), ErrInfraComponentsInvalid)
}

func TestValidatePodPorts(t *testing.T) {
	valid := SandboxPoolSpec{PodPorts: []PodPort{
		{Name: "sidecar", Port: 9000},
		{Name: "debug", Port: 9091},
	}}
	require.NoError(t, valid.ValidatePodPorts())

	duplicateName := SandboxPoolSpec{PodPorts: []PodPort{
		{Name: "sidecar", Port: 9000},
		{Name: "sidecar", Port: 9001},
	}}
	require.ErrorIs(t, duplicateName.ValidatePodPorts(), ErrPodPortsInvalid)

	duplicatePort := SandboxPoolSpec{PodPorts: []PodPort{
		{Name: "sidecar", Port: 9000},
		{Name: "debug", Port: 9000},
	}}
	require.ErrorIs(t, duplicatePort.ValidatePodPorts(), ErrPodPortsInvalid)

	reservedPort := SandboxPoolSpec{PodPorts: []PodPort{{Name: "shadow", Port: FastletProxyPort}}}
	require.ErrorIs(t, reservedPort.ValidatePodPorts(), ErrPodPortsInvalid)

	reservedName := SandboxPoolSpec{PodPorts: []PodPort{{Name: "fast-sandbox-ctl", Port: 9000}}}
	require.ErrorIs(t, reservedName.ValidatePodPorts(), ErrPodPortsInvalid)
}

func TestSandboxResourceProfileHashIsCanonical(t *testing.T) {
	a := SandboxResourceProfile{CPU: resource.MustParse("1"), Memory: resource.MustParse("1024Mi"), PIDs: 256}
	b := SandboxResourceProfile{CPU: resource.MustParse("1000m"), Memory: resource.MustParse("1Gi"), PIDs: 256}
	require.Equal(t, a.Hash(), b.Hash())
	b.PIDs++
	require.NotEqual(t, a.Hash(), b.Hash())
}

func TestValidateSandboxResourceProfile(t *testing.T) {
	valid := SandboxResourceProfile{CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"), PIDs: 128}
	require.NoError(t, ValidateSandboxResourceProfile(valid))
	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{}), ErrInvalidSandboxResourceProfile)
	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{
		CPU: resource.MustParse("1"),
	}), ErrInvalidSandboxResourceProfile)
	require.ErrorIs(t, ValidateSandboxResourceProfile(SandboxResourceProfile{
		CPU: resource.MustParse("1m"), Memory: resource.MustParse("256Mi"), PIDs: 128,
	}), ErrInvalidSandboxResourceProfile)
}
