// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testTenantID      = "11111111-1111-1111-1111-111111111111"
	testBlueprintID   = "22222222-2222-2222-2222-222222222222"
	testFirstAgentID  = "33333333-3333-3333-3333-333333333333"
	testSecondAgentID = "44444444-4444-4444-4444-444444444444"
)

func agentOAuthTestClient(t *testing.T, transport http.RoundTripper) *oauthClient {
	t.Helper()
	assertionFile := filepath.Join(t.TempDir(), "federated-token")
	if err := os.WriteFile(assertionFile, []byte("federated-workload-assertion\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &oauthClient{
		cfg:    Config{TenantID: testTenantID, BlueprintClientID: testBlueprintID, FederatedTokenFile: assertionFile},
		client: &http.Client{Transport: transport},
	}
}

func agentOAuthForm(t *testing.T, request *http.Request) url.Values {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "login.microsoftonline.com" || request.URL.Path != "/"+testTenantID+"/oauth2/v2.0/token" {
		t.Fatalf("Agent token request = %s %s", request.Method, request.URL)
	}
	if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q", got)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	return form
}

func TestAgentOAuthUsesTwoStepAutonomousFlowWithOpaqueAssertions(t *testing.T) {
	requests := []url.Values{}
	client := agentOAuthTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, agentOAuthForm(t, request))
		switch len(requests) {
		case 1:
			return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "opaque-exchange-token", "token_type": "Bearer", "expires_in": 3600}), nil
		case 2:
			return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "opaque-agent-token", "token_type": "Bearer", "expires_in": 3600}), nil
		default:
			t.Fatalf("unexpected Agent token request %d", len(requests))
			return nil, nil
		}
	}))

	token, err := client.agentToken(context.Background(), testFirstAgentID)
	if err != nil {
		t.Fatalf("agentToken() error = %v", err)
	}
	if token.AccessToken != "opaque-agent-token" || token.ExpiresIn != 3600 || token.RefreshToken != "" || token.IDToken != "" {
		t.Fatalf("Agent token = %#v", token)
	}
	if len(requests) != 2 {
		t.Fatalf("token requests = %d, want 2", len(requests))
	}
	exchange, resource := requests[0], requests[1]
	if exchange.Get("client_id") != testBlueprintID || exchange.Get("grant_type") != "client_credentials" || exchange.Get("scope") != "api://AzureADTokenExchange/.default" || exchange.Get("fmi_path") != testFirstAgentID || exchange.Get("client_assertion_type") != tokenAssertionType || exchange.Get("client_assertion") != "federated-workload-assertion" {
		t.Fatalf("exchange form = %#v", exchange)
	}
	if resource.Get("client_id") != testFirstAgentID || resource.Get("grant_type") != "client_credentials" || resource.Get("scope") != "https://management.azure.com/.default" || resource.Get("fmi_path") != "" || resource.Get("client_assertion_type") != tokenAssertionType || resource.Get("client_assertion") != "opaque-exchange-token" {
		t.Fatalf("resource form = %#v", resource)
	}
	for _, form := range requests {
		if form.Get("grant_type") == "refresh_token" || form.Get("refresh_token") != "" {
			t.Fatalf("Agent OAuth attempted personal credential refresh: %#v", form)
		}
	}
}

func TestAgentOAuthKeepsChildAssertionsIsolated(t *testing.T) {
	var forms []url.Values
	client := agentOAuthTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		form := agentOAuthForm(t, request)
		forms = append(forms, form)
		if form.Get("scope") == "api://AzureADTokenExchange/.default" {
			return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "exchange-for-" + form.Get("fmi_path"), "token_type": "Bearer", "expires_in": 3600}), nil
		}
		return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "resource-for-" + form.Get("client_id"), "token_type": "Bearer", "expires_in": 3600}), nil
	}))

	for _, child := range []string{testFirstAgentID, testSecondAgentID} {
		token, err := client.agentToken(context.Background(), child)
		if err != nil || token.AccessToken != "resource-for-"+child {
			t.Fatalf("child %s token = %#v, %v", child, token, err)
		}
	}
	if len(forms) != 4 {
		t.Fatalf("forms = %d, want 4", len(forms))
	}
	for index, child := range []string{testFirstAgentID, testSecondAgentID} {
		exchange, resource := forms[index*2], forms[index*2+1]
		if exchange.Get("fmi_path") != child || resource.Get("client_id") != child || resource.Get("client_assertion") != "exchange-for-"+child {
			t.Fatalf("child %s exchanged forms = %#v / %#v", child, exchange, resource)
		}
		if strings.Contains(resource.Get("client_assertion"), testFirstAgentID) && child == testSecondAgentID {
			t.Fatalf("second child received first child assertion: %#v", resource)
		}
	}
}

func TestAgentOAuthRejectsRedirectsAndMalformedTokenResponses(t *testing.T) {
	tests := []struct {
		name     string
		response func(*http.Request) *http.Response
	}{
		{
			name: "redirect",
			response: func(*http.Request) *http.Response {
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.example/token"}}, Body: io.NopCloser(strings.NewReader(""))}
			},
		},
		{
			name: "non bearer token type",
			response: func(*http.Request) *http.Response {
				return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "token", "token_type": "MAC", "expires_in": 3600})
			},
		},
		{
			name: "too short expiry",
			response: func(*http.Request) *http.Response {
				return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "token", "token_type": "Bearer", "expires_in": 30})
			},
		},
		{
			name: "nonpositive expiry",
			response: func(*http.Request) *http.Response {
				return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "token", "token_type": "Bearer", "expires_in": 0})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := agentOAuthTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				_ = agentOAuthForm(t, request)
				return test.response(request), nil
			}))
			_, err := client.agentToken(context.Background(), testFirstAgentID)
			category, ok := oauthTokenFailureCategory(err)
			if !ok || category != string(oauthTokenInvalidResponse) && category != string(oauthTokenEndpointRejected) {
				t.Fatalf("failure = %q, %v", category, err)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
		})
	}
}
