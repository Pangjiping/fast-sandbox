package ocitemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
)

// DeletePair deletes only the two manifests owned by id, never their blobs.
// Rootfs manifests carry the pair ID too, so identical layer sets in different
// snapshots have distinct manifest digests and can be deleted independently.
// The snapfiles publication point is deleted last, making partial deletion
// retryable using the same immutable reference and ID.
func DeletePair(ctx context.Context, reference, id string, credentials CredentialFunc) error {
	if err := validatePairID(id); err != nil {
		return err
	}
	repo, ref, err := Repository(reference, credentials)
	if err != nil {
		return err
	}
	if err := ValidateDigestReference(reference, strings.TrimPrefix(ref.Reference, "sha256:")); err != nil {
		return err
	}
	var targets []oci.Descriptor
	// Validate both objects before the first deletion. A mismatched tag or
	// annotation must never delete one half of another snapshot.
	for _, name := range []string{id + "_rootfs", ref.Reference} {
		desc, err := repo.Resolve(ctx, name)
		if errors.Is(err, errdef.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		raw, err := fetchSmall(ctx, repo, desc)
		if err != nil {
			return err
		}
		var manifest oci.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		if manifest.Annotations[AnnotationTemplateID] != id {
			return fmt.Errorf("refusing to delete manifest %s: snapshot pair ID mismatch", desc.Digest)
		}
		targets = append(targets, desc)
	}
	for _, target := range targets {
		if err := repo.Manifests().Delete(ctx, target); err != nil && !errors.Is(err, errdef.ErrNotFound) {
			return err
		}
	}
	return nil
}
