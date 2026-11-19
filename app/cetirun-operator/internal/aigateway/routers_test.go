// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package aigateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestAddRouterSendsTheComplexityRouter(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/model/new" || r.Header.Get("Authorization") != "Bearer sk-master" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"model_id":"x"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "sk-master", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := RouterRequest{
		ID: "cetirun-router-0123456789abcdef-01234567", ModelName: "cetirun-router-0123456789abcdef",
		AccessGroups: []string{"cetirun-router-0123456789abcdef"}, DefaultModel: "medium-model",
		Config:            map[string]any{"classifier_type": "heuristic", "tiers": map[string]any{"SIMPLE": "small-model"}},
		InputCostPerToken: 0.00001, OutputCostPerToken: 0.00005,
	}
	if err := client.AddRouter(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	params := body["litellm_params"].(map[string]any)
	info := body["model_info"].(map[string]any)
	if body["model_name"] != request.ModelName || params["model"] != "auto_router/complexity_router" ||
		params["complexity_router_default_model"] != "medium-model" || params["input_cost_per_token"] != 0.00001 ||
		params["output_cost_per_token"] != 0.00005 || params["complexity_router_config"].(map[string]any)["classifier_type"] != "heuristic" ||
		info["id"] != request.ID || info["access_groups"].([]any)[0] != request.ModelName {
		t.Fatalf("body = %v", body)
	}
	for _, field := range []string{"api_key", "api_base"} {
		if _, ok := params[field]; ok {
			t.Fatalf("a router has no endpoint; %s must not be sent", field)
		}
	}
	if _, ok := info["team_id"]; ok {
		t.Fatal("team_id makes a licensed team model")
	}
}

func TestAddRouterRejectsIncompleteRequests(t *testing.T) {
	client, err := New("http://gateway.example.com", "sk-master", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []RouterRequest{
		{ModelName: "m", Config: map[string]any{"a": 1}, InputCostPerToken: 1, OutputCostPerToken: 1},
		{ID: "i", ModelName: "m", InputCostPerToken: 1, OutputCostPerToken: 1},
		{ID: "i", ModelName: "m", Config: map[string]any{"a": 1}},
	} {
		if err := client.AddRouter(context.Background(), request); err == nil {
			t.Fatalf("request %+v was accepted", request)
		}
	}
}

func TestAddRouterReportsARefusedConfiguration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"complexity_router_config is invalid at tiers: bad"}}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "sk-master", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.AddRouter(context.Background(), RouterRequest{ID: "i", ModelName: "m", Config: map[string]any{"a": 1}, InputCostPerToken: 1, OutputCostPerToken: 1})
	if err == nil || !strings.Contains(err.Error(), "refused the router configuration") || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("err = %v", err)
	}
}

func routerFrom(t *testing.T, params string) *Router {
	t.Helper()
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(params), &parsed); err != nil {
		t.Fatal(err)
	}
	return parseRouter(parsed)
}

func tier(name, model string) RouterModel {
	return RouterModel{Use: RouterTier, Tier: name, Name: model}
}

// The litellm_params below are what LiteLLM 1.105.1's /model/info shows for
// a router of the Gateway chart: the chart's own settings included, prices,
// no credentials.
func TestParseRouterReadsEveryModelAComplexityRouterUses(t *testing.T) {
	router := routerFrom(t, `{
		"model": "auto_router/complexity_router", "input_cost_per_token": 0.00001, "output_cost_per_token": 0.00004,
		"complexity_router_default_model": "medium-model",
		"complexity_router_config": {
			"classifier_type": "llm", "classifier_llm_config": {"model": "small-model", "timeout_ms": 2000},
			"tiers": {"REASONING": ["large-model", {"model_name": "deep-model", "litellm_params": {"reasoning_effort": "high"}}],
				"SIMPLE": "small-model", "MEDIUM": "medium-model", "COMPLEX": "large-model"},
			"reminder_markers": [{"open": "<system-reminder>", "close": "</system-reminder>"}],
			"context_compaction": false, "max_tokens_from_tier_model": false, "enable_context_window_escalation": true,
			"embedding_model": "None", "token_thresholds": {"simple": 15, "complex": 400}
		}}`)
	want := []RouterModel{
		tier("SIMPLE", "small-model"), tier("MEDIUM", "medium-model"), tier("COMPLEX", "large-model"),
		tier("REASONING", "large-model"), tier("REASONING", "deep-model"),
		{Use: RouterDefault, Name: "medium-model"}, {Use: RouterClassifier, Name: "small-model"},
	}
	if router == nil || router.Unverifiable != "" || !slices.Equal(router.Models, want) {
		t.Fatalf("router = %+v, want models %+v", router, want)
	}
}

