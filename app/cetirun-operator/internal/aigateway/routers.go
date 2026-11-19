// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package aigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// ComplexityRouterModel is LiteLLM's model value that makes a deployment a
// complexity router: requests for its model name are classified and sent to
// the model of the tier the classifier picks.
const ComplexityRouterModel = "auto_router/complexity_router"

// AutoRouterPrefix starts the litellm_params.model of every LiteLLM auto
// router. LiteLLM 1.105.1 takes any model under it that starts with
// complexity_router for a complexity router
// (router_utils/auto_router_model_naming.py, classify_strategy_router_model).
const AutoRouterPrefix = "auto_router/"

// RouterModelUse is what an auto router sends a model.
type RouterModelUse string

const (
	// RouterTier models answer the requests the router puts in their tier.
	RouterTier RouterModelUse = "tier"
	// RouterDefault models answer the requests the router cannot classify.
	RouterDefault RouterModelUse = "default"
	// RouterCompactor models shorten a conversation the tier's model cannot
	// hold, with the conversation itself.
	RouterCompactor RouterModelUse = "compactor"
	// RouterClassifier models classify requests, or embed them to match
	// keyword rules; they do not answer them.
	RouterClassifier RouterModelUse = "classifier"
)

// RouterModel is one model an auto router sends requests to.
type RouterModel struct {
	Use RouterModelUse `json:"use"`
	// Tier is the tier of a tier model, as the configuration names it.
	Tier string `json:"tier,omitempty"`
	Name string `json:"name"`
}

// Router describes an auto router deployment by the models it sends
// requests to, read from the configuration /model/info reports.
type Router struct {
	// Models are the models the router can send requests to: its tiers', in
	// tier order, then its default model, the context compaction model and
	// its classifiers.
	Models []RouterModel `json:"models,omitempty"`
	// Unverifiable explains why the router may also send requests to models
	// that Models does not list, such as a setting this client does not
	// know; it is empty when Models lists them all.
	Unverifiable string `json:"unverifiable,omitempty"`
}

// complexityRouterSettings are the fields of complexity_router_config in
// LiteLLM 1.105.1 (ComplexityRouterConfig in
// router_strategy/complexity_router/config.py). Those that name a model are
// read by parseRouter; the others name tiers, keywords, thresholds or
// switches. LiteLLM accepts fields it does not have, and a later version may
// read one that names a model, so any other field makes a router
// unverifiable.
var complexityRouterSettings = func() map[string]bool {
	settings := map[string]bool{}
	for _, field := range strings.Fields(`tiers tier_model_configs enable_non_reasoning_tier tier_definitions
		fallback_tier classification_prompt classification_examples tier_labels tier_boundaries
		reasoning_override_min_score token_thresholds dimension_weights custom_dimensions code_keywords
		reasoning_keywords technical_keywords custom_technical_keywords simple_keywords default_model
		return_raw_model_name classifier_type llm_v2_config heuristic_v2_artifact heuristic_v2_success_threshold
		classifier_llm_config capability_classifier_config jev_classifier_config heuristic_first_max_tier
		hybrid_boundary_margin classifier_plugin classifier_plugin_timeout_ms classifier_fallback
		classifier_context_window_size classifier_context_budget_chars classifier_context_per_turn_chars
		classifier_context_include_assistant_turns adaptive adaptive_weights tier_distance_penalty
		adaptive_eligible escalation_keywords keyword_tier_rules stall_escalation_enabled stall_escalation_window
		stall_escalation_repeat_threshold plan_mode_min_tier plan_mode_patterns max_tokens_from_tier_model
		route_housekeeping_to_cheapest_tier housekeeping_patterns context_compaction
		enable_context_window_escalation context_window_escalation_buffer modality_routing modality_pin_override
		semantic_keyword_matching embedding_model match_threshold classification_mode cache_aware_routing
		cache_aware_routing_output_tokens cache_aware_routing_timeout_ms session_affinity deployment_affinity
		session_affinity_ttl_seconds plugins reminder_markers`) {
		settings[field] = true
	}
	return settings
}()

// builtInTiers are LiteLLM's own tier names in ascending order.
var builtInTiers = []string{"NON_REASONING", "SIMPLE", "MEDIUM", "COMPLEX", "REASONING"}

// liteLLMMaskedKeySegments are the parts of a key, between underscores or
// hyphens, for which LiteLLM 1.105.1's /model/info masks the value
// (litellm_core_utils/sensitive_data_masker.py, applied by
// proxy/common_utils/openai_endpoint_utils.py), unless another part is
// "cost". A string, or a list's strings, under such a key are masked; a
// nested object is masked by its own keys.
var liteLLMMaskedKeySegments = map[string]bool{
	"password": true, "secret": true, "key": true, "token": true, "auth": true, "authorization": true,
	"cookie": true, "credential": true, "credentials": true, "access": true, "private": true,
	"certificate": true, "fingerprint": true, "tenancy": true,
}

