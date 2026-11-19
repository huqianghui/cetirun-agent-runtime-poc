// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type credentialPolicyFixture struct {
	*connectionFixture
	generation         int64
	identityGeneration int64
	teamUID            string
	policyUID          string
	teamName           string
	policyName         string
	accepted           bool
	controls           agenttools.CredentialPolicyIdentity
	policySecrets      map[string]secretDocument
}

func newCredentialPolicyFixture(t *testing.T) *credentialPolicyFixture {
	t.Helper()
	f := &credentialPolicyFixture{
		connectionFixture: newConnectionFixture(t), generation: 7, identityGeneration: 7,
		teamUID: "team-uid", policyUID: "policy-uid", teamName: "example-team", policyName: "example-policy",
		accepted: true, policySecrets: map[string]secretDocument{},
		controls: agenttools.CredentialPolicyIdentity{Allowed: true, CredentialServices: []string{agenttools.CredentialServiceARM}},
	}
	policy := json.RawMessage(`{"enforcement":{"credential":{"desiredRevision":"not-evidence"}}}`)
	f.agent.Status.Policy = &policy
	transport := f.server.store.client.Transport
	f.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasPrefix(request.URL.Path, "/api/v1/namespaces/credentials/secrets") {
			return f.recordRequest(t, request), nil
		}
		switch request.URL.Path {
		case "/apis/cetirun.microsoft.com/v1alpha1/agents/agent":
			raw, _ := json.Marshal(f.agent)
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			document["metadata"].(map[string]any)["generation"] = f.generation
			document["metadata"].(map[string]any)["resourceVersion"] = "current-agent-version"
			document["spec"].(map[string]any)["team"] = f.teamName
			status := document["status"].(map[string]any)
			if identity, ok := status["agentIdentity"].(map[string]any); ok {
				identity["observedGeneration"] = f.identityGeneration
			}
			return securityJSONResponse(t, 200, document), nil
		case "/apis/cetirun.microsoft.com/v1alpha1/teams/" + f.teamName:
			return securityJSONResponse(t, 200, map[string]any{
				"metadata": credentialObjectMeta{Name: f.teamName, UID: f.teamUID, Generation: 2},
				"spec":     map[string]any{"agentPolicyRef": f.policyName},
				"status":   map[string]any{"observedGeneration": 2, "conditions": []credentialCondition{{Type: "Accepted", Status: "True", ObservedGeneration: 2}}},
			}), nil
		case "/apis/cetirun.microsoft.com/v1alpha1/agentpolicies/" + f.policyName:
			accepted := "True"
			if !f.accepted {
				accepted = "False"
			}
			return securityJSONResponse(t, 200, map[string]any{
				"metadata": credentialObjectMeta{Name: f.policyName, UID: f.policyUID, Generation: 3},
				"spec":     map[string]any{"agentIdentity": f.controls},
				"status":   map[string]any{"observedGeneration": 3, "conditions": []credentialCondition{{Type: "Accepted", Status: accepted, ObservedGeneration: 3}}},
			}), nil
		}
		return transport.RoundTrip(request)
	})
	return f
}

func (f *credentialPolicyFixture) recordRequest(t *testing.T, r *http.Request) *http.Response {
	t.Helper()
	prefix := "/api/v1/namespaces/credentials/secrets"
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		var record secretDocument
		if json.NewDecoder(r.Body).Decode(&record) != nil || record.Metadata.Namespace != "credentials" ||
			record.Metadata.Labels[recordLabelPrefix+"record-type"] != credentialPolicyRecord {
			t.Fatal("unexpected credential policy record write")
		}
		previous, exists := f.policySecrets[record.Metadata.Name]
		if r.Method == http.MethodPost && exists ||
			r.Method == http.MethodPut && (!exists || record.Metadata.ResourceVersion != previous.Metadata.ResourceVersion) {
			return securityJSONResponse(t, 409, map[string]any{})
		}
		f.version++
		record.Metadata.ResourceVersion = strconv.FormatUint(f.version, 10)
		record.Metadata.UID = "credential-policy-record-uid"
		f.policySecrets[record.Metadata.Name] = record
		return securityJSONResponse(t, 200, record)
	}
	name := strings.TrimPrefix(r.URL.Path, prefix+"/")
	record, exists := f.policySecrets[name]
	if !exists {
		return securityJSONResponse(t, 404, map[string]any{})
	}
	if r.Method == http.MethodDelete {
		var options struct {
			Preconditions map[string]string `json:"preconditions"`
		}
		if json.NewDecoder(r.Body).Decode(&options) != nil || options.Preconditions["uid"] != record.Metadata.UID ||
			options.Preconditions["resourceVersion"] != record.Metadata.ResourceVersion {
			return securityJSONResponse(t, 409, map[string]any{})
		}
		delete(f.policySecrets, name)
	}
	return securityJSONResponse(t, 200, record)
}

