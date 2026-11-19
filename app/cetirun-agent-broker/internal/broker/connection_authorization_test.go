// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type connectionFixture struct {
	server     *Server
	agent      agentDocument
	connection connectionRecord
	binding    ConnectionBinding
	secrets    map[string]secretDocument
	version    uint64
}

func newConnectionFixture(t *testing.T) *connectionFixture {
	t.Helper()
	owner := Owner{TenantID: testTenantID, ObjectID: "33333333-3333-3333-3333-333333333333"}
	now := time.Now().UTC().Truncate(time.Second)
	fixture := &connectionFixture{secrets: map[string]secretDocument{}}
	fixture.agent.Metadata.Name = "agent"
	fixture.agent.Spec.Agent.Type = "openclaw"
	fixture.agent.Metadata.UID = "11111111-1111-1111-1111-111111111111"
	fixture.agent.Metadata.Annotations = map[string]string{
		"agentidentity.cetirun.microsoft.com/owner-tenant-id": owner.TenantID,
		"agentidentity.cetirun.microsoft.com/owner-object-id": owner.ObjectID,
	}
	fixture.agent.Status.Namespace, fixture.agent.Status.Phase = "agents", "Ready"
	fixture.agent.Spec.AgentIdentity = &struct {
		Enabled bool `json:"enabled"`
	}{true}
	fixture.agent.Status.AgentIdentity = &struct {
		Phase    string `json:"phase"`
		ClientID string `json:"clientId"`
		ObjectID string `json:"objectId"`
	}{"Ready", testFirstAgentID, "55555555-5555-5555-5555-555555555555"}
	cfg := Config{PortalNamespace: "portal", PortalServiceAccount: "portal", OperatorNamespace: "operator", OperatorServiceAccount: "operator",
		ToolServiceNamespace: "cetirun-system", ToolServiceAccount: "cetirun-connections-mcp", WorkloadTokenAudience: "cetirun-connections-mcp",
		TenantID: owner.TenantID, BlueprintClientID: "44444444-4444-4444-4444-444444444444", AuthorizationClientID: "55555555-5555-5555-5555-555555555555",
		PortalOrigin: "https://portal.example.com", RedirectURI: "https://portal.example.com/api/v1/connections/callback",
	}
	fixture.connection = connectionRecord{Connection: Connection{
		ID: "connection", Owner: owner, Provider: "azure", Mode: modeAgent, Identity: &IdentityBinding{InstanceUID: fixture.agent.Metadata.UID, TenantID: owner.TenantID, ClientID: testFirstAgentID, PrincipalID: fixture.agent.Status.AgentIdentity.ObjectID}, AccountID: fixture.agent.Status.AgentIdentity.ObjectID, AccountDisplay: "original@example.com",
		Services: agenttools.ProviderServices("azure"), Revision: 1, Status: "connected", CreatedAt: now, UpdatedAt: now,
	}, InstanceName: "agent", InstanceNamespace: "agents"}
	fixture.binding = ConnectionBinding{
		ID: bindingKey(fixture.agent.Metadata.UID, fixture.connection.Connection.ID), InstanceUID: fixture.agent.Metadata.UID, ConnectionID: fixture.connection.Connection.ID, Owner: owner,
		Services: []string{"azure_arm"}, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	tokenFile := filepath.Join(t.TempDir(), "synthetic-kubernetes-token")
	if err := os.WriteFile(tokenFile, []byte("synthetic-kubernetes-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &kubeClient{baseURL: "https://kubernetes.example.com", namespace: "credentials", agentNamespaces: true, tokenFile: tokenFile, wrapper: memoryWrapper{key: []byte("synthetic-encryption-key")}}
	store.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		path := request.URL.Path
		if request.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/namespaces/credentials/secrets/agent-tools-connection-") {
			return securityJSONResponse(t, http.StatusNotFound, map[string]string{"message": "not found"}), nil
		}
		if request.Method == http.MethodPost && path == "/apis/authentication.k8s.io/v1/tokenreviews" {
			var input tokenReview
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(input.Spec.Audiences, []string{"cetirun-connections-mcp"}) {
				t.Fatalf("TokenReview audiences = %#v", input.Spec.Audiences)
			}
			return securityJSONResponse(t, http.StatusOK, map[string]any{"status": map[string]any{
				"authenticated": true, "audiences": []string{"cetirun-connections-mcp"},
				"user": map[string]any{"username": "system:serviceaccount:agents:openclaw-agent-cetirun", "uid": "service-account-uid", "extra": map[string][]string{
					"authentication.kubernetes.io/pod-name": {"agent-pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"},
				}},
			}}), nil
		}
		switch path {
		case "/api/v1/namespaces/agents":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"metadata": secretMetadata{Name: "agents", OwnerReferences: []ownerReference{{APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: "agent", UID: "11111111-1111-1111-1111-111111111111"}}}}), nil
		case "/api/v1/namespaces/agents/serviceaccounts/openclaw-agent-cetirun":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"metadata": map[string]string{"uid": "service-account-uid"}}), nil
		case "/api/v1/namespaces/agents/pods/agent-pod":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"metadata": map[string]string{"uid": "pod-uid"}, "spec": map[string]string{"serviceAccountName": "openclaw-agent-cetirun"}}), nil
		case "/apis/cetirun.microsoft.com/v1alpha1/agents":
			return securityJSONResponse(t, http.StatusOK, agentList{Items: []agentDocument{fixture.agent}}), nil
		case "/apis/cetirun.microsoft.com/v1alpha1/agents/agent":
			return securityJSONResponse(t, http.StatusOK, fixture.agent), nil
		}
		prefix := "/api/v1/namespaces/agents/secrets"
		if !strings.HasPrefix(path, prefix) {
			t.Fatalf("unexpected Kubernetes request: %s %s", request.Method, path)
		}
		name := strings.TrimPrefix(path, prefix+"/")
		if request.Method == http.MethodPost || request.Method == http.MethodPut {
			var secret secretDocument
			if err := json.NewDecoder(request.Body).Decode(&secret); err != nil {
				t.Fatal(err)
			}
			existing, present := fixture.secrets[secret.Metadata.Name]
			if (request.Method == http.MethodPost && present) || (request.Method == http.MethodPut && (!present || secret.Metadata.ResourceVersion != existing.Metadata.ResourceVersion)) {
				return securityJSONResponse(t, http.StatusConflict, map[string]any{}), nil
			}
			if secret.Metadata.Namespace != "agents" {
				t.Fatalf("wrong namespace: %s", secret.Metadata.Namespace)
			}
			secret.Metadata.UID = "secret-" + secret.Metadata.Name
			fixture.version++
			secret.Metadata.ResourceVersion = strconv.FormatUint(fixture.version, 10)
			fixture.secrets[secret.Metadata.Name] = secret
			return securityJSONResponse(t, http.StatusOK, secret), nil
		}
		if path == prefix {
			items := []secretDocument{}
			for _, secret := range fixture.secrets {
				matches := true
				for _, selector := range strings.Split(request.URL.Query().Get("labelSelector"), ",") {
					if selector == "" {
						continue
					}
					label, value, present := strings.Cut(selector, "=")
					if !present || secret.Metadata.Labels[label] != value {
						matches = false
					}
				}
				if matches {
					items = append(items, secret)
				}
			}
			return securityJSONResponse(t, http.StatusOK, secretList{Items: items}), nil
		}
		secret, present := fixture.secrets[name]
		if !present {
			return securityJSONResponse(t, http.StatusNotFound, map[string]any{}), nil
		}

		if request.Method == http.MethodDelete {
			var options struct {
				Preconditions map[string]string `json:"preconditions"`
			}
			if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
				t.Fatal(err)
			}
			if options.Preconditions["uid"] != secret.Metadata.UID || options.Preconditions["resourceVersion"] != secret.Metadata.ResourceVersion {
				return securityJSONResponse(t, 409, map[string]any{}), nil
			}
			delete(fixture.secrets, name)
		}
		return securityJSONResponse(t, http.StatusOK, secret), nil
	})}
	fixture.server = &Server{cfg: cfg, store: store, mux: http.NewServeMux(), tlsMux: http.NewServeMux()}
	fixture.server.routes()
	if err := store.putConnection(context.Background(), &fixture.connection, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.putBinding(context.Background(), &bindingRecord{Binding: fixture.binding, InstanceName: "agent", InstanceNamespace: "agents"}, ""); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *connectionFixture) request(t *testing.T, path string, configure ...func(*serviceRequest)) *httptest.ResponseRecorder {
	t.Helper()
	input := serviceRequest{CallerProof: "synthetic-workload-proof", Service: fixture.binding.Services[0], ConnectionID: fixture.connection.Connection.ID, BindingRevision: &fixture.binding.Revision, ConnectionRevision: &fixture.connection.Connection.Revision, ContractVersion: agenttools.ContractVersion}
	for _, apply := range configure {
		apply(&input)
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	identity, err := url.Parse("spiffe://cluster.local/ns/cetirun-system/sa/cetirun-connections-mcp")
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{URIs: []*url.URL{identity}}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}
	response := httptest.NewRecorder()
	fixture.server.TLSHandler().ServeHTTP(response, request)
	return response
}

