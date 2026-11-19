// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"reflect"
	"testing"
)

const testRouterGatewayModel = "cetirun-router-0123456789abcdef"

// routerInput is an Agent on the Gateway whose default is a Team model
// router: the operator lists it with its internal Gateway name as the ID and
// the router's name as the display name.
func routerInput(agentType string) map[string]any {
	input := aiGatewayInput(agentType, "openai")
	input["aiGateway"].(map[string]any)["models"] = []any{
		map[string]any{"id": "gpt-6-sol", "name": "gpt-6-sol", "contextWindow": 272000, "maxTokens": 128000},
		map[string]any{"id": testRouterGatewayModel, "name": "auto", "contextWindow": 128000, "maxTokens": 16384},
	}
	input["secrets"].(map[string]any)["model"] = testRouterGatewayModel
	return input
}

func TestOpenClawListsAModelRouterByItsName(t *testing.T) {
	rendered, err := renderAIGateway(t, routerInput("openclaw"))
	if err != nil {
		t.Fatal(err)
	}
	var router map[string]any
	for _, entry := range openClawProvider(t, rendered)["models"].([]any) {
		if model := entry.(map[string]any); model["id"] == testRouterGatewayModel {
			router = model
		}
	}
	if router == nil || router["name"] != "auto" || router["contextWindow"] != float64(128000) || router["maxTokens"] != float64(16384) {
		t.Fatalf("router entry = %v", router)
	}
	defaults := openClawAgentDefaults(t, rendered)
	if primary := defaults["model"].(map[string]any)["primary"]; primary != "openai/"+testRouterGatewayModel {
		t.Fatalf("primary model = %v, want the router's Gateway name", primary)
	}
	allowed := defaults["modelPolicy"].(map[string]any)["allow"].([]any)
	found := false
	for _, ref := range allowed {
		found = found || ref == "openai/"+testRouterGatewayModel
	}
	if !found {
		t.Fatalf("the router must be allowed: %v", allowed)
	}
}

func TestHermesListsAModelRouterByItsGatewayName(t *testing.T) {
	hermes := renderHermes(t, routerInput("hermes"))
	provider := hermes.config["providers"].(map[string]any)["cetirun-gateway"].(map[string]any)
	models := provider["models"].(map[string]any)
	router, ok := models[testRouterGatewayModel].(map[string]any)
	if !ok || router["context_length"] != float64(128000) || provider["default_model"] != testRouterGatewayModel {
		t.Fatalf("Hermes provider = %v", provider)
	}
}

// visionInput is routerInput with the router marked as accepting images, and
// the other model not.
func visionInput(agentType string, vision ...any) map[string]any {
	input := routerInput(agentType)
	// The operator sends an empty list, not null, when no model accepts images.
	input["aiGateway"].(map[string]any)["visionModels"] = append([]any{}, vision...)
	return input
}

// A model the operator lists as accepting images gets them with a request:
// OpenClaw takes input text and image, Hermes supports_vision. The others
// carry nothing, so OpenClaw keeps its own catalog's value, and Hermes still
// asks the model to describe an image. A release the operator prepares
// renders the configuration with the operator's renderer, standalone Helm
// with the chart's; both read the same template.
func TestAgentsSendImagesToTheModelsThatAcceptThem(t *testing.T) {
	for _, renderer := range []struct {
		name   string
		render func(*testing.T, map[string]any) (map[string]string, error)
	}{{"standalone", renderStandalone}, {"prepared", renderAIGateway}} {
		t.Run(renderer.name, func(t *testing.T) {
			rendered, err := renderer.render(t, visionInput("openclaw", testRouterGatewayModel))
			if err != nil {
				t.Fatal(err)
			}
			inputs := map[string]any{}
			for _, entry := range openClawProvider(t, rendered)["models"].([]any) {
				model := entry.(map[string]any)
				inputs[model["id"].(string)] = model["input"]
			}
			if !reflect.DeepEqual(inputs[testRouterGatewayModel], []any{"text", "image"}) || inputs["gpt-6-sol"] != nil {
				t.Fatalf("OpenClaw inputs = %v", inputs)
			}
			rendered, err = renderer.render(t, visionInput("hermes", testRouterGatewayModel))
			if err != nil {
				t.Fatal(err)
			}
			hermes := parseHermes(t, rendered)
			models := hermes.config["providers"].(map[string]any)["cetirun-gateway"].(map[string]any)["models"].(map[string]any)
			router, other := models[testRouterGatewayModel].(map[string]any), models["gpt-6-sol"].(map[string]any)
			if router["supports_vision"] != true || router["context_length"] != float64(128000) {
				t.Fatalf("Hermes router = %v", router)
			}
			if _, set := other["supports_vision"]; set {
				t.Fatalf("Hermes model = %v", other)
			}
			// No model accepts images: the configurations are those of a chart
			// without the value.
			for _, agentType := range []string{"openclaw", "hermes"} {
				with, err := renderer.render(t, visionInput(agentType))
				if err != nil {
					t.Fatal(err)
				}
				without := routerInput(agentType)
				delete(without["aiGateway"].(map[string]any), "visionModels")
				before, err := renderer.render(t, without)
				if err != nil {
					t.Fatal(err)
				}
				if with["cetirun/templates/configmap.yaml"] != before["cetirun/templates/configmap.yaml"] {
					t.Fatalf("%s: an empty list changed the configuration", agentType)
				}
			}
		})
	}
}

// The chart refuses what the operator never sends.
func TestVisionModelsMustBeModelIDs(t *testing.T) {
	for name, value := range map[string]any{
		"an empty ID": []any{""},
		"not a list":  testRouterGatewayModel,
		"not strings": []any{7},
	} {
		input := routerInput("openclaw")
		input["aiGateway"].(map[string]any)["visionModels"] = value
		if _, err := renderAIGateway(t, input); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
