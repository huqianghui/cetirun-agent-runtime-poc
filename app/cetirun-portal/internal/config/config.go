// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/CetiRun/app/internal/chataccess"
)

var defaultAllowedAgentTypes = []string{"hermes", "openclaw", "custom"}

// Config holds all runtime configuration for the portal.
type Config struct {
	CustomChatEnabled bool
	ChatIdentityRealm string
	ChatCaller        chataccess.Caller
	// ListenAddr is the address the public HTTP server binds to.
	ListenAddr string
	// InternalListenAddr is the address for the internal-only HTTP server (cluster traffic only).
	// This server exposes /internal/auth for the Istio EnvoyFilter Lua ownership check.
	// It must NOT be reachable from outside the cluster (not exposed via Istio VirtualService).
	InternalListenAddr string
	// TailscaleEnabled controls whether the Tailscale feature is exposed in the portal.
	TailscaleEnabled bool
	// NoticeBanner is an optional message displayed as a banner in the portal UI.
	// Empty string means no banner.
	NoticeBanner string
	// MaxInstancesPerUser is the maximum number of Agents a single user may create.
	// 0 means unlimited. Defaults to 1.
	MaxInstancesPerUser int
	// AllowedAgentTypes controls type availability in the form and backend.
	AllowedAgentTypes []string
	// SandboxRuntimes pins deployment-approved built-in creation inputs.
	SandboxRuntimes map[string]SandboxRuntime
	// SandboxControllerDefaults requires the common Sandbox executor.
	SandboxControllerDefaults bool
	// AgentIdentityEnabled requires cryptographically verified Entra owner
	// claims for Agent Identity lifecycle management.
	AgentIdentityEnabled bool
	// AgentToolsEnabled exposes owner-scoped external account connections and
	// per-instance tool grants. It independently requires verified owner claims.
	AgentToolsEnabled      bool
	AgentToolsRuntimeTypes []string
	// EntraTenantID and PortalClientID identify the issuer and audience of the
	// ID token passed by oauth2-proxy to the Portal backend.
	EntraTenantID  string
	PortalClientID string
	// AgentToolsBrokerURL is an internal cluster URL, never browser-visible.
	AgentToolsBrokerURL         string
	PortalOrigin                string
	RegistryCredentialNamespace string
	// AIGatewayEnabled exposes the platform AI Gateway choices: the model
	// sources of new OpenClaw Agents, the GitHub Copilot sign-in and the
	// Agent's model list. Agents then require a Team.
	AIGatewayEnabled bool
	// AIGatewayCatalogNamespace is the operator namespace, where the operator
	// publishes the shared models.
	AIGatewayCatalogNamespace string
	// SkillMarketplaceEnabled makes every Team skill without the "npm:"
	// prefix a Skill Marketplace skill: Team saves check new names against
	// the marketplace catalog, and Agent details show the Team's skills.
	SkillMarketplaceEnabled bool
	// SkillMarketplaceCatalogNamespace is the operator namespace, where the
	// Skill Marketplace publishes its catalog.
	SkillMarketplaceCatalogNamespace string
	SandboxStateNamespace            string
	// SandboxStorage selects the same trusted platform configuration as the
	// Operator. The default class is a preference, never an additional approval.
	SandboxStoragePlatformNamespace     string
	SandboxStoragePlatformConfigMapName string
	SandboxStorageDefaultClassName      string
	// ConfigurationCoordination pins the authority shared with strict Sandbox
	// admission for protected Team, Sandbox and AgentPolicy mutations.
	ConfigurationCoordinationNamespace string
	ConfigurationCoordinationName      string
	ConfigurationCoordinationUID       string
}

