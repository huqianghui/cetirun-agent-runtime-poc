// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGitHubOAuthAuthorizationURLUsesPersonalOAuthAppAndRepoScope(t *testing.T) {
	provider := newProviderClient(Config{
		GitHubOAuthClientID:     "github-client-id",
		GitHubOAuthClientSecret: "github-client-secret",
		RedirectURI:             "https://portal.example.com/api/v1/connections/callback",
	})
	location, err := provider.authorizationURL("github", strings.Repeat("a", 43), strings.Repeat("b", 43), "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.Path != "/login/oauth/authorize" || query.Get("client_id") != "github-client-id" || query.Get("redirect_uri") != provider.cfg.RedirectURI || query.Get("scope") != "repo" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		t.Fatalf("GitHub OAuth authorization URL = %q", location)
	}
}

func TestGitHubOAuthCallbackStoresExplicitNonExpiringCredential(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	fixture.server.cfg.PortalNamespace, fixture.server.cfg.PortalServiceAccount = "portal", "portal"
	state := strings.Repeat("a", 43)
	browserBinding := strings.Repeat("c", 43)
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Status, record.Connection.AccountID, record.Credential = "interaction_required", "42", nil
	tx := &connectionTransaction{TargetConnectionID: record.Connection.ID, ExpectedAccountID: "42", Provider: "github", StateHash: stateHash(state), BrowserBindingHash: stateHash(browserBinding), CodeVerifier: strings.Repeat("b", 43), ExpiresAt: time.Now().Add(time.Hour)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	stageOAuthTransaction(t, fixture, tx)
	fixture.server.providers = &providerClient{cfg: Config{GitHubOAuthClientID: "github-client-id", GitHubOAuthClientSecret: "github-client-secret"}, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case "https://github.com/login/oauth/access_token":
			if request.FormValue("client_id") != "github-client-id" || request.FormValue("client_secret") != "github-client-secret" || request.FormValue("code_verifier") == "" {
				t.Fatalf("unexpected GitHub OAuth exchange request")
			}
			return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "synthetic-github-token", "scope": "repo, gist"}), nil
		case "https://api.github.com/user":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"id": 42, "login": "octocat"}), nil
		default:
			t.Fatalf("unexpected request: %s", request.URL)
			return nil, nil
		}
	})}}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/connections/callback?state="+state+"&code=synthetic-code", nil)
	request.Header.Set(connectionBindingHeader, browserBinding)
	request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
	fixture.server.Handler().ServeHTTP(response, request)
	current, _, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Sign-in complete") || current.Credential == nil {
		t.Fatalf("GitHub OAuth callback failed: status=%d body=%q record=%#v", response.Code, response.Body.String(), current)
	}
	if !current.Credential.NonExpiring || !current.Credential.ExpiresAt.IsZero() || current.Credential.RefreshToken != "" || !contains(current.Credential.Scopes, "repo") || len(current.Connection.Services) != len(agenttools.ProviderServices("github")) || current.Connection.AccountID != "42" || current.Connection.AccountDisplay != "octocat" {
		t.Fatalf("GitHub OAuth callback persisted invalid metadata: nonExpiring=%t expiresAt=%s hasRefresh=%t scopes=%q capabilities=%q account=%q display=%q", current.Credential.NonExpiring, current.Credential.ExpiresAt, current.Credential.RefreshToken != "", current.Credential.Scopes, current.Connection.Services, current.Connection.AccountID, current.Connection.AccountDisplay)
	}
}

func TestPersonalOAuthStartCallbackAndReconnectRequireBrowserBinding(t *testing.T) {
	for _, provider := range []string{"github", "google"} {
		t.Run(provider, func(t *testing.T) { testPersonalOAuthStartCallbackAndReconnectRequireBrowserBinding(t, provider) })
	}
}

