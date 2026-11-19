// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"net/http"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

// CreateOptionsResponse describes the selected Team's available creation
// choices. It is a form projection, not evidence of runtime authorization.
type CreateOptionsResponse struct {
	Team              string                   `json:"team"`
	PolicyName        string                   `json:"policyName,omitempty"`
	AllowedAgentTypes []api.AgentType          `json:"allowedAgentTypes"`
	AIGatewayEnabled  bool                     `json:"aiGatewayEnabled"`
	Gateway           AIGatewayCatalogResponse `json:"gateway"`
	AgentIdentity     CreateIdentityOptions    `json:"agentIdentity"`
	// ModelRouters are the names of the Team's smart-routing models the
	// selected Team's Policy allows. Whether an Agent can use one also
	// depends on its model sources; the operator decides and reports it.
	ModelRouters []string `json:"modelRouters"`
	// RateLimits are the AI Gateway limits the selected Team's policy sets on
	// each Agent's key; absent when it sets none.
	RateLimits *AgentPolicyRateLimitsDTO `json:"rateLimits,omitempty"`
	Blocked    *ErrorDetail              `json:"blocked,omitempty"`
}

type CreateIdentityOptions struct {
	Allowed       bool `json:"allowed"`
	Required      bool `json:"required"`
	CustomAllowed bool `json:"customAllowed,omitempty"`
}

type creationOptions struct {
	CreateOptionsResponse
	sandboxRef string
}

// GetCreateOptions handles GET /api/v1/instance/create-options. Reads are
// request-scoped so changing Teams or policies never reuses cached authority.
func (h *InstanceHandler) GetCreateOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := auth.ClaimsFromContext(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "not_authenticated", "Authentication is required")
		return
	}
	options, err := h.resolveCreationOptions(r, r.URL.Query().Get("team"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, options.CreateOptionsResponse)
}

