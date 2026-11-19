// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The source catalog above is also the source of the Operator's delegation
// grant. Regenerate with the repository's pinned Go toolchain and this test.
func TestCetiAdminDelegationManifest(t *testing.T) {
	groups := map[string][]string{}
	for _, resource := range DiagnosticResources() {
		groups[resource.Group] = append(groups[resource.Group], resource.Resource)
	}
	groupNames := make([]string, 0, len(groups))
	for name := range groups {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	var rules []rbacv1.PolicyRule
	for _, group := range groupNames {
		resources := groups[group]
		sort.Strings(resources)
		rules = append(rules, ReadRule(group, slices.Compact(resources)...))
	}
	rules = append(rules, ClusterOperationRules()...)
	rules = append(rules, NamespaceOperationRules(false)...)
	for _, rule := range rules {
		for _, set := range [][]string{rule.APIGroups, rule.Resources, rule.Verbs, rule.NonResourceURLs} {
			for _, value := range set {
				if strings.Contains(value, "*") {
					t.Fatal("delegation contains a wildcard")
				}
			}
		}
		if slices.Contains(rule.Verbs, "escalate") || slices.Contains(rule.Verbs, "bind") {
			t.Fatal("delegation must use the actual required API permissions")
		}
	}
	encoded, err := yaml.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	header := `{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
Generated from internal/cetiadmin/permissions.go. Do not edit by hand.
Update: CETIRUN_UPDATE_CETIADMIN_RBAC=true go test ./internal/cetiadmin -run '^TestCetiAdminDelegationManifest$'
*/}}
{{- if include "cetirun-operator.cetiadminAdmission" . }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cetirun-cetiadmin-delegation
rules:
`
	tail := `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cetirun-cetiadmin-delegation
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cetirun-cetiadmin-delegation
subjects:
  - kind: ServiceAccount
    name: {{ .Values.serviceAccount.name | quote }}
    namespace: {{ .Values.namespace | quote }}
{{- end }}
`
	expected := []byte(header + "  " + strings.ReplaceAll(strings.TrimSuffix(string(encoded), "\n"), "\n", "\n  ") + "\n" + tail)
	path := filepath.Join("..", "..", "chart", "templates", "cetiadmin-delegation.yaml")
	if os.Getenv("CETIRUN_UPDATE_CETIADMIN_RBAC") == "true" {
		if err := os.WriteFile(path, expected, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatal("CetiAdmin delegation manifest differs from the permission catalog; regenerate with the command in the generated header")
	}
}
