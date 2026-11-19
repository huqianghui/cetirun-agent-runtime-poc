// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package aigateway is a minimal client for the virtual-key management API of
// the CetiRun AI Gateway (LiteLLM). The operator uses it to give each Agent its
// own key.
//
// Key values never appear in URLs, error messages, or logs. The Gateway stores
// a key as the SHA-256 hash of its value, so lookups, updates, and deletes use
// that hash (the "token") or the key alias instead of the value.
package aigateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// NoModels is LiteLLM's model entry that grants no model access. An empty
// model list grants every model, so a key without access groups must carry
// this entry instead.
const NoModels = "no-default-models"

// LLMAPIRoutes restricts a key to inference routes, so it cannot call the
// management API even for its own key.
const LLMAPIRoutes = "llm_api_routes"

// KeyRoutes are the routes an Agent's key may call: the inference routes, and
// the model lookup by name, GET /v1/models/<model> and /models/<model>, which
// LiteLLM files under its internal-user routes rather than the inference ones.
// Clients such as Hermes look a model up before using it. The prefixes match
// only the lookup and Google's generateContent routes, which are inference
// routes already.
var KeyRoutes = []string{LLMAPIRoutes, "/v1/models", "/models"}

// DefaultTimeout bounds each Gateway call.
const DefaultTimeout = 15 * time.Second

// ErrAliasExists reports that a key with the requested alias already exists.
var ErrAliasExists = errors.New("AI Gateway key alias already exists")

// APIError is a non-2xx Gateway response. Message is truncated and has any
// key-like substrings removed.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("AI Gateway returned HTTP %d: %s", e.StatusCode, e.Message)
}

// Key is the non-secret subset of a virtual key's settings.
type Key struct {
	Alias          string
	Models         []string
	MaxBudget      *float64
	BudgetDuration string
	Blocked        bool
	// Spend and BudgetResetAt are enforcement observations returned by
	// LiteLLM. They are deliberately non-secret: callers use them only for
	// status/read-back and never infer a budget from a missing value.
	Spend         *float64
	BudgetResetAt *time.Time
	TeamID        string
	UserID        string
	// Aliases maps a model name the key's callers send to the model name the
	// Gateway serves the request with (LiteLLM's key aliases).
	Aliases map[string]string
	// AllowedRoutes are the routes the key may call; see KeyRoutes.
	AllowedRoutes []string
	// Limits are the key's stored request limits.
	Limits KeyLimits
}

// GenerateRequest creates a key with a caller-chosen value, which lets the
// caller persist the value before the create request and recover by lookup
// when the outcome of the request is unknown.
type GenerateRequest struct {
	Key            string
	Alias          string
	Models         []string
	MaxBudget      float64
	BudgetDuration string
	// Blocked creates a revoked key. This is used when an Agent is suspended
	// before it has ever received a Gateway key: creating it unblocked, even
	// briefly, would violate the suspension boundary.
	Blocked  bool
	Metadata map[string]string
	// TeamID and UserID attribute the key's spend to a Gateway team and
	// user. The team must exist; the user need not.
	TeamID string
	UserID string
	// Aliases are the key's model aliases; see Key.Aliases.
	Aliases map[string]string
	// Limits are the key's request limits; absent limits are unlimited.
	Limits KeyLimits
}

// Client calls the Gateway management API with the master key.
type Client struct {
	base      *url.URL
	masterKey string
	http      *http.Client
}