func (fixture *connectionFixture) mutateBinding(t *testing.T, change func(*ConnectionBinding)) {
	t.Helper()
	record, version, err := fixture.server.store.getBinding(context.Background(), fixture.binding.ID, agentFromDocument(fixture.agent))
	if err != nil {
		t.Fatal(err)
	}
	change(&record.Binding)
	if err := fixture.server.store.putBinding(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
}

func TestAzureWorkloadProofRejectsTerminatingBindings(t *testing.T) {
	for _, resource := range []string{"active", "serviceaccount", "pod"} {
		t.Run(resource, func(t *testing.T) {
			fixture := newConnectionFixture(t)
			transport := fixture.server.store.client.Transport
			fixture.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if resource == "serviceaccount" && strings.Contains(request.URL.Path, "/serviceaccounts/") {
					return securityJSONResponse(t, http.StatusOK, map[string]any{"metadata": map[string]any{
						"uid": "service-account-uid", "deletionTimestamp": time.Now().UTC(),
					}}), nil
				}
				if resource == "pod" && strings.Contains(request.URL.Path, "/pods/") {
					return securityJSONResponse(t, http.StatusOK, map[string]any{
						"metadata": map[string]any{"uid": "pod-uid", "deletionTimestamp": time.Now().UTC()},
						"spec":     map[string]string{"serviceAccountName": "openclaw-agent-cetirun"},
					}), nil
				}
				return transport.RoundTrip(request)
			})
			_, err := fixture.server.store.verifyWorkloadProof(context.Background(), "synthetic-workload-proof", "cetirun-connections-mcp")
			if resource == "active" {
				if err != nil {
					t.Fatalf("active workload rejected: %v", err)
				}
			} else if !errors.Is(err, errWorkloadProofInvalid) {
				t.Fatalf("terminating %s proof = %v, want invalid proof", resource, err)
			}
		})
	}
}

