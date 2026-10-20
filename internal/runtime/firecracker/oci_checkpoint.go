package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"

	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

const ociSnapshotRepositoryEnv = "FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY"

func checkpointDestination(repository, id string) (string, error) {
	repository = strings.TrimPrefix(strings.TrimSpace(repository), ocitemplate.Prefix)
	ref, err := registry.ParseReference(repository)
	if err != nil || ref.Reference != "" {
		return "", fmt.Errorf("%w: %s must be a registry/repository without a tag or digest", ErrInvalidConfig, ociSnapshotRepositoryEnv)
	}
	return ref.Registry + "/" + ref.Repository + ":" + id, nil
}

func checkpointDestinationForSource(source, id string) (string, error) {
	repository := strings.TrimSpace(os.Getenv(ociSnapshotRepositoryEnv))
	if repository == "" {
		ref, err := registry.ParseReference(strings.TrimPrefix(source, ocitemplate.Prefix))
		if err != nil {
			return "", err
		}
		repository = ref.Registry + "/" + ref.Repository
	}
	return checkpointDestination(repository, id)
}
func checkpointCredential(reference string) (registryconfig.Credential, error) {
	path := os.Getenv("FAST_SANDBOX_REGISTRY_CONFIG_PATH")
	if path == "" {
		path = registryconfig.MountPath
	}
	credential, found, err := registryconfig.NewFileProvider(path).Credentials(reference)
	if err != nil {
		return credential, err
	}
	if !found {
		return credential, fmt.Errorf("no registry credential configured for the checkpoint destination")
	}
	return credential, nil
}

// A repository may have separate read and write accounts. Only the destination
// receives its write credential; inherited parent blobs use their own read rule.
func checkpointCredentials(destination string) (ocitemplate.CredentialFunc, error) {
	credential, err := checkpointCredential(destination)
	if err != nil {
		return nil, err
	}
	if credential.WriteUsername != "" {
		credential.Username, credential.Password = credential.WriteUsername, credential.WritePassword
		credential.IdentityToken = ""
	}
	if _, err := ocitemplate.BasicCredential(credential.Username, credential.Password, credential.IdentityToken); err != nil {
		return nil, err
	}
	target, err := registry.ParseReference(destination)
	if err != nil {
		return nil, err
	}
	return func(_ context.Context, reference string) (auth.Credential, error) {
		ref, err := registry.ParseReference(reference)
		if err != nil {
			return auth.EmptyCredential, err
		}
		value := credential
		if ref.Registry != target.Registry || ref.Repository != target.Repository {
			path := os.Getenv("FAST_SANDBOX_REGISTRY_CONFIG_PATH")
			if path == "" {
				path = registryconfig.MountPath
			}
			value, _, err = registryconfig.NewFileProvider(path).Credentials(reference)
		}
		if err != nil {
			return auth.EmptyCredential, err
		}
		return ocitemplate.BasicCredential(value.Username, value.Password, value.IdentityToken)
	}, nil
}

