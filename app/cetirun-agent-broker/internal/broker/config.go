// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"errors"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Config struct {
	// Disabled flags preserve enabled defaults for embedded configurations.
	DisableAzureAgent            bool
	DisableMicrosoftDelegated    bool
	AuthorizationCleanupInterval time.Duration

	ListenAddress           string
	TLSListenAddress        string
	TLSCertFile             string
	TLSKeyFile              string
	TLSClientCAFile         string
	TenantID                string
	BlueprintClientID       string
	AuthorizationClientID   string
	RedirectURI             string
	PortalOrigin            string
	PortalBaseURL           string
	PortalNamespace         string
	PortalServiceAccount    string
	KeyVaultKeyURL          string
	EncryptionClientID      string
	AuthSDKURL              string
	StoreNamespace          string
	FederatedTokenFile      string
	KubernetesTokenFile     string
	KubernetesCAFile        string
	OperatorNamespace       string
	OperatorServiceAccount  string
	ToolRuntimeTypes        []string
	ToolServiceNamespace    string
	ToolServiceAccount      string
	WorkloadTokenAudience   string
	GitHubOAuthClientID     string
	GitHubOAuthClientSecret string
	GitHubAppID             int64
	GitHubAppSlug           string
	GitHubAppClientID       string
	GitHubAppClientSecret   string
	GitHubAppPrivateKeyFile string
	GoogleClientID          string
	GoogleClientSecret      string
	TransactionTTL          time.Duration
	RequestTimeout          time.Duration
}