func TestParseRouterReadsTheOptionalModels(t *testing.T) {
	router := routerFrom(t, `{"model": "auto_router/complexity_router", "complexity_router_config": {
		"tiers": {"SIMPLE": ["small-model"], "Premium": "large-model"},
		"tier_model_configs": {"SIMPLE": [{"model_name": "small-model", "litellm_params": {"temperature": 0}}]},
		"default_model": "fallback-model", "context_compaction": {"model": "summary-model", "max_tokens": 4096},
		"semantic_keyword_matching": true, "embedding_model": "embedding-model",
		"classifier_type": "jev", "jev_classifier_config": {"deployment_name": "decision-model"}}}`)
	want := []RouterModel{
		tier("SIMPLE", "small-model"), tier("Premium", "large-model"),
		{Use: RouterDefault, Name: "fallback-model"}, {Use: RouterCompactor, Name: "summary-model"},
		{Use: RouterClassifier, Name: "embedding-model"}, {Use: RouterClassifier, Name: "decision-model"},
	}
	if router == nil || router.Unverifiable != "" || !slices.Equal(router.Models, want) {
		t.Fatalf("router = %+v, want models %+v", router, want)
	}
}

// What the operator cannot read makes the router unverifiable, with the
// reason; the models it could read are still listed.
func TestParseRouterReportsWhatItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct {
		params string
		reason string
	}{
		"an unknown setting": {`{"model": "auto_router/complexity_router", "complexity_router_config": {"tiers": {"SIMPLE": "small-model"},
			"future_model_pool": ["other-model"]}}`, "sets future_model_pool, a setting the operator does not know"},
		"a tier the Gateway masks": {`{"model": "auto_router/complexity_router", "complexity_router_config": {
			"tiers": {"SIMPLE": "small-model", "private_tier": "sens****odel"}, "tier_definitions": [], "classifier_type": "llm"}}`,
			"masks the models of tier private_tier"},
		"a masked tier list": {`{"model": "auto_router/complexity_router", "complexity_router_config": {
			"tiers": {"SIMPLE": "small-model", "ACCESS": ["****"]}}}`, "masks the models of tier ACCESS"},
		"a classifier plugin": {`{"model": "auto_router/complexity_router", "complexity_router_config": {"tiers": {"SIMPLE": "small-model"},
			"classifier_type": "custom", "classifier_plugin": "plugins.classify"}}`, "classifier plugin"},
		"routing plugins": {`{"model": "auto_router/complexity_router", "complexity_router_config": {"tiers": {"SIMPLE": "small-model"},
			"plugins": ["plugins.narrow"]}}`, "routing plugins"},
		"an external jev classifier": {`{"model": "auto_router/complexity_router", "complexity_router_config": {"tiers": {"SIMPLE": "small-model"},
			"classifier_type": "jev", "jev_classifier_config": {"model": "jev-latest", "api_key": "sk-1****cdef"}}}`,
			"jev classifier calls a service outside the AI Gateway"},
		"a tier entry that sets its model": {`{"model": "auto_router/complexity_router", "complexity_router_config": {
			"tiers": {"SIMPLE": [{"model_name": "small-model", "litellm_params": {"model": "azure/other"}}]}}}`,
			"tier SIMPLE sets model for small-model"},
		"a tier that is not a model": {`{"model": "auto_router/complexity_router", "complexity_router_config": {"tiers": {"SIMPLE": 7}}}`,
			"tier SIMPLE is neither a model name nor a list of them"},
		"a classifier that is not a model": {`{"model": "auto_router/complexity_router", "complexity_router_config": {
			"tiers": {"SIMPLE": "small-model"}, "classifier_llm_config": {"model": ["a", "b"]}}}`, "classifier_llm_config.model is not a model name"},
		"no tiers": {`{"model": "auto_router/complexity_router", "complexity_router_default_model": "small-model",
			"complexity_router_config": {"classifier_type": "heuristic"}}`, "names no tiers"},
		"no configuration": {`{"model": "auto_router/complexity_router", "complexity_router_default_model": "small-model"}`,
			"has no complexity_router_config"},
		"another kind of auto router": {`{"model": "auto_router/quality_router", "quality_router_config": {"available_models": ["a"]}}`,
			"auto_router/quality_router router"},
	} {
		t.Run(name, func(t *testing.T) {
			router := routerFrom(t, tc.params)
			if router == nil || !strings.Contains(router.Unverifiable, tc.reason) {
				t.Fatalf("router = %+v, want a reason with %q", router, tc.reason)
			}
		})
	}
	// A known model is still read beside what cannot be.
	router := routerFrom(t, `{"model": "auto_router/complexity_router", "complexity_router_config": {"tiers": {"SIMPLE": "small-model"}, "x_extra": 1}}`)
	if !slices.Equal(router.Models, []RouterModel{tier("SIMPLE", "small-model")}) {
		t.Fatalf("models = %+v", router.Models)
	}
}

