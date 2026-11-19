// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// AdminSandboxHandler manages reusable Sandbox configuration, not runtime installation.
// Its routes require the same administrator middleware as Team and Policy routes.
type AdminSandboxHandler struct {
	Kube   *kube.Client
	Config config.Config
}

type SandboxDTO struct {
	Generation         int64              `json:"generation"`
	ObservedGeneration int64              `json:"observedGeneration"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	Name               string             `json:"name"`
	UID                string             `json:"uid"`
	ResourceVersion    string             `json:"resourceVersion"`
	api.SandboxSpec
	Ready    string `json:"ready"`
	Deleting bool   `json:"deleting"`
}

type SandboxUpsertRequest struct {
	Name            string `json:"name,omitempty"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	api.SandboxSpec
}

func toSandboxDTO(box *api.Sandbox) SandboxDTO {
	dto := SandboxDTO{Name: box.Name, UID: string(box.UID), ResourceVersion: box.ResourceVersion,
		Generation: box.Generation, ObservedGeneration: box.Status.ObservedGeneration,
		Conditions:  append([]metav1.Condition(nil), box.Status.Conditions...),
		SandboxSpec: *box.Spec.DeepCopy(), Ready: "Unknown", Deleting: !box.DeletionTimestamp.IsZero()}
	for _, condition := range box.Status.Conditions {
		if condition.Type == "Ready" && condition.ObservedGeneration == box.Generation {
			dto.Ready = string(condition.Status)
		}
	}
	return dto
}

func validateSandboxSpec(spec api.SandboxSpec) string {
	if spec.Type != api.SandboxKata && spec.Type != api.SandboxGVisor && spec.Type != api.SandboxContainer {
		return "type must be kata, gvisor or container"
	}
	for _, container := range []struct {
		name      string
		resources api.SandboxContainerResources
	}{
		{"agent", spec.Resources.Agent}, {"helpers", spec.Resources.Helpers},
	} {
		if len(container.resources.Requests) != 3 || len(container.resources.Limits) != 3 {
			return "resources." + container.name + " requests and limits must contain exactly cpu, memory and ephemeral-storage"
		}
		for _, key := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			request, limit := container.resources.Requests[key], container.resources.Limits[key]
			if request.Sign() <= 0 || limit.Sign() <= 0 {
				return fmt.Sprintf("resources.%s %s request and limit must be positive quantities", container.name, key)
			}
			if request.Cmp(limit) > 0 {
				return fmt.Sprintf("resources.%s %s request must not exceed its limit", container.name, key)
			}
		}
	}
	if len(validation.IsDNS1123Subdomain(spec.Storage.Home.StorageClassName)) != 0 {
		return "storage.home.storageClassName must name a StorageClass"
	}
	if spec.Storage.Home.Size.Sign() <= 0 {
		return "storage.home.size must be positive"
	}
	if spec.Storage.Home.AccessMode != corev1.ReadWriteOnce && spec.Storage.Home.AccessMode != corev1.ReadWriteOncePod {
		return "storage.home.accessMode must be ReadWriteOnce or ReadWriteOncePod"
	}
	if spec.Storage.Scratch.SizeLimit.Sign() <= 0 {
		return "storage.scratch.sizeLimit must be positive"
	}
	return ""
}

func configurationVersion(uid, resourceVersion string) (kube.ConfigurationVersion, bool) {
	return kube.ConfigurationVersion{UID: types.UID(uid), ResourceVersion: resourceVersion}, uid != "" && resourceVersion != ""
}

