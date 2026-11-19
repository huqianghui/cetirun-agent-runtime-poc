// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func createPendingGitHubAppConnection(t *testing.T, f *authorizationFixture, owner Owner, userID, appSlug, token string) string {
	t.Helper()
	id, err := f.server.store.completeGitHubInstallationAccount(context.Background(), owner,
		connectionTransaction{Provider: "github", Mode: modeInstallation, AppSlug: appSlug},
		providerCredential{AccessToken: token, RefreshToken: token + "-refresh", ExpiresAt: time.Now().Add(time.Hour)},
		"octocat", userID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func selectPendingGitHubInstallation(t *testing.T, f *authorizationFixture, owner Owner, id, appSlug string, installationID int64) *connectionRecord {
	t.Helper()
	record, err := f.server.store.selectGitHubInstallation(context.Background(), owner, id,
		agenttools.GitHubInstallation{InstallationID: installationID, AppSlug: appSlug, AccountLogin: "example-org"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestGitHubInstallationSelectionConvergesConcurrentSignIns(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	first := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "first")
	second := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "second")

	// The in-memory Secret fixture models Kubernetes CAS but its map needs the
	// same serialization that independently running Broker replicas receive.
	transport := f.server.store.client.Transport
	var transportMu sync.Mutex
	f.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		transportMu.Lock()
		defer transportMu.Unlock()
		return transport.RoundTrip(request)
	})

	ids := make(chan string, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, source := range []string{first, second} {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			record, err := f.server.store.selectGitHubInstallation(context.Background(), owner, source,
				agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org"}, time.Now().UTC())
			if err == nil {
				ids <- record.Connection.ID
			}
			errs <- err
		}(source)
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	selected := ""
	for id := range ids {
		if selected != "" && selected != id {
			t.Fatalf("concurrent selections diverged: %q and %q", selected, id)
		}
		selected = id
	}
	connections, err := f.server.store.listConnections(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, connection := range connections {
		if connectionMode(connection) == modeInstallation && connection.AccountID == "github-app-user-42" && connection.GitHub != nil && connection.GitHub.AppSlug == "cetirun" && connection.GitHub.InstallationID == 17 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("listed selected installation connections = %d, want one", count)
	}
}

func TestGitHubInstallationSelectionPreservesHealthyCanonicalConnection(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	targetID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "target")
	target := selectPendingGitHubInstallation(t, f, owner, targetID, "cetirun", 17)
	binding := &bindingRecord{Binding: ConnectionBinding{ID: "target-binding", InstanceUID: f.agent.Metadata.UID, ConnectionID: targetID, Owner: owner, Enabled: true, Revision: 1}}
	if err := f.server.store.putBinding(context.Background(), binding, ""); err != nil {
		t.Fatal(err)
	}
	before, _, err := f.server.store.getConnection(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "newer")
	selected := selectPendingGitHubInstallation(t, f, owner, sourceID, "cetirun", 17)
	if selected.Connection.ID != target.Connection.ID {
		t.Fatalf("selected %q, want existing healthy %q", selected.Connection.ID, target.Connection.ID)
	}
	after, _, err := f.server.store.getConnection(context.Background(), targetID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("healthy canonical connection was changed: before=%#v after=%#v err=%v", before, after, err)
	}
	storedBinding, _, err := f.server.store.getBinding(context.Background(), binding.Binding.ID)
	if err != nil || !reflect.DeepEqual(storedBinding.Binding, binding.Binding) {
		t.Fatalf("canonical binding changed: %#v %v", storedBinding, err)
	}
	consumed, _, err := f.server.store.getConnection(context.Background(), sourceID)
	if err != nil || !consumed.Deleted || consumed.Connection.CanonicalConnectionID != targetID || consumed.Credential != nil {
		t.Fatalf("duplicate source was not safely consumed: %#v %v", consumed, err)
	}
}

func TestGitHubInstallationSelectionDoesNotConsumeSourceAfterCanonicalDeletion(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	targetID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "target")
	target := selectPendingGitHubInstallation(t, f, owner, targetID, "cetirun", 17)
	sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "source")
	source, sourceVersion, err := f.server.store.getConnection(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	source.PendingInstallation = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}
	if err := f.server.store.putConnection(context.Background(), source, sourceVersion); err != nil {
		t.Fatal(err)
	}
	canonical, canonicalVersion, err := f.server.store.getConnection(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	canonical.Deleted = true
	canonical.Connection.Revision++
	canonical.Connection.UpdatedAt = time.Now().UTC()
	if err := f.server.store.putConnection(context.Background(), canonical, canonicalVersion); err != nil {
		t.Fatal(err)
	}
	if err := f.server.store.consumeGitHubSelection(context.Background(), sourceID, target.Connection, time.Now().UTC()); !errors.Is(err, errConflict) {
		t.Fatalf("consumed source after canonical deletion: %v", err)
	}
	current, _, err := f.server.store.getConnection(context.Background(), sourceID)
	if err != nil || current.Deleted || current.Credential == nil || current.PendingInstallation == nil {
		t.Fatalf("source authorization was lost after canonical deletion: %#v %v", current, err)
	}
}

func TestGitHubInstallationProjectionToleratesDeletionAfterListSnapshot(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	id := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "snapshot")
	selectPendingGitHubInstallation(t, f, owner, id, "cetirun", 17)
	snapshot, err := f.server.store.listConnections(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	record, version, err := f.server.store.getConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	record.Deleted = true
	if err := f.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	if err := f.server.store.projectCanonicalConnections(context.Background(), snapshot); err != nil {
		t.Fatalf("projection failed after listed App connection was deleted: %v", err)
	}
}

func TestGitHubInstallationSelectionDoesNotReturnDeletedCanonicalAfterSourceConsumption(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	targetID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "target")
	selectPendingGitHubInstallation(t, f, owner, targetID, "cetirun", 17)
	sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "source")
	source, sourceVersion, err := f.server.store.getConnection(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	source.PendingInstallation = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}
	if err := f.server.store.putConnection(context.Background(), source, sourceVersion); err != nil {
		t.Fatal(err)
	}
	target, targetVersion, err := f.server.store.getConnection(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	target.Deleted = true
	target.Connection.Revision++
	target.Connection.UpdatedAt = time.Now().UTC()
	deletedData, err := f.server.store.encodeConnection(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	deletedSecret := f.secrets[toolRecordName("connection", targetID)]
	deletedSecret.Data = deletedData
	deletedSecret.Metadata.ResourceVersion = targetVersion + "-deleted"

	transport := f.server.store.client.Transport
	deletedAfterConsumption := false
	f.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(request)
		if err == nil && request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/"+toolRecordName("connection", sourceID)) && !deletedAfterConsumption {
			// The original transport has persisted the temporary-source tombstone.
			// Simulate an administrator deleting the target immediately afterward.
			f.secrets[toolRecordName("connection", targetID)] = deletedSecret
			deletedAfterConsumption = true
		}
		return response, err
	})
	if _, err := f.server.store.selectGitHubInstallation(context.Background(), owner, sourceID,
		agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org"}, time.Now().UTC()); !errors.Is(err, errConflict) {
		t.Fatalf("selection returned a canonical connection deleted after consumption: %v", err)
	}
	if !deletedAfterConsumption {
		t.Fatal("test hook did not observe consumed-source tombstone")
	}
}

