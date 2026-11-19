// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

const connectionBindingHeader = "X-CetiRun-Connection-Binding"

type connectionStartRequest struct {
	Provider string `json:"provider"`
	Mode     string `json:"mode,omitempty"`
}
type toolAuthorizationError struct {
	code   string
	status int
}

var errPortalBindingRuntimeNotReady = errors.New("Agent runtime is not ready")

func (e toolAuthorizationError) Error() string { return e.code }

func toolAuthorizationFailure(code string, status int) error {
	return toolAuthorizationError{code: code, status: status}
}

func ownerFromPortal(r *http.Request) (Owner, error) {
	owner := Owner{TenantID: r.Header.Get("X-CetiRun-Owner-Tenant-ID"), ObjectID: r.Header.Get("X-CetiRun-Owner-Object-ID")}
	if !uuidPattern.MatchString(owner.TenantID) || !uuidPattern.MatchString(owner.ObjectID) {
		return Owner{}, errors.New("verified owner identity is required")
	}
	return owner, nil
}

func (s *Server) portalBindingAgent(r *http.Request, owner Owner) (Agent, error) {
	agent, err := s.store.readAgent(r.Context(), r.PathValue("name"))
	if err != nil {
		return Agent{}, err
	}
	if agent.Deleting || !sameOwner(Owner{agent.OwnerTenantID, agent.OwnerObjectID}, owner) || subtle.ConstantTimeCompare([]byte(agent.UID), []byte(r.Header.Get("X-CetiRun-Instance-UID"))) != 1 {
		return Agent{}, errors.New("instance ownership is invalid")
	}
	if agent.Namespace == "" {
		return Agent{}, errPortalBindingRuntimeNotReady
	}
	return agent, nil
}

func (s *Server) connectionsList(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(r)
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner identity is required", correlation)
		return
	}
	connections, err := s.store.listConnections(r.Context(), owner)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection storage is unavailable", correlation)
		return
	}
	if err := s.store.projectCanonicalConnections(r.Context(), connections); err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Account index is unavailable", correlation)
		return
	}
	for index := range connections {
		connections[index] = projectedConnection(connections[index])
	}
	writeJSON(w, http.StatusOK, connections)
}

func (s *Server) connectionStart(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(r)
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner identity is required", correlation)
		return
	}
	var input connectionStartRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if !validProvider(input.Provider) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "The provider is not supported", correlation)
		return
	}
	_, err = connectionBrowserBindingHash(input.Provider, r.Header.Get(connectionBindingHeader))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Personal sign-in requires a browser binding proof", correlation)
		return
	}
	if input.Mode != "" && input.Mode != modeInstallation {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "The connection mode is not supported", correlation)
		return
	}
	if input.Mode == modeInstallation {
		if input.Provider != "github" || !s.cfg.githubAppEnabled() {
			writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "GitHub App connections are not configured", correlation)
			return
		}
		s.startInstallationAuthorization(w, r, owner, "", "")
		return
	}
	s.startAuthorization(w, r, owner, input.Provider, "", "")
}

func (s *Server) connectionGet(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(r)
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner identity is required", correlation)
		return
	}
	record, _, err := s.store.getOwnedConnection(r.Context(), owner, r.PathValue("id"))
	if errors.Is(err, errNotFound) || (err == nil && (record.Deleted || !sameOwner(record.Connection.Owner, owner))) {
		writeAPIError(w, http.StatusNotFound, "not_found", "Connection not found", correlation)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection storage is unavailable", correlation)
		return
	}
	writeJSON(w, http.StatusOK, record.Connection)
}

func (s *Server) connectionDelete(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(r)
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner identity is required", correlation)
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		record, version, err := s.store.getOwnedConnection(r.Context(), owner, r.PathValue("id"))
		if errors.Is(err, errNotFound) || (err == nil && (record.Deleted || !sameOwner(record.Connection.Owner, owner))) {
			writeAPIError(w, http.StatusNotFound, "not_found", "Connection not found", correlation)
			return
		}
		if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection storage is unavailable", correlation)
			return
		}
		if !isSharedConnectionMode(connectionMode(record.Connection)) {
			writeAPIError(w, 400, "invalid_request", "Disconnect this identity from its Agent instead", correlation)
			return
		}
		if err := s.store.reconcileReservations(r.Context(), record, s.currentTime()); err != nil {
			writeAPIError(w, 503, "dependency_unavailable", "Connection usage is unavailable", correlation)
			return
		}
		// Re-read after reconciliation; this version also fences concurrent
		// registrations while legacy bindings are checked across namespaces.
		record, version, err = s.store.getOwnedConnection(r.Context(), owner, r.PathValue("id"))
		if err != nil || record.Deleted {
			writeAPIError(w, 409, "conflict", "Connection changed; reload", correlation)
			return
		}
		usage, err := s.store.usage(r.Context(), record)
		if err != nil {
			writeAPIError(w, 503, "dependency_unavailable", "Connection usage is unavailable", correlation)
			return
		}
		if !usage.CanRemove {
			writeAPIError(w, 409, "connection_in_use", "Disconnect this account from its Agents before removing it", correlation)
			return
		}
		auditTarget(r, record.Connection.ID, "", "")
		auditMutation(r)
		record.Deleted, record.Credential, record.Transaction = true, nil, nil
		record.Connection.Status, record.Connection.Services, record.Connection.AccountDisplay, record.Connection.AccountID = "disconnected", nil, "", ""
		record.Connection.Revision++
		record.Connection.UpdatedAt = s.currentTime().UTC()
		if err := s.store.putConnection(r.Context(), record, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection could not be removed", correlation)
			return
		}
		auditCommit(r)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeAPIError(w, http.StatusConflict, "conflict", "Connection state changed; retry", correlation)
}

