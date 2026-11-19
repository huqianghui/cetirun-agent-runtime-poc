// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func stageOAuthTransaction(t *testing.T, f *authorizationFixture, tx *connectionTransaction) string {
	t.Helper()
	id := "oauth-" + tx.StateHash[:40]
	tx.Status = "pending"
	record := &connectionRecord{Connection: Connection{ID: id, Owner: f.grant.Owner, Provider: tx.Provider, Mode: modeDelegated, Revision: 1}, Transaction: tx}
	if err := f.server.store.putOAuthRecord(context.Background(), "authorization", record, ""); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConcurrentAccountLoginReusesOneConnectionAndPreservesHealthyCredential(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	// Simulate independently executing replicas against the same CAS API.
	transport := f.server.store.client.Transport
	var mu sync.Mutex
	f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		return transport.RoundTrip(r)
	})
	owner := f.grant.Owner
	const n = 8
	ids := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := f.server.store.completeAccount(context.Background(), owner, connectionTransaction{Provider: "github"}, providerCredential{AccessToken: "first", NonExpiring: true}, "octocat", "42", time.Now())
			ids <- id
			errs <- err
		}()
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
			t.Fatal("concurrent callbacks created duplicate accounts")
		}
		selected = id
	}
	before, _, err := f.server.store.getConnection(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.server.store.completeAccount(context.Background(), owner, connectionTransaction{Provider: "github"}, providerCredential{AccessToken: "replacement", NonExpiring: true}, "renamed", "42", time.Now())
	after, _, readErr := f.server.store.getConnection(context.Background(), selected)
	if err != nil || readErr != nil || id != selected || !reflect.DeepEqual(before, after) {
		t.Fatal("healthy repeat sign-in replaced credentials or account metadata")
	}
	different, err := f.server.store.completeAccount(context.Background(), owner, connectionTransaction{Provider: "github"}, providerCredential{AccessToken: "another", NonExpiring: true}, "octocat", "43", time.Now())
	if err != nil || different == selected {
		t.Fatal("display name used as account identity")
	}
}

