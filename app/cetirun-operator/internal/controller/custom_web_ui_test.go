// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestCustomWebUIRoutePreservesPrefixAndExcludesDesktop(t *testing.T) {
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "editor"}, Spec: api.AgentSpec{Agent: api.HarnessSpec{
		Type: api.AgentTypeCustom, Port: ptr.To[int32](8088), WebUI: true,
	}}}
	vs := buildVirtualService(agent, "example.com", "", customAccess{})
	found := false
	for _, route := range vs.Spec.Http {
		for _, match := range route.Match {
			if match.Uri.GetPrefix() == "/editor/desktop/" {
				t.Fatal("disabled desktop has a route")
			}
			if match.Uri.GetPrefix() == "/ui/editor/" {
				found = true
				if route.Rewrite.Uri != "/ui/editor/" || route.Route[0].Destination.Port.Number != 8088 || route.Retries != nil || route.Timeout != nil {
					t.Fatal("browser prefix or long-lived WebSocket contract changed")
				}
			}
		}
	}
	if !found {
		t.Fatal("browser route missing")
	}
	if runtimeRequest(agent, "")["webUI"] != true {
		t.Fatal("Web UI omitted from runtime plan")
	}
	agent.UID = "11111111-1111-1111-1111-111111111111"
	agent.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "registry-editor"}
	request := runtimeRequest(agent, "")
	if request["webUI"] != true || request["port"] != int32(8088) || request["pullSecretName"] != "registry-pull-11111111111111111111111111111111" {
		t.Fatal("Web UI and Registry bindings must be preserved together")
	}
	agent.Spec.Agent.WebUI = false
	vs = buildVirtualService(agent, "example.com", "", customAccess{})
	for _, route := range vs.Spec.Http {
		for _, destination := range route.Route {
			if destination.Destination.Port.Number == 8088 {
				t.Fatal("disabling Web UI exposed the private Chat port")
			}
		}
	}
}