func (f *credentialPolicyFixture) desired(t *testing.T) agenttools.CredentialPolicy {
	t.Helper()
	p, err := f.server.currentCredentialPolicy(context.Background(), agentFromDocument(f.agent))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *credentialPolicyFixture) operator(t *testing.T, method string, input any, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest(method, "/operator/v1/instances/"+f.agent.Metadata.UID+"/credential-policy", bytes.NewReader(raw))
	if authenticated {
		r.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/operator/sa/operator")
	}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	return w
}

func (f *credentialPolicyFixture) apply(t *testing.T) agenttools.CredentialPolicy {
	t.Helper()
	desired := f.desired(t)
	if response := f.operator(t, http.MethodPut, desired, true); response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("application acknowledgment = %d %s", response.Code, response.Body.String())
	}
	return desired
}

func TestCredentialPolicyApplicationPersistsWithoutCertifyingNativeExecution(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	// Policy preparation cannot require a running workload: that would cycle
	// through aggregate PolicyApplied. The canonical identity must already exist.
	f.agent.Status.Phase = "Pending"
	if got := f.operator(t, http.MethodGet, nil, true); got.Code != 404 {
		t.Fatalf("desired-only state returned %d", got.Code)
	}
	before := len(f.secrets)
	desired := f.apply(t)
	if len(f.secrets) != before || len(f.policySecrets) != 1 {
		t.Fatal("policy application unexpectedly changed Connection/Binding records")
	}
	version := f.version
	f.apply(t)
	if f.version != version {
		t.Fatal("idempotent retry rewrote an unchanged enforcement record")
	}
	// Reconstruct the serving process; the evidence must survive a restart.
	old := f.server
	f.server = &Server{cfg: old.cfg, store: old.store, mux: http.NewServeMux(), tlsMux: http.NewServeMux()}
	f.server.routes()
	response := f.operator(t, http.MethodGet, nil, true)
	if response.Code != 503 {
		t.Fatalf("unsupported native executor produced ready readback: %d", response.Code)
	}
	agent := agentFromDocument(f.agent)
	store, err := f.server.store.credentialPolicyStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	readback, err := store.readCredentialPolicy(context.Background(), agent)
	if err != nil || !readback.Matches(desired) {
		t.Fatalf("persisted enforcement record = %#v, %v", readback, err)
	}
	if strings.Contains(response.Body.String(), "accessToken") || strings.Contains(response.Body.String(), "callerProof") {
		t.Fatal("readback exposed credential material")
	}
}

func TestCredentialPolicyLostWriteReplyIsRecoveredByRead(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed write", true: "lost reply"}[accepted], func(t *testing.T) {
			f := newCredentialPolicyFixture(t)
			desired := f.desired(t)
			transport := f.server.store.client.Transport
			writes := 0
			f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/secrets") {
					writes++
					if writes == 1 {
						if accepted {
							response, err := transport.RoundTrip(r)
							if err != nil {
								return response, err
							}
							response.Body.Close()
						}
						return nil, errors.New("synthetic write outcome unknown")
					}
				}
				return transport.RoundTrip(r)
			})
			if response := f.operator(t, http.MethodPut, desired, true); response.Code != 503 {
				t.Fatalf("uncertain write returned %d", response.Code)
			}
			response := f.operator(t, http.MethodGet, nil, true)
			if accepted && response.Code != 503 || !accepted && response.Code != 404 {
				t.Fatalf("read after uncertain write returned %d", response.Code)
			}
			f.apply(t)
			want := 2
			if accepted {
				want = 1
			}
			if writes != want {
				t.Fatalf("write count = %d, want %d", writes, want)
			}
		})
	}
}