func TestGitHubInstallationIdentityDoesNotMergeAcrossBoundaries(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	otherOwner := owner
	otherOwner.ObjectID = "99999999-9999-9999-9999-999999999999"
	selectOne := func(owner Owner, userID, slug string, installationID int64) string {
		id := createPendingGitHubAppConnection(t, f, owner, userID, slug, slug+userID)
		return selectPendingGitHubInstallation(t, f, owner, id, slug, installationID).Connection.ID
	}
	base := selectOne(owner, "42", "cetirun", 17)
	for _, candidate := range []string{
		selectOne(owner, "43", "cetirun", 17),
		selectOne(owner, "42", "other-app", 19),
		selectOne(owner, "42", "cetirun", 18),
		selectOne(otherOwner, "42", "cetirun", 17),
	} {
		if candidate == base {
			t.Fatalf("distinct GitHub App authority merged into %q", base)
		}
	}
	legacy, err := f.server.store.completeAccount(context.Background(), owner, connectionTransaction{Provider: "github"}, providerCredential{AccessToken: "legacy", NonExpiring: true}, "octocat", "42", time.Now().UTC())
	if err != nil || legacy == base {
		t.Fatalf("legacy GitHub OAuth merged with App installation: id=%q err=%v", legacy, err)
	}
}