// Azure RBAC is evaluated by ARM. A binding only selects an identity and service.
func TestAzureConnectionRequiresNoResourceGrant(t *testing.T) {
	f := newConnectionFixture(t)
	for _, path := range []string{"/internal/v1/service-authorizations/list", "/internal/v1/service-authorizations/check"} {
		response := f.request(t, path)
		if response.Code != 200 || strings.Contains(response.Body.String(), "accessToken") {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
	}
	agent := agentFromDocument(f.agent)
	connection, _, err := f.server.store.getConnectionForAgent(context.Background(), agent, f.connection.Connection.ID)
	if err != nil || !reflect.DeepEqual(connection, &f.connection) {
		t.Fatalf("encrypted connection round trip failed: %v", err)
	}
	for _, secret := range f.secrets {
		if secret.Metadata.Namespace != "agents" || !recordOwnedBy(secret, agent) || secret.Metadata.Labels[recordLabelPrefix+"instance-name"] != "agent" {
			t.Fatal("record is not identifiable and owned by its Agent")
		}
		if secret.Data["envelope.json"] == "" {
			t.Fatal("unencrypted record")
		}
	}
	f.mutateBinding(t, func(binding *ConnectionBinding) { binding.Revoked = true })
	binding, _, err := f.server.store.getBinding(context.Background(), f.binding.ID, agent)
	if err != nil || !binding.Binding.Revoked || !sameOwner(binding.Binding.Owner, f.binding.Owner) {
		t.Fatalf("private binding fields lost: %v", err)
	}
}

func TestAzureCredentialRechecksIdentityAndBinding(t *testing.T) {
	for _, change := range []string{"none", "binding", "principal", "pod"} {
		t.Run(change, func(t *testing.T) {
			f := newConnectionFixture(t)
			calls := 0
			f.server.oauth = agentOAuthTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				form := agentOAuthForm(t, r)
				if form.Get("grant_type") != "client_credentials" || form.Get("refresh_token") != "" {
					t.Fatal("personal credentials used")
				}
				if calls == 2 {
					switch change {
					case "binding":
						f.mutateBinding(t, func(b *ConnectionBinding) { b.Revision++ })
					case "principal":
						f.agent.Status.AgentIdentity.ObjectID = testSecondAgentID
					case "pod":
						original := f.server.store.client.Transport
						f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
							if strings.Contains(r.URL.Path, "/pods/") {
								return securityJSONResponse(t, 404, map[string]any{}), nil
							}
							return original.RoundTrip(r)
						})
					}
				}
				return securityJSONResponse(t, 200, map[string]any{"access_token": "opaque-agent-token", "token_type": "Bearer", "expires_in": 3600}), nil
			}))
			response := f.request(t, "/internal/v1/service-credentials/issue")
			expected := 200
			if change != "none" {
				expected = 409
			}
			if response.Code != expected || calls != 2 {
				t.Fatalf("issuance: status %d, exchanges %d: %s", response.Code, calls, response.Body)
			}
			if change != "none" && strings.Contains(response.Body.String(), "opaque-agent-token") {
				t.Fatal("changed authorization leaked token")
			}
		})
	}
}

