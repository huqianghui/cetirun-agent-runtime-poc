// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func sameGitHubInstallation(a, b Connection) bool {
	return sameOwner(a.Owner, b.Owner) && a.Provider == "github" && b.Provider == "github" &&
		connectionMode(a) == modeInstallation && connectionMode(b) == modeInstallation &&
		a.AccountID != "" && a.AccountID == b.AccountID && a.GitHub != nil && b.GitHub != nil &&
		a.GitHub.AppSlug != "" && b.GitHub.AppSlug != "" &&
		a.GitHub.InstallationID > 0 && a.GitHub.InstallationID == b.GitHub.InstallationID
}

func installationIdentity(record *connectionRecord) Connection {
	c := record.Connection
	if record.PendingInstallation != nil && c.GitHub != nil && c.GitHub.InstallationID == 0 {
		c.GitHub = record.PendingInstallation
	}
	return c
}

// GitHub installation IDs identify the installation across App renames. The
// mutable App slug is display metadata, not part of the canonical identity.
// Claim before promotion: independently running replicas converge on one ID.
// An interrupted request leaves an unbindable source that another request can
// finish. Historical connected records keep their IDs and binding references.
func (k *kubeClient) canonicalGitHubInstallation(ctx context.Context, c Connection) (string, error) {
	if !sameGitHubInstallation(c, c) {
		return "", errConflict
	}
	key := accountIndexID(c.Owner, "github-installation", c.AccountID+"\x00"+strconv.FormatInt(c.GitHub.InstallationID, 10))
	for attempt := 0; attempt < 8; attempt++ {
		index, version, err := k.getOAuthRecord(ctx, "account-index", key)
		if err == nil {
			if !sameGitHubInstallation(index.Connection, c) {
				return "", errConflict
			}
			target, _, readErr := k.getConnection(ctx, index.Connection.CanonicalConnectionID)
			if readErr != nil && !errors.Is(readErr, errNotFound) {
				return "", readErr
			}
			if readErr == nil && !target.Deleted && (target.PendingInstallation == nil || target.Connection.Status == "installation_required") {
				if !sameGitHubInstallation(installationIdentity(target), c) {
					return "", errConflict
				}
				return target.Connection.ID, nil
			}
		} else if !errors.Is(err, errNotFound) {
			return "", err
		}
		records, err := k.connectionRecords(ctx, recordLabelPrefix+"owner-tenant="+c.Owner.TenantID+","+recordLabelPrefix+"owner-object="+c.Owner.ObjectID)
		if err != nil {
			return "", err
		}
		matches := []Connection{}
		for _, item := range records {
			if !item.record.Deleted && (item.record.PendingInstallation == nil || item.record.Connection.Status == "installation_required") && sameGitHubInstallation(installationIdentity(item.record), c) {
				matches = append(matches, item.record.Connection)
			}
		}
		if len(matches) == 0 {
			return "", errConflict
		}
		sort.Slice(matches, func(i, j int) bool {
			if (matches[i].Status == "connected") != (matches[j].Status == "connected") {
				return matches[i].Status == "connected"
			}
			// Prefer an existing selected connection over a new sign-in, even
			// when the existing connection needs renewed authorization.
			if (matches[i].GitHub.InstallationID > 0) != (matches[j].GitHub.InstallationID > 0) {
				return matches[i].GitHub.InstallationID > 0
			}
			if matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
				return matches[i].ID < matches[j].ID
			}
			return matches[i].CreatedAt.Before(matches[j].CreatedAt)
		})
		entry := c
		entry.ID, entry.CanonicalConnectionID, entry.Revision = key, matches[0].ID, 1
		if index != nil {
			entry.Revision = index.Connection.Revision + 1
		}
		if err := k.putOAuthRecord(ctx, "account-index", &connectionRecord{Connection: entry}, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return "", err
		}
		return entry.CanonicalConnectionID, nil
	}
	return "", errConflict
}

