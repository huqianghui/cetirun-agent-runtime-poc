// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import (
	"io/fs"
	"net/http"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/handler"
)

// New creates and configures the HTTP server mux.
func New(
	staticFS fs.FS,
	instanceHandler *handler.InstanceHandler,
	healthHandler *handler.HealthHandler,
	featuresHandler *handler.FeaturesHandler,
	configHandler *handler.ConfigHandler,
	meHandler *handler.MeHandler,
	adminTeamHandler *handler.AdminTeamHandler,
	adminInstanceHandler *handler.AdminInstanceHandler,
	adminPolicyHandler *handler.AdminPolicyHandler,
	authMiddleware func(http.Handler) http.Handler,
	requireAdmin func(http.Handler) http.Handler,
	toolConnectionsHandlers ...*handler.ToolConnectionsHandler,
) *http.ServeMux {
	mux := http.NewServeMux()

	// Health endpoints (no auth at pod level; external access may be restricted at ingress/gateway).
	mux.HandleFunc("GET /healthz", healthHandler.Liveness)
	mux.HandleFunc("GET /readyz", healthHandler.Readiness)

	// Features endpoint: no auth required, safe to expose publicly.
	mux.HandleFunc("GET /api/features", featuresHandler.GetFeatures)

	// Public config endpoint (no auth required — serves static portal configuration).
	mux.HandleFunc("GET /api/v1/config", configHandler.GetConfig)

	// API endpoints (auth headers validated by middleware).
	api := http.NewServeMux()
	chatHandler := handler.NewChatHandler(instanceHandler)
	api.HandleFunc("GET /api/v1/instances/{name}/chat", chatHandler.Describe)
	api.HandleFunc("GET /api/v1/instances/{name}/chat/v1/models", chatHandler.Models)
	api.HandleFunc("POST /api/v1/instances/{name}/chat/v1/chat/completions", chatHandler.Complete)
	api.HandleFunc("GET /api/v1/me", meHandler.GetMe)
	api.HandleFunc("GET /api/v1/instance/create-options", instanceHandler.GetCreateOptions)
	api.HandleFunc("GET /api/v1/instance", instanceHandler.GetInstance)
	api.HandleFunc("POST /api/v1/instance", instanceHandler.CreateInstance)
	api.HandleFunc("PATCH /api/v1/instance", instanceHandler.UpdateInstance)
	api.HandleFunc("DELETE /api/v1/instance", instanceHandler.DeleteInstance)
	api.HandleFunc("POST /api/v1/instance/upgrade", instanceHandler.UpgradeInstance)
	api.HandleFunc("POST /api/v1/instance/restart", instanceHandler.RestartInstance)
	api.HandleFunc("POST /api/v1/instance/suspend", instanceHandler.SuspendInstance)
	api.HandleFunc("POST /api/v1/instance/resume", instanceHandler.ResumeInstance)
	api.HandleFunc("GET /api/v1/instance/status", instanceHandler.GetInstanceStatus)

	// Multi-instance API
	api.HandleFunc("GET /api/v1/instances", instanceHandler.ListInstances)
	api.HandleFunc("POST /api/v1/instances", instanceHandler.CreateInstance)
	api.HandleFunc("GET /api/v1/instances/{name}", instanceHandler.GetInstanceByName)
	api.HandleFunc("PATCH /api/v1/instances/{name}", instanceHandler.UpdateInstanceByName)
	api.HandleFunc("DELETE /api/v1/instances/{name}", instanceHandler.DeleteInstanceByName)
	api.HandleFunc("PUT /api/v1/instances/{name}/registry-auth", instanceHandler.PutRegistryAuth)
	api.HandleFunc("DELETE /api/v1/instances/{name}/registry-auth", instanceHandler.DeleteRegistryAuth)
	api.HandleFunc("POST /api/v1/instances/{name}/upgrade", instanceHandler.UpgradeInstanceByName)
	api.HandleFunc("POST /api/v1/instances/{name}/restart", instanceHandler.RestartInstanceByName)
	api.HandleFunc("POST /api/v1/instances/{name}/suspend", instanceHandler.SuspendInstanceByName)
	api.HandleFunc("POST /api/v1/instances/{name}/resume", instanceHandler.ResumeInstanceByName)
	api.HandleFunc("GET /api/v1/instances/{name}/status", instanceHandler.GetInstanceStatusByName)
	api.HandleFunc("GET /api/v1/teams", instanceHandler.ListTeams)
	api.HandleFunc("GET /api/v1/ai-gateway/catalog", instanceHandler.GetAIGatewayCatalog)
	api.HandleFunc("POST /api/v1/instances/{name}/copilot/login", instanceHandler.StartCopilotLogin)
	api.HandleFunc("GET /api/v1/instances/{name}/copilot/login", instanceHandler.PollCopilotLogin)
	api.HandleFunc("DELETE /api/v1/instances/{name}/copilot/login", instanceHandler.CancelCopilotLogin)
	if len(toolConnectionsHandlers) > 0 && toolConnectionsHandlers[0] != nil {
		toolConnectionsHandler := toolConnectionsHandlers[0]
		mux.HandleFunc("GET /api/v1/connections/callback", toolConnectionsHandler.ConnectionCallback)
		mux.HandleFunc("POST /api/v1/connections/callback", toolConnectionsHandler.ConnectionCallback)
		mux.HandleFunc("GET /api/v1/connections/github-app/setup", toolConnectionsHandler.GitHubAppSetup)
		api.HandleFunc("GET /api/v1/connections", toolConnectionsHandler.ListConnections)
		api.HandleFunc("POST /api/v1/connections/start", toolConnectionsHandler.StartConnection)
		// Keep this exact route before the parameterized connection routes. The
		// App configuration is not a connection named "github-app".
		api.HandleFunc("GET /api/v1/connections/github-app/config", toolConnectionsHandler.GitHubAppConfig)
		api.HandleFunc("GET /api/v1/connections/{id}", toolConnectionsHandler.GetConnection)
		api.HandleFunc("GET /api/v1/connections/{id}/github/installations", toolConnectionsHandler.ListGitHubInstallations)
		api.HandleFunc("GET /api/v1/connections/{id}/github/repositories", toolConnectionsHandler.ListGitHubRepositories)
		api.HandleFunc("POST /api/v1/connections/{id}/github/installation", toolConnectionsHandler.SelectGitHubInstallation)
		api.HandleFunc("GET /api/v1/connection-authorizations/{id}", toolConnectionsHandler.GetConnectionAuthorization)
		api.HandleFunc("GET /api/v1/connections/{id}/usage", toolConnectionsHandler.GetConnectionUsage)
		api.HandleFunc("DELETE /api/v1/connections/{id}", toolConnectionsHandler.DeleteConnection)
		api.HandleFunc("POST /api/v1/connections/{id}/reconnect", toolConnectionsHandler.ReconnectConnection)
		api.HandleFunc("GET /api/v1/instances/{name}/connection-bindings", toolConnectionsHandler.ListConnectionBindings)
		api.HandleFunc("POST /api/v1/instances/{name}/tool-identities", toolConnectionsHandler.CreateToolIdentity)
		api.HandleFunc("POST /api/v1/instances/{name}/connection-bindings", toolConnectionsHandler.CreateConnectionBinding)
		api.HandleFunc("PUT /api/v1/instances/{name}/connection-bindings/{id}", toolConnectionsHandler.UpdateConnectionBinding)
		api.HandleFunc("DELETE /api/v1/instances/{name}/connection-bindings/{id}", toolConnectionsHandler.DeleteConnectionBinding)
	}

	// Admin API: gated by RequireAdmin (in addition to the auth middleware
	// applied to the entire /api/ subtree). Mounted on a child mux so
	// requireAdmin only wraps these routes.
	adminMux := http.NewServeMux()
	adminSandboxHandler := &handler.AdminSandboxHandler{Kube: adminTeamHandler.Kube, Config: adminTeamHandler.Config}
	adminMux.HandleFunc("GET /api/v1/admin/sandbox-storage-options", adminSandboxHandler.GetStorageOptions)
	adminMux.HandleFunc("GET /api/v1/admin/sandboxes", adminSandboxHandler.ListSandboxes)
	adminMux.HandleFunc("POST /api/v1/admin/sandboxes", adminSandboxHandler.CreateSandbox)
	adminMux.HandleFunc("GET /api/v1/admin/sandboxes/{name}", adminSandboxHandler.GetSandbox)
	adminMux.HandleFunc("PUT /api/v1/admin/sandboxes/{name}", adminSandboxHandler.UpdateSandbox)
	adminMux.HandleFunc("DELETE /api/v1/admin/sandboxes/{name}", adminSandboxHandler.DeleteSandbox)
	adminMux.HandleFunc("GET /api/v1/admin/teams", adminTeamHandler.ListTeams)
	adminMux.HandleFunc("POST /api/v1/admin/teams", adminTeamHandler.CreateTeam)
	adminMux.HandleFunc("GET /api/v1/admin/teams/{name}", adminTeamHandler.GetTeam)
	adminMux.HandleFunc("PUT /api/v1/admin/teams/{name}", adminTeamHandler.UpdateTeam)
	adminMux.HandleFunc("DELETE /api/v1/admin/teams/{name}", adminTeamHandler.DeleteTeam)
	adminMux.HandleFunc("GET /api/v1/admin/policies", adminPolicyHandler.ListPolicies)
	adminMux.HandleFunc("POST /api/v1/admin/policies", adminPolicyHandler.CreatePolicy)
	adminMux.HandleFunc("GET /api/v1/admin/policies/{name}", adminPolicyHandler.GetPolicy)
	adminMux.HandleFunc("PUT /api/v1/admin/policies/{name}", adminPolicyHandler.UpdatePolicy)
	adminMux.HandleFunc("DELETE /api/v1/admin/policies/{name}", adminPolicyHandler.DeletePolicy)
	// The Skill Marketplace catalog Team skills are chosen from; it shares
	// the Team handler's client and configuration.
	adminSkillMarketplaceHandler := &handler.AdminSkillMarketplaceHandler{Kube: adminTeamHandler.Kube, Config: adminTeamHandler.Config}
	adminMux.HandleFunc("GET /api/v1/admin/skill-marketplace", adminSkillMarketplaceHandler.GetSkillMarketplace)
	adminMux.HandleFunc("GET /api/v1/admin/instances", adminInstanceHandler.ListAllInstances)
	adminMux.HandleFunc("GET /api/v1/admin/instances/{name}", adminInstanceHandler.GetInstance)
	adminMux.HandleFunc("GET /api/v1/admin/instances/{name}/status", adminInstanceHandler.GetInstanceStatus)
	adminMux.HandleFunc("PATCH /api/v1/admin/instances/{name}", adminInstanceHandler.UpdateInstance)
	adminMux.HandleFunc("DELETE /api/v1/admin/instances/{name}", adminInstanceHandler.DeleteInstance)
	api.Handle("/api/v1/admin/", requireAdmin(adminMux))

	mux.Handle("/api/", authMiddleware(api))

	// Internal endpoint for Istio EnvoyFilter ownership check.
	// Intentionally NOT registered here — served on a separate internal port (:8081)
	// that is not exposed via Istio VirtualService, preventing external access.
	// See NewInternalMux().

	// SPA: serve static files, fallback to index.html for client-side routing.
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/", spaHandler(fileServer, staticFS))

	return mux
}

// spaHandler serves static files when they exist, otherwise falls back to index.html.
func spaHandler(fileServer http.Handler, fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "index.html"
		} else {
			path = path[1:] // strip leading slash
		}
		if _, err := fs.Stat(fsys, path); err != nil {
			// File not found: serve index.html for SPA routing.
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}

// NewInternalMux creates the internal-only HTTP mux served on :8081.
// This port must NOT be reachable from outside the cluster — it is not exposed via
// the Istio VirtualService. Only the Istio EnvoyFilter Lua script calls it directly
// via cluster-internal DNS (portal_auth_cluster), preventing external information leakage.
func NewInternalMux(instanceHandler *handler.InstanceHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/auth", instanceHandler.InternalAuth)
	return mux
}
