// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type serviceRequest = agenttools.Request

func connectionView(c Connection) agenttools.Connection {
	c = projectedConnection(c)
	var identity *agenttools.IdentityBinding
	if c.Identity != nil {
		identity = &agenttools.IdentityBinding{InstanceUID: c.Identity.InstanceUID, TenantID: c.Identity.TenantID, PrincipalID: c.Identity.PrincipalID, ClientID: c.Identity.ClientID}
	}
	return agenttools.Connection{ID: c.ID, Mode: connectionMode(c), Identity: identity, GitHub: c.GitHub, Provider: c.Provider, AccountDisplay: c.AccountDisplay, Services: c.Services, Scopes: c.Scopes, Revision: c.Revision, Status: c.Status, CreatedAt: c.CreatedAt}
}

func bindingView(b ConnectionBinding) agenttools.Binding {
	return agenttools.Binding{ID: b.ID, InstanceUID: b.InstanceUID, ConnectionID: b.ConnectionID, Services: b.Services, Enabled: b.Enabled && !b.Revoked, Revision: b.Revision, GitHub: b.GitHub}
}

func serviceAuthorization(identity workloadIdentity, connection *connectionRecord, binding ConnectionBinding, service string) agenttools.CheckResponse {
	executor := connection.Connection.AccountID
	if connectionMode(connection.Connection) == modeInstallation && connection.Connection.GitHub != nil && connection.Connection.GitHub.InstallationID > 0 {
		executor = "github-installation:" + strconv.FormatInt(connection.Connection.GitHub.InstallationID, 10)
	}
	return agenttools.CheckResponse{ExecutorAccountID: executor, InstanceUID: identity.Agent.UID, Connection: connectionView(connection.Connection), Binding: bindingView(binding), Service: service, ContractVersion: agenttools.ContractVersion, CredentialPolicy: identity.CredentialPolicy}
}

func (s *Server) checkServiceAuthorization(ctx context.Context, input serviceRequest, requireVersions bool) (workloadIdentity, *connectionRecord, ConnectionBinding, error) {
	fail := func(code string, status int) (workloadIdentity, *connectionRecord, ConnectionBinding, error) {
		return workloadIdentity{}, nil, ConnectionBinding{}, toolAuthorizationFailure(code, status)
	}
	identity, err := s.store.verifyWorkloadProof(ctx, input.CallerProof, s.cfg.WorkloadTokenAudience)
	if err != nil {
		if errors.Is(err, errDependencyUnavailable) {
			return fail("dependency_unavailable", 503)
		}
		return fail("unauthenticated", 401)
	}
	if input.ContractVersion != agenttools.ContractVersion {
		return fail("authorization_changed", 409)
	}
	service, ok := agenttools.LookupService(input.Service)
	if !ok || input.ConnectionID == "" {
		return fail("invalid_request", 400)
	}
	if err := s.authorizeCredentialPolicy(ctx, &identity, input.Service, service.Provider); err != nil {
		return workloadIdentity{}, nil, ConnectionBinding{}, err
	}
	if requireVersions && !reflect.DeepEqual(input.CredentialPolicy, identity.CredentialPolicy) {
		return fail("authorization_changed", 409)
	}
	owner := Owner{identity.Agent.OwnerTenantID, identity.Agent.OwnerObjectID}
	// A same-owner Connection is never sufficient: require its deterministic,
	// encrypted per-Agent binding before reading a shared provider credential.
	record, _, err := s.store.getBinding(ctx, bindingKey(identity.Agent.UID, input.ConnectionID), identity.Agent)
	if errors.Is(err, errNotFound) {
		return fail("binding_required", 403)
	}
	if err != nil {
		return fail("dependency_unavailable", 503)
	}
	binding := record.Binding
	if !activeBinding(binding, s.currentTime()) || binding.InstanceUID != identity.Agent.UID || binding.ConnectionID != input.ConnectionID || !sameOwner(binding.Owner, owner) || !contains(binding.Services, input.Service) {
		return fail("permission_denied", 403)
	}
	connection, _, err := s.store.getConnectionForAgent(ctx, identity.Agent, input.ConnectionID)
	if errors.Is(err, errNotFound) {
		return fail("binding_required", 403)
	}
	if err != nil {
		return fail("dependency_unavailable", 503)
	}
	c := connection.Connection
	if !s.cfg.connectionModeEnabled(c) {
		return fail("dependency_unavailable", 503)
	}
	if !sameOwner(c.Owner, owner) || !s.connectionMatchesAgent(c, identity.Agent) || c.Provider != service.Provider || !contains(connectionServices(c), input.Service) {
		return fail("permission_denied", 403)
	}
	if connection.Deleted || c.Status == "disconnected" {
		return fail("binding_required", 403)
	}
	if connectionMode(c) == modeInstallation && (c.GitHub == nil || c.GitHub.InstallationID <= 0 || c.GitHub.AccountLogin == "" || c.GitHub.UserLogin == "" || !binding.GitHub.Valid()) {
		return fail("permission_denied", 403)
	}
	if c.Status != "connected" {
		return fail("interaction_required", 401)
	}
	if requireVersions && (input.BindingRevision == nil || *input.BindingRevision != binding.Revision || input.ConnectionRevision == nil || *input.ConnectionRevision != c.Revision) {
		return fail("authorization_changed", 409)
	}
	return identity, connection, binding, nil
}