func TestAzureBindingRejectsStaleOrForeignAuthorization(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *connectionFixture)
	}{
		{"unbound", func(t *testing.T, f *connectionFixture) { delete(f.secrets, toolRecordName("binding", f.binding.ID)) }},
		{"revoked", func(t *testing.T, f *connectionFixture) {
			f.mutateBinding(t, func(b *ConnectionBinding) { b.Revoked = true })
		}},
		{"disabled", func(t *testing.T, f *connectionFixture) {
			f.mutateBinding(t, func(b *ConnectionBinding) { b.Enabled = false })
		}},
		{"revision", func(t *testing.T, f *connectionFixture) {
			f.mutateBinding(t, func(b *ConnectionBinding) { b.Revision++ })
		}},
		{"principal", func(t *testing.T, f *connectionFixture) { f.agent.Status.AgentIdentity.ObjectID = testSecondAgentID }},
		{"client", func(t *testing.T, f *connectionFixture) { f.agent.Status.AgentIdentity.ClientID = testSecondAgentID }},
		{"tenant", func(t *testing.T, f *connectionFixture) { f.server.cfg.TenantID = testSecondAgentID }},
		{"owner", func(t *testing.T, f *connectionFixture) {
			f.agent.Metadata.Annotations["agentidentity.cetirun.microsoft.com/owner-object-id"] = testSecondAgentID
		}},
		{"recreated agent", func(t *testing.T, f *connectionFixture) { f.agent.Metadata.UID = testSecondAgentID }},
		{"not ready", func(t *testing.T, f *connectionFixture) { f.agent.Status.AgentIdentity.Phase = "Pending" }},
		{"suspended", func(t *testing.T, f *connectionFixture) { f.agent.Spec.Suspended = true }},
		{"custom", func(t *testing.T, f *connectionFixture) { f.agent.Spec.Agent.Type = "custom" }},
		{"sandbox", func(t *testing.T, f *connectionFixture) { f.agent.Spec.SandboxRef = "shared" }},
		{"Azure mode off", func(t *testing.T, f *connectionFixture) { f.server.cfg.DisableAzureAgent = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConnectionFixture(t)
			test.mutate(t, f)
			f.server.oauth = agentOAuthTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("denied request acquired a provider token")
				return nil, nil
			}))
			response := f.request(t, "/internal/v1/service-credentials/issue")
			if response.Code == 200 || strings.Contains(response.Body.String(), "accessToken") {
				t.Fatalf("unauthorized issuance: %d %s", response.Code, response.Body)
			}
		})
	}
}