func TestCredentialPolicyCurrentSourcesAndOwnedRecordAreMandatory(t *testing.T) {
	for name, mutate := range map[string]func(*credentialPolicyFixture){
		"owner": func(f *credentialPolicyFixture) {
			f.agent.Metadata.Annotations["agentidentity.cetirun.microsoft.com/owner-object-id"] = "99999999-9999-9999-9999-999999999999"
		},
		"client": func(f *credentialPolicyFixture) {
			f.agent.Status.AgentIdentity.ClientID = "99999999-9999-9999-9999-999999999999"
		},
		"principal": func(f *credentialPolicyFixture) {
			f.agent.Status.AgentIdentity.ObjectID = "99999999-9999-9999-9999-999999999999"
		},
		"generation":  func(f *credentialPolicyFixture) { f.generation++ },
		"team":        func(f *credentialPolicyFixture) { f.teamUID = "replacement-team" },
		"policy":      func(f *credentialPolicyFixture) { f.policyUID = "replacement-policy" },
		"acceptance":  func(f *credentialPolicyFixture) { f.accepted = false },
		"disabled":    func(f *credentialPolicyFixture) { f.agent.Spec.AgentIdentity.Enabled = false },
		"no identity": func(f *credentialPolicyFixture) { f.agent.Status.AgentIdentity = nil },
		"failed":      func(f *credentialPolicyFixture) { f.agent.Status.AgentIdentity.Phase = "Failed" },
		"suspended":   func(f *credentialPolicyFixture) { f.agent.Spec.Suspended = true },
		"denied":      func(f *credentialPolicyFixture) { f.controls.Allowed = false },
		"service":     func(f *credentialPolicyFixture) { f.controls.CredentialServices = []string{"microsoft_graph"} },
		"required":    func(f *credentialPolicyFixture) { f.controls.Required = true },
		"deployment":  func(f *credentialPolicyFixture) { f.server.cfg.ToolRuntimeTypes = []string{"hermes"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newCredentialPolicyFixture(t)
			desired := f.apply(t)
			mutate(f)
			if response := f.operator(t, http.MethodGet, nil, true); response.Code == 200 {
				t.Fatal("stale credential evidence remained ready")
			}
			if response := f.operator(t, http.MethodPut, desired, true); response.Code == 204 {
				t.Fatal("an old desired input was reapplied")
			}
		})
	}
}

func TestCredentialPolicyReadbackRechecksAfterRecordRead(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	f.apply(t)
	transport := f.server.store.client.Transport
	f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/secrets/agent-tools-credential-policy-") {
			f.controls.Required = true
		}
		return response, err
	})
	if response := f.operator(t, http.MethodGet, nil, true); response.Code != 409 {
		t.Fatalf("post-read source race returned %d", response.Code)
	}
}

func TestCredentialPolicyRecordCannotBeAdoptedOrDeletedAcrossOwners(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	desired := f.apply(t)
	name := toolRecordName(credentialPolicyRecord, f.agent.Metadata.UID)
	secret := f.policySecrets[name]
	secret.Metadata.OwnerReferences[0].UID = "99999999-9999-9999-9999-999999999999"
	f.policySecrets[name] = secret
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if response := f.operator(t, method, desired, true); response.Code != 503 {
			t.Fatalf("foreign record %s returned %d", method, response.Code)
		}
	}
	if _, exists := f.policySecrets[name]; !exists {
		t.Fatal("foreign record was deleted")
	}
}

func TestCredentialPolicyDeletionIsOwnedRetryableAndObserved(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	f.apply(t)
	transport := f.server.store.client.Transport
	deletes := 0
	f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/secrets/agent-tools-credential-policy-") {
			deletes++
			response.Body.Close()
			return nil, errors.New("synthetic lost deletion reply")
		}
		return response, err
	})
	if got := f.operator(t, http.MethodDelete, nil, true); got.Code != 503 {
		t.Fatalf("unknown cleanup returned %d", got.Code)
	}
	if got := f.operator(t, http.MethodDelete, nil, true); got.Code != 204 || deletes != 1 {
		t.Fatalf("cleanup retry = %d, deletes %d", got.Code, deletes)
	}
	if got := f.operator(t, http.MethodGet, nil, true); got.Code != 404 {
		t.Fatalf("removed record returned %d", got.Code)
	}
	if len(f.secrets) != 2 {
		t.Fatal("policy cleanup removed unrelated Connection or Binding")
	}
}

func TestCredentialPolicyUsesExistingAuthorizationAndKeepsNativeBoundary(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	if response := f.request(t, "/internal/v1/service-authorizations/check"); response.Code != 401 {
		t.Fatalf("unqualified native workload reached authorization: %d", response.Code)
	}
	f.apply(t)
	identity := workloadIdentity{Agent: agentFromDocument(f.agent)}
	if err := f.server.authorizeCredentialPolicy(context.Background(), &identity, "azure_arm", "azure"); err == nil ||
		identity.CredentialPolicy != nil {
		t.Fatal("installed restriction was confused with supported native execution")
	}
	if err := f.server.authorizeCredentialPolicy(context.Background(), &identity, "microsoft_graph", "microsoft"); err == nil {
		t.Fatal("fixed ARM policy authorized Microsoft Graph")
	}
	for _, path := range []string{"/internal/v1/service-authorizations/check", "/internal/v1/service-credentials/issue"} {
		if response := f.request(t, path); response.Code == 200 {
			t.Fatal("credential policy silently enabled an unsupported native workload")
		}
	}
	enableNativeProofFixture(t, f)
	f.controls.Required = true
	if response := f.request(t, "/internal/v1/service-authorizations/check"); response.Code != 409 {
		t.Fatalf("stale policy reached authorization: %d", response.Code)
	}
}

