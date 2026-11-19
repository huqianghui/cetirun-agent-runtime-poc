// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"log/slog"
	"net/http"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

// AdminInstanceHandler manages Agents through the administrator-only routes.
// It deliberately does not use the owner's credential-bearing response.
type AdminInstanceHandler struct {
	Kube   *kube.Client
	Config config.Config
}

// AdminAgentConfig is an explicit allowlist, independent of future CRD fields.
type AdminAgentConfig struct {
	Type     cetirunv1alpha1.AgentType `json:"type"`
	Image    *string                   `json:"image,omitempty"`
	Username *string                   `json:"username,omitempty"`
	Port     *int32                    `json:"port,omitempty"`
	WebUI    bool                      `json:"webUI"`
}

// AdminInstanceSummary is the row shape for the admin instances list.
type AdminInstanceSummary struct {
	Name        string            `json:"name"`
	Alias       string            `json:"alias"`
	DisplayName string            `json:"displayName,omitempty"`
	Email       string            `json:"email,omitempty"`
	Team        string            `json:"team,omitempty"`
	Agent       AdminAgentConfig  `json:"agent"`
	UID         string            `json:"uid"`
	Phase       string            `json:"phase,omitempty"`
	Ready       bool              `json:"ready"`
	Governance  *GovernanceStatus `json:"governance,omitempty"`
	CreatedAt   string            `json:"createdAt"`
}

// ListAllInstances handles GET /api/v1/admin/instances.
func (h *AdminInstanceHandler) ListAllInstances(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	items, err := h.Kube.ListAllInstances(r.Context())
	if err != nil {
		slog.Error("admin: list instances failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list instances")
		return
	}
	out := make([]AdminInstanceSummary, 0, len(items))
	for i := range items {
		out = append(out, adminInstanceSummary(&items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
}

func adminInstanceSummary(ins *cetirunv1alpha1.Agent) AdminInstanceSummary {
	agent := agentconfig.Defaults(ins.Spec.Agent)
	phase, ready := ins.Status.Phase, isReady(ins)
	if phase == "" {
		phase = "Pending"
	}
	if !ins.DeletionTimestamp.IsZero() {
		phase, ready = "Deleting", false
	}
	return AdminInstanceSummary{
		Name:        ins.Name,
		Alias:       ins.Spec.User.Alias,
		DisplayName: ins.Spec.User.DisplayName,
		Email:       ins.Spec.User.Email,
		Team:        instanceTeam(ins),
		Agent:       AdminAgentConfig{Type: agent.Type, Image: agent.Image, Username: agent.Username, Port: agent.Port, WebUI: agent.WebUI},
		UID:         string(ins.UID),
		Phase:       phase,
		Ready:       ready,
		Governance:  governanceStatus(ins),
		CreatedAt:   ins.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// instanceTeam returns the Team selected by the Agent.
func instanceTeam(ins *cetirunv1alpha1.Agent) string {
	return ins.Spec.Team
}

func isReady(ins *cetirunv1alpha1.Agent) bool {
	for _, c := range ins.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