// New returns a client for the Gateway at baseURL.
func New(baseURL, masterKey string, httpClient *http.Client) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("AI Gateway URL is invalid")
	}
	if strings.TrimSpace(masterKey) == "" {
		return nil, fmt.Errorf("AI Gateway master key is empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{base: base, masterKey: strings.TrimSpace(masterKey), http: httpClient}, nil
}

// NewKeyValue returns a new random key value in the Gateway's required form.
func NewKeyValue() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate virtual key: %w", err)
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidKeyValue reports whether value has the form NewKeyValue produces.
func ValidKeyValue(value string) bool {
	return keyValuePattern.MatchString(value)
}

var keyValuePattern = regexp.MustCompile(`^sk-[A-Za-z0-9_-]{43}$`)

// Token returns the Gateway's identifier for a key value: its SHA-256 hash.
func Token(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// GetKey returns the key identified by token, or nil if missing or archived.
func (c *Client) GetKey(ctx context.Context, token string) (*Key, error) {
	query := url.Values{"key": []string{token}}
	var response struct {
		Info struct {
			KeyAlias       string     `json:"key_alias"`
			Models         []string   `json:"models"`
			MaxBudget      *float64   `json:"max_budget"`
			BudgetDuration string     `json:"budget_duration"`
			Blocked        *bool      `json:"blocked"`
			Spend          *float64   `json:"spend"`
			BudgetResetAt  *string    `json:"budget_reset_at"`
			DeletedAt      *time.Time `json:"deleted_at"`
			TeamID         *string    `json:"team_id"`
			UserID         *string    `json:"user_id"`
			Aliases        any        `json:"aliases"`
			AllowedRoutes  []string   `json:"allowed_routes"`
			keyLimitsInfo
		} `json:"info"`
	}
	status, err := c.do(ctx, http.MethodGet, "/key/info?"+query.Encode(), nil, &response)
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info := response.Info
	if info.DeletedAt != nil {
		return nil, nil
	}
	key := &Key{
		Alias: info.KeyAlias, Models: info.Models, MaxBudget: info.MaxBudget,
		BudgetDuration: info.BudgetDuration, Blocked: info.Blocked != nil && *info.Blocked, Spend: info.Spend,
		TeamID: deref(info.TeamID), UserID: deref(info.UserID), Aliases: keyAliases(info.Aliases),
		AllowedRoutes: info.AllowedRoutes,
		Limits:        info.limits(),
	}
	if info.BudgetResetAt != nil && strings.TrimSpace(*info.BudgetResetAt) != "" {
		resetAt, err := time.Parse(time.RFC3339, *info.BudgetResetAt)
		if err != nil {
			return nil, fmt.Errorf("decode AI Gateway key budget reset time: %w", err)
		}
		key.BudgetResetAt = &resetAt
	}
	return key, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// GenerateKey creates a key. It returns ErrAliasExists when the alias is taken.
func (c *Client) GenerateKey(ctx context.Context, req GenerateRequest) error {
	if !strings.HasPrefix(req.Key, "sk-") {
		return fmt.Errorf("virtual key value must start with sk-")
	}
	if len(req.Models) == 0 {
		return fmt.Errorf("virtual key model list must not be empty; an empty list grants every model")
	}
	if err := req.Limits.Validate(); err != nil {
		return err
	}
	body := map[string]any{
		"key":            req.Key,
		"key_alias":      req.Alias,
		"models":         req.Models,
		"allowed_routes": KeyRoutes,
		"metadata":       req.Metadata,
	}
	req.Limits.addTo(body)
	if req.MaxBudget > 0 {
		body["max_budget"] = req.MaxBudget
	}
	if req.BudgetDuration != "" {
		body["budget_duration"] = req.BudgetDuration
	}
	if req.Blocked {
		body["blocked"] = true
	}
	if req.TeamID != "" {
		body["team_id"] = req.TeamID
	}
	if req.UserID != "" {
		body["user_id"] = req.UserID
	}
	if len(req.Aliases) > 0 {
		body["aliases"] = req.Aliases
	}
	status, err := c.do(ctx, http.MethodPost, "/key/generate", body, nil)
	var apiErr *APIError
	if status == http.StatusBadRequest && errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "already exists") {
		return ErrAliasExists
	}
	return err
}

// UpdateKeyModels replaces the model list of the key identified by token and
// leaves every other setting, including the budget, unchanged.
func (c *Client) UpdateKeyModels(ctx context.Context, token string, models []string) error {
	if len(models) == 0 {
		return fmt.Errorf("virtual key model list must not be empty; an empty list grants every model")
	}
	_, err := c.do(ctx, http.MethodPost, "/key/update", map[string]any{"key": token, "models": models}, nil)
	return err
}

// UpdateKeyRoutes replaces the routes the key identified by token may call.
func (c *Client) UpdateKeyRoutes(ctx context.Context, token string, routes []string) error {
	if len(routes) == 0 {
		return fmt.Errorf("virtual key routes must not be empty; an empty list allows every route")
	}
	_, err := c.do(ctx, http.MethodPost, "/key/update", map[string]any{"key": token, "allowed_routes": routes}, nil)
	return err
}

// KeyRoutesCurrent reports whether a key may call exactly KeyRoutes.
func KeyRoutesCurrent(routes []string) bool {
	return len(routes) == len(KeyRoutes) && !slices.ContainsFunc(KeyRoutes, func(route string) bool { return !slices.Contains(routes, route) })
}

// UpdateKeyBudget replaces the native LiteLLM USD budget without replacing
// the key. Keeping the same key preserves LiteLLM's current-period spend and
// reset state; callers must GetKey afterwards before treating the change as
// applied.
func (c *Client) UpdateKeyBudget(ctx context.Context, token string, maxBudget float64, budgetDuration string) error {
	if maxBudget <= 0 {
		return fmt.Errorf("virtual key budget must be positive")
	}
	if budgetDuration == "" {
		return fmt.Errorf("virtual key budget duration is required")
	}
	_, err := c.do(ctx, http.MethodPost, "/key/update", map[string]any{
		"key": token, "max_budget": maxBudget, "budget_duration": budgetDuration,
	}, nil)
	return err
}

// SetKeyBlocked changes only LiteLLM's blocked flag. It is used for
// suspension and no-entitlement tightening, where deleting or recreating the
// key would lose current-period budget accounting.
func (c *Client) SetKeyBlocked(ctx context.Context, token string, blocked bool) error {
	_, err := c.do(ctx, http.MethodPost, "/key/update", map[string]any{"key": token, "blocked": blocked}, nil)
	return err
}

// UpdateKeyOwner moves the key identified by token to another team and user.
// An empty team or user clears it. The Gateway requires the key's user to be
// a member of the new team.
func (c *Client) UpdateKeyOwner(ctx context.Context, token, teamID, userID string) error {
	body := map[string]any{"key": token, "team_id": nil, "user_id": nil}
	if teamID != "" {
		body["team_id"] = teamID
	}
	if userID != "" {
		body["user_id"] = userID
	}
	_, err := c.do(ctx, http.MethodPost, "/key/update", body, nil)
	return err
}

// EnsureTeam creates a team with an empty model list, which leaves model
// access to each key, unless a team with that ID exists.
func (c *Client) EnsureTeam(ctx context.Context, teamID, alias string, metadata map[string]string) error {
	status, err := c.do(ctx, http.MethodPost, "/team/new", map[string]any{
		"team_id": teamID, "team_alias": alias, "models": []string{}, "metadata": metadata,
	}, nil)
	var apiErr *APIError
	if status == http.StatusBadRequest && errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "already exists") {
		return nil
	}
	return err
}

// EnsureUser creates a user without a key of its own, unless it exists.
func (c *Client) EnsureUser(ctx context.Context, userID string, metadata map[string]string) error {
	status, err := c.do(ctx, http.MethodPost, "/user/new", map[string]any{
		"user_id": userID, "user_alias": userID, "auto_create_key": false, "metadata": metadata,
	}, nil)
	var apiErr *APIError
	if status == http.StatusConflict || (errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "already exists")) {
		return nil
	}
	return err
}

