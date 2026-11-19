// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const (
	namespaceAgentUID = "11111111-1111-1111-1111-111111111111"
	namespaceTenant   = "22222222-2222-2222-2222-222222222222"
	namespaceOwner    = "33333333-3333-3333-3333-333333333333"
)

type namespaceFixture struct {
	t                  *testing.T
	store              *kubeClient
	agent              agentDocument
	secrets            map[string]map[string]secretDocument
	version            uint64
	conflictNextDelete bool
	server             *httptest.Server
	customReceipt      *configMapDocument
}

func newNamespaceFixture(t *testing.T) *namespaceFixture {
	t.Helper()
	fixture := &namespaceFixture{t: t, secrets: map[string]map[string]secretDocument{"credentials": {}, "agents": {}}}
	fixture.agent.Metadata.Name, fixture.agent.Metadata.UID = "agent", namespaceAgentUID
	fixture.agent.Metadata.Annotations = map[string]string{
		"agentidentity.cetirun.microsoft.com/owner-tenant-id": namespaceTenant,
		"agentidentity.cetirun.microsoft.com/owner-object-id": namespaceOwner,
	}
	fixture.agent.Status.Namespace, fixture.agent.Status.Phase = "agents", "Ready"
	fixture.agent.Spec.Agent.Type = "hermes"
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(fixture.server.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.store = &kubeClient{
		baseURL: fixture.server.URL, namespace: "credentials", tokenFile: tokenFile, agentNamespaces: true,
		client: fixture.server.Client(), wrapper: memoryWrapper{key: []byte("namespace-encryption-key")},
	}
	return fixture
}

func (fixture *namespaceFixture) serveHTTP(response http.ResponseWriter, request *http.Request) {
	fixture.t.Helper()
	if request.Method == http.MethodGet && request.URL.Path == "/apis/cetirun.microsoft.com/v1alpha1/agents" {
		fixture.writeJSON(response, http.StatusOK, agentList{Items: []agentDocument{fixture.agent}})
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/apis/cetirun.microsoft.com/v1alpha1/agents/agent" {
		fixture.writeJSON(response, http.StatusOK, fixture.agent)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/agents" {
		metadata := secretMetadata{Name: "agents", UID: "namespace-agent"}
		if fixture.agent.Spec.Agent.Type == "custom" {
			metadata.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": fixture.agent.Metadata.UID}
		} else {
			metadata.OwnerReferences = []ownerReference{agentRecordOwner(agentFromDocument(fixture.agent))}
		}
		fixture.writeJSON(response, http.StatusOK, map[string]any{"metadata": metadata})
		return
	}
	if request.Method == http.MethodGet && fixture.customReceipt != nil && request.URL.Path == "/api/v1/namespaces/cetirun-system/configmaps/"+fixture.customReceipt.Metadata.Name {
		fixture.writeJSON(response, http.StatusOK, fixture.customReceipt)
		return
	}
	const prefix = "/api/v1/namespaces/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		fixture.t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
	}
	rest := strings.TrimPrefix(request.URL.Path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "secrets" || (len(parts) != 2 && len(parts) != 3) {
		fixture.t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
	}
	namespace := parts[0]
	if _, ok := fixture.secrets[namespace]; !ok {
		fixture.t.Fatalf("unexpected namespace %q", namespace)
	}
	if len(parts) == 2 {
		fixture.handleSecretCollection(response, request, namespace)
		return
	}
	fixture.handleSecret(response, request, namespace, parts[2])
}

func TestRecordStoreForOrdinaryCustomUsesOwnershipReceipt(t *testing.T) {
	fixture := newNamespaceFixture(t)
	fixture.agent.Spec.Agent.Type = "custom"
	agent := agentFromDocument(fixture.agent)
	receipt := `{"agentName":"agent","agentUID":"` + agent.UID + `","namespace":"agents","namespaceUID":"namespace-agent"}`
	immutable := true
	fixture.customReceipt = &configMapDocument{Immutable: &immutable, Data: map[string]string{"ownership": receipt}}
	fixture.customReceipt.Metadata.Name = agenttools.CustomOwnershipRecordName(agent.UID)
	fixture.customReceipt.Metadata.Namespace = "cetirun-system"
	fixture.customReceipt.Metadata.OwnerReferences = []ownerReference{agentRecordOwner(agent)}
	fixture.store.nativeOperatorNamespace = "cetirun-system"
	store, err := fixture.store.recordStoreForAgent(context.Background(), agent)
	if err != nil || store.namespace != "agents" {
		t.Fatalf("recordStoreForAgent() = %#v, %v", store, err)
	}
	metadata := secretMetadata{Name: "agents", UID: "namespace-agent", Annotations: map[string]string{"cetirun.microsoft.com/agent-uid": agent.UID}}
	metadata.Annotations["cetirun.microsoft.com/agent-uid"] = "forged-agent"
	if err := fixture.store.customRecordNamespaceOwned(context.Background(), agent, metadata); err == nil {
		t.Fatal("forged namespace annotation was accepted")
	}
	metadata.Annotations["cetirun.microsoft.com/agent-uid"] = agent.UID
	metadata.UID = "replacement-namespace"
	if err := fixture.store.customRecordNamespaceOwned(context.Background(), agent, metadata); err == nil {
		t.Fatal("replacement namespace UID was accepted")
	}
	metadata.UID = "namespace-agent"
	fixture.customReceipt.Metadata.OwnerReferences = []ownerReference{{APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: agent.Name, UID: "replaced-agent"}}
	if err := fixture.store.customRecordNamespaceOwned(context.Background(), agent, metadata); err == nil {
		t.Fatal("foreign receipt owner was accepted")
	}
	fixture.customReceipt.Metadata.OwnerReferences = nil
	if err := fixture.store.customRecordNamespaceOwned(context.Background(), agent, metadata); err == nil {
		t.Fatal("ownerless receipt was accepted before deletion")
	}
	fixture.customReceipt.Metadata.OwnerReferences = []ownerReference{agentRecordOwner(agent)}
	if connectionMatchesAgent(Connection{Mode: modeDelegated}, agent, namespaceTenant) {
		t.Fatal("ordinary Custom receipt enabled credential issuance")
	}
	mutable := false
	fixture.customReceipt.Immutable = &mutable
	if _, err := fixture.store.recordStoreForAgent(context.Background(), agent); err == nil {
		t.Fatal("mutable Custom ownership receipt was accepted")
	}
}

func (fixture *namespaceFixture) handleSecretCollection(response http.ResponseWriter, request *http.Request, namespace string) {
	switch request.Method {
	case http.MethodGet:
		items := []secretDocument{}
		for _, secret := range fixture.secrets[namespace] {
			if namespaceSecretMatches(secret, request.URL.Query().Get("labelSelector")) {
				items = append(items, secret)
			}
		}
		fixture.writeJSON(response, http.StatusOK, secretList{Items: items})
	case http.MethodPost:
		var secret secretDocument
		if err := json.NewDecoder(request.Body).Decode(&secret); err != nil {
			fixture.t.Fatal(err)
		}
		if _, exists := fixture.secrets[namespace][secret.Metadata.Name]; exists {
			fixture.writeJSON(response, http.StatusConflict, map[string]any{})
			return
		}
		fixture.version++
		secret.Metadata.Namespace = namespace
		secret.Metadata.UID = "secret-" + strconv.FormatUint(fixture.version, 10)
		secret.Metadata.ResourceVersion = strconv.FormatUint(fixture.version, 10)
		fixture.secrets[namespace][secret.Metadata.Name] = secret
		fixture.writeJSON(response, http.StatusCreated, secret)
	default:
		fixture.t.Fatalf("unexpected collection request: %s", request.Method)
	}
}

func (fixture *namespaceFixture) handleSecret(response http.ResponseWriter, request *http.Request, namespace, name string) {
	secret, exists := fixture.secrets[namespace][name]
	switch request.Method {
	case http.MethodGet:
		if !exists {
			fixture.writeJSON(response, http.StatusNotFound, map[string]any{})
			return
		}
		fixture.writeJSON(response, http.StatusOK, secret)
	case http.MethodPut:
		if !exists {
			fixture.writeJSON(response, http.StatusNotFound, map[string]any{})
			return
		}
		var replacement secretDocument
		if err := json.NewDecoder(request.Body).Decode(&replacement); err != nil {
			fixture.t.Fatal(err)
		}
		if replacement.Metadata.ResourceVersion != secret.Metadata.ResourceVersion {
			fixture.writeJSON(response, http.StatusConflict, map[string]any{})
			return
		}
		fixture.version++
		replacement.Metadata.Namespace, replacement.Metadata.UID = namespace, secret.Metadata.UID
		replacement.Metadata.ResourceVersion = strconv.FormatUint(fixture.version, 10)
		fixture.secrets[namespace][name] = replacement
		fixture.writeJSON(response, http.StatusOK, replacement)
	case http.MethodDelete:
		if !exists {
			fixture.writeJSON(response, http.StatusNotFound, map[string]any{})
			return
		}
		if fixture.conflictNextDelete {
			fixture.conflictNextDelete = false
			fixture.version++
			secret.Metadata.ResourceVersion = strconv.FormatUint(fixture.version, 10)
			fixture.secrets[namespace][name] = secret
			fixture.writeJSON(response, http.StatusConflict, map[string]any{})
			return
		}
		var options struct {
			Preconditions map[string]string `json:"preconditions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
			fixture.t.Fatal(err)
		}
		if options.Preconditions["uid"] != secret.Metadata.UID || options.Preconditions["resourceVersion"] != secret.Metadata.ResourceVersion {
			fixture.writeJSON(response, http.StatusConflict, map[string]any{})
			return
		}
		delete(fixture.secrets[namespace], name)
		fixture.writeJSON(response, http.StatusOK, map[string]any{})
	default:
		fixture.t.Fatalf("unexpected item request: %s", request.Method)
	}
}

func (fixture *namespaceFixture) writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		fixture.t.Fatal(err)
	}
}

func namespaceSecretMatches(secret secretDocument, selector string) bool {
	for _, requirement := range strings.Split(selector, ",") {
		if requirement == "" {
			continue
		}
		key, value, found := strings.Cut(requirement, "=")
		if !found || secret.Metadata.Labels[key] != value {
			return false
		}
	}
	return true
}

func (fixture *namespaceFixture) centralStore() *kubeClient {
	central := *fixture.store
	central.agentNamespaces, central.recordAgent = false, nil
	return &central
}

func (fixture *namespaceFixture) putCentralConnection(t *testing.T, record *connectionRecord) {
	t.Helper()
	if err := fixture.centralStore().putConnection(context.Background(), record, ""); err != nil {
		t.Fatal(err)
	}
}

func (fixture *namespaceFixture) putCentralBinding(t *testing.T, record *bindingRecord) {
	t.Helper()
	if err := fixture.centralStore().putBinding(context.Background(), record, ""); err != nil {
		t.Fatal(err)
	}
}

func namespaceAgentConnection(now time.Time) *connectionRecord {
	return &connectionRecord{Connection: Connection{
		ID: "agent-connection", Owner: Owner{TenantID: namespaceTenant, ObjectID: namespaceOwner}, Provider: "azure", Mode: modeAgent,
		Identity:  &IdentityBinding{InstanceUID: namespaceAgentUID, TenantID: namespaceTenant, ClientID: "44444444-4444-4444-4444-444444444444", PrincipalID: "55555555-5555-5555-5555-555555555555"},
		AccountID: "55555555-5555-5555-5555-555555555555", Status: "connected", Revision: 1, CreatedAt: now, UpdatedAt: now,
	}}
}

func namespaceDelegatedConnection(now time.Time) *connectionRecord {
	return &connectionRecord{Connection: Connection{
		ID: "shared-connection", Owner: Owner{TenantID: namespaceTenant, ObjectID: namespaceOwner}, Provider: "github", AccountID: "42", Status: "connected", Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, Credential: &providerCredential{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: now.Add(time.Hour)}}
}

func namespaceBinding(id, connectionID string, now time.Time) *bindingRecord {
	return &bindingRecord{Binding: ConnectionBinding{
		ID: id, Owner: Owner{TenantID: namespaceTenant, ObjectID: namespaceOwner}, InstanceUID: namespaceAgentUID, ConnectionID: connectionID,
		Services: []string{"azure_arm"}, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}}
}
