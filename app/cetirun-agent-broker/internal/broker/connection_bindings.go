// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"errors"
	"net/http"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type bindingRequest struct {
	ConnectionID string                  `json:"connectionId"`
	Services     []string                `json:"services"`
	Enabled      bool                    `json:"enabled"`
	Revision     uint64                  `json:"revision,omitempty"`
	GitHub       *agenttools.GitHubGrant `json:"github,omitempty"`
}

func buildBinding(input bindingRequest, owner Owner, uid string, c Connection, now time.Time) (ConnectionBinding, error) {
	if !input.Enabled {
		return ConnectionBinding{}, errors.New("use Disconnect to remove an association")
	}
	if input.ConnectionID == "" || input.ConnectionID != c.ID || len(input.Services) == 0 || c.Status != "connected" {
		return ConnectionBinding{}, errors.New("a connected account and explicit services are required")
	}
	if len(uniqueStrings(input.Services)) != len(input.Services) {
		return ConnectionBinding{}, errors.New("services must be unique")
	}
	for _, id := range input.Services {
		service, ok := agenttools.LookupService(id)
		if !ok || service.Provider != c.Provider || !contains(connectionServices(c), id) {
			return ConnectionBinding{}, errors.New("service is not available for this connection")
		}
	}
	if connectionMode(c) == modeInstallation {
		if c.Provider != "github" || c.GitHub == nil || c.GitHub.InstallationID <= 0 || !input.GitHub.Valid() || len(input.Services) != 1 || input.Services[0] != "github" {
			return ConnectionBinding{}, errors.New("a GitHub App binding requires explicit repositories and permissions")
		}
	} else if input.GitHub != nil {
		return ConnectionBinding{}, errors.New("GitHub App permissions are only valid for a GitHub App connection")
	}
	return ConnectionBinding{ID: bindingKey(uid, c.ID), InstanceUID: uid, ConnectionID: c.ID, Services: input.Services, Enabled: input.Enabled, Revision: 1, Owner: owner, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), GitHub: input.GitHub}, nil
}

func (s *Server) bindingAgent(w http.ResponseWriter, r *http.Request) (Owner, Agent, bool) {
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner identity is required", correlationID(r))
		return Owner{}, Agent{}, false
	}
	agent, err := s.portalBindingAgent(r, owner)
	if err != nil {
		if errors.Is(err, errPortalBindingRuntimeNotReady) {
			writeAPIError(w, http.StatusConflict, "runtime_not_ready", "The Agent runtime is not ready yet", correlationID(r))
			return Owner{}, Agent{}, false
		}
		if errors.Is(err, errDependencyUnavailable) {
			writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Agent runtime lookup is unavailable", correlationID(r))
			return Owner{}, Agent{}, false
		}
		writeAPIError(w, http.StatusForbidden, "forbidden", "The instance owner binding is invalid", correlationID(r))
		return Owner{}, Agent{}, false
	}
	if agent.PolicyManaged && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		agent, err = s.store.qualifyNativeAgent(r.Context(), agent, nil)
		if err != nil {
			writeAPIError(w, http.StatusConflict, "identity_not_ready", "Current native workload identity is not ready", correlationID(r))
			return Owner{}, Agent{}, false
		}
	}
	return owner, agent, true
}

func (s *Server) bindingsList(w http.ResponseWriter, r *http.Request) {
	owner, agent, ok := s.bindingAgent(w, r)
	if !ok {
		return
	}
	bindings, err := s.store.listBindings(r.Context(), owner, agent.UID, agent.Name)
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Binding storage is unavailable", correlationID(r))
		return
	}
	visible := []ConnectionBinding{}
	for _, binding := range bindings {
		if !binding.Revoked && binding.Enabled {
			visible = append(visible, binding)
		}
	}
	writeJSON(w, http.StatusOK, visible)
}

func (s *Server) bindingCreate(w http.ResponseWriter, r *http.Request) { s.bindingSave(w, r, false) }
func (s *Server) bindingUpdate(w http.ResponseWriter, r *http.Request) { s.bindingSave(w, r, true) }