func TestOAuthStartIsIndependentAndReconnectCancellationPreservesCredential(t *testing.T) {
	f := personalOAuthFixture(t, "github")
	f.server.cfg.PortalNamespace = "portal"
	f.server.cfg.PortalServiceAccount = "portal"
	f.server.cfg.GitHubOAuthClientID = "client"
	f.server.cfg.GitHubOAuthClientSecret = "secret"
	f.server.cfg.TransactionTTL = 10 * time.Minute
	before, _, _ := f.server.store.getConnection(context.Background(), f.connection.Connection.ID)
	request := httptest.NewRequest("POST", "/v1/connections/"+before.Connection.ID+"/reconnect", nil)
	setPortalRequestHeaders(request, f.grant.Owner, strings.Repeat("b", 43))
	response := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(response, request)
	var start struct {
		AuthorizationID  string `json:"authorizationId"`
		AuthorizationURL string `json:"authorizationUrl"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &start) != nil || start.AuthorizationID == "" {
		t.Fatal("reconnect did not start")
	}
	after, _, _ := f.server.store.getConnection(context.Background(), before.Connection.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("starting reconnect modified the live connection")
	}
	location, _ := url.Parse(start.AuthorizationURL)
	callback := httptest.NewRequest("GET", "/api/v1/connections/callback?state="+location.Query().Get("state")+"&error=access_denied", nil)
	setPortalCallbackHeaders(callback, strings.Repeat("b", 43))
	f.server.Handler().ServeHTTP(httptest.NewRecorder(), callback)
	after, _, _ = f.server.store.getConnection(context.Background(), before.Connection.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("cancelled reconnect modified credentials")
	}
	tx, _, err := f.server.store.getOAuthRecord(context.Background(), "authorization", start.AuthorizationID)
	if err != nil || tx.Transaction.Status != "failed" || !tx.Transaction.Consumed {
		t.Fatal("cancelled transaction not completed")
	}
	get := httptest.NewRequest("GET", "/v1/connection-authorizations/"+start.AuthorizationID, nil)
	other := f.grant.Owner
	other.ObjectID = "99999999-9999-9999-9999-999999999999"
	setPortalRequestHeaders(get, other, "")
	denied := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(denied, get)
	if denied.Code != 404 {
		t.Fatal("authorization result leaked to another owner")
	}
	list, err := f.server.store.listConnections(context.Background(), f.grant.Owner)
	if err != nil || len(list) != 1 {
		t.Fatal("OAuth transaction appeared as an account")
	}
}

func TestRemovalRequiresNoBindingOrInFlightRegistration(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	f.server.cfg.PortalNamespace = "portal"
	f.server.cfg.PortalServiceAccount = "portal"
	remove := func() int {
		r := httptest.NewRequest("DELETE", "/v1/connections/connection", nil)
		setPortalRequestHeaders(r, f.grant.Owner, "")
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if got := remove(); got != 409 {
		t.Fatalf("used account removal = %d", got)
	}
	// Begin another write before the old binding is disconnected.
	b := f.grant
	b.Revision++
	token, err := f.server.store.reserveBinding(context.Background(), f.connection.Connection, agentFromDocument(f.agent), b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.mutateBinding(t, func(b *ConnectionBinding) { b.Revoked = true; b.Enabled = false })
	if got := remove(); got != 409 {
		t.Fatalf("pending registration did not block deletion: %d", got)
	}
	if err := f.server.store.releaseBindingReservation(context.Background(), f.grant.Owner, "connection", token); err != nil {
		t.Fatal(err)
	}
	if got := remove(); got != 204 {
		t.Fatalf("unused account removal = %d", got)
	}
	if _, err := f.server.store.reserveBinding(context.Background(), f.connection.Connection, agentFromDocument(f.agent), b, time.Now()); !errors.Is(err, errConflict) {
		t.Fatal("deleted account accepted a new registration")
	}
}

func TestInterruptedReservationIsFencedBeforeRemoval(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	delete(f.secrets, toolRecordName("binding", f.grant.ID))
	token, err := f.server.store.reserveBinding(context.Background(), f.connection.Connection, agentFromDocument(f.agent), f.grant, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	record, _, _ := f.server.store.getConnection(context.Background(), "connection")
	if err := f.server.store.reconcileReservations(context.Background(), record, time.Now()); err != nil {
		t.Fatal(err)
	}
	late := &bindingRecord{Binding: f.grant, ReservationToken: token}
	if err := f.server.store.putBinding(context.Background(), late, ""); !errors.Is(err, errConflict) {
		t.Fatal("delayed binding write bypassed reservation fence")
	}
	record, _, _ = f.server.store.getConnection(context.Background(), "connection")
	if len(record.References) != 0 {
		t.Fatal("fenced reservation was not released")
	}
}

func TestUsageIncludesBindingsOfDeletingAndUnsupportedAgents(t *testing.T) {
	for _, state := range []string{"deleting", "suspended", "sandbox", "unsupported"} {
		t.Run(state, func(t *testing.T) {
			f := newNamespaceFixture(t)
			ctx := context.Background()
			owner := Owner{namespaceTenant, namespaceOwner}
			connection := &connectionRecord{Connection: Connection{ID: "personal", Provider: "github", Mode: modeDelegated, Owner: owner, Revision: 1, Status: "connected", AccountID: "42"}}
			if err := f.store.putConnection(ctx, connection, ""); err != nil {
				t.Fatal(err)
			}
			b := &bindingRecord{Binding: ConnectionBinding{ID: "binding", InstanceUID: namespaceAgentUID, ConnectionID: "personal", Owner: owner, Enabled: true, Revision: 1}}
			token, err := f.store.reserveBinding(ctx, connection.Connection, agentFromDocument(f.agent), b.Binding, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			b.ReservationToken = token
			if err := f.store.putBinding(ctx, b, ""); err != nil {
				t.Fatal(err)
			}
			connection, _, err = f.store.getConnection(ctx, connection.Connection.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "deleting":
				now := time.Now()
				f.agent.Metadata.DeletionTimestamp = &now
			case "suspended":
				f.agent.Spec.Suspended = true
			case "sandbox":
				f.agent.Spec.SandboxRef = "shared"
			case "unsupported":
				f.agent.Spec.Agent.Type = "custom"
				agent := agentFromDocument(f.agent)
				immutable := true
				f.customReceipt = &configMapDocument{Immutable: &immutable, Data: map[string]string{"ownership": `{"agentName":"agent","agentUID":"` + agent.UID + `","namespace":"agents","namespaceUID":"namespace-agent"}`}}
				f.customReceipt.Metadata.Name = agenttools.CustomOwnershipRecordName(agent.UID)
				f.customReceipt.Metadata.Namespace = "cetirun-system"
				f.customReceipt.Metadata.OwnerReferences = []ownerReference{agentRecordOwner(agent)}
				f.store.nativeOperatorNamespace = "cetirun-system"
			}
			usage, err := f.store.usage(ctx, connection)
			if err != nil || usage.CanRemove || len(usage.Agents) != 1 || usage.Agents[0].InstanceUID != namespaceAgentUID {
				t.Fatalf("usage omitted %s Agent: %#v %v", state, usage, err)
			}
		})
	}
}

func TestRefreshOnlyMarksExplicitInvalidGrantAsInteractionRequired(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "network failure", true: "invalid grant"}[invalid], func(t *testing.T) {
			f := personalOAuthFixture(t, "google")
			ctx := context.Background()
			c, version, err := f.server.store.getConnection(ctx, "connection")
			if err != nil {
				t.Fatal(err)
			}
			c.Credential.ExpiresAt = time.Now().Add(-time.Minute)
			if err := f.server.store.putConnection(ctx, c, version); err != nil {
				t.Fatal(err)
			}
			c, _, err = f.server.store.getConnection(ctx, "connection")
			if err != nil {
				t.Fatal(err)
			}
			f.server.providers = &providerClient{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if invalid {
					return securityJSONResponse(t, 400, map[string]string{"error": "invalid_grant"}), nil
				}
				return nil, errors.New("synthetic transport failure")
			})}}
			if _, _, err := f.server.issueServiceCredential(ctx, c, "google_gmail", agentFromDocument(f.agent)); err == nil {
				t.Fatal("failed refresh succeeded")
			}
			current, _, err := f.server.store.getConnection(ctx, "connection")
			if err != nil {
				t.Fatal(err)
			}
			expected := "connected"
			if invalid {
				expected = "interaction_required"
			}
			if current.Connection.Status != expected {
				t.Fatalf("status %s want %s", current.Connection.Status, expected)
			}
			if !reflect.DeepEqual(current.Credential, c.Credential) {
				t.Fatal("refresh failure cleared credentials")
			}
		})
	}
}

func TestExpiredOAuthTransactionCleanupDoesNotRemoveAccounts(t *testing.T) {
	f := newNamespaceFixture(t)
	ctx := context.Background()
	owner := Owner{namespaceTenant, namespaceOwner}
	account := &connectionRecord{Connection: Connection{ID: "personal", Provider: "github", Owner: owner, Revision: 1, Mode: modeDelegated}}
	if err := f.store.putConnection(ctx, account, ""); err != nil {
		t.Fatal(err)
	}
	tx := &connectionRecord{Connection: Connection{ID: "oauth-old", Provider: "github", Owner: owner, Revision: 1}, Transaction: &connectionTransaction{ExpiresAt: time.Now().Add(-2 * time.Hour)}}
	if err := f.store.putOAuthRecord(ctx, "authorization", tx, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pruneAuthorizations(ctx, owner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.getOAuthRecord(ctx, "authorization", "oauth-old"); !errors.Is(err, errNotFound) {
		t.Fatal("expired authorization retained")
	}
	if _, _, err := f.store.getConnection(ctx, "personal"); err != nil {
		t.Fatal("account was removed during OAuth cleanup")
	}
}
