// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package custom

import (
	"reflect"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"k8s.io/utils/ptr"
)

func TestWebUIIsNotChat(t *testing.T) {
	request := agents.Request{Type: "custom", Image: "registry.example.com/editor:1", Port: ptr.To[int32](8080), WebUI: true}
	agent, err := agents.ResolveCustom(request)
	if err != nil {
		t.Fatal(err)
	}
	bindings := agents.Bindings{ContextRevision: "context-1", Model: agents.ModelBinding{BaseURL: "https://gateway.example.com/v1", CredentialSlot: "model", Revision: "gateway-key"}, WebBasePath: "/ui/editor/", WebInstance: "uid-1"}
	c, err := (Harness{}).Build(agent, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP == nil || c.HTTP.HealthPath != "/healthz" || c.HTTP.ChatPath != "" || c.HTTP.ModelsPath != "" ||
		!reflect.DeepEqual(c.Ports, []agents.PortBinding{{Name: "http", Number: 8080}}) {
		t.Fatalf("wrong browser contract: %#v", c)
	}
	if !reflect.DeepEqual(c.Environment, []agents.EnvironmentValue{
		{Name: "HOME", Value: "/home/vibe"}, {Name: "OPENAI_BASE_URL", Value: "https://gateway.example.com/v1"}, {Name: "OPENAI_API_KEY", CredentialSlot: "model"}, {Name: "PORT", Value: "8080"},
		{Name: "CETIRUN_WEB_BASE_PATH", Value: "/ui/editor/"}, {Name: "CETIRUN_WEB_INSTANCE", Value: "uid-1"},
	}) {
		t.Fatalf("browser port or platform environment changed: %#v", c.Environment)
	}
	request.Port = nil
	if _, err := agents.ResolveCustom(request); err == nil {
		t.Fatal("Web UI without a port accepted")
	}
	bindings.WebBasePath = "/ui/editor/../other/"
	if _, err := (Harness{}).Build(agent, bindings); err == nil {
		t.Fatal("unsafe browser prefix accepted")
	}
}

func TestWebUIFlagSelectsHTTPContract(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		request := agents.Request{Type: "custom", Image: "registry.example.com/editor:1", Port: ptr.To[int32](8080), WebUI: enabled}
		agent, err := agents.ResolveCustom(request)
		if err != nil {
			t.Fatal(err)
		}
		bindings := agents.Bindings{ContextRevision: "context-1", Model: agents.ModelBinding{BaseURL: "https://gateway.example.com/v1", CredentialSlot: "model", Revision: "gateway-key"}, WebBasePath: "/ui/editor/", WebInstance: "uid-1"}
		c, err := (Harness{}).Build(agent, bindings)
		if err != nil || c.Agent.WebUI != enabled || (c.HTTP.ChatPath == "") != enabled || (c.HTTP.ModelsPath == "") != enabled {
			t.Fatalf("webUI=%v selected the wrong protocol: %#v, %v", enabled, c, err)
		}
		request.Type = "hermes"
		_, err = agents.Resolve(request, agents.Profile{ID: "test", Image: request.Image, Port: 8080})
		if (err != nil) != enabled {
			t.Fatalf("built-in webUI=%v, error=%v", enabled, err)
		}
	}
}
