// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import "testing"

func TestConnectionConfigRequiresTrustedCallersAndCompleteTLS(t *testing.T) {
	for k, v := range map[string]string{
		"ENTRA_TENANT_ID": testTenantID, "AGENT_ID_BLUEPRINT_CLIENT_ID": testBlueprintID,
		"AGENT_ID_AUTHORIZATION_CLIENT_ID": testFirstAgentID, "AZURE_CLIENT_ID": testSecondAgentID,
		"AZURE_FEDERATED_TOKEN_FILE": "/synthetic/assertion", "PORTAL_ORIGIN": "https://portal.example.com",
		"PORTAL_BASE_URL": "https://portal.example.com", "AGENT_ID_REDIRECT_URI": "https://portal.example.com/callback",
		"OPERATOR_NAMESPACE": "operator", "OPERATOR_SERVICE_ACCOUNT": "operator",
		"KEY_VAULT_KEY_URL": "https://example.vault.azure.net/keys/cache/version",
		"TLS_CERT_FILE":     "", "TLS_KEY_FILE": "", "TLS_CLIENT_CA_FILE": "",
	} {
		t.Setenv(k, v)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("missing trusted caller configuration accepted")
	}
	for k, v := range map[string]string{"PORTAL_NAMESPACE": "portal", "PORTAL_SERVICE_ACCOUNT": "portal", "TOOL_SERVICE_NAMESPACE": "tools", "TOOL_SERVICE_ACCOUNT": "tools", "WORKLOAD_TOKEN_AUDIENCE": "cetirun-connections-mcp", "TLS_CERT_FILE": "/synthetic/tls.crt", "TLS_KEY_FILE": "/synthetic/tls.key", "TLS_CLIENT_CA_FILE": "/synthetic/ca.crt"} {
		t.Setenv(k, v)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.TLSCertFile == "" {
		t.Fatalf("complete configuration rejected: %v", err)
	}
	t.Run("default MCP audience", func(t *testing.T) {
		t.Setenv("WORKLOAD_TOKEN_AUDIENCE", "")
		cfg, err := LoadConfig()
		if err != nil || cfg.WorkloadTokenAudience != "cetirun-connections-mcp" {
			t.Fatalf("unexpected default MCP audience %q: %v", cfg.WorkloadTokenAudience, err)
		}
	})
	for _, field := range []string{"PORTAL_NAMESPACE", "PORTAL_SERVICE_ACCOUNT", "TOOL_SERVICE_NAMESPACE", "TOOL_SERVICE_ACCOUNT", "TLS_CERT_FILE", "TLS_KEY_FILE", "TLS_CLIENT_CA_FILE"} {
		t.Run(field, func(t *testing.T) {
			t.Setenv(field, "")
			if _, err := LoadConfig(); err == nil {
				t.Fatal("partial configuration accepted")
			}
		})
	}
}
