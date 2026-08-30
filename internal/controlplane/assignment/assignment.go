package assignment

import (
	"context"
	"encoding/json"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProjectAssignmentToStatus copies the durable assignment annotation into the
// status subresource. The annotation remains authoritative and is revalidated
// on every retry so a concurrent reassignment cannot be projected as stale
// status.
func ProjectAssignmentToStatus(
	ctx context.Context,
	k8sClient client.Client,
	key types.NamespacedName,
) (*apiv1alpha2.Sandbox, error) {
	var result *apiv1alpha2.Sandbox
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current apiv1alpha2.Sandbox
		if err := k8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		envelope, err := AssignmentFromAnnotation(&current)
		if err != nil {
			return err
		}
		if envelope == nil {
			return ErrAssignmentAnnotationMissing
		}
		placement := envelope.StatusPlacement()
		if placementsEqual(current.Status.Placement, placement) &&
			current.Status.Runtime.Generation == envelope.InstanceGeneration &&
			current.Status.DataPlane.RouteGeneration == envelope.RouteGeneration {
			result = current.DeepCopy()
			return nil
		}
		patchBody, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": current.ResourceVersion},
			"status": map[string]any{
				"placement": placement,
				"runtime":   map[string]any{"generation": envelope.InstanceGeneration},
				"dataPlane": map[string]any{"routeGeneration": envelope.RouteGeneration},
			},
		})
		if err != nil {
			return err
		}
		patchTarget := &apiv1alpha2.Sandbox{}
		patchTarget.Namespace, patchTarget.Name = key.Namespace, key.Name
		if err := k8sClient.Status().Patch(ctx, patchTarget, client.RawPatch(types.MergePatchType, patchBody)); err != nil {
			return err
		}
		if err := k8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		if _, err := EffectiveAssignment(&current); err != nil {
			return err
		}
		result = current.DeepCopy()
		return nil
	})
	return result, err
}

func placementsEqual(a, b apiv1alpha2.PlacementStatus) bool {
	return a.FastletName == b.FastletName &&
		a.FastletPodUID == b.FastletPodUID &&
		a.Attempt == b.Attempt
}
