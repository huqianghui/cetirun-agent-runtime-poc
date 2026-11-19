// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// ModelAccessRequest selects the model sources of a new Agent through the
// platform AI Gateway. Omitted switches use the sources currently available
// to the selected Team, or deployment defaults without a Team.
type ModelAccessRequest struct {
	SharedModels  *bool `json:"sharedModels,omitempty"`
	GitHubCopilot *bool `json:"githubCopilot,omitempty"`
}

// AIGatewayResponse describes an Agent's models in the AI Gateway. It holds
// names and states only, never keys or addresses.
type AIGatewayResponse struct {
	// Routed reports whether the Agent's model traffic uses the Gateway.
	Routed bool `json:"routed"`
	// Phase and Reason report the Agent's virtual key.
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	// SharedModels and GitHubCopilot are the owner's effective switches.
	SharedModels  bool `json:"sharedModels"`
	GitHubCopilot bool `json:"githubCopilot"`
	// Models are the models the Agent is configured with, default first
	// when it has one.
	Models       []string                  `json:"models"`
	DefaultModel string                    `json:"defaultModel,omitempty"`
	Copilot      *AIGatewayCopilotResponse `json:"copilot,omitempty"`
	// ModelRouters are the Agent's smart-routing models, its Team's and, with
	// a policy, the Gateway's own routers the policy lists, by name: whether
	// the Agent may use each, and why not. Models and DefaultModel name
	// granted routers by these names too.
	ModelRouters []AIGatewayModelRouterResponse `json:"modelRouters,omitempty"`
	// RateLimits are the limits the Agent's policy sets on its key. Only
	// the single-Agent read fills them in.
	RateLimits *AgentPolicyRateLimitsDTO `json:"rateLimits,omitempty"`
}

// AIGatewayCopilotResponse describes the Agent's GitHub Copilot proxy.
type AIGatewayCopilotResponse struct {
	// Phase is SignInRequired, Starting, Ready or Degraded.
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Models are the registered Copilot models, as the Agent names them: a
	// shared model's name or the Copilot model's own ID.
	Models []string `json:"models"`
	// Unpriced are the account's models left out for lack of a price.
	Unpriced []string `json:"unpriced,omitempty"`
	// Shared are the shared models the Agent's Copilot account serves too:
	// requests for them may go to either.
	Shared []string `json:"shared,omitempty"`
	// Skipped are the account's models left out because a shared model
	// already has their name.
	Skipped []string `json:"skipped,omitempty"`
	// NotAllowed are the account's models left out because the Agent's
	// policy does not allow the shared models that are the same models.
	NotAllowed []string `json:"notAllowed,omitempty"`
}

// AIGatewayCatalogResponse is the JSON response for GET /api/v1/ai-gateway/catalog.
type AIGatewayCatalogResponse struct {
	// Known is false until the operator has published the catalog.
	Known        bool                                    `json:"known"`
	SharedModels []cetirunv1alpha1.AIGatewayCatalogModel `json:"sharedModels"`
	// DecisionModels are the shared decision models a Team router's jev
	// classifier may use; no Agent is configured with them.
	DecisionModels []string `json:"decisionModels,omitempty"`
	// GitHubCopilot reports whether the deployment offers GitHub Copilot to
	// Agents.
	GitHubCopilot bool `json:"githubCopilot"`
	// AgentTypes are the agent types new Agents can take the Gateway's models
	// with: those the operator's default agent chart routes through it. They
	// are null while the operator has not read that chart.
	AgentTypes []cetirunv1alpha1.AgentType `json:"agentTypes"`
}

// agentTypeNames are the agent types the Gateway can route, as the Portal
// names them.
var agentTypeNames = map[cetirunv1alpha1.AgentType]string{
	cetirunv1alpha1.AgentTypeOpenClaw: "OpenClaw",
	cetirunv1alpha1.AgentTypeHermes:   "Hermes",
	cetirunv1alpha1.AgentTypeCustom:   "Custom",
}

func isAIGatewayPlaceholder(model string) bool {
	return model == cetirunv1alpha1.AIGatewayNoModelsModel || model == cetirunv1alpha1.AIGatewaySignInRequiredModel
}