func testPersonalOAuthStartCallbackAndReconnectRequireBrowserBinding(t *testing.T, provider string) {
	t.Helper()
	fixture := personalOAuthFixture(t, provider)
	fixture.server.cfg.PortalNamespace, fixture.server.cfg.PortalServiceAccount = "portal", "portal"
	fixture.server.cfg.GitHubOAuthClientID, fixture.server.cfg.GitHubOAuthClientSecret = "github-client-id", "github-client-secret"
	fixture.server.cfg.GoogleClientID, fixture.server.cfg.GoogleClientSecret = "google-client-id", "google-client-secret"
	fixture.server.cfg.TransactionTTL = 10 * time.Minute
	firstBinding := strings.Repeat("a", 43)
	start := httptest.NewRequest(http.MethodPost, "/v1/connections/start", strings.NewReader(`{"provider":"`+provider+`"}`))
	setPortalRequestHeaders(start, fixture.grant.Owner, firstBinding)
	started := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(started, start)
	var startResponse struct {
		ID                     string `json:"authorizationId"`
		AuthorizationURL       string `json:"authorizationUrl"`
		BrowserBindingRequired bool   `json:"browserBindingRequired"`
	}
	if started.Code != http.StatusOK || json.NewDecoder(started.Body).Decode(&startResponse) != nil || startResponse.ID == "" || !startResponse.BrowserBindingRequired {
		t.Fatalf("OAuth start response = %d %s", started.Code, started.Body)
	}
	authorizationURL, err := url.Parse(startResponse.AuthorizationURL)
	if err != nil || authorizationURL.Query().Get("state") == "" {
		t.Fatalf("OAuth start authorization URL = %q", startResponse.AuthorizationURL)
	}
	state := authorizationURL.Query().Get("state")
	record, _, err := fixture.server.store.getOAuthRecord(context.Background(), "authorization", startResponse.ID)
	if err != nil || record.Transaction == nil || record.Transaction.BrowserBindingHash != stateHash(firstBinding) {
		t.Fatalf("OAuth start transaction did not retain only the binding hash: %#v, %v", record, err)
	}
	fixture.server.providers = personalOAuthCallbackProvider(t, fixture.server.cfg)
	callback := httptest.NewRequest(http.MethodGet, "/api/v1/connections/callback?state="+state+"&code=synthetic-code", nil)
	setPortalCallbackHeaders(callback, firstBinding)
	completed := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(completed, callback)
	if completed.Code != http.StatusOK || !strings.Contains(completed.Body.String(), "Sign-in complete") {
		t.Fatalf("OAuth bound callback = %d %s", completed.Code, completed.Body)
	}

	completedRecord, _, err := fixture.server.store.getOAuthRecord(context.Background(), "authorization", startResponse.ID)
	if err != nil || completedRecord.Transaction.ResultConnectionID == "" {
		t.Fatal("missing authorization result")
	}
	connectionID := completedRecord.Transaction.ResultConnectionID
	secondBinding := strings.Repeat("b", 43)
	reconnect := httptest.NewRequest(http.MethodPost, "/v1/connections/"+connectionID+"/reconnect", nil)
	setPortalRequestHeaders(reconnect, fixture.grant.Owner, secondBinding)
	reconnected := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(reconnected, reconnect)
	var reconnectResponse struct {
		ID                     string `json:"authorizationId"`
		AuthorizationURL       string `json:"authorizationUrl"`
		BrowserBindingRequired bool   `json:"browserBindingRequired"`
	}
	if reconnected.Code != http.StatusOK || json.NewDecoder(reconnected.Body).Decode(&reconnectResponse) != nil || reconnectResponse.AuthorizationURL == "" || !reconnectResponse.BrowserBindingRequired {
		t.Fatalf("OAuth reconnect response = %d %s", reconnected.Code, reconnected.Body)
	}
	record, _, err = fixture.server.store.getOAuthRecord(context.Background(), "authorization", reconnectResponse.ID)
	if err != nil || record.Transaction == nil || record.Transaction.BrowserBindingHash != stateHash(secondBinding) {
		t.Fatalf("OAuth reconnect transaction did not rotate the binding: %#v, %v", record, err)
	}
}

