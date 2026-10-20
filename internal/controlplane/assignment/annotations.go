package assignment

const (
	// SnapshotCleanupFinalizer protects published OCI artifacts until deletion succeeds.
	SnapshotCleanupFinalizer = "sandbox.fast.io/snapshot-cleanup"
	// AnnotationSnapshotPoolRef pins the credential scope independently of the source Sandbox.
	AnnotationSnapshotPoolRef = "sandbox.fast.io/snapshot-pool-ref"
	// LabelCreatedBy records which component created the sandbox.
	LabelCreatedBy = "sandbox.fast.io/created-by"
	// AnnotationRequestID stores the FastPath Create idempotency key.
	AnnotationRequestID = "sandbox.fast.io/request-id"
	// AnnotationCreateSpecHash binds a request ID to its immutable create intent.
	AnnotationCreateSpecHash = "sandbox.fast.io/create-spec-hash"
)
