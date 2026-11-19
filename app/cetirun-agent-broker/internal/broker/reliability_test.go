// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdentityModeConfiguration(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			for name, value := range map[string]string{
				"ENTRA_TENANT_ID": namespaceTenant, "AZURE_CLIENT_ID": namespaceOwner,
				"AGENT_ID_BLUEPRINT_CLIENT_ID": namespaceAgentUID, "AGENT_ID_AUTHORIZATION_CLIENT_ID": namespaceAgentUID,
				"AZURE_AGENT_ENABLED": "true", "MICROSOFT_DELEGATED_ENABLED": "true",
				"AZURE_FEDERATED_TOKEN_FILE": "/synthetic/assertion", "KEY_VAULT_KEY_URL": "https://example.vault.azure.net/keys/cache",
				"AGENT_ID_REDIRECT_URI": "https://portal.example.com/api/v1/connections/callback", "PORTAL_ORIGIN": "https://portal.example.com", "PORTAL_BASE_URL": "https://portal.example.com",
				"PORTAL_NAMESPACE": "portal", "PORTAL_SERVICE_ACCOUNT": "portal", "OPERATOR_NAMESPACE": "system", "OPERATOR_SERVICE_ACCOUNT": "operator", "TOOL_SERVICE_NAMESPACE": "tools", "TOOL_SERVICE_ACCOUNT": "mcp",
			} {
				t.Setenv(name, value)
			}
			if disabled {
				t.Setenv("AZURE_AGENT_ENABLED", "false")
				t.Setenv("MICROSOFT_DELEGATED_ENABLED", "false")
				t.Setenv("AGENT_ID_BLUEPRINT_CLIENT_ID", "")
				t.Setenv("AGENT_ID_AUTHORIZATION_CLIENT_ID", "")
			}
			cfg, err := LoadConfig()
			if err != nil || cfg.DisableAzureAgent != disabled || cfg.DisableMicrosoftDelegated != disabled {
				t.Fatalf("mode config: %v", err)
			}
			t.Setenv("AZURE_AGENT_ENABLED", "invalid")
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid mode flag accepted")
			}
		})
	}
}

func TestSDKOutageDoesNotAffectOtherIdentityModes(t *testing.T) {
	for _, provider := range []string{"github", "google", "azure-agent", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			var f *authorizationFixture
			switch provider {
			case "azure-agent":
				f = prepareAgentAuthorizationFixture(t)
			case "google":
				f = newAuthorizationFixture(t, "google", "google_gmail")
			case "microsoft":
				f = newAuthorizationFixture(t, "microsoft", "microsoft_graph")
			default:
				f = newAuthorizationFixture(t, "github", "github")
			}
			sdkCalls := 0
			f.server.authSDK = &authSDKClient{baseURL: "http://sdk.example", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				sdkCalls++
				return nil, errors.New("synthetic SDK unavailable")
			})}}
			if provider == "azure-agent" {
				f.server.oauth = agentOAuthTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
					return securityJSONResponse(t, 200, map[string]any{"access_token": "synthetic-agent-token", "token_type": "Bearer", "expires_in": 3600}), nil
				}))
			}
			if provider == "microsoft" {
				f.agent.Status.AgentIdentity = &struct {
					Phase    string `json:"phase"`
					ClientID string `json:"clientId"`
					ObjectID string `json:"objectId"`
				}{"Ready", testFirstAgentID, testAgentPrincipalID}
				f.agent.Spec.AgentIdentity = &struct {
					Enabled bool `json:"enabled"`
				}{true}
			}
			ready := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
			if ready.Code != 204 {
				t.Fatal("SDK affected Broker readiness")
			}
			response := f.request(t, "/internal/v1/service-credentials/issue")
			if provider == "microsoft" {
				if response.Code != 503 || sdkCalls != 1 {
					t.Fatalf("delegated error status=%d calls=%d", response.Code, sdkCalls)
				}
			} else if response.Code != 200 || sdkCalls != 0 {
				t.Fatalf("unrelated flow status=%d SDK calls=%d", response.Code, sdkCalls)
			}
			current, _, err := f.server.store.getConnection(context.Background(), f.connection.Connection.ID)
			if err != nil || current.Connection.Status != "connected" {
				t.Fatal("dependency failure changed authorization")
			}
		})
	}
}

