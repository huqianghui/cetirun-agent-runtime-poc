// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"github.com/Azure/CetiRun/app/internal/agenttools"

	"context"

	"strings"
	"testing"
	"time"
)

const (
	testAgentPrincipalID = "55555555-5555-5555-5555-555555555555"
	testSubscriptionID   = "66666666-6666-6666-6666-666666666666"
	testRoleDefinitionID = "/subscriptions/" + testSubscriptionID + "/providers/Microsoft.Authorization/roleDefinitions/acdd72a7-3385-48ef-bd42-f606fba81ae7"
)

// prepareAgentAuthorizationFixture converts the established encrypted-store
// fixture to the independent Azure identity shape without bypassing the Broker
// storage layer. The returned binding is immediately usable; Azure evaluates RBAC.
func prepareAgentAuthorizationFixture(t *testing.T) *authorizationFixture {
	t.Helper()
	fixture := newAuthorizationFixture(t, "azure", "azure_arm")
	fixture.agent.Spec.AgentIdentity = &struct {
		Enabled bool `json:"enabled"`
	}{Enabled: true}
	fixture.agent.Status.AgentIdentity = &struct {
		Phase    string `json:"phase"`
		ClientID string `json:"clientId"`
		ObjectID string `json:"objectId"`
	}{Phase: "Ready", ClientID: testFirstAgentID, ObjectID: testAgentPrincipalID}

	connection, connectionVersion, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection.Credential = nil
	connection.Connection.Mode = modeAgent
	connection.Connection.Identity = &IdentityBinding{
		InstanceUID: fixture.agent.Metadata.UID,
		TenantID:    fixture.server.cfg.TenantID,
		ClientID:    testFirstAgentID,
		PrincipalID: testAgentPrincipalID,
	}
	connection.Connection.AccountID = testAgentPrincipalID
	connection.Connection.AccountDisplay = "Agent " + fixture.agent.Metadata.Name
	connection.Connection.Services = agenttools.ProviderServices("azure")
	connection.Connection.Revision = 7
	if err := fixture.server.store.putConnection(context.Background(), connection, connectionVersion); err != nil {
		t.Fatal(err)
	}
	fixture.connection = *connection

	grant, err := buildBinding(bindingRequest{
		ConnectionID: fixture.connection.Connection.ID,
		Services:     []string{"azure_arm"},
		Enabled:      true,
	}, fixture.connection.Connection.Owner, fixture.agent.Metadata.UID, fixture.connection.Connection, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	grant.ID = fixture.grant.ID
	stored, grantVersion, err := fixture.server.store.getBinding(context.Background(), grant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Binding.ID != grant.ID {
		t.Fatalf("stored grant ID = %q", stored.Binding.ID)
	}
	if err := fixture.server.store.putBinding(context.Background(), &bindingRecord{Binding: grant}, grantVersion); err != nil {
		t.Fatal(err)
	}
	fixture.grant = grant
	return fixture
}

func TestAzureBindingUsesProviderAuthorizationWithoutRBACPreverification(t *testing.T) {
	fixture := prepareAgentAuthorizationFixture(t)
	response := fixture.request(t, "/internal/v1/service-authorizations/check")
	if response.Code != 200 {
		t.Fatalf("Azure binding required an extra resource grant: %d %s", response.Code, response.Body)
	}
}
func TestBindingRejectsDifferentAgentIdentityAndServices(t *testing.T) {
	for _, change := range []func(*authorizationFixture){
		func(f *authorizationFixture) {
			f.agent.Status.AgentIdentity.ObjectID = "77777777-7777-7777-7777-777777777777"
		},
		func(f *authorizationFixture) {
			f.agent.Status.AgentIdentity.ClientID = "77777777-7777-7777-7777-777777777777"
		},
		func(f *authorizationFixture) { f.agent.Spec.Agent.Type = "custom" },
	} {
		f := prepareAgentAuthorizationFixture(t)
		change(f)
		if r := f.request(t, "/internal/v1/service-authorizations/check"); r.Code == 200 {
			t.Fatal("changed identity retained binding access")
		}
	}
	f := prepareAgentAuthorizationFixture(t)
	if _, err := buildBinding(bindingRequest{ConnectionID: f.connection.Connection.ID, Services: []string{"github"}, Enabled: true}, f.grant.Owner, f.grant.InstanceUID, f.connection.Connection, time.Now()); err == nil {
		t.Fatal("cross-provider service binding accepted")
	}
}
func TestPersonalConnectionIsNotAutomaticallySharedWithAgents(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	delete(f.secrets, toolRecordName("binding", f.grant.ID))
	if r := f.request(t, "/internal/v1/service-authorizations/check"); r.Code != 403 {
		t.Fatalf("unbound personal connection usable: %d", r.Code)
	}
	if r := f.request(t, "/internal/v1/service-authorizations/list"); r.Code != 200 || strings.Contains(r.Body.String(), "original@example.com") {
		t.Fatal("unbound personal account exposed")
	}
}
func TestDisabledAndStaleBindingCannotIssueCredential(t *testing.T) {
	for _, change := range []func(*ConnectionBinding){func(b *ConnectionBinding) { b.Enabled = false }, func(b *ConnectionBinding) { b.Revision++ }, func(b *ConnectionBinding) { b.Revoked = true }} {
		f := newAuthorizationFixture(t, "github", "github")
		f.mutateBinding(t, change)
		r := f.request(t, "/internal/v1/service-credentials/issue")
		if r.Code == 200 || strings.Contains(r.Body.String(), "accessToken") {
			t.Fatal("stale binding obtained credential")
		}
	}
}
