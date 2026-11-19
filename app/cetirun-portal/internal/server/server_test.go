// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/handler"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

func TestSPAHandler_ExistingFile(t *testing.T) {
	staticFS := fstest.MapFS{
		"index.html":     {Data: []byte("<html>test</html>")},
		"assets/main.js": {Data: []byte("console.log('hello')")},
	}

	fileServer := http.FileServer(http.FS(staticFS))
	handler := spaHandler(fileServer, staticFS)

	req := httptest.NewRequest(http.MethodGet, "/assets/main.js", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "console.log('hello')" {
		t.Errorf("body = %q, want %q", w.Body.String(), "console.log('hello')")
	}
}

func TestSPAHandler_ThirdPartyNotices(t *testing.T) {
	notice := "CetiRun Portal third-party notices\nCopyright Example Authors\n"
	staticFS := fstest.MapFS{
		"index.html":               {Data: []byte("<html>SPA</html>")},
		"THIRD_PARTY_LICENSES.txt": {Data: []byte(notice)},
	}
	handler := spaHandler(http.FileServer(http.FS(staticFS)), staticFS)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/THIRD_PARTY_LICENSES.txt", nil))
	if response.Code != http.StatusOK || response.Body.String() != notice {
		t.Fatalf("notice response: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestSPAHandler_IndexFallback(t *testing.T) {
	staticFS := fstest.MapFS{
		"index.html": {Data: []byte("<html>SPA</html>")},
	}

	fileServer := http.FileServer(http.FS(staticFS))
	handler := spaHandler(fileServer, staticFS)

	req := httptest.NewRequest(http.MethodGet, "/some/spa/route", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if body != "<html>SPA</html>" {
		t.Errorf("body = %q, want index.html content", body)
	}
}

func TestSPAHandler_RootPath(t *testing.T) {
	staticFS := fstest.MapFS{
		"index.html": {Data: []byte("<html>root</html>")},
	}

	fileServer := http.FileServer(http.FS(staticFS))
	handler := spaHandler(fileServer, staticFS)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if body != "<html>root</html>" {
		t.Errorf("body = %q, want index.html content", body)
	}
}

func TestOwnerSuspensionRoutesRequireAuthenticationAndVersion(t *testing.T) {
	mux := New(fstest.MapFS{}, &handler.InstanceHandler{}, &handler.HealthHandler{},
		&handler.FeaturesHandler{}, &handler.ConfigHandler{}, &handler.MeHandler{},
		&handler.AdminTeamHandler{}, &handler.AdminInstanceHandler{}, &handler.AdminPolicyHandler{},
		auth.Middleware(nil), auth.RequireAdmin())
	for _, path := range []string{"/api/v1/instance/suspend", "/api/v1/instance/resume",
		"/api/v1/instances/alice/suspend", "/api/v1/instances/alice/resume"} {
		for _, signedIn := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodPost, path, nil)
			want := http.StatusUnauthorized
			if signedIn {
				r.Header.Set("X-Auth-Request-Email", "alice@example.com")
				want = http.StatusPreconditionRequired
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("%s signedIn=%t: %d, want %d", path, signedIn, w.Code, want)
			}
		}
	}
}

func TestRemovedTeamsAppRoutesReturnNotFound(t *testing.T) {
	mux := New(fstest.MapFS{}, &handler.InstanceHandler{}, &handler.HealthHandler{},
		&handler.FeaturesHandler{}, &handler.ConfigHandler{}, &handler.MeHandler{},
		&handler.AdminTeamHandler{}, &handler.AdminInstanceHandler{}, &handler.AdminPolicyHandler{},
		auth.Middleware(nil), auth.RequireAdmin())
	for _, path := range []string{"/api/v1/instance/teams-app", "/api/v1/instances/alice/teams-app"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Auth-Request-Email", "alice@example.com")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s: got %d want 404", path, response.Code)
		}
	}
}