// Load reads configuration from environment variables.
func Load() Config {
	var chatLabels map[string]string
	_ = json.Unmarshal([]byte(os.Getenv("CHAT_CALLER_LABELS")), &chatLabels)
	rawRuntimes := os.Getenv("SANDBOX_RUNTIME_PROFILES")
	runtimes := loadSandboxRuntimes(rawRuntimes)
	controllerDefaults := os.Getenv("SANDBOX_RUNTIME_DEFAULTS_FROM_CONTROLLER") == "true"
	if rawRuntimes != "" && runtimes == nil {
		slog.Error("Invalid Sandbox runtime profiles; controller defaults are disabled")
		controllerDefaults = false
	}
	return Config{
		CustomChatEnabled:                  os.Getenv("FEATURE_CUSTOM_CHAT_ENABLED") == "true",
		ChatIdentityRealm:                  os.Getenv("CHAT_IDENTITY_REALM"),
		ChatCaller:                         chataccess.Caller{Namespace: os.Getenv("CHAT_CALLER_NAMESPACE"), PodLabels: chatLabels},
		ListenAddr:                         envOrDefault("LISTEN_ADDR", ":8080"),
		InternalListenAddr:                 envOrDefault("INTERNAL_LISTEN_ADDR", ":8081"),
		TailscaleEnabled:                   os.Getenv("FEATURE_TAILSCALE_ENABLED") == "true",
		NoticeBanner:                       os.Getenv("NOTICE_BANNER"),
		MaxInstancesPerUser:                envOrDefaultInt("MAX_INSTANCES_PER_USER", 1),
		AllowedAgentTypes:                  envOrDefaultAgentTypes("ALLOWED_AGENT_TYPES", defaultAllowedAgentTypes),
		SandboxRuntimes:                    runtimes,
		SandboxControllerDefaults:          controllerDefaults,
		AgentIdentityEnabled:               os.Getenv("FEATURE_AGENT_IDENTITY_ENABLED") == "true",
		AgentToolsRuntimeTypes:             strings.Split(envOrDefault("CONNECTIONS_MCP_RUNTIME_TYPES", "hermes,openclaw"), ","),
		AgentToolsEnabled:                  os.Getenv("FEATURE_AGENT_TOOLS_ENABLED") == "true",
		EntraTenantID:                      envOrDefault("ENTRA_TENANT_ID", ""),
		PortalClientID:                     envOrDefault("PORTAL_OIDC_CLIENT_ID", ""),
		AgentToolsBrokerURL:                envOrDefault("AGENT_TOOLS_BROKER_URL", "http://cetirun-agent-broker.cetirun-credentials.svc.cluster.local:8080"),
		PortalOrigin:                       envOrDefault("PORTAL_ORIGIN", ""),
		RegistryCredentialNamespace:        envOrDefault("REGISTRY_CREDENTIAL_NAMESPACE", "cetirun-registry-credentials"),
		AIGatewayEnabled:                   os.Getenv("FEATURE_AI_GATEWAY_ENABLED") == "true",
		AIGatewayCatalogNamespace:          envOrDefault("AI_GATEWAY_CATALOG_NAMESPACE", "cetirun-system"),
		SkillMarketplaceEnabled:            os.Getenv("SKILL_MARKETPLACE_ENABLED") == "true",
		SkillMarketplaceCatalogNamespace:   envOrDefault("SKILL_MARKETPLACE_CATALOG_NAMESPACE", "cetirun-system"),
		SandboxStateNamespace:              envOrDefault("SANDBOX_STATE_NAMESPACE", "cetirun-system"),
		ConfigurationCoordinationNamespace: envOrDefault("CONFIGURATION_COORDINATION_NAMESPACE", ""),
		ConfigurationCoordinationName:      envOrDefault("CONFIGURATION_COORDINATION_NAME", ""),
		ConfigurationCoordinationUID:       envOrDefault("CONFIGURATION_COORDINATION_UID", ""),

		SandboxStoragePlatformNamespace:     os.Getenv("SANDBOX_STORAGE_PLATFORM_NAMESPACE"),
		SandboxStoragePlatformConfigMapName: os.Getenv("SANDBOX_STORAGE_PLATFORM_CONFIG_MAP_NAME"),
		SandboxStorageDefaultClassName:      os.Getenv("SANDBOX_STORAGE_DEFAULT_CLASS_NAME"),
	}
}

func (c Config) AgentTypeAllowed(kind string) bool {
	allowed := c.AllowedAgentTypes
	if allowed == nil {
		allowed = defaultAllowedAgentTypes
	}
	return slices.Contains(allowed, kind)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrDefaultInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

func envOrDefaultAgentTypes(key string, fallback []string) []string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		return append([]string(nil), fallback...)
	}

	seen := map[string]bool{}
	agentTypes := []string{}
	for _, item := range strings.Split(v, ",") {
		agentType := strings.TrimSpace(strings.ToLower(item))
		if seen[agentType] || (agentType != "openclaw" && agentType != "hermes" && agentType != "custom") {
			continue
		}
		seen[agentType] = true
		agentTypes = append(agentTypes, agentType)
	}
	if len(agentTypes) == 0 {
		return append([]string(nil), fallback...)
	}
	return agentTypes
}