// EnsureTeamMember adds the user to the team as a member, unless it is one.
func (c *Client) EnsureTeamMember(ctx context.Context, teamID, userID string) error {
	status, err := c.do(ctx, http.MethodPost, "/team/member_add", map[string]any{
		"team_id": teamID, "member": map[string]any{"role": "user", "user_id": userID},
	}, nil)
	var apiErr *APIError
	if status == http.StatusBadRequest && errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "already in team") {
		return nil
	}
	return err
}

// Deployment is the non-secret subset of a model deployment.
type Deployment struct {
	ID           string
	ModelName    string
	AccessGroups []string
	FromDatabase bool
	// DatabaseSourceKnown distinguishes an explicit db_model=false (a model
	// loaded from trusted static configuration) from an older or malformed
	// response which omitted the provenance field.
	DatabaseSourceKnown bool
	MaxInputTokens      int64
	MaxOutputTokens     int64
	// Mode is the deployment's model_info.mode, the kind of call LiteLLM
	// makes to it, such as chat, responses, embedding or evaluation; empty
	// when the model info names none.
	Mode string
	// Priced reports whether the deployment carries a price by the parts of
	// LiteLLM's unpriced-model check that /model/info shows: a price set in
	// litellm_params, even zero, or a positive price or tiered pricing in the
	// model info. LiteLLM also accepts an explicit zero in model_info, but
	// /model/info fills unknown prices with zero, so such a model counts as
	// unpriced here.
	Priced bool
	// Model is litellm_params.model, the provider model LiteLLM calls, and
	// BaseModel model_info.base_model, the price-table entry the deployment
	// is billed as, when set.
	Model     string
	BaseModel string
	// Vision reports that the deployment's model info says it accepts
	// images: supports_vision is true. LiteLLM takes the value from its price
	// table entry for the deployment's base model or model, unless the
	// administrator sets it in model_info. A model the table does not know
	// has none.
	Vision bool
	// Routing is how LiteLLM weighs the deployment against the others of its
	// model name.
	Routing Routing
	// Router is set for a LiteLLM auto router: the models it sends requests
	// to, as far as its configuration shows them.
	Router *Router
}

