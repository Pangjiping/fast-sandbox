package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuntimeName is the canonical runtime profile selected by a SandboxPool.
// +kubebuilder:validation:Enum=container;gvisor;kata-qemu;kata-clh;kata-fc;kata-dragonball;boxlite
type RuntimeName string

const (
	// RuntimeContainer is the default runc runtime (process-level isolation).
	RuntimeContainer RuntimeName = "container"
	// RuntimeGVisor uses gVisor with runsc (user-space kernel).
	RuntimeGVisor RuntimeName = "gvisor"
	// RuntimeKataQemu uses Kata Containers with QEMU hypervisor.
	RuntimeKataQemu RuntimeName = "kata-qemu"
	// RuntimeKataFc uses Kata Containers with Firecracker microVM.
	RuntimeKataFc RuntimeName = "kata-fc"
	// RuntimeKataClh uses Kata Containers with Cloud Hypervisor.
	RuntimeKataClh RuntimeName = "kata-clh"
	// RuntimeKataDragonball uses the Kata Rust runtime with the Dragonball VMM.
	RuntimeKataDragonball RuntimeName = "kata-dragonball"
	// RuntimeBoxLite uses the BoxLite microVM runtime driver.
	RuntimeBoxLite RuntimeName = "boxlite"
)

// SandboxResourceProfile defines the fixed resource limit for every Sandbox in
// a Pool. Fastlet is the component that enforces these values at runtime.
type SandboxResourceProfile struct {
	CPU    resource.Quantity `json:"cpu"`
	Memory resource.Quantity `json:"memory"`
	// +kubebuilder:validation:Minimum=1
	PIDs int64 `json:"pids"`
}

// InfraRestartPolicy controls process restart after exit.
// +kubebuilder:validation:Enum=Never;OnFailure;Always
type InfraRestartPolicy string

const (
	InfraRestartNever     InfraRestartPolicy = "Never"
	InfraRestartOnFailure InfraRestartPolicy = "OnFailure"
	InfraRestartAlways    InfraRestartPolicy = "Always"
)

// InfraArtifactImage selects an immutable OCI image or index as an artifact
// carrier. Fast Sandbox injects mapped files and never starts this image.
type InfraArtifactImage struct {
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	Reference string `json:"reference"`
}

// InfraArtifactArchive selects one immutable gzip-compressed tar archive.
type InfraArtifactArchive struct {
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
}

// InfraArtifactSource contains exactly one immutable source.
// +kubebuilder:validation:XValidation:rule="has(self.image) != has(self.archive)",message="exactly one of image or archive is required"
type InfraArtifactSource struct {
	Image   *InfraArtifactImage   `json:"image,omitempty"`
	Archive *InfraArtifactArchive `json:"archive,omitempty"`
}

// InfraArtifactMapping maps one final file or directory from the verified
// artifact root to one final path in the Sandbox.
type InfraArtifactMapping struct {
	// +kubebuilder:validation:Pattern=`^/`
	SourcePath string `json:"sourcePath"`
	// +kubebuilder:validation:Pattern=`^/\.fast/components/`
	TargetPath string `json:"targetPath"`
}

// InfraArtifact defines immutable delivery for one component.
type InfraArtifact struct {
	Source InfraArtifactSource `json:"source"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Mappings []InfraArtifactMapping `json:"mappings"`
}

// InfraHTTPGet is an HTTP readiness probe on the component endpoint.
type InfraHTTPGet struct {
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`
}

// InfraTCPConnect selects a TCP connect readiness probe.
type InfraTCPConnect struct{}

