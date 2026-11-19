// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

const validConfigJSON = `{
  "owner":{"alias":"platform-owner","tenantID":"11111111-1111-1111-1111-111111111111","objectID":"22222222-2222-2222-2222-222222222222"},
  "kubernetes":{"apiAudience":"api://test-cluster","apiEgressCIDRs":["192.0.2.44/32"]},
  "model":{"initialMonthlyBudgetUSD":"10.25"}
}`

func TestConfigRejectsLegacyScope(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, scope := range []string{`null`, `{}`, `{"mode":"all-managed-agents"}`, `{"mode":"teams","teamNames":["example-team"]}`} {
			t.Run(scope, func(t *testing.T) {
				raw := strings.TrimSuffix(validConfigJSON, "}") + `,"scope":` + scope
				if !enabled {
					raw += `,"enabled":false`
				}
				if _, err := ParseConfig(raw + "}"); err == nil || !strings.Contains(err.Error(), `unknown field "scope"`) {
					t.Fatalf("legacy scope must be rejected even when disabled: %v", err)
				}
			})
		}
	}
}

func TestDefaultConfigHasNoManagementListFields(t *testing.T) {
	data, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scope", "teamNames", "targets", "accessHash", "platformReadNamespaces"} {
		if _, exists := fields[name]; exists {
			t.Errorf("default configuration contains unexpected %s", name)
		}
	}
}