// Resolve only a completed selection (including a consumed temporary source).
// This makes a retry after a lost response safe without reviving deleted access.
func (k *kubeClient) completedGitHubSelection(ctx context.Context, owner Owner, id string, installationID int64) (*connectionRecord, error) {
	source, _, err := k.getConnection(ctx, id)
	if err != nil {
		return nil, err
	}
	if !sameOwner(source.Connection.Owner, owner) || connectionMode(source.Connection) != modeInstallation || source.Connection.Provider != "github" || source.Connection.GitHub == nil {
		return nil, errNotFound
	}
	if source.Deleted {
		if source.PendingInstallation == nil || source.Connection.CanonicalConnectionID == "" {
			return nil, errNotFound
		}
		target, _, err := k.getConnection(ctx, source.Connection.CanonicalConnectionID)
		if err != nil {
			return nil, err
		}
		if target.Deleted || !sameGitHubInstallation(installationIdentity(source), target.Connection) {
			return nil, errNotFound
		}
		source = target
	}
	if source.Connection.GitHub.InstallationID == 0 {
		return nil, nil
	}
	if source.Connection.GitHub.InstallationID != installationID {
		return nil, errConflict
	}
	return source, nil
}

func (k *kubeClient) selectGitHubInstallation(ctx context.Context, owner Owner, sourceID string, github agenttools.GitHubInstallation, now time.Time) (*connectionRecord, error) {
	var source *connectionRecord
	for attempt := 0; attempt < 8; attempt++ {
		var version string
		var err error
		source, version, err = k.getConnection(ctx, sourceID)
		if err != nil {
			return nil, err
		}
		if !sameOwner(source.Connection.Owner, owner) || source.Connection.Provider != "github" || connectionMode(source.Connection) != modeInstallation || source.Connection.GitHub == nil || source.Connection.GitHub.AppSlug != github.AppSlug {
			return nil, errNotFound
		}
		if source.Deleted || source.Connection.GitHub.InstallationID > 0 {
			return k.completedGitHubSelection(ctx, owner, sourceID, github.InstallationID)
		}
		if source.Connection.Status != "installation_required" || source.Credential == nil || source.RefreshLease != nil || len(source.References) != 0 {
			return nil, errConflict
		}
		if source.PendingInstallation != nil {
			if source.PendingInstallation.InstallationID != github.InstallationID || source.PendingInstallation.AppSlug != github.AppSlug {
				return nil, errConflict
			}
			break
		}
		github.UserLogin = source.Connection.GitHub.UserLogin
		source.PendingInstallation = &github
		source.Connection.Revision++
		source.Connection.UpdatedAt = now
		if err := k.putConnection(ctx, source, version); errors.Is(err, errConflict) {
			source = nil
			continue
		} else if err != nil {
			return nil, err
		}
		break
	}
	if source == nil {
		return nil, errConflict
	}
	identity := installationIdentity(source)
	id, err := k.canonicalGitHubInstallation(ctx, identity)
	if err != nil {
		return nil, err
	}
	var selected *connectionRecord
	for attempt := 0; attempt < 8; attempt++ {
		current, version, err := k.getConnection(ctx, id)
		if err != nil {
			return nil, err
		}
		if current.Deleted || !sameGitHubInstallation(installationIdentity(current), identity) {
			return nil, errConflict
		}
		if current.RefreshLease != nil {
			return nil, errConflict
		}
		if healthyAccount(current) {
			if current.Connection.GitHub.AppSlug != identity.GitHub.AppSlug {
				// The selection was verified against the configured App ID.
				// Refresh renamed App metadata without replacing credentials or
				// changing any Agent's repository and permission selections.
				current.Connection.GitHub.AppSlug = identity.GitHub.AppSlug
				current.Connection.AccountDisplay = identity.GitHub.AppSlug + "[bot]"
				current.Connection.Revision++
				current.Connection.UpdatedAt = now
				if err := k.putConnection(ctx, current, version); errors.Is(err, errConflict) {
					continue
				} else if err != nil {
					return nil, err
				}
			}
			selected = current
			break
		}
		if current.Connection.GitHub.InstallationID == 0 && current.Connection.Status != "installation_required" {
			return nil, errConflict
		}
		// Keep a pending winner's latest credential, which may have refreshed
		// since selection started. Repair an old disconnected connection using
		// this newly verified authorization without changing its Agent grants.
		if current.Connection.GitHub.InstallationID > 0 {
			fresh, _, err := k.getConnection(ctx, sourceID)
			if err != nil {
				return nil, err
			}
			if fresh.Deleted || fresh.Connection.Status != "installation_required" || fresh.RefreshLease != nil || !sameGitHubInstallation(installationIdentity(fresh), identity) {
				return nil, errConflict
			}
			current.Credential = fresh.Credential
		}
		if current.Credential == nil {
			return nil, errConflict
		}
		selection := *identity.GitHub
		selection.UserLogin = current.Connection.GitHub.UserLogin
		current.Connection.GitHub = &selection
		current.Connection.AccountDisplay = selection.AppSlug + "[bot]"
		current.PendingInstallation = nil
		current.Connection.Status = "connected"
		current.Connection.Revision++
		current.Connection.UpdatedAt = now
		if err := k.putConnection(ctx, current, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return nil, err
		}
		selected = current
		break
	}
	if selected == nil {
		return nil, errConflict
	}
	if id != sourceID {
		if err := k.consumeGitHubSelection(ctx, sourceID, selected.Connection, now); err != nil {
			return nil, err
		}
	}
	return k.liveGitHubSelection(ctx, selected.Connection)
}