func (f *connectionFixture) manage(t *testing.T, method, path string, body any, revision string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
	request.Header.Set("X-CetiRun-Owner-Tenant-ID", f.connection.Connection.Owner.TenantID)
	request.Header.Set("X-CetiRun-Owner-Object-ID", f.connection.Connection.Owner.ObjectID)
	request.Header.Set("X-CetiRun-Instance-UID", f.agent.Metadata.UID)
	if revision != "" {
		request.Header.Set("If-Match", revision)
	}
	response := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(response, request)
	return response
}

func TestConnectionManagementLifecycleAndOptimisticRevisions(t *testing.T) {
	f := newConnectionFixture(t)
	path := "/v1/instances/agent/tool-identities"
	first := f.manage(t, "POST", path, map[string]string{"provider": "azure"}, "")
	if first.Code != 200 {
		t.Fatalf("create: %d %s", first.Code, first.Body)
	}
	var connection Connection
	if err := json.Unmarshal(first.Body.Bytes(), &connection); err != nil {
		t.Fatal(err)
	}
	again := f.manage(t, "POST", path, map[string]string{"provider": "azure"}, "")
	if again.Code != 200 || again.Body.String() != first.Body.String() {
		t.Fatal("identity create was not idempotent")
	}
	base := "/v1/instances/agent/connection-bindings"
	input := bindingRequest{ConnectionID: connection.ID, Services: []string{"azure_arm"}, Enabled: true}
	created := f.manage(t, "POST", base, input, "")
	if created.Code != 200 {
		t.Fatalf("bind: %d %s", created.Code, created.Body)
	}
	var binding ConnectionBinding
	if err := json.Unmarshal(created.Body.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	if response := f.manage(t, "POST", base, input, ""); response.Code != 409 {
		t.Fatal("duplicate binding overwrote active binding")
	}
	input.Revision = binding.Revision
	if response := f.manage(t, "PUT", base+"/"+binding.ID, input, ""); response.Code != 200 {
		t.Fatalf("update: %d %s", response.Code, response.Body)
	}
	if response := f.manage(t, "PUT", base+"/"+binding.ID, input, ""); response.Code != 409 {
		t.Fatal("stale update accepted")
	}
	if response := f.manage(t, "DELETE", base+"/"+binding.ID, nil, ""); response.Code != 428 {
		t.Fatal("delete without revision accepted")
	}
	if response := f.manage(t, "DELETE", base+"/"+binding.ID, nil, "1"); response.Code != 409 {
		t.Fatal("stale delete accepted")
	}
	if response := f.manage(t, "DELETE", base+"/"+binding.ID, nil, "2"); response.Code != 204 {
		t.Fatalf("delete: %d %s", response.Code, response.Body)
	}
	reconnected := f.manage(t, "POST", base, input, "")
	if reconnected.Code != 200 {
		t.Fatalf("reconnect: %d %s", reconnected.Code, reconnected.Body)
	}
	if err := json.Unmarshal(reconnected.Body.Bytes(), &binding); err != nil || binding.Revision != 4 {
		t.Fatal("reconnect reused revoked revision")
	}
	for _, services := range [][]string{{"github"}, {"azure_arm", "azure_arm"}, nil} {
		input.Services = services
		input.Revision = binding.Revision
		if response := f.manage(t, "PUT", base+"/"+binding.ID, input, ""); response.Code != 400 {
			t.Fatalf("invalid service selection accepted: %v", services)
		}
	}
}

func TestConnectionRecordsRejectTamperingAndNamespaceCollision(t *testing.T) {
	for _, tamper := range []string{"namespace owner", "secret owner", "metadata", "envelope"} {
		t.Run(tamper, func(t *testing.T) {
			f := newConnectionFixture(t)
			key := toolRecordName("connection", f.connection.Connection.ID)
			secret := f.secrets[key]
			switch tamper {
			case "namespace owner":
				original := f.server.store.client.Transport
				f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v1/namespaces/agents" {
						return securityJSONResponse(t, 200, map[string]any{"metadata": secretMetadata{Name: "agents", OwnerReferences: []ownerReference{{APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: "agent", UID: testSecondAgentID}}}}), nil
					}
					return original.RoundTrip(r)
				})
			case "secret owner":
				secret.Metadata.OwnerReferences[0].UID = testSecondAgentID
			case "metadata":
				secret.Metadata.Labels[recordLabelPrefix+"owner-object"] = testSecondAgentID
			case "envelope":
				secret.Data["envelope.json"] = "corrupt"
			}
			f.secrets[key] = secret
			if _, _, err := f.server.store.getConnectionForAgent(context.Background(), agentFromDocument(f.agent), f.connection.Connection.ID); err == nil {
				t.Fatal("tampered record accepted")
			}
		})
	}
}

