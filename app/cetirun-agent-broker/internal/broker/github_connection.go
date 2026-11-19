// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func (s *Server) githubAppConfig(w http.ResponseWriter, r *http.Request) {
	if _, err := ownerFromPortal(r); err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified owner identity is required", correlationID(r))
		return
	}
	result := map[string]any{"enabled": s.cfg.githubAppEnabled()}
	if s.cfg.githubAppEnabled() {
		result["installUrl"] = "https://github.com/apps/" + s.cfg.GitHubAppSlug + "/installations/new"
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) githubConnectionForOwner(ctx context.Context, owner Owner, id string) (*connectionRecord, string, error) {
	record, version, err := s.store.getOwnedConnection(ctx, owner, id)
	if err != nil {
		return nil, "", err
	}
	if record == nil {
		return nil, "", errNotFound
	}
	_, validAccountID := githubAppUserID(record.Connection.AccountID)
	if !validAccountID || record.Deleted || !sameOwner(record.Connection.Owner, owner) || connectionMode(record.Connection) != modeInstallation || record.Connection.Provider != "github" || record.Credential == nil || record.Connection.GitHub == nil || record.Connection.GitHub.UserLogin == "" {
		return nil, "", errNotFound
	}
	return record, version, nil
}

func (s *Server) githubUserCredential(ctx context.Context, record *connectionRecord) (providerCredential, error) {
	provider := s.providers
	if provider == nil {
		provider = newProviderClient(s.cfg)
	}
	for attempt := 0; attempt < 4; attempt++ {
		latest, version, err := s.store.getConnection(ctx, record.Connection.ID)
		if err != nil || latest.Deleted || latest.Credential == nil || connectionMode(latest.Connection) != modeInstallation {
			return providerCredential{}, errors.New("connection changed")
		}
		credential := *latest.Credential
		if credential.NonExpiring || credential.ExpiresAt.IsZero() || credential.RefreshToken == "" {
			return providerCredential{}, errors.New("GitHub App user authorization is invalid")
		}
		// Selection may transfer this authorization to the canonical account.
		// Do not rotate its refresh token while that transfer is in progress.
		if latest.PendingInstallation != nil {
			return providerCredential{}, errors.New("GitHub installation selection is in progress")
		}
		if time.Until(credential.ExpiresAt) >= 2*time.Minute {
			return credential, nil
		}
		now := s.currentTime().UTC()
		if latest.RefreshLease != nil {
			if latest.RefreshLease.ExpiresAt.After(now) {
				return providerCredential{}, errors.New("GitHub App user credential refresh is in progress")
			}
			// An expired lease means the upstream exchange might have rotated the
			// token, but the result was not durably recorded. Never retry that
			// refresh token; require a fresh browser authorization instead.
			latest.Connection.Status = "interaction_required"
			latest.Connection.Revision++
			latest.Connection.UpdatedAt = now
			latest.RefreshLease = nil
			if err := s.store.putConnection(ctx, latest, version); errors.Is(err, errConflict) {
				continue
			} else if err != nil {
				return providerCredential{}, err
			}
			return providerCredential{}, newOAuthTokenError(oauthTokenInvalidGrant)
		}
		lease := &credentialRefreshLease{Token: opaqueID(), ExpiresAt: now.Add(30 * time.Second)}
		latest.RefreshLease = lease
		if err := s.store.putConnection(ctx, latest, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return providerCredential{}, err
		}

		refreshed, refreshErr := provider.refreshGitHubAppUser(ctx, credential)
		if refreshErr != nil {
			if category, ok := oauthTokenFailureCategory(refreshErr); ok && category == string(oauthTokenInvalidGrant) {
				// A concurrent reconnect can have installed a new credential after
				// this request obtained its lease. Prefer that live credential.
				if current, _, readErr := s.store.getConnection(ctx, record.Connection.ID); readErr == nil && current.Credential != nil && current.Credential.AccessToken != credential.AccessToken && current.Credential.ExpiresAt.After(s.currentTime().Add(2*time.Minute)) {
					return *current.Credential, nil
				}
				s.markGitHubAppInteractionRequired(ctx, record)
			}
			return providerCredential{}, refreshErr
		}
		for commit := 0; commit < 4; commit++ {
			current, currentVersion, readErr := s.store.getConnection(ctx, record.Connection.ID)
			if readErr != nil || current.Deleted || current.Credential == nil {
				return providerCredential{}, errors.New("connection changed")
			}
			if current.Credential.AccessToken != credential.AccessToken {
				if current.Credential.ExpiresAt.After(s.currentTime().Add(2 * time.Minute)) {
					return *current.Credential, nil
				}
				return providerCredential{}, errors.New("GitHub App user credential changed during refresh")
			}
			if current.RefreshLease == nil || current.RefreshLease.Token != lease.Token {
				return providerCredential{}, errors.New("GitHub App user credential refresh changed")
			}
			current.Credential, current.RefreshLease = &refreshed, nil
			if err := s.store.putConnection(ctx, current, currentVersion); errors.Is(err, errConflict) {
				continue
			} else if err != nil {
				return providerCredential{}, err
			}
			return refreshed, nil
		}
		return providerCredential{}, errors.New("GitHub App user credential refresh could not be committed")
	}
	return providerCredential{}, errors.New("GitHub App user credential changed during refresh")
}

