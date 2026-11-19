// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

// OAuth transactions and account indexes use the same encrypted envelope as
// Connections, but distinct Secret types and AAD domains. Neither is an account.
func (k *kubeClient) putOAuthRecord(ctx context.Context, kind string, record *connectionRecord, version string) error {
	data, err := k.encodeConnection(ctx, record, kind)
	if err != nil {
		return err
	}
	labels := map[string]string{recordLabelPrefix + "id": record.Connection.ID, recordLabelPrefix + "owner-tenant": record.Connection.Owner.TenantID, recordLabelPrefix + "owner-object": record.Connection.Owner.ObjectID, recordLabelPrefix + "provider": record.Connection.Provider, recordLabelPrefix + "account-id": record.Connection.AccountID}
	return k.putToolSecret(ctx, kind, record.Connection.ID, labels, nil, data, version)
}
func (k *kubeClient) getOAuthRecord(ctx context.Context, kind, id string) (*connectionRecord, string, error) {
	secret, err := k.getToolSecret(ctx, kind, id)
	if err != nil {
		return nil, "", err
	}
	if secret.Metadata.Labels[recordLabelPrefix+"record-type"] != kind || secret.Metadata.Name != toolRecordName(kind, id) {
		return nil, "", errNotFound
	}
	record, err := k.decodeConnection(ctx, secret)
	return record, secret.Metadata.ResourceVersion, err
}

func (s *Server) startAuthorization(w http.ResponseWriter, r *http.Request, owner Owner, provider, target, accountID string) {
	s.startAuthorizationMode(w, r, owner, provider, modeDelegated, target, accountID)
}

func (s *Server) startInstallationAuthorization(w http.ResponseWriter, r *http.Request, owner Owner, target, accountID string) {
	s.startAuthorizationMode(w, r, owner, "github", modeInstallation, target, accountID)
}

func (s *Server) startAuthorizationMode(w http.ResponseWriter, r *http.Request, owner Owner, provider, mode, target, accountID string) {
	if !s.cfg.connectionModeEnabled(Connection{Provider: provider, Mode: mode}) {
		writeAPIError(w, 503, "dependency_unavailable", "This identity mode is disabled", correlationID(r))
		return
	}
	proof, err := connectionBrowserBindingHash(provider, r.Header.Get(connectionBindingHeader))
	if err != nil {
		writeAPIError(w, 400, "invalid_request", "Personal sign-in requires a browser binding proof", correlationID(r))
		return
	}
	state, verifier, nonce, err := authorizationRandomValues(randomValue)
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Sign-in could not be started", correlationID(r))
		return
	}
	client := s.providers
	if client == nil {
		client = newProviderClient(s.cfg)
	}
	var location string
	if mode == modeInstallation {
		location, err = client.githubAppAuthorizationURL(state, verifier)
	} else {
		location, err = client.authorizationURL(provider, state, verifier, nonce)
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Provider OAuth is not configured", correlationID(r))
		return
	}
	now := s.currentTime().UTC()
	// The unguessable state digest also allows an exact lookup without a scan.
	id := "oauth-" + stateHash(state)[:40]
	record := &connectionRecord{Connection: Connection{ID: id, Provider: provider, Mode: mode, Owner: owner, Revision: 1, CreatedAt: now}, Transaction: &connectionTransaction{Provider: provider, Mode: mode, AppSlug: s.cfg.GitHubAppSlug, StateHash: stateHash(state), BrowserBindingHash: proof, CodeVerifier: verifier, Nonce: nonce, ExpectedAccountID: accountID, TargetConnectionID: target, ExpiresAt: now.Add(s.cfg.TransactionTTL), Status: "pending"}}
	if err := s.store.pruneAuthorizations(r.Context(), owner, now); err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Sign-in storage is unavailable", correlationID(r))
		return
	}
	auditTarget(r, target, "", "")
	auditMutation(r)
	if err := s.store.putOAuthRecord(r.Context(), "authorization", record, ""); err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Sign-in could not be started", correlationID(r))
		return
	}
	auditCommit(r)
	writeJSON(w, 200, map[string]any{"authorizationId": id, "connectionId": target, "authorizationUrl": location, "expiresAt": record.Transaction.ExpiresAt, "browserBindingRequired": requiresConnectionBrowserBinding(provider)})
}

// Retain results briefly for the initiating window, then remove abandoned and
// completed transactions during maintenance or the next sign-in.
func (k *kubeClient) pruneAuthorizations(ctx context.Context, owner Owner, now time.Time) error {
	return k.pruneAuthorizationRecords(ctx, &owner, now)
}

