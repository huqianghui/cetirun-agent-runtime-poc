// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"log/slog"
	"net/http"
	"slices"

	"github.com/Azure/CetiRun/app/cetirun-operator/sandboxcatalog"
)

type sandboxStorageOptionsDTO struct {
	sandboxcatalog.Options
	DefaultStorageClassName string `json:"defaultStorageClassName"`
}

// GetStorageOptions supplies administrator-only form choices. It neither
// qualifies CSI support nor replaces admission or coordinated write recovery.
func (h *AdminSandboxHandler) GetStorageOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.Config.SandboxStoragePlatformNamespace == "" || h.Config.SandboxStoragePlatformConfigMapName == "" {
		writeError(w, http.StatusServiceUnavailable, "sandbox_storage_not_configured", "Sandbox storage options are not configured. Configure the Portal to read the Operator's approved platform configuration.")
		return
	}
	options, err := h.Kube.GetSandboxStorageOptions(r.Context(), h.Config.SandboxStoragePlatformNamespace, h.Config.SandboxStoragePlatformConfigMapName)
	if err != nil {
		slog.Error("admin: read Sandbox storage options", "error", err)
		writeError(w, http.StatusServiceUnavailable, "sandbox_storage_unavailable", "Unable to load approved Sandbox storage options. Retry or ask the platform administrator to check the configuration and Portal read permissions.")
		return
	}
	defaultName := h.Config.SandboxStorageDefaultClassName
	if !slices.ContainsFunc(options.StorageClasses, func(option sandboxcatalog.StorageClass) bool { return option.Name == defaultName }) {
		defaultName = ""
	}
	writeJSON(w, http.StatusOK, sandboxStorageOptionsDTO{Options: options, DefaultStorageClassName: defaultName})
}