func (s *Server) bindingSave(w http.ResponseWriter, r *http.Request, update bool) {
	owner, agent, ok := s.bindingAgent(w, r)
	if !ok {
		return
	}
	var input bindingRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	connection, _, err := s.store.getConnectionForAgent(r.Context(), agent, input.ConnectionID)
	if errors.Is(err, errNotFound) || (err == nil && (connection.Deleted || !sameOwner(connection.Connection.Owner, owner))) {
		writeAPIError(w, 404, "not_found", "Connection not found", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
		return
	}
	if !s.connectionMatchesAgent(connection.Connection, agent) {
		writeAPIError(w, 403, "identity_changed", "Identity does not belong to this Agent", correlationID(r))
		return
	}
	binding, err := buildBinding(input, owner, agent.UID, connection.Connection, s.currentTime())
	if err != nil {
		writeAPIError(w, 400, "invalid_request", err.Error(), correlationID(r))
		return
	}
	if connectionMode(connection.Connection) == modeInstallation {
		if err := s.validateGitHubAppBinding(r.Context(), connection, binding); err != nil {
			writeAPIError(w, 403, "permission_denied", "GitHub installation cannot authorize this binding", correlationID(r))
			return
		}
		// Validation may persist a renamed installation account. Reload before
		// reserving so that the reservation is fenced by the current revision.
		connection, _, err = s.store.getConnectionForAgent(r.Context(), agent, input.ConnectionID)
		if errors.Is(err, errNotFound) || (err == nil && (connection.Deleted || !sameOwner(connection.Connection.Owner, owner))) {
			writeAPIError(w, 404, "not_found", "Connection not found", correlationID(r))
			return
		}
		if err != nil {
			writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
			return
		}
		if !s.connectionMatchesAgent(connection.Connection, agent) {
			writeAPIError(w, 403, "identity_changed", "Identity does not belong to this Agent", correlationID(r))
			return
		}
	}
	if update && (input.Revision == 0 || r.PathValue("id") != binding.ID) {
		writeAPIError(w, 400, "invalid_request", "Binding and revision are required", correlationID(r))
		return
	}
	version := ""
	prior, priorVersion, err := s.store.getBinding(r.Context(), binding.ID, agent)
	if err == nil {
		if !sameOwner(prior.Binding.Owner, owner) || prior.Binding.InstanceUID != agent.UID || prior.Binding.ConnectionID != binding.ConnectionID {
			writeAPIError(w, 403, "forbidden", "Binding ownership changed", correlationID(r))
			return
		}
		if (update && (prior.Binding.Revoked || prior.Binding.Revision != input.Revision)) || (!update && !prior.Binding.Revoked && prior.Binding.Enabled) {
			writeAPIError(w, 409, "conflict", "Binding changed; reload before updating", correlationID(r))
			return
		}
		binding.Revision, binding.CreatedAt, version = prior.Binding.Revision+1, prior.Binding.CreatedAt, priorVersion
	} else if !errors.Is(err, errNotFound) {
		writeAPIError(w, 503, "dependency_unavailable", "Binding storage is unavailable", correlationID(r))
		return
	} else if update {
		writeAPIError(w, 404, "not_found", "Binding not found", correlationID(r))
		return
	}
	auditTarget(r, binding.ConnectionID, agent.UID, binding.ID)
	auditMutation(r)
	token, err := s.store.reserveBinding(r.Context(), connection.Connection, agent, binding, s.currentTime())
	if err != nil {
		status, code := 503, "dependency_unavailable"
		if errors.Is(err, errConflict) {
			status, code = 409, "conflict"
		}
		writeAPIError(w, status, code, "Connection changed; reload before connecting", correlationID(r))
		return
	}
	if err := s.store.putBinding(r.Context(), &bindingRecord{Binding: binding, ReservationToken: token, InstanceName: agent.Name, InstanceNamespace: agent.Namespace}, version); err != nil {
		// A conflict guarantees this write did not commit. For an unknown
		// outcome keep the reservation until it can be safely fenced.
		if errors.Is(err, errConflict) {
			_ = s.store.releaseBindingReservation(r.Context(), owner, binding.ConnectionID, token)
		}
		status, code := 503, "dependency_unavailable"
		if errors.Is(err, errConflict) {
			status, code = 409, "conflict"
		}
		writeAPIError(w, status, code, "Binding changed; reload before updating", correlationID(r))
		return
	}
	if prior != nil {
		_ = s.store.releaseBindingReservation(r.Context(), owner, binding.ConnectionID, prior.ReservationToken)
	}
	auditCommit(r)
	writeJSON(w, http.StatusOK, binding)
}

func (s *Server) bindingDelete(w http.ResponseWriter, r *http.Request) {
	owner, agent, ok := s.bindingAgent(w, r)
	if !ok {
		return
	}
	expected, ok := ifMatchRevision(r)
	if !ok {
		writeAPIError(w, 428, "revision_required", "If-Match revision is required", correlationID(r))
		return
	}
	record, version, err := s.store.getBinding(r.Context(), r.PathValue("id"), agent)
	if errors.Is(err, errNotFound) || (err == nil && (record.Binding.Revoked || record.Binding.InstanceUID != agent.UID || !sameOwner(record.Binding.Owner, owner))) {
		writeAPIError(w, 404, "not_found", "Binding not found", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Binding storage is unavailable", correlationID(r))
		return
	}
	if record.Binding.Revision != expected {
		writeAPIError(w, 409, "conflict", "Binding revision changed", correlationID(r))
		return
	}
	auditTarget(r, record.Binding.ConnectionID, agent.UID, record.Binding.ID)
	auditMutation(r)
	// Keep a tombstone so re-creation cannot reuse a previously issued revision.
	record.Binding.Revoked, record.Binding.Enabled = true, false
	record.Binding.Revision++
	record.Binding.UpdatedAt = s.currentTime().UTC()
	if err := s.store.putBinding(r.Context(), record, version); err != nil {
		status, code := 503, "dependency_unavailable"
		if errors.Is(err, errConflict) {
			status, code = 409, "conflict"
		}
		writeAPIError(w, status, code, "Binding changed; reload before deleting", correlationID(r))
		return
	}
	_ = s.store.releaseBindingReservation(r.Context(), owner, record.Binding.ConnectionID, record.ReservationToken)
	auditCommit(r)
	w.WriteHeader(http.StatusNoContent)
}