// Routing holds the litellm_params fields that decide which of a model name's
// deployments LiteLLM sends a request to. A nil field is not set.
type Routing struct {
	// Order: deployments with a higher order serve only while those with
	// the lowest are unavailable.
	Order *int64
	// Weight, RPM and TPM weigh the deployments of one order against each
	// other.
	Weight *float64
	RPM    *int64
	TPM    *int64
}

// IsZero reports whether no routing field is set.
func (r Routing) IsZero() bool {
	return r.Order == nil && r.Weight == nil && r.RPM == nil && r.TPM == nil
}

// String renders the set fields in a stable form.
func (r Routing) String() string {
	var parts []string
	if r.Order != nil {
		parts = append(parts, fmt.Sprintf("order=%d", *r.Order))
	}
	if r.Weight != nil {
		parts = append(parts, fmt.Sprintf("weight=%g", *r.Weight))
	}
	if r.RPM != nil {
		parts = append(parts, fmt.Sprintf("rpm=%d", *r.RPM))
	}
	if r.TPM != nil {
		parts = append(parts, fmt.Sprintf("tpm=%d", *r.TPM))
	}
	return strings.Join(parts, ",")
}

func (r Routing) params(params map[string]any) {
	if r.Order != nil {
		params["order"] = *r.Order
	}
	if r.Weight != nil {
		params["weight"] = *r.Weight
	}
	if r.RPM != nil {
		params["rpm"] = *r.RPM
	}
	if r.TPM != nil {
		params["tpm"] = *r.TPM
	}
}

// routing reads the routing fields of a deployment's litellm_params. A field
// that is not a positive number (for order, not a number of at least zero)
// counts as not set, as LiteLLM's routing ignores it.
func routing(params map[string]json.RawMessage) Routing {
	var out Routing
	// LiteLLM takes order 0 as the lowest, so it is kept, unlike a zero weight,
	// rpm or tpm.
	if value, ok := nonNegativeNumber(params["order"]); ok && value == math.Trunc(value) {
		order := int64(value)
		out.Order = &order
	}
	if value, ok := positiveNumber(params["weight"]); ok {
		out.Weight = &value
	}
	if value, ok := positiveNumber(params["rpm"]); ok && value == math.Trunc(value) {
		rpm := int64(value)
		out.RPM = &rpm
	}
	if value, ok := positiveNumber(params["tpm"]); ok && value == math.Trunc(value) {
		tpm := int64(value)
		out.TPM = &tpm
	}
	return out
}

