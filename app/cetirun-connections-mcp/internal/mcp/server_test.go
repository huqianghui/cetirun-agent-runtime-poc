// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/identity"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/tools"
)

type fakeService struct {
	authCalls int
	listCalls int
	calls     int
	result    map[string]any
	err       error
	authErr   error
}

func (f *fakeService) Authenticate(_ context.Context, bearer string) (identity.Caller, error) {
	f.authCalls++
	if f.authErr != nil {
		return identity.Caller{}, f.authErr
	}
	if bearer != "synthetic-workload-proof" {
		return identity.Caller{}, identity.ErrUnauthenticated
	}
	return identity.Caller{Proof: bearer, InstanceUID: "00000000-0000-0000-0000-000000000001"}, nil
}
func (f *fakeService) List(context.Context, identity.Caller) ([]tools.Spec, error) {
	f.listCalls++
	return []tools.Spec{}, nil
}
func (f *fakeService) Call(context.Context, identity.Caller, string, map[string]any) (map[string]any, error) {
	f.calls++
	if f.result == nil {
		return map[string]any{}, f.err
	}
	return f.result, f.err
}

func TestToolsCallRejectsUnknownArgumentsBeforeService(t *testing.T) {
	fake := &fakeService{}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"service_request","arguments":{"connection_id":"conn-demo","service":"azure_arm","method":"GET","path":"/subscriptions","extra":"no"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	response := httptest.NewRecorder()
	New(fake).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if fake.calls != 0 {
		t.Fatalf("service calls = %d, want 0", fake.calls)
	}
	if !strings.Contains(response.Body.String(), `"isError":true`) || !strings.Contains(response.Body.String(), `invalid_request`) || !strings.Contains(response.Body.String(), `"correlation_id":"`) {
		t.Fatalf("unexpected response %s", response.Body.String())
	}
}

func TestToolsCallPreservesProviderDiagnosticsForNativeClients(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		result map[string]any
		err    error
		status float64
		code   string
	}{
		{"provider-denied", map[string]any{"status": 403, "body": map[string]any{"error": "ProviderDenied"}, "correlation_id": "trace-id", "request_ids": []string{"upstream-id"}}, nil, 403, ""},
		{"response-read-failed", nil, &tools.ToolError{Code: "execution_unknown", CorrelationID: "trace-id", ProviderStatus: 201, RequestIDs: []string{"upstream-id"}}, 201, "execution_unknown"},
		{"response-too-large", map[string]any{"status": 202, "body": strings.Repeat("x", maxToolResultBytes), "correlation_id": "trace-id", "request_ids": []string{"upstream-id"}}, nil, 202, "execution_unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fake := &fakeService{result: scenario.result, err: scenario.err}
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"service_request","arguments":{"connection_id":"conn-demo","service":"azure_arm","method":"POST","path":"/providers/Microsoft.ResourceGraph/resources","body":{"query":"Resources"}}}}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
			request.Header.Set("MCP-Protocol-Version", protocolVersion)
			response := httptest.NewRecorder()
			New(fake).ServeHTTP(response, request)
			var payload struct {
				Result struct {
					IsError    bool                    `json:"isError"`
					Content    []struct{ Text string } `json:"content"`
					Structured map[string]any          `json:"structuredContent"`
				} `json:"result"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil || !payload.Result.IsError || len(payload.Result.Content) != 1 || fake.calls != 1 {
				t.Fatalf("invalid error response: %d %s", response.Code, response.Body.String())
			}
			var textResult map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(payload.Result.Content[0].Text, untrustedExternalDataNotice)), &textResult) != nil || !reflect.DeepEqual(textResult, payload.Result.Structured) {
				t.Fatal("text-only clients lost structured diagnostics")
			}
			statusKey := "status"
			if scenario.code != "" {
				statusKey = "provider_status"
				if textResult["code"] != scenario.code {
					t.Fatalf("wrong error code: %#v", textResult)
				}
			} else if !reflect.DeepEqual(textResult["body"], map[string]any{"error": "ProviderDenied"}) {
				t.Fatal("provider error body was lost")
			}
			if textResult[statusKey] != scenario.status || textResult["correlation_id"] != "trace-id" || !reflect.DeepEqual(textResult["request_ids"], []any{"upstream-id"}) {
				t.Fatalf("provider diagnostics were lost: %#v", textResult)
			}
		})
	}
}

func TestMCPRequiresBearerProof(t *testing.T) {
	for _, method := range []string{"initialize", "ping"} {
		t.Run(method, func(t *testing.T) {
			fake := &fakeService{}
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("MCP-Protocol-Version", protocolVersion)
			response := httptest.NewRecorder()
			New(fake).ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "unauthenticated") || fake.authCalls != 0 || fake.listCalls != 0 || fake.calls != 0 {
				t.Fatalf("unexpected response or service calls: %d %s %#v", response.Code, response.Body.String(), fake)
			}
		})
	}
}

func TestMCPAuthenticationFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   int
	}{
		{"rejected credential", identity.ErrUnauthenticated, http.StatusUnauthorized, -32001},
		{"wrapped rejection", fmt.Errorf("synthetic detail: %w", identity.ErrUnauthenticated), http.StatusUnauthorized, -32001},
		{"dependency outage", identity.ErrDependencyUnavailable, http.StatusServiceUnavailable, -32000},
		{"wrapped outage", fmt.Errorf("synthetic detail: %w", identity.ErrDependencyUnavailable), http.StatusServiceUnavailable, -32000},
		{"deadline", context.DeadlineExceeded, http.StatusServiceUnavailable, -32000},
		{"unexpected error", errors.New("synthetic private diagnostic"), http.StatusServiceUnavailable, -32000},
	} {
		for _, method := range []string{"initialize", "ping", "tools/list", "tools/call", "notifications/initialized"} {
			t.Run(test.name+"/"+method, func(t *testing.T) {
				fake := &fakeService{authErr: test.err}
				message := map[string]any{"jsonrpc": "2.0", "method": method}
				if method != "notifications/initialized" {
					message["id"] = "request-1"
				}
				if method == "tools/call" {
					message["params"] = map[string]any{"name": "connections_status", "arguments": map[string]any{}}
				}
				body, err := json.Marshal(message)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
				request.Header.Set("MCP-Protocol-Version", protocolVersion)
				response := httptest.NewRecorder()
				New(fake).ServeHTTP(response, request)
				if response.Code != test.status || fake.authCalls != 1 || fake.listCalls != 0 || fake.calls != 0 {
					t.Fatalf("status/auth/list/call = %d/%d/%d/%d, want %d/1/0/0", response.Code, fake.authCalls, fake.listCalls, fake.calls, test.status)
				}
				if method == "notifications/initialized" {
					if response.Body.Len() != 0 {
						t.Fatal("notification returned a JSON-RPC response")
					}
					return
				}
				wantMessage := "dependency_unavailable"
				if test.status == http.StatusUnauthorized {
					wantMessage = "unauthenticated"
				}
				var payload rpcResponse
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.JSONRPC != "2.0" || string(payload.ID) != `"request-1"` || payload.Result != nil || payload.Error == nil || payload.Error.Code != test.code || payload.Error.Message != wantMessage || payload.Error.Data != nil {
					t.Fatalf("unexpected authentication response: %s", response.Body.String())
				}
			})
		}
	}
}

func TestInitializeNegotiatesSupportedVersionAfterAuthentication(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-01-01"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
	response := httptest.NewRecorder()
	New(&fakeService{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"protocolVersion":"2025-11-25"`) || !strings.Contains(response.Body.String(), `"version":"`+Version+`"`) || !strings.Contains(response.Body.String(), `"name":"cetirun-connections-mcp"`) {
		t.Fatalf("unexpected negotiation response: %d %s", response.Code, response.Body.String())
	}
}