func TestGitHubInstallationSelectionCannotChangeLockedInstallation(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	id := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "first")
	selectPendingGitHubInstallation(t, f, owner, id, "cetirun", 17)
	if _, err := f.server.store.selectGitHubInstallation(context.Background(), owner, id,
		agenttools.GitHubInstallation{InstallationID: 18, AppSlug: "cetirun", AccountLogin: "other-org"}, time.Now().UTC()); !errors.Is(err, errConflict) {
		t.Fatalf("selected connection accepted a different installation: %v", err)
	}
}

func TestGitHubInstallationSelectionSkipsInvalidPendingIndexTarget(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	invalidID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "expired")
	invalid, version, err := f.server.store.getConnection(context.Background(), invalidID)
	if err != nil {
		t.Fatal(err)
	}
	invalid.PendingInstallation = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}
	if err := f.server.store.putConnection(context.Background(), invalid, version); err != nil {
		t.Fatal(err)
	}
	if canonical, err := f.server.store.canonicalGitHubInstallation(context.Background(), installationIdentity(invalid)); err != nil || canonical != invalidID {
		t.Fatalf("initial pending source did not claim index: %q %v", canonical, err)
	}
	invalid, version, err = f.server.store.getConnection(context.Background(), invalidID)
	if err != nil {
		t.Fatal(err)
	}
	invalid.Connection.Status = "interaction_required"
	if err := f.server.store.putConnection(context.Background(), invalid, version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.store.selectGitHubInstallation(context.Background(), owner, invalidID, *invalid.PendingInstallation, time.Now().UTC()); !errors.Is(err, errConflict) {
		t.Fatalf("invalid pending source was promoted: %v", err)
	}

	freshID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "fresh")
	selected := selectPendingGitHubInstallation(t, f, owner, freshID, "cetirun", 17)
	if selected.Connection.ID != freshID || selected.Connection.Status != "connected" {
		t.Fatalf("fresh sign-in did not bypass invalid index target: %#v", selected)
	}
	stillInvalid, _, err := f.server.store.getConnection(context.Background(), invalidID)
	if err != nil || stillInvalid.Connection.Status != "interaction_required" || stillInvalid.Connection.GitHub.InstallationID != 0 {
		t.Fatalf("invalid pending source was revived: %#v %v", stillInvalid, err)
	}
}