func (k *kubeClient) pruneAuthorizationRecords(ctx context.Context, owner *Owner, now time.Time) error {
	selector := ""
	if owner != nil {
		selector = recordLabelPrefix + "owner-tenant=" + owner.TenantID + "," + recordLabelPrefix + "owner-object=" + owner.ObjectID
	}
	list, err := k.listToolSecrets(ctx, "authorization", selector)
	if err != nil {
		return err
	}
	for _, secret := range list.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if secret.Metadata.Labels[recordLabelPrefix+"record-type"] != "authorization" {
			continue
		}
		record, err := k.decodeConnection(ctx, secret)
		if err != nil {
			slog.Warn("OAuth transaction cleanup skipped invalid record", "record_id", secret.Metadata.Name)
			continue
		}
		if owner != nil && !sameOwner(record.Connection.Owner, *owner) {
			return errors.New("OAuth transaction owner changed")
		}
		if record.Credential != nil || record.Transaction == nil || record.Transaction.ExpiresAt.IsZero() || record.Connection.ID == "" || secret.Metadata.Name != toolRecordName("authorization", record.Connection.ID) {
			slog.Warn("OAuth transaction cleanup skipped unexpected record", "record_id", secret.Metadata.Name)
			continue
		}
		if now.After(record.Transaction.ExpiresAt.Add(time.Hour)) {
			if err := k.deleteToolSecret(ctx, secret); err != nil && !errors.Is(err, errNotFound) && !errors.Is(err, errConflict) {
				return err
			}
		}
	}
	return nil
}

func (s *Server) authorizationGet(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, 403, "forbidden", "Verified owner identity is required", correlationID(r))
		return
	}
	record, _, err := s.store.getOAuthRecord(r.Context(), "authorization", r.PathValue("id"))
	if errors.Is(err, errNotFound) || (err == nil && (!sameOwner(record.Connection.Owner, owner) || record.Transaction == nil)) {
		writeAPIError(w, 404, "not_found", "Authorization not found", correlationID(r))
		return
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Authorization storage is unavailable", correlationID(r))
		return
	}
	status := record.Transaction.Status
	if status != "complete" && s.currentTime().After(record.Transaction.ExpiresAt) {
		status = "failed"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"authorizationId": record.Connection.ID, "status": status, "connectionId": record.Transaction.ResultConnectionID})
}

func (s *Server) connectionCallback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if r.ParseForm() != nil {
		s.callbackPage(w, false)
		return
	}
	state, code := r.Form.Get("state"), r.Form.Get("code")
	if !validOAuthState(state) {
		s.callbackPage(w, false)
		return
	}
	id := "oauth-" + stateHash(state)[:40]
	record, version, err := s.store.getOAuthRecord(r.Context(), "authorization", id)
	if err != nil || record.Transaction == nil || record.Transaction.StateHash != stateHash(state) || record.Transaction.Consumed || !validProvider(record.Transaction.Provider) || !s.cfg.connectionModeEnabled(Connection{Provider: record.Transaction.Provider, Mode: record.Transaction.Mode}) || !s.currentTime().Before(record.Transaction.ExpiresAt) {
		s.callbackPage(w, false, id)
		return
	}
	tx := *record.Transaction
	if requiresConnectionBrowserBinding(tx.Provider) && (!portalPrincipalAllowed(r.Header.Get("X-Forwarded-Client-Cert"), s.cfg) || !connectionBrowserBindingMatches(tx, r.Header.Get(connectionBindingHeader))) {
		s.callbackPage(w, false, id)
		return
	}
	record.Transaction.Consumed = true
	// Consume before contacting the provider; two replicas cannot exchange a code.
	if s.store.putOAuthRecord(r.Context(), "authorization", record, version) != nil {
		s.callbackPage(w, false, id)
		return
	}
	result := ""
	if r.Form.Get("error") == "" && validAuthorizationCode(code) {
		credential, display, accountID, finishErr := s.finishConnectionCallback(r.Context(), record, tx, code)
		if finishErr == nil && accountID != "" && (tx.ExpectedAccountID == "" || accountID == tx.ExpectedAccountID) {
			auditOwner(r, record.Connection.Owner)
			auditMutation(r)
			if tx.Mode == modeInstallation {
				result, err = s.store.completeGitHubInstallationAccount(r.Context(), record.Connection.Owner, tx, credential, display, accountID, s.currentTime().UTC())
			} else {
				result, err = s.store.completeAccount(r.Context(), record.Connection.Owner, tx, credential, display, accountID, s.currentTime().UTC())
			}
			if err != nil {
				result = ""
			} else {
				auditTarget(r, result, "", "")
				auditCommit(r)
			}
		}
	}
	current, version, err := s.store.getOAuthRecord(r.Context(), "authorization", id)
	if err != nil || current.Transaction == nil {
		s.callbackPage(w, false, id)
		return
	}
	current.Transaction.Status = "failed"
	current.Transaction.CodeVerifier = ""
	current.Transaction.Nonce = ""
	if result != "" {
		current.Transaction.Status = "complete"
		current.Transaction.ResultConnectionID = result
	}
	if s.store.putOAuthRecord(r.Context(), "authorization", current, version) != nil {
		s.callbackPage(w, false, id)
		return
	}
	s.callbackPage(w, result != "", id)
}