func TestCredentialPolicyOperatorAuthorizationAndDependencyFailure(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	enableNativeProofFixture(t, f)
	desired := f.desired(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if response := f.operator(t, method, desired, false); response.Code != 403 {
			t.Fatalf("untrusted operator %s returned %d", method, response.Code)
		}

	}
	f.apply(t)
	transport := f.server.store.client.Transport
	f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/agentpolicies/") {
			return securityJSONResponse(t, 503, map[string]any{}), nil
		}
		return transport.RoundTrip(r)
	})
	if response := f.operator(t, http.MethodGet, nil, true); response.Code != 503 {
		t.Fatalf("unreadable policy returned %d", response.Code)
	}
	if response := f.request(t, "/internal/v1/service-credentials/issue"); response.Code != 503 {
		t.Fatalf("unreadable policy reached issuance: %d", response.Code)
	}
}

func TestCredentialPolicyStorageAndCleanupNeedNoWorkloadSecretPermission(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	transport := f.server.store.client.Transport
	f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/agents") {
			t.Fatal("credential policy acquired a workload namespace dependency")
		}
		return transport.RoundTrip(r)
	})
	f.apply(t)
	f.agent.Metadata.Annotations["agentidentity.cetirun.microsoft.com/owner-object-id"] = "99999999-9999-9999-9999-999999999999"
	if response := f.operator(t, http.MethodGet, nil, true); response.Code == 200 {
		t.Fatal("new human owner adopted old policy evidence")
	}
	// This record contains no provider credential. The same immutable Agent
	// owner reference authorizes cleanup even after human-owner drift.
	if response := f.operator(t, http.MethodDelete, nil, true); response.Code != 204 || len(f.policySecrets) != 0 {
		t.Fatalf("exact Agent cleanup after owner drift = %d", response.Code)
	}
	if len(f.secrets) != 2 {
		t.Fatal("credential policy cleanup affected Connection/Binding records")
	}
}

func TestCredentialPolicyReadbackMatcherAfterSupportedRuntimePrerequisite(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	desired := f.apply(t)
	agent := agentFromDocument(f.agent)
	enableNativeProofFixture(t, f)
	readback, err := f.server.credentialPolicyReadback(context.Background(), agent)
	if err != nil || !readback.Matches(desired) {
		t.Fatalf("supported-executor boundary failed current readback: %v", err)
	}
	identity := workloadIdentity{Agent: agent}
	if err := f.server.authorizeCredentialPolicy(context.Background(), &identity, "azure_arm", "azure"); err != nil ||
		!identity.CredentialPolicy.Valid() || identity.CredentialPolicy.Revision != desired.Revision() {
		t.Fatalf("ARM projection did not reach the authorization boundary: %v", err)
	}
	if err := f.server.authorizeCredentialPolicy(context.Background(), &identity, "microsoft_graph", "microsoft"); err == nil {
		t.Fatal("ARM projection expanded the supported executor to Graph")
	}
}

func TestCredentialPolicyUsesKubernetesConfigurationNames(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	f.teamName = strings.Repeat("t", 70) + ".example"
	f.policyName = "example.policy-v1"
	f.apply(t)
	for _, name := range []string{"", "../other", "other/policy", "Uppercase", strings.Repeat("x", 254)} {
		if validCredentialConfigurationName(name) {
			t.Fatalf("invalid configuration name accepted: %q", name)
		}
	}
}

func TestCredentialPolicyUsesAvailableARMIdentityModes(t *testing.T) {
	for _, test := range []struct {
		name             string
		disableAgent     bool
		disableDelegated bool
		available        bool
	}{
		{name: "both", available: true},
		{name: "app only", disableDelegated: true, available: true},
		{name: "delegated only", disableAgent: true, available: true},
		{name: "disabled", disableAgent: true, disableDelegated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCredentialPolicyFixture(t)
			f.server.cfg.DisableAzureAgent = test.disableAgent
			f.server.cfg.DisableMicrosoftDelegated = test.disableDelegated
			_, err := f.server.currentCredentialPolicy(context.Background(), agentFromDocument(f.agent))
			if (err == nil) != test.available {
				t.Fatalf("ARM adapter availability = %v, want %v", err == nil, test.available)
			}
		})
	}
}

