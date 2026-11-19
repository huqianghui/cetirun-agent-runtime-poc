// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func githubAppSecurityFixture(t *testing.T) (*authorizationFixture, *connectionRecord, ConnectionBinding) {
	t.Helper()
	fixture := newAuthorizationFixture(t, "github", "github")
	cfg := githubAppTestConfig(t)
	cfg.PortalNamespace, cfg.PortalServiceAccount = "portal", "portal"
	fixture.server.cfg = cfg
	record, version, err := fixture.server.store.getConnection(context.Background(), fixture.connection.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection.Mode = modeInstallation
	record.Connection.AccountID = "github-app-user-42"
	record.Connection.Status = "connected"
	record.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 17, AppSlug: cfg.GitHubAppSlug, AccountLogin: "example-org", UserLogin: "octocat"}
	record.Credential = &providerCredential{AccessToken: "user-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour)}
	if err := fixture.server.store.putConnection(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	grant := fixture.grant
	grant.GitHub = &agenttools.GitHubGrant{RepositoryIDs: []int64{44}, Permissions: map[string]string{"contents": "read"}}
	return fixture, record, grant
}

func githubAppSecurityInstallation() map[string]any {
	return map[string]any{
		"id": 17, "app_id": 9,
		"account":     map[string]any{"id": 1, "login": "example-org", "type": "Organization"},
		"permissions": map[string]string{"contents": "write"},
	}
}

func githubAppSecuritySuspendedInstallation() map[string]any {
	installation := githubAppSecurityInstallation()
	installation["suspended_at"] = time.Now().UTC()
	return installation
}

func githubAppSecurityRepository(id int64, admin bool) map[string]any {
	return map[string]any{
		"id": id, "full_name": "example-org/repository-" + strconv.FormatInt(id, 10),
		"owner":       map[string]any{"id": 1, "login": "example-org", "type": "Organization"},
		"permissions": map[string]bool{"admin": admin},
	}
}

func TestGitHubAppManagementIsOwnerScoped(t *testing.T) {
	fixture, _, _ := githubAppSecurityFixture(t)
	called := 0
	fixture.server.providers = &providerClient{cfg: fixture.server.cfg, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called++
		return securityJSONResponse(t, http.StatusInternalServerError, map[string]any{}), nil
	})}}
	otherOwner := Owner{TenantID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ObjectID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}
	for _, test := range []struct {
		name, method, target, body string
	}{
		{"installations", http.MethodGet, "/v1/connections/connection/github/installations", ""},
		{"repositories", http.MethodGet, "/v1/connections/connection/github/repositories?installationId=17", ""},
		{"select", http.MethodPost, "/v1/connections/connection/github/installation", `{"installationId":17}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			setPortalRequestHeaders(request, otherOwner, strings.Repeat("a", 43))
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusNotFound {
				t.Fatalf("management request status = %d, want %d: %s", response.Code, http.StatusNotFound, response.Body.String())
			}
		})
	}
	if called != 0 {
		t.Fatalf("foreign owner reached GitHub API %d times", called)
	}
}

func TestGitHubAppAccountNamespaceCannotCollideWithLegacyOAuth(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	owner := fixture.grant.Owner
	legacyID, err := fixture.server.store.completeAccount(context.Background(), owner, connectionTransaction{Provider: "github"}, providerCredential{AccessToken: "legacy-token", NonExpiring: true}, "octocat", "42", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	appCredential := providerCredential{AccessToken: "app-token", RefreshToken: "app-refresh", ExpiresAt: time.Now().Add(time.Hour)}
	appID, err := fixture.server.store.completeGitHubInstallationAccount(context.Background(), owner, connectionTransaction{Provider: "github", Mode: modeInstallation, AppSlug: "cetirun"}, appCredential, "octocat", "42", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	legacy, _, legacyErr := fixture.server.store.getConnection(context.Background(), legacyID)
	app, _, appErr := fixture.server.store.getConnection(context.Background(), appID)
	if legacyErr != nil || appErr != nil || legacyID == appID || legacy.Connection.AccountID != "42" || legacy.Credential == nil || legacy.Credential.AccessToken != "legacy-token" || connectionMode(app.Connection) != modeInstallation || app.Connection.AccountID != "github-app-user-42" || app.Credential == nil || app.Credential.AccessToken != "app-token" {
		t.Fatalf("legacy and App GitHub accounts collided: legacy=%#v (%v), app=%#v (%v)", legacy, legacyErr, app, appErr)
	}
}

func TestGitHubAppRevokedInstallationAccessPreventsBindingAndIssuance(t *testing.T) {
	for _, test := range []struct {
		name          string
		installations []any
		repositories  []any
	}{
		{"installation_removed", []any{}, nil},
		{"installation_suspended", []any{githubAppSecuritySuspendedInstallation()}, nil},
		{"repository_removed", []any{githubAppSecurityInstallation()}, []any{}},
		{"organization_admin_lost", []any{githubAppSecurityInstallation()}, []any{githubAppSecurityRepository(44, false)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, record, grant := githubAppSecurityFixture(t)
			installationTokenPosts := 0
			fixture.server.providers = &providerClient{cfg: fixture.server.cfg, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/user/installations":
					return securityJSONResponse(t, http.StatusOK, map[string]any{"installations": test.installations}), nil
				case "/user/installations/17/repositories":
					return securityJSONResponse(t, http.StatusOK, map[string]any{"repositories": test.repositories}), nil
				case "/app/installations/17/access_tokens":
					installationTokenPosts++
					return securityJSONResponse(t, http.StatusCreated, map[string]any{}), nil
				default:
					t.Fatalf("unexpected GitHub request %s", request.URL)
					return nil, nil
				}
			})}}
			if err := fixture.server.validateGitHubAppBinding(context.Background(), record, grant); err == nil {
				t.Fatal("revoked installation access authorized a binding")
			}
			if _, _, err := fixture.server.issueGitHubInstallationCredential(context.Background(), record, grant); err == nil {
				t.Fatal("revoked installation access issued a credential")
			}
			if installationTokenPosts != 0 {
				t.Fatalf("issued %d installation token requests after authorization failed", installationTokenPosts)
			}
		})
	}
}

func TestGitHubAppBindingFindsSelectedRepositoryAfterFivePages(t *testing.T) {
	fixture, record, grant := githubAppSecurityFixture(t)
	grant.GitHub.RepositoryIDs = []int64{606}
	pages := 0
	fixture.server.providers = &providerClient{cfg: fixture.server.cfg, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/user/installations":
			return securityJSONResponse(t, http.StatusOK, map[string]any{"installations": []any{githubAppSecurityInstallation()}}), nil
		case "/user/installations/17/repositories":
			page, err := strconv.Atoi(request.URL.Query().Get("page"))
			if err != nil {
				t.Fatal(err)
			}
			pages++
			if page <= 5 {
				repositories := make([]any, 100)
				for index := range repositories {
					repositories[index] = githubAppSecurityRepository(int64(page*100+index+1), true)
				}
				return securityJSONResponse(t, http.StatusOK, map[string]any{"repositories": repositories}), nil
			}
			if page == 6 {
				return securityJSONResponse(t, http.StatusOK, map[string]any{"repositories": []any{githubAppSecurityRepository(606, true)}}), nil
			}
			t.Fatalf("unexpected repository page %d", page)
			return nil, nil
		default:
			t.Fatalf("unexpected GitHub request %s", request.URL)
			return nil, nil
		}
	})}}
	if err := fixture.server.validateGitHubAppBinding(context.Background(), record, grant); err != nil {
		t.Fatalf("repository after five pages was rejected: %v", err)
	}
	if pages != 6 {
		t.Fatalf("repository pages read = %d, want 6", pages)
	}
}

func TestGitHubAppResponseSizeAndJSONBoundaries(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"metadata_over_one_mebibyte_is_accepted", `{"id":1,"login":"octocat","metadata":"` + strings.Repeat("a", 2<<20) + `"}`, true},
		{"metadata_over_eight_mebibytes_is_rejected", `{"id":1,"login":"octocat","metadata":"` + strings.Repeat("a", (8<<20)+1) + `"}`, false},
		{"trailing_json_is_rejected", `{"id":1,"login":"octocat"}{"other":true}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newProviderClient(Config{})
			provider.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			account, err := provider.githubUser(context.Background(), "user-token")
			if test.want && (err != nil || account.ID != 1 || account.Login != "octocat") {
				t.Fatalf("bounded GitHub metadata rejected: account=%#v err=%v", account, err)
			}
			if !test.want && err == nil {
				t.Fatal("invalid GitHub response was accepted")
			}
		})
	}
}

