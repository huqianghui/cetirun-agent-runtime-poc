// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"errors"
	"net/http"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (h *Handler) validateCoordinated(ctx context.Context, request admission.Request) admission.Response {
	response := h.validate(ctx, request)
	if !response.Allowed || h.coordination == nil || request.SubResource != "" ||
		(request.Operation != admissionv1.Update && request.Operation != admissionv1.Delete) {
		return response
	}
	switch request.Resource.Resource {
	case "teams", "sandboxes", "agentpolicies":
	default:
		return response
	}
	object, previous, err := decodeRequest(request, ValidationStrict)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if request.Operation == admissionv1.Delete {
		var options metav1.DeleteOptions
		if err := decodeJSON(request.Options.Raw, &options, true); err != nil {
			return admission.Denied("CoordinationRequired: deletion requires explicit UID/resourceVersion preconditions and background propagation")
		}
		if err := h.coordination.AuthorizeConfigurationDeletion(ctx, previous, options); err != nil {
			if errors.Is(err, sandboxcoordination.ErrConflict) {
				return admission.Denied("CoordinationRequired: configuration deletion requires the exact submitted delete reservation; use sandbox-config-change")
			}
			return dependencyError(ctx, err)
		}
		return response
	}
	if garbageCollectorFinalizerUpdate(request, object, previous) {
		return response
	}
	if !previous.GetDeletionTimestamp().IsZero() {
		if !rawMetadataOnly(request) {
			return admission.Denied("CoordinationRequired: terminating configuration may only remove finalizers")
		}
		if err := h.coordination.AuthorizeConfigurationFinalization(ctx, previous, object); err != nil {
			if errors.Is(err, sandboxcoordination.ErrConflict) {
				return admission.Denied("CoordinationRequired: finalizer removal requires unchanged configuration and no outstanding operations")
			}
			return dependencyError(ctx, err)
		}
		return response
	}
	if err := h.coordination.AuthorizeConfigurationUpdate(ctx, previous, object); err != nil {
		if errors.Is(err, sandboxcoordination.ErrConflict) {
			return admission.Denied("CoordinationRequired: configuration updates require the exact submitted change reservation; use sandbox-config-change")
		}
		return dependencyError(ctx, err)
	}
	return response
}