func TestPersonalOAuthCallbackFailsClosedWithoutBoundPortalRequest(t *testing.T) {
	for _, provider := range []string{"github", "google"} {
		t.Run(provider, func(t *testing.T) { testPersonalOAuthCallbackFailsClosedWithoutBoundPortalRequest(t, provider) })
	}
}

func testPersonalOAuthCallbackFailsClosedWithoutBoundPortalRequest(t *testing.T, provider string) {
	t.Helper()
	tests := []struct {
		name    string
		proof   string
		trusted bool
		legacy  bool
	}{
		{name: "missing proof", trusted: true},
		{name: "different proof", proof: strings.Repeat("c", 43), trusted: true},
		{name: "untrusted portal", proof: strings.Repeat("b", 43)},
		{name: "legacy transaction", proof: strings.Repeat("b", 43), trusted: true, legacy: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := personalOAuthFixture(t, provider)
			fixture.server.providers = &providerClient{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unbound callback must not call the provider")
				return nil, nil
			})}}
			fixture.server.cfg.PortalNamespace, fixture.server.cfg.PortalServiceAccount = "portal", "portal"
			state, expectedProof := strings.Repeat("a", 43), strings.Repeat("b", 43)
			record, _, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
			if err != nil {
				t.Fatal(err)
			}
			bindingHash := stateHash(expectedProof)
			if test.legacy {
				bindingHash = ""
			}

			record.Transaction = &connectionTransaction{Provider: provider, StateHash: stateHash(state), BrowserBindingHash: bindingHash, CodeVerifier: strings.Repeat("d", 43), ExpiresAt: time.Now().Add(time.Hour)}
			id := stageOAuthTransaction(t, fixture, record.Transaction)
			callback := httptest.NewRequest(http.MethodGet, "/api/v1/connections/callback?state="+state+"&code=synthetic-code", nil)
			if test.proof != "" {
				callback.Header.Set(connectionBindingHeader, test.proof)
			}
			if test.trusted {
				callback.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, callback)
			current, _, err := fixture.server.store.getOAuthRecord(context.Background(), "authorization", id)
			if err != nil || response.Code != http.StatusOK || strings.Contains(response.Body.String(), "Sign-in complete") || current.Transaction == nil || current.Transaction.Consumed {
				t.Fatalf("unbound OAuth callback was not rejected before consumption: status=%d body=%q record=%#v err=%v", response.Code, response.Body.String(), current, err)
			}
		})
	}
}

func personalOAuthCallbackProvider(t *testing.T, cfg Config) *providerClient {
	t.Helper()
	return &providerClient{cfg: cfg, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "synthetic-github-token", "scope": "repo"}), nil
		case "https://api.github.com/user":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"id": 42, "login": "octocat"}), nil
		case "https://oauth2.googleapis.com/token":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "synthetic-google-token", "refresh_token": "synthetic-google-refresh", "expires_in": 3600, "scope": "https://www.googleapis.com/auth/gmail.readonly"}), nil
		case "https://openidconnect.googleapis.com/v1/userinfo":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"sub": "google-account", "email": "person@example.com"}), nil
		default:
			t.Fatalf("unexpected request: %s", request.URL)
			return nil, nil
		}
	})}}
}

func setPortalRequestHeaders(request *http.Request, owner Owner, proof string) {
	request.Header.Set("X-CetiRun-Owner-Tenant-ID", owner.TenantID)
	request.Header.Set("X-CetiRun-Owner-Object-ID", owner.ObjectID)
	request.Header.Set(connectionBindingHeader, proof)
	setPortalCallbackHeaders(request, proof)
}

func setPortalCallbackHeaders(request *http.Request, proof string) {
	request.Header.Set(connectionBindingHeader, proof)
	request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/portal/sa/portal")
}

