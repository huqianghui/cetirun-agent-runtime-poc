// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package aigateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

type recordedRequest struct {
	method, path, query, auth string
	body                      map[string]any
}

func testServer(t *testing.T, handler func(recordedRequest) (int, string)) (*Client, *[]recordedRequest) {
	t.Helper()
	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.body); err != nil {
				t.Fatalf("request body is not JSON: %v", err)
			}
		}
		requests = append(requests, req)
		status, body := handler(req)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL+"/", "sk-master", nil)
	if err != nil {
		t.Fatal(err)
	}
	return client, &requests
}

func TestNewKeyValueHasGatewayFormAndIsValid(t *testing.T) {
	a, err := NewKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewKeyValue()
	if !ValidKeyValue(a) || !ValidKeyValue(b) || a == b {
		t.Fatalf("key values must be valid and distinct")
	}
	for _, invalid := range []string{"", "sk-", "nosk" + a[3:], a + "x", "sk-" + strings.Repeat("!", 43)} {
		if ValidKeyValue(invalid) {
			t.Errorf("ValidKeyValue(%q) = true", invalid)
		}
	}
}

func TestTokenIsSHA256Hex(t *testing.T) {
	// printf %s sk-test | sha256sum
	if got := Token("sk-test"); got != "f3abf2a6cc4f00987743db5f544ba345b4899ae31f326d8ee9c4816de153c9e0" {
		t.Fatalf("Token = %s", got)
	}
}

func TestGetKeyUsesTokenAndMasterKey(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) {
		return 200, `{"key":"abc","info":{"key_alias":"cetirun-a","models":["cetirun-shared"],"max_budget":10,"budget_duration":"1mo","blocked":null,"team_id":"cetirun-team-a","user_id":"alice"}}`
	})
	key, err := client.GetKey(context.Background(), "abc")
	if err != nil || key == nil {
		t.Fatalf("GetKey = %v, %v", key, err)
	}
	if key.Alias != "cetirun-a" || len(key.Models) != 1 || key.Models[0] != "cetirun-shared" || key.MaxBudget == nil || *key.MaxBudget != 10 || key.Blocked ||
		key.TeamID != "cetirun-team-a" || key.UserID != "alice" {
		t.Fatalf("unexpected key %+v", key)
	}
	r := (*requests)[0]
	if r.method != http.MethodGet || r.path != "/key/info" || r.query != "key=abc" || r.auth != "Bearer sk-master" {
		t.Fatalf("unexpected request %+v", r)
	}
}

func TestGetKeyReadsBudgetObservations(t *testing.T) {
	client, _ := testServer(t, func(recordedRequest) (int, string) {
		return 200, `{"info":{"key_alias":"cetirun-a","models":["model-group"],"max_budget":10,"budget_duration":"1mo","spend":1.25,"budget_reset_at":"2026-02-01T00:00:00Z","blocked":false}}`
	})
	key, err := client.GetKey(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if key.Spend == nil || *key.Spend != 1.25 || key.BudgetResetAt == nil || key.BudgetResetAt.UTC().Format(time.RFC3339) != "2026-02-01T00:00:00Z" {
		t.Fatalf("GetKey observations = %+v", key)
	}
}

func TestGetKeyNotFoundIsNil(t *testing.T) {
	client, _ := testServer(t, func(recordedRequest) (int, string) {
		return 404, `{"error":{"message":"Key not found in database"}}`
	})
	key, err := client.GetKey(context.Background(), "abc")
	if key != nil || err != nil {
		t.Fatalf("GetKey = %v, %v; want nil, nil", key, err)
	}
}

func TestGetKeyDistinguishesDeletionArchives(t *testing.T) {
	for _, test := range []struct {
		name, deletedAt string
		absent, invalid bool
	}{
		{"active", "null", false, false},
		{"archived", `"2026-01-02T03:04:05.123456Z"`, true, false},
		{"invalid timestamp", `"not-a-time"`, false, true},
		{"invalid type", "123", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, requests := testServer(t, func(recordedRequest) (int, string) {
				return http.StatusOK, `{"info":{"key_alias":"cetirun-example","models":["example-model"],"spend":1.25,"blocked":false,"deleted_at":` + test.deletedAt + `}}`
			})
			key, err := client.GetKey(context.Background(), "example-token-hash")
			if test.invalid {
				if err == nil || key != nil {
					t.Fatalf("malformed deletion metadata must fail, got key=%v error=%v", key, err)
				}
			} else if err != nil || (key == nil) != test.absent {
				t.Fatalf("GetKey archive handling: key=%v error=%v want absent=%t", key, err, test.absent)
			}
			if len(*requests) != 1 || (*requests)[0].method != http.MethodGet {
				t.Fatal("reading key state must not delete an archive or change a key")
			}
		})
	}
}

