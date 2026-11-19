// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/chatproxy"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/chattest"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	"github.com/Azure/CetiRun/app/internal/chataccess"
)

type chatTransport func(*http.Request) (*http.Response, error)

func (f chatTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChatOwnerBindingAndForwarding(t *testing.T) {
	for _, mtls := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "common-mTLS"}[mtls], func(t *testing.T) {
			testChatOwnerBindingAndForwarding(t, mtls)
		})
	}
}

func testChatOwnerBindingAndForwarding(t *testing.T, mtls bool) {
	agent, objects := chattest.Objects()
	if mtls {
		agent, objects = chattest.MTLSObjects(t)
	}
	k := kube.NewClient(newFakeClient(objects...))
	target, err := k.ResolveChat(context.Background(), agent, chattest.Caller)
	if err != nil || !target.Available {
		t.Fatalf("fixture: %v %+v", err, target)
	}
	h := NewChatHandler(&InstanceHandler{Kube: k, Config: config.Config{CustomChatEnabled: true, ChatIdentityRealm: "test-realm", ChatCaller: chattest.Caller, PortalOrigin: "https://portal.example.com"}})
	var posts atomic.Int32
	h.Proxy = &chatproxy.Client{HTTP: &http.Client{Transport: chatTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Internal-Instance") != "" {
			t.Error("browser credentials or control headers reached Agent")
		}
		body := `{"object":"list","data":[{"id":"example-agent","object":"model"}]}`
		if r.Method == http.MethodPost {
			if r.Header.Get("X-Envoy-Max-Retries") != "0" {
				t.Error("caller input enabled mesh retries")
			}
			posts.Add(1)
			raw, _ := io.ReadAll(r.Body)
			if strings.Contains(string(raw), "conversationId") {
				t.Error("native session mapping introduced")
			}
			body = `{"choices":[{"index":0,"message":{"role":"assistant","content":"synthetic result"},"finish_reason":"stop"}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	makeRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/instances/chat-example/chat/v1/chat/completions", strings.NewReader(`{"model":"example-agent","messages":[{"role":"user","content":"synthetic task"}]}`))
		r.SetPathValue("name", agent.Name)
		r.Header.Set("Origin", h.Instances.Config.PortalOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CetiRun-Chat-Scope", chataccess.StorageScope("test-realm", "subject-alice"))
		r.Header.Set("X-CetiRun-Agent-UID", string(agent.UID))
		r.Header.Set("X-CetiRun-Chat-Binding", target.Revision)
		r.Header.Set("X-CetiRun-Request-ID", "33333333-3333-4333-8333-333333333333")
		r.Header.Set("Cookie", "test-secret")
		r.Header.Set("Authorization", "Bearer test-secret")
		r.Header.Set("X-Internal-Instance", "foreign")
		r.Header.Set("X-Envoy-Max-Retries", "9")
		return r.WithContext(auth.NewContext(r.Context(), auth.Claims{Alias: "alice", Subject: "subject-alice"}))
	}
	models := makeRequest()
	models.Method, models.URL.Path, models.Body = http.MethodGet, "/api/v1/instances/chat-example/chat/v1/models", nil
	w := httptest.NewRecorder()
	h.Models(w, models)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "example-agent") || posts.Load() != 0 {
		t.Fatalf("models: %d %s posts=%d", w.Code, w.Body.String(), posts.Load())
	}
	w = httptest.NewRecorder()
	h.Complete(w, makeRequest())
	if w.Code != 200 || posts.Load() != 1 || !strings.Contains(w.Body.String(), "synthetic result") {
		t.Fatalf("completion: %d %s posts=%d", w.Code, w.Body.String(), posts.Load())
	}
	cases := map[string]func(*http.Request){
		"foreign owner": func(r *http.Request) {
			*r = *r.WithContext(auth.NewContext(r.Context(), auth.Claims{Alias: "bob", Subject: "subject-alice", IsAdmin: true}))
		},
		"identity change":    func(r *http.Request) { r.Header.Set("X-CetiRun-Chat-Scope", "other") },
		"UID replacement":    func(r *http.Request) { r.Header.Set("X-CetiRun-Agent-UID", "different") },
		"stale target":       func(r *http.Request) { r.Header.Set("X-CetiRun-Chat-Binding", "different") },
		"missing origin":     func(r *http.Request) { r.Header.Del("Origin") },
		"cross site":         func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.example.com") },
		"query override":     func(r *http.Request) { r.URL.RawQuery = "url=https://untrusted.example.com" },
		"invalid request ID": func(r *http.Request) { r.Header.Set("X-CetiRun-Request-ID", "arbitrary") },
		"client tools": func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"model":"example-agent","messages":[{"role":"user","content":"hello"}],"tools":[]}`))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := makeRequest()
			mutate(r)
			w := httptest.NewRecorder()
			h.Complete(w, r)
			if w.Code < 400 || posts.Load() != 1 {
				t.Fatalf("request escaped gate: %d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("X-CetiRun-Chat-Dispatch") != "not-sent" {
				t.Fatal("rejection dispatch state is not explicit")
			}
		})
	}
	h.Instances.Config.CustomChatEnabled = false
	w = httptest.NewRecorder()
	h.Complete(w, makeRequest())
	if w.Code != 404 {
		t.Fatal("disabled Chat remained reachable")
	}
}

func TestMeScopeNeverReturnsSubject(t *testing.T) {
	h := &MeHandler{ChatIdentityRealm: "test-realm"}
	r := httptest.NewRequest("GET", "/api/v1/me", nil)
	r = r.WithContext(auth.NewContext(r.Context(), auth.Claims{Alias: "alice", Subject: "private-subject-sentinel"}))
	w := httptest.NewRecorder()
	h.GetMe(w, r)
	var value MeResponse
	_ = json.Unmarshal(w.Body.Bytes(), &value)
	if value.ChatStorageScope == "" || strings.Contains(w.Body.String(), "private-subject-sentinel") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invalid local-history identity response")
	}
}
