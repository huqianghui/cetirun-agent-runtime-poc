// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

const (
	instanceNameMetadata      = "agenttools.cetirun.microsoft.com/instance-name"
	instanceNamespaceMetadata = "agenttools.cetirun.microsoft.com/instance-namespace"
	instanceUIDMetadata       = "agenttools.cetirun.microsoft.com/instance-uid"
)

func inventorySecret(t *testing.T, fixture *authorizationFixture, kind, id string) secretDocument {
	t.Helper()
	secret, ok := fixture.secrets[toolRecordName(kind, id)]
	if !ok {
		t.Fatalf("%s Secret %q was not stored", kind, id)
	}
	return secret
}

func TestBindingCreateStoresTrustedInstanceDisplayMetadata(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	fixture.mutateBinding(t, func(grant *ConnectionBinding) {
		grant.Revoked = true
		grant.Revision++
	})

	response := bindingCreateRequest(t, fixture)
	if response.Code != http.StatusOK {
		t.Fatalf("grant create = %d: %s", response.Code, response.Body.String())
	}
	var created ConnectionBinding
	result := response.Result()
	defer result.Body.Close()
	if err := json.NewDecoder(result.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	stored, _, err := fixture.server.store.getBinding(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.InstanceName != fixture.agent.Metadata.Name || stored.InstanceNamespace != fixture.agent.Status.Namespace {
		t.Fatalf("encrypted grant display snapshot = (%q, %q), want (%q, %q)", stored.InstanceName, stored.InstanceNamespace, fixture.agent.Metadata.Name, fixture.agent.Status.Namespace)
	}
	secret := inventorySecret(t, fixture, "binding", created.ID)
	if got := secret.Metadata.Annotations[instanceNameMetadata]; got != fixture.agent.Metadata.Name {
		t.Fatalf("grant instance-name annotation = %q, want %q", got, fixture.agent.Metadata.Name)
	}
	if got := secret.Metadata.Annotations[instanceNamespaceMetadata]; got != fixture.agent.Status.Namespace {
		t.Fatalf("grant instance-namespace annotation = %q, want %q", got, fixture.agent.Status.Namespace)
	}
	if got := secret.Metadata.Labels[instanceNameMetadata]; got != fixture.agent.Metadata.Name {
		t.Fatalf("grant instance-name label = %q, want %q", got, fixture.agent.Metadata.Name)
	}
	if got := secret.Metadata.Labels[instanceNamespaceMetadata]; got != fixture.agent.Status.Namespace {
		t.Fatalf("grant instance-namespace label = %q, want %q", got, fixture.agent.Status.Namespace)
	}
}

func TestConnectionDisplayMetadataDoesNotChangeEncryptedIdentityAuthority(t *testing.T) {
	fixture := prepareAgentAuthorizationFixture(t)
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.InstanceName, record.InstanceNamespace = fixture.agent.Metadata.Name, fixture.agent.Status.Namespace
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}

	secret := inventorySecret(t, fixture, "connection", record.Connection.ID)
	secret.Metadata.Annotations[instanceNameMetadata] = "untrusted-display-name"
	secret.Metadata.Annotations[instanceNamespaceMetadata] = "untrusted-display-namespace"
	secret.Metadata.Labels[instanceNameMetadata] = "untrusted-display-name"
	secret.Metadata.Labels[instanceNamespaceMetadata] = "untrusted-display-namespace"
	fixture.secrets[secret.Metadata.Name] = secret

	stored, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.InstanceName != fixture.agent.Metadata.Name || stored.InstanceNamespace != fixture.agent.Status.Namespace {
		t.Fatalf("tampered display metadata changed encrypted snapshot to (%q, %q)", stored.InstanceName, stored.InstanceNamespace)
	}
	if stored.Connection.Identity == nil || stored.Connection.Identity.InstanceUID != fixture.agent.Metadata.UID {
		t.Fatalf("encrypted identity binding = %#v, want UID %q", stored.Connection.Identity, fixture.agent.Metadata.UID)
	}
	agent := agentFromDocument(fixture.agent)
	if !fixture.server.connectionMatchesAgent(stored.Connection, agent) {
		t.Fatal("trusted Agent was rejected after display metadata was tampered")
	}
	agent.UID = "77777777-7777-7777-7777-777777777777"
	if fixture.server.connectionMatchesAgent(stored.Connection, agent) {
		t.Fatal("display metadata changed the Agent UID authority")
	}
}

func TestDelegatedConnectionNeverGetsExclusiveAgentMetadata(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.InstanceName, record.InstanceNamespace = fixture.agent.Metadata.Name, fixture.agent.Status.Namespace
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}

	secret := inventorySecret(t, fixture, "connection", record.Connection.ID)
	if got := secret.Metadata.Labels["agenttools.cetirun.microsoft.com/mode"]; got != modeDelegated {
		t.Fatalf("delegated connection mode label = %q, want %q", got, modeDelegated)
	}
	for _, key := range []string{instanceUIDMetadata, instanceNameMetadata, instanceNamespaceMetadata} {
		if got := secret.Metadata.Labels[key]; got != "" {
			t.Fatalf("delegated connection has exclusive label %q=%q", key, got)
		}
		if got := secret.Metadata.Annotations[key]; got != "" {
			t.Fatalf("delegated connection has exclusive annotation %q=%q", key, got)
		}
	}
}

func TestAgentConnectionProjectsValidDisplayLabelsAndInvalidValuesAsAnnotations(t *testing.T) {
	fixture := prepareAgentAuthorizationFixture(t)
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.InstanceName, record.InstanceNamespace = fixture.agent.Metadata.Name, fixture.agent.Status.Namespace
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}

	secret := inventorySecret(t, fixture, "connection", record.Connection.ID)
	if got := secret.Metadata.Labels[instanceUIDMetadata]; got != fixture.agent.Metadata.UID {
		t.Fatalf("Agent connection instance UID label = %q, want %q", got, fixture.agent.Metadata.UID)
	}
	for key, want := range map[string]string{
		instanceNameMetadata:      fixture.agent.Metadata.Name,
		instanceNamespaceMetadata: fixture.agent.Status.Namespace,
	} {
		if got := secret.Metadata.Labels[key]; got != want {
			t.Fatalf("Agent connection label %q = %q, want %q", key, got, want)
		}
		if got := secret.Metadata.Annotations[key]; got != want {
			t.Fatalf("Agent connection annotation %q = %q, want %q", key, got, want)
		}
	}

	record, version, err = fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	const longDottedName = "this-agent-display-name-is-longer-than-a-kubernetes-label-and.example"
	const dottedNamespace = "agents.example"
	if dnsLabel(longDottedName) || dnsLabel(dottedNamespace) {
		t.Fatal("test values must be outside the short DNS-label projection")
	}
	record.InstanceName, record.InstanceNamespace = longDottedName, dottedNamespace
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}

	secret = inventorySecret(t, fixture, "connection", record.Connection.ID)
	if got := secret.Metadata.Labels[instanceUIDMetadata]; got != fixture.agent.Metadata.UID {
		t.Fatalf("Agent connection lost UID label after display change: %q", got)
	}
	for key, want := range map[string]string{
		instanceNameMetadata:      longDottedName,
		instanceNamespaceMetadata: dottedNamespace,
	} {
		if got := secret.Metadata.Annotations[key]; got != want {
			t.Fatalf("invalid display annotation %q = %q, want %q", key, got, want)
		}
		if got := secret.Metadata.Labels[key]; got != "" {
			t.Fatalf("invalid display value was stored as label %q=%q", key, got)
		}
	}
}