func (s *Server) markGitHubAppInteractionRequired(ctx context.Context, record *connectionRecord) {
	for attempt := 0; attempt < 3; attempt++ {
		latest, version, err := s.store.getConnection(ctx, record.Connection.ID)
		if err == nil && latest.Deleted && latest.PendingInstallation != nil && latest.Connection.CanonicalConnectionID != "" && record.Credential != nil {
			// An in-flight provider request can finish after its temporary
			// connection was consumed. Invalidate only that exact credential.
			target, _, readErr := s.store.getConnection(ctx, latest.Connection.CanonicalConnectionID)
			if readErr == nil && sameGitHubInstallation(installationIdentity(latest), target.Connection) {
				_ = s.store.invalidateGitHubSelectionCredential(ctx, target.Connection, record.Credential.AccessToken, s.currentTime().UTC())
			}
			return
		}
		if err != nil || latest.Deleted || connectionMode(latest.Connection) != modeInstallation || latest.Credential == nil || record.Credential == nil || latest.Credential.AccessToken != record.Credential.AccessToken {
			return
		}
		latest.Connection.Status = "interaction_required"
		latest.Connection.Revision++
		latest.Connection.UpdatedAt = s.currentTime().UTC()
		latest.RefreshLease = nil
		if err := s.store.putConnection(ctx, latest, version); !errors.Is(err, errConflict) {
			return
		}
	}
}

func writeGitHubUserCredentialError(w http.ResponseWriter, r *http.Request, err error) {
	if category, ok := oauthTokenFailureCategory(err); ok && category == string(oauthTokenInvalidGrant) {
		writeAPIError(w, 401, "interaction_required", "GitHub sign-in must be refreshed", correlationID(r))
		return
	}
	writeAPIError(w, 503, "dependency_unavailable", "GitHub sign-in is temporarily unavailable", correlationID(r))
}