func (s *Server) toolAuthorizationsList(w http.ResponseWriter, r *http.Request) {
	var input serviceRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.ContractVersion != agenttools.ContractVersion {
		writeAPIError(w, 409, "authorization_changed", "Service contract changed", correlationID(r))
		return
	}
	identity, err := s.store.verifyWorkloadProof(r.Context(), input.CallerProof, s.cfg.WorkloadTokenAudience)
	if err != nil {
		code, status := "unauthenticated", 401
		if errors.Is(err, errDependencyUnavailable) {
			code, status = "dependency_unavailable", 503
		}
		writeAPIError(w, status, code, "Workload proof could not be verified", correlationID(r))
		return
	}
	owner := Owner{identity.Agent.OwnerTenantID, identity.Agent.OwnerObjectID}
	bindings, err := s.store.listBindings(r.Context(), owner, identity.Agent.UID, identity.Agent.Name)
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Binding storage is unavailable", correlationID(r))
		return
	}
	// Discovery reports owned binding metadata. Per-service credential policy
	// is checked on check/issue, not as a dependency of unrelated connections.
	result := agenttools.ListResponse{InstanceUID: identity.Agent.UID, Connections: []agenttools.Connection{}, Bindings: []agenttools.Binding{}, ContractVersion: agenttools.ContractVersion}
	for _, binding := range bindings {
		if !activeBinding(binding, s.currentTime()) || binding.InstanceUID != identity.Agent.UID || binding.ID != bindingKey(identity.Agent.UID, binding.ConnectionID) || !sameOwner(binding.Owner, owner) {
			continue
		}
		connection, _, err := s.store.getConnectionForAgent(r.Context(), identity.Agent, binding.ConnectionID)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
			return
		}
		if connection.Deleted || !sameOwner(connection.Connection.Owner, owner) || !s.connectionMatchesAgent(connection.Connection, identity.Agent) {
			continue
		}
		result.Connections = append(result.Connections, connectionView(connection.Connection))
		result.Bindings = append(result.Bindings, bindingView(binding))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) toolAuthorizationsCheck(w http.ResponseWriter, r *http.Request) {
	var input serviceRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	identity, connection, binding, err := s.checkServiceAuthorization(r.Context(), input, false)
	if err != nil {
		writeToolAuthorizationError(w, r, err)
		return
	}
	auditTarget(r, connection.Connection.ID, identity.Agent.UID, binding.ID)
	writeJSON(w, http.StatusOK, serviceAuthorization(identity, connection, binding, input.Service))
}

