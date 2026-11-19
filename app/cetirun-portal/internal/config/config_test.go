// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"os"
	"reflect"
	"testing"
)

func TestEnvOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		envValue string
		setEnv   bool
		fallback string
		want     string
	}{
		{
			name:     "env set",
			key:      "TEST_ENV_OR_DEFAULT_SET",
			envValue: "custom",
			setEnv:   true,
			fallback: "default",
			want:     "custom",
		},
		{
			name:     "env not set",
			key:      "TEST_ENV_OR_DEFAULT_UNSET",
			setEnv:   false,
			fallback: "default",
			want:     "default",
		},
		{
			name:     "env set to empty",
			key:      "TEST_ENV_OR_DEFAULT_EMPTY",
			envValue: "",
			setEnv:   true,
			fallback: "default",
			want:     "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				os.Setenv(tt.key, tt.envValue)
				defer os.Unsetenv(tt.key)
			} else {
				os.Unsetenv(tt.key)
			}

			got := envOrDefault(tt.key, tt.fallback)
			if got != tt.want {
				t.Errorf("envOrDefault(%q, %q) = %q, want %q", tt.key, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	// Set all environment variables.
	os.Setenv("LISTEN_ADDR", ":9090")
	defer func() {
		os.Unsetenv("LISTEN_ADDR")
	}()

	cfg := Load()

	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":9090")
	}
}

func TestSandboxStorageConfiguration(t *testing.T) {
	t.Setenv("SANDBOX_STORAGE_PLATFORM_NAMESPACE", "operator-system")
	t.Setenv("SANDBOX_STORAGE_PLATFORM_CONFIG_MAP_NAME", "sandbox-platform")
	t.Setenv("SANDBOX_STORAGE_DEFAULT_CLASS_NAME", "example-csi")
	cfg := Load()
	if cfg.SandboxStoragePlatformNamespace != "operator-system" || cfg.SandboxStoragePlatformConfigMapName != "sandbox-platform" || cfg.SandboxStorageDefaultClassName != "example-csi" {
		t.Fatal("Sandbox storage configuration did not load")
	}
}

func TestLoad_Defaults(t *testing.T) {
	os.Unsetenv("LISTEN_ADDR")
	t.Setenv("ALLOWED_AGENT_TYPES", "")

	cfg := Load()

	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want default %q", cfg.ListenAddr, ":8080")
	}
	if !reflect.DeepEqual(cfg.AllowedAgentTypes, []string{"hermes", "openclaw", "custom"}) {
		t.Errorf("AllowedAgentTypes = %v, want Hermes first", cfg.AllowedAgentTypes)
	}
}

func TestEnvOrDefaultAgentTypes(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		fallback []string
		want     []string
	}{
		{
			name:     "unset uses fallback",
			fallback: []string{"openclaw", "hermes"},
			want:     []string{"openclaw", "hermes"},
		},
		{
			name:     "filters and normalizes values",
			value:    " Hermes, invalid, openclaw, hermes ",
			fallback: []string{"openclaw", "hermes"},
			want:     []string{"hermes", "openclaw"},
		},
		{
			name:     "all invalid uses fallback",
			value:    "invalid",
			fallback: []string{"openclaw", "hermes"},
			want:     []string{"openclaw", "hermes"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "TEST_ALLOWED_AGENT_TYPES"
			if tt.value != "" {
				t.Setenv(key, tt.value)
			} else {
				os.Unsetenv(key)
			}

			got := envOrDefaultAgentTypes(key, tt.fallback)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("envOrDefaultAgentTypes() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCustomTypeAvailabilityKeepsAgentIdentitySeparate(t *testing.T) {
	t.Setenv("ALLOWED_AGENT_TYPES", "hermes,custom")
	t.Setenv("FEATURE_AGENT_IDENTITY_ENABLED", "false")
	cfg := Load()
	if !cfg.AgentTypeAllowed("custom") || cfg.AgentTypeAllowed("openclaw") {
		t.Fatal("configured type availability was not enforced")
	}
	if cfg.AgentIdentityEnabled {
		t.Fatal("Custom type availability enabled Agent Identity")
	}
	if !(Config{}).AgentTypeAllowed("custom") || !(Config{}).AgentTypeAllowed("hermes") {
		t.Fatal("default type availability changed")
	}
	t.Setenv("ALLOWED_AGENT_TYPES", "hermes,openclaw")
	if Load().AgentTypeAllowed("custom") {
		t.Fatal("explicit type list did not disable Custom")
	}
}

func TestAgentToolsOptIn(t *testing.T) {
	t.Setenv("FEATURE_AGENT_TOOLS_ENABLED", "")
	t.Setenv("FEATURE_AGENT_IDENTITY_ENABLED", "false")
	t.Setenv("CONNECTIONS_MCP_RUNTIME_TYPES", "")
	t.Setenv("AGENT_TOOLS_BROKER_URL", "")
	cfg := Load()
	if cfg.AgentToolsEnabled || !reflect.DeepEqual(cfg.AgentToolsRuntimeTypes, []string{"hermes", "openclaw"}) {
		t.Fatalf("unexpected Agent Tools defaults: enabled=%v runtimes=%v", cfg.AgentToolsEnabled, cfg.AgentToolsRuntimeTypes)
	}
	t.Setenv("FEATURE_AGENT_TOOLS_ENABLED", "true")
	t.Setenv("CONNECTIONS_MCP_RUNTIME_TYPES", "hermes")
	t.Setenv("AGENT_TOOLS_BROKER_URL", "http://broker.example.com:8080")
	cfg = Load()
	if !cfg.AgentToolsEnabled || cfg.AgentIdentityEnabled ||
		!reflect.DeepEqual(cfg.AgentToolsRuntimeTypes, []string{"hermes"}) ||
		cfg.AgentToolsBrokerURL != "http://broker.example.com:8080" {
		t.Fatal("Agent Tools opt-in configuration was not loaded independently")
	}
}

func TestSkillMarketplaceConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, enabled, namespace string
		wantEnabled              bool
		wantNamespace            string
	}{
		{name: "unset", wantNamespace: "cetirun-system"},
		{name: "disabled", enabled: "false", namespace: "operator-system", wantNamespace: "operator-system"},
		{name: "only true enables", enabled: "1", wantNamespace: "cetirun-system"},
		{name: "enabled", enabled: "true", wantEnabled: true, wantNamespace: "cetirun-system"},
		{name: "enabled elsewhere", enabled: "true", namespace: "operator-system", wantEnabled: true, wantNamespace: "operator-system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SKILL_MARKETPLACE_ENABLED", tc.enabled)
			t.Setenv("SKILL_MARKETPLACE_CATALOG_NAMESPACE", tc.namespace)
			t.Setenv("AI_GATEWAY_CATALOG_NAMESPACE", "")
			cfg := Load()
			if cfg.SkillMarketplaceEnabled != tc.wantEnabled || cfg.SkillMarketplaceCatalogNamespace != tc.wantNamespace {
				t.Fatalf("enabled=%v namespace=%q, want %v %q", cfg.SkillMarketplaceEnabled, cfg.SkillMarketplaceCatalogNamespace, tc.wantEnabled, tc.wantNamespace)
			}
			if cfg.AIGatewayCatalogNamespace != "cetirun-system" {
				t.Fatalf("the marketplace namespace changed the AI Gateway's: %q", cfg.AIGatewayCatalogNamespace)
			}
		})
	}
}