func TestGenerateKeySendsRestrictedKey(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	err := client.GenerateKey(context.Background(), GenerateRequest{
		Key: "sk-value", Alias: "cetirun-a", Models: []string{"cetirun-shared"}, MaxBudget: 10,
		BudgetDuration: "1mo", Metadata: map[string]string{"cetirun_agent": "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := (*requests)[0].body
	if body["key"] != "sk-value" || body["key_alias"] != "cetirun-a" || body["max_budget"] != float64(10) || body["budget_duration"] != "1mo" {
		t.Fatalf("unexpected body %v", body)
	}
	// Inference routes, and the model lookup by name; no management route.
	if routes, _ := body["allowed_routes"].([]any); len(routes) != 3 || routes[0] != LLMAPIRoutes || routes[1] != "/v1/models" || routes[2] != "/models" {
		t.Fatalf("allowed_routes = %v", body["allowed_routes"])
	}
}

func TestKeyRoutesAreReadAndUpdated(t *testing.T) {
	client, requests := testServer(t, func(r recordedRequest) (int, string) {
		if r.method == http.MethodGet {
			return 200, `{"info":{"key_alias":"a","models":["m"],"allowed_routes":["llm_api_routes"]}}`
		}
		return 200, `{}`
	})
	key, err := client.GetKey(context.Background(), "abc")
	if err != nil || !slices.Equal(key.AllowedRoutes, []string{LLMAPIRoutes}) || KeyRoutesCurrent(key.AllowedRoutes) {
		t.Fatalf("GetKey = %+v, %v", key, err)
	}
	if err := client.UpdateKeyRoutes(context.Background(), "abc", KeyRoutes); err != nil {
		t.Fatal(err)
	}
	body := (*requests)[1].body
	if r := (*requests)[1]; r.path != "/key/update" || body["key"] != "abc" || len(body) != 2 {
		t.Fatalf("update request %+v", r)
	}
	if err := client.UpdateKeyRoutes(context.Background(), "abc", nil); err == nil {
		t.Fatal("an empty route list allows every route and must be refused")
	}
	for routes, current := range map[string]bool{"llm_api_routes,/v1/models,/models": true, "/models,/v1/models,llm_api_routes": true,
		"llm_api_routes": false, "": false, "llm_api_routes,/v1/models,/models,/key/info": false} {
		if got := KeyRoutesCurrent(strings.Split(routes, ",")); got != current {
			t.Errorf("KeyRoutesCurrent(%q) = %v", routes, got)
		}
	}
}

func TestGenerateAndUpdateRefuseEmptyModelList(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	if err := client.GenerateKey(context.Background(), GenerateRequest{Key: "sk-v", Alias: "a"}); err == nil {
		t.Fatal("GenerateKey accepted an empty model list")
	}
	if err := client.UpdateKeyModels(context.Background(), "abc", nil); err == nil {
		t.Fatal("UpdateKeyModels accepted an empty model list")
	}
	if len(*requests) != 0 {
		t.Fatalf("no request may be sent, got %d", len(*requests))
	}
}

func TestGenerateKeyAliasConflict(t *testing.T) {
	client, _ := testServer(t, func(recordedRequest) (int, string) {
		return 400, `{"error":{"message":"Key with alias 'cetirun-a' already exists. Unique key aliases across all keys are required."}}`
	})
	err := client.GenerateKey(context.Background(), GenerateRequest{Key: "sk-v", Alias: "cetirun-a", Models: []string{NoModels}})
	if !errors.Is(err, ErrAliasExists) {
		t.Fatalf("err = %v, want ErrAliasExists", err)
	}
}

func TestUpdateKeyModelsSendsOnlyModels(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	if err := client.UpdateKeyModels(context.Background(), "abc", []string{"cetirun-shared"}); err != nil {
		t.Fatal(err)
	}
	body := (*requests)[0].body
	if len(body) != 2 || body["key"] != "abc" {
		t.Fatalf("update must carry only the token and models, got %v", body)
	}
}

func TestUpdateKeyBudgetAndBlockedUseNarrowUpdates(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	if err := client.UpdateKeyBudget(context.Background(), "abc", 20, "1mo"); err != nil {
		t.Fatal(err)
	}
	if err := client.SetKeyBlocked(context.Background(), "abc", true); err != nil {
		t.Fatal(err)
	}
	if got := (*requests)[0].body; len(got) != 3 || got["key"] != "abc" || got["max_budget"] != float64(20) || got["budget_duration"] != "1mo" {
		t.Fatalf("unexpected budget update %v", got)
	}
	if got := (*requests)[1].body; len(got) != 2 || got["key"] != "abc" || got["blocked"] != true {
		t.Fatalf("unexpected block update %v", got)
	}
	if err := client.UpdateKeyBudget(context.Background(), "abc", 0, "1mo"); err == nil {
		t.Fatal("UpdateKeyBudget accepted zero budget")
	}
	if err := client.UpdateKeyBudget(context.Background(), "abc", 1, ""); err == nil {
		t.Fatal("UpdateKeyBudget accepted empty duration")
	}
}

func TestDeleteKeyByAliasTreatsNotFoundAsDeleted(t *testing.T) {
	calls := 0
	client, requests := testServer(t, func(recordedRequest) (int, string) {
		calls++
		if calls == 1 {
			return 200, `{"deleted_keys":["cetirun-a"]}`
		}
		return 404, `{"error":{"message":"{'error': 'No keys found'}"}}`
	})
	for i := 0; i < 2; i++ {
		if err := client.DeleteKeyByAlias(context.Background(), "cetirun-a"); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if aliases, _ := (*requests)[0].body["key_aliases"].([]any); len(aliases) != 1 || aliases[0] != "cetirun-a" {
		t.Fatalf("unexpected body %v", (*requests)[0].body)
	}
}

func TestErrorsNeverCarryKeyMaterial(t *testing.T) {
	client, _ := testServer(t, func(recordedRequest) (int, string) {
		return 401, `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-...beWw, Key Hash (Token) =50d1"}}`
	})
	_, err := client.GetKey(context.Background(), "abc")
	if err == nil || strings.Contains(err.Error(), "beWw") || strings.Contains(err.Error(), "sk-master") {
		t.Fatalf("error leaks key material: %v", err)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ url, key string }{
		{"", "sk-m"}, {"ftp://gateway:4000", "sk-m"}, {"http://user:pw@gateway:4000", "sk-m"},
		{"http://gateway:4000?x=1", "sk-m"}, {"http://gateway:4000", " "},
	} {
		if _, err := New(tc.url, tc.key, nil); err == nil {
			t.Errorf("New(%q) accepted invalid configuration", tc.url)
		}
	}
}

// The responses follow LiteLLM's /model/info: the configured model_info with
// the price table's data added, and litellm_params without credentials. A
// deployment the price table does not know shows zero prices there.
func TestListModelsCountsPricesTheWayTheGatewayBlocksUnpricedModels(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) {
		return 200, `{"data":[
			{"model_name":"listed","litellm_params":{"model":"azure/gpt-5"},"model_info":{"id":"a","access_groups":["cetirun-shared"],
				"db_model":true,"max_input_tokens":200000,"max_output_tokens":32768,"base_model":"azure/gpt-5",
				"input_cost_per_token":0.000001,"output_cost_per_token":0.000004}},
			{"model_name":"free","litellm_params":{"model":"openai/local","input_cost_per_token":0,"output_cost_per_token":0},
				"model_info":{"id":"b","input_cost_per_token":0,"output_cost_per_token":0}},
			{"model_name":"unknown-price","litellm_params":{"model":"azure/my-deployment"},
				"model_info":{"id":"c","input_cost_per_token":0,"output_cost_per_token":0}},
			{"model_name":"per-second","litellm_params":{"model":"openai/whisper"},"model_info":{"id":"d","input_cost_per_second":0.0001}},
			{"model_name":"per-query","litellm_params":{"model":"openai/search"},
				"model_info":{"id":"e","search_context_cost_per_query":{"search_context_size_low":0.03}}},
			{"model_name":"tiered","litellm_params":{"model":"openai/tiered"},"model_info":{"id":"f","tiered_pricing":[{"input_cost_per_token":0.000001}]}},
			{"model_name":"no-info","litellm_params":{"model":"azure/other"}}
		]}`
	})
	deployments, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r := (*requests)[0]; r.method != http.MethodGet || r.path != "/model/info" || r.auth != "Bearer sk-master" {
		t.Fatalf("unexpected request %+v", r)
	}
	want := map[string]bool{"listed": true, "free": true, "unknown-price": false, "per-second": true, "per-query": true, "tiered": true, "no-info": false}
	if len(deployments) != len(want) {
		t.Fatalf("got %d deployments, want %d", len(deployments), len(want))
	}
	for _, deployment := range deployments {
		if deployment.Priced != want[deployment.ModelName] {
			t.Errorf("%s: Priced = %v, want %v", deployment.ModelName, deployment.Priced, want[deployment.ModelName])
		}
	}
	if first := deployments[0]; first.ID != "a" || len(first.AccessGroups) != 1 || first.AccessGroups[0] != "cetirun-shared" ||
		!first.FromDatabase || !first.DatabaseSourceKnown || first.MaxInputTokens != 200000 || first.MaxOutputTokens != 32768 {
		t.Fatalf("deployment fields not read: %+v", first)
	}
	if deployments[1].DatabaseSourceKnown {
		t.Fatal("an omitted db_model field must not be treated as trusted static provenance")
	}
}

// LiteLLM's model info says whether a deployment accepts images; only an
// explicit true counts.
func TestListModelsReadsWhetherADeploymentAcceptsImages(t *testing.T) {
	client, _ := testServer(t, func(recordedRequest) (int, string) {
		return 200, `{"data":[
			{"model_name":"sees","litellm_params":{"model":"azure/gpt-6-sol"},"model_info":{"id":"a","supports_vision":true}},
			{"model_name":"blind","litellm_params":{"model":"azure/o1-mini"},"model_info":{"id":"b","supports_vision":false}},
			{"model_name":"router","litellm_params":{"model":"auto_router/complexity_router"},"model_info":{"id":"c","supports_vision":null}},
			{"model_name":"odd","litellm_params":{"model":"azure/x"},"model_info":{"id":"d","supports_vision":"true"}},
			{"model_name":"no-info","litellm_params":{"model":"azure/y"}}
		]}`
	})
	deployments, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sees": true, "blind": false, "router": false, "odd": false, "no-info": false}
	if len(deployments) != len(want) {
		t.Fatalf("got %d deployments", len(deployments))
	}
	for _, deployment := range deployments {
		if deployment.Vision != want[deployment.ModelName] {
			t.Errorf("%s: Vision = %v, want %v", deployment.ModelName, deployment.Vision, want[deployment.ModelName])
		}
	}
}

// The price table names the models with a price and those that accept images.
func TestPriceTableReadsPricesAndImageSupport(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) {
		return 200, `{
			"sample_spec":{"input_cost_per_token":0,"supports_vision":"true or false"},
			"gpt-5.5":{"input_cost_per_token":0.000001,"output_cost_per_token":0.00001,"supports_vision":true},
			"claude-sonnet-4-5":{"input_cost_per_token":0.000003,"supports_vision":false},
			"free-vision":{"input_cost_per_token":0,"output_cost_per_token":0,"supports_vision":true},
			"no-vision-field":{"output_cost_per_token":0.00002}
		}`
	})
	table, err := client.PriceTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r := (*requests)[0]; r.method != http.MethodGet || r.path != "/public/litellm_model_cost_map" {
		t.Fatalf("unexpected request %+v", r)
	}
	wantPriced := map[string]bool{"gpt-5.5": true, "claude-sonnet-4-5": true, "no-vision-field": true}
	wantVision := map[string]bool{"gpt-5.5": true, "free-vision": true}
	if !maps.Equal(table.Priced, wantPriced) || !maps.Equal(table.Vision, wantVision) {
		t.Fatalf("priced %v vision %v", table.Priced, table.Vision)
	}
}