// publishOCISnapshot runs inside the existing asynchronous snapshot worker.
// Success means the snapfiles manifest publication point is visible in ACR.
func (d *Driver) publishOCISnapshot(ctx context.Context, plan dumpPlan, sizeBytes int64, kind fastletapi.SnapshotKind, onPublishing func()) (*runtimecontract.SnapshotResult, error) {
	defer func() {
		if _, err := os.Stat(filepath.Join(plan.staging, ".pack-resources-active")); err == nil {
			klog.ErrorS(fmt.Errorf("pack resources may still be active"), "checkpoint backing files retained for recovery", "staging", plan.staging)
			return
		}
		_ = os.RemoveAll(plan.staging)
	}()
	state, err := loadState(plan.sandboxDir)
	if err != nil {
		return nil, err
	}
	sourceRef, err := restoreReference(state.Config.Spec)
	if err != nil {
		return nil, err
	}
	destination, err := checkpointDestinationForSource(sourceRef, plan.snapshotID)
	if err != nil {
		return nil, err
	}
	credentials, err := checkpointCredentials(destination)
	if err != nil {
		return nil, err
	}
	// Preserve fsb-vm's machine/device contract in the snapfiles filesystem.
	source := filepath.Join(d.config.StateRoot, "oci/sandboxes", plan.sandboxID, "mnt/metadata.json")
	if err = copyFile(source, filepath.Join(plan.staging, "metadata.json")); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(plan.staging, snapshotManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	raw, err = ociSnapshotMetadata(raw, kind, state.Config.Identity)
	if err != nil {
		return nil, err
	}
	// Sanitize before packing too: a reusable template must not retain an
	// inherited checkpoint owner inside snapfiles/manifest.json.
	if err = os.WriteFile(manifestPath, raw, 0644); err != nil { //nolint:gosec // published metadata, world-readable by design
		return nil, err
	}
	args := append(d.fsbArgs(), "--build-work-dir", filepath.Join(plan.staging, "pack-work"), "--build-output-dir", filepath.Join(plan.staging, "artifacts"), "pack", "--bundle-dir", plan.staging, "--output", "snapshot:"+plan.snapshotID)
	if output, err := exec.CommandContext(ctx, "fsb-vm", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("package OCI snapshot: %w: %s", err, output)
	}
	if onPublishing != nil {
		onPublishing()
	}
	ref, _, err := ocitemplate.Push(ctx, filepath.Join(plan.staging, "artifacts", "snapshot_"+plan.snapshotID), destination, raw, credentials)
	if err != nil {
		return nil, fmt.Errorf("publish OCI snapshot: %w", err)
	}
	parsed, err := registry.ParseReference(strings.TrimPrefix(ref, ocitemplate.Prefix))
	if err != nil {
		return nil, err
	}
	return &runtimecontract.SnapshotResult{SnapshotID: plan.snapshotID, ManifestRef: ref, ArtifactDigest: strings.TrimPrefix(parsed.Reference, "sha256:"), SizeBytes: sizeBytes}, nil
}

func ociSnapshotMetadata(raw []byte, kind fastletapi.SnapshotKind, owner fastletapi.SandboxIdentity) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, fmt.Errorf("%w: snapshot metadata must be an object", ErrInvalidConfig)
	}
	delete(document, "oci")
	delete(document, "checkpoint")
	document["format"] = "overlaybd"
	if kind == fastletapi.SnapshotKindCheckpoint {
		document["checkpoint"] = map[string]string{"sandboxUID": owner.SandboxUID, "namespace": owner.Namespace}
	}
	return json.Marshal(document)
}

// Checkpoints are instance-private even though their bytes use an OCI envelope.
// They cannot be used as golden templates to boot a different sandbox.
func validateOCIResumeOwner(stateRoot, reference string, config *fastletapi.RuntimeSandboxConfig) error {
	if !strings.HasPrefix(reference, ocitemplate.Prefix) {
		return nil
	}
	raw, err := os.ReadFile(cachedManifestPath(stateRoot, reference))
	if err != nil {
		return err
	}
	var document struct {
		Checkpoint *struct {
			SandboxUID string `json:"sandboxUID"`
			Namespace  string `json:"namespace"`
		} `json:"checkpoint"`
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		return err
	}
	if config.Spec.Restore != nil && document.Checkpoint == nil {
		return fmt.Errorf("%w: OCI resume reference is not a checkpoint", ErrInvalidConfig)
	}
	if document.Checkpoint == nil {
		return nil
	}
	if config.Spec.Restore == nil || document.Checkpoint.SandboxUID != config.Identity.SandboxUID || document.Checkpoint.Namespace != config.Identity.Namespace {
		return fmt.Errorf("%w: OCI checkpoint belongs to a different sandbox", ErrInvalidConfig)
	}
	return nil
}

func prepareOCISnapshot(plan dumpPlan) (bool, error) {
	state, err := loadState(plan.sandboxDir)
	if err != nil {
		return false, err
	}
	source, err := restoreReference(state.Config.Spec)
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(source, ocitemplate.Prefix) {
		return false, nil
	}
	if err := validateOCISupport(source); err != nil {
		return false, err
	}
	_, err = checkpointDestinationForSource(source, plan.snapshotID)
	return true, err
}

// cleanupSnapshotStaging must release pack mounts and devices before deleting their backing files.
func (d *Driver) cleanupSnapshotStaging(ctx context.Context, staging string) error {
	if _, err := os.Stat(filepath.Join(staging, ".pack-resources-active")); err == nil {
		args := append(d.fsbArgs(), "--build-work-dir", filepath.Join(staging, "pack-work"), "cleanup-pack", "--bundle-dir", staging)
		if output, err := exec.CommandContext(ctx, "fsb-vm", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("recover checkpoint pack resources: %w: %s", err, output)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(staging)
}