func (s *Server) githubInstallations(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, 403, "forbidden", "Verified owner identity is required", correlationID(r))
		return
	}
	record, _, err := s.githubConnectionForOwner(r.Context(), owner, r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		writeAPIError(w, 404, "not_found", "GitHub App connection not found", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
		return
	}
	if err != nil || !s.cfg.githubAppEnabled() {
		writeAPIError(w, 503, "dependency_unavailable", "GitHub App is unavailable", correlationID(r))
		return
	}
	if pending := record.PendingInstallation; pending != nil && pending.AppSlug == s.cfg.GitHubAppSlug && record.Connection.Status == "installation_required" {
		// Keep an interrupted, already verified choice visible after a page
		// reload, even when its user token has since expired. Selecting it
		// resumes the fenced write; this metadata grants no repository access.
		writeJSON(w, 200, map[string]any{"installations": []any{map[string]any{"id": pending.InstallationID, "accountLogin": pending.AccountLogin, "accountType": "", "permissions": map[string]string{}}}, "installUrl": "https://github.com/apps/" + s.cfg.GitHubAppSlug + "/installations/new"})
		return
	}
	credential, err := s.githubUserCredential(r.Context(), record)
	if err != nil {
		writeGitHubUserCredentialError(w, r, err)
		return
	}
	provider := s.providers
	if provider == nil {
		provider = newProviderClient(s.cfg)
	}
	installations, err := provider.githubInstallations(r.Context(), credential.AccessToken)
	if err != nil {
		if errors.Is(err, errGitHubUserUnauthorized) {
			s.markGitHubAppInteractionRequired(r.Context(), record)
			writeAPIError(w, 401, "interaction_required", "GitHub sign-in must be refreshed", correlationID(r))
			return
		}
		writeAPIError(w, 503, "dependency_unavailable", "GitHub installations are unavailable", correlationID(r))
		return
	}
	result := make([]map[string]any, 0, len(installations))
	for _, installation := range installations {
		result = append(result, map[string]any{"id": installation.ID, "accountLogin": installation.Account.Login, "accountType": installation.Account.Type, "permissions": installation.Permissions})
	}
	writeJSON(w, 200, map[string]any{"installations": result, "installUrl": "https://github.com/apps/" + s.cfg.GitHubAppSlug + "/installations/new"})
}

func (s *Server) githubRepositories(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, 403, "forbidden", "Verified owner identity is required", correlationID(r))
		return
	}
	record, _, err := s.githubConnectionForOwner(r.Context(), owner, r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		writeAPIError(w, 404, "not_found", "GitHub App connection not found", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
		return
	}
	installationID, parseErr := strconv.ParseInt(r.URL.Query().Get("installationId"), 10, 64)
	page, pageErr := strconv.Atoi(r.URL.Query().Get("page"))
	if pageErr != nil || page < 1 {
		page = 1
	}
	if parseErr != nil || installationID <= 0 {
		writeAPIError(w, 400, "invalid_request", "Installation ID is required", correlationID(r))
		return
	}
	credential, err := s.githubUserCredential(r.Context(), record)
	if err != nil {
		writeGitHubUserCredentialError(w, r, err)
		return
	}
	provider := s.providers
	if provider == nil {
		provider = newProviderClient(s.cfg)
	}
	if _, err := provider.githubInstallation(r.Context(), credential.AccessToken, installationID); err != nil {
		if errors.Is(err, errGitHubUserUnauthorized) {
			s.markGitHubAppInteractionRequired(r.Context(), record)
			writeAPIError(w, 401, "interaction_required", "GitHub sign-in must be refreshed", correlationID(r))
			return
		}
		writeAPIError(w, 403, "forbidden", "GitHub installation is not available", correlationID(r))
		return
	}
	repositories, more, err := provider.githubInstallationRepositories(r.Context(), credential.AccessToken, installationID, page)
	if err != nil {
		if errors.Is(err, errGitHubUserUnauthorized) {
			s.markGitHubAppInteractionRequired(r.Context(), record)
			writeAPIError(w, 401, "interaction_required", "GitHub sign-in must be refreshed", correlationID(r))
			return
		}
		writeAPIError(w, 503, "dependency_unavailable", "GitHub repositories are unavailable", correlationID(r))
		return
	}
	result := make([]map[string]any, 0, len(repositories))
	userID, _ := githubAppUserID(record.Connection.AccountID)
	for _, repository := range repositories {
		result = append(result, map[string]any{"id": repository.ID, "fullName": repository.FullName, "admin": githubRepositoryAuthorized(repository, userID)})
	}
	writeJSON(w, 200, map[string]any{"repositories": result, "hasNextPage": more})
}