func TestAgentCleanupDeletesOnlyOwnedRecordsWithPreconditions(t *testing.T) {
	f := newConnectionFixture(t)
	now := time.Now().UTC()
	f.agent.Metadata.DeletionTimestamp = &now
	agent := agentFromDocument(f.agent)
	if err := f.server.store.deleteAgentRecords(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if len(f.secrets) != 0 {
		t.Fatal("Agent records orphaned")
	}
	if err := f.server.store.deleteAgentRecords(context.Background(), agent); err != nil {
		t.Fatal("repeated cleanup failed", err)
	}
	if err := f.server.store.putConnection(context.Background(), &f.connection, ""); err == nil {
		t.Fatal("created record while Agent deleting")
	}
}

func TestServiceProofRequiresCurrentAudiencePodAndServiceAccount(t *testing.T) {
	for _, test := range []string{"wrong audience", "multiple audiences", "old pod", "old service account", "dependency failure"} {
		t.Run(test, func(t *testing.T) {
			f := newConnectionFixture(t)
			original := f.server.store.client.Transport
			f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(test, "audience") && strings.HasSuffix(r.URL.Path, "/tokenreviews"):
					audience := []string{"other"}
					if test == "multiple audiences" {
						audience = []string{"cetirun-connections-mcp", "other"}
					}
					return securityJSONResponse(t, 200, map[string]any{"status": map[string]any{"authenticated": true, "audiences": audience}}), nil
				case test == "old pod" && strings.Contains(r.URL.Path, "/pods/"):
					return securityJSONResponse(t, 200, map[string]any{"metadata": map[string]string{"uid": "replacement-pod"}, "spec": map[string]string{"serviceAccountName": "openclaw-agent-cetirun"}}), nil
				case test == "old service account" && strings.Contains(r.URL.Path, "/serviceaccounts/"):
					return securityJSONResponse(t, 200, map[string]any{"metadata": map[string]string{"uid": "replacement-sa"}}), nil
				case test == "dependency failure" && strings.HasSuffix(r.URL.Path, "/tokenreviews"):
					return securityJSONResponse(t, 503, map[string]any{}), nil
				}
				return original.RoundTrip(r)
			})
			response := f.request(t, "/internal/v1/service-authorizations/check")
			want := 401
			if test == "dependency failure" {
				want = 503
			}
			if response.Code != want {
				t.Fatalf("proof result: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestToolServiceRequiresExactVerifiedCertificateIdentity(t *testing.T) {
	cfg := Config{ToolServiceNamespace: "tools", ToolServiceAccount: "broker-client"}
	correct, _ := url.Parse("spiffe://cluster.local/ns/tools/sa/broker-client")
	other, _ := url.Parse("spiffe://cluster.local/ns/tools/sa/other")
	for _, test := range []struct {
		name     string
		uris     []*url.URL
		verified bool
		want     bool
	}{
		{"exact", []*url.URL{correct}, true, true},
		{"wrong service", []*url.URL{other}, true, false},
		{"ambiguous", []*url.URL{correct, other}, true, false},
		{"unverified", []*url.URL{correct}, false, false},
	} {
		request := httptest.NewRequest("POST", "/", nil)
		cert := &x509.Certificate{URIs: test.uris}
		request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		if test.verified {
			request.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
		}
		if toolServiceTLSPrincipalAllowed(request, cfg) != test.want {
			t.Fatal(test.name)
		}
	}
	request := httptest.NewRequest("POST", "/", nil)
	request.Header.Set("X-Forwarded-Client-Cert", "URI="+correct.String())
	if toolServiceTLSPrincipalAllowed(request, cfg) {
		t.Fatal("XFCC substituted for TLS certificate")
	}
}

func TestConnectionManagementRejectsForeignCaller(t *testing.T) {
	f := newConnectionFixture(t)
	for _, changed := range []string{"principal", "owner", "agent UID"} {
		request := httptest.NewRequest("GET", "/v1/instances/agent/connection-bindings", nil)
		request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
		request.Header.Set("X-CetiRun-Owner-Tenant-ID", f.connection.Connection.Owner.TenantID)
		request.Header.Set("X-CetiRun-Owner-Object-ID", f.connection.Connection.Owner.ObjectID)
		request.Header.Set("X-CetiRun-Instance-UID", f.agent.Metadata.UID)
		switch changed {
		case "principal":
			request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/agents/sa/openclaw-agent-cetirun")
		case "owner":
			request.Header.Set("X-CetiRun-Owner-Object-ID", testSecondAgentID)
		case "agent UID":
			request.Header.Set("X-CetiRun-Instance-UID", testSecondAgentID)
		}
		response := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(response, request)
		if response.Code != 403 {
			t.Fatalf("%s accepted", changed)
		}
	}
}

func TestAzureAgentListPaginationIsCompleteAndBounded(t *testing.T) {
	for _, variant := range []string{"complete", "repeated", "cycle", "second-page-fails", "oversize"} {
		t.Run(variant, func(t *testing.T) {
			f := newConnectionFixture(t)
			calls := 0
			f.server.store.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Query().Get("limit") != "50" {
					t.Error("list has no page bound")
				}
				if variant == "second-page-fails" && calls == 2 {
					return securityJSONResponse(t, 503, map[string]any{}), nil
				}
				list := agentList{}
				count := 50
				if variant == "oversize" {
					count = 120
				}
				for i := 0; i < count; i++ {
					a := f.agent
					a.Metadata.Name = fmt.Sprintf("agent-%d-%d", calls, i)
					a.Metadata.Annotations = map[string]string{"example.com/data": strings.Repeat("x", 12000)}
					list.Items = append(list.Items, a)
				}
				switch variant {
				case "repeated":
					list.Metadata.Continue = "same"
				case "cycle":
					list.Metadata.Continue = fmt.Sprint(calls % 2)
				default:
					if calls < 3 {
						list.Metadata.Continue = fmt.Sprint(calls)
					}
				}
				return securityJSONResponse(t, 200, list), nil
			})}
			result, err := f.server.store.listAgents(context.Background())
			if variant == "complete" {
				if err != nil || len(result.Items) != 150 || calls != 3 {
					t.Fatalf("incomplete list items=%d calls=%d err=%v", len(result.Items), calls, err)
				}
			} else if err == nil || len(result.Items) != 0 {
				t.Fatal("partial or invalid list accepted")
			}
		})
	}
}

