package ocitemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	AnnotationTemplateID   = "io.fast-sandbox.template.id"
	AnnotationMetadata     = "io.fast-sandbox.metadata"
	AnnotationSandboxUID   = "io.fast-sandbox.sandbox.uid"
	AnnotationNamespace    = "io.fast-sandbox.sandbox.namespace"
	AnnotationCheckpointID = "io.fast-sandbox.checkpoint.id"
)

// validatePairID validates both derived tag names, including the registry tag length
// limit. It is a tag component, never a repository path or a digest.
func validatePairID(id string) error {
	for _, suffix := range []string{"_rootfs", "_snapfiles"} {
		ref := registry.Reference{Reference: id + suffix}
		if id == "" || ref.ValidateReferenceAsTag() != nil {
			return fmt.Errorf("invalid template pair ID %q", id)
		}
	}
	return nil
}

func pairAnnotations(id string, metadata []byte) (map[string]string, error) {
	if err := validatePairID(id); err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("template metadata must be a JSON object")
	}
	annotations := map[string]string{AnnotationTemplateID: id}
	if raw, ok := doc["checkpoint"]; ok {
		var owner struct {
			SandboxUID string `json:"sandboxUID"`
			Namespace  string `json:"namespace"`
		}
		if err := json.Unmarshal(raw, &owner); err != nil {
			return nil, err
		}
		if strings.TrimSpace(owner.SandboxUID) == "" || strings.TrimSpace(owner.Namespace) == "" {
			return nil, fmt.Errorf("checkpoint requires sandbox UID and namespace")
		}
		annotations[AnnotationSandboxUID], annotations[AnnotationNamespace], annotations[AnnotationCheckpointID] = owner.SandboxUID, owner.Namespace, id
	}
	// Pair references are derived at resolution time. Owner fields have a single
	// authority on the published manifest, outside the platform metadata JSON.
	delete(doc, "oci")
	delete(doc, "checkpoint")
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	annotations[AnnotationMetadata] = string(raw)
	return annotations, nil
}

func resolvePair(ctx context.Context, repo MetadataRepository, ref registry.Reference, snap oci.Descriptor, manifest oci.Manifest) ([]byte, error) {
	annotations := manifest.Annotations
	id := annotations[AnnotationTemplateID]
	if err := validatePairID(id); err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(annotations[AnnotationMetadata]), &doc); err != nil {
		return nil, fmt.Errorf("invalid snapfiles metadata: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("snapfiles metadata must be a JSON object")
	}
	// Untrusted embedded fields must never override owner annotations or the
	// derived pair. Never infer a checkpoint owner from the tag text alone.
	delete(doc, "checkpoint")
	uid, namespace, checkpoint := annotations[AnnotationSandboxUID], annotations[AnnotationNamespace], annotations[AnnotationCheckpointID]
	if uid != "" || namespace != "" || checkpoint != "" {
		if strings.TrimSpace(uid) == "" || strings.TrimSpace(namespace) == "" || checkpoint != id {
			return nil, fmt.Errorf("incomplete or mismatched checkpoint owner annotations")
		}
		raw, err := json.Marshal(map[string]string{"sandboxUID": uid, "namespace": namespace, "id": checkpoint})
		if err != nil {
			return nil, err
		}
		doc["checkpoint"] = raw
	}
	root, err := repo.Resolve(ctx, id+"_rootfs")
	if err != nil {
		return nil, err
	}
	if root.MediaType != oci.MediaTypeImageManifest {
		return nil, fmt.Errorf("unexpected rootfs manifest media type %q", root.MediaType)
	}
	repository := ref.Registry + "/" + ref.Repository
	pair := Pair{Rootfs: repository + "@" + root.Digest.String(), Snapfiles: repository + "@" + snap.Digest.String()}
	raw, err := json.Marshal(pair)
	if err != nil {
		return nil, err
	}
	doc["oci"] = raw
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, err = Decode(payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// IDs are write-once. Identical retries are harmless, but conflicting content
// must not change one half of an already published pair. Registry-side tag
// immutability is still needed against concurrent writers and other clients.
func pushImmutableManifest(ctx context.Context, repo *remote.Repository, tag string, desc oci.Descriptor, raw []byte) error {
	existing, err := repo.Resolve(ctx, tag)
	if err == nil {
		if existing.Digest != desc.Digest {
			return fmt.Errorf("refusing to overwrite published template tag %q", tag)
		}
		return nil
	}
	if !errors.Is(err, errdef.ErrNotFound) {
		return err
	}
	if err = repo.Manifests().PushReference(ctx, desc, bytes.NewReader(raw), tag); err != nil {
		return err
	}
	got, err := repo.Resolve(ctx, tag)
	if err != nil {
		return err
	}
	if got.Digest != desc.Digest {
		return fmt.Errorf("remote template digest mismatch for %q", tag)
	}
	return nil
}
