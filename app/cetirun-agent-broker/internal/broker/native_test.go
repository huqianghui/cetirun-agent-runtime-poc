// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/internal/agenttools"
	"github.com/Azure/CetiRun/app/internal/agenttools/testfixture"
)

func enableNativeProofFixture(t *testing.T, f *credentialPolicyFixture) *testfixture.Native {
	return enableNativeProofForServer(t, f.server, &f.agent)
}

func enableNativeProofForServer(t *testing.T, server *Server, agent *agentDocument) *testfixture.Native {
	t.Helper()
	native := testfixture.NewNative(t)
	delete(native.Objects, "/apis/cetirun.microsoft.com/v1alpha1/agents/agent")
	native.Objects["/apis/rbac.authorization.k8s.io/v1/namespaces/agents/rolebindings/cetirun-agent-tools-broker"] = map[string]any{
		"metadata": map[string]any{"name": "cetirun-agent-tools-broker", "namespace": "agents", "uid": "grant-uid",
			"ownerReferences": []ownerReference{agentRecordOwner(agentFromDocument(*agent))}},
		"roleRef": map[string]string{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "cetirun-agent-tools-broker"},
	}
	previous := server.store.client.Transport
	server.store.nativeOperatorNamespace = "operator"
	server.store.nativeAudience = server.cfg.WorkloadTokenAudience
	server.store.nativeAdmissionTransport = native.Transport
	server.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews" {
			return securityJSONResponse(t, 200, map[string]any{"status": map[string]any{
				"authenticated": true, "audiences": []string{server.cfg.WorkloadTokenAudience},
				"user": map[string]any{"username": "system:serviceaccount:agents:" + native.Identity.ServiceAccountName, "uid": native.Identity.ServiceAccountUID,
					"extra": map[string][]string{"authentication.kubernetes.io/pod-name": {native.Identity.PodName},
						"authentication.kubernetes.io/pod-uid": {native.Identity.PodUID}}},
			}}), nil
		}
		if response, ok := native.Response(r.URL.Path); ok {
			return response, nil
		}
		if r.URL.Path == "/apis/cetirun.microsoft.com/v1alpha1/agents/agent" {
			response, err := previous.RoundTrip(r)
			if err != nil {
				return response, err
			}
			var body map[string]any
			if json.NewDecoder(response.Body).Decode(&body) != nil {
				t.Fatal("invalid fixture Agent")
			}
			response.Body.Close()
			metadata := body["metadata"].(map[string]any)
			metadata["resourceVersion"] = "current-agent-version"
			// The basic connection fixture omits fields unused by its reader.
			// Native verification observes the API object's positive generation.
			if _, exists := metadata["generation"]; !exists {
				metadata["generation"] = 1
			}
			return securityJSONResponse(t, response.StatusCode, body), nil
		}
		return previous.RoundTrip(r)
	})
	return native
}

func TestNativeIndependentProvidersDoNotRequireBrokerAgentIdentity(t *testing.T) {
	for _, profile := range agenttools.Services() {
		if agenttools.UsesAgentIdentity(profile.Provider) {
			continue
		}
		t.Run(profile.ID, func(t *testing.T) {
			f := newAuthorizationFixture(t, profile.Provider, profile.ID)
			policy := json.RawMessage(`{"phase":"Applied"}`)
			f.agent.Status.Policy = &policy
			enableNativeProofForServer(t, f.server, &f.agent)
			if f.agent.Spec.AgentIdentity != nil {
				t.Fatal("independent fixture unexpectedly provisions an Entra Agent Identity")
			}
			for _, path := range []string{"/internal/v1/service-authorizations/list", "/internal/v1/service-authorizations/check", "/internal/v1/service-credentials/issue"} {
				response := f.request(t, path)
				if response.Code != 200 {
					t.Fatalf("independent native provider denied: %s %d %s", path, response.Code, response.Body)
				}
			}
		})
	}
}

