// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func rulesAllow(rules []rbacv1.PolicyRule, group, resource, verb, name string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) &&
			(len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, name)) {
			return true
		}
	}
	return false
}

func TestCetiAdminPermissionCapabilitiesAndBoundaries(t *testing.T) {
	rules := append(ClusterOperationRules(), NamespaceOperationRules(false)...)
	for _, item := range []struct{ group, resource, verb, name string }{
		{"cetirun.microsoft.com", "agents", "create", "new-agent"}, {"cetirun.microsoft.com", "agents", "patch", "ordinary"},
		{"", "pods/log", "get", "pod"}, {"", "pods/exec", "create", "pod"}, {"", "pods", "delete", "pod"},
		{"", "secrets", "get", "helm-revision"}, {"", "secrets", "list", ""}, {"", "secrets", "watch", ""}, {"", "secrets", "create", "helm-revision"},
		{"", "secrets", "patch", "native-auth"}, {"", "configmaps", "create", "runtime-config"},
		{"apps", "statefulsets", "patch", "runtime"}, {"", "services", "patch", "runtime"},
		{"", "persistentvolumeclaims", "patch", "home"}, {"", "events", "create", "audit"}, {"events.k8s.io", "events", "patch", "audit"},
	} {
		if !rulesAllow(rules, item.group, item.resource, item.verb, item.name) {
			t.Errorf("missing native capability: %+v", item)
		}
	}
	for _, item := range []struct{ group, resource, verb, name string }{
		{"cetirun.microsoft.com", "agents", "delete", "ordinary"},
		{"cetirun.microsoft.com", "agents/status", "patch", "ordinary"}, {"cetirun.microsoft.com", "agents/finalizers", "update", "ordinary"},
		{"cetirun.microsoft.com", "teams", "patch", "team"}, {"cetirun.microsoft.com", "agentpolicies", "patch", "policy"},
		{"", "namespaces", "delete", "ordinary-ns"}, {"", "persistentvolumeclaims", "delete", "home"}, {"", "secrets", "delete", "native-auth"},
		{"", "pods", "create", "pod"}, {"", "pods/attach", "create", "pod"}, {"", "pods/portforward", "create", "pod"},
		{"", "serviceaccounts/token", "create", "controller"}, {"", "serviceaccounts", "impersonate", "controller"},
		{"rbac.authorization.k8s.io", "roles", "patch", "role"}, {"rbac.authorization.k8s.io", "clusterrolebindings", "create", "binding"},
		{"apps", "statefulsets", "create", "runtime"}, {"networking.k8s.io", "networkpolicies", "patch", "runtime"},
	} {
		if rulesAllow(rules, item.group, item.resource, item.verb, item.name) {
			t.Errorf("forbidden capability: %+v", item)
		}
	}
	for _, rule := range rules {
		for _, value := range append(append(slices.Clone(rule.APIGroups), rule.Resources...), rule.Verbs...) {
			if strings.Contains(value, "*") {
				t.Fatalf("wildcard authority: %+v", rule)
			}
		}
	}
	rules = NamespaceOperationRules(true)
	for _, resource := range []string{"pods/exec", "pods/attach", "pods/portforward", "pods", "configmaps", "secrets"} {
		if rulesAllow(rules, "", resource, "create", "anything") || rulesAllow(rules, "", resource, "delete", "anything") {
			t.Errorf("protected target gained direct write/exec: %s", resource)
		}
	}
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
		if rulesAllow(rules, "", "secrets", verb, "runtime-secret") {
			t.Errorf("protected target gained Secret %s", verb)
		}
	}
}

func TestCetiAdminClusterRoleManagesAgentsWithoutInstanceNames(t *testing.T) {
	rules := ClusterOperationRules()
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch"} {
		if !rulesAllow(rules, "cetirun.microsoft.com", "agents", verb, "new-agent-in-another-team") {
			t.Fatalf("%s requires a registered Agent name", verb)
		}
	}
	for _, resource := range []string{"secrets", "pods/exec", "persistentvolumeclaims"} {
		for _, verb := range []string{"get", "list", "create", "patch", "delete"} {
			if rulesAllow(rules, "", resource, verb, "unrelated-platform-resource") {
				t.Fatalf("cluster role grants %s %s outside Agent namespaces", verb, resource)
			}
		}
	}
}

func TestCetiAdminIstioSecurityManifestRemainsReadOnly(t *testing.T) {
	for kind, resource := range map[string]string{"AuthorizationPolicy": "authorizationpolicies", "PeerAuthentication": "peerauthentications"} {
		if !SupportedManifestKind("security.istio.io", kind) {
			t.Errorf("existing runtime security kind is not recognized: %s", kind)
		}
		rules := NamespaceOperationRules(false)
		for _, verb := range []string{"create", "update", "patch", "delete"} {
			if rulesAllow(rules, "security.istio.io", resource, verb, "runtime-security") {
				t.Errorf("manifest recognition granted a security-policy write: %s %s", verb, resource)
			}
		}
	}
	if SupportedManifestKind("security.istio.io", "RequestAuthentication") || SupportedManifestKind("arbitrary.example.com", "AuthorizationPolicy") {
		t.Fatal("the two known kinds opened a broader manifest allowlist")
	}
}

