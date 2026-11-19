// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	strictjson "sigs.k8s.io/json"
)

func (h *Handler) validateNode(ctx context.Context, request admission.Request) admission.Response {
	if request.Kind != (metav1.GroupVersionKind{Version: "v1", Kind: "Node"}) ||
		request.Namespace != "" || !validName(request.Name) {
		return reject(ctx, request, invalid("InvalidNodeRequest", "Node lifecycle admission requires one exact cluster-scoped Node"))
	}
	if request.Operation == admissionv1.Update && request.SubResource == "status" {
		return admission.Allowed("Node status does not change placement identity")
	}
	if request.SubResource != "" || (request.Operation != admissionv1.Update && request.Operation != admissionv1.Delete) {
		return reject(ctx, request, denied("UnsupportedRequest", "unsupported Node lifecycle operation"))
	}
	ctx, cancel := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancel()
	state := &requestState{reader: h.reader}
	registry, failure := state.readRegistry(ctx, h.config)
	if failure != nil {
		return reject(ctx, request, failure)
	}
	old := &corev1.Node{}
	if !decodeNode(request.OldObject.Raw, old) || old.Name != request.Name || old.Namespace != "" ||
		old.UID == "" || old.ResourceVersion == "" {
		return reject(ctx, request, invalid("InvalidPreviousNode", "Node lifecycle admission requires an exact persisted previous Node"))
	}
	var updated *corev1.Node
	if request.Operation == admissionv1.Update {
		updated = &corev1.Node{}
		if !decodeNode(request.Object.Raw, updated) || updated.Name != old.Name || updated.Namespace != "" ||
			updated.UID != old.UID || updated.ResourceVersion != old.ResourceVersion {
			return reject(ctx, request, invalid("InvalidNodeUpdate", "Node update must retain its previous name, UID and resource version"))
		}
	}
	if registered := registry.node(request.Name); registered != nil && registered.Reservation != nil {
		// Even a replacement UID must not make the reserved name deletable.
		// There is no controller, administrator or ServiceAccount exception.
		if request.Operation == admissionv1.Delete {
			return reject(ctx, request, denied("ReservedNodeDeletion", "a Node cannot be deleted while its registered reservation is present"))
		}
		current, _, failure := state.readNode(ctx, registry, request.Name)
		if failure != nil {
			return reject(ctx, request, failure)
		}
		if old.UID != current.UID || old.ResourceVersion != current.ResourceVersion ||
			old.Spec.ProviderID != current.Spec.ProviderID || !old.DeletionTimestamp.IsZero() ||
			updated.UID != registered.UID || updated.Spec.ProviderID != registered.ProviderID ||
			!updated.DeletionTimestamp.IsZero() {
			return reject(ctx, request, denied("ReservedNodeIdentityChanged", "reserved Node update differs from its live pinned identity or introduces termination"))
		}
	}
	if failure := state.recheck(ctx); failure != nil {
		return reject(ctx, request, failure)
	}
	// Unknown Node lifecycle operations cannot authorize a placement: Pod
	// CREATE and binding still require the complete explicit Node inventory.
	return admission.Allowed("Node lifecycle request preserves registered reservations; placement checks remain independent")
}

func decodeNode(raw []byte, node *corev1.Node) bool {
	problems, err := strictjson.UnmarshalStrict(raw, node)
	return err == nil && len(problems) == 0 && node.APIVersion == "v1" && node.Kind == "Node"
}
