// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package copilot runs the GitHub device-code sign-in for GitHub Copilot and
// lists the models a signed-in account can use. The Portal stores the
// resulting GitHub token for the Agent's Copilot proxy; the proxy itself never
// performs authorization.
package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

const (
	// ClientID is the GitHub Copilot OAuth client the agents used before the
	// Gateway, and the one LiteLLM's Copilot provider accepts tokens from.
	ClientID = "Iv1.b507a08c87ecfe98"
	scope    = "read:user"

	defaultGitHubURL      = "https://github.com"
	defaultGitHubAPIURL   = "https://api.github.com"
	defaultCopilotAPIBase = "https://api.individual.githubcopilot.com"

	userAgent               = "CetiRunPortal/1.0"
	copilotIntegrationID    = "vscode-chat"
	copilotEditorVersion    = "vscode/1.107.0"
	copilotPluginVersion    = "copilot-chat/0.35.0"
	copilotUserAgent        = "GitHubCopilotChat/0.35.0"
	copilotGitHubAPIVersion = "2025-04-01"

	// MaxModels matches the operator's limit on the annotation.
	MaxModels = 128
	// maxResponseBytes bounds what the Portal reads from GitHub.
	maxResponseBytes = 4 << 20
)

// modelIDPattern matches the model IDs the operator accepts.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// completionsEndpoints serve chat completions, the API agents call.
var completionsEndpoints = map[string]bool{"/v1/chat/completions": true, "/chat/completions": true}

// responsesEndpoints serve the Responses API.
var responsesEndpoints = map[string]bool{"/v1/responses": true, "/responses": true}

// ErrNoCopilot reports a GitHub account that GitHub Copilot does not accept.
var ErrNoCopilot = errors.New("this GitHub account cannot use GitHub Copilot")

// Client calls GitHub and GitHub Copilot. The zero value uses the public
// endpoints.
type Client struct {
	HTTP *http.Client
	// GitHubURL and GitHubAPIURL override the public endpoints in tests.
	GitHubURL    string
	GitHubAPIURL string
	// TrustAPIBase decides whether a Copilot API address from GitHub's token
	// response is used. The default accepts HTTPS hosts under
	// githubcopilot.com only, so a response cannot send the token elsewhere.
	TrustAPIBase func(*url.URL) bool
}

// DeviceCode is a started device-code sign-in.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// PollResult is one poll of a device-code sign-in. AccessToken is set once
// the user has authorized; otherwise Error holds GitHub's OAuth error code,
// such as authorization_pending or slow_down.
type PollResult struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	Interval    int    `json:"interval"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (c *Client) githubURL() string {
	if c.GitHubURL != "" {
		return strings.TrimRight(c.GitHubURL, "/")
	}
	return defaultGitHubURL
}

func (c *Client) githubAPIURL() string {
	if c.GitHubAPIURL != "" {
		return strings.TrimRight(c.GitHubAPIURL, "/")
	}
	return defaultGitHubAPIURL
}

// StartDeviceFlow asks GitHub for a device code and the code the user enters.
func (c *Client) StartDeviceFlow(ctx context.Context) (DeviceCode, error) {
	var code DeviceCode
	form := url.Values{"client_id": {ClientID}, "scope": {scope}}
	status, body, err := c.postForm(ctx, c.githubURL()+"/login/device/code", form)
	if err != nil {
		return code, err
	}
	if status != http.StatusOK {
		return code, fmt.Errorf("GitHub device code request returned HTTP %d", status)
	}
	if err := json.Unmarshal(body, &code); err != nil {
		return code, fmt.Errorf("decode GitHub device code response: %w", err)
	}
	if code.DeviceCode == "" || code.UserCode == "" || code.VerificationURI == "" {
		return code, errors.New("GitHub returned an incomplete device code response")
	}
	if code.Interval <= 0 {
		code.Interval = 5
	}
	if code.ExpiresIn <= 0 {
		code.ExpiresIn = 900
	}
	return code, nil
}

// PollAccessToken asks GitHub once whether the user has authorized the code.
func (c *Client) PollAccessToken(ctx context.Context, deviceCode string) (PollResult, error) {
	var result PollResult
	form := url.Values{
		"client_id":   {ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	status, body, err := c.postForm(ctx, c.githubURL()+"/login/oauth/access_token", form)
	if err != nil {
		return result, err
	}
	if status != http.StatusOK {
		return result, fmt.Errorf("GitHub token request returned HTTP %d", status)
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("decode GitHub token response: %w", err)
	}
	if result.AccessToken == "" && result.Error == "" {
		return result, errors.New("GitHub returned neither a token nor an error")
	}
	return result, nil
}

func (c *Client) postForm(ctx context.Context, target string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	return c.do(req)
}

func (c *Client) do(req *http.Request) (int, []byte, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		// Credentials travel in headers and bodies, never in the URL the error names.
		return 0, nil, fmt.Errorf("call %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("read response from %s: %w", req.URL.Host, err)
	}
	return resp.StatusCode, body, nil
}

func copilotHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Copilot-Integration-Id", copilotIntegrationID)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("User-Agent", copilotUserAgent)
}

// ListModels exchanges the GitHub token for a Copilot token, as the Copilot
// proxy will, and returns the account's chat models with their limits.
func (c *Client) ListModels(ctx context.Context, githubToken string) ([]cetirunv1alpha1.CopilotModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.githubAPIURL()+"/copilot_internal/v2/token", nil)
	if err != nil {
		return nil, err
	}
	copilotHeaders(req, githubToken)
	req.Header.Set("X-Github-Api-Version", copilotGitHubAPIVersion)
	status, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		return nil, ErrNoCopilot
	case status != http.StatusOK:
		return nil, fmt.Errorf("GitHub Copilot token exchange returned HTTP %d", status)
	}
	var exchange struct {
		Token     string `json:"token"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(body, &exchange); err != nil || strings.TrimSpace(exchange.Token) == "" {
		return nil, errors.New("GitHub Copilot returned an invalid token response")
	}
	base := c.apiBase(exchange.Endpoints.API)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	copilotHeaders(req, exchange.Token)
	status, body, err = c.do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, ErrNoCopilot
	case status != http.StatusOK:
		return nil, fmt.Errorf("GitHub Copilot model list returned HTTP %d", status)
	}
	return ParseModels(body)
}