func (s *Server) toolCredentialsIssue(w http.ResponseWriter, r *http.Request) {
	var input serviceRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	identity, connection, binding, err := s.checkServiceAuthorization(r.Context(), input, true)
	if err != nil {
		writeToolAuthorizationError(w, r, err)
		return
	}
	auditTarget(r, connection.Connection.ID, identity.Agent.UID, binding.ID)
	token, expiresAt, err := s.issueServiceCredential(r.Context(), connection, input.Service, identity.Agent, binding)
	if err != nil {
		code, status := "dependency_unavailable", 503
		if category, ok := oauthTokenFailureCategory(err); ok && category == string(oauthTokenInvalidGrant) && isSharedConnectionMode(connectionMode(connection.Connection)) {
			code, status = "interaction_required", 401
		}
		writeAPIError(w, status, code, "Provider credential is unavailable", correlationID(r))
		return
	}
	currentIdentity, current, currentBinding, err := s.checkServiceAuthorization(r.Context(), input, true)
	if err != nil || !reflect.DeepEqual(identity, currentIdentity) || !reflect.DeepEqual(connection.Connection, current.Connection) || !reflect.DeepEqual(binding, currentBinding) || !s.currentTime().Before(expiresAt) {
		writeAPIError(w, 409, "authorization_changed", "Connection or binding changed during credential acquisition", correlationID(r))
		return
	}
	writeJSON(w, http.StatusOK, agenttools.IssueResponse{CheckResponse: serviceAuthorization(currentIdentity, current, currentBinding, input.Service), AccessToken: token, ExpiresAt: expiresAt.UTC().Format(time.RFC3339)})
}

func writeToolAuthorizationError(w http.ResponseWriter, r *http.Request, err error) {
	var typed toolAuthorizationError
	if errors.As(err, &typed) {
		writeAPIError(w, typed.status, typed.code, "Service authorization was denied", correlationID(r))
		return
	}
	writeAPIError(w, 503, "dependency_unavailable", "Service authorization is unavailable", correlationID(r))
}

