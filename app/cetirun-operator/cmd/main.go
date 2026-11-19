// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can use them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/buildinfo"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	helmclient "github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	"github.com/Azure/CetiRun/app/internal/registryauth"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cetirunv1alpha1.AddToScheme(scheme))
	// Register Istio networking types for VirtualService management.
	utilruntime.Must(istiov1beta1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var chartRepoURL string
	var chartVersion string
	var domainSuffix string
	var istioGateway string
	var defaultModel string
	var memoryImage string
	var memoryEnabled bool
	var showVersion bool
	var mode string
	var cetiAdminOutputDir string
	var cetiAdminHook cetiadmin.HookOptions
	var admissionCertDir string
	var admissionPort int
	var admissionPodStateNamespace string
	var admissionPodTimeout time.Duration

	flag.StringVar(&mode, "mode", "operator", "Process mode: operator, sandbox-admission, cetiadmin-export, cetiadmin-check or cetiadmin-stop.")
	flag.StringVar(&cetiAdminOutputDir, "cetiadmin-output-dir", "", "Output directory for the CetiAdmin asset export mode.")
	flag.StringVar(&cetiAdminHook.Namespace, "cetiadmin-namespace", "", "Operator namespace containing the CetiAdmin installation source.")
	flag.StringVar(&cetiAdminHook.ConfigMapName, "cetiadmin-config-map", "", "Exact CetiAdmin source ConfigMap for a lifecycle hook.")
	flag.StringVar(&cetiAdminHook.ExpectedConfigHash, "cetiadmin-config-hash", "", "SHA256 of the hook's rendered config.json.")
	flag.DurationVar(&cetiAdminHook.Timeout, "cetiadmin-timeout", 10*time.Minute, "CetiAdmin lifecycle hook deadline.")
	flag.StringVar(&admissionCertDir, "admission-cert-dir", "", "Directory containing platform-issued tls.crt and tls.key for Sandbox admission.")
	flag.IntVar(&admissionPort, "admission-port", 9443, "Sandbox admission HTTPS port.")
	flag.StringVar(&admissionPodStateNamespace, "admission-pod-state-namespace", "", "Protected controller state namespace for optional Sandbox Pod admission.")
	flag.DurationVar(&admissionPodTimeout, "admission-pod-timeout", sandboxadmission.DefaultPodRequestTimeout, "Total Sandbox admission validation budget; must be shorter than the API-server webhook timeout.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&chartRepoURL, "chart-repo-url", "oci://mcr.microsoft.com/aks/ai-runtime/cetirun/helm/cetirun",
		"OCI registry URL for the CetiRun Helm chart.")
	flag.StringVar(&chartVersion, "chart-version", "0.1.1-src.20261111",
		"Version of the CetiRun Helm chart to deploy.")
	flag.StringVar(&domainSuffix, "domain-suffix", "", "Base domain suffix for instance access URLs.")
	flag.StringVar(&istioGateway, "istio-gateway", controller.DefaultIstioGateway,
		"Istio Gateway namespace/name used by instance VirtualServices.")
	flag.StringVar(&defaultModel, "default-model", "gpt-6-astra", "Default GitHub Copilot model for instances without spec.model.")
	flag.BoolVar(&memoryEnabled, "memory-context-enabled", true, "Install Memory Context for Agents.")
	flag.StringVar(&memoryImage, "memory-context-image", "", "Memory Context image override; empty uses each selected runtime chart's registry and appVersion.")
	flag.BoolVar(&showVersion, "version", false, "Print version information and exit.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if showVersion {
		fmt.Printf("cetirun-operator %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return
	}
	if mode == "cetiadmin-export" {
		assets, err := cetiadmin.BundledAssets()
		if err == nil {
			err = cetiadmin.ExportAssets(cetiAdminOutputDir, assets, cetiadmin.PackagedHelmPath)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "CetiAdmin asset export failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if mode == "cetiadmin-check" || mode == "cetiadmin-stop" {
		cetiAdminHook.Mode = strings.TrimPrefix(mode, "cetiadmin-")
		kube, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err == nil {
			err = cetiadmin.RunHook(ctrl.SetupSignalHandler(), kube, cetiAdminHook)
		}
		if err != nil {
			setupLog.Error(err, "CetiAdmin lifecycle hook failed", "mode", mode)
			os.Exit(1)
		}
		return
	}
	if mode == "sandbox-admission" {
		config, err := sandboxadmission.ParseConfig(os.Getenv("CETIRUN_SANDBOX_ADMISSION"))
		if err != nil {
			setupLog.Error(err, "invalid Sandbox admission configuration")
			os.Exit(1)
		}
		setupLog.Info("Starting Sandbox configuration admission; runtime execution remains disabled",
			"version", buildinfo.Version, "commit", buildinfo.Commit)
		if err := sandboxadmission.Run(ctrl.SetupSignalHandler(), ctrl.GetConfigOrDie(), config, sandboxadmission.ServerOptions{
			CertDirectory: admissionCertDir, Port: admissionPort, MetricsAddress: metricsAddr, ProbeAddress: probeAddr,
			PodStateNamespace: admissionPodStateNamespace, PodRequestTimeout: admissionPodTimeout,
		}); err != nil {
			setupLog.Error(err, "Sandbox admission server failed")
			os.Exit(1)
		}
		return
	}
	if mode != "operator" {
		setupLog.Error(fmt.Errorf("unsupported process mode"), "unknown process mode", "mode", mode)
		os.Exit(1)
	}
	setupLog.Info("Starting CetiRun Operator", "version", buildinfo.Version, "commit", buildinfo.Commit)

	// Support environment variable overrides for chart configuration.
	if url := os.Getenv("CETIRUN_CHART_REPO_URL"); url != "" {
		chartRepoURL = url
	}
	if version := os.Getenv("CETIRUN_CHART_VERSION"); version != "" {
		chartVersion = version
	}
	if model := os.Getenv("CETIRUN_DEFAULT_MODEL"); model != "" {
		defaultModel = model
	}

	chartRef := helmclient.ChartReference{
		RepoURL: chartRepoURL,
		Version: chartVersion,
	}
	// Runtime images share the chart's registry prefix, not just its hostname.
	chartRegistry := runtimeRegistry(chartRepoURL)
	if !memoryEnabled {
		memoryImage = ""
	}
	setupLog.Info("Using OCI chart repository", "url", chartRepoURL, "version", chartVersion, "registry", chartRegistry)
	if chartRegistry != "" {
		setupLog.Info("Overriding agent image repositories",
			"openclaw", chartRegistry+"/openclaw-gateway",
			"hermes", chartRegistry+"/hermes-gateway",
			"desktop", chartRegistry+"/cetirun-desktop")
	} else {
		setupLog.Info("Could not derive image registry from chart repo URL, using chart default image.repository")
	}

	if err := chartRef.Validate(); err != nil {
		setupLog.Error(err, "invalid chart reference configuration")
		os.Exit(1)
	}

	if domainSuffix == "" {
		setupLog.Info("domain-suffix must be set (e.g. --domain-suffix=example.com)")
		os.Exit(1)
	}
	if istioGateway == "" {
		setupLog.Info("istio-gateway must be set (e.g. --istio-gateway=aks-istio-ingress/cetirun-gateway)")
		os.Exit(1)
	}

	istioRevision := os.Getenv("ISTIO_REVISION")

	// Optional: override the chart's default skills storage account for non-prod environments.
	// Deprecated: use CETIRUN_HELM_VALUES with skills.storageAccount instead.
	skillsStorageAccount := os.Getenv("SKILLS_STORAGE_ACCOUNT")
	if skillsStorageAccount != "" {
		setupLog.Info("Overriding skills storage account", "storageAccount", skillsStorageAccount)
	}

	// Operator-level Helm value overrides applied to every instance.
	var cetirunHelmValues map[string]interface{}
	if raw := os.Getenv("CETIRUN_HELM_VALUES"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cetirunHelmValues); err != nil {
			setupLog.Error(err, "failed to parse CETIRUN_HELM_VALUES")
			os.Exit(1)
		}
		setupLog.Info("Loaded operator-level CetiRun Helm values", "keys", helmValueKeys(cetirunHelmValues))
	}

	sandboxSourcesConfig, err := controller.ParseSandboxSourcesConfig(os.Getenv("CETIRUN_SANDBOX_SOURCES"))
	if err != nil {
		setupLog.Error(err, "invalid Sandbox source configuration")
		os.Exit(1)
	}
	skillMarketplaceConfig, err := controller.ParseSkillMarketplaceConfig(os.Getenv("CETIRUN_SKILL_MARKETPLACE"))
	if err != nil {
		setupLog.Error(err, "invalid Skill Marketplace configuration")
		os.Exit(1)
	}
	if skillMarketplaceConfig.Enabled {
		setupLog.Info("Skill Marketplace enabled: skill entries without the npm: prefix name Skill Marketplace skills")
		if sandboxSourcesConfig.SkillsClaimName == "" {
			setupLog.Error(fmt.Errorf("empty sandboxSources.skillsClaimName"), "the Skill Marketplace needs the name of the skills claim")
			os.Exit(1)
		}
	}
	// The operator creates the skills claims of native Agents; prerequisites
	// wait for a missing claim instead of refusing the Agent.
	sandboxSourcesConfig.SkillMarketplace = skillMarketplaceConfig.Enabled

	agentIdentityEnabled := strings.EqualFold(os.Getenv("AGENT_ID_ENABLED"), "true")
	agentIdentityCleanupEnabled := strings.EqualFold(os.Getenv("AGENT_ID_CLEANUP_ENABLED"), "true")
	agentIdentityTenantID := os.Getenv("AGENT_ID_TENANT_ID")
	agentIdentityBlueprintClientID := os.Getenv("AGENT_ID_BLUEPRINT_CLIENT_ID")
	agentIdentityNamePrefix := getEnvOrDefault("AGENT_ID_NAME_PREFIX", "cetirun-agent")
	agentIdentityBrokerURL := os.Getenv("AGENT_ID_BROKER_URL")
	if (agentIdentityEnabled || agentIdentityCleanupEnabled) && (agentIdentityTenantID == "" || agentIdentityBlueprintClientID == "" || agentIdentityBrokerURL == "") {
		setupLog.Info("Agent Identity requires tenant ID, Blueprint client ID, and Broker URL")
		os.Exit(1)
	}
	aiGatewayConfig, err := controller.ParseAIGatewayConfig(os.Getenv)
	if err != nil {
		setupLog.Error(err, "invalid AI Gateway configuration")
		os.Exit(1)
	}
	if aiGatewayConfig.Enabled {
		setupLog.Info("AI Gateway enabled", "url", aiGatewayConfig.URL,
			"defaultModel", aiGatewayConfig.DefaultModel, "githubCopilot", aiGatewayConfig.CopilotProxyImage != "",
			"copilotHealthChecks", aiGatewayConfig.CopilotHealthChecks)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cetirun-operator.cetirun.microsoft.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	if metricsAddr != "0" {
		if err := controller.RegisterSandboxMetrics(metrics.Registry, mgr); err != nil {
			setupLog.Error(err, "unable to register Sandbox reported-status metrics")
			os.Exit(1)
		}
	}

	helmClient := helmclient.NewClient(chartRef)

	customConfig, err := controller.ParseCustomRuntimeConfig(os.Getenv("CETIRUN_CUSTOM_RUNTIME"))
	if err != nil {
		setupLog.Error(err, "invalid Custom runtime configuration")
		os.Exit(1)
	}
	if raw, present := os.LookupEnv("CONNECTIONS_MCP_RUNTIME_TYPES"); present && strings.TrimSpace(raw) == "" {
		setupLog.Error(fmt.Errorf("empty runtime allowlist"), "invalid Connections MCP runtime configuration")
		os.Exit(1)
	}
	connectionsMCPRuntimeTypes, err := controller.ParseConnectionsMCPRuntimeTypes(os.Getenv("CONNECTIONS_MCP_RUNTIME_TYPES"))
	if err != nil {
		setupLog.Error(err, "invalid Connections MCP runtime configuration")
		os.Exit(1)
	}

	brokerStorageEnabled, err := controller.ParseConnectionsMCPBrokerStorageConfig(os.Getenv("CONNECTIONS_MCP_BROKER_NAMESPACE"), os.Getenv("CONNECTIONS_MCP_BROKER_SERVICE_ACCOUNT"))
	if err != nil {
		setupLog.Error(err, "invalid Broker storage configuration")
		os.Exit(1)
	}
	if brokerStorageEnabled && agentIdentityBrokerURL == "" {
		setupLog.Info("Broker storage requires Agent Identity Broker URL for cleanup")
		os.Exit(1)
	}
	agentReconciler := &controller.AgentReconciler{
		Client:                             mgr.GetClient(),
		APIReader:                          mgr.GetAPIReader(),
		Scheme:                             mgr.GetScheme(),
		EventRecorder:                      mgr.GetEventRecorderFor("cetirun-operator"),
		HelmClient:                         helmClient,
		OperatorNamespace:                  getEnvOrDefault("OPERATOR_NAMESPACE", "cetirun-system"),
		OperatorSAName:                     getEnvOrDefault("OPERATOR_SA_NAME", "cetirun-operator-controller-manager"),
		ChartVersion:                       chartVersion,
		IstioRevision:                      istioRevision,
		IstioGateway:                       istioGateway,
		DomainSuffix:                       domainSuffix,
		SkillsStorageAccount:               skillsStorageAccount,
		SkillMarketplace:                   skillMarketplaceConfig.Enabled,
		CetiRunHelmValues:                  cetirunHelmValues,
		MemoryImage:                        memoryImage,
		MemoryEnabled:                      memoryEnabled,
		MemoryRegistry:                     chartRegistry,
		ChartRegistry:                      chartRegistry,
		DefaultModel:                       defaultModel,
		RegistryCredentialNamespace:        getEnvOrDefault("REGISTRY_CREDENTIAL_NAMESPACE", registryauth.DefaultNamespace),
		AgentIdentityEnabled:               agentIdentityEnabled,
		AgentIdentityTenantID:              agentIdentityTenantID,
		AgentIdentityBlueprintClientID:     agentIdentityBlueprintClientID,
		AgentIdentityNamePrefix:            agentIdentityNamePrefix,
		AgentIdentityBrokerURL:             agentIdentityBrokerURL,
		ConnectionsMCPURL:                  os.Getenv("CONNECTIONS_MCP_URL"),
		ConnectionsMCPAudience:             os.Getenv("WORKLOAD_TOKEN_AUDIENCE"),
		ConnectionsMCPCASecretName:         os.Getenv("CONNECTIONS_MCP_CA_SECRET_NAME"),
		ConnectionsMCPRuntimeTypes:         connectionsMCPRuntimeTypes,
		ConnectionsMCPBrokerNamespace:      os.Getenv("CONNECTIONS_MCP_BROKER_NAMESPACE"),
		ConnectionsMCPBrokerServiceAccount: os.Getenv("CONNECTIONS_MCP_BROKER_SERVICE_ACCOUNT"),
		ConnectionsMCPNativeEnabled:        strings.EqualFold(os.Getenv("CONNECTIONS_MCP_NATIVE_ENABLED"), "true"),
		AIGateway:                          aiGatewayConfig,
	}
	if customConfig.Enabled {
		agentReconciler.CustomBindings = controller.NewKubernetesCustomBindings(agentReconciler, customConfig)
	}
	agentReconciler.ChatAccess, err = chataccess.Parse(os.Getenv("CETIRUN_CHAT_ACCESS"))
	if err != nil {
		setupLog.Error(err, "invalid Chat access configuration")
		os.Exit(1)
	}
	agentReconciler.SandboxPreparation, err = controller.NewKubernetesSandboxPreparation(
		sandboxSourcesConfig, mgr.GetAPIReader(), helmClient, agentReconciler.OperatorNamespace, chartVersion,
	)
	if err != nil {
		setupLog.Error(err, "unable to configure Sandbox sources")
		os.Exit(1)
	}
	agentReconciler.SandboxNativeCoordination, err = controller.NewNativeSandboxCoordination(
		sandboxSourcesConfig, mgr.GetClient(), mgr.GetAPIReader(), agentReconciler.OperatorNamespace,
	)
	if err != nil {
		setupLog.Error(err, "unable to configure native Sandbox source coordination")
		os.Exit(1)
	}
	var sandboxSource controller.SandboxConfigurationSource
	if agentReconciler.SandboxPreparation != nil {
		sandboxSource = agentReconciler.SandboxPreparation.Configuration
		if sandboxSourcesConfig.NativeExecution {
			setupLog.Info("Native Sandbox execution enabled for the bounded OpenClaw profile; physical failover fencing is not provided")
		} else {
			setupLog.Info("Kubernetes Sandbox sources enabled; qualified writer exclusion is unavailable and execution remains held")
		}
	}
	if sourceName := os.Getenv("CETIRUN_CETIADMIN_CONFIG_MAP"); sourceName != "" {
		installer := &controller.CetiAdminReconciler{
			Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Runtime: agentReconciler,
			Source:        types.NamespacedName{Namespace: agentReconciler.OperatorNamespace, Name: sourceName},
			OperatorImage: os.Getenv("CETIRUN_OPERATOR_IMAGE"), ChartVersion: chartVersion,
		}
		installer.AdmissionReady, err = installer.NewAdmissionProbe(mgr.GetConfig())
		if err != nil {
			setupLog.Error(err, "unable to configure CetiAdmin admission prerequisite")
			os.Exit(1)
		}
		installer.GrantAccess, installer.RevokeAccess = installer.GrantInstallationAccess, installer.RevokeInstallationAccess
		agentReconciler.CetiAdmin = installer
		if err = installer.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create CetiAdmin installation controller")
			os.Exit(1)
		}
	}
	if err = agentReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Agent")
		os.Exit(1)
	}
	if err := (&controller.SandboxReconciler{Source: sandboxSource, NativeExecution: sandboxSourcesConfig.NativeExecution}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Sandbox")
		os.Exit(1)
	}
	if err := (&controller.AgentPolicyReconciler{Source: sandboxSource, NativeExecution: sandboxSourcesConfig.NativeExecution}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentPolicy")
		os.Exit(1)
	}
	if err := (&controller.TeamReconciler{Source: sandboxSource, NativeExecution: sandboxSourcesConfig.NativeExecution}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Team")
		os.Exit(1)
	}
	if err := (&controller.TeamModelRouterReconciler{Agents: agentReconciler}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TeamModelRouter")
		os.Exit(1)
	}
	if skillMarketplaceConfig.Enabled {
		if err := setupSkillMarketplace(mgr, skillMarketplaceConfig, sandboxSourcesConfig.SkillsClaimName, agentReconciler.OperatorNamespace); err != nil {
			setupLog.Error(err, "unable to set up the Skill Marketplace")
			os.Exit(1)
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func getEnvOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// setupSkillMarketplace adds the operator's part of the Skill Marketplace: the
// Teams document for the worker, the Agents' skills claims, and the removal of
// the volumes whose claim is gone.
func setupSkillMarketplace(mgr ctrl.Manager, config controller.SkillMarketplaceConfig, claimName, namespace string) error {
	storage := skillmarketplace.StorageSource{
		ResourceGroup: config.Storage.ResourceGroup, StorageAccount: config.Storage.StorageAccount,
		ContainerName: config.Storage.ContainerName, StorageClassName: config.Storage.StorageClassName,
	}
	if err := (&skillmarketplace.TeamsReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Namespace: namespace}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&skillmarketplace.ClaimReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Storage: storage, ClaimName: claimName}).SetupWithManager(mgr); err != nil {
		return err
	}
	return mgr.Add(&skillmarketplace.VolumeCollector{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Interval: 5 * time.Minute})
}

func runtimeRegistry(repoURL string) string {
	registry := extractRegistryHost(repoURL)
	if registry == "" {
		return ""
	}
	// Published charts and images share the registry prefix, including staging
	// paths. The hostname alone would select a different image repository.
	if prefix, ok := strings.CutSuffix(strings.TrimPrefix(repoURL, "oci://"), "/helm/cetirun"); ok {
		registry = prefix
	}
	return registry
}

// extractRegistryHost extracts the registry hostname from an OCI chart URL.
// Example: "oci://registry.example.com/helm/cetirun" → "registry.example.com".
// Returns empty string if the URL cannot be parsed or the host is empty.
func extractRegistryHost(repoURL string) string {
	// Replace oci:// with https:// so net/url can parse it.
	parsed, err := url.Parse(strings.Replace(repoURL, "oci://", "https://", 1))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

// helmValueKeys returns the top-level keys of a map for logging purposes.
func helmValueKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
