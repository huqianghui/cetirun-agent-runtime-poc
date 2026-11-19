// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGoogleOAuthAuthorizationRequiresRenewableConsent(t *testing.T) {
	cfg := Config{
		GoogleClientID:     "google-client-id",
		GoogleClientSecret: "google-client-secret",
		RedirectURI:        "https://portal.example.com/api/v1/connections/callback",
	}
	state, verifier := strings.Repeat("a", 43), strings.Repeat("b", 43)
	location, err := newProviderClient(cfg).authorizationURL("google", state, verifier, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "accounts.google.com" || parsed.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("unexpected Google authorization endpoint: %s", location)
	}
	for key, want := range map[string]string{
		"client_id": cfg.GoogleClientID, "redirect_uri": cfg.RedirectURI,
		"response_type": "code", "access_type": "offline", "prompt": "consent",
		"include_granted_scopes": "false", "state": state,
		"code_challenge_method": "S256", "code_challenge": pkceChallenge(verifier),
	} {
		if got := parsed.Query().Get(key); got != want {
			t.Errorf("Google authorization %s = %q, want %q", key, got, want)
		}
	}
	for _, incomplete := range []Config{{}, {GoogleClientID: cfg.GoogleClientID}, {GoogleClientSecret: cfg.GoogleClientSecret}} {
		if _, err := newProviderClient(incomplete).authorizationURL("google", state, verifier, ""); err == nil {
			t.Fatal("Google authorization accepted incomplete OAuth client configuration")
		}
	}
}