func TestConnectionRoutesAreOptInAndAuthenticated(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var tools *handler.ToolConnectionsHandler
		if enabled {
			tools = handler.NewToolConnectionsHandler(nil, "http://broker.example.com", "https://portal.example.com")
		}
		mux := New(fstest.MapFS{}, &handler.InstanceHandler{}, &handler.HealthHandler{},
			&handler.FeaturesHandler{}, &handler.ConfigHandler{}, &handler.MeHandler{},
			&handler.AdminTeamHandler{}, &handler.AdminInstanceHandler{}, &handler.AdminPolicyHandler{},
			auth.Middleware(nil), auth.RequireAdmin(), tools)
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/connections"},
			{http.MethodPost, "/api/v1/connections/start"},
			{http.MethodGet, "/api/v1/connections/github-app/config"},
			{http.MethodGet, "/api/v1/connections/id"},
			{http.MethodGet, "/api/v1/connections/id/github/installations"},
			{http.MethodGet, "/api/v1/connections/id/github/repositories?installationId=1&page=1"},
			{http.MethodPost, "/api/v1/connections/id/github/installation"},
			{http.MethodGet, "/api/v1/connections/id/usage"},
			{http.MethodGet, "/api/v1/connection-authorizations/id"},
			{http.MethodDelete, "/api/v1/connections/id"},
			{http.MethodPost, "/api/v1/connections/id/reconnect"},
			{http.MethodPut, "/api/v1/instances/demo/connection-bindings/binding-id"},
			{http.MethodPost, "/api/v1/instances/demo/tool-identities"},
			{http.MethodGet, "/api/v1/instances/demo/connection-bindings"},
			{http.MethodPost, "/api/v1/instances/demo/connection-bindings"},
			{http.MethodDelete, "/api/v1/instances/demo/connection-bindings/binding-id"},
		} {
			for _, signedIn := range []bool{false, true} {
				req := httptest.NewRequest(route.method, route.path, nil)
				req.Header.Set("Origin", "https://portal.example.com")
				if signedIn {
					req.Header.Set("X-Auth-Request-Email", "alice@example.com")
				}
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, req)
				want := http.StatusUnauthorized
				if signedIn {
					want = http.StatusNotFound
					if enabled {
						want = http.StatusForbidden
					}
				}
				if response.Code != want {
					t.Errorf("%s %s enabled=%v signedIn=%v: got %d want %d", route.method, route.path, enabled, signedIn, response.Code, want)
				}
			}
		}
		setup := httptest.NewRequest(http.MethodGet, "/api/v1/connections/github-app/setup?installation_id=untrusted", nil)
		setupResponse := httptest.NewRecorder()
		mux.ServeHTTP(setupResponse, setup)
		if enabled && setupResponse.Code != http.StatusOK {
			t.Errorf("setup enabled=%v: got %d want 200", enabled, setupResponse.Code)
		}
		if !enabled && setupResponse.Code != http.StatusUnauthorized {
			t.Errorf("setup enabled=%v: got %d want 401", enabled, setupResponse.Code)
		}
		// The final cutover removes the legacy authorization API.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/instances/demo/agent-authorization", nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		if response.Code != http.StatusNotFound {
			t.Errorf("legacy route enabled=%v: got %d want 404", enabled, response.Code)
		}
	}
}

func TestAdminAgentManagementRoutesRequireAuthenticatedAdministrator(t *testing.T) {
	mux := New(fstest.MapFS{}, &handler.InstanceHandler{}, &handler.HealthHandler{},
		&handler.FeaturesHandler{}, &handler.ConfigHandler{}, &handler.MeHandler{},
		&handler.AdminTeamHandler{}, &handler.AdminInstanceHandler{}, &handler.AdminPolicyHandler{},
		auth.Middleware(auth.NewAdminSet([]string{"administrator"})), auth.RequireAdmin())

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/instances/foreign-agent"},
		{http.MethodGet, "/api/v1/admin/instances/foreign-agent/status"},
		{http.MethodPatch, "/api/v1/admin/instances/foreign-agent"},
		{http.MethodDelete, "/api/v1/admin/instances/foreign-agent"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			unauthenticated := httptest.NewRecorder()
			mux.ServeHTTP(unauthenticated, httptest.NewRequest(route.method, route.path, nil))
			if unauthenticated.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d want=%d", unauthenticated.Code, http.StatusUnauthorized)
			}

			nonAdminRequest := httptest.NewRequest(route.method, route.path, nil)
			nonAdminRequest.Header.Set("X-Auth-Request-Email", "owner@example.com")
			nonAdmin := httptest.NewRecorder()
			mux.ServeHTTP(nonAdmin, nonAdminRequest)
			if nonAdmin.Code != http.StatusForbidden {
				t.Fatalf("non-admin status=%d want=%d body=%s", nonAdmin.Code, http.StatusForbidden, nonAdmin.Body.String())
			}
		})
	}
}