func writeConfigurationError(w http.ResponseWriter, operation, kind, name string, err error) {
	if writeCompatibilityError(w, err) {
		return
	}
	switch {
	case kube.IsConfigurationCoordinationUnavailable(err):
		writeError(w, http.StatusServiceUnavailable, "configuration_change_unavailable", "Configuration coordination is unavailable or the write outcome is uncertain; recover the recorded operation before resubmitting")
	case kube.IsConfigurationCoordinationConflict(err):
		writeError(w, http.StatusConflict, "configuration_change_conflict", "Another coordinated configuration change or governed execution is in progress; retry after it completes")
	case errors.Is(err, kube.ErrConfigurationChanged), apierrors.IsConflict(err):
		writeError(w, http.StatusConflict, "configuration_changed", kube.ErrConfigurationChanged.Error())
	case kube.IsNotFound(err):
		writeError(w, http.StatusNotFound, "configuration_not_found", kind+" "+name+" not found")
	case kube.IsAlreadyExists(err):
		writeError(w, http.StatusConflict, "configuration_exists", kind+" "+name+" already exists")
	case apierrors.IsInvalid(err) && kind == "Team":
		slog.Error("admin: team configuration rejected", "operation", operation, "name", name, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "invalid_team", teamValidationMessage(err))
	case kube.IsForbidden(err) && kind == "Team" && teamAdmissionRejected(err):
		slog.Error("admin: team admission rejected", "operation", operation, "name", name, "error", err)
		writeError(w, http.StatusForbidden, "admission_rejected", "Platform admission denied the Team operation. Check the selected Policy and Sandbox prerequisites and Portal logs; changing ClusterRole bindings may not resolve this rejection.")
	case kube.IsForbidden(err) && kind == "Team":
		slog.Error("admin: team operation forbidden", "operation", operation, "name", name, "error", err)
		writeError(w, http.StatusForbidden, "rbac_forbidden", "Portal service account is not allowed to "+operation+" teams. Check ClusterRole bindings.")
	case apierrors.IsInvalid(err), kube.IsForbidden(err):
		// Kubernetes validation and admission messages describe why this specific
		// configuration was rejected. Transport and other internal errors stay in logs.
		status := http.StatusUnprocessableEntity
		if kube.IsForbidden(err) {
			status = http.StatusForbidden
		}
		writeError(w, status, "configuration_rejected", err.Error())
	default:
		slog.Error("admin: configuration operation failed", "operation", operation, "kind", kind, "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Unable to "+operation+" "+strings.ToLower(kind))
	}
}

func (h *AdminSandboxHandler) ListSandboxes(w http.ResponseWriter, r *http.Request) {
	boxes, err := h.Kube.ListSandboxes(r.Context())
	if err != nil {
		writeConfigurationError(w, "list", "Sandbox", "", err)
		return
	}
	out := make([]SandboxDTO, 0, len(boxes))
	for i := range boxes {
		out = append(out, toSandboxDTO(&boxes[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
}

func (h *AdminSandboxHandler) GetSandbox(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid Sandbox name")
		return
	}
	box, err := h.Kube.GetSandbox(r.Context(), name)
	if err != nil {
		writeConfigurationError(w, "read", "Sandbox", name, err)
		return
	}
	writeJSON(w, http.StatusOK, toSandboxDTO(box))
}

func (h *AdminSandboxHandler) CreateSandbox(w http.ResponseWriter, r *http.Request) {
	req := SandboxUpsertRequest{SandboxSpec: api.SandboxSpec{Type: api.SandboxKata}}
	if !decodeConfigurationRequest(w, r, &req) {
		return
	}
	if !teamNamePattern.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Name must be a DNS-1123 label (start with letter, lowercase, max 63 chars)")
		return
	}
	if msg := validateSandboxSpec(req.SandboxSpec); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_sandbox", msg)
		return
	}
	box, err := h.Kube.CreateSandbox(r.Context(), &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: req.Name}, Spec: req.SandboxSpec})
	if err != nil {
		writeConfigurationError(w, "create", "Sandbox", req.Name, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSandboxDTO(box))
}

func (h *AdminSandboxHandler) UpdateSandbox(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid Sandbox name")
		return
	}
	req := SandboxUpsertRequest{SandboxSpec: api.SandboxSpec{Type: api.SandboxKata}}
	if !decodeConfigurationRequest(w, r, &req) {
		return
	}
	if req.Name != "" && req.Name != name {
		writeError(w, http.StatusBadRequest, "name_mismatch", "body.name does not match URL name")
		return
	}
	if msg := validateSandboxSpec(req.SandboxSpec); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_sandbox", msg)
		return
	}
	version, valid := configurationVersion(req.UID, req.ResourceVersion)
	if !valid {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Sandbox are required")
		return
	}
	box, err := h.Kube.UpdateSandboxSpec(r.Context(), name, req.SandboxSpec, version)
	if err != nil {
		writeConfigurationError(w, "update", "Sandbox", name, err)
		return
	}
	writeJSON(w, http.StatusOK, toSandboxDTO(box))
}

func (h *AdminSandboxHandler) DeleteSandbox(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid Sandbox name")
		return
	}
	var req struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	}
	if !decodeConfigurationRequest(w, r, &req) {
		return
	}
	version, valid := configurationVersion(req.UID, req.ResourceVersion)
	if !valid {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Sandbox are required")
		return
	}
	if err := h.Kube.DeleteSandbox(r.Context(), name, version); err != nil {
		writeConfigurationError(w, "delete", "Sandbox", name, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeConfigurationRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid configuration JSON: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_body", "Provide exactly one JSON object")
		return false
	}
	return true
}