func TestNativeBrokerChecksPolicyAndIssuesOnlyAfterCurrentSourceValidation(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	native := enableNativeProofFixture(t, f)
	f.apply(t)
	exchanges := 0
	f.server.oauth = agentOAuthTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		exchanges++
		form := agentOAuthForm(t, r)
		if form.Get("grant_type") != "client_credentials" {
			t.Fatal("native identity used a delegated credential unexpectedly")
		}
		return securityJSONResponse(t, 200, map[string]any{"access_token": "synthetic-native-token", "token_type": "Bearer", "expires_in": 3600}), nil
	}))
	response := f.request(t, "/internal/v1/service-authorizations/check")
	var checked agenttools.CheckResponse
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &checked) != nil || !checked.CredentialPolicy.Valid() {
		t.Fatalf("native authorization did not reach current policy: %d %s", response.Code, response.Body)
	}
	response = f.request(t, "/internal/v1/service-credentials/issue", func(request *serviceRequest) {
		request.CredentialPolicy = checked.CredentialPolicy
	})
	if response.Code != 200 || exchanges != 2 || native.Calls < 3 || !strings.Contains(response.Body.String(), "synthetic-native-token") {
		t.Fatalf("native issuance: %d exchanges=%d admission=%d %s", response.Code, exchanges, native.Calls, response.Body)
	}
	native.Allowed = false
	response = f.request(t, "/internal/v1/service-credentials/issue", func(request *serviceRequest) {
		request.CredentialPolicy = checked.CredentialPolicy
	})
	if response.Code == 200 || exchanges != 2 || strings.Contains(response.Body.String(), "synthetic-native-token") {
		t.Fatal("revoked common-executor authority retained native credential access")
	}
}

func TestNativeBrokerOwnerConnectionCreationUsesCurrentPodAuthority(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	native := enableNativeProofFixture(t, f)
	f.apply(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/instances/agent/tool-identities", bytes.NewBufferString(`{"provider":"azure"}`))
	request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
	request.Header.Set("X-CetiRun-Owner-Tenant-ID", f.connection.Connection.Owner.TenantID)
	request.Header.Set("X-CetiRun-Owner-Object-ID", f.connection.Connection.Owner.ObjectID)
	request.Header.Set("X-CetiRun-Instance-UID", f.agent.Metadata.UID)
	response := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(response, request)
	if response.Code != 200 || native.Calls == 0 || strings.Contains(response.Body.String(), "accessToken") {
		t.Fatalf("native owner association = %d %s", response.Code, response.Body)
	}
	var connection agenttools.Connection
	if json.Unmarshal(response.Body.Bytes(), &connection) != nil {
		t.Fatal("invalid native connection response")
	}
	data, _ := json.Marshal(bindingRequest{ConnectionID: connection.ID, Services: []string{"azure_arm"}, Enabled: true})
	bind := httptest.NewRequest(http.MethodPost, "/v1/instances/agent/connection-bindings", bytes.NewReader(data))
	bind.Header = request.Header.Clone()
	bound := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(bound, bind)
	if bound.Code != 200 {
		t.Fatalf("native binding creation = %d %s", bound.Code, bound.Body)
	}
	other := httptest.NewRequest(http.MethodPost, "/v1/instances/agent/connection-bindings", bytes.NewReader(data))
	other.Header = request.Header.Clone()
	other.Header.Set("X-CetiRun-Owner-Object-ID", "99999999-9999-9999-9999-999999999999")
	denied := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(denied, other)
	if denied.Code != http.StatusForbidden {
		t.Fatal("another owner could bind the native identity")
	}
}

func TestNativeBrokerCleanupDoesNotRequireAStillRunningPod(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	native := enableNativeProofFixture(t, f)
	f.apply(t)
	native.Allowed = false
	request := httptest.NewRequest(http.MethodDelete, "/operator/v1/instances/"+f.agent.Metadata.UID+"/authorization", nil)
	request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/operator/sa/operator")
	response := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || len(f.secrets) != 0 || len(f.policySecrets) != 0 || native.Calls != 0 {
		t.Fatalf("native cleanup depended on active proof: %d records=%d policy=%d calls=%d",
			response.Code, len(f.secrets), len(f.policySecrets), native.Calls)
	}
}

func TestNativeBrokerPolicyReadbackCanPrecedeAggregateReady(t *testing.T) {
	f := newCredentialPolicyFixture(t)
	enableNativeProofFixture(t, f)
	f.agent.Status.Phase = "Pending"
	desired := f.apply(t)
	response := f.operator(t, http.MethodGet, nil, true)
	var readback agenttools.CredentialPolicyReadback
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &readback) != nil || !readback.Matches(desired) {
		t.Fatalf("native credential readback cycled on aggregate Ready: %d %s", response.Code, response.Body)
	}
	// Serving credentials remains gated by a Ready Agent, unlike configuration readback.
	if response := f.request(t, "/internal/v1/service-credentials/issue"); response.Code == 200 {
		t.Fatal("pre-Ready Agent received a provider credential")
	}
}