func TestAzureBindingDisconnectAuditMatchesStoredOutcome(t *testing.T) {
	for _, variant := range []string{"success", "conflict", "write-response-lost", "unauthenticated"} {
		t.Run(variant, func(t *testing.T) {
			f := newConnectionFixture(t)
			f.server.cfg.PortalNamespace, f.server.cfg.PortalServiceAccount = "portal", "portal"
			var logs bytes.Buffer
			f.server.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			original := f.server.store.client.Transport
			if variant == "write-response-lost" {
				f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					response, err := original.RoundTrip(r)
					if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, toolRecordName("binding", f.binding.ID)) {
						if response != nil {
							response.Body.Close()
						}
						return nil, errors.New("synthetic response lost after commit")
					}
					return response, err
				})
			}
			request := httptest.NewRequest("DELETE", "/v1/instances/agent/connection-bindings/"+f.binding.ID, nil)
			request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
			request.Header.Set("X-CetiRun-Owner-Tenant-ID", f.binding.Owner.TenantID)
			request.Header.Set("X-CetiRun-Owner-Object-ID", f.binding.Owner.ObjectID)
			request.Header.Set("X-CetiRun-Instance-UID", f.binding.InstanceUID)
			request.Header.Set("If-Match", "1")
			request.Header.Set("X-Correlation-ID", "disconnect-audit")
			if variant == "conflict" {
				request.Header.Set("If-Match", "2")
			}
			if variant == "unauthenticated" {
				request.Header.Del("X-Forwarded-Client-Cert")
			}
			response := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(response, request)
			wantStatus := map[string]int{"success": 204, "conflict": 409, "write-response-lost": 503, "unauthenticated": 403}[variant]
			if response.Code != wantStatus {
				t.Fatalf("HTTP status = %d, want %d", response.Code, wantStatus)
			}
			record, _, err := f.server.store.getBinding(context.Background(), f.binding.ID, agentFromDocument(f.agent))
			wantRevoked := variant == "success" || variant == "write-response-lost"
			if err != nil || record.Binding.Revoked != wantRevoked {
				t.Fatal("unexpected persisted binding state")
			}
			entries := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(entries) != 2 {
				t.Fatalf("expected audit lifecycle, got %d entries", len(entries))
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(entries[1]), &event); err != nil {
				t.Fatal(err)
			}
			wantOutcome := map[string]string{"success": "succeeded", "conflict": "rejected", "write-response-lost": "execution_unknown", "unauthenticated": "rejected"}[variant]
			if event["operation"] != "binding.disconnect" || event["outcome"] != wantOutcome || event["correlation_id"] != "disconnect-audit" {
				t.Fatalf("unexpected disconnect audit: %v", event)
			}
			if wantRevoked && (event["agent_uid"] != f.binding.InstanceUID || event["binding_id"] != f.binding.ID) {
				t.Fatal("audit lost mutation target")
			}
			if variant == "unauthenticated" && event["owner_object_id"] != nil {
				t.Fatal("unverified owner appeared in audit")
			}
		})
	}
}