func TestGitHubInstallationEndpointFinishesVerifiedPendingSelectionWithoutRefreshing(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	cfg := githubAppTestConfig(t)
	cfg.PortalNamespace, cfg.PortalServiceAccount = "portal", "portal"
	f.server.cfg = cfg
	owner := f.grant.Owner
	id := createPendingGitHubAppConnection(t, f, owner, "42", cfg.GitHubAppSlug, "expired")
	record, version, err := f.server.store.getConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	record.Credential.ExpiresAt = time.Now().Add(-time.Hour)
	record.PendingInstallation = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: cfg.GitHubAppSlug, AccountLogin: "example-org", UserLogin: "octocat"}
	if err := f.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	f.server.providers = &providerClient{cfg: cfg, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("endpoint refreshed or revalidated an already verified pending selection")
		return nil, nil
	})}}
	// Reopening the wizard must rediscover the locked choice before it can
	// retry POST. Neither listing nor completing it needs a token refresh.
	listRequest := httptest.NewRequest(http.MethodGet, "/v1/connections/"+id+"/github/installations", nil)
	setPortalRequestHeaders(listRequest, owner, "")
	listResponse := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(listResponse, listRequest)
	var choices struct {
		Installations []struct {
			ID           int64             `json:"id"`
			AccountLogin string            `json:"accountLogin"`
			Permissions  map[string]string `json:"permissions"`
		} `json:"installations"`
	}
	if listResponse.Code != http.StatusOK || json.Unmarshal(listResponse.Body.Bytes(), &choices) != nil || len(choices.Installations) != 1 || choices.Installations[0].ID != 17 || choices.Installations[0].AccountLogin != "example-org" || len(choices.Installations[0].Permissions) != 0 {
		t.Fatalf("pending installation was not available for retry: %d %s", listResponse.Code, listResponse.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/connections/"+id+"/github/installation", strings.NewReader(`{"installationId":17}`))
	setPortalRequestHeaders(request, owner, "")
	response := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("pending selection retry status = %d: %s", response.Code, response.Body.String())
	}
	selected, _, err := f.server.store.getConnection(context.Background(), id)
	if err != nil || selected.Connection.Status != "connected" || selected.Connection.GitHub.InstallationID != 17 {
		t.Fatalf("pending selection was not completed: %#v %v", selected, err)
	}
}

func TestGitHubInstallationSelectionDoesNotUseCredentialUnderRefreshLease(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	id := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "fresh")
	record, version, err := f.server.store.getConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	before := *record.Credential
	record.RefreshLease = &credentialRefreshLease{Token: "refresh-in-flight", ExpiresAt: time.Now().Add(time.Minute)}
	if err := f.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.store.selectGitHubInstallation(context.Background(), owner, id,
		agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org"}, time.Now().UTC()); !errors.Is(err, errConflict) {
		t.Fatalf("selection used a credential under refresh lease: %v", err)
	}
	after, _, err := f.server.store.getConnection(context.Background(), id)
	if err != nil || after.PendingInstallation != nil || !reflect.DeepEqual(*after.Credential, before) || after.RefreshLease == nil {
		t.Fatalf("selection changed source protected by refresh lease: %#v %v", after, err)
	}
}

func TestGitHubInstallationSelectionInvalidatesTransferredCredentialBeforeAndAfterConsumption(t *testing.T) {
	t.Run("before consumption", func(t *testing.T) {
		f := newAuthorizationFixture(t, "github", "github")
		owner := f.grant.Owner
		targetID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "old")
		selectPendingGitHubInstallation(t, f, owner, targetID, "cetirun", 17)
		sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "fresh")
		source, sourceVersion, err := f.server.store.getConnection(context.Background(), sourceID)
		if err != nil {
			t.Fatal(err)
		}
		source.PendingInstallation = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}
		source.Connection.Status = "interaction_required"
		if err := f.server.store.putConnection(context.Background(), source, sourceVersion); err != nil {
			t.Fatal(err)
		}
		target, targetVersion, err := f.server.store.getConnection(context.Background(), targetID)
		if err != nil {
			t.Fatal(err)
		}
		target.Credential = source.Credential
		if err := f.server.store.putConnection(context.Background(), target, targetVersion); err != nil {
			t.Fatal(err)
		}
		if err := f.server.store.consumeGitHubSelection(context.Background(), sourceID, target.Connection, time.Now().UTC()); !errors.Is(err, errConflict) {
			t.Fatalf("invalid source was consumed: %v", err)
		}
		currentTarget, _, err := f.server.store.getConnection(context.Background(), targetID)
		currentSource, _, sourceErr := f.server.store.getConnection(context.Background(), sourceID)
		if err != nil || sourceErr != nil || currentTarget.Connection.Status != "interaction_required" || currentSource.Deleted || currentSource.Credential == nil {
			t.Fatalf("invalidation before consumption was unsafe: target=%#v (%v), source=%#v (%v)", currentTarget, err, currentSource, sourceErr)
		}
	})
	t.Run("after consumption", func(t *testing.T) {
		f := newAuthorizationFixture(t, "github", "github")
		owner := f.grant.Owner
		targetID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "old")
		selectPendingGitHubInstallation(t, f, owner, targetID, "cetirun", 17)
		target, version, err := f.server.store.getConnection(context.Background(), targetID)
		if err != nil {
			t.Fatal(err)
		}
		target.Connection.Status = "interaction_required"
		if err := f.server.store.putConnection(context.Background(), target, version); err != nil {
			t.Fatal(err)
		}
		sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "fresh")
		sourceBefore, _, err := f.server.store.getConnection(context.Background(), sourceID)
		if err != nil {
			t.Fatal(err)
		}
		selected := selectPendingGitHubInstallation(t, f, owner, sourceID, "cetirun", 17)
		if selected.Connection.ID != targetID || selected.Credential.AccessToken != sourceBefore.Credential.AccessToken {
			t.Fatalf("selection did not transfer current source credential: %#v", selected)
		}
		f.server.markGitHubAppInteractionRequired(context.Background(), sourceBefore)
		currentTarget, _, err := f.server.store.getConnection(context.Background(), targetID)
		if err != nil || currentTarget.Connection.Status != "interaction_required" {
			t.Fatalf("late invalidation did not follow consumed source: %#v %v", currentTarget, err)
		}
	})
}