func nonNegativeNumber(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value < 0 || math.IsInf(value, 0) || value > 1<<53 {
		return 0, false
	}
	return value, true
}

func positiveNumber(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value <= 0 || math.IsInf(value, 0) || value > 1<<53 {
		return 0, false
	}
	return value, true
}

func stringValue(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// ListModels returns every model deployment the Gateway serves.
func (c *Client) ListModels(ctx context.Context) ([]Deployment, error) {
	var response struct {
		Data []struct {
			ModelName     string                     `json:"model_name"`
			LiteLLMParams map[string]json.RawMessage `json:"litellm_params"`
			ModelInfo     json.RawMessage            `json:"model_info"`
		} `json:"data"`
	}
	if _, err := c.doLimit(ctx, http.MethodGet, "/model/info", nil, &response, listLimit); err != nil {
		return nil, err
	}
	deployments := make([]Deployment, 0, len(response.Data))
	for _, item := range response.Data {
		var info struct {
			ID              string          `json:"id"`
			AccessGroups    []string        `json:"access_groups"`
			DBModel         *bool           `json:"db_model"`
			MaxInputTokens  *float64        `json:"max_input_tokens"`
			MaxOutputTokens *float64        `json:"max_output_tokens"`
			BaseModel       json.RawMessage `json:"base_model"`
		}
		var fields map[string]any
		if len(item.ModelInfo) > 0 {
			if err := json.Unmarshal(item.ModelInfo, &info); err != nil {
				return nil, fmt.Errorf("decode AI Gateway model info: %w", err)
			}
			if err := json.Unmarshal(item.ModelInfo, &fields); err != nil {
				return nil, fmt.Errorf("decode AI Gateway model info: %w", err)
			}
		}
		deployments = append(deployments, Deployment{
			ID: info.ID, ModelName: item.ModelName, AccessGroups: info.AccessGroups,
			FromDatabase: info.DBModel != nil && *info.DBModel, DatabaseSourceKnown: info.DBModel != nil,
			MaxInputTokens: tokens(info.MaxInputTokens), MaxOutputTokens: tokens(info.MaxOutputTokens),
			Mode:   modelMode(fields),
			Priced: declaresPrice(item.LiteLLMParams) || hasPrice(fields),
			Model:  stringValue(item.LiteLLMParams["model"]), BaseModel: stringValue(info.BaseModel),
			Vision:  fields["supports_vision"] == true,
			Routing: routing(item.LiteLLMParams), Router: parseRouter(item.LiteLLMParams),
		})
	}
	return deployments, nil
}

// declaresPrice reports a price set in a deployment's litellm_params. LiteLLM
// counts any such field, even zero, as pricing the deployment.
func declaresPrice(params map[string]json.RawMessage) bool {
	for key, value := range params {
		if (strings.Contains(key, "cost_per") || key == "tiered_pricing") && string(value) != "null" {
			return true
		}
	}
	return false
}

// hasPrice reports tiered pricing or a positive price for any billed metric,
// tokens or otherwise, in a deployment's model info.
func hasPrice(info map[string]any) bool {
	if info["tiered_pricing"] != nil {
		return true
	}
	for key, value := range info {
		if !strings.Contains(key, "cost_per") {
			continue
		}
		if price, ok := value.(float64); ok && price > 0 {
			return true
		}
		if nested, ok := value.(map[string]any); ok {
			for _, inner := range nested {
				if price, ok := inner.(float64); ok && price > 0 {
					return true
				}
			}
		}
	}
	return false
}

// BlockUnpricedModels reports whether the Gateway refuses requests for models
// without a price. An administrator can change the setting in the Gateway's
// console, which keeps the change in its database over the chart's value.
func (c *Client) BlockUnpricedModels(ctx context.Context) (bool, error) {
	var response struct {
		Enabled *bool `json:"enabled"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/config/block_requests_for_models_without_pricing", nil, &response); err != nil {
		return false, err
	}
	if response.Enabled == nil {
		return false, fmt.Errorf("the AI Gateway did not report whether it blocks unpriced models")
	}
	return *response.Enabled, nil
}

// modelMode is the mode a deployment's model info names, or "" for none or a
// value that is not a string.
func modelMode(info map[string]any) string {
	mode, _ := info["mode"].(string)
	return mode
}

// ChatModel reports whether Agents can hold a conversation with a deployment
// of the given mode: LiteLLM serves chat or Responses API requests with it, or
// its model info names no mode. A model of another mode, such as an embedding
// model or a decision model an auto router classifies requests with, answers
// no conversation.
func ChatModel(mode string) bool {
	switch mode {
	case "", "chat", "completion", "responses":
		return true
	}
	return false
}

// DecisionModel reports whether a deployment of the given mode is a decision
// model, such as Microsoft-Decision-1: LiteLLM lists it with the evaluation
// mode and calls it through its Decisions API, where it returns one
// probability per tier for an auto router's jev classifier. It answers no
// conversation.
func DecisionModel(mode string) bool {
	return mode == "evaluation"
}

func tokens(value *float64) int64 {
	if value == nil || *value <= 0 {
		return 0
	}
	return int64(*value)
}

func positive(value *float64) bool { return value != nil && *value > 0 }

// ModelRequest registers a deployment behind a LiteLLM-compatible endpoint.
type ModelRequest struct {
	// ID is the caller-chosen deployment ID, which makes registration
	// traceable and removal possible without a lookup.
	ID        string
	ModelName string
	// Model is LiteLLM's provider-prefixed model, such as litellm_proxy/gpt-5.
	Model        string
	APIBase      string
	APIKey       string
	AccessGroups []string
	// BaseModel names the price-table entry when Model's own name has none.
	BaseModel string
	// Routing sets the deployment's routing fields.
	Routing Routing
	// NoCooldown sets litellm_params.cooldown_time to 0: LiteLLM never takes
	// the deployment out of rotation after a failure.
	NoCooldown bool
	// ProbeWithoutRetries sets model_info.health_check_params.max_retries to
	// 0. The Gateway's background health check calls deployments outside its
	// router, so the provider client's own retries apply: two more calls for
	// each failed probe, which a rate-limited upstream answers with more
	// rate limiting.
	ProbeWithoutRetries bool
	// NoBackgroundHealthCheck sets model_info.disable_background_health_check:
	// the Gateway's background health checks skip the deployment. It stays in
	// rotation, since routing leaves out only deployments whose last probe
	// failed.
	NoBackgroundHealthCheck bool
}

// AddModel registers a deployment. The Gateway rejects a second deployment
// with the same ID.
func (c *Client) AddModel(ctx context.Context, req ModelRequest) error {
	if req.ID == "" || req.ModelName == "" || req.Model == "" {
		return fmt.Errorf("model ID, name and provider model are required")
	}
	info := map[string]any{"id": req.ID, "access_groups": req.AccessGroups}
	if req.BaseModel != "" {
		info["base_model"] = req.BaseModel
	}
	if req.ProbeWithoutRetries {
		info["health_check_params"] = map[string]any{"max_retries": 0}
	}
	if req.NoBackgroundHealthCheck {
		info["disable_background_health_check"] = true
	}
	params := map[string]any{"model": req.Model, "api_base": req.APIBase}
	if req.APIKey != "" {
		params["api_key"] = req.APIKey
	}
	req.Routing.params(params)
	if req.NoCooldown {
		params["cooldown_time"] = 0
	}
	_, err := c.do(ctx, http.MethodPost, "/model/new", map[string]any{
		"model_name": req.ModelName, "litellm_params": params, "model_info": info,
	}, nil)
	return err
}

// DeleteModel removes the deployment with the given ID. A deployment that
// does not exist is treated as already deleted.
func (c *Client) DeleteModel(ctx context.Context, id string) error {
	status, err := c.do(ctx, http.MethodPost, "/model/delete", map[string]any{"id": id}, nil)
	var apiErr *APIError
	if status == http.StatusNotFound ||
		(status == http.StatusBadRequest && errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "not found")) {
		return nil
	}
	return err
}

// PriceTable is what the operator reads of the Gateway's model price table.
type PriceTable struct {
	// Priced are the names that carry a price. They are how a caller picks
	// a base model for a deployment whose own name the table does not know.
	Priced map[string]bool
	// Vision are the names whose entry says the model accepts images
	// (supports_vision), the value LiteLLM reports in the model info of a
	// deployment billed as that name.
	Vision map[string]bool
}

// PriceTable reads the Gateway's model price table.
func (c *Client) PriceTable(ctx context.Context) (PriceTable, error) {
	var table map[string]struct {
		InputCostPerToken  *float64 `json:"input_cost_per_token"`
		OutputCostPerToken *float64 `json:"output_cost_per_token"`
		// Read leniently: one odd entry must not fail the whole table.
		SupportsVision json.RawMessage `json:"supports_vision"`
	}
	if _, err := c.doLimit(ctx, http.MethodGet, "/public/litellm_model_cost_map", nil, &table, listLimit); err != nil {
		return PriceTable{}, err
	}
	out := PriceTable{Priced: make(map[string]bool, len(table)), Vision: map[string]bool{}}
	for name, entry := range table {
		if positive(entry.InputCostPerToken) || positive(entry.OutputCostPerToken) {
			out.Priced[name] = true
		}
		if string(entry.SupportsVision) == "true" {
			out.Vision[name] = true
		}
	}
	return out, nil
}

// DeleteKeys deletes the keys identified by tokens. Keys that do not exist
// are treated as already deleted.
func (c *Client) DeleteKeys(ctx context.Context, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	status, err := c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": tokens}, nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

// DeleteKeyByAlias deletes the key with the given alias. A key that does not
// exist is treated as already deleted.
func (c *Client) DeleteKeyByAlias(ctx context.Context, alias string) error {
	status, err := c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"key_aliases": []string{alias}}, nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

// listLimit bounds list responses, such as every model deployment or the
// price table, which outgrow the limit for single-object responses.
const listLimit = 64 << 20

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	return c.doLimit(ctx, method, path, body, out, 1<<20)
}

func (c *Client) doLimit(ctx context.Context, method, path string, body any, out any, limit int64) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode AI Gateway request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, reader)
	if err != nil {
		return 0, fmt.Errorf("create AI Gateway request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.masterKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("call AI Gateway %s: %w", strings.SplitN(path, "?", 2)[0], redactError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read AI Gateway response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &APIError{StatusCode: resp.StatusCode, Message: errorMessage(raw)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode AI Gateway response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

var keyLike = regexp.MustCompile(`sk-[A-Za-z0-9_.*-]*`)

// errorMessage extracts LiteLLM's error message, bounded and with key-like
// text removed. LiteLLM echoes a masked key in some messages; removing the
// remainder keeps even partial key material out of conditions and logs.
func errorMessage(raw []byte) string {
	var parsed struct {
		Error struct {
			Message any `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	message := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &parsed) == nil {
		switch {
		case parsed.Error.Message != nil:
			message = fmt.Sprint(parsed.Error.Message)
		case parsed.Detail != nil:
			message = fmt.Sprint(parsed.Detail)
		}
	}
	message = keyLike.ReplaceAllString(message, "sk-[redacted]")
	if len(message) > 200 {
		message = message[:200] + "..."
	}
	return message
}

// redactError keeps transport errors from carrying request URLs, which hold
// only hashes but are not needed in conditions.
func redactError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
