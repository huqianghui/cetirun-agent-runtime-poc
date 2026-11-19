// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/configurationcoordination"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/buildinfo"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/handler"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/server"
)

//go:embed all:web/dist
var staticFiles embed.FS

func main() {
	showVersion := flag.Bool("version", false, "Print version information and exit.")
	flag.Parse()
	if *showVersion {
		fmt.Printf("cetirun-portal %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("Starting CetiRun Portal", "version", buildinfo.Version, "commit", buildinfo.Commit)

	cfg := config.Load()

	// Load admin alias allowlist (optional). Path defaults to the location
	// the Helm chart mounts the admins ConfigMap; empty / missing file is
	// not an error — the portal simply has no admin users.
	adminFile := os.Getenv("ADMIN_ALIASES_FILE")
	if adminFile == "" {
		adminFile = "/etc/cetirun-portal/admins/aliases"
	}
	adminAliases, err := config.LoadAdminAliases(adminFile)
	if err != nil {
		slog.Error("Failed to load admin aliases", "path", adminFile, "error", err)
		os.Exit(1)
	}
	admins := auth.NewAdminSet(adminAliases)
	slog.Info("Admin allowlist loaded", "count", len(admins), "path", adminFile)

	// Register CRD scheme.
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cetirunv1alpha1.AddToScheme(scheme))

	// Create Kubernetes client.
	restConfig, err := ctrlconfig.GetConfig()
	if err != nil {
		slog.Error("Failed to get kubeconfig", "error", err)
		os.Exit(1)
	}
	k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		slog.Error("Failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	// Features that manage owner-scoped resources require a signed Entra ID
	// token from oauth2-proxy. A configured Entra identity also enables the
	// internal ownership endpoint for already-bound Agents, independently of
	// those feature flags. NewOIDCVerifier requires both IDs and therefore
	// rejects an incomplete configured identity at startup.
	var oidcVerifier *auth.OIDCVerifier
	if cfg.AgentIdentityEnabled || cfg.AgentToolsEnabled || cfg.EntraTenantID != "" || cfg.PortalClientID != "" {
		oidcVerifier, err = auth.NewOIDCVerifier(cfg.EntraTenantID, cfg.PortalClientID)
		if err != nil {
			slog.Error("Invalid Portal OIDC configuration", "error", err)
			os.Exit(1)
		}
	}
	authMiddleware := auth.Middleware(admins, oidcVerifier)
	requireAdmin := auth.RequireAdmin()

	// Create handlers.
	kubeClient := kube.NewClient(k8sClient)
	kubeClient.SetSandboxStateNamespace(cfg.SandboxStateNamespace)
	if cfg.ConfigurationCoordinationName != "" || cfg.ConfigurationCoordinationNamespace != "" || cfg.ConfigurationCoordinationUID != "" {
		coordination, err := configurationcoordination.New(k8sClient, k8sClient, configurationcoordination.Authority{
			Namespace: cfg.ConfigurationCoordinationNamespace,
			Name:      cfg.ConfigurationCoordinationName,
			UID:       types.UID(cfg.ConfigurationCoordinationUID),
		})
		if err != nil {
			slog.Error("Invalid configuration coordination authority", "error", err)
			os.Exit(1)
		}
		kubeClient.SetConfigurationCoordination(coordination)
	}
	kubeClient.SetRegistryCredentialNamespace(cfg.RegistryCredentialNamespace)
	instanceHandler := &handler.InstanceHandler{Kube: kubeClient, Config: cfg}
	if oidcVerifier != nil {
		// Reuse the public middleware verifier and its JWKS cache for the
		// internal gateway ownership check.
		instanceHandler.VerifyInternalIdentity = oidcVerifier.Verify
	}
	healthHandler := &handler.HealthHandler{KubeClient: k8sClient}
	featuresHandler := &handler.FeaturesHandler{Config: cfg}
	configHandler := &handler.ConfigHandler{NoticeBanner: cfg.NoticeBanner}
	meHandler := &handler.MeHandler{ChatIdentityRealm: cfg.ChatIdentityRealm}
	adminTeamHandler := &handler.AdminTeamHandler{Kube: kubeClient, Config: cfg}
	adminInstanceHandler := &handler.AdminInstanceHandler{Kube: kubeClient, Config: cfg}
	adminPolicyHandler := &handler.AdminPolicyHandler{Kube: kubeClient, Config: cfg}
	var toolConnectionsHandler *handler.ToolConnectionsHandler
	if cfg.AgentToolsEnabled {
		toolConnectionsHandler = handler.NewToolConnectionsHandler(kubeClient, cfg.AgentToolsBrokerURL, cfg.PortalOrigin)
		toolConnectionsHandler.RuntimeTypes = cfg.AgentToolsRuntimeTypes
	}

	// Serve embedded SPA.
	dist, err := fs.Sub(staticFiles, "web/dist")
	if err != nil {
		slog.Error("Failed to access embedded static files", "error", err)
		os.Exit(1)
	}

	// Build and start public server.
	mux := server.New(dist, instanceHandler, healthHandler, featuresHandler, configHandler, meHandler, adminTeamHandler, adminInstanceHandler, adminPolicyHandler, authMiddleware, requireAdmin, toolConnectionsHandler)
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	// Build and start internal server (cluster-internal only, not exposed via Istio VS).
	// Serves /internal/auth for the Istio EnvoyFilter Lua ownership check.
	internalMux := server.NewInternalMux(instanceHandler)
	internalSrv := &http.Server{
		Addr:    cfg.InternalListenAddr,
		Handler: internalMux,
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		slog.Info("Starting portal server", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server failed", "error", err)
			os.Exit(1)
		}
	}()

	go func() {
		slog.Info("Starting internal server", "addr", cfg.InternalListenAddr)
		if err := internalSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Internal server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("Shutting down servers")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Server shutdown error", "error", err)
	}
	if err := internalSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Internal server shutdown error", "error", err)
	}
}
