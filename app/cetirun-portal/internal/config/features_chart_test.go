// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestCustomChartKeepsAgentIdentitySeparate(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for the Portal chart rendering check")
	}
	args := []string{"template", "portal", "../../chart", "--set", "istio.host=portal.example.com"}
	output, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("Custom chart render: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `value: "hermes,openclaw,custom"`) {
		t.Fatal("Custom type was not enabled")
	}
	for _, absent := range []string{"name: AGENT_ADMISSION_RULES", "name: REQUIRE_VERIFIED_OWNER", "name: ENTRA_TENANT_ID", "name: PORTAL_OIDC_CLIENT_ID", "name: AGENT_TOOLS_BROKER_URL"} {
		if strings.Contains(string(output), absent) {
			t.Errorf("Custom enabled unrelated configuration: %s", absent)
		}
	}
	for _, tc := range []struct {
		value string
		want  string
	}{
		{`null`, "hermes,openclaw,custom"},
		{`["hermes","openclaw"]`, "hermes,openclaw"},
	} {
		configured := append(append([]string{}, args...), "--set-json", "features.allowedAgentTypes="+tc.value)
		output, err := exec.Command(helm, configured...).CombinedOutput()
		if err != nil {
			t.Fatalf("type availability render: %v\n%s", err, output)
		}
		env, _ := renderedPortal(t, output)
		if env["ALLOWED_AGENT_TYPES"] != tc.want {
			t.Fatalf("type availability = %q, want %q", env["ALLOWED_AGENT_TYPES"], tc.want)
		}
	}
	args = append(args, "--set", "features.agentIdentityEnabled=true")
	output, err = exec.Command(helm, args...).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "tenantId is required") {
		t.Fatalf("Agent Identity accepted missing OIDC configuration: %v\n%s", err, output)
	}
	args = append(args, "--set", "agentIdentity.tenantId=11111111-1111-1111-1111-111111111111",
		"--set", "agentIdentity.portalClientId=33333333-3333-3333-3333-333333333333")
	output, err = exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("Agent Identity chart render: %v\n%s", err, output)
	}
	for _, expected := range []string{"name: ENTRA_TENANT_ID", "name: PORTAL_OIDC_CLIENT_ID", "name: PORTAL_ORIGIN"} {
		if !strings.Contains(string(output), expected) {
			t.Errorf("Agent Identity configuration is missing: %s", expected)
		}
	}
	args = []string{"template", "portal", "../../chart", "--set", "istio.host=portal.example.com", "--set", "features.agentToolsEnabled=true"}
	output, err = exec.Command(helm, args...).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "tenantId is required") {
		t.Fatalf("Agent Tools accepted missing OIDC configuration: %v\n%s", err, output)
	}
	args = append(args,
		"--set", "agentIdentity.tenantId=11111111-1111-1111-1111-111111111111",
		"--set", "agentIdentity.portalClientId=33333333-3333-3333-3333-333333333333")
	output, err = exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("Agent Tools chart render: %v\n%s", err, output)
	}
	for _, expected := range []string{"name: FEATURE_AGENT_TOOLS_ENABLED", "name: AGENT_TOOLS_BROKER_URL", "name: ENTRA_TENANT_ID", "name: PORTAL_OIDC_CLIENT_ID"} {
		if !strings.Contains(string(output), expected) {
			t.Errorf("Agent Tools configuration is missing: %s", expected)
		}
	}
}

func TestConfigurationCoordinationChartWiring(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for the Portal chart rendering check")
	}
	base := []string{"template", "portal", "../../chart", "--set", "istio.host=portal.example.com"}
	partial := append(append([]string{}, base...), "--set", "configurationCoordination.name=configuration-state")
	output, err := exec.Command(helm, partial...).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "requires namespace, name and uid together") {
		t.Fatalf("partial coordination authority was accepted: %v\n%s", err, output)
	}
	configured := append(append([]string{}, base...),
		"--set", "configurationCoordination.namespace=cetirun-system",
		"--set", "configurationCoordination.name=configuration-state",
		"--set", "configurationCoordination.uid=authority-uid")
	output, err = exec.Command(helm, configured...).CombinedOutput()
	if err != nil {
		t.Fatalf("coordination chart render: %v\n%s", err, output)
	}
	rendered := string(output)
	for _, expected := range []string{
		"name: CONFIGURATION_COORDINATION_NAMESPACE",
		"name: CONFIGURATION_COORDINATION_NAME",
		"name: CONFIGURATION_COORDINATION_UID",
		"name: cetirun-portal-configuration-coordination",
		`resourceNames: ["configuration-state"]`,
		`verbs: ["get", "update"]`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("coordination chart output is missing %q", expected)
		}
	}
}

