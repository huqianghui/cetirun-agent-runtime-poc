// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeBrokerPodNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{strings.Repeat("a", 63) + "-0", true},
		{"approved.workload-0", true},
		{"a..b-0", false}, {"Uppercase-0", false}, {"a/b", false},
		{strings.Repeat("a", 254), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCredentialPolicyFixture(t)
			native := enableNativeProofFixture(t, f)
			native.SetPodName(tc.name)
			response := f.request(t, "/internal/v1/service-authorizations/list")
			if (response.Code == http.StatusOK) != tc.valid {
				t.Fatalf("Pod name acceptance: status=%d want valid=%t", response.Code, tc.valid)
			}
			if tc.valid && native.Calls == 0 || !tc.valid && native.Calls != 0 {
				t.Fatal("Pod name validation bypassed or incorrectly reached current authority")
			}
		})
	}
}

func TestNativeBrokerServiceAccountNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"approved.identity", true},
		{strings.Repeat("a", 64), true},
		{"approved." + strings.Repeat("a", 63) + ".identity", true},
		{strings.Repeat("a", 253), true},
		{"", false}, {"Uppercase", false}, {"a..b", false}, {"-a", false},
		{"a.-b", false}, {"a-.b", false}, {"a.", false}, {"a_b", false},
		{"a/b", false}, {"a:b", false}, {strings.Repeat("a", 254), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCredentialPolicyFixture(t)
			native := enableNativeProofFixture(t, f)
			native.SetServiceAccountName(tc.name)
			f.apply(t)
			token := f.request(t, "/internal/v1/service-authorizations/list")
			request := httptest.NewRequest(http.MethodPost, "/v1/instances/agent/tool-identities", strings.NewReader(`{"provider":"azure"}`))
			request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
			request.Header.Set("X-CetiRun-Owner-Tenant-ID", f.connection.Connection.Owner.TenantID)
			request.Header.Set("X-CetiRun-Owner-Object-ID", f.connection.Connection.Owner.ObjectID)
			request.Header.Set("X-CetiRun-Instance-UID", f.agent.Metadata.UID)
			owner := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(owner, request)
			if (token.Code == http.StatusOK) != tc.valid || (owner.Code == http.StatusOK) != tc.valid {
				t.Fatalf("ServiceAccount name acceptance: token=%d owner=%d want valid=%t", token.Code, owner.Code, tc.valid)
			}
			if !tc.valid && native.Calls != 0 {
				t.Fatal("invalid ServiceAccount name reached native admission")
			}
			if tc.valid {
				if native.Calls < 2 {
					t.Fatal("valid names bypassed current native proof")
				}
				for _, namespace := range []string{"agents.example", strings.Repeat("a", 64)} {
					if _, err := f.server.store.getAgentByWorkload(context.Background(), namespace, tc.name); err == nil {
						t.Fatal("ServiceAccount-name support relaxed namespace validation")
					}
				}
			}
		})
	}
}
