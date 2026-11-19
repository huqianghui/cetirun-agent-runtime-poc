// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registryauth

import (
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
)

const execBindingJSON = `{"targetAgentUID":"target-uid","namespaceUID":"namespace-uid","username":"system:serviceaccount:cetirun-operations:managed-admin","serviceAccountUID":"account-uid","sourceUID":"source-uid"}`

func TestExecAdminBinding(t *testing.T) {
	binding, err := ParseExecAdminBinding(execBindingJSON, "target-uid", "namespace-uid")
	if err != nil {
		t.Fatal(err)
	}
	want := admissionv1.MatchCondition{Name: "exclude-cetiadmin", Expression: `request.userInfo.username != "system:serviceaccount:cetirun-operations:managed-admin" || request.userInfo.uid != "account-uid"`}
	if got := binding.MatchCondition(); got != want {
		t.Fatalf("wrong exact exception: %#v", got)
	}
	raw, err := json.Marshal(binding)
	if err != nil || string(raw) != execBindingJSON {
		t.Fatalf("annotation contract changed: %s, %v", raw, err)
	}
}

func TestExecAdminBindingRejectsUntrustedInputs(t *testing.T) {
	for _, test := range []struct {
		name, raw           string
		agentUID, namespace types.UID
	}{
		{"missing", "", "target-uid", "namespace-uid"},
		{"null", "null", "target-uid", "namespace-uid"},
		{"array", "[]", "target-uid", "namespace-uid"},
		{"unbound agent", execBindingJSON, "", "namespace-uid"},
		{"unbound namespace", execBindingJSON, "target-uid", ""},
		{"recreated agent", execBindingJSON, "new-target-uid", "namespace-uid"},
		{"recreated namespace", execBindingJSON, "target-uid", "new-namespace-uid"},
		{"duplicate", strings.Replace(execBindingJSON, `"sourceUID":`, `"sourceUID":"other","sourceUID":`, 1), "target-uid", "namespace-uid"},
		{"unknown", strings.Replace(execBindingJSON, `"sourceUID":`, `"allow":true,"sourceUID":`, 1), "target-uid", "namespace-uid"},
		{"wrong case", strings.Replace(execBindingJSON, "sourceUID", "SourceUID", 1), "target-uid", "namespace-uid"},
		{"missing source", strings.Replace(execBindingJSON, `,"sourceUID":"source-uid"`, "", 1), "target-uid", "namespace-uid"},
		{"empty account UID", strings.Replace(execBindingJSON, "account-uid", "", 1), "target-uid", "namespace-uid"},
		{"null field", strings.Replace(execBindingJSON, `"source-uid"`, "null", 1), "target-uid", "namespace-uid"},
		{"wrong type", strings.Replace(execBindingJSON, `"source-uid"`, "42", 1), "target-uid", "namespace-uid"},
		{"non SA", strings.Replace(execBindingJSON, "system:serviceaccount:cetirun-operations:managed-admin", "system:masters", 1), "target-uid", "namespace-uid"},
		{"empty SA name", strings.Replace(execBindingJSON, ":managed-admin", ":", 1), "target-uid", "namespace-uid"},
		{"invalid SA namespace", strings.Replace(execBindingJSON, "cetirun-operations", "Not-A-Namespace", 1), "target-uid", "namespace-uid"},
		{"UID injection", strings.Replace(execBindingJSON, "account-uid", `account\" || true || \"`, 1), "target-uid", "namespace-uid"},
		{"extra object", execBindingJSON + "{}", "target-uid", "namespace-uid"},
		{"oversized", strings.Repeat(" ", 4096) + execBindingJSON, "target-uid", "namespace-uid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseExecAdminBinding(test.raw, test.agentUID, test.namespace); err == nil {
				t.Fatal("accepted an invalid or unbound annotation")
			}
		})
	}
}