func accountIndexID(owner Owner, provider, accountID string) string {
	return "account-" + stateHash(strings.ToLower(owner.TenantID) + "\x00" + strings.ToLower(owner.ObjectID) + "\x00" + provider + "\x00" + accountID)[:40]
}
func healthyAccount(record *connectionRecord) bool {
	return !record.Deleted && record.Connection.Status == "connected" && record.Credential != nil
}

// The account index is claimed before creating a Connection. Every contender
// creates the same selected ID, so an interrupted first writer is recoverable.
func (k *kubeClient) canonicalAccount(ctx context.Context, owner Owner, provider, accountID string) (string, error) {
	key := accountIndexID(owner, provider, accountID)
	for attempt := 0; attempt < 8; attempt++ {
		index, version, err := k.getOAuthRecord(ctx, "account-index", key)
		if err == nil {
			if !sameOwner(index.Connection.Owner, owner) || index.Connection.Provider != provider || index.Connection.AccountID != accountID {
				return "", errors.New("account index mismatch")
			}
			id := index.Connection.CanonicalConnectionID
			record, _, readErr := k.getConnection(ctx, id)
			if errors.Is(readErr, errNotFound) {
				return id, nil
			}
			if readErr != nil {
				return "", readErr
			}
			if !record.Deleted {
				if !sameOwner(record.Connection.Owner, owner) || record.Connection.Provider != provider || record.Connection.AccountID != accountID {
					return "", errors.New("account index mismatch")
				}
				return id, nil
			}
		} else if !errors.Is(err, errNotFound) {
			return "", err
		}
		records, err := k.connectionRecords(ctx, recordLabelPrefix+"owner-tenant="+owner.TenantID+","+recordLabelPrefix+"owner-object="+owner.ObjectID)
		if err != nil {
			return "", err
		}
		matches := []Connection{}
		for _, item := range records {
			c := item.record.Connection
			if !item.record.Deleted && connectionMode(c) == modeDelegated && sameOwner(c.Owner, owner) && c.Provider == provider && c.AccountID == accountID {
				matches = append(matches, c)
			}
		}
		sort.Slice(matches, func(i, j int) bool {
			if (matches[i].Status == "connected") != (matches[j].Status == "connected") {
				return matches[i].Status == "connected"
			}
			if matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
				return matches[i].ID < matches[j].ID
			}
			return matches[i].CreatedAt.Before(matches[j].CreatedAt)
		})
		id := opaqueID()
		if len(matches) > 0 {
			id = matches[0].ID
		}
		revision := uint64(1)
		if index != nil {
			revision = index.Connection.Revision + 1
		}
		index = &connectionRecord{Connection: Connection{ID: key, Owner: owner, Provider: provider, AccountID: accountID, CanonicalConnectionID: id, Revision: revision}}
		if err := k.putOAuthRecord(ctx, "account-index", index, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return "", err
		}
		return id, nil
	}
	return "", errConflict
}

func (k *kubeClient) completeAccount(ctx context.Context, owner Owner, tx connectionTransaction, credential providerCredential, display, accountID string, now time.Time) (string, error) {
	id := tx.TargetConnectionID
	if id == "" {
		var err error
		id, err = k.canonicalAccount(ctx, owner, tx.Provider, accountID)
		if err != nil {
			return "", err
		}
	}
	for attempt := 0; attempt < 8; attempt++ {
		record, version, err := k.getConnection(ctx, id)
		if errors.Is(err, errNotFound) && tx.TargetConnectionID == "" {
			record = &connectionRecord{Connection: Connection{ID: id, Provider: tx.Provider, Mode: modeDelegated, Owner: owner, AccountID: accountID, CreatedAt: now}}
		} else if err != nil {
			return "", err
		}
		if record.Deleted || !sameOwner(record.Connection.Owner, owner) || record.Connection.Provider != tx.Provider || record.Connection.AccountID != accountID {
			return "", errConflict
		}
		if tx.TargetConnectionID == "" && healthyAccount(record) {
			return id, nil
		}
		record.Credential = &credential
		record.RefreshLease = nil
		record.Transaction = nil
		record.Connection.AccountDisplay = display
		record.Connection.Scopes = credential.Scopes
		record.Connection.Services = connectionServices(record.Connection)
		record.Connection.Status = "connected"
		record.Connection.Revision++
		record.Connection.UpdatedAt = now
		if err := k.putConnection(ctx, record, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return "", err
		}
		return id, nil
	}
	return "", errConflict
}