func (h *InstanceHandler) resolveCreationOptions(r *http.Request, requestedTeam string) (*creationOptions, error) {
	team, sandboxRef, err := h.resolveSelectedTeam(r, requestedTeam)
	if err != nil {
		return nil, err
	}
	claims, _ := auth.ClaimsFromContext(r.Context())
	options := &creationOptions{CreateOptionsResponse: CreateOptionsResponse{
		Team: strings.TrimSpace(requestedTeam), AllowedAgentTypes: []api.AgentType{},
		AIGatewayEnabled: h.Config.AIGatewayEnabled,
		Gateway:          AIGatewayCatalogResponse{SharedModels: []api.AIGatewayCatalogModel{}, AgentTypes: []api.AgentType{}},
		AgentIdentity:    CreateIdentityOptions{Allowed: h.Config.AgentIdentityEnabled && claims.TenantID != "" && claims.ObjectID != ""},
		ModelRouters:     []string{},
	}, sandboxRef: sandboxRef}
	for _, kind := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw, api.AgentTypeCustom} {
		if h.Config.AgentTypeAllowed(string(kind)) {
			options.AllowedAgentTypes = append(options.AllowedAgentTypes, kind)
		}
	}
	block := func(code, message string) (*creationOptions, error) {
		options.Blocked = &ErrorDetail{Code: code, Message: message}
		return options, nil
	}
	if !h.Config.AIGatewayEnabled {
		return block("gateway_required", "The AI Gateway is required for Agent creation in this deployment")
	}
	options.Gateway = h.sharedModelCatalog(r.Context())
	// Decision models only classify a Team router's requests; no Agent
	// is created with one.
	options.Gateway.DecisionModels = nil
	options.ModelRouters = teamModelRouterNames(team, nil)
	if team != nil && team.Spec.PolicyConfigured() {
		policy, err := h.Kube.GetAgentPolicy(r.Context(), team.Spec.AgentPolicyRef)
		if err != nil {
			if kube.IsNotFound(err) {
				return nil, &managementError{http.StatusConflict, "policy_not_found", "The selected Team's Agent Policy does not exist"}
			}
			return nil, &managementError{http.StatusServiceUnavailable, "policy_unavailable", "Unable to read the selected Team's Agent Policy; retry after it is available"}
		}
		options.PolicyName = policy.Name
		options.RateLimits = toAgentPolicyRateLimitsDTO(policy.Spec.ModelAccess.RateLimits)
		if !policy.DeletionTimestamp.IsZero() {
			return block("policy_unavailable", "The selected Team's Agent Policy is being deleted")
		}
		// Validate the Portal-managed entitlement fields using the same rules as
		// policy administration. Runtime-only policy domains remain with admission.
		dto := toAgentPolicyDTO(policy)
		if message := validateAgentPolicyRequest(AgentPolicyUpsertRequest{Harnesses: dto.Harnesses, Skills: dto.Skills, ModelAccess: dto.ModelAccess}); message != "" {
			return block("invalid_policy", "The selected Team's Agent Policy is invalid: "+message)
		}
		options.AllowedAgentTypes = slices.DeleteFunc(options.AllowedAgentTypes, func(kind api.AgentType) bool {
			return !slices.Contains(policy.Spec.Harnesses.Allowed, kind)
		})
		options.Gateway.SharedModels = slices.DeleteFunc(options.Gateway.SharedModels, func(model api.AIGatewayCatalogModel) bool {
			return !sharedModelAllowed(model, policy)
		})
		options.Gateway.GitHubCopilot = options.Gateway.GitHubCopilot && policy.Spec.ModelAccess.Copilot.Allowed
		if h.Config.AIGatewayEnabled {
			options.ModelRouters = teamModelRouterNames(team, policy)
		}
		identity := policy.Spec.AgentIdentity
		options.AgentIdentity.Required = identity.Required
		options.AgentIdentity.Allowed = options.AgentIdentity.Allowed && identity.Allowed && slices.Contains(identity.CredentialServices, "azure-resource-manager")
		for _, skill := range team.Spec.Skills {
			if !slices.Contains(policy.Spec.Skills.Allowed, skill) {
				return block("policy_skills_conflict", "The Team's default Skills are not allowed by its Agent Policy; ask an administrator to update the Team or Policy")
			}
		}
	}
	if len(options.AllowedAgentTypes) == 0 {
		return block("agent_type_not_allowed", "No Agent type is allowed by both this deployment and the selected Team's Policy")
	}
	if options.AgentIdentity.Required && !options.AgentIdentity.Allowed {
		return block("policy_identity_unavailable", "The Policy requires Agent Identity, but the deployment, Policy capability or verified Microsoft Entra owner identity is unavailable")
	}
	options.AgentIdentity.CustomAllowed = options.AgentIdentity.Allowed && sandboxRef != "" &&
		h.Config.AgentToolsEnabled && slices.Contains(h.Config.AgentToolsRuntimeTypes, "custom")
	if options.AgentIdentity.Required && !options.AgentIdentity.CustomAllowed {
		options.AllowedAgentTypes = slices.DeleteFunc(options.AllowedAgentTypes, func(kind api.AgentType) bool {
			return kind == api.AgentTypeCustom
		})
		if len(options.AllowedAgentTypes) == 0 {
			return block("policy_identity_unavailable", "The Policy requires Agent Identity, but Custom Agents do not support it")
		}
	}
	return options, nil
}

// resolveSelectedTeam keeps Team selection optional. A selected policy-bound
// Team must still have its complete policy and Sandbox binding.
func (h *InstanceHandler) resolveSelectedTeam(r *http.Request, requestedTeam string) (*api.Team, string, error) {
	name := strings.TrimSpace(requestedTeam)
	if name == "" {
		return nil, "", nil
	}
	team, err := h.Kube.GetTeam(r.Context(), name)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil, "", &managementError{http.StatusBadRequest, "unknown_team", "The selected Team does not exist"}
		}
		return nil, "", &managementError{http.StatusServiceUnavailable, "team_unavailable", "Unable to read the selected Team; retry after it is available"}
	}
	if !team.DeletionTimestamp.IsZero() {
		return nil, "", &managementError{http.StatusConflict, "team_unavailable", "The selected Team is being deleted"}
	}
	if !team.Spec.PolicyConfigured() {
		return team, "", nil
	}
	sandboxRef := strings.TrimSpace(team.Spec.DefaultSandboxRef)
	if sandboxRef == "" || team.Spec.AgentPolicyRef == "" || !slices.Contains(team.Spec.SandboxRefs, sandboxRef) {
		return nil, "", &managementError{http.StatusConflict, "team_not_configured", "The selected Team has incomplete policy or Sandbox configuration"}
	}
	return team, sandboxRef, nil
}

func (h *InstanceHandler) resolveTeamBinding(w http.ResponseWriter, r *http.Request, requestedTeam string) (string, string, bool) {
	team, sandboxRef, err := h.resolveSelectedTeam(r, requestedTeam)
	if err != nil {
		writeManagementError(w, err)
		return "", "", false
	}
	if team == nil {
		return "", "", true
	}
	return team.Name, sandboxRef, true
}