func LoadConfig() (Config, error) {
	cfg := Config{
		ListenAddress:           env("LISTEN_ADDR", ":8080"),
		TLSListenAddress:        env("TLS_LISTEN_ADDR", ":8443"),
		TLSCertFile:             os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:              os.Getenv("TLS_KEY_FILE"),
		TLSClientCAFile:         os.Getenv("TLS_CLIENT_CA_FILE"),
		TenantID:                os.Getenv("ENTRA_TENANT_ID"),
		BlueprintClientID:       os.Getenv("AGENT_ID_BLUEPRINT_CLIENT_ID"),
		AuthorizationClientID:   os.Getenv("AGENT_ID_AUTHORIZATION_CLIENT_ID"),
		RedirectURI:             os.Getenv("AGENT_ID_REDIRECT_URI"),
		PortalOrigin:            strings.TrimRight(os.Getenv("PORTAL_ORIGIN"), "/"),
		PortalBaseURL:           strings.TrimRight(os.Getenv("PORTAL_BASE_URL"), "/"),
		PortalNamespace:         os.Getenv("PORTAL_NAMESPACE"),
		PortalServiceAccount:    os.Getenv("PORTAL_SERVICE_ACCOUNT"),
		KeyVaultKeyURL:          os.Getenv("KEY_VAULT_KEY_URL"),
		EncryptionClientID:      os.Getenv("AZURE_CLIENT_ID"),
		AuthSDKURL:              strings.TrimRight(env("ENTRA_SDK_URL", "http://127.0.0.1:5000"), "/"),
		StoreNamespace:          env("AUTHORIZATION_STORE_NAMESPACE", "cetirun-credentials"),
		FederatedTokenFile:      os.Getenv("AZURE_FEDERATED_TOKEN_FILE"),
		KubernetesTokenFile:     env("KUBERNETES_TOKEN_FILE", "/var/run/cetirun/kubernetes/token"),
		KubernetesCAFile:        env("KUBERNETES_CA_FILE", "/var/run/cetirun/kubernetes/ca.crt"),
		OperatorNamespace:       os.Getenv("OPERATOR_NAMESPACE"),
		OperatorServiceAccount:  os.Getenv("OPERATOR_SERVICE_ACCOUNT"),
		ToolRuntimeTypes:        strings.Split(env("CONNECTIONS_MCP_RUNTIME_TYPES", "hermes,openclaw"), ","),
		ToolServiceNamespace:    os.Getenv("TOOL_SERVICE_NAMESPACE"),
		ToolServiceAccount:      os.Getenv("TOOL_SERVICE_ACCOUNT"),
		WorkloadTokenAudience:   env("WORKLOAD_TOKEN_AUDIENCE", "cetirun-connections-mcp"),
		GitHubOAuthClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		GitHubOAuthClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		GitHubAppSlug:           os.Getenv("GITHUB_APP_SLUG"),
		GitHubAppClientID:       os.Getenv("GITHUB_APP_CLIENT_ID"),
		GitHubAppClientSecret:   os.Getenv("GITHUB_APP_CLIENT_SECRET"),
		GitHubAppPrivateKeyFile: os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"),
		GoogleClientID:          os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleClientSecret:      os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		TransactionTTL:          10 * time.Minute,
		RequestTimeout:          15 * time.Second,

		AuthorizationCleanupInterval: 5 * time.Minute,
	}
	appID := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	if appID != "" {
		value, err := strconv.ParseInt(appID, 10, 64)
		if err != nil || value <= 0 {
			return Config{}, errors.New("GITHUB_APP_ID must be a positive integer")
		}
		cfg.GitHubAppID = value
	}
	azureAgent, err := strconv.ParseBool(env("AZURE_AGENT_ENABLED", "true"))
	if err != nil {
		return Config{}, errors.New("AZURE_AGENT_ENABLED must be a boolean")
	}
	microsoftDelegated, err := strconv.ParseBool(env("MICROSOFT_DELEGATED_ENABLED", "true"))
	if err != nil {
		return Config{}, errors.New("MICROSOFT_DELEGATED_ENABLED must be a boolean")
	}
	cfg.DisableAzureAgent, cfg.DisableMicrosoftDelegated = !azureAgent, !microsoftDelegated
	for _, kind := range cfg.ToolRuntimeTypes {
		if kind != "hermes" && kind != "openclaw" && kind != "custom" {
			return Config{}, errors.New("CONNECTIONS_MCP_RUNTIME_TYPES must list hermes, openclaw or governed custom")
		}
	}
	requiredIDs := []string{cfg.TenantID, cfg.EncryptionClientID}
	if azureAgent || microsoftDelegated {
		requiredIDs = append(requiredIDs, cfg.BlueprintClientID)
	}
	if microsoftDelegated {
		requiredIDs = append(requiredIDs, cfg.AuthorizationClientID)
	}
	for _, value := range requiredIDs {
		if !uuidPattern.MatchString(value) {
			return Config{}, errors.New("Agent Identity UUID configuration is missing or invalid")
		}
	}
	if cfg.FederatedTokenFile == "" || cfg.PortalOrigin == "" || cfg.PortalBaseURL == "" ||
		!dnsLabel(cfg.PortalNamespace) || !dnsLabel(cfg.PortalServiceAccount) ||
		!dnsLabel(cfg.OperatorNamespace) || !dnsLabel(cfg.OperatorServiceAccount) ||
		!dnsLabel(cfg.ToolServiceNamespace) || !dnsLabel(cfg.ToolServiceAccount) ||
		strings.TrimSpace(cfg.WorkloadTokenAudience) == "" {
		return Config{}, errors.New("workload token and Portal configuration are required")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") || (cfg.TLSCertFile == "") != (cfg.TLSClientCAFile == "") {
		return Config{}, errors.New("TLS certificate, key, and client CA must be configured together")
	}
	redirect, err := url.Parse(cfg.RedirectURI)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.RawQuery != "" || redirect.Fragment != "" {
		return Config{}, errors.New("Agent Identity redirect URI must be an absolute HTTPS URL")
	}
	portal, err := url.Parse(cfg.PortalOrigin)
	if err != nil || portal.Scheme != "https" || portal.Host == "" || portal.Path != "" {
		return Config{}, errors.New("Portal origin must be an HTTPS origin")
	}
	keyHost, _, _, keyOK := parseKeyID(cfg.KeyVaultKeyURL, false)
	if !keyOK || !strings.HasSuffix(strings.ToLower(keyHost), ".vault.azure.net") {
		return Config{}, errors.New("Key Vault key URL is invalid")
	}
	if err := cfg.validateGitHubApp(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) githubAppEnabled() bool {
	return c.GitHubAppID != 0 || c.GitHubAppSlug != "" || c.GitHubAppClientID != "" || c.GitHubAppClientSecret != "" || c.GitHubAppPrivateKeyFile != ""
}

func (c Config) validateGitHubApp() error {
	if !c.githubAppEnabled() {
		return nil
	}
	if c.GitHubAppID <= 0 || !dnsLabel(c.GitHubAppSlug) || c.GitHubAppClientID == "" || c.GitHubAppClientSecret == "" || c.GitHubAppPrivateKeyFile == "" {
		return errors.New("GitHub App configuration is incomplete")
	}
	return nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func (c Config) connectionModeEnabled(connection Connection) bool {
	if connectionMode(connection) == modeAgent {
		return !c.DisableAzureAgent
	}
	if connection.Provider == "azure" || connection.Provider == "microsoft" {
		return !c.DisableMicrosoftDelegated
	}
	return true
}