// InfraHealthCheck contains exactly one probe kind.
// +kubebuilder:validation:XValidation:rule="has(self.httpGet) != has(self.tcpConnect)",message="exactly one of httpGet or tcpConnect is required"
type InfraHealthCheck struct {
	HTTPGet    *InfraHTTPGet    `json:"httpGet,omitempty"`
	TCPConnect *InfraTCPConnect `json:"tcpConnect,omitempty"`
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// InfraProcess defines the process supervised inside the user Sandbox.
type InfraProcess struct {
	// +kubebuilder:validation:MinItems=1
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	// +kubebuilder:default=OnFailure
	RestartPolicy InfraRestartPolicy `json:"restartPolicy,omitempty"`
	HealthCheck   InfraHealthCheck   `json:"healthCheck"`
}

// InfraEndpoint is the single public endpoint of a component.
type InfraEndpoint struct {
	// +kubebuilder:validation:Enum=HTTP
	Protocol string `json:"protocol"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// InfraComponent injects one artifact-backed managed process with one public
// named endpoint.
type InfraComponent struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name     string        `json:"name"`
	Artifact InfraArtifact `json:"artifact"`
	Process  InfraProcess  `json:"process"`
	Endpoint InfraEndpoint `json:"endpoint"`
}

// SandboxPoolSpec defines the desired state of SandboxPool.
type SandboxPoolSpec struct {
	Capacity PoolCapacity `json:"capacity"`

	// +kubebuilder:validation:Minimum=1
	MaxSandboxesPerPod int32 `json:"maxSandboxesPerPod"`

	// Runtime selects one immutable, platform-owned runtime profile.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runtime is immutable"
	Runtime RuntimeName `json:"runtime"`

	// SandboxResources is the immutable resource profile applied to each
	// Sandbox created from this Pool.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sandboxResources is immutable"
	SandboxResources SandboxResourceProfile `json:"sandboxResources"`

	// WarmImages are asynchronously pulled and protected from ordinary cache GC.
	// Fastlet readiness does not wait for this list to finish warming.
	// +kubebuilder:validation:MaxItems=128
	WarmImages []string `json:"warmImages,omitempty"`

	// InfraComponents are compiled into an immutable revision. Updating this
	// list rolls Fastlets; existing Sandboxes remain on their admitted revision.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	InfraComponents []InfraComponent `json:"infraComponents,omitempty"`

	// FastletTemplate is intentionally preserved as a Kubernetes-native pod
	// template. Fast Sandbox validates the platform-owned fields separately.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	FastletTemplate corev1.PodTemplateSpec `json:"fastletTemplate"`
}

// PoolCapacity describes the sizing policy of the fastlet pool.
type PoolCapacity struct {
	// +kubebuilder:validation:Minimum=0
	PoolMin int32 `json:"poolMin"`
	// +kubebuilder:validation:Minimum=0
	PoolMax int32 `json:"poolMax"`
	// +kubebuilder:validation:Minimum=0
	BufferMin int32 `json:"bufferMin"`
	// +kubebuilder:validation:Minimum=0
	BufferMax int32 `json:"bufferMax"`
}

// SandboxPoolStatus defines the observed state of SandboxPool.
type SandboxPoolStatus struct {
	ObservedGeneration int64                     `json:"observedGeneration,omitempty"`
	CurrentPods        int32                     `json:"currentPods,omitempty"`
	ReadyPods          int32                     `json:"readyPods,omitempty"`
	TotalFastlets      int32                     `json:"totalFastlets,omitempty"`
	IdleFastlets       int32                     `json:"idleFastlets,omitempty"`
	BusyFastlets       int32                     `json:"busyFastlets,omitempty"`
	RuntimeRevision    string                    `json:"runtimeRevision,omitempty"`
	InfraRevision      string                    `json:"infraRevision,omitempty"`
	PreparedFastlets   int32                     `json:"preparedFastlets,omitempty"`
	InfraComponents    []InfraComponentSummary   `json:"infraComponents,omitempty"`
	Registry           RegistryApplicationStatus `json:"registry,omitempty"`
	WarmImages         []WarmImageStatus         `json:"warmImages,omitempty"`
	Conditions         []metav1.Condition        `json:"conditions,omitempty"`
}

// InfraComponentSummary is safe discovery data for a compiled Pool revision.
type InfraComponentSummary struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Port       int32  `json:"port"`
	HealthKind string `json:"healthKind"`
}

// RegistryApplicationStatus reports configuration rollout without exposing
// credentials.
type RegistryApplicationStatus struct {
	TargetGeneration int64  `json:"targetGeneration,omitempty"`
	AppliedFastlets  int32  `json:"appliedFastlets,omitempty"`
	TotalFastlets    int32  `json:"totalFastlets,omitempty"`
	LastError        string `json:"lastError,omitempty"`
}

// WarmImageStatus aggregates cache state for one configured warm image.
type WarmImageStatus struct {
	Image              string `json:"image"`
	DesiredFastlets    int32  `json:"desiredFastlets,omitempty"`
	CachedFastlets     int32  `json:"cachedFastlets,omitempty"`
	PullingFastlets    int32  `json:"pullingFastlets,omitempty"`
	FailedFastlets     int32  `json:"failedFastlets,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	LastError          string `json:"lastError,omitempty"`
}

// Pool condition types
const (
	PoolConditionRuntimeReady  = "RuntimeReady"
	PoolConditionInfraReady    = "InfraReady"
	PoolConditionRegistryReady = "RegistryReady"
)

// Pool condition reasons
const (
	ReasonRuntimeAvailable         = "RuntimeAvailable"
	ReasonRuntimeUnavailable       = "RuntimeUnavailable"
	ReasonRuntimeProfileInvalid    = "RuntimeProfileInvalid"
	ReasonRuntimeCapabilityPending = "RuntimeCapabilityPending"
	ReasonRuntimeUnsupported       = "RuntimeUnsupported"
	ReasonResourceProfileInvalid   = "ResourceProfileInvalid"
	ReasonInfraComponentsInvalid   = "InfraComponentsInvalid"
	ReasonInfraComponentsAvailable = "InfraComponentsAvailable"
	ReasonRegistryInvalid          = "RegistryInvalid"
	ReasonRegistryAvailable        = "RegistryAvailable"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SandboxPool is the Schema for the sandboxpools API.
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxPoolSpec   `json:"spec,omitempty"`
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxPoolList contains a list of SandboxPool.
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
