package fixtures

import apiv1alpha2 "fast-sandbox/api/v1alpha2"

const OpenSandboxExecdImage = "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd@sha256:1dc98c7de10b9a73450ac75aa0f200ad7972f2c40f5225f6a8998e166b45d6dd"

// OpenSandboxExecdComponent is the common inline component used by Quick
// Start and E2E. The Execd application token is deliberately disabled;
// Fast Sandbox route credentials protect external proxy access.
func OpenSandboxExecdComponent() apiv1alpha2.InfraComponent {
	return apiv1alpha2.InfraComponent{
		Name: "execd",
		Artifact: &apiv1alpha2.InfraArtifact{
			Source: apiv1alpha2.InfraArtifactSource{Image: &apiv1alpha2.InfraArtifactImage{
				Reference: OpenSandboxExecdImage,
			}},
			Mappings: []apiv1alpha2.InfraArtifactMapping{{
				SourcePath: "/execd", TargetPath: "/.fast/components/execd/execd",
			}},
		},
		Process: apiv1alpha2.InfraProcess{
			Command:       []string{"/.fast/components/execd/execd"},
			RestartPolicy: apiv1alpha2.InfraRestartOnFailure,
			HealthCheck: apiv1alpha2.InfraHealthCheck{
				HTTPGet: &apiv1alpha2.InfraHTTPGet{Path: "/ping"}, TimeoutSeconds: 10,
			},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}
}
