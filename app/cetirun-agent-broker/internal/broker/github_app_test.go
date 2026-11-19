// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func githubAppTestConfig(t *testing.T) Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "github-app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{GitHubAppID: 9, GitHubAppSlug: "cetirun", GitHubAppClientID: "client", GitHubAppClientSecret: "secret", GitHubAppPrivateKeyFile: path, RequestTimeout: time.Second}
}

func TestGitHubAppInstallationTokenUsesExactGrant(t *testing.T) {
	cfg := githubAppTestConfig(t)
	provider := newProviderClient(cfg)
	provider.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/user/installations":
			if request.Header.Get("Authorization") != "Bearer user-token" {
				t.Fatal("installation query did not use the user token")
			}
			return securityJSONResponse(t, 200, map[string]any{"installations": []any{
				map[string]any{"id": 17, "app_id": 9, "account": map[string]any{"id": 1, "login": "example-org", "type": "Organization"}, "permissions": map[string]string{"contents": "write", "issues": "write"}},
			}}), nil
		case "/user/installations/17/repositories":
			return securityJSONResponse(t, 200, map[string]any{"repositories": []any{
				map[string]any{"id": 44, "full_name": "example-org/repo", "owner": map[string]any{"id": 1, "login": "example-org", "type": "Organization"}, "permissions": map[string]bool{"admin": true}},
			}}), nil
		case "/app/installations/17/access_tokens":
			if request.Header.Get("Authorization") == "Bearer " || request.Header.Get("Authorization") == "Bearer user-token" {
				t.Fatal("installation token request did not use an App JWT")
			}
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 44 || body.Permissions["contents"] != "read" {
				t.Fatalf("installation token request grant = %#v, %v", body, err)
			}
			return securityJSONResponse(t, 201, map[string]any{"token": "installation-token", "expires_at": time.Now().Add(time.Hour), "permissions": map[string]string{"metadata": "read", "contents": "read"}, "repositories": []any{map[string]any{"id": 44, "full_name": "example-org/repo"}}}), nil
		default:
			t.Fatalf("unexpected GitHub request %s", request.URL)
			return nil, nil
		}
	})
	fixture := newAuthorizationFixture(t, "github", "github")
	fixture.server.cfg = cfg
	fixture.server.providers = provider
	fixture.connection.Connection.Mode = modeInstallation
	fixture.connection.Connection.AccountID = "github-app-user-42"
	fixture.connection.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 17, AccountLogin: "example-org", UserLogin: "octocat"}
	fixture.connection.Credential = &providerCredential{AccessToken: "user-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour)}
	stored, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Connection, stored.Credential = fixture.connection.Connection, fixture.connection.Credential
	if err := fixture.server.store.putConnection(context.Background(), stored, version); err != nil {
		t.Fatal(err)
	}
	grant := fixture.grant
	grant.GitHub = &agenttools.GitHubGrant{RepositoryIDs: []int64{44}, Permissions: map[string]string{"contents": "read"}}
	if err := fixture.server.validateGitHubAppBinding(context.Background(), &fixture.connection, grant); err != nil {
		t.Fatal(err)
	}
	token, expires, err := fixture.server.issueServiceCredential(context.Background(), &fixture.connection, "github", agentFromDocument(fixture.agent), grant)
	if err != nil || token != "installation-token" || !expires.After(time.Now()) {
		t.Fatalf("installation issuance = %q %v %v", token, expires, err)
	}
}

func TestGitHubAppInstallationTokenRejectsBroaderPermissionResponse(t *testing.T) {
	provider := newProviderClient(githubAppTestConfig(t))
	provider.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return securityJSONResponse(t, 201, map[string]any{"token": "token", "expires_at": time.Now().Add(time.Hour), "permissions": map[string]string{"metadata": "read", "contents": "read", "issues": "write"}, "repositories": []any{map[string]any{"id": 44, "full_name": "example/repo"}}}), nil
	})
	if _, _, err := provider.githubInstallationToken(context.Background(), 17, &agenttools.GitHubGrant{RepositoryIDs: []int64{44}, Permissions: map[string]string{"contents": "read"}}); err == nil {
		t.Fatal("broader installation token permission response accepted")
	}
}