func TestDisabledModesRejectIssuanceAndSignIn(t *testing.T) {
	for _, independent := range []bool{true, false} {
		t.Run(fmt.Sprint(independent), func(t *testing.T) {
			f := newAuthorizationFixture(t, "microsoft", "microsoft_graph")
			if independent {
				f = prepareAgentAuthorizationFixture(t)
				f.server.cfg.DisableAzureAgent = true
			} else {
				f.server.cfg.DisableMicrosoftDelegated = true
			}
			response := f.request(t, "/internal/v1/service-credentials/issue")
			if response.Code != 503 || !strings.Contains(response.Body.String(), "dependency_unavailable") {
				t.Fatalf("disabled mode response: %d", response.Code)
			}
		})
	}
	f := newAuthorizationFixture(t, "microsoft", "microsoft_graph")
	f.server.cfg.DisableMicrosoftDelegated = true
	response := httptest.NewRecorder()
	f.server.startAuthorization(response, httptest.NewRequest("POST", "/v1/connections/start", nil), f.connection.Connection.Owner, "microsoft", "", "")
	if response.Code != 503 {
		t.Fatal("disabled OAuth sign-in accepted")
	}
}

func TestWorkloadNamespaceOwnerLookup(t *testing.T) {
	for _, variant := range []string{"valid", "recreated", "missing-owner", "wrong-owner", "duplicate-owner", "deleting-namespace", "wrong-namespace"} {
		t.Run(variant, func(t *testing.T) {
			f := newAuthorizationFixture(t, "github", "github")
			original := f.server.store.client.Transport
			owner := ownerReference{"cetirun.microsoft.com/v1alpha1", "Agent", "agent", namespaceAgentUID}
			meta := secretMetadata{Name: "agents", OwnerReferences: []ownerReference{owner}}
			switch variant {
			case "recreated":
				f.agent.Metadata.UID = "99999999-9999-9999-9999-999999999999"
			case "missing-owner":
				meta.OwnerReferences = nil
			case "wrong-owner":
				meta.OwnerReferences[0].UID = "99999999-9999-9999-9999-999999999999"
			case "duplicate-owner":
				meta.OwnerReferences = append(meta.OwnerReferences, owner)
			case "deleting-namespace":
				now := time.Now()
				meta.DeletionTimestamp = &now
			case "wrong-namespace":
				f.agent.Status.Namespace = "another-namespace"
			}
			f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/apis/cetirun.microsoft.com/v1alpha1/agents" {
					t.Error("workload path listed Agents")
					return nil, errors.New("unexpected list")
				}
				if r.URL.Path == "/api/v1/namespaces/agents" {
					return securityJSONResponse(t, 200, map[string]any{"metadata": meta}), nil
				}
				return original.RoundTrip(r)
			})
			response := f.request(t, "/internal/v1/service-authorizations/check")
			if (response.Code == 200) != (variant == "valid") {
				t.Fatalf("namespace check status %d", response.Code)
			}
		})
	}
}

