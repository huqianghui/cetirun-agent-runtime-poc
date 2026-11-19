// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestCustomWebUIAccessAndSuspension(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.reconciler.DomainSuffix = "platform.example.com"
	current := s.current()
	current.Spec.Agent.Port = ptr.To[int32](8088)
	current.Spec.Agent.WebUI = true
	current.Spec.Helm.Values = map[string]runtime.RawExtension{
		"desktop": {Raw: []byte(`{"enabled":false}`)},
		"ttyd":    {Raw: []byte(`{"enabled":false}`)},
	}
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if got := s.current().Status.AccessURL; got != "https://platform.example.com/ui/completion/" {
		t.Fatalf("wrong Web UI URL: %q", got)
	}
	route := &istiov1beta1.VirtualService{}
	key := types.NamespacedName{Namespace: "cetirun-completion", Name: "cetirun-completion"}
	if err := s.client.Get(context.Background(), key, route); err != nil {
		t.Fatal(err)
	}
	for _, rule := range route.Spec.Http {
		for _, destination := range rule.Route {
			if destination.Destination.Port.Number != 8088 {
				t.Fatal("Web UI exposed an unrelated service")
			}
		}
	}
	current = s.current()
	current.Spec.Suspended = true
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Get(context.Background(), key, route); !apierrors.IsNotFound(err) {
		t.Fatalf("suspension retained Web UI route: %v", err)
	}
	if s.current().Status.AccessURL != "" {
		t.Fatal("suspension retained Web UI URL")
	}
}

func TestCustomDesktopAccessAndSuspension(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.reconciler.DomainSuffix = "platform.example.com"
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if got := s.current().Status.AccessURL; got != "" {
		t.Fatalf("Custom repurposed the native UI access URL: %q", got)
	}
	route := &istiov1beta1.VirtualService{}
	key := types.NamespacedName{Namespace: "cetirun-completion", Name: "cetirun-completion"}
	if err := s.client.Get(context.Background(), key, route); err != nil {
		t.Fatal(err)
	}
	if len(route.OwnerReferences) != 1 || route.OwnerReferences[0].UID != s.agent.UID {
		t.Fatal("desktop route lost Agent ownership")
	}
	for _, rule := range route.Spec.Http {
		for _, destination := range rule.Route {
			if destination.Destination.Port.Number != 6080 {
				t.Fatal("Custom published an Agent, native auth or ttyd endpoint")
			}
		}
	}
	current := s.current()
	current.Spec.Suspended = true
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Get(context.Background(), key, route); !apierrors.IsNotFound(err) {
		t.Fatalf("suspension retained desktop route: %v", err)
	}
}

func TestCustomDesktopCanBeDisabledWithoutLosingControls(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	current := s.current()
	current.Spec.Helm.Values["desktop"] = runtime.RawExtension{Raw: []byte(`{"enabled":false}`)}
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if s.current().Status.AccessURL != "" {
		t.Fatal("disabled desktop received an access URL")
	}
	if s.helm.lastValues["execRestriction"].(map[string]any)["enabled"] != true {
		t.Fatal("disabling desktop disabled a platform security control")
	}
}

func TestCustomWebUIAndTerminalCanBeDisabledIndependently(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, disable := range []string{"webUI", "terminal"} {
		t.Run(disable, func(t *testing.T) {
			s := newCompletionScenario(t)
			enableTerminal(t, s, false)
			current := s.current()
			current.Spec.Agent.Port = ptr.To[int32](8088)
			current.Spec.Agent.WebUI = true
			if err := s.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			if s.current().Status.AccessURL == "" || !apimeta.IsStatusConditionTrue(s.current().Status.Conditions, api.ConditionTerminalConfigured) {
				t.Fatal("Web UI and terminal were not configured together")
			}
			current = s.current()
			if disable == "webUI" {
				current.Spec.Agent.WebUI = false
			} else {
				current.Spec.Helm.Values["ttyd"] = runtime.RawExtension{Raw: []byte(`{"enabled":false}`)}
			}
			if err := s.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			route := &istiov1beta1.VirtualService{}
			if err := s.client.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-completion", Name: "cetirun-completion"}, route); err != nil {
				t.Fatal(err)
			}
			foundUI, foundTerminal := false, false
			for _, rule := range route.Spec.Http {
				for _, match := range rule.Match {
					foundUI = foundUI || match.Uri.GetPrefix() == "/ui/completion/"
					foundTerminal = foundTerminal || match.Uri.GetPrefix() == "/completion/term/"
				}
			}
			if foundUI != (disable != "webUI") || foundTerminal != (disable != "terminal") ||
				(s.current().Status.AccessURL != "") != foundUI ||
				apimeta.IsStatusConditionTrue(s.current().Status.Conditions, api.ConditionTerminalConfigured) != foundTerminal {
				t.Fatal("disabling one service changed the other service or retained its URL")
			}
		})
	}
}
