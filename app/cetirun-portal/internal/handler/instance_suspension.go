// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"log/slog"
	"net/http"
	"strconv"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
)

const agentGenerationHeader = "X-CetiRun-Agent-Generation"

func (h *InstanceHandler) SuspendInstance(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	h.setInstanceSuspended(w, r, claims.Alias, true)
}

func (h *InstanceHandler) ResumeInstance(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	h.setInstanceSuspended(w, r, claims.Alias, false)
}

func (h *InstanceHandler) SuspendInstanceByName(w http.ResponseWriter, r *http.Request) {
	h.setInstanceSuspended(w, r, r.PathValue("name"), true)
}

func (h *InstanceHandler) ResumeInstanceByName(w http.ResponseWriter, r *http.Request) {
	h.setInstanceSuspended(w, r, r.PathValue("name"), false)
}

func (h *InstanceHandler) setInstanceSuspended(w http.ResponseWriter, r *http.Request, name string, suspended bool) {
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
	if values := r.Header.Values(agentGenerationHeader); len(values) != 0 {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_generation", "Agent generation header must contain one positive integer")
			return
		}
		generation, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || generation < 1 {
			writeError(w, http.StatusBadRequest, "invalid_generation", "Agent generation header must be a positive integer")
			return
		}
		version.Generation = generation
	}
	instance, err := h.Kube.SetInstanceSuspendedByName(r.Context(), name, version, suspended,
		func(current *api.Agent) error {
			if !ownsInstance(current, claims) {
				return &managementError{http.StatusForbidden, "forbidden", "Only the instance owner may suspend or resume this Agent"}
			}
			return nil
		})
	if err != nil {
		slog.Warn("Instance suspension change failed", "name", name, "suspended", suspended, "error", err)
		writeManagementError(w, err)
		return
	}
	response, err := h.toInstanceResponse(r.Context(), instance)
	if err != nil {
		slog.Warn("Failed to read accepted suspension change", "name", name, "error", err)
		writeManagementError(w, err)
		return
	}
	slog.Info("Instance suspension intent accepted", "name", name, "suspended", suspended)
	writeJSON(w, http.StatusAccepted, response)
}
