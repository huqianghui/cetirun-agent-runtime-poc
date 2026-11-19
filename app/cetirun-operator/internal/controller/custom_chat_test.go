// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Azure/CetiRun/app/internal/chataccess"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCustomChatCallerDoesNotReplaceTheAgent(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	agent := s.current()
	agent.Spec.Agent.Port = ptr.To[int32](8080)
	if err := s.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	s.observeApplied()
	before := s.helm.lastValues["runtime"].(map[string]any)["workloadHash"]
	approvals := s.provider.store.approvals
	s.reconciler.ChatAccess = chataccess.Config{Enabled: true, Callers: []chataccess.Caller{{Namespace: "test-platform", PodLabels: map[string]string{"app": "portal"}}}}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if s.helm.lastValues["runtime"].(map[string]any)["workloadHash"] != before || s.provider.store.approvals != approvals {
		t.Fatal("Chat access entered writer handoff")
	}
	if !meta.IsStatusConditionTrue(s.current().Status.Conditions, chataccess.Condition) {
		t.Fatal("Chat configuration was not observed")
	}
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(chataccess.RouteGVK)
	key := client.ObjectKey{Namespace: "cetirun-" + agent.Name, Name: chataccess.RouteName(agent.Name)}
	if err := s.client.Get(context.Background(), key, route); err != nil {
		t.Fatal(err)
	}
	route.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)["retries"] = map[string]any{"attempts": int64(2)}
	if err := s.client.Update(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Get(context.Background(), key, route); err != nil {
		t.Fatal(err)
	}
	expected := chataccess.PrivateRoute(agent.Name, string(agent.UID), key.Namespace, 8080, s.reconciler.ChatAccess.Callers)
	if !chataccess.RouteSpecsEqual(route.Object["spec"], expected.Object["spec"]) {
		t.Fatal("route drift did not recover the complete private route")
	}
	s.reconciler.ChatAccess.Enabled = false
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if s.helm.lastValues["runtime"].(map[string]any)["workloadHash"] != before || s.provider.store.approvals != approvals {
		t.Fatal("Chat removal replaced Agent")
	}
	if meta.IsStatusConditionTrue(s.current().Status.Conditions, chataccess.Condition) {
		t.Fatal("disabled Chat remained configured")
	}
	if err := s.client.Get(context.Background(), key, route); !apierrors.IsNotFound(err) {
		t.Fatal("disabled Chat retained its private route")
	}
}

func TestCustomChatDoesNotAlterAnOlderChart(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.helm.disableChatContract = true
	agent := s.current()
	agent.Spec.Agent.Port = ptr.To[int32](8080)
	if err := s.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	s.reconciler.ChatAccess = chataccess.Config{Enabled: true, Callers: []chataccess.Caller{{Namespace: "test-platform", PodLabels: map[string]string{"app": "portal"}}}}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(s.current().Status.Conditions, chataccess.Condition)
	if condition == nil || condition.Reason != "ChatContractUnsupported" {
		t.Fatal("old Chart was presented as compatible")
	}
	bindings := s.helm.lastValues["runtime"].(map[string]any)["bindings"].(map[string]any)
	got, _ := json.Marshal(bindings["network"].(map[string]any)["callers"])
	want, _ := json.Marshal(s.provider.bindings.Network.Callers)
	if string(got) != string(want) {
		t.Fatalf("old Chart callers changed: %s != %s", got, want)
	}
}
