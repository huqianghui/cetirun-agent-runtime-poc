// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeGitHub serves the GitHub and Copilot endpoints the Portal calls.
type fakeGitHub struct {
	t            *testing.T
	pollAnswers  []map[string]any
	exchangeCode int
	models       string
	apiBase      string
	requests     []string
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, "device")
		if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != ClientID || r.Form.Get("scope") != "read:user" {
			f.t.Errorf("device code form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev-123", "user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 5,
		})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, "poll")
		if err := r.ParseForm(); err != nil || r.Form.Get("device_code") != "dev-123" ||
			r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			f.t.Errorf("token form = %v", r.Form)
		}
		answer := f.pollAnswers[0]
		f.pollAnswers = f.pollAnswers[1:]
		_ = json.NewEncoder(w).Encode(answer)
	})
	mux.HandleFunc("GET /copilot_internal/v2/token", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, "exchange")
		if r.Header.Get("Authorization") != "Bearer gho_user" || r.Header.Get("Editor-Version") == "" {
			f.t.Errorf("exchange headers = %v", r.Header)
		}
		if f.exchangeCode != 0 {
			w.WriteHeader(f.exchangeCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tid=copilot", "endpoints": map[string]string{"api": f.apiBase}})
	})
	mux.HandleFunc("GET /models", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, "models")
		if r.Header.Get("Authorization") != "Bearer tid=copilot" {
			f.t.Errorf("models authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(f.models))
	})
	return mux
}

func newFake(t *testing.T) (*fakeGitHub, *Client) {
	f := &fakeGitHub{t: t}
	server := httptest.NewServer(f.handler())
	t.Cleanup(server.Close)
	f.apiBase = server.URL
	client := &Client{
		HTTP: server.Client(), GitHubURL: server.URL, GitHubAPIURL: server.URL,
		TrustAPIBase: func(u *url.URL) bool { return "http://"+u.Host == server.URL },
	}
	return f, client
}

func TestDeviceFlow(t *testing.T) {
	f, client := newFake(t)
	code, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code.UserCode != "ABCD-1234" || code.DeviceCode != "dev-123" || code.Interval != 5 || code.ExpiresIn != 900 {
		t.Fatalf("device code = %+v", code)
	}
	f.pollAnswers = []map[string]any{
		{"error": "authorization_pending"},
		{"error": "slow_down", "interval": 10},
		{"access_token": "gho_user", "token_type": "bearer"},
	}
	for _, want := range []PollResult{{Error: "authorization_pending"}, {Error: "slow_down", Interval: 10}, {AccessToken: "gho_user"}} {
		got, err := client.PollAccessToken(context.Background(), "dev-123")
		if err != nil || got != want {
			t.Fatalf("poll = %+v, %v; want %+v", got, err, want)
		}
	}
}

