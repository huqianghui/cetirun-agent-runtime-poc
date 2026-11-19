// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type toolIdentityRequest struct {
	Provider string `json:"provider"`
}

func connectionMode(c Connection) string {
	if c.Mode == "" {
		return modeDelegated
	}
	return c.Mode
}

func isSharedConnectionMode(mode string) bool {
	return mode == modeDelegated || mode == modeInstallation
}

func connectionMatchesAgent(c Connection, agent Agent, tenant string) bool {
	if agent.UnsupportedRuntime {
		return false
	}
	switch connectionMode(c) {
	case modeDelegated, modeInstallation:
		return c.Identity == nil
	case modeAgent:
		b := c.Identity
		return c.Provider == "azure" && b != nil && !agent.UnsupportedRuntime && agent.AgentIdentityReady &&
			b.InstanceUID == agent.UID && sameIdentityID(b.TenantID, tenant) && sameIdentityID(b.TenantID, agent.OwnerTenantID) &&
			sameIdentityID(b.ClientID, agent.AgentIdentityClientID) && sameIdentityID(b.PrincipalID, agent.AgentIdentityObjectID)
	default:
		return false
	}
}

func (s *Server) agentIdentityCreate(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(r)
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner is required", correlation)
		return
	}
	agent, err := s.portalBindingAgent(r, owner)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Instance owner binding is invalid", correlation)
		return
	}
	if agent.PolicyManaged {
		agent, err = s.store.qualifyNativeAgent(r.Context(), agent, nil)
		if err != nil {
			writeAPIError(w, http.StatusConflict, "identity_not_ready", "Current native workload identity is not ready", correlation)
			return
		}
	}
	var input toolIdentityRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Provider != "azure" {
		writeAPIError(w, http.StatusBadRequest, "unsupported_identity_mode", "Independent identity is not provisioned for this provider", correlation)
		return
	}
	binding := &IdentityBinding{InstanceUID: agent.UID, TenantID: s.cfg.TenantID, ClientID: agent.AgentIdentityClientID, PrincipalID: agent.AgentIdentityObjectID}
	c := Connection{Provider: input.Provider, Mode: modeAgent, Identity: binding}
	if !s.cfg.connectionModeEnabled(c) {
		writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "This identity mode is disabled", correlation)
		return
	}
	if !s.connectionMatchesAgent(c, agent) || !uuidPattern.MatchString(binding.ClientID) || !uuidPattern.MatchString(binding.PrincipalID) {
		writeAPIError(w, http.StatusConflict, "identity_not_ready", "Enable Agent Identity on a supported instance and wait for provisioning", correlation)
		return
	}
	// A deterministic binding key makes concurrent create requests idempotent.
	c.ID = "agent-" + stateHash(agent.UID + "/azure/" + strings.ToLower(binding.TenantID+"/"+binding.PrincipalID+"/"+binding.ClientID))[:40]
	auditTarget(r, c.ID, agent.UID, "")
	existing, _, err := s.store.getConnectionForAgent(r.Context(), agent, c.ID)
	if err == nil {
		if existing.Deleted || !sameOwner(existing.Connection.Owner, owner) || !s.connectionMatchesAgent(existing.Connection, agent) {
			writeAPIError(w, http.StatusConflict, "identity_disconnected", "This identity binding has been disconnected", correlation)
			return
		}
		writeJSON(w, http.StatusOK, projectedConnection(existing.Connection))
		return
	}
	if !errors.Is(err, errNotFound) {
		writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Identity storage is unavailable", correlation)
		return
	}
	now := s.currentTime().UTC()
	c.Owner, c.Revision, c.Status, c.CreatedAt, c.UpdatedAt = owner, 1, "connected", now, now
	c.AccountDisplay, c.AccountID, c.Services = "Agent "+agent.Name, binding.PrincipalID, connectionServices(c)
	auditMutation(r)
	if err := s.store.putConnection(r.Context(), &connectionRecord{Connection: c, InstanceName: agent.Name, InstanceNamespace: agent.Namespace}, ""); err != nil {
		status, code := http.StatusServiceUnavailable, "dependency_unavailable"
		if errors.Is(err, errConflict) {
			status, code = http.StatusConflict, "conflict"
		}
		writeAPIError(w, status, code, "Identity state changed; reload before retrying", correlation)
		return
	}
	auditCommit(r)
	writeJSON(w, http.StatusOK, projectedConnection(c))
}

func (s *Server) issueAgentCredential(ctx context.Context, connection Connection, agent Agent) (string, time.Time, error) {
	if connection.Provider != "azure" || connectionMode(connection) != modeAgent || !s.connectionMatchesAgent(connection, agent) {
		return "", time.Time{}, errors.New("independent identity binding is invalid")
	}
	client := s.oauth
	if client == nil {
		client = newOAuthClient(s.cfg)
	}
	started := s.currentTime()
	token, err := client.agentToken(ctx, connection.Identity.ClientID)
	if err != nil {
		return "", time.Time{}, err
	}
	// ExpiresIn is trusted token-endpoint metadata; access tokens remain opaque.
	expires := started.Add(time.Duration(token.ExpiresIn)*time.Second - 30*time.Second)
	if !expires.After(s.currentTime()) {
		return "", time.Time{}, errors.New("Agent credential is already expiring")
	}
	return token.AccessToken, expires, nil
}

// Deployment qualification is independent of the provider identity mode.
func (s *Server) connectionMatchesAgent(c Connection, agent Agent) bool {
	if (s.cfg.ToolRuntimeTypes != nil || agent.RuntimeType == "custom") && !contains(s.cfg.ToolRuntimeTypes, agent.RuntimeType) {
		return false
	}
	return s.cfg.connectionModeEnabled(c) && connectionMatchesAgent(c, agent, s.cfg.TenantID)
}