func TestCredentialPolicyDoesNotGateMetadataListing(t *testing.T) {
	for _, state := range []string{"missing", "revoked", "stale", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			f := newCredentialPolicyFixture(t)
			enableNativeProofFixture(t, f)
			expected := http.StatusForbidden
			switch state {
			case "revoked":
				f.apply(t)
				if response := f.operator(t, http.MethodDelete, nil, true); response.Code != http.StatusNoContent {
					t.Fatalf("policy removal = %d", response.Code)
				}
			case "stale":
				f.apply(t)
				name := toolRecordName(credentialPolicyRecord, f.agent.Metadata.UID)
				record := f.policySecrets[name]
				raw, _ := base64.StdEncoding.DecodeString(record.Data["policy.json"])
				var previous agenttools.CredentialPolicy
				if json.Unmarshal(raw, &previous) != nil {
					t.Fatal("invalid fixture policy")
				}
				previous.Identity.Required = !previous.Identity.Required
				raw, _ = json.Marshal(previous)
				record.Data["policy.json"] = base64.StdEncoding.EncodeToString(raw)
				f.policySecrets[name] = record
				expected = http.StatusConflict
			case "unavailable":
				expected = http.StatusServiceUnavailable
			}
			agent := agentFromDocument(f.agent)
			if !agent.PolicyManaged || !agent.UnsupportedRuntime || agent.AgentIdentityClientID == "" {
				t.Fatal("fixture must preserve the current unsupported-native boundary")
			}
			transport := f.server.store.client.Transport
			policyReads := 0
			f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/teams/") || strings.Contains(r.URL.Path, "/agentpolicies/") ||
					strings.Contains(r.URL.Path, "/secrets/agent-tools-credential-policy-") {
					policyReads++
					if state == "unavailable" && strings.Contains(r.URL.Path, "/secrets/agent-tools-credential-policy-") {
						return securityJSONResponse(t, http.StatusServiceUnavailable, map[string]any{}), nil
					}
				}
				return transport.RoundTrip(r)
			})
			response := f.request(t, "/internal/v1/service-authorizations/list")
			var metadata agenttools.ListResponse
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &metadata) != nil || policyReads != 0 {
				t.Fatalf("metadata listing depended on ARM policy: status=%d reads=%d", response.Code, policyReads)
			}
			// The live native source check qualifies the current Pod without
			// borrowing ARM policy evidence for metadata discovery.
			if len(metadata.Connections) != 1 || len(metadata.Bindings) != 1 {
				t.Fatal("verified native metadata listing lost the owner binding")
			}
			for _, route := range []string{"/internal/v1/service-authorizations/check", "/internal/v1/service-credentials/issue"} {
				before := policyReads
				response := f.request(t, route)
				if response.Code != expected || policyReads <= before || strings.Contains(response.Body.String(), "accessToken") {
					t.Fatalf("ARM credential gate changed: %s status=%d reads=%d", route, response.Code, policyReads)
				}
			}
		})
	}
}

func TestCredentialPolicyLeavesIndependentConnectionMetadataListable(t *testing.T) {
	for _, profile := range agenttools.Services() {
		if agenttools.UsesAgentIdentity(profile.Provider) {
			continue
		}
		t.Run(profile.ID, func(t *testing.T) {
			f := newAuthorizationFixture(t, profile.Provider, profile.ID)
			f.agent.Spec.AgentIdentity = &struct {
				Enabled bool `json:"enabled"`
			}{Enabled: true}
			f.agent.Status.AgentIdentity = &struct {
				Phase    string `json:"phase"`
				ClientID string `json:"clientId"`
				ObjectID string `json:"objectId"`
			}{Phase: "Degraded", ClientID: testFirstAgentID, ObjectID: testAgentPrincipalID}
			transport := f.server.store.client.Transport
			f.server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/agentpolicies/") ||
					strings.Contains(r.URL.Path, "/secrets/agent-tools-credential-policy-") {
					t.Fatal("independent connection metadata required ARM evidence")
				}
				return transport.RoundTrip(r)
			})
			response := f.request(t, "/internal/v1/service-authorizations/list")
			var metadata agenttools.ListResponse
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &metadata) != nil ||
				len(metadata.Connections) != 1 || metadata.Connections[0].Provider != profile.Provider ||
				len(metadata.Bindings) != 1 || metadata.Bindings[0].ConnectionID != f.connection.Connection.ID {
				t.Fatalf("independent metadata was not listable: %d %s", response.Code, response.Body)
			}
		})
	}
}
