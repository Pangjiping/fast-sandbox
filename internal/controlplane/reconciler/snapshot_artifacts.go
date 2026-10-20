package reconciler

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/ocitemplate"
	"fast-sandbox/internal/registryconfig"
)

// Remote cleanup belongs to the controller, not to the source VM's Fastlet:
// the source and its node may already be gone when the snapshot is deleted.
func (r *SandboxSnapshotReconciler) deletePublishedOCISnapshot(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot) error {
	if !strings.HasPrefix(snapshot.Status.ManifestRef, ocitemplate.Prefix) {
		return nil
	}
	if err := ocitemplate.ValidateDigestReference(snapshot.Status.ManifestRef, snapshot.Status.ArtifactDigest); err != nil {
		return err
	}
	pool := snapshot.Annotations[assignment.AnnotationSnapshotPoolRef]
	if pool == "" {
		return fmt.Errorf("snapshot credential Pool reference is missing")
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: snapshot.Namespace, Name: registrySecretName(pool)}, &secret); err != nil {
		return err
	}
	compiled, err := registryconfig.ParseCompiled(secret.Data[registryconfig.SecretKey])
	if err != nil {
		return err
	}
	credentials := func(_ context.Context, reference string) (auth.Credential, error) {
		value, _ := compiled.Match(reference)
		if value.WriteUsername != "" {
			value.Username, value.Password, value.IdentityToken = value.WriteUsername, value.WritePassword, ""
		}
		return ocitemplate.BasicCredential(value.Username, value.Password, value.IdentityToken)
	}
	return ocitemplate.DeletePair(ctx, snapshot.Status.ManifestRef, snapshot.Status.SnapshotID, credentials)
}