func TestAgentToolsChartRequiresVerifiedOwnerConfiguration(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for the Portal chart rendering check")
	}
	base := []string{"template", "portal", "../../chart", "--set", "istio.host=portal.example.com"}
	cases := []struct {
		name      string
		values    []string
		wantError string
	}{
		{"defaults", nil, ""},
		{"missing tenant", []string{"features.agentToolsEnabled=true"}, "tenantId is required"},
		{"missing client", []string{"features.agentToolsEnabled=true", "agentIdentity.tenantId=tenant"}, "portalClientId is required"},
		{"missing origin", []string{"features.agentToolsEnabled=true", "agentIdentity.tenantId=tenant", "agentIdentity.portalClientId=client", "istio.host="}, "istio.host is required"},
		{"tools only", []string{"features.agentToolsEnabled=true", "agentIdentity.tenantId=tenant", "agentIdentity.portalClientId=client", "istio.host=portal.example.com", "agentTools.runtimeTypes={hermes}"}, ""},
		{"empty runtimes", []string{"features.agentToolsEnabled=true", "agentIdentity.tenantId=tenant", "agentIdentity.portalClientId=client", "istio.host=portal.example.com", "agentTools.runtimeTypes=null"}, "runtimeTypes must not be empty"},
		{"empty broker", []string{"features.agentToolsEnabled=true", "agentIdentity.tenantId=tenant", "agentIdentity.portalClientId=client", "istio.host=portal.example.com", "agentTools.brokerURL="}, "brokerURL is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{}, base...)
			for _, value := range tc.values {
				args = append(args, "--set", value)
			}
			output, err := exec.Command(helm, args...).CombinedOutput()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(string(output), tc.wantError) {
					t.Fatalf("expected %q, got %v: %s", tc.wantError, err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v: %s", err, output)
			}
			text := string(output)
			for _, env := range []string{"ENTRA_TENANT_ID", "PORTAL_OIDC_CLIENT_ID", "AGENT_TOOLS_BROKER_URL", "CONNECTIONS_MCP_RUNTIME_TYPES"} {
				if got := strings.Contains(text, "name: "+env); got != (tc.name == "tools only") {
					t.Errorf("unexpected %s configuration: present=%v", env, got)
				}
			}
			if strings.Contains(text, "name: AGENT_ID_BROKER_URL") {
				t.Error("Agent Tools implicitly enabled legacy authorization")
			}
		})
	}
}

// renderedPortal returns the environment of the Portal container and the
// rules of the Portal's own ClusterRole.
func renderedPortal(t *testing.T, output []byte) (map[string]string, []rbacv1.PolicyRule) {
	t.Helper()
	env := map[string]string{}
	var rules []rbacv1.PolicyRule
	for _, document := range strings.Split(string(output), "\n---") {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("parse rendered object: %v", err)
		}
		switch {
		case object.Kind == "Deployment" && object.Metadata.Name == "cetirun-portal":
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal([]byte(document), &deployment); err != nil {
				t.Fatal(err)
			}
			for _, item := range deployment.Spec.Template.Spec.Containers[0].Env {
				env[item.Name] = item.Value
			}
		case object.Kind == "ClusterRole" && object.Metadata.Name == "cetirun-portal":
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(document), &role); err != nil {
				t.Fatal(err)
			}
			rules = role.Rules
		}
	}
	if len(env) == 0 || len(rules) == 0 {
		t.Fatalf("the render has no Portal Deployment or ClusterRole:\n%s", output)
	}
	return env, rules
}

func TestSkillMarketplaceChartWiring(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for the Portal chart rendering check")
	}
	catalogReader := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"cetirun-skill-marketplace"}, Verbs: []string{"get"}}
	gatewayReader := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"cetirun-ai-gateway-catalog"}, Verbs: []string{"get"}}
	base := []string{"template", "portal", "../../chart", "--set", "istio.host=portal.example.com"}
	for _, tc := range []struct {
		name          string
		values        []string
		wantNamespace string
		wantError     string
	}{
		{name: "defaults"},
		{name: "disabled", values: []string{"--set", "skillMarketplace.enabled=false", "--set", "skillMarketplace.catalogNamespace=operator-system"}},
		{name: "enabled", values: []string{"--set", "skillMarketplace.enabled=true"}, wantNamespace: "cetirun-system"},
		{name: "enabled elsewhere", values: []string{"--set", "skillMarketplace.enabled=true", "--set", "skillMarketplace.catalogNamespace=operator-system"}, wantNamespace: "operator-system"},
		{name: "a string is not a switch", values: []string{"--set-string", "skillMarketplace.enabled=false"}, wantError: "skillMarketplace.enabled must be a boolean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Only standard output is YAML; Helm reports failures on standard error.
			output, err := exec.Command(helm, append(append([]string{}, base...), tc.values...)...).Output()
			var failure string
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				failure = string(exitError.Stderr)
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(failure, tc.wantError) {
					t.Fatalf("expected %q, got %v: %s", tc.wantError, err, failure)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v: %s", err, failure)
			}
			env, rules := renderedPortal(t, output)
			enabled := tc.wantNamespace != ""
			if got, ok := env["SKILL_MARKETPLACE_ENABLED"]; ok != enabled || (enabled && got != "true") {
				t.Errorf("SKILL_MARKETPLACE_ENABLED = %q (set %v), want set %v", got, ok, enabled)
			}
			if got := env["SKILL_MARKETPLACE_CATALOG_NAMESPACE"]; got != tc.wantNamespace {
				t.Errorf("SKILL_MARKETPLACE_CATALOG_NAMESPACE = %q, want %q", got, tc.wantNamespace)
			}
			hasReader := slices.ContainsFunc(rules, func(rule rbacv1.PolicyRule) bool { return reflect.DeepEqual(rule, catalogReader) })
			if hasReader != enabled {
				t.Errorf("catalog reader present=%v, want %v: %+v", hasReader, enabled, rules)
			}
			if !slices.ContainsFunc(rules, func(rule rbacv1.PolicyRule) bool { return reflect.DeepEqual(rule, gatewayReader) }) {
				t.Error("the AI Gateway catalog reader changed")
			}
			for _, rule := range rules {
				if slices.Contains(rule.Resources, "configmaps") && len(rule.ResourceNames) == 0 {
					t.Errorf("a ConfigMap rule is not limited to named ConfigMaps: %+v", rule)
				}
			}
		})
	}
}
