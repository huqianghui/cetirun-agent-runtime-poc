// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"net/http"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
)

// FeaturesHandler handles /api/features requests.
type FeaturesHandler struct {
	Config config.Config
}

// FeaturesResponse is the JSON response for GET /api/features.
type FeaturesResponse struct {
	CustomChatEnabled       bool     `json:"customChatEnabled"`
	TailscaleEnabled        bool     `json:"tailscaleEnabled"`
	AgentIdentityEnabled    bool     `json:"agentIdentityEnabled"`
	AgentToolsEnabled       bool     `json:"agentToolsEnabled"`
	MaxInstancesPerUser     int      `json:"maxInstancesPerUser"`
	AllowedAgentTypes       []string `json:"allowedAgentTypes"`
	AIGatewayEnabled        bool     `json:"aiGatewayEnabled"`
	SkillMarketplaceEnabled bool     `json:"skillMarketplaceEnabled"`
}

// GetFeatures handles GET /api/features.
func (h *FeaturesHandler) GetFeatures(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, FeaturesResponse{
		CustomChatEnabled:       h.Config.CustomChatEnabled,
		TailscaleEnabled:        h.Config.TailscaleEnabled,
		AgentIdentityEnabled:    h.Config.AgentIdentityEnabled,
		AgentToolsEnabled:       h.Config.AgentToolsEnabled,
		MaxInstancesPerUser:     h.Config.MaxInstancesPerUser,
		AllowedAgentTypes:       h.Config.AllowedAgentTypes,
		AIGatewayEnabled:        h.Config.AIGatewayEnabled,
		SkillMarketplaceEnabled: h.Config.SkillMarketplaceEnabled,
	})
}