// App user authorization is deliberately separate from legacy OAuth account
// canonicalization. A shared App connection cannot accidentally reuse a broad
// legacy GitHub OAuth credential for the same GitHub user.
func (k *kubeClient) completeGitHubInstallationAccount(ctx context.Context, owner Owner, tx connectionTransaction, credential providerCredential, display, accountID string, now time.Time) (string, error) {
	if credential.NonExpiring || credential.ExpiresAt.IsZero() || credential.RefreshToken == "" || display == "" || accountID == "" {
		return "", errors.New("GitHub App user authorization is incomplete")
	}
	id := tx.TargetConnectionID
	if id == "" {
		id = opaqueID()
	}
	for attempt := 0; attempt < 8; attempt++ {
		record, version, err := k.getConnection(ctx, id)
		if errors.Is(err, errNotFound) && tx.TargetConnectionID == "" {
			record = &connectionRecord{Connection: Connection{ID: id, Provider: "github", Mode: modeInstallation, Owner: owner, AccountID: "github-app-user-" + accountID, CreatedAt: now}}
		} else if err != nil {
			return "", err
		}
		if record.Deleted || !sameOwner(record.Connection.Owner, owner) || record.Connection.Provider != "github" || connectionMode(record.Connection) != modeInstallation || record.Connection.AccountID != "github-app-user-"+accountID {
			return "", errConflict
		}
		github := record.Connection.GitHub
		if pending := record.PendingInstallation; pending != nil {
			// A provider request started before selection can invalidate this
			// authorization after its installation choice has been locked. Let
			// the same user reconnect, retaining the choice and account-index
			// claim so selection can resume without changing its identity.
			if record.Connection.Status != "interaction_required" || github == nil || github.InstallationID != 0 || tx.AppSlug == "" || pending.AppSlug != tx.AppSlug || github.AppSlug != tx.AppSlug || pending.InstallationID <= 0 || len(record.References) != 0 || record.RefreshLease != nil {
				return "", errConflict
			}
		}
		if tx.TargetConnectionID != "" && (github == nil || github.InstallationID < 0 || (github.InstallationID == 0 && github.AccountLogin != "") || (github.InstallationID > 0 && github.AccountLogin == "")) {
			return "", errConflict
		}
		record.Credential = &credential
		record.RefreshLease = nil
		record.Transaction = nil
		if tx.AppSlug == "" {
			record.Connection.AccountDisplay = display
		} else {
			record.Connection.AccountDisplay = tx.AppSlug + "[bot]"
		}
		record.Connection.AccountID = "github-app-user-" + accountID
		if tx.TargetConnectionID == "" {
			github = &agenttools.GitHubInstallation{UserLogin: display, AppSlug: tx.AppSlug}
		} else {
			if tx.AppSlug != "" {
				github.AppSlug = tx.AppSlug
			}
			github.UserLogin = display
		}
		record.Connection.GitHub = github
		record.Connection.Services = connectionServices(record.Connection)
		record.Connection.Scopes = credential.Scopes
		if github.InstallationID > 0 {
			record.Connection.Status = "connected"
		} else {
			record.Connection.Status = "installation_required"
		}
		record.Connection.Revision++
		record.Connection.UpdatedAt = now
		if err := k.putConnection(ctx, record, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return "", err
		}
		return id, nil
	}
	return "", errConflict
}

func (k *kubeClient) projectCanonicalConnections(ctx context.Context, connections []Connection) error {
	for i := range connections {
		c := &connections[i]
		if c.AccountID == "" {
			continue
		}
		var id string
		var err error
		switch connectionMode(*c) {
		case modeDelegated:
			id, err = k.canonicalAccount(ctx, c.Owner, c.Provider, c.AccountID)
		case modeInstallation:
			if !sameGitHubInstallation(*c, *c) {
				continue
			}
			id, err = k.canonicalGitHubInstallation(ctx, *c)
			// A removal can win after listConnections took its snapshot.
			if errors.Is(err, errConflict) || errors.Is(err, errNotFound) {
				continue
			}
		default:
			continue
		}
		if err != nil {
			return err
		}
		c.CanonicalConnectionID = id
	}
	return nil
}