func decodeJSONDiscard(r *http.Request, target any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}
func opaqueID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		panic("cryptographic random source failed")
	}
	return hex.EncodeToString(data)
}
func bindingKey(uid, connectionID string) string {
	return "binding-" + stateHash(uid + "\x00" + connectionID)[:40]
}
func validProvider(provider string) bool { return len(agenttools.ProviderServices(provider)) > 0 }
func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func uniqueStrings(values []string) []string {
	result := []string{}
	for _, value := range values {
		if value != "" && !contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}
func ifMatchRevision(r *http.Request) (uint64, bool) {
	revision, err := strconv.ParseUint(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	return revision, err == nil && revision > 0
}
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("downstream token has no verifiable expiry")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("downstream token has invalid expiry")
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= time.Now().Unix() {
		return time.Time{}, errors.New("downstream token is expired")
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}

func (s *Server) issueServiceCredential(ctx context.Context, connection *connectionRecord, serviceID string, agent Agent, bindings ...ConnectionBinding) (string, time.Time, error) {
	profile, ok := agenttools.LookupService(serviceID)
	if !ok || profile.Provider != connection.Connection.Provider {
		return "", time.Time{}, errors.New("invalid service")
	}
	if !s.connectionMatchesAgent(connection.Connection, agent) {
		return "", time.Time{}, errors.New("identity binding changed")
	}
	if connectionMode(connection.Connection) == modeAgent {
		return s.issueAgentCredential(ctx, connection.Connection, agent)
	}
	var binding ConnectionBinding
	if len(bindings) == 1 {
		binding = bindings[0]
	}
	if connectionMode(connection.Connection) == modeInstallation {
		return s.issueGitHubInstallationCredential(ctx, connection, binding)
	}
	if connection.Credential == nil {
		return "", time.Time{}, errors.New("credential is unavailable")
	}
	credential := *connection.Credential
	if credential.NonExpiring && (connection.Connection.Provider != "github" || !credential.ExpiresAt.IsZero()) {
		return "", time.Time{}, errors.New("provider credential has an invalid non-expiring marker")
	}
	if !credential.NonExpiring && credential.ExpiresAt.IsZero() {
		return "", time.Time{}, errors.New("provider credential is missing an expiry")
	}
	if !credential.NonExpiring && time.Until(credential.ExpiresAt) < 2*time.Minute {
		provider := s.providers
		if provider == nil {
			provider = newProviderClient(s.cfg)
		}
		refreshed, err := provider.refresh(ctx, connection.Connection.Provider, credential)
		if err != nil {
			if category, ok := oauthTokenFailureCategory(err); ok && category == string(oauthTokenInvalidGrant) {
				latest, version, readErr := s.store.getConnection(ctx, connection.Connection.ID)
				if readErr == nil && !latest.Deleted && latest.Connection.Revision == connection.Connection.Revision && reflect.DeepEqual(latest.Credential, connection.Credential) {
					latest.Connection.Status = "interaction_required"
					latest.Connection.Revision++
					latest.Connection.UpdatedAt = s.currentTime().UTC()
					if writeErr := s.store.putConnection(ctx, latest, version); writeErr != nil {
						return "", time.Time{}, writeErr
					}
				}
			}
			return "", time.Time{}, err
		}
		latest, version, err := s.store.getConnection(ctx, connection.Connection.ID)
		if err != nil || latest.Deleted || latest.Connection.Revision != connection.Connection.Revision {
			return "", time.Time{}, errors.New("connection changed")
		}
		latest.Credential = &refreshed
		if !reflect.DeepEqual(latest.Connection.Scopes, refreshed.Scopes) {
			latest.Connection.Scopes = refreshed.Scopes
			latest.Connection.Revision++
			latest.Connection.UpdatedAt = s.currentTime().UTC()
		}
		if err := s.store.putConnection(ctx, latest, version); err != nil {
			return "", time.Time{}, err
		}
		credential = refreshed
	}
	if !credential.NonExpiring && time.Now().After(credential.ExpiresAt) {
		return "", time.Time{}, errors.New("provider credential expired")
	}
	if connection.Connection.Provider == "azure" || connection.Connection.Provider == "microsoft" {
		if !agent.AgentIdentityReady || agent.AgentIdentityClientID == "" {
			return "", time.Time{}, errors.New("Agent Identity is not ready")
		}
		service := "MicrosoftGraph"
		if serviceID == "azure_arm" {
			service = "AzureResourceManager"
		}
		header, err := s.authSDK.authorizationHeaderFor(ctx, credential.AccessToken, agent.AgentIdentityClientID, service)
		if err != nil {
			return "", time.Time{}, err
		}
		token, err := accessTokenFromAuthorizationHeader(header)
		if err != nil {
			return "", time.Time{}, err
		}
		expiresAt, err := jwtExpiry(token)
		if err != nil {
			return "", time.Time{}, err
		}
		return token, expiresAt, nil
	}
	if connection.Connection.Provider == "github" {
		// GitHub OAuth Apps may issue non-expiring user tokens. This is CetiRun's
		// internal MCP issuance validity, not an upstream token expiry: the Agent
		// runtime never receives this access token. Keep it short so MCP returns
		// for binding and revocation checks.
		expiresAt := s.currentTime().UTC().Add(5 * time.Minute)
		if !credential.NonExpiring && credential.ExpiresAt.Before(expiresAt) {
			expiresAt = credential.ExpiresAt
		}
		if !expiresAt.After(s.currentTime()) {
			return "", time.Time{}, errors.New("provider credential is expiring")
		}
		return credential.AccessToken, expiresAt, nil
	}
	return credential.AccessToken, credential.ExpiresAt, nil
}

func toolServiceTLSPrincipalAllowed(r *http.Request, cfg Config) bool {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	leaf := r.TLS.PeerCertificates[0]
	if len(leaf.URIs) != 1 {
		return false
	}
	expected := "spiffe://cluster.local/ns/" + cfg.ToolServiceNamespace + "/sa/" + cfg.ToolServiceAccount
	return leaf.URIs[0].String() == expected
}