func TestGitHubInstallationSelectionRetryIsOwnerScopedAndDoesNotReviveTarget(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	targetID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "target")
	selectPendingGitHubInstallation(t, f, owner, targetID, "cetirun", 17)
	sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "source")
	selectPendingGitHubInstallation(t, f, owner, sourceID, "cetirun", 17)

	retry, err := f.server.store.completedGitHubSelection(context.Background(), owner, sourceID, 17)
	if err != nil || retry.Connection.ID != targetID {
		t.Fatalf("consumed source retry = %#v %v, want %q", retry, err, targetID)
	}
	other := owner
	other.ObjectID = "99999999-9999-9999-9999-999999999999"
	if _, err := f.server.store.completedGitHubSelection(context.Background(), other, sourceID, 17); !errors.Is(err, errNotFound) {
		t.Fatalf("other owner retried consumed connection: %v", err)
	}
	target, version, err := f.server.store.getConnection(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	target.Deleted = true
	if err := f.server.store.putConnection(context.Background(), target, version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.store.completedGitHubSelection(context.Background(), owner, sourceID, 17); !errors.Is(err, errNotFound) {
		t.Fatalf("retry revived or exposed deleted canonical connection: %v", err)
	}
}

func TestGitHubInstallationIndexRecoversClaimedPendingSelectionAndRepairsInteraction(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	targetID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "old")
	selectPendingGitHubInstallation(t, f, owner, targetID, "cetirun", 17)
	target, version, err := f.server.store.getConnection(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	target.Connection.Status = "interaction_required"
	if err := f.server.store.putConnection(context.Background(), target, version); err != nil {
		t.Fatal(err)
	}

	sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "fresh")
	source, version, err := f.server.store.getConnection(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	source.PendingInstallation = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}
	if err := f.server.store.putConnection(context.Background(), source, version); err != nil {
		t.Fatal(err)
	}
	// Simulate a process crash after durable index claim and before promotion.
	if id, err := f.server.store.canonicalGitHubInstallation(context.Background(), installationIdentity(source)); err != nil || id != targetID {
		t.Fatalf("claimed index did not select existing installation: %q %v", id, err)
	}
	repaired := selectPendingGitHubInstallation(t, f, owner, sourceID, "cetirun", 17)
	if repaired.Connection.ID != targetID || repaired.Connection.Status != "connected" || repaired.Credential == nil || repaired.Credential.AccessToken != "fresh" {
		t.Fatalf("pending recovery did not repair disconnected canonical connection: %#v", repaired)
	}
}

