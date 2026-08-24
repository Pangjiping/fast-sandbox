package firecracker

import runtimecontract "fast-sandbox/internal/runtime/contract"

type SandboxMetadata = runtimecontract.Metadata
type CapabilityReport = runtimecontract.CapabilityReport
type RuntimeDriver = runtimecontract.Driver
type RuntimeArtifactCache = runtimecontract.ArtifactCache
type RuntimeResourceRecoverer = runtimecontract.ResourceRecoverer
type AccessDescriptorProvider = runtimecontract.AccessDescriptorProvider

var (
	ErrInvalidConfig      = runtimecontract.ErrInvalidConfig
	ErrNetworkUnavailable = runtimecontract.ErrNetworkUnavailable
)

var validateExistingRuntimeProfile = runtimecontract.ValidateProfile