func TestOperatorCleanupBeforeNamespaceProvisioningIsIdempotent(t *testing.T) {
	f := newConnectionFixture(t)
	f.agent.Status.Namespace = ""
	f.secrets = map[string]secretDocument{}
	original := f.server.store.client.Transport
	f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/credentials/secrets/"+toolRecordName(credentialPolicyRecord, f.agent.Metadata.UID) {
			return securityJSONResponse(t, 404, map[string]any{}), nil
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/credentials/secrets/agent-authorization-") {
			return securityJSONResponse(t, 404, map[string]any{}), nil
		}
		if r.URL.Path == "/api/v1/namespaces/credentials/secrets" && r.Method == "POST" {
			// Preserve the legacy revocation tombstone even without an Agent namespace.
			return securityJSONResponse(t, 200, map[string]any{}), nil
		}
		if strings.Contains(r.URL.Path, "/api/v1/namespaces/") {
			t.Fatalf("cleanup accessed an unprovisioned namespace: %s", r.URL.Path)
		}
		return original.RoundTrip(r)
	})
	request := httptest.NewRequest("DELETE", "/operator/v1/instances/"+f.agent.Metadata.UID+"/authorization", nil)
	request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/operator/sa/operator")
	response := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(response, request)
	if response.Code != 204 {
		t.Fatalf("unprovisioned namespace blocked Agent deletion: %d %s", response.Code, response.Body)
	}
}