func TestGoogleOAuthCallbackRequiresRefreshTokenAndUsesGrantedScopes(t *testing.T) {
	for _, test := range []struct {
		name      string
		offline   bool
		connected bool
	}{
		{name: "offline consent", offline: true},
		{name: "missing refresh token"},
		{name: "connected account grants Gmail after Calendar-only consent", offline: true, connected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthorizationFixture(t, "google", "google_gmail")
			state, verifier := strings.Repeat("a", 43), strings.Repeat("b", 43)
			binding := strings.Repeat("c", 43)
			fixture.server.cfg.PortalNamespace, fixture.server.cfg.PortalServiceAccount = "portal", "portal"
			record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
			if err != nil {
				t.Fatal(err)
			}
			record.Connection.Status, record.Connection.AccountID, record.Credential = "interaction_required", "google-account", nil
			grantedScopes := []string{"openid", "email", "https://www.googleapis.com/auth/gmail.readonly"}
			if test.connected {
				record.Connection.Status = "connected"
				record.Connection.Scopes = []string{"openid", "email", "https://www.googleapis.com/auth/calendar"}
				record.Credential = &providerCredential{
					AccessToken: "synthetic-calendar-token", RefreshToken: "synthetic-calendar-refresh-token",
					ExpiresAt: time.Now().Add(time.Hour), Scopes: record.Connection.Scopes,
				}
				grantedScopes = append(grantedScopes, "https://www.googleapis.com/auth/gmail.send", "https://www.googleapis.com/auth/calendar")
			}
			tx := &connectionTransaction{TargetConnectionID: record.Connection.ID, ExpectedAccountID: "google-account", Provider: "google", StateHash: stateHash(state), BrowserBindingHash: stateHash(binding), CodeVerifier: verifier, ExpiresAt: time.Now().Add(time.Hour)}
			if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
				t.Fatal(err)
			}
			authorizationID := stageOAuthTransaction(t, fixture, tx)
			cfg := fixture.server.cfg
			cfg.GoogleClientID, cfg.GoogleClientSecret = "google-client-id", "google-client-secret"
			accountCalls := 0
			fixture.server.providers = &providerClient{cfg: cfg, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.String() {
				case "https://oauth2.googleapis.com/token":
					if request.Method != http.MethodPost || request.FormValue("client_id") != cfg.GoogleClientID || request.FormValue("client_secret") != cfg.GoogleClientSecret || request.FormValue("redirect_uri") != cfg.RedirectURI || request.FormValue("code_verifier") != verifier || request.FormValue("grant_type") != "authorization_code" || request.FormValue("code") != "synthetic-code" {
						t.Fatal("unexpected Google code exchange request")
					}
					payload := map[string]any{"access_token": "synthetic-google-token", "expires_in": 3600, "scope": strings.Join(grantedScopes, " ")}
					if test.offline {
						payload["refresh_token"] = "synthetic-google-refresh-token"
					}
					return securityJSONResponse(t, http.StatusOK, payload), nil
				case "https://openidconnect.googleapis.com/v1/userinfo":
					accountCalls++
					if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer synthetic-google-token" {
						t.Fatal("unexpected Google account request")
					}
					return securityJSONResponse(t, http.StatusOK, map[string]any{"sub": "google-account", "email": "person@example.com"}), nil
				default:
					t.Fatalf("unexpected Google request: %s", request.URL)
					return nil, nil
				}
			})}}
			response := httptest.NewRecorder()
			callback := httptest.NewRequest(http.MethodGet, "/api/v1/connections/callback?state="+state+"&code=synthetic-code", nil)
			setPortalCallbackHeaders(callback, binding)
			fixture.server.Handler().ServeHTTP(response, callback)
			current, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !test.offline {
				if current.Credential != nil || current.Connection.Status == "connected" || accountCalls != 0 || !strings.Contains(response.Body.String(), "could not be completed") {
					t.Fatal("Google callback accepted a connection without offline access")
				}
				return
			}
			if current.Connection.Status != "connected" || current.Transaction != nil || current.Connection.AccountID != "google-account" || current.Connection.AccountDisplay != "person@example.com" || accountCalls != 1 {
				t.Fatal("Google callback did not save the connected account")
			}
			credential := current.Credential
			if credential == nil || credential.AccessToken != "synthetic-google-token" || credential.RefreshToken != "synthetic-google-refresh-token" || credential.NonExpiring || !credential.ExpiresAt.After(time.Now()) {
				t.Fatal("Google callback did not save a renewable expiring credential")
			}
			if !reflect.DeepEqual(current.Connection.Scopes, grantedScopes) || !reflect.DeepEqual(credential.Scopes, grantedScopes) {
				t.Fatal("Google callback advertised tools outside the granted OAuth scopes")
			}
			if current.Connection.ID != record.Connection.ID || current.Connection.Revision != record.Connection.Revision+1 {
				t.Fatal("Google reauthorization did not update the existing connection")
			}
			authorization, _, err := fixture.server.store.getOAuthRecord(context.Background(), "authorization", authorizationID)
			if err != nil || authorization.Transaction.Status != "complete" || authorization.Transaction.ResultConnectionID != record.Connection.ID {
				t.Fatal("Google reauthorization did not return the existing connection")
			}
			storedBinding, _, err := fixture.server.store.getBinding(context.Background(), fixture.grant.ID)
			if err != nil || !reflect.DeepEqual(storedBinding.Binding, fixture.grant) {
				t.Fatal("Google reauthorization changed the existing Agent binding")
			}
		})
	}
}

func TestGoogleRefreshRetainsOfflineCredentialWhenNotRotated(t *testing.T) {
	cfg := Config{GoogleClientID: "google-client-id", GoogleClientSecret: "google-client-secret"}
	current := providerCredential{
		AccessToken: "synthetic-old-token", RefreshToken: "synthetic-refresh-token",
		ExpiresAt: time.Now().Add(-time.Minute), Scopes: []string{"https://www.googleapis.com/auth/gmail.readonly"},
	}
	provider := &providerClient{cfg: cfg, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://oauth2.googleapis.com/token" || request.Method != http.MethodPost || request.FormValue("client_id") != cfg.GoogleClientID || request.FormValue("client_secret") != cfg.GoogleClientSecret || request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != current.RefreshToken {
			t.Fatal("unexpected Google token refresh request")
		}
		return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "synthetic-new-token", "expires_in": 3600}), nil
	})}}
	next, err := provider.refresh(context.Background(), "google", current)
	if err != nil {
		t.Fatal(err)
	}
	if next.AccessToken != "synthetic-new-token" || next.RefreshToken != current.RefreshToken || next.NonExpiring || !next.ExpiresAt.After(time.Now()) || !reflect.DeepEqual(next.Scopes, current.Scopes) {
		t.Fatal("Google refresh lost the offline credential or its capabilities")
	}
}