func TestPingReturnsEmptyResultAfterAuthentication(t *testing.T) {
	for _, id := range []string{`"heartbeat-1"`, `1`, `0`, `9007199254740993`} {
		t.Run(id, func(t *testing.T) {
			fake := &fakeService{}
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}`, id)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
			request.Header.Set("MCP-Protocol-Version", protocolVersion)
			response := httptest.NewRecorder()
			New(fake).ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected ping response: %d %s", response.Code, response.Body.String())
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload) != 3 || string(payload["jsonrpc"]) != `"2.0"` || string(payload["id"]) != id || string(payload["result"]) != `{}` {
				t.Fatalf("unexpected ping response: %s", response.Body.String())
			}
			if fake.authCalls != 1 || fake.listCalls != 0 || fake.calls != 0 {
				t.Fatalf("auth/list/call = %d/%d/%d, want 1/0/0", fake.authCalls, fake.listCalls, fake.calls)
			}
		})
	}
}

func TestToolsCallPreservesPaginationAndMarksExternalData(t *testing.T) {
	fake := &fakeService{result: map[string]any{"value": "completed", "nextPageToken": "opaque-page"}}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"service_request","arguments":{"connection_id":"conn-demo","service":"azure_arm","method":"GET","path":"/subscriptions"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	response := httptest.NewRecorder()
	New(fake).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Result.Content) != 1 || payload.Result.Content[0].Text != untrustedExternalDataNotice+`{"nextPageToken":"opaque-page","value":"completed"}` || payload.Result.Structured["value"] != "completed" {
		t.Fatalf("unexpected tool result %#v", payload.Result)
	}
}

func TestToolsCallRejectsOversizedBusinessData(t *testing.T) {
	fake := &fakeService{result: map[string]any{"value": strings.Repeat("x", maxToolResultBytes)}}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"service_request","arguments":{"connection_id":"conn-demo","service":"azure_arm","method":"GET","path":"/subscriptions"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	response := httptest.NewRecorder()
	New(fake).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"isError":true`) || !strings.Contains(response.Body.String(), "execution_unknown") {
		t.Fatalf("oversized response = %d %s", response.Code, response.Body.String())
	}
}

func TestMCPHTTPGuards(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		headers map[string]string
		status  int
	}{
		{name: "strict trailing payload", body: `{"jsonrpc":"2.0","id":1,"method":"initialize"}{}`, headers: map[string]string{"Authorization": "Bearer synthetic-workload-proof"}, status: http.StatusBadRequest},
		{name: "protocol header after initialize", body: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, headers: map[string]string{"Authorization": "Bearer synthetic-workload-proof"}, status: http.StatusBadRequest},
		{name: "ping protocol header", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, headers: map[string]string{"Authorization": "Bearer synthetic-workload-proof"}, status: http.StatusBadRequest},
		{name: "cross origin", body: `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, headers: map[string]string{"Authorization": "Bearer synthetic-workload-proof", "Origin": "https://untrusted.example.com"}, status: http.StatusForbidden},
		{name: "unsupported accept", body: `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, headers: map[string]string{"Authorization": "Bearer synthetic-workload-proof", "Accept": "text/plain"}, status: http.StatusNotAcceptable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			for key, value := range test.headers {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()
			New(&fakeService{}).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestMCPNotificationHasNoResponse(t *testing.T) {
	fake := &fakeService{}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	response := httptest.NewRecorder()
	New(fake).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Body.Len() != 0 {
		t.Fatalf("notification response = %d %q", response.Code, response.Body.String())
	}
}

func TestMCPExecutableMethodsRequireID(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		headers map[string]string
	}{
		{name: "null id", body: `{"jsonrpc":"2.0","id":null,"method":"initialize"}`},
		{name: "object id", body: `{"jsonrpc":"2.0","id":{},"method":"initialize"}`},
		{name: "boolean id", body: `{"jsonrpc":"2.0","id":true,"method":"initialize"}`},
		{name: "initialize", body: `{"jsonrpc":"2.0","method":"initialize"}`},
		{name: "ping", body: `{"jsonrpc":"2.0","method":"ping"}`, headers: map[string]string{"MCP-Protocol-Version": protocolVersion}},
		{name: "tools list", body: `{"jsonrpc":"2.0","method":"tools/list"}`, headers: map[string]string{"MCP-Protocol-Version": protocolVersion}},
		{name: "tools call", body: `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"service_request","arguments":{"connection_id":"conn-demo","service":"azure_arm","method":"GET","path":"/subscriptions"}}}`, headers: map[string]string{"MCP-Protocol-Version": protocolVersion}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeService{}
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer synthetic-workload-proof")
			for key, value := range test.headers {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()
			New(fake).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || fake.authCalls != 0 || fake.listCalls != 0 || fake.calls != 0 {
				t.Fatalf("status/auth/list/call = %d/%d/%d/%d, want 400/0/0/0", response.Code, fake.authCalls, fake.listCalls, fake.calls)
			}
		})
	}
}