func TestGitHubInstallationAliasProjectionKeepsHistoricalBindings(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	first := &connectionRecord{Connection: Connection{ID: "historical-first", Owner: owner, Provider: "github", Mode: modeInstallation, AccountID: "github-app-user-42", Status: "connected", Revision: 1, CreatedAt: time.Now().Add(-time.Minute), GitHub: &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}}, Credential: &providerCredential{AccessToken: "first", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}}
	second := &connectionRecord{Connection: Connection{ID: "historical-second", Owner: owner, Provider: "github", Mode: modeInstallation, AccountID: "github-app-user-42", Status: "connected", Revision: 1, CreatedAt: time.Now(), GitHub: &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}}, Credential: &providerCredential{AccessToken: "second", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}}
	for _, record := range []*connectionRecord{first, second} {
		if err := f.server.store.putConnection(context.Background(), record, ""); err != nil {
			t.Fatal(err)
		}
		if err := f.server.store.putBinding(context.Background(), &bindingRecord{Binding: ConnectionBinding{ID: record.Connection.ID + "-binding", InstanceUID: f.agent.Metadata.UID, ConnectionID: record.Connection.ID, Owner: owner, Enabled: true, Revision: 1}}, ""); err != nil {
			t.Fatal(err)
		}
	}
	connections, err := f.server.store.listConnections(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.store.projectCanonicalConnections(context.Background(), connections); err != nil {
		t.Fatal(err)
	}
	aliases := map[string]string{}
	for _, connection := range connections {
		if connection.ID == first.Connection.ID || connection.ID == second.Connection.ID {
			aliases[connection.ID] = connection.CanonicalConnectionID
		}
	}
	if aliases[first.Connection.ID] == "" || aliases[first.Connection.ID] != aliases[second.Connection.ID] {
		t.Fatalf("historical aliases were not projected together: %#v", aliases)
	}
	for _, record := range []*connectionRecord{first, second} {
		binding, _, err := f.server.store.getBinding(context.Background(), record.Connection.ID+"-binding")
		if err != nil || binding.Binding.ConnectionID != record.Connection.ID {
			t.Fatalf("historical binding was changed: %#v %v", binding, err)
		}
	}
}

func TestGitHubInstallationPendingSelectionRecoversAfterConcurrentUnauthorizedResponse(t *testing.T) {
	for _, existingCanonical := range []bool{false, true} {
		name := "new connection"
		if existingCanonical {
			name = "existing canonical connection"
		}
		t.Run(name, func(t *testing.T) {
			f := newAuthorizationFixture(t, "github", "github")
			owner := f.grant.Owner
			canonicalID := ""
			if existingCanonical {
				canonicalID = createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "canonical")
				selectPendingGitHubInstallation(t, f, owner, canonicalID, "cetirun", 17)
				canonical, version, err := f.server.store.getConnection(context.Background(), canonicalID)
				if err != nil {
					t.Fatal(err)
				}
				canonical.Connection.Status = "interaction_required"
				if err := f.server.store.putConnection(context.Background(), canonical, version); err != nil {
					t.Fatal(err)
				}
			}
			sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "cetirun", "source")
			inFlight, _, err := f.server.store.getConnection(context.Background(), sourceID)
			if err != nil {
				t.Fatal(err)
			}
			source, version, err := f.server.store.getConnection(context.Background(), sourceID)
			if err != nil {
				t.Fatal(err)
			}
			source.PendingInstallation = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "cetirun", AccountLogin: "example-org", UserLogin: "octocat"}
			if err := f.server.store.putConnection(context.Background(), source, version); err != nil {
				t.Fatal(err)
			}
			if canonicalID == "" {
				canonicalID = sourceID
			}
			claimed, err := f.server.store.canonicalGitHubInstallation(context.Background(), installationIdentity(source))
			if err != nil || claimed != canonicalID {
				t.Fatalf("installation claim = %q, want %q: %v", claimed, canonicalID, err)
			}
			// A listing request obtained this token before selection started,
			// then returned unauthorized after the durable index claim.
			f.server.markGitHubAppInteractionRequired(context.Background(), inFlight)
			tx := connectionTransaction{Provider: "github", Mode: modeInstallation, AppSlug: "cetirun", TargetConnectionID: sourceID, ExpectedAccountID: "42"}
			fresh := providerCredential{AccessToken: "reauthorized", RefreshToken: "reauthorized-refresh", ExpiresAt: time.Now().Add(time.Hour)}
			if _, err := f.server.store.completeGitHubInstallationAccount(context.Background(), owner, tx, fresh, "octocat", "42", time.Now().UTC()); err != nil {
				t.Fatalf("pending selection could not reauthorize: %v", err)
			}
			// A repeated late response for the old credential must not undo the
			// completed sign-in or discard the installation-index identity.
			f.server.markGitHubAppInteractionRequired(context.Background(), inFlight)
			reconnected, _, err := f.server.store.getConnection(context.Background(), sourceID)
			if err != nil || reconnected.Connection.Status != "installation_required" || reconnected.PendingInstallation == nil || !reflect.DeepEqual(reconnected.PendingInstallation, source.PendingInstallation) || reconnected.Credential.AccessToken != fresh.AccessToken {
				t.Fatalf("reconnect did not retain the resumable selection: %#v %v", reconnected, err)
			}
			selected := selectPendingGitHubInstallation(t, f, owner, sourceID, "cetirun", 17)
			if selected.Connection.ID != canonicalID || selected.Connection.Status != "connected" || selected.Credential.AccessToken != fresh.AccessToken || selected.PendingInstallation != nil {
				t.Fatalf("reauthorized selection did not converge: %#v", selected)
			}
		})
	}
}

