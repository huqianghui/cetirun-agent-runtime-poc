// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package aigateway

import (
	"context"
	"maps"
	"net/http"
	"testing"
)

func TestKeyAliasesAreCreatedReadAndReplaced(t *testing.T) {
	client, requests := testServer(t, func(req recordedRequest) (int, string) {
		if req.method == http.MethodGet {
			return 200, `{"key":"abc","info":{"key_alias":"cetirun-a","models":["cetirun-copilot-uid"],"aliases":{"copilot/gpt-5.5":"gpt-5.5","odd":7}}}`
		}
		return 200, `{}`
	})
	ctx := context.Background()
	aliases := map[string]string{"copilot/gpt-5.5": "gpt-5.5"}
	if err := client.GenerateKey(ctx, GenerateRequest{Key: "sk-new", Alias: "cetirun-a", Models: []string{"cetirun-copilot-uid"}, Aliases: aliases}); err != nil {
		t.Fatal(err)
	}
	if got := (*requests)[0].body["aliases"]; !maps.Equal(stringMap(got), aliases) {
		t.Fatalf("the key is created with its aliases, got %v", got)
	}
	if err := client.GenerateKey(ctx, GenerateRequest{Key: "sk-other", Alias: "cetirun-b", Models: []string{NoModels}}); err != nil {
		t.Fatal(err)
	}
	if _, set := (*requests)[1].body["aliases"]; set {
		t.Fatal("a key without aliases is created without the field")
	}
	key, err := client.GetKey(ctx, "abc")
	if err != nil || key == nil || !maps.Equal(key.Aliases, aliases) {
		t.Fatalf("string aliases are read back, others ignored: %+v %v", key, err)
	}
	if err := client.UpdateKeyAliases(ctx, "abc", nil); err != nil {
		t.Fatal(err)
	}
	last := (*requests)[len(*requests)-1]
	if last.path != "/key/update" || last.body["key"] != "abc" || len(stringMap(last.body["aliases"])) != 0 || last.body["aliases"] == nil {
		t.Fatalf("removing every alias sends an empty map: %+v", last)
	}
	if len(last.body) != 2 {
		t.Fatalf("an alias update changes nothing else: %v", last.body)
	}
}

func stringMap(value any) map[string]string {
	entries, _ := value.(map[string]any)
	out := map[string]string{}
	for name, target := range entries {
		if text, ok := target.(string); ok {
			out[name] = text
		}
	}
	return out
}

func TestListModelsReadsRealModelAndRoutingFields(t *testing.T) {
	client, _ := testServer(t, func(recordedRequest) (int, string) {
		return 200, `{"data":[
			{"model_name":"gpt-x","litellm_params":{"model":"azure/gpt-4.1","api_key":"sk-masked","order":1,"weight":2.5,"rpm":60,"tpm":"9"},
			 "model_info":{"id":"a","base_model":"azure/gpt-4.1","access_groups":["cetirun-shared"]}},
			{"model_name":"gpt-x","litellm_params":{"model":"openai/gpt-4.1","order":0,"weight":-1,"rpm":1.5},"model_info":{"id":"b","base_model":null}}]}`
	})
	deployments, err := client.ListModels(context.Background())
	if err != nil || len(deployments) != 2 {
		t.Fatalf("ListModels = %v, %v", deployments, err)
	}
	first, second := deployments[0], deployments[1]
	if first.Model != "azure/gpt-4.1" || first.BaseModel != "azure/gpt-4.1" || first.Routing.String() != "order=1,weight=2.5,rpm=60" {
		t.Fatalf("first deployment %+v routing %s", first, first.Routing)
	}
	// order 0 is LiteLLM's lowest order and kept; a negative weight and a
	// fractional rpm are ignored (review).
	if second.Model != "openai/gpt-4.1" || second.BaseModel != "" || second.Routing.String() != "order=0" {
		t.Fatalf("fields LiteLLM's routing ignores count as unset: %+v routing %s", second, second.Routing)
	}
}

func TestAddModelSendsRoutingAndCooldown(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	order, weight := int64(2), 3.0
	if err := client.AddModel(context.Background(), ModelRequest{
		ID: "route-a", ModelName: "gpt-x", Model: "litellm_proxy/gpt-4.1", APIBase: "http://proxy.example.com:4000",
		AccessGroups: []string{"cetirun-copilot-uid"}, Routing: Routing{Order: &order, Weight: &weight},
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.AddModel(context.Background(), ModelRequest{
		ID: "route-b", ModelName: "gpt-4.1", Model: "litellm_proxy/gpt-4.1", APIBase: "http://proxy.example.com:4000", NoCooldown: true,
		ProbeWithoutRetries: true, NoBackgroundHealthCheck: true,
	}); err != nil {
		t.Fatal(err)
	}
	merged := (*requests)[0].body["litellm_params"].(map[string]any)
	if merged["order"] != float64(2) || merged["weight"] != float64(3) || merged["cooldown_time"] != nil {
		t.Fatalf("a route sharing a model's name copies its routing and keeps the default cooldown: %v", merged)
	}
	if info := (*requests)[0].body["model_info"].(map[string]any); info["health_check_params"] != nil {
		t.Fatalf("health check parameters are sent only when asked for: %v", info)
	}
	if _, set := (*requests)[0].body["model_info"].(map[string]any)["disable_background_health_check"]; set {
		t.Fatalf("a route is left out of the background health checks only when asked for: %v", (*requests)[0].body["model_info"])
	}
	own := (*requests)[1].body["litellm_params"].(map[string]any)
	if cooldown, set := own["cooldown_time"]; !set || cooldown != float64(0) || own["order"] != nil {
		t.Fatalf("a route of its own is never cooled down: %v", own)
	}
	probe, _ := (*requests)[1].body["model_info"].(map[string]any)["health_check_params"].(map[string]any)
	if retries, set := probe["max_retries"]; !set || retries != float64(0) {
		t.Fatalf("a route probed without retries carries max_retries 0 in its health check parameters: %v", probe)
	}
	if info := (*requests)[1].body["model_info"].(map[string]any); info["disable_background_health_check"] != true {
		t.Fatalf("a route left out of the background health checks says so in its model info: %v", info)
	}
}