// aiGatewayResponse summarises the Agent's Gateway status, or returns nil
// when the Agent has none.
func aiGatewayResponse(instance *cetirunv1alpha1.Agent) *AIGatewayResponse {
	status := instance.Status.AIGateway
	if status == nil {
		return nil
	}
	resp := &AIGatewayResponse{
		Routed:        status.Routed,
		Phase:         status.Phase,
		Reason:        status.Reason,
		SharedModels:  instance.Spec.SharedModelsEnabled(),
		GitHubCopilot: instance.Spec.GitHubCopilotEnabled(),
		Models:        []string{},
	}
	if !isAIGatewayPlaceholder(status.DefaultModel) {
		resp.DefaultModel = status.DefaultModel
	}
	for _, model := range status.Models {
		if !isAIGatewayPlaceholder(model) {
			resp.Models = append(resp.Models, model)
		}
	}
	if copilot := status.Copilot; copilot != nil {
		resp.Copilot = &AIGatewayCopilotResponse{
			Phase:      copilot.Phase,
			Reason:     copilot.Reason,
			Models:     append([]string{}, copilot.Models...),
			Unpriced:   append([]string(nil), copilot.Unpriced...),
			Skipped:    append([]string(nil), copilot.Skipped...),
			NotAllowed: append([]string(nil), copilot.NotAllowed...),
		}
		for _, route := range copilot.Routes {
			if route.Shared && !slices.Contains(resp.Copilot.Shared, route.Name) {
				resp.Copilot.Shared = append(resp.Copilot.Shared, route.Name)
			}
		}
	}
	applyModelRouterNames(resp, status)
	return resp
}

// sharedModelCatalog reads the operator's catalog; an unreadable catalog
// counts as unknown.
func (h *InstanceHandler) sharedModelCatalog(ctx context.Context) AIGatewayCatalogResponse {
	catalog, known, err := h.Kube.GetAIGatewayCatalog(ctx, h.Config.AIGatewayCatalogNamespace)
	if err != nil {
		slog.Warn("Failed to read the AI Gateway catalog", "error", err)
		return AIGatewayCatalogResponse{SharedModels: []cetirunv1alpha1.AIGatewayCatalogModel{}, AgentTypes: []cetirunv1alpha1.AgentType{}}
	}
	var agentTypes []cetirunv1alpha1.AgentType
	switch {
	case !known:
		agentTypes = []cetirunv1alpha1.AgentType{}
	case catalog.Routing == nil:
		// Routing must be explicitly published before native creation.
		agentTypes = nil
	default:
		// Null while the operator has not read its default agent chart.
		agentTypes = catalog.Routing.AgentTypes
	}
	return AIGatewayCatalogResponse{Known: known, SharedModels: catalog.SharedModels, DecisionModels: catalog.DecisionModels,
		GitHubCopilot: catalog.GitHubCopilot, AgentTypes: agentTypes}
}

// GetAIGatewayCatalog handles GET /api/v1/ai-gateway/catalog: the shared
// models new Agents can use, by name and limits.
func (h *InstanceHandler) GetAIGatewayCatalog(w http.ResponseWriter, r *http.Request) {
	if !h.Config.AIGatewayEnabled {
		writeError(w, http.StatusNotFound, "feature_disabled", "The AI Gateway is not enabled for this deployment")
		return
	}
	writeJSON(w, http.StatusOK, h.sharedModelCatalog(r.Context()))
}

// validateAIGatewayCreate uses the same current options as the form and records
// explicit model switches. The caller must run it before any creation writes.
func (h *InstanceHandler) validateAIGatewayCreate(w http.ResponseWriter, r *http.Request, req CreateInstanceRequest, agentType cetirunv1alpha1.AgentType) (string, string, *cetirunv1alpha1.ModelAccessSpec, bool) {
	return h.validateAIGatewaySelection(w, r, req, agentType, true)
}