func TestListModelsReadsTheMode(t *testing.T) {
	client, _ := testServer(t, func(recordedRequest) (int, string) {
		return 200, `{"data":[
			{"model_name":"chat","litellm_params":{"model":"azure/gpt-5"},"model_info":{"id":"a","mode":"chat"}},
			{"model_name":"decision","litellm_params":{"model":"azure_ai/decision-1"},"model_info":{"id":"b","mode":"evaluation"}},
			{"model_name":"router","litellm_params":{"model":"auto_router/complexity_router"},"model_info":{"id":"c","mode":null}},
			{"model_name":"odd","litellm_params":{"model":"azure/x"},"model_info":{"id":"d","mode":7}},
			{"model_name":"no-info","litellm_params":{"model":"azure/y"}}
		]}`
	})
	deployments, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"chat": "chat", "decision": "evaluation", "router": "", "odd": "", "no-info": ""}
	for _, deployment := range deployments {
		if deployment.Mode != want[deployment.ModelName] {
			t.Errorf("%s: Mode = %q, want %q", deployment.ModelName, deployment.Mode, want[deployment.ModelName])
		}
	}
	for mode, chat := range map[string]bool{"": true, "chat": true, "completion": true, "responses": true,
		"evaluation": false, "embedding": false, "image_generation": false, "rerank": false, "Chat": false} {
		if ChatModel(mode) != chat {
			t.Errorf("ChatModel(%q) = %v, want %v", mode, !chat, chat)
		}
	}
	// Only the evaluation mode marks a decision model.
	for mode, decision := range map[string]bool{"evaluation": true, "": false, "chat": false, "responses": false,
		"embedding": false, "Evaluation": false} {
		if DecisionModel(mode) != decision {
			t.Errorf("DecisionModel(%q) = %v, want %v", mode, !decision, decision)
		}
	}
}

func TestBlockUnpricedModelsReadsTheGatewaysSetting(t *testing.T) {
	for enabled, body := range map[bool]string{true: `{"enabled":true}`, false: `{"enabled":false}`} {
		client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, body })
		got, err := client.BlockUnpricedModels(context.Background())
		if err != nil || got != enabled {
			t.Fatalf("BlockUnpricedModels = %v, %v; want %v", got, err, enabled)
		}
		if r := (*requests)[0]; r.method != http.MethodGet || r.path != "/config/block_requests_for_models_without_pricing" || r.auth != "Bearer sk-master" {
			t.Fatalf("unexpected request %+v", r)
		}
	}
	// A Gateway that does not report the setting gives an error, not a guess.
	for status, body := range map[int]string{200: `{}`, 404: `{"detail":"Not Found"}`} {
		client, _ := testServer(t, func(recordedRequest) (int, string) { return status, body })
		if _, err := client.BlockUnpricedModels(context.Background()); err == nil {
			t.Fatalf("status %d %s: expected an error", status, body)
		}
	}
}