func TestOnlyGitHubAllowsAnExplicitNonExpiringTokenResponse(t *testing.T) {
	provider := newProviderClient(Config{})
	provider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "synthetic-token"}), nil
	})}
	credential, err := provider.tokenRequest(context.Background(), "github", "https://github.com/login/oauth/access_token", url.Values{})
	if err != nil || !credential.NonExpiring || !credential.ExpiresAt.IsZero() {
		t.Fatalf("GitHub non-expiring token = %#v, %v", credential, err)
	}
	if _, err := provider.tokenRequest(context.Background(), "google", "https://oauth2.googleapis.com/token", url.Values{}); err == nil {
		t.Fatal("Google token response without expires_in was accepted")
	}
	for _, payload := range []string{
		`{"access_token":"synthetic-token","expires_in":null}`,
		`{"access_token":"synthetic-token","expires_in":"3600"}`,
		`{"access_token":"synthetic-token","expires_in":0}`,
		`{"access_token":"synthetic-token","expires_in":-1}`,
		`{"access_token":"synthetic-token","expires_in":9223372036854775808}`,
		`{"access_token":"synthetic-token","expires_in":9223372037}`,
	} {
		provider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString(payload)), Header: make(http.Header)}, nil
		})}
		if _, err := provider.tokenRequest(context.Background(), "github", "https://github.com/login/oauth/access_token", url.Values{}); err == nil {
			t.Fatalf("GitHub token response accepted invalid expires_in: %s", payload)
		}
	}
	if scopes := githubScopes("repo, gist repo\nread:user"); len(scopes) != 3 || !contains(scopes, "repo") || !contains(scopes, "gist") || !contains(scopes, "read:user") {
		t.Fatalf("GitHub scopes were not normalized: %#v", scopes)
	}

}

func TestGitHubToolCredentialIsShortLivedAndBoundedByBinding(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	fixture.server.now = func() time.Time { return now }
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Credential.ExpiresAt = time.Time{}
	record.Credential.NonExpiring = true
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	token, expiresAt, err := fixture.server.issueServiceCredential(context.Background(), record, "github", agentFromDocument(fixture.agent))
	if err != nil || token != "synthetic-provider-token" || !expiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("short-lived issuance = %s, %v", expiresAt, err)
	}

}

func TestNonExpiringCredentialMarkerFailsClosedForInvalidShapes(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		expires  time.Time
		marked   bool
	}{
		{name: "GitHub marker with expiry", provider: "github", expires: time.Now().Add(time.Hour), marked: true},
		{name: "Google marker without expiry", provider: "google", marked: true},
		{name: "GitHub missing marker", provider: "github"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := "github"
			if test.provider == "google" {
				service = "google_gmail"
			}
			fixture := newAuthorizationFixture(t, test.provider, service)
			record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
			if err != nil {
				t.Fatal(err)
			}
			record.Credential.NonExpiring, record.Credential.ExpiresAt = test.marked, test.expires
			if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
				t.Fatal(err)
			}
			if _, _, err := fixture.server.issueServiceCredential(context.Background(), record, service, agentFromDocument(fixture.agent)); err == nil {
				t.Fatal("invalid non-expiring credential shape issued a tool credential")
			}
		})
	}
}

func TestLegacyGitHubAgentModeFailsClosed(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	legacy := fixture.connection.Connection
	legacy.Mode = modeAgent
	legacy.Identity = &IdentityBinding{InstanceUID: fixture.agent.Metadata.UID}
	if fixture.server.connectionMatchesAgent(legacy, agentFromDocument(fixture.agent)) {
		t.Fatal("legacy GitHub independent identity remained usable")
	}
	record, version, err := fixture.server.store.getConnection(context.Background(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection = legacy
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	if response := fixture.request(t, "/internal/v1/service-authorizations/check"); response.Code != http.StatusForbidden {
		t.Fatalf("unsupported identity accepted: %d", response.Code)
	}

}

func personalOAuthFixture(t *testing.T, provider string) *authorizationFixture {
	t.Helper()
	if provider == "google" {
		return newAuthorizationFixture(t, provider, "google_gmail")
	}
	return newAuthorizationFixture(t, provider, "github")
}
