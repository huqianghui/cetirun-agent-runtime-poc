// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import "testing"

func TestHermesWebappRejectsPlatformOverrides(t *testing.T) {
	for _, name := range []string{
		"HERMES_WEBAPP_PUBLIC_URL", "HERMES_WEBAPP_HOST", "HERMES_WEBAPP_PORT", "HERMES_WEBAPP_ENABLED",
		"HERMES_DASHBOARD_PUBLIC_URL", "HERMES_DASHBOARD_HOST", "HERMES_DASHBOARD_PORT", "HERMES_DASHBOARD_ENABLED",
		"HERMES_DASHBOARD_BASIC_AUTH_USERNAME", "HERMES_DASHBOARD_BASIC_AUTH_PASSWORD", "HERMES_DASHBOARD_BASIC_AUTH_SECRET",
		"HERMES_DASHBOARD_SESSION_TOKEN", "HERMES_MANAGED_DIR", "HERMES_HOME", "HERMES_CONFIG_PATH", "HERMES_ENV_FILE", "HERMES_PROFILE", "HERMES_CONFIG_BACKEND", "HERMES_IGNORE_USER_CONFIG", "HERMES_GATEWAY_CONFIG", "HERMES_INSTALL_ROOT", "HERMES_RUNTIME_DIR",
		"HERMES_PYTHON", "API_SERVER_ENABLED", "API_SERVER_HOST", "API_SERVER_PORT", "API_SERVER_KEY", "API_SERVER_CORS_ORIGINS", "CETIRUN_AUTH_KEY", "CETIRUN_GATEWAY_KEY_ENV",
	} {
		t.Run(name, func(t *testing.T) {
			for _, values := range []map[string]any{
				{"hermes": map[string]any{"env": map[string]any{name: "synthetic"}}},
				{"extraEnv": []any{map[string]any{"name": name, "value": "synthetic"}}},
			} {
				if err := validateRuntimeOverrides(values, "hermes"); err == nil {
					t.Fatalf("accepted platform override %s", name)
				}
			}
		})
	}
	for _, config := range []map[string]any{
		{"webapp": map[string]any{"host": "0.0.0.0"}},
		{"dashboard": map[string]any{"port": 9119}},
		{"api_server": map[string]any{"host": "0.0.0.0"}},
		{"platforms": map[string]any{"api_server": map[string]any{"enabled": false}}},
		{"gateway": map[string]any{"platforms": map[string]any{"api_server": map[string]any{"key": "synthetic"}}}},
		{"gateway": map[string]any{"webapp": map[string]any{"public_url": "https://other.example.com"}}},
	} {
		if err := validateRuntimeOverrides(map[string]any{"hermes": map[string]any{"config": config}}, "hermes"); err == nil {
			t.Fatal("accepted platform configuration override")
		}
	}
	if err := validateRuntimeOverrides(map[string]any{"hermes": map[string]any{"config": map[string]any{"display": map[string]any{"compact": true}}}}, "hermes"); err != nil {
		t.Fatalf("unrelated preference was rejected: %v", err)
	}
}

func TestHermesWebappRejectsExternalSecretOverrides(t *testing.T) {
	for _, sources := range []map[string]any{
		{"onepassword": map[string]any{"enabled": true, "env": map[string]any{"API_SERVER_KEY": "op://example/item/key"}}},
		{"onepassword": map[string]any{"enabled": true, "env": map[string]any{"HERMES_PROFILE_NAME": "op://example/item/profile"}}},
		{"onepassword": map[string]any{"enabled": true, "service_account_token_env": "CETIRUN_AUTH_KEY"}},
		{"bitwarden": map[string]any{"enabled": true}},
		{"command": map[string]any{"enabled": true}},
	} {
		values := map[string]any{"hermes": map[string]any{"config": map[string]any{"secrets": sources}}}
		if err := validateRuntimeOverrides(values, "hermes"); err == nil {
			t.Fatal("accepted an external source that can override platform environment")
		}
	}
	values := map[string]any{"hermes": map[string]any{"config": map[string]any{"secrets": map[string]any{
		"onepassword": map[string]any{"enabled": true, "env": map[string]any{"EXAMPLE_SERVICE_TOKEN": "op://example/item/token"}},
		"bitwarden":   map[string]any{"enabled": false},
	}}}}
	if err := validateRuntimeOverrides(values, "hermes"); err != nil {
		t.Fatalf("rejected an unrelated mapped credential: %v", err)
	}
}