func TestAgentListPaginationIsCompleteAndBounded(t *testing.T) {
	for _, variant := range []string{"complete", "repeated", "cycle", "second-page-fails", "oversize"} {
		t.Run(variant, func(t *testing.T) {
			f := newNamespaceFixture(t)
			calls := 0
			f.store.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
			result, err := f.store.listAgents(context.Background())
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

func TestBackgroundSweepOnlyExpiresOAuthTransactions(t *testing.T) {
	f := newNamespaceFixture(t)
	ctx := context.Background()
	owner := Owner{namespaceTenant, namespaceOwner}
	now := time.Now()
	for _, id := range []string{"old", "fresh", "conflict", "no-expiry"} {
		expiry := now.Add(-2 * time.Hour)
		if id == "fresh" {
			expiry = now.Add(time.Minute)
		}
		if id == "no-expiry" {
			expiry = time.Time{}
		}
		r := &connectionRecord{Connection: Connection{ID: id, Provider: "github", Owner: owner, Revision: 1}, Transaction: &connectionTransaction{ExpiresAt: expiry}}
		if err := f.store.putOAuthRecord(ctx, "authorization", r, ""); err != nil {
			t.Fatal(err)
		}
	}
	account := &connectionRecord{Connection: Connection{ID: "account", Provider: "github", Owner: owner, Mode: modeDelegated, Revision: 1}}
	if err := f.store.putConnection(ctx, account, ""); err != nil {
		t.Fatal(err)
	}
	index := &connectionRecord{Connection: Connection{ID: "index", Provider: "github", Owner: owner, Revision: 1}}
	if err := f.store.putOAuthRecord(ctx, "account-index", index, ""); err != nil {
		t.Fatal(err)
	}
	broken := secretDocument{Metadata: secretMetadata{Name: "broken", Namespace: "credentials", Labels: map[string]string{recordLabelPrefix + "record-type": "authorization"}}}
	f.secrets["credentials"]["broken"] = broken
	f.conflictNextDelete = true
	if err := f.store.pruneAuthorizationRecords(ctx, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pruneAuthorizationRecords(ctx, nil, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "conflict"} {
		if _, _, err := f.store.getOAuthRecord(ctx, "authorization", id); !errors.Is(err, errNotFound) {
			t.Fatal("expired transaction retained")
		}
	}
	for _, id := range []string{toolRecordName("connection", "account"), toolRecordName("account-index", "index"), toolRecordName("authorization", "fresh"), toolRecordName("authorization", "no-expiry"), "broken"} {
		if _, ok := f.secrets["credentials"][id]; !ok {
			t.Fatalf("unrelated record %s removed", id)
		}
	}
}

func TestMaintenanceRunsWithoutSignInAndStops(t *testing.T) {
	f := newNamespaceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	original := f.store.client.Transport
	swept := make(chan struct{}, 1)
	f.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if strings.Contains(r.URL.RawQuery, "authorization") {
			select {
			case swept <- struct{}{}:
			default:
			}
		}
		return response, err
	})
	s := &Server{store: f.store}
	done := make(chan struct{})
	go func() { s.RunMaintenance(ctx); close(done) }()
	select {
	case <-swept:
	case <-time.After(time.Second):
		t.Fatal("no autonomous sweep")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
}

func TestManagementAuditTracksMutationOutcomes(t *testing.T) {
	for _, variant := range []string{"success", "rejected", "unknown", "unauthenticated"} {
		t.Run(variant, func(t *testing.T) {
			var logs bytes.Buffer
			s := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			handler := requestSecurityHeaders(s.auditManagement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if variant != "unauthenticated" {
					auditOwner(r, Owner{namespaceTenant, namespaceOwner})
					auditTarget(r, "connection", namespaceAgentUID, "binding")
				}
				switch variant {
				case "success":
					auditMutation(r)
					auditCommit(r)
					w.WriteHeader(204)
				case "rejected":
					w.WriteHeader(409)
				case "unknown":
					auditMutation(r)
					w.WriteHeader(503)
				default:
					w.WriteHeader(403)
				}
			})))
			request := httptest.NewRequest("DELETE", "/v1/instances/agent/connection-bindings/binding?private-query", strings.NewReader("private-body"))
			request.Header.Set("Authorization", "private-token")
			request.Header.Set("X-Correlation-ID", "audit-test")
			handler.ServeHTTP(httptest.NewRecorder(), request)
			for _, value := range []string{"private-query", "private-body", "private-token"} {
				if strings.Contains(logs.String(), value) {
					t.Fatal("audit leaked request content")
				}
			}
			entries := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(entries) != 2 {
				t.Fatal("audit missing lifecycle event")
			}
			var last map[string]any
			if err := json.Unmarshal([]byte(entries[1]), &last); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"success": "succeeded", "rejected": "rejected", "unknown": "execution_unknown", "unauthenticated": "rejected"}[variant]
			if last["outcome"] != want || last["correlation_id"] != "audit-test" || last["duration_ms"] == nil {
				t.Fatalf("audit outcome %v", last)
			}
			if variant == "unauthenticated" && last["owner_object_id"] != nil {
				t.Fatal("unverified owner audited as authenticated")
			}
		})
	}
}

func TestBindingDisconnectAuditMatchesStoredOutcome(t *testing.T) {
	for _, variant := range []string{"success", "conflict", "write-response-lost", "unauthenticated"} {
		t.Run(variant, func(t *testing.T) {
			f := newAuthorizationFixture(t, "github", "github")
			f.server.cfg.PortalNamespace, f.server.cfg.PortalServiceAccount = "portal", "portal"
			var logs bytes.Buffer
			f.server.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			original := f.server.store.client.Transport
			if variant == "write-response-lost" {
				f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					response, err := original.RoundTrip(r)
					if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, toolRecordName("binding", f.grant.ID)) {
						if response != nil {
							response.Body.Close()
						}
						return nil, errors.New("synthetic response lost after commit")
					}
					return response, err
				})
			}
			request := httptest.NewRequest("DELETE", "/v1/instances/agent/connection-bindings/"+f.grant.ID, nil)
			request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
			request.Header.Set("X-CetiRun-Owner-Tenant-ID", f.grant.Owner.TenantID)
			request.Header.Set("X-CetiRun-Owner-Object-ID", f.grant.Owner.ObjectID)
			request.Header.Set("X-CetiRun-Instance-UID", f.grant.InstanceUID)
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
			record, _, err := f.server.store.getBinding(context.Background(), f.grant.ID)
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
			if wantRevoked && (event["agent_uid"] != f.grant.InstanceUID || event["binding_id"] != f.grant.ID) {
				t.Fatal("audit lost mutation target")
			}
			if variant == "unauthenticated" && event["owner_object_id"] != nil {
				t.Fatal("unverified owner appeared in audit")
			}
		})
	}
}