func TestConfigAcceptsPlatformReadNamespaces(t *testing.T) {
	raw := strings.TrimSuffix(validConfigJSON, "}") + `,"platformReadNamespaces":["platform-test","kube-system"]}`
	config, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"platformReadNamespaces":["platform-test","kube-system"]`) || strings.Contains(string(data), `"scope"`) {
		t.Fatalf("platform diagnostics must use a top-level namespace field: %s", data)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	got, err := ParseConfig(validConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.InstanceName != "cetiadmin" || len(got.PlatformReadNamespaces) != 0 {
		t.Fatalf("default installation: %#v", got)
	}
	if got.Model.Default != "auto" || got.Model.InitialMonthlyBudgetUSD != "10.25" || got.Model.GitHubCopilot {
		t.Fatalf("model defaults: %#v", got.Model)
	}
	if got.Kubernetes.APIServer != "https://kubernetes.default.svc" || got.Kubernetes.CAConfigMapName != "kube-root-ca.crt" {
		t.Fatalf("API defaults: %#v", got.Kubernetes)
	}
	if got.AzureWorkloadIdentity.Enabled || got.SharedSkills.Enabled || got.Monitor.Enabled || got.Monitor.Deliver != "local" || got.Monitor.Timezone != "UTC" {
		t.Fatal("optional integrations were enabled implicitly")
	}
}

func TestDisabledConfigNeedsNoNewIdentity(t *testing.T) {
	got, err := ParseConfig(`{"enabled":false}`)
	if err != nil || got.Enabled {
		t.Fatalf("disabled config: %#v, %v", got, err)
	}
}

func TestConfigInstanceNameFitsHelmRelease(t *testing.T) {
	for _, tt := range []struct {
		name    string
		length  int
		enabled bool
	}{
		{"maximum", 44, true},
		{"too long", 45, true},
		{"namespace limit", 55, true},
		{"disabled maximum", 44, false},
		{"disabled too long", 45, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config, err := ParseConfig(validConfigJSON)
			if err != nil {
				t.Fatal(err)
			}
			config.InstanceName, config.Enabled = strings.Repeat("a", tt.length), tt.enabled
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ParseConfig(string(raw))
			if tt.length <= 44 && err != nil {
				t.Fatalf("valid Helm release name rejected: %v", err)
			}
			if tt.length > 44 && (err == nil || !strings.Contains(err.Error(), "instanceName")) {
				t.Fatalf("oversized Helm release name accepted: %v", err)
			}
		})
	}
}

func TestParseConfigRejectsInvalidInputs(t *testing.T) {
	replace := func(old, next string) string { return strings.Replace(validConfigJSON, old, next, 1) }
	appendField := func(field string) string { return strings.TrimSuffix(validConfigJSON, "}") + "," + field + "}" }
	for _, tt := range []struct {
		name, raw, field string
	}{
		{"empty", "", "configuration"},
		{"null", "null", "configuration"},
		{"array", "[]", "configuration"},
		{"trailing", validConfigJSON + `{}`, "configuration"},
		{"unknown", appendField(`"assets":{"image":"registry.example.com/extra:1"}`), "unknown field"},
		{"teams removed", appendField(`"msteams":{"enabled":true}`), "unknown field"},
		{"missing owner", `{}`, "owner"},
		{"owner alias", replace(`"platform-owner"`, `"Other_User"`), "owner.alias"},
		{"owner identity", replace(`"22222222-2222-2222-2222-222222222222"`, `"not-an-object-id"`), "owner.objectID"},
		{"negative budget", replace(`"10.25"`, `"-1"`), "model.initialMonthlyBudgetUSD"},
		{"budget precision", replace(`"10.25"`, `"1.1234567"`), "model.initialMonthlyBudgetUSD"},
		{"missing audience", replace(`"api://test-cluster"`, `""`), "kubernetes.apiAudience"},
		{"broad API CIDR", replace(`"192.0.2.44/32"`, `"192.0.2.0/24"`), "kubernetes.apiEgressCIDRs"},
		{"IMDS", replace(`"192.0.2.44/32"`, `"169.254.169.254/32"`), "kubernetes.apiEgressCIDRs"},
		{"loopback", replace(`"192.0.2.44/32"`, `"127.0.0.1/32"`), "kubernetes.apiEgressCIDRs"},
		{"API credentials", replace(`"apiAudience"`, `"apiServer":"https://name:secret@cluster.example.com","apiAudience"`), "kubernetes.apiServer"},
		{"API path", replace(`"apiAudience"`, `"apiServer":"https://cluster.example.com/private","apiAudience"`), "kubernetes.apiServer"},
		{"empty API query", replace(`"apiAudience"`, `"apiServer":"https://cluster.example.com?","apiAudience"`), "kubernetes.apiServer"},
		{"API plaintext", replace(`"apiAudience"`, `"apiServer":"http://cluster.example.com","apiAudience"`), "kubernetes.apiServer"},
		{"scope", appendField(`"scope":{"mode":"all-namespaces"}`), `unknown field "scope"`},
		{"empty teams", appendField(`"scope":{"mode":"teams"}`), `unknown field "scope"`},
		{"same migration source", appendField(`"migrationSource":{"agent":{"name":"cetiadmin","uid":"old-agent"},"namespace":{"name":"cetirun-cetiadmin","uid":"old-namespace"}}`), "migrationSource"},
		{"missing migration UID", appendField(`"migrationSource":{"agent":{"name":"old-admin"},"namespace":{"name":"cetirun-old-admin","uid":"old-namespace"}}`), "migrationSource"},
		{"mixed scope", appendField(`"scope":{"mode":"all-managed-agents","teamNames":["example-team"]}`), `unknown field "scope"`},
		{"team names", appendField(`"teamNames":["example-team"]`), `unknown field "teamNames"`},
		{"namespace", appendField(`"platformReadNamespaces":["Wrong_Name"]`), "platformReadNamespaces"},
		{"namespace domain", appendField(`"platformReadNamespaces":["platform.example"]`), "platformReadNamespaces"},
		{"namespace length", appendField(`"platformReadNamespaces":["` + strings.Repeat("a", 64) + `"]`), "platformReadNamespaces"},
		{"namespace type", appendField(`"platformReadNamespaces":"platform"`), "platformReadNamespaces"},
		{"namespace item type", appendField(`"platformReadNamespaces":[42]`), "platformReadNamespaces"},
		{"workload identity", appendField(`"azureWorkloadIdentity":{"enabled":true}`), "azureWorkloadIdentity.clientId"},
		{"zero workload identity", appendField(`"azureWorkloadIdentity":{"enabled":true,"clientId":"00000000-0000-0000-0000-000000000000"}`), "azureWorkloadIdentity.clientId"},
		{"runtime port", appendField(`"runtime":{"port":0}`), "runtime.port"},
		{"timezone", appendField(`"monitor":{"enabled":true,"schedule":"every 30m","timezone":"Mars/Nowhere"}`), "monitor.timezone"},
		{"implicit local timezone", appendField(`"monitor":{"enabled":true,"schedule":"every 30m","timezone":"Local"}`), "monitor.timezone"},
		{"missing schedule", appendField(`"monitor":{"enabled":true}`), "monitor.schedule"},
		{"Teams delivery", appendField(`"monitor":{"enabled":true,"schedule":"every 30m","deliver":"teams"}`), "monitor.deliver"},
		{"Slack destination", appendField(`"monitor":{"enabled":true,"schedule":"every 30m","deliver":"slack"}`), "monitor.slackChannel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfig(tt.raw)
			if err == nil || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("expected %q error, got %v", tt.field, err)
			}
		})
	}
}

func TestConfigKeepsExplicitIntegrations(t *testing.T) {
	raw := strings.TrimSuffix(validConfigJSON, "}") + `,
  "platformReadNamespaces":["system-test"],
  "azureWorkloadIdentity":{"enabled":true,"clientId":"33333333-3333-3333-3333-333333333333"},
  "sharedSkills":{"enabled":true},
  "monitor":{"enabled":true,"schedule":"every 30m","timezone":"Asia/Singapore","deliver":"slack","slackChannel":"C012EXAMPLE","slackSecretName":"monitor-slack"}
}`
	got, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AzureWorkloadIdentity.Enabled || !got.SharedSkills.Enabled || !got.Monitor.Enabled || got.Monitor.SlackChannel != "C012EXAMPLE" || got.Monitor.Deliver != "slack" {
		t.Fatal("explicit integrations were lost")
	}
	if err := got.ValidateMCPAudience(got.Kubernetes.APIAudience); err == nil {
		t.Fatal("Kubernetes and MCP audiences must not be interchangeable")
	}
	if err := got.ValidateMCPAudience("api://connections"); err != nil {
		t.Fatal(err)
	}
}