func TestParseRouterLeavesOtherModelsAlone(t *testing.T) {
	// ComplexityRouterConfig in LiteLLM 1.105.1 has 67 fields.
	if len(complexityRouterSettings) != 67 {
		t.Fatalf("%d known settings", len(complexityRouterSettings))
	}
	for _, params := range []string{`{"model": "azure/gpt-5"}`, `{"model": "openai/auto_router/complexity_router"}`, `{}`} {
		if router := routerFrom(t, params); router != nil {
			t.Fatalf("%s: router = %+v", params, router)
		}
	}
	// LiteLLM takes any auto_router/complexity_router... model for a
	// complexity router.
	if router := routerFrom(t, `{"model": "auto_router/complexity_router_v2", "complexity_router_config": {"tiers": {"SIMPLE": "small-model"}}}`); router == nil || router.Unverifiable != "" {
		t.Fatalf("router = %+v", router)
	}
}

// LiteLLM 1.105.1 masks a value whose key has a sensitive part, unless a part
// is "cost".
func TestLiteLLMMasksTheKeysItsMaskerMasks(t *testing.T) {
	for key, want := range map[string]bool{
		"api_key": true, "SECRET": true, "private-tier": true, "access": true, "input_cost_per_token": false,
		"max_tokens": false, "token_thresholds": true, "SIMPLE": false, "keywords": false, "model_name": false,
	} {
		if got := liteLLMMasks(key); got != want {
			t.Errorf("liteLLMMasks(%q) = %v, want %v", key, got, want)
		}
	}
}

// ListModels gives each router deployment what it sends requests to.
func TestListModelsReadsRouters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"model_name":"auto","litellm_params":{"model":"auto_router/complexity_router","input_cost_per_token":0.00001,
				"output_cost_per_token":0.00004,"complexity_router_config":{"tiers":{"SIMPLE":"small-model","MEDIUM":"medium-model"}}},
				"model_info":{"id":"cetirun-shared-auto","access_groups":["cetirun-model-auto"],"db_model":false}},
			{"model_name":"small-model","litellm_params":{"model":"azure/small"},"model_info":{"id":"cetirun-shared-small","db_model":false}}
		]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "sk-master", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 2 || deployments[0].Router == nil || deployments[1].Router != nil ||
		!slices.Equal(deployments[0].Router.Models, []RouterModel{tier("SIMPLE", "small-model"), tier("MEDIUM", "medium-model")}) {
		t.Fatalf("deployments = %+v", deployments)
	}
}