// validateAIGatewaySelection always checks current permissions and model sources.
// Default-chart suitability applies only when creating an Agent. A matched
// retained creation keeps its own chart, whose routing the Operator reconciles.
func (h *InstanceHandler) validateAIGatewaySelection(w http.ResponseWriter, r *http.Request, req CreateInstanceRequest, agentType cetirunv1alpha1.AgentType, newAgent bool) (string, string, *cetirunv1alpha1.ModelAccessSpec, bool) {
	options, err := h.resolveCreationOptions(r, req.Team)
	if err != nil {
		writeManagementError(w, err)
		return "", "", nil, false
	}
	reject := func(status int, code, message string) (string, string, *cetirunv1alpha1.ModelAccessSpec, bool) {
		writeError(w, status, code, message)
		return "", "", nil, false
	}
	if options.Blocked != nil {
		return reject(http.StatusConflict, options.Blocked.Code, options.Blocked.Message)
	}
	if !slices.Contains(options.AllowedAgentTypes, agentType) {
		return reject(http.StatusForbidden, "agent_type_not_allowed", "This Agent type is not allowed by the deployment or the selected Team's Policy")
	}
	if options.AgentIdentity.Required && !req.AgentIdentityEnabled {
		return reject(http.StatusForbidden, "policy_identity_required", "The selected Team's Policy requires Agent Identity")
	}
	if req.AgentIdentityEnabled && !options.AgentIdentity.Allowed {
		return reject(http.StatusForbidden, "policy_identity_not_allowed", "Agent Identity is not available to this owner under the selected Team's Policy and deployment settings")
	}
	if !options.AIGatewayEnabled {
		return reject(http.StatusConflict, "gateway_required", "The AI Gateway is required for Agent creation in this deployment")
	}
	catalog := options.Gateway
	if !catalog.Known {
		return reject(http.StatusServiceUnavailable, "model_sources_unknown", "The AI Gateway has not published its models yet. Try again in a minute.")
	}
	if newAgent && catalog.AgentTypes == nil {
		return reject(http.StatusServiceUnavailable, "agent_types_unknown", "The operator has not read its agent chart yet. Try again in a minute.")
	}
	if newAgent && !slices.Contains(catalog.AgentTypes, agentType) {
		return reject(http.StatusConflict, "agent_chart_unsupported", "This deployment's agent chart does not route "+agentTypeNames[agentType]+" agents through the AI Gateway. Ask an administrator to upgrade the agent chart.")
	}
	sharedAvailable, copilotAvailable := len(catalog.SharedModels) > 0, catalog.GitHubCopilot
	if !sharedAvailable && !copilotAvailable {
		return reject(http.StatusConflict, "no_model_source", "No models are available under the selected Team's Policy and deployment settings")
	}
	shared, copilot := sharedAvailable, copilotAvailable
	if req.ModelAccess != nil {
		if req.ModelAccess.SharedModels != nil {
			shared = *req.ModelAccess.SharedModels
		}
		if req.ModelAccess.GitHubCopilot != nil {
			copilot = *req.ModelAccess.GitHubCopilot
		}
	}
	if (shared && !sharedAvailable) || (copilot && !copilotAvailable) || (!shared && !copilot) {
		return reject(http.StatusBadRequest, "invalid_model_access", "Choose at least one model source allowed by the selected Team's Policy and available in this deployment")
	}
	model := strings.TrimSpace(req.Model)
	if model != "" && !(shared && slices.ContainsFunc(catalog.SharedModels, func(candidate cetirunv1alpha1.AIGatewayCatalogModel) bool { return candidate.Name == model })) && !(copilot && strings.HasPrefix(model, cetirunv1alpha1.AIGatewayCopilotModelPrefix) && !isAIGatewayPlaceholder(model)) &&
		!slices.Contains(options.ModelRouters, model) {
		return reject(http.StatusForbidden, "model_not_allowed", "The requested default model is not available under the selected Team's Policy and model sources")
	}
	return options.Team, options.sandboxRef, &cetirunv1alpha1.ModelAccessSpec{SharedModels: &shared, GitHubCopilot: &copilot}, true
}

// aiGatewayRouted reports whether the Agent's model traffic uses the Gateway,
// which replaces the in-agent GitHub Copilot sign-in helper.
func aiGatewayRouted(instance *cetirunv1alpha1.Agent) bool {
	return instance.Status.AIGateway != nil && instance.Status.AIGateway.Routed
}