func TestGitHubInstallationSelectionReusesLegacyConnectionAfterAppRename(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	owner := f.grant.Owner
	id := createPendingGitHubAppConnection(t, f, owner, "42", "old-app", "existing")
	existing, version, err := f.server.store.getConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	existing.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: "old-app", AccountLogin: "example-org", UserLogin: "octocat"}
	existing.Connection.Status = "connected"
	if err := f.server.store.putConnection(context.Background(), existing, version); err != nil {
		t.Fatal(err)
	}
	// Older releases included the mutable App slug in their index key.
	legacyKey := accountIndexID(owner, "github-installation", existing.Connection.AccountID+"\x00old-app\x0017")
	legacyIndex := existing.Connection
	legacyIndex.ID, legacyIndex.CanonicalConnectionID = legacyKey, id
	if err := f.server.store.putOAuthRecord(context.Background(), "account-index", &connectionRecord{Connection: legacyIndex}, ""); err != nil {
		t.Fatal(err)
	}
	binding := &bindingRecord{Binding: ConnectionBinding{ID: "existing-binding", InstanceUID: f.agent.Metadata.UID, ConnectionID: id, Owner: owner, Enabled: true, Revision: 1}}
	if err := f.server.store.putBinding(context.Background(), binding, ""); err != nil {
		t.Fatal(err)
	}
	sourceID := createPendingGitHubAppConnection(t, f, owner, "42", "renamed-app", "new-sign-in")
	selected := selectPendingGitHubInstallation(t, f, owner, sourceID, "renamed-app", 17)
	if selected.Connection.ID != id || selected.Connection.GitHub.AppSlug != "renamed-app" || selected.Connection.AccountDisplay != "renamed-app[bot]" || !reflect.DeepEqual(selected.Credential, existing.Credential) {
		t.Fatalf("App rename did not preserve the existing connection and credentials: %#v", selected)
	}
	stored, _, err := f.server.store.getBinding(context.Background(), binding.Binding.ID)
	if err != nil || !reflect.DeepEqual(stored.Binding, binding.Binding) {
		t.Fatalf("App rename changed the existing Agent binding: %#v %v", stored, err)
	}
	retryID := createPendingGitHubAppConnection(t, f, owner, "42", "renamed-app", "another-sign-in")
	if retry := selectPendingGitHubInstallation(t, f, owner, retryID, "renamed-app", 17); retry.Connection.ID != id {
		t.Fatalf("App rename did not establish a stable installation index: %q", retry.Connection.ID)
	}
}