func TestGitHubAppRefreshCommitConflictPersistsRotatedTokenWithoutSecondExchange(t *testing.T) {
	fixture, record, _ := githubAppSecurityFixture(t)
	record.Credential = &providerCredential{AccessToken: "expired", RefreshToken: "rotating-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	versionRecord, version, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	versionRecord.Credential = record.Credential
	if err := fixture.server.store.putConnection(context.Background(), versionRecord, version); err != nil {
		t.Fatal(err)
	}
	base := fixture.server.store.client.Transport
	var mu sync.Mutex
	exchanged, conflictInjected, exchanges := false, false, 0
	fixture.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		inject := exchanged && !conflictInjected && request.Method == http.MethodPut
		if inject {
			conflictInjected = true
		}
		mu.Unlock()
		if inject {
			return securityJSONResponse(t, http.StatusConflict, map[string]any{}), nil
		}
		return base.RoundTrip(request)
	})
	fixture.server.providers = &providerClient{cfg: Config{GitHubAppClientID: "client", GitHubAppClientSecret: "secret"}, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		exchanged = true
		exchanges++
		mu.Unlock()
		return securityJSONResponse(t, http.StatusOK, map[string]any{"access_token": "fresh", "refresh_token": "fresh-refresh", "expires_in": 3600}), nil
	})}}
	credential, err := fixture.server.githubUserCredential(context.Background(), record)
	mu.Lock()
	gotConflict, gotExchanges := conflictInjected, exchanges
	mu.Unlock()
	if err != nil || credential.AccessToken != "fresh" || !gotConflict || gotExchanges != 1 {
		t.Fatalf("refresh after commit conflict = %#v, err=%v, conflict=%t, exchanges=%d", credential, err, gotConflict, gotExchanges)
	}
	current, _, err := fixture.server.store.getConnection(context.Background(), record.Connection.ID)
	if err != nil || current.Credential == nil || current.Credential.AccessToken != "fresh" || current.Credential.RefreshToken != "fresh-refresh" || current.RefreshLease != nil {
		t.Fatalf("rotated credential was not persisted after conflict: %#v, %v", current, err)
	}
}