func liteLLMMasks(key string) bool {
	segments := strings.Split(strings.ReplaceAll(strings.ToLower(key), "-", "_"), "_")
	if slices.Contains(segments, "cost") {
		return false
	}
	return slices.ContainsFunc(segments, func(segment string) bool { return liteLLMMaskedKeySegments[segment] })
}

// unserializable is what LiteLLM's masker puts in place of a value it could
// not copy.
const unserializable = "<unable to serialize>"

// absent reports a value LiteLLM treats as not set. /model/info shows a null
// inside litellm_params as the string "None": its masker turns values other
// than numbers, booleans, strings and lists into their Python text.
func absent(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value == "" || value == "null" || value == `"None"`
}

// parseRouter reads what an auto router deployment sends requests to from its
// litellm_params as /model/info returns them, or returns nil for a deployment
// that is not an auto router.
func parseRouter(params map[string]json.RawMessage) *Router {
	var model string
	if json.Unmarshal(params["model"], &model) != nil || !strings.HasPrefix(model, AutoRouterPrefix) {
		return nil
	}
	if !strings.HasPrefix(strings.TrimPrefix(model, AutoRouterPrefix), "complexity_router") {
		return &Router{Unverifiable: fmt.Sprintf("it is a LiteLLM %s router, and only complexity routers are read", model)}
	}
	var parser routerParser
	parser.complexityRouter(params)
	return &Router{Models: parser.models, Unverifiable: strings.Join(parser.reasons, "; ")}
}

// routerParser collects the models a complexity router uses, and why it may
// use others.
type routerParser struct {
	models  []RouterModel
	reasons []string
}

func (p *routerParser) use(model RouterModel) {
	if model.Name != "" && !slices.Contains(p.models, model) {
		p.models = append(p.models, model)
	}
}

func (p *routerParser) cannot(format string, args ...any) {
	if reason := fmt.Sprintf(format, args...); !slices.Contains(p.reasons, reason) {
		p.reasons = append(p.reasons, reason)
	}
}

// complexityRouter reads the fields of LiteLLM 1.105.1 that name a model a
// complexity router calls (strategy_router_dependencies in
// router_utils/auto_router_model_naming.py, and the fields that module leaves
// out because the router ignores them, read anyway to stay on the safe side).
func (p *routerParser) complexityRouter(params map[string]json.RawMessage) {
	raw := params["complexity_router_config"]
	config := map[string]json.RawMessage{}
	switch {
	case absent(raw):
		p.cannot("it has no complexity_router_config, so LiteLLM's default tiers apply")
	case json.Unmarshal(raw, &config) != nil:
		p.cannot("its complexity_router_config is not an object")
	default:
		fields := make([]string, 0, len(config))
		for field := range config {
			fields = append(fields, field)
		}
		slices.Sort(fields)
		for _, field := range fields {
			if !complexityRouterSettings[field] {
				p.cannot("its complexity_router_config sets %s, a setting the operator does not know", field)
			}
		}
		if absent(config["tiers"]) {
			p.cannot("it names no tiers, so LiteLLM's default tiers apply")
		} else {
			p.tiers("tiers", config["tiers"])
		}
		if !absent(config["tier_model_configs"]) {
			p.tiers("tier_model_configs", config["tier_model_configs"])
		}
	}
	// The default model of requests the router cannot classify. Without one,
	// LiteLLM takes a tier's model. It overwrites the configuration's own
	// default_model with either, so that one is never called; it is read
	// all the same.
	p.model(RouterDefault, "complexity_router_default_model", params["complexity_router_default_model"])
	p.model(RouterDefault, "complexity_router_config.default_model", config["default_model"])
	// context_compaction is false, or settings whose model compacts the
	// conversation; without a model, a tier's model does.
	if compaction := config["context_compaction"]; strings.TrimSpace(string(compaction)) != "false" {
		p.nested(RouterCompactor, "context_compaction", "model", compaction)
	}
	p.nested(RouterClassifier, "classifier_llm_config", "model", config["classifier_llm_config"])
	// LiteLLM marks the embedding calls of semantic keyword matching as
	// classifier calls, like those of a model classifier.
	p.model(RouterClassifier, "complexity_router_config.embedding_model", config["embedding_model"])
	jevDeployment := p.nested(RouterClassifier, "jev_classifier_config", "deployment_name", config["jev_classifier_config"])
	var classifierType string
	_ = json.Unmarshal(config["classifier_type"], &classifierType)
	switch {
	case classifierType == "custom" || !absent(config["classifier_plugin"]):
		p.cannot("it classifies requests with a classifier plugin, code the operator cannot check")
	case classifierType == "jev" && !jevDeployment:
		p.cannot("its jev classifier calls a service outside the AI Gateway")
	}
	if plugins := config["plugins"]; !absent(plugins) && strings.TrimSpace(string(plugins)) != "[]" {
		p.cannot("it runs routing plugins, code the operator cannot check")
	}
}