func TestSkillMarketplaceRouteRequiresAuthenticatedAdministrator(t *testing.T) {
	catalog := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cetirunv1alpha1.SkillMarketplaceCatalogConfigMap, Namespace: "operator-system"},
		Data:       map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: `{"version":"v1","repository":{"url":"https://git.example.com/skills.git"},"skills":[{"name":"pdf-tools","description":"Read and write PDF files","version":"0123456789ab","path":"skills/pdf-tools","files":1,"bytes":10}]}`},
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	kubeClient := kube.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(catalog).Build())
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "disabled", want: `{"enabled":false,"known":false,"catalog":null}`},
		{name: "enabled", cfg: config.Config{SkillMarketplaceEnabled: true, SkillMarketplaceCatalogNamespace: "operator-system"},
			want: `{"enabled":true,"known":true,"catalog":{"version":"v1","repository":{"url":"https://git.example.com/skills.git"},"skills":[{"name":"pdf-tools","description":"Read and write PDF files","version":"0123456789ab","path":"skills/pdf-tools","files":1,"bytes":10}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The route uses the Team handler's client and configuration.
			mux := New(fstest.MapFS{}, &handler.InstanceHandler{}, &handler.HealthHandler{},
				&handler.FeaturesHandler{}, &handler.ConfigHandler{}, &handler.MeHandler{},
				&handler.AdminTeamHandler{Kube: kubeClient, Config: tc.cfg}, &handler.AdminInstanceHandler{}, &handler.AdminPolicyHandler{},
				auth.Middleware(auth.NewAdminSet([]string{"administrator"})), auth.RequireAdmin())
			for _, caller := range []struct {
				email  string
				status int
			}{{"", http.StatusUnauthorized}, {"owner@example.com", http.StatusForbidden}, {"administrator@example.com", http.StatusOK}} {
				request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/skill-marketplace", nil)
				if caller.email != "" {
					request.Header.Set("X-Auth-Request-Email", caller.email)
				}
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request)
				if response.Code != caller.status {
					t.Fatalf("%q: status=%d want=%d body=%s", caller.email, response.Code, caller.status, response.Body.String())
				}
				if caller.status == http.StatusOK && strings.TrimSpace(response.Body.String()) != tc.want {
					t.Fatalf("administrator body=%s want=%s", response.Body.String(), tc.want)
				}
				if caller.status != http.StatusOK && strings.Contains(response.Body.String(), "pdf-tools") {
					t.Fatalf("%q saw the catalog: %s", caller.email, response.Body.String())
				}
			}
		})
	}
}

func TestSandboxStorageRouteRequiresAuthenticatedAdministrator(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-platform", Namespace: "operator-system", UID: "platform-uid"},
		Data:       map[string]string{"platform.json": `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{},"storageClasses":{}}`},
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	kubeClient := kube.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build())
	for _, configured := range []bool{false, true} {
		cfg := config.Config{}
		if configured {
			cfg.SandboxStoragePlatformNamespace = cm.Namespace
			cfg.SandboxStoragePlatformConfigMapName = cm.Name
		}
		mux := New(fstest.MapFS{}, &handler.InstanceHandler{}, &handler.HealthHandler{},
			&handler.FeaturesHandler{}, &handler.ConfigHandler{}, &handler.MeHandler{},
			&handler.AdminTeamHandler{Kube: kubeClient, Config: cfg}, &handler.AdminInstanceHandler{}, &handler.AdminPolicyHandler{},
			auth.Middleware(auth.NewAdminSet([]string{"administrator"})), auth.RequireAdmin())
		for _, caller := range []struct {
			email  string
			status int
		}{{"", http.StatusUnauthorized}, {"owner@example.com", http.StatusForbidden}, {"administrator@example.com", http.StatusOK}} {
			want := caller.status
			if caller.email == "administrator@example.com" && !configured {
				want = http.StatusServiceUnavailable
			}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/sandbox-storage-options", nil)
			r.Header.Set("X-Auth-Request-Email", caller.email)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("configured=%v caller=%q status=%d body=%s", configured, caller.email, w.Code, w.Body.String())
			}
			if want == http.StatusOK && strings.TrimSpace(w.Body.String()) != `{"storageClasses":[],"homeAccessModes":{},"defaultStorageClassName":""}` {
				t.Fatalf("wrong options route or configuration: %s", w.Body.String())
			}
		}
	}
}