func TestGitHubAppBindingRequiresSelectedInstallationAndGrant(t *testing.T) {
	owner := Owner{TenantID: testTenantID, ObjectID: testFirstAgentID}
	connection := Connection{ID: "connection", Provider: "github", Mode: modeInstallation, Status: "connected", GitHub: &agenttools.GitHubInstallation{UserLogin: "octocat"}}
	if _, err := buildBinding(bindingRequest{ConnectionID: connection.ID, Services: []string{"github"}, Enabled: true}, owner, "agent", connection, time.Now()); err == nil {
		t.Fatal("unselected installation was bindable")
	}
	connection.GitHub.InstallationID, connection.GitHub.AccountLogin = 17, "octocat"
	if _, err := buildBinding(bindingRequest{ConnectionID: connection.ID, Services: []string{"github"}, Enabled: true, GitHub: &agenttools.GitHubGrant{RepositoryIDs: []int64{44}, Permissions: map[string]string{"contents": "read"}}}, owner, "agent", connection, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubAppConnectionForOwnerHandlesMissingAndUnavailableRecords(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	if _, _, err := fixture.server.githubConnectionForOwner(context.Background(), fixture.grant.Owner, "missing"); !errors.Is(err, errNotFound) {
		t.Fatalf("missing connection error = %v, want not found", err)
	}
	fixture.server.store.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return securityJSONResponse(t, http.StatusInternalServerError, map[string]any{}), nil
	})
	if _, _, err := fixture.server.githubConnectionForOwner(context.Background(), fixture.grant.Owner, fixture.connection.Connection.ID); err == nil || errors.Is(err, errNotFound) {
		t.Fatalf("unavailable connection error = %v, want storage failure", err)
	}
}

func TestGitHubAppActiveRefreshLeaseIsTemporarilyUnavailableToManagementHandlers(t *testing.T) {
	fixture, _, _ := githubAppSecurityFixture(t)
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Status = "installation_required"
	record.Connection.GitHub = &agenttools.GitHubInstallation{AppSlug: fixture.server.cfg.GitHubAppSlug, UserLogin: "octocat"}
	record.Credential = &providerCredential{AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	record.RefreshLease = &credentialRefreshLease{Token: "lease", ExpiresAt: time.Now().Add(time.Minute)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, target, body string
	}{
		{http.MethodGet, "/v1/connections/connection/github/installations", ""},
		{http.MethodGet, "/v1/connections/connection/github/repositories?installationId=17", ""},
		{http.MethodPost, "/v1/connections/connection/github/installation", `{"installationId":17}`},
	} {
		request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
		setPortalRequestHeaders(request, fixture.grant.Owner, strings.Repeat("a", 43))
		response := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status = %d, want %d: %s", test.method, test.target, response.Code, http.StatusServiceUnavailable, response.Body.String())
		}
	}
}

func TestGitHubAppReconnectPreservesSelectedInstallation(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Mode = modeInstallation
	record.Connection.AccountID = "github-app-user-42"
	record.Connection.AccountDisplay = "octocat"
	record.Connection.Status = "connected"
	record.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 17, AccountLogin: "example-org", UserLogin: "octocat"}
	record.Credential = &providerCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.server.store.completeGitHubInstallationAccount(context.Background(), fixture.grant.Owner, connectionTransaction{Provider: "github", Mode: modeInstallation, TargetConnectionID: record.Connection.ID}, providerCredential{AccessToken: "new", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}, "renamed-octocat", "42", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil || current.Connection.Status != "connected" || current.Connection.GitHub == nil || current.Connection.GitHub.InstallationID != 17 || current.Connection.GitHub.AccountLogin != "example-org" || current.Connection.GitHub.UserLogin != "renamed-octocat" || current.Credential == nil || current.Credential.AccessToken != "new" {
		t.Fatalf("reconnect did not preserve the selected installation: %#v %v", current, err)
	}
}