func (c *Client) apiBase(advertised string) string {
	trust := c.TrustAPIBase
	if trust == nil {
		trust = trustedCopilotAPIBase
	}
	if parsed, err := url.Parse(strings.TrimSpace(advertised)); err == nil && parsed.Host != "" && trust(parsed) {
		return strings.TrimRight(parsed.Scheme+"://"+parsed.Host, "/")
	}
	return defaultCopilotAPIBase
}

func trustedCopilotAPIBase(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && u.Port() == "" && strings.HasSuffix(host, ".githubcopilot.com")
}

// ParseModels keeps the chat models of a Copilot model list, in order,
// without duplicates, with IDs the operator accepts, and at most MaxModels.
func ParseModels(body []byte) ([]cetirunv1alpha1.CopilotModel, error) {
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode GitHub Copilot model list: %w", err)
	}
	type rawModel struct {
		ID                 string   `json:"id"`
		Name               string   `json:"name"`
		Object             string   `json:"object"`
		ModelPickerEnabled *bool    `json:"model_picker_enabled"`
		SupportedEndpoints []string `json:"supported_endpoints"`
		Policy             *struct {
			State string `json:"state"`
		} `json:"policy"`
		Capabilities struct {
			Type   string `json:"type"`
			Limits struct {
				MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
				MaxPromptTokens        int64 `json:"max_prompt_tokens"`
				MaxOutputTokens        int64 `json:"max_output_tokens"`
			} `json:"limits"`
		} `json:"capabilities"`
	}
	models := []cetirunv1alpha1.CopilotModel{}
	seen := map[string]bool{}
	for _, raw := range payload.Data {
		var m rawModel
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		id := strings.TrimSpace(m.ID)
		if !modelIDPattern.MatchString(id) || seen[id] {
			continue
		}
		if m.Object != "" && m.Object != "model" {
			continue
		}
		if m.Capabilities.Type != "" && !strings.EqualFold(m.Capabilities.Type, "chat") {
			continue
		}
		if m.ModelPickerEnabled != nil && !*m.ModelPickerEnabled {
			continue
		}
		if m.Policy != nil && strings.EqualFold(m.Policy.State, "disabled") {
			continue
		}
		completions, responses := false, false
		for _, raw := range m.SupportedEndpoints {
			endpoint := strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
			completions = completions || completionsEndpoints[endpoint]
			responses = responses || responsesEndpoints[endpoint]
		}
		// Agents call chat completions, which the proxy passes on or, for a
		// model served only through the Responses API, bridges to it. A model
		// served only through another API, such as the Messages API, would
		// fail every call, so it is left out.
		if len(m.SupportedEndpoints) > 0 && !completions && !responses {
			continue
		}
		contextWindow := m.Capabilities.Limits.MaxContextWindowTokens
		if contextWindow <= 0 {
			contextWindow = m.Capabilities.Limits.MaxPromptTokens
		}
		name := strings.TrimSpace(m.Name)
		if len(name) > 200 {
			name = name[:200]
		}
		models = append(models, cetirunv1alpha1.CopilotModel{
			ID: id, Name: name, ContextWindow: max(contextWindow, 0), MaxOutputTokens: max(m.Capabilities.Limits.MaxOutputTokens, 0),
			// Copilot refuses chat completions for these; the proxy bridges.
			ResponsesOnly: responses && !completions,
		})
		seen[id] = true
		if len(models) == MaxModels {
			break
		}
	}
	return models, nil
}
