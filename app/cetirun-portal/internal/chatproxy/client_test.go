// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chatproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type inspectTransport func(*http.Request) (*http.Response, error)

func (f inspectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestClientCompleteUsesContentLength(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[streaming], func(t *testing.T) {
			request := Request{Model: "agent", Messages: []Message{{Role: "user", Content: "Hello 世界"}}, Stream: streaming}
			wantBody, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if len(r.TransferEncoding) != 0 || r.ContentLength != int64(len(wantBody)) || r.Header.Get("Content-Length") != strconv.Itoa(len(wantBody)) {
					t.Errorf("request framing: TransferEncoding=%v ContentLength=%d Content-Length=%q; want %d bytes without transfer encoding", r.TransferEncoding, r.ContentLength, r.Header.Get("Content-Length"), len(wantBody))
					http.Error(w, "content length required", http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != string(wantBody) {
					t.Errorf("request body = %q, err = %v; want %q", body, err, wantBody)
				}
				if r.Header.Get("X-Envoy-Max-Retries") != "0" {
					t.Error("mesh retries were not disabled")
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, textEvent+finishEvent+"data: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Hello 世界"},"finish_reason":"stop"}]}`)
				}
			}))
			defer upstream.Close()

			client := NewClient()
			transport := client.HTTP.Transport
			client.HTTP.Transport = inspectTransport(func(r *http.Request) (*http.Response, error) {
				if r.GetBody != nil {
					t.Error("POST body must not be replayable")
				}
				return transport.RoundTrip(r)
			})
			response := httptest.NewRecorder()
			client.Complete(context.Background(), response, Target{BaseURL: upstream.URL}, request, "test")
			if calls.Load() != 1 || response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Hello 世界") {
				t.Fatalf("calls=%d status=%d body=%s", calls.Load(), response.Code, response.Body.String())
			}
			if streaming && !strings.Contains(response.Body.String(), "data: [DONE]") {
				t.Fatal("stream did not complete")
			}
		})
	}
}