func TestGitHubAppReconnectWithoutSelectedInstallationStaysPendingSelection(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Mode = modeInstallation
	record.Connection.AccountID = "github-app-user-42"
	record.Connection.Status = "installation_required"
	record.Connection.GitHub = &agenttools.GitHubInstallation{AppSlug: "cetirun", UserLogin: "old-octocat"}
	record.Credential = &providerCredential{AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.server.store.completeGitHubInstallationAccount(context.Background(), fixture.grant.Owner, connectionTransaction{Provider: "github", Mode: modeInstallation, TargetConnectionID: record.Connection.ID, AppSlug: "cetirun"}, providerCredential{AccessToken: "new", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, "renamed-octocat", "42", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil || current.Connection.Status != "installation_required" || current.Connection.GitHub == nil || current.Connection.GitHub.InstallationID != 0 || current.Connection.GitHub.AccountLogin != "" || current.Connection.GitHub.UserLogin != "renamed-octocat" || current.Credential == nil || current.Credential.AccessToken != "new" {
		t.Fatalf("reconnect did not preserve pending installation selection: %#v %v", current, err)
	}
}

func TestGitHubAppBindingAcceptsRenamedInstallationAccount(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	cfg := githubAppTestConfig(t)
	fixture.server.cfg = cfg
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Mode = modeInstallation
	record.Connection.AccountID = "github-app-user-42"
	record.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: cfg.GitHubAppSlug, AccountLogin: "old-org", UserLogin: "octocat"}
	record.Credential = &providerCredential{AccessToken: "user-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	fixture.server.providers = &providerClient{cfg: cfg, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/user/installations":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"installations": []any{map[string]any{
				"id": 17, "app_id": 9, "account": map[string]any{"id": 1, "login": "renamed-org", "type": "Organization"}, "permissions": map[string]string{"contents": "write"},
			}}}), nil
		case "/user/installations/17/repositories":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"repositories": []any{map[string]any{
				"id": 44, "full_name": "renamed-org/repository", "owner": map[string]any{"id": 1, "login": "renamed-org", "type": "Organization"}, "permissions": map[string]bool{"admin": true},
			}}}), nil
		default:
			t.Fatalf("unexpected GitHub request %s", request.URL)
			return nil, nil
		}
	})}}
	grant := fixture.grant
	grant.GitHub = &agenttools.GitHubGrant{RepositoryIDs: []int64{44}, Permissions: map[string]string{"contents": "read"}}
	if err := fixture.server.validateGitHubAppBinding(context.Background(), record, grant); err != nil {
		t.Fatalf("renamed installation account was rejected: %v", err)
	}
	current, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil || current.Connection.GitHub == nil || current.Connection.GitHub.AccountLogin != "renamed-org" {
		t.Fatalf("renamed installation account was not persisted: %#v %v", current, err)
	}
}

func TestGitHubAppPersonalRepositoryAuthorizationUsesNumericUserID(t *testing.T) {
	repository := githubRepository{Owner: githubAccount{ID: 42, Login: "renamed-octocat", Type: "User"}}
	if !githubRepositoryAuthorized(repository, 42) || githubRepositoryAuthorized(repository, 43) {
		t.Fatal("personal repository authorization did not use the stable GitHub user ID")
	}
}

func TestGitHubAppInvalidRefreshMarksInteractionRequired(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Mode = modeInstallation
	record.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 17, AccountLogin: "example-org", UserLogin: "octocat"}
	record.Credential = &providerCredential{AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	fixture.server.providers = &providerClient{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return securityJSONResponse(t, 400, map[string]string{"error": "invalid_grant"}), nil
	})}}
	if _, err := fixture.server.githubUserCredential(context.Background(), record); err == nil {
		t.Fatal("invalid refresh was accepted")
	}
	current, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil || current.Connection.Status != "interaction_required" {
		t.Fatalf("invalid refresh did not require interaction: %#v %v", current.Connection, err)
	}
}

func TestGitHubAppRefreshLeaseAllowsOnlyOneRotatingTokenExchange(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Mode = modeInstallation
	record.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 17, AccountLogin: "example-org", UserLogin: "octocat"}
	record.Credential = &providerCredential{AccessToken: "expired", RefreshToken: "rotating-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	base := fixture.server.store.client.Transport
	var storeMu sync.Mutex
	fixture.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		storeMu.Lock()
		defer storeMu.Unlock()
		return base.RoundTrip(request)
	})
	started, release := make(chan struct{}), make(chan struct{})
	var tokenMu sync.Mutex
	exchanges := 0
	fixture.server.providers = &providerClient{cfg: Config{GitHubAppClientID: "client", GitHubAppClientSecret: "secret"}, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		tokenMu.Lock()
		exchanges++
		tokenMu.Unlock()
		close(started)
		<-release
		return securityJSONResponse(t, 200, map[string]any{"access_token": "fresh", "refresh_token": "fresh-refresh", "expires_in": 3600}), nil
	})}}
	first := make(chan error, 1)
	go func() { _, err := fixture.server.githubUserCredential(context.Background(), record); first <- err }()
	<-started
	if _, err := fixture.server.githubUserCredential(context.Background(), record); err == nil {
		t.Fatal("second refresh exchanged a rotating token while the durable lease was active")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if exchanges != 1 {
		t.Fatalf("refresh exchanges = %d, want 1", exchanges)
	}
	current, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil || current.RefreshLease != nil || current.Credential == nil || current.Credential.AccessToken != "fresh" {
		t.Fatalf("refresh lease was not committed: %#v %v", current, err)
	}
}
