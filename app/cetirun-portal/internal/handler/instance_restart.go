// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"log/slog"
	"net/http"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
)

func (h *InstanceHandler) RestartInstance(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	h.restartInstance(w, r, claims.Alias)
}

func (h *InstanceHandler) RestartInstanceByName(w http.ResponseWriter, r *http.Request) {
	h.restartInstance(w, r, r.PathValue("name"))
}

func (h *InstanceHandler) restartInstance(w http.ResponseWriter, r *http.Request, name string) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not_authenticated", "Authentication is required")
		return
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_name", "Agent name is required")
		return
	}
	version, ok := h.readInstanceVersion(w, r)
	if !ok {
		return
	}
	instance, err := h.Kube.RestartInstanceByName(r.Context(), name, version, r.Header.Get("Idempotency-Key"),
		func(current *api.Agent) error {
			if !ownsInstance(current, claims) {
				return &managementError{http.StatusForbidden, "forbidden", "Only the instance owner may restart this Agent"}
			}
			return nil
		})
	if err != nil {
		slog.Warn("Instance restart rejected", "name", name, "error", err)
		writeManagementError(w, err)
		return
	}
	response, err := h.toInstanceResponse(r.Context(), instance)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	slog.Info("Instance restart request accepted", "name", name)
	writeJSON(w, http.StatusAccepted, response)
}
