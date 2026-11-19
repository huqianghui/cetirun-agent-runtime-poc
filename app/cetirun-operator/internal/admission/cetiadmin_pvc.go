// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (h *CetiAdminHandler) cetiAdminPVCGrowth(ctx context.Context, request admission.Request, agent *api.Agent, target cetiadmin.TargetIdentity) admission.Response {
	response := cetiAdminPVCGrowth(request)
	if !response.Allowed {
		return response
	}
	var previous corev1.PersistentVolumeClaim
	if decodeJSON(request.OldObject.Raw, &previous, false) != nil || previous.Name != request.Name || previous.Namespace != target.Namespace.Name {
		return admission.Denied("PVC expansion requires the target runtime claim")
	}
	current := &corev1.PersistentVolumeClaim{}
	if h.reader.Get(ctx, client.ObjectKeyFromObject(&previous), current) != nil || !matchingObjectIdentity(&previous, current) {
		return admission.Denied("PVC expansion requires the current claim UID and resourceVersion")
	}
	if cetiAdminAgentOwner(current, agent) {
		return response
	}
	workload := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: target.Namespace.Name, Name: "openclaw-" + agent.Name + "-cetirun"}
	if h.reader.Get(ctx, key, workload) != nil || workload.UID == "" || workload.DeletionTimestamp != nil {
		return admission.Denied("PVC expansion cannot confirm the current runtime workload")
	}
	owned := cetiAdminAgentOwner(workload, agent) || (len(workload.OwnerReferences) == 0 &&
		workload.Annotations["meta.helm.sh/release-name"] == target.Release &&
		workload.Annotations["meta.helm.sh/release-namespace"] == target.Namespace.Name &&
		workload.Labels["app.kubernetes.io/managed-by"] == "Helm")
	if !owned {
		return admission.Denied("PVC expansion requires a runtime owned by the current Agent")
	}
	for _, claim := range workload.Spec.VolumeClaimTemplates {
		if claim.Name+"-"+workload.Name+"-0" == current.Name {
			return response
		}
	}
	for _, volume := range workload.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == current.Name {
			return response
		}
	}
	return admission.Denied("PVC expansion requires a claim owned or referenced by the target runtime")
}