// tiers reads tier names and their models: a model name, or a list of model
// names or {model_name, litellm_params} entries.
func (p *routerParser) tiers(field string, raw json.RawMessage) {
	tiers := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &tiers) != nil {
		p.cannot("its %s is not an object", field)
		return
	}
	names := make([]string, 0, len(tiers))
	for tier := range tiers {
		names = append(names, tier)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := slices.Index(builtInTiers, names[i]), slices.Index(builtInTiers, names[j])
		if a != b {
			if a < 0 || b < 0 {
				return b < 0
			}
			return a < b
		}
		return names[i] < names[j]
	})
	for _, tier := range names {
		masked := liteLLMMasks(tier)
		var name string
		if json.Unmarshal(tiers[tier], &name) == nil {
			p.tierModel(tier, name, masked)
			continue
		}
		var entries []json.RawMessage
		if json.Unmarshal(tiers[tier], &entries) != nil {
			p.cannot("tier %s is neither a model name nor a list of them", tier)
			continue
		}
		for _, entry := range entries {
			var entryName string
			if json.Unmarshal(entry, &entryName) == nil {
				p.tierModel(tier, entryName, masked)
				continue
			}
			var model struct {
				ModelName     *string                    `json:"model_name"`
				LiteLLMParams map[string]json.RawMessage `json:"litellm_params"`
			}
			if json.Unmarshal(entry, &model) != nil || model.ModelName == nil {
				p.cannot("tier %s lists an entry that names no model", tier)
				continue
			}
			// An object's model_name is masked by its own key, which is not
			// sensitive.
			p.tierModel(tier, *model.ModelName, false)
			// The entry's litellm_params are added to each request the tier
			// sends to the model.
			for key := range model.LiteLLMParams {
				if slices.Contains(strings.Split(strings.ToLower(key), "_"), "model") {
					p.cannot("tier %s sets %s for %s, which may change the model it calls", tier, key, *model.ModelName)
				}
			}
		}
	}
}

func (p *routerParser) tierModel(tier, name string, masked bool) {
	switch {
	case name == "" || name == "None":
	case masked:
		p.cannot("the Gateway's model list masks the models of tier %s", tier)
	case name == unserializable:
		p.cannot("the Gateway's model list does not show the models of tier %s", tier)
	default:
		p.use(RouterModel{Use: RouterTier, Tier: tier, Name: name})
	}
}

// model reads a model name; it reports whether one is set.
func (p *routerParser) model(use RouterModelUse, field string, raw json.RawMessage) bool {
	if absent(raw) {
		return false
	}
	var name string
	if json.Unmarshal(raw, &name) != nil || name == unserializable {
		p.cannot("its %s is not a model name", field)
		return false
	}
	p.use(RouterModel{Use: use, Name: name})
	return name != ""
}

// nested reads the model name of key in an object field; it reports whether
// one is set.
func (p *routerParser) nested(use RouterModelUse, field, key string, raw json.RawMessage) bool {
	if absent(raw) {
		return false
	}
	object := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &object) != nil {
		p.cannot("its %s is not an object", field)
		return false
	}
	return p.model(use, field+"."+key, object[key])
}

// RouterRequest registers a complexity router as a database deployment.
type RouterRequest struct {
	// ID is the caller-chosen deployment ID.
	ID string
	// ModelName is the model name callers request.
	ModelName    string
	AccessGroups []string
	// Config is LiteLLM's complexity_router_config.
	Config map[string]any
	// DefaultModel is the model of requests that cannot be classified.
	DefaultModel string
	// InputCostPerToken and OutputCostPerToken price the router's own name,
	// which the Gateway's check for unpriced models requires. Each request is
	// charged at the price of the tier model it is sent to.
	InputCostPerToken  float64
	OutputCostPerToken float64
}

// RouterBody is the /model/new request body for req. It carries no
// credentials: a router has no endpoint of its own.
func RouterBody(req RouterRequest) map[string]any {
	params := map[string]any{
		"model":                    ComplexityRouterModel,
		"complexity_router_config": req.Config,
		"input_cost_per_token":     req.InputCostPerToken,
		"output_cost_per_token":    req.OutputCostPerToken,
	}
	if req.DefaultModel != "" {
		params["complexity_router_default_model"] = req.DefaultModel
	}
	return map[string]any{
		"model_name":     req.ModelName,
		"litellm_params": params,
		"model_info":     map[string]any{"id": req.ID, "access_groups": req.AccessGroups},
	}
}

// AddRouter registers a complexity router. The Gateway rejects a second
// deployment with the same ID; LiteLLM also keeps one router per model name,
// so a router replacing another of the same name is added after that one is
// deleted.
func (c *Client) AddRouter(ctx context.Context, req RouterRequest) error {
	if req.ID == "" || req.ModelName == "" || len(req.Config) == 0 {
		return fmt.Errorf("router ID, name and configuration are required")
	}
	if req.InputCostPerToken <= 0 || req.OutputCostPerToken <= 0 {
		return fmt.Errorf("router prices must be positive")
	}
	_, err := c.do(ctx, http.MethodPost, "/model/new", RouterBody(req), nil)
	if err != nil && strings.Contains(err.Error(), "complexity_router_config") {
		return fmt.Errorf("the AI Gateway refused the router configuration: %w", err)
	}
	return err
}
