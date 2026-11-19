// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import "net/http"

// ConfigResponse is the JSON response for GET /api/v1/config.
type ConfigResponse struct {
	NoticeBanner string `json:"noticeBanner"`
}

// ConfigHandler serves portal configuration to the frontend.
type ConfigHandler struct {
	NoticeBanner string
}

// GetConfig handles GET /api/v1/config.
func (h *ConfigHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ConfigResponse{
		NoticeBanner: h.NoticeBanner,
	})
}