func (s *Server) githubInstallationSelect(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, 403, "forbidden", "Verified owner identity is required", correlationID(r))
		return
	}
	var input struct {
		InstallationID int64 `json:"installationId"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.InstallationID <= 0 {
		writeAPIError(w, 400, "invalid_request", "Installation ID is required", correlationID(r))
		return
	}
	completed, err := s.store.completedGitHubSelection(r.Context(), owner, r.PathValue("id"), input.InstallationID)
	if errors.Is(err, errConflict) {
		writeAPIError(w, 409, "conflict", "GitHub installation cannot be changed; create a new connection", correlationID(r))
		return
	}
	if errors.Is(err, errNotFound) {
		writeAPIError(w, 404, "not_found", "GitHub App connection not found", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
		return
	}
	if completed != nil {
		writeJSON(w, 200, projectedConnection(completed.Connection))
		return
	}
	record, _, err := s.githubConnectionForOwner(r.Context(), owner, r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		writeAPIError(w, 404, "not_found", "GitHub App connection not found", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
		return
	}
	if record.Connection.GitHub.InstallationID != 0 {
		writeAPIError(w, 409, "conflict", "GitHub installation cannot be changed; create a new connection", correlationID(r))
		return
	}
	selection := record.PendingInstallation
	if selection != nil {
		// Finish the exact selection already verified before an interrupted
		// write. Bindings and execution still recheck live GitHub authority.
		if selection.InstallationID != input.InstallationID || selection.AppSlug != s.cfg.GitHubAppSlug {
			writeAPIError(w, 409, "conflict", "Installation selection is already in progress", correlationID(r))
			return
		}
	} else {
		credential, err := s.githubUserCredential(r.Context(), record)
		if err != nil {
			writeGitHubUserCredentialError(w, r, err)
			return
		}
		provider := s.providers
		if provider == nil {
			provider = newProviderClient(s.cfg)
		}
		installation, err := provider.githubInstallation(r.Context(), credential.AccessToken, input.InstallationID)
		if err != nil {
			if errors.Is(err, errGitHubUserUnauthorized) {
				s.markGitHubAppInteractionRequired(r.Context(), record)
				writeAPIError(w, 401, "interaction_required", "GitHub sign-in must be refreshed", correlationID(r))
				return
			}
			writeAPIError(w, 403, "forbidden", "GitHub installation is not available", correlationID(r))
			return
		}
		selection = &agenttools.GitHubInstallation{InstallationID: installation.ID, AppSlug: s.cfg.GitHubAppSlug, AccountLogin: installation.Account.Login}
	}
	selected, err := s.store.selectGitHubInstallation(r.Context(), owner, record.Connection.ID, *selection, s.currentTime().UTC())
	if errors.Is(err, errConflict) || errors.Is(err, errNotFound) {
		writeAPIError(w, 409, "conflict", "Connection changed; reload before selecting an installation", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection storage is unavailable", correlationID(r))
		return
	}
	auditTarget(r, selected.Connection.ID, "", "")
	auditMutation(r)
	auditCommit(r)
	selected.Connection.CanonicalConnectionID = selected.Connection.ID
	writeJSON(w, 200, projectedConnection(selected.Connection))
}

func githubPermissionsAllowed(available, requested map[string]string) bool {
	for permission, wanted := range requested {
		actual := available[permission]
		if actual != wanted && actual != "write" {
			return false
		}
	}
	return true
}

func (s *Server) validateGitHubAppBinding(ctx context.Context, connection *connectionRecord, binding ConnectionBinding) error {
	c := connection.Connection
	userID, validAccountID := githubAppUserID(c.AccountID)
	if connectionMode(c) != modeInstallation || c.GitHub == nil || c.GitHub.InstallationID <= 0 || !validAccountID || !binding.GitHub.Valid() {
		return errors.New("GitHub App binding is invalid")
	}
	credential, err := s.githubUserCredential(ctx, connection)
	if err != nil {
		return err
	}
	provider := s.providers
	if provider == nil {
		provider = newProviderClient(s.cfg)
	}
	installation, err := provider.githubInstallation(ctx, credential.AccessToken, c.GitHub.InstallationID)
	if err != nil || !githubPermissionsAllowed(installation.Permissions, binding.GitHub.Permissions) {
		if errors.Is(err, errGitHubUserUnauthorized) {
			s.markGitHubAppInteractionRequired(ctx, connection)
			return newOAuthTokenError(oauthTokenInvalidGrant)
		}
		return errors.New("GitHub App installation authorization changed")
	}
	if err := s.updateGitHubInstallationAccountLogin(ctx, connection, installation.Account.Login, userID); err != nil {
		return err
	}
	err = provider.githubGrantRepositories(ctx, credential.AccessToken, c.GitHub.InstallationID, userID, binding.GitHub)
	if errors.Is(err, errGitHubUserUnauthorized) {
		s.markGitHubAppInteractionRequired(ctx, connection)
		return newOAuthTokenError(oauthTokenInvalidGrant)
	}
	return err
}

// updateGitHubInstallationAccountLogin persists a renamed installation account
// after its stable installation ID has been revalidated with GitHub.
func (s *Server) updateGitHubInstallationAccountLogin(ctx context.Context, connection *connectionRecord, accountLogin string, userID int64) error {
	if accountLogin == "" || userID <= 0 {
		return errors.New("GitHub App installation authorization changed")
	}
	for attempt := 0; attempt < 4; attempt++ {
		current, version, err := s.store.getConnection(ctx, connection.Connection.ID)
		if err != nil || current.Deleted || current.Connection.Provider != "github" || connectionMode(current.Connection) != modeInstallation || current.Connection.GitHub == nil || current.Connection.GitHub.InstallationID != connection.Connection.GitHub.InstallationID {
			return errors.New("GitHub App connection changed")
		}
		currentUserID, validAccountID := githubAppUserID(current.Connection.AccountID)
		if !validAccountID || currentUserID != userID {
			return errors.New("GitHub App connection changed")
		}
		if current.Connection.GitHub.AccountLogin == accountLogin {
			connection.Connection.GitHub.AccountLogin = accountLogin
			return nil
		}
		current.Connection.GitHub.AccountLogin = accountLogin
		current.Connection.Revision++
		current.Connection.UpdatedAt = s.currentTime().UTC()
		if err := s.store.putConnection(ctx, current, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return err
		}
		connection.Connection.GitHub.AccountLogin = accountLogin
		connection.Connection.Revision = current.Connection.Revision
		return nil
	}
	return errors.New("GitHub App connection changed")
}

func (s *Server) issueGitHubInstallationCredential(ctx context.Context, connection *connectionRecord, binding ConnectionBinding) (string, time.Time, error) {
	c := connection.Connection
	if c.Provider != "github" || connectionMode(c) != modeInstallation || c.GitHub == nil || c.GitHub.InstallationID <= 0 || c.GitHub.AccountLogin == "" || c.GitHub.UserLogin == "" || !binding.GitHub.Valid() || connection.Credential == nil {
		return "", time.Time{}, errors.New("GitHub App binding is invalid")
	}
	// The user still has to be authorized for this exact App installation and
	// every repository on every issuance. This makes revoked membership and
	// removed repositories effective without waiting for a webhook.
	if err := s.validateGitHubAppBinding(ctx, connection, binding); err != nil {
		return "", time.Time{}, err
	}
	provider := s.providers
	if provider == nil {
		provider = newProviderClient(s.cfg)
	}
	return provider.githubInstallationToken(ctx, c.GitHub.InstallationID, binding.GitHub)
}
