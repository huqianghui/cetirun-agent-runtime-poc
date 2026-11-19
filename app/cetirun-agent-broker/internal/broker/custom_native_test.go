// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func TestCustomConnectionsRequireExplicitDeploymentAndCurrentNativeProof(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-enabled", true: "enabled"}[allowed], func(t *testing.T) {
			f := newCredentialPolicyFixture(t)
			f.agent.Spec.Agent.Type = "custom"
			if allowed {
				f.server.cfg.ToolRuntimeTypes = []string{"custom"}
			}
			native := enableNativeProofFixture(t, f)
			if !allowed {
				response := f.operator(t, http.MethodGet, nil, true)
				if response.Code == http.StatusOK {
					t.Fatal("Custom inherited the default managed runtime allowlist")
				}
				return
			}
			f.apply(t)
			exchanges := 0
			f.server.oauth = agentOAuthTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				exchanges++
				return securityJSONResponse(t, http.StatusOK, map[string]any{
					"access_token": "synthetic-custom-provider-token", "token_type": "Bearer", "expires_in": 3600,
				}), nil
			}))
			response := f.request(t, "/internal/v1/service-authorizations/check")
			var checked agenttools.CheckResponse
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &checked) != nil || !checked.CredentialPolicy.Valid() {
				t.Fatalf("Custom did not obtain current credential authorization: %d %s", response.Code, response.Body)
			}
			response = f.request(t, "/internal/v1/service-credentials/issue", func(request *serviceRequest) {
				request.CredentialPolicy = checked.CredentialPolicy
			})
			if response.Code != http.StatusOK || exchanges != 2 || native.Calls == 0 {
				t.Fatalf("Custom internal issuance did not validate live authority: %d %s", response.Code, response.Body)
			}
			native.Allowed = false
			response = f.request(t, "/internal/v1/service-credentials/issue", func(request *serviceRequest) {
				request.CredentialPolicy = checked.CredentialPolicy
			})
			if response.Code == http.StatusOK || exchanges != 2 || strings.Contains(response.Body.String(), "synthetic-custom-provider-token") {
				t.Fatal("revoked Custom authority retained credential access")
			}
		})
	}
}

func TestCustomPersonalConnectionsDoNotRequireAgentIdentity(t *testing.T) {
	for _, service := range agenttools.Services() {
		if agenttools.UsesAgentIdentity(service.Provider) {
			continue
		}
		t.Run(service.ID, func(t *testing.T) {
			f := newAuthorizationFixture(t, service.Provider, service.ID)
			f.agent.Spec.Agent.Type = "custom"
			policy := json.RawMessage(`{"phase":"Applied"}`)
			f.agent.Status.Policy = &policy
			f.server.cfg.ToolRuntimeTypes = []string{"custom"}
			enableNativeProofForServer(t, f.server, &f.agent)
			if f.agent.Spec.AgentIdentity != nil {
				t.Fatal("personal provider unexpectedly required Entra Agent Identity")
			}
			for _, path := range []string{"/internal/v1/service-authorizations/list", "/internal/v1/service-authorizations/check", "/internal/v1/service-credentials/issue"} {
				if response := f.request(t, path); response.Code != http.StatusOK {
					t.Fatalf("Custom personal provider request failed: %s %d %s", path, response.Code, response.Body)
				}
			}
		})
	}
}