func TestListModelsKeepsChatModelsWithLimits(t *testing.T) {
	f, client := newFake(t)
	f.models = `{"data":[
	  {"id":"gpt-5.5","name":"GPT-5.5","object":"model","model_picker_enabled":true,
	   "capabilities":{"type":"chat","limits":{"max_context_window_tokens":400000,"max_output_tokens":128000}},
	   "supported_endpoints":["/chat/completions","/responses"]},
	  {"id":"claude-sonnet-4.5","name":"Claude Sonnet 4.5","capabilities":{"type":"chat","limits":{"max_prompt_tokens":128000,"max_output_tokens":16000}}},
	  {"id":"text-embedding-3-small","capabilities":{"type":"embeddings"}},
	  {"id":"gpt-4o-mini-hidden","model_picker_enabled":false,"capabilities":{"type":"chat"}},
	  {"id":"gpt-disabled","policy":{"state":"disabled"},"capabilities":{"type":"chat"}},
	  {"id":"completions-only","supported_endpoints":["/completions"],"capabilities":{"type":"chat"}},
	  {"id":"messages-only","supported_endpoints":["/v1/messages"],"capabilities":{"type":"chat"}},
	  {"id":"accounts/private","capabilities":{"type":"chat"}},
	  {"id":"gpt-5.5","capabilities":{"type":"chat"}},
	  {"id":"gpt-6-sol","name":"GPT-6 Sol","capabilities":{"type":"chat"},"supported_endpoints":["/responses","ws:/responses"]},
	  {"id":"claude-opus-4.5","capabilities":{"type":"chat"},"supported_endpoints":["/v1/messages","/chat/completions"]}
	]}`
	models, err := client.ListModels(context.Background(), "gho_user")
	if err != nil {
		t.Fatal(err)
	}
	// A model served only through the Messages API is left out: the proxy
	// would send it chat completions, which it does not serve.
	if len(models) != 4 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ResponsesOnly || models[1].ResponsesOnly {
		t.Fatalf("models served on chat completions are not responses-only: %+v", models[:2])
	}
	if m := models[2]; m.ID != "gpt-6-sol" || !m.ResponsesOnly {
		t.Fatalf("a model served only on /responses must be marked: %+v", m)
	}
	if m := models[3]; m.ID != "claude-opus-4.5" || m.ResponsesOnly {
		t.Fatalf("a model also served on chat completions is kept as a chat model: %+v", m)
	}
	if m := models[0]; m.ID != "gpt-5.5" || m.Name != "GPT-5.5" || m.ContextWindow != 400000 || m.MaxOutputTokens != 128000 {
		t.Fatalf("first model = %+v", m)
	}
	if m := models[1]; m.ID != "claude-sonnet-4.5" || m.ContextWindow != 128000 || m.MaxOutputTokens != 16000 {
		t.Fatalf("second model = %+v", m)
	}
	if strings.Join(f.requests, ",") != "exchange,models" {
		t.Fatalf("requests = %v", f.requests)
	}
}

func TestListModelsReportsAccountWithoutCopilot(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		f, client := newFake(t)
		f.exchangeCode = code
		if _, err := client.ListModels(context.Background(), "gho_user"); !errors.Is(err, ErrNoCopilot) {
			t.Fatalf("HTTP %d: err = %v, want ErrNoCopilot", code, err)
		}
	}
	f, client := newFake(t)
	f.exchangeCode = http.StatusBadGateway
	if _, err := client.ListModels(context.Background(), "gho_user"); err == nil || errors.Is(err, ErrNoCopilot) {
		t.Fatalf("HTTP 502: err = %v, want a temporary error", err)
	}
}

func TestAPIBaseAcceptsOnlyCopilotHosts(t *testing.T) {
	client := &Client{}
	for advertised, want := range map[string]string{
		"https://api.business.githubcopilot.com":      "https://api.business.githubcopilot.com",
		"https://api.individual.githubcopilot.com/":   "https://api.individual.githubcopilot.com",
		"https://attacker.example.com":                defaultCopilotAPIBase,
		"http://api.business.githubcopilot.com":       defaultCopilotAPIBase,
		"https://api.githubcopilot.com.example.com":   defaultCopilotAPIBase,
		"https://api.business.githubcopilot.com:8443": defaultCopilotAPIBase,
		"": defaultCopilotAPIBase,
	} {
		if got := client.apiBase(advertised); got != want {
			t.Errorf("apiBase(%q) = %q, want %q", advertised, got, want)
		}
	}
}

func TestParseModelsCapsAndValidatesIDs(t *testing.T) {
	var entries []string
	for i := 0; i < MaxModels+10; i++ {
		entries = append(entries, fmt.Sprintf(`{"id":"model-%d"}`, i))
	}
	entries = append([]string{`{"id":"bad id"}`, `{"id":"-leading"}`}, entries...)
	models, err := ParseModels([]byte(`{"data":[` + strings.Join(entries, ",") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != MaxModels {
		t.Fatalf("len = %d, want %d", len(models), MaxModels)
	}
	for _, m := range models {
		if !modelIDPattern.MatchString(m.ID) {
			t.Fatalf("invalid id kept: %q", m.ID)
		}
	}
	if _, err := ParseModels([]byte(`not json`)); err == nil {
		t.Fatal("want an error for a malformed list")
	}
}