func TestCetiAdminDiagnosticDomainsAreExplicit(t *testing.T) {
	groups := map[string]bool{}
	resources := map[string]bool{}
	for _, resource := range DiagnosticResources() {
		groups[resource.Group] = true
		key := resource.Group + "/" + resource.Resource
		if resources[key] || strings.Contains(key, "*") || resource.Kind == "" || resource.Resource == "secrets" || strings.Contains(resource.Resource, "/") {
			t.Fatalf("invalid diagnostic declaration: %+v", resource)
		}
		resources[key] = true
	}
	for _, group := range []string{"", "apps", "batch", "autoscaling", "policy", "discovery.k8s.io", "networking.k8s.io", "networking.istio.io", "security.istio.io", "telemetry.istio.io", "gateway.networking.k8s.io", "cilium.io", "storage.k8s.io", "snapshot.storage.k8s.io", "rbac.authorization.k8s.io", "apiextensions.k8s.io", "apiregistration.k8s.io", "admissionregistration.k8s.io", "certificates.k8s.io", "config.gatekeeper.sh", "templates.gatekeeper.sh", "acn.azure.com", "aks.clusterconfig.azure.com", "clusterconfig.azure.com", "azmonitoring.coreos.com", "node.k8s.io", "scheduling.k8s.io", "coordination.k8s.io", "flowcontrol.apiserver.k8s.io", "cetirun.microsoft.com"} {
		if !groups[group] {
			t.Errorf("diagnostic domain missing: %q", group)
		}
	}
	for _, key := range []string{"/serviceaccounts", "rbac.authorization.k8s.io/roles", "rbac.authorization.k8s.io/rolebindings", "networking.k8s.io/networkpolicies", "networking.istio.io/sidecars", "admissionregistration.k8s.io/validatingadmissionpolicies", "admissionregistration.k8s.io/validatingadmissionpolicybindings", "rbac.authorization.k8s.io/clusterrolebindings"} {
		if !resources[key] {
			t.Errorf("unchanged Helm dependency is unreadable: %s", key)
		}
	}
}

func TestCetiAdminInstallationHoldAndStateRoundTrip(t *testing.T) {
	config, err := ParseConfig(validConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	state := Installation{Record: Record{SchemaVersion: 1, Source: ObjectIdentity{Name: "install", Namespace: "platform", UID: "source-uid"}},
		Configuration: config, Phase: PhaseActive, Hold: true, RecordUID: "record-uid", BootstrapReleasePending: true}
	if state.CanRun() {
		t.Fatal("installation hold was ignored")
	}
	raw, _ := json.Marshal(state)
	parsed, err := ParseInstallation(string(raw))
	if err != nil || parsed.RecordUID != state.RecordUID || !parsed.BootstrapReleasePending {
		t.Fatalf("state round trip: %v", err)
	}
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "stringData") {
		t.Fatal("scope included credential payload")
	}
	state.Hold = false
	if !state.CanRun() {
		t.Fatal("active phase was lost")
	}
	state.Phase = PhaseStopping
	if state.CanRun() {
		t.Fatal("stopping installation can run")
	}
}

func TestLegacyInstallationIsCleanupOnly(t *testing.T) {
	config, err := ParseConfig(validConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	state := Installation{Record: Record{SchemaVersion: 1, Source: ObjectIdentity{Name: "install", Namespace: "platform", UID: "source-uid"}}, Configuration: config, Phase: PhaseReady}
	raw, _ := json.Marshal(state)
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	stored["targets"] = []any{map[string]any{"agent": map[string]any{"name": "old-target", "uid": "target-uid"}}}
	stored["accessHash"] = "old-inventory-hash"
	stored["configuration"].(map[string]any)["scope"] = map[string]any{"mode": "all-managed-agents", "platformReadNamespaces": []string{"diagnostics"}}
	raw, _ = json.Marshal(stored)
	parsed, err := ParseInstallation(string(raw))
	if err != nil || parsed.CanRun() || len(parsed.Configuration.PlatformReadNamespaces) != 1 {
		t.Fatalf("old installation was not retained solely for cleanup: %v", err)
	}
	next, _ := json.Marshal(parsed)
	for _, field := range []string{"\"targets\"", "\"accessHash\"", "\"scope\""} {
		if strings.Contains(string(next), field) {
			t.Fatalf("old authorization field survived the new encoding: %s", field)
		}
	}
}
