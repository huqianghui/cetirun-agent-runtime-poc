// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chatproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestSubset(t *testing.T) {
	valid := `{"model":"agent","messages":[{"role":"user","content":"hello"}],"stream":true}`
	if _, err := DecodeRequest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"model":"a","model":"b","messages":[]}`, strings.Replace(valid, `"stream":true`, `"stream":null`, 1), strings.Replace(valid, `"stream":true`, `"tools":[]`, 1), strings.Replace(valid, `"content":"hello"`, `"content":null`, 1), strings.Replace(valid, `"role":"user"`, `"role":"system"`, 1), strings.Replace(valid, "hello", strings.Repeat("x", InputBytes+1), 1)} {
		if _, err := DecodeRequest([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid request %.120s", raw)
		}
	}
}

const textEvent = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello 世界\"},\"finish_reason\":null}]}\n\n"
const finishEvent = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

func TestStreamTerminalStatesAndRedaction(t *testing.T) {
	cases := []struct {
		name, body string
		success    bool
	}{
		{"normal", textEvent + finishEvent + "data: [DONE]\n\n", true},
		{"CRLF", strings.ReplaceAll(textEvent+finishEvent+"data: [DONE]\n\n", "\n", "\r\n"), true},
		{"CR", strings.ReplaceAll(textEvent+finishEvent+"data: [DONE]\n\n", "\n", "\r"), true},
		{"length", textEvent + strings.Replace(finishEvent, "stop", "length", 1) + "data: [DONE]\n\n", true},
		{"filter", textEvent + strings.Replace(finishEvent, "stop", "content_filter", 1) + "data: [DONE]\n\n", true},
		{"multi-line", strings.Replace(textEvent, "{\"choices\":", "{\ndata: \"choices\":", 1) + finishEvent + "data: [DONE]\n\n", true},
		{"usage", textEvent + finishEvent + "data: {\"choices\":[],\"usage\":{\"total_tokens\":3}}\n\ndata: [DONE]\n\n", true},
		{"done alone", "data: [DONE]\n\n", false},
		{"EOF", textEvent, false}, {"finish without done", textEvent + finishEvent, false},
		{"error then done", textEvent + "data: {\"error\":{\"message\":\"secret-sentinel\"}}\n\ndata: [DONE]\n\n", false},
		{"tool", `data: {"choices":[{"index":0,"delta":{"tool_calls":[]}}]}` + "\n\n", false},
		{"oversized", "data: " + strings.Repeat("x", FrameBytes) + "\n\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			err := stream(strings.NewReader(tc.body), &eventWriter{w: w}, "test", "agent")
			if (err == nil) != tc.success {
				t.Fatalf("success=%v err=%v", tc.success, err)
			}
			if strings.Contains(w.Body.String(), "secret-sentinel") {
				t.Fatal("raw upstream error leaked")
			}
			if !tc.success && strings.Contains(w.Body.String(), "[DONE]") {
				t.Fatal("failure forged success")
			}
		})
	}
}

type byteReader struct{ io.Reader }

func (r byteReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:1]) }

func TestStreamPreservesUTF8AcrossReads(t *testing.T) {
	w := httptest.NewRecorder()
	err := stream(byteReader{strings.NewReader(textEvent + finishEvent + "data: [DONE]\n\n")}, &eventWriter{w: w}, "test", "agent")
	if err != nil || !strings.Contains(w.Body.String(), "Hello 世界") {
		t.Fatalf("split UTF-8 stream: %v %s", err, w.Body.String())
	}
}

func TestClientDoesNotFollowRedirectOrReplayPost(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials forwarded")
		}
		if r.Header.Get("X-Envoy-Max-Retries") != "0" {
			t.Error("mesh retries were not explicitly disabled")
		}
		w.Header().Set("Set-Cookie", "credential=secret-sentinel")
		w.Header().Set("Location", "http://outside.example.com/")
		w.WriteHeader(302)
	}))
	defer upstream.Close()
	c := NewClient()
	w := httptest.NewRecorder()
	c.Complete(context.Background(), w, Target{BaseURL: upstream.URL}, Request{Model: "agent", Messages: []Message{{Role: "user", Content: "hello"}}}, "test")
	if calls.Load() != 1 || w.Code != 502 || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Location") != "" {
		t.Fatalf("unsafe redirect response: %d %v", w.Code, w.Header())
	}
}

func TestClientDistinguishesReportedFailureFromStreamLoss(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost", true: "reported"}[reported], func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, textEvent)
				if reported {
					_, _ = io.WriteString(w, "data: {\"error\":{\"code\":\"tool_budget_exceeded\",\"message\":\"secret-sentinel\"}}\n\n")
				}
			}))
			defer upstream.Close()
			w := httptest.NewRecorder()
			NewClient().Complete(context.Background(), w, Target{BaseURL: upstream.URL}, Request{Model: "agent", Stream: true}, "test")
			body := w.Body.String()
			if calls.Load() != 1 || strings.Contains(body, "secret-sentinel") || strings.Contains(body, "[DONE]") {
				t.Fatalf("unexpected replay, disclosure or completion: %s", body)
			}
			if reported {
				if !strings.Contains(body, `"code":"agent_reported_error"`) || !strings.Contains(body, "Earlier tool actions may have completed") || strings.Contains(body, "may still be running") {
					t.Fatalf("reported failure treated as unknown transport loss: %s", body)
				}
			} else if !strings.Contains(body, `"code":"agent_stream_interrupted"`) || !strings.Contains(body, "may still be running") {
				t.Fatalf("stream loss lost its uncertainty: %s", body)
			}
		})
	}
}

func TestClientModelsAndCancellation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"agent","object":"model","secret":"secret-sentinel"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, textEvent)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()
	c := NewClient()
	target := Target{BaseURL: upstream.URL}
	models, err := c.Models(context.Background(), target)
	raw, _ := json.Marshal(models)
	if err != nil || strings.Contains(string(raw), "secret") {
		t.Fatalf("models: %s %v", raw, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Complete(ctx, httptest.NewRecorder(), target, Request{Model: "agent", Stream: true}, "test")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel not propagated")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestClientForwardsOnlyRestrictiveMemoryHeader(t *testing.T) {
	for _, mode := range []string{"off", "no-capture", "", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expected := mode
				if mode == "enabled" {
					expected = ""
				}
				if r.Header.Get("X-CetiRun-Memory-Mode") != expected {
					t.Error("wrong restriction forwarded")
				}
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "Memory") || strings.Contains(string(body), "memory") {
					t.Error("HTTP body contract changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"fixture"},"finish_reason":"stop"}]}`)
			}))
			defer upstream.Close()
			w := httptest.NewRecorder()
			NewClient().Complete(context.Background(), w, Target{BaseURL: upstream.URL}, Request{Model: "fixture", MemoryMode: mode}, "fixture")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestLimiterBoundsAndRelease(t *testing.T) {
	var l Limiter
	release, ok := l.Acquire("a", "one")
	if !ok {
		t.Fatal("first denied")
	}
	if _, ok := l.Acquire("b", "one"); ok {
		t.Fatal("parallel Agent admitted")
	}
	release()
	release()
	release, ok = l.Acquire("b", "one")
	if !ok {
		t.Fatal("release failed")
	}
	release()
}