func (s *Server) connectionReconnect(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(r)
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner identity is required", correlation)
		return
	}
	record, _, err := s.store.getOwnedConnection(r.Context(), owner, r.PathValue("id"))
	if errors.Is(err, errNotFound) || (err == nil && (record.Deleted || !sameOwner(record.Connection.Owner, owner))) {
		writeAPIError(w, 404, "not_found", "Connection not found", correlation)
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlation)
		return
	}
	if !isSharedConnectionMode(connectionMode(record.Connection)) || record.Connection.AccountID == "" {
		writeAPIError(w, 400, "invalid_request", "This identity does not support personal sign-in", correlation)
		return
	}
	if connectionMode(record.Connection) == modeInstallation {
		accountID := strings.TrimPrefix(record.Connection.AccountID, "github-app-user-")
		if accountID == "" || accountID == record.Connection.AccountID {
			writeAPIError(w, 409, "conflict", "GitHub App connection identity is invalid", correlation)
			return
		}
		s.startInstallationAuthorization(w, r, owner, record.Connection.ID, accountID)
		return
	}
	s.startAuthorization(w, r, owner, record.Connection.Provider, record.Connection.ID, record.Connection.AccountID)
}

func requiresConnectionBrowserBinding(provider string) bool {
	return provider == "github" || provider == "google"
}

func connectionBrowserBindingHash(provider, proof string) (string, error) {
	if !requiresConnectionBrowserBinding(provider) {
		return "", nil
	}
	if !validConnectionBrowserBindingProof(proof) {
		return "", errors.New("connection browser binding proof is invalid")
	}
	return stateHash(proof), nil
}

func connectionBrowserBindingMatches(tx connectionTransaction, proof string) bool {
	if !validConnectionBrowserBindingProof(proof) || tx.BrowserBindingHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stateHash(proof)), []byte(tx.BrowserBindingHash)) == 1
}

func validConnectionBrowserBindingProof(proof string) bool {
	if !validOAuthState(proof) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(proof)
	return err == nil && len(decoded) == 32
}

func (s *Server) finishConnectionCallback(ctx context.Context, record *connectionRecord, tx connectionTransaction, code string) (providerCredential, string, string, error) {
	if tx.Mode == modeInstallation {
		provider := s.providers
		if provider == nil {
			provider = newProviderClient(s.cfg)
		}
		return provider.exchangeGitHubAppUserCode(ctx, code, tx.CodeVerifier)
	}
	if tx.Provider == "azure" || tx.Provider == "microsoft" {
		oauth := s.oauth
		if oauth == nil {
			oauth = newOAuthClient(s.cfg)
		}
		tokens, err := oauth.exchangeCode(ctx, code, tx.CodeVerifier)
		if err != nil || tokens.RefreshToken == "" || tokens.IDToken == "" {
			return providerCredential{}, "", "", errors.New("Microsoft OAuth exchange failed")
		}
		display, err := oauth.verifier.verifyIDToken(ctx, tokens.IDToken, tx.Nonce, record.Connection.Owner.TenantID, record.Connection.Owner.ObjectID)
		if err != nil {
			return providerCredential{}, "", "", err
		}
		if err := s.authSDK.validate(ctx, tokens.AccessToken, record.Connection.Owner.TenantID, s.cfg.BlueprintClientID, s.cfg.AuthorizationClientID, record.Connection.Owner.ObjectID); err != nil {
			return providerCredential{}, "", "", err
		}
		return credentialFromOAuth(tokens), display, record.Connection.Owner.ObjectID, nil
	}
	provider := s.providers
	if provider == nil {
		provider = newProviderClient(s.cfg)
	}
	return provider.exchangeCode(ctx, tx.Provider, code, tx.CodeVerifier)
}