func (k *kubeClient) liveGitHubSelection(ctx context.Context, selected Connection) (*connectionRecord, error) {
	current, _, err := k.getConnection(ctx, selected.ID)
	if err != nil {
		return nil, err
	}
	if current.Deleted || !sameGitHubInstallation(current.Connection, selected) {
		return nil, errConflict
	}
	return current, nil
}

func (k *kubeClient) consumeGitHubSelection(ctx context.Context, sourceID string, selected Connection, now time.Time) error {
	for attempt := 0; attempt < 8; attempt++ {
		source, version, err := k.getConnection(ctx, sourceID)
		if err != nil {
			return err
		}
		if !sameGitHubInstallation(installationIdentity(source), selected) || source.Connection.GitHub.InstallationID != 0 || len(source.References) != 0 {
			return errConflict
		}
		if source.Deleted {
			if source.Connection.CanonicalConnectionID == selected.ID {
				return nil
			}
			return errConflict
		}
		if source.Connection.Status != "installation_required" {
			if source.Credential != nil {
				if err := k.invalidateGitHubSelectionCredential(ctx, selected, source.Credential.AccessToken, now); err != nil {
					return err
				}
			}
			return errConflict
		}
		// Keep the pending authorization if removal already won. A later
		// removal still wins and must never resurrect either connection.
		if _, err := k.liveGitHubSelection(ctx, selected); err != nil {
			return err
		}
		source.Deleted = true
		source.Credential, source.RefreshLease, source.Transaction = nil, nil, nil
		source.Connection.CanonicalConnectionID = selected.ID
		source.Connection.Status = "disconnected"
		source.Connection.Revision++
		source.Connection.UpdatedAt = now
		if err := k.putConnection(ctx, source, version); errors.Is(err, errConflict) {
			continue
		} else {
			return err
		}
	}
	return errConflict
}

func (k *kubeClient) invalidateGitHubSelectionCredential(ctx context.Context, selected Connection, token string, now time.Time) error {
	for attempt := 0; attempt < 8; attempt++ {
		current, version, err := k.getConnection(ctx, selected.ID)
		if err != nil {
			return err
		}
		if current.Deleted || !sameGitHubInstallation(current.Connection, selected) || current.Credential == nil || current.Credential.AccessToken != token {
			return nil
		}
		current.Connection.Status = "interaction_required"
		current.Connection.Revision++
		current.Connection.UpdatedAt = now
		current.RefreshLease = nil
		if err := k.putConnection(ctx, current, version); errors.Is(err, errConflict) {
			continue
		} else {
			return err
		}
	}
	return errConflict
}
