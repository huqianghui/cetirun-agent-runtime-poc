// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func restRequest() Request {
	return Request{ConnectionID: "connection", Service: "azure_arm", Method: "POST", Path: "/providers/Microsoft.ResourceGraph/resources", Body: json.RawMessage(`{"query":"Resources | take 2"}`)}
}
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestArbitraryServiceRoutesAndProviderAuthority(t *testing.T) {
	for _, item := range []struct{ service, path, method, host, fullPath string }{
		{"azure_arm", "/subscriptions/example/resourcegroups/new", "PUT", "management.azure.com", "/subscriptions/example/resourcegroups/new"},
		{"azure_arm", "/providers/Microsoft.ResourceGraph/resources", "POST", "management.azure.com", "/providers/Microsoft.ResourceGraph/resources"},
		{"azure_arm", "/subscriptions", "GET", "management.azure.com", "/subscriptions"},
		{"github", "/repos/example/project/issues", "POST", "api.github.com", "/repos/example/project/issues"},
		{"microsoft_graph", "/me/drive/root/children", "GET", "graph.microsoft.com", "/v1.0/me/drive/root/children"},
		{"google_gmail", "/users/me/messages", "GET", "gmail.googleapis.com", "/gmail/v1/users/me/messages"},
		{"google_calendar", "/calendars/primary/events", "POST", "www.googleapis.com", "/calendar/v3/calendars/primary/events"},
	} {
		t.Run(item.service+item.path, func(t *testing.T) {
			calls := 0
			client := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != item.host || r.URL.Path != item.fullPath || r.Method != item.method || r.Header.Get("Authorization") != "Bearer synthetic-token" || r.GetBody != nil {
					t.Fatalf("unexpected service request: %s", r.URL)
				}
				if r.Header.Get("X-CetiRun-Correlation-ID") != "correlation" {
					t.Error("provider request lost platform correlation")
				}
				return jsonResponse(403, `{"error":{"code":"ProviderDenied"},"nextPageToken":"opaque-page"}`), nil
			}))
			input := Request{ConnectionID: "c", Service: item.service, Path: item.path, Method: item.method, Query: map[string]any{"pageToken": "a+b /?"}}
			result, err := client.Execute(context.Background(), input, "synthetic-token", "correlation")
			if err != nil || result.Status != 403 || calls != 1 || result.Body.(map[string]any)["nextPageToken"] != "opaque-page" {
				t.Fatalf("provider response lost: %#v, %v", result, err)
			}
		})
	}
}
func TestRejectsUnsafeRequestBeforeCredentialUse(t *testing.T) {
	tests := []Request{}
	for _, p := range []string{"https://evil.example/api", "//evil.example/api", "/a/../b", "/%2e%2e/b", "/a%2fb", "/a%5cb", "/%252e%252e/b", "/a?token=x", "/a#fragment", "/a//b", "/a\r\nx", "/a%00b", "/a/%2E/b"} {
		r := restRequest()
		r.Path = p
		tests = append(tests, r)
	}
	for _, h := range []string{"Authorization", "Host", "Cookie", "X-Forwarded-Host", "Proxy-Authorization", "Content-Type", "Accept", "Idempotency-Key", "X-CetiRun-Correlation-ID"} {
		r := restRequest()
		r.Headers = map[string]string{h: "value"}
		tests = append(tests, r)
	}
	r := restRequest()
	r.Headers = map[string]string{"If-Match": "x\r\nAuthorization: injected"}
	tests = append(tests, r)
	r = restRequest()
	r.Headers = map[string]string{"If-Match": "one", "if-match": "two"}
	tests = append(tests, r)
	r = restRequest()
	r.Query = map[string]any{"access_token": "override"}
	tests = append(tests, r)
	r = restRequest()
	r.Method = "CONNECT"
	tests = append(tests, r)
	r = restRequest()
	r.Service = "https://evil.example"
	tests = append(tests, r)
	r = restRequest()
	r.Body = json.RawMessage(`{"x":"` + strings.Repeat("x", MaxBodyBytes) + `"}`)
	tests = append(tests, r)
	calls := 0
	client := New(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not call") }))
	for _, input := range tests {
		_, err := client.Execute(context.Background(), input, "synthetic-token", "correlation")
		if err == nil || err.Error() != "invalid_request" {
			t.Fatalf("unsafe input accepted %#v: %v", input, err)
		}
	}
	if calls != 0 {
		t.Fatal("unsafe input reached upstream")
	}
}
func TestRedirectsResponseBoundsAndNoRetry(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		response *http.Response
		err      error
		code     string
	}{
		{"redirect", &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://evil.example"}}, Body: io.NopCloser(strings.NewReader(""))}, nil, "redirect_rejected"},
		{"transport", nil, errors.New("failed with sensitive details"), "execution_unknown"},
		{"large", jsonResponse(201, `{"value":"`+strings.Repeat("a", MaxResponseBytes)+`"}`), nil, "execution_unknown"},
		{"non-json", &http.Response{StatusCode: 201, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader("binary"))}, nil, "execution_unknown"},
		{"invalid-json", jsonResponse(201, `{"value":`), nil, "execution_unknown"},
		{"trailing-json", jsonResponse(201, `{} {}`), nil, "execution_unknown"},
		{"credential-echo", jsonResponse(200, `{"nested":[{"value":"Bearer synthetic-token"}]}`), nil, "execution_unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			client := New(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return scenario.response, scenario.err }))
			_, err := client.Execute(context.Background(), restRequest(), "synthetic-token", "c")
			if err == nil || err.Error() != scenario.code || calls != 1 {
				t.Fatalf("outcome = %v; calls = %d", err, calls)
			}
		})
	}
}
func TestProductionTransportDoesNotReplayRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = c.Close()
	}))
	defer server.Close()
	client := New(nil)
	transport := client.http.Transport.(*http.Transport)
	if !transport.DisableKeepAlives || transport.ForceAttemptHTTP2 || transport.Proxy != nil || transport.TLSNextProto == nil {
		t.Fatal("transport permits proxy or replay")
	}
	// Replace only network address/trust with a local synthetic server.
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	for _, method := range []string{"GET", "POST", "PUT"} {
		input := restRequest()
		input.Method = method
		if method == "GET" {
			input.Body = nil
		}
		before := calls.Load()
		_, err := client.Execute(context.Background(), input, "synthetic-token", "c")
		if err == nil || err.Error() != "execution_unknown" || calls.Load() != before+1 {
			t.Fatalf("%s replayed: %v, %d", method, err, calls.Load()-before)
		}
	}
}

func TestProviderCredentialEchoNeverReachesResultsOrDiagnostics(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		body            string
		requestID       string
		wantRequestIDs  []string
		wantUnknownBody bool
	}{
		{name: "safe Azure request ID", body: `{"value":"safe","nextPageToken":"opaque-page"}`, requestID: "safe-request-id", wantRequestIDs: []string{"safe-request-id"}},
		{name: "credential in Azure request ID", body: `{"value":"safe","nextPageToken":"opaque-page"}`, requestID: "Bearer synthetic-token", wantRequestIDs: []string{}},
		{name: "credential escaped in body", body: `{"nested":{"synthetic-\u0074oken":"echo"}}`, requestID: "safe-request-id", wantRequestIDs: []string{"safe-request-id"}, wantUnknownBody: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			response := jsonResponse(201, scenario.body)
			response.Header.Set("x-ms-request-id", scenario.requestID)
			response.Header.Set("x-ms-correlation-request-id", "bad\r\nheader")
			response.Header.Set("location", "https://management.azure.com/?access_token=synthetic-token")
			response.Header.Set("etag", "safe-etag")
			client := New(roundTripFunc(func(*http.Request) (*http.Response, error) { return response, nil }))
			result, err := client.Execute(context.Background(), restRequest(), "synthetic-token", "trace-id")
			if result.Status != 201 || !reflect.DeepEqual(result.RequestIDs, scenario.wantRequestIDs) || len(result.Headers) != 1 || result.Headers["etag"] != "safe-etag" {
				t.Fatalf("unsafe or missing diagnostics: %#v", result)
			}
			if scenario.wantUnknownBody {
				if err == nil || err.Error() != "execution_unknown" || result.Body != nil {
					t.Fatal("escaped credential echo was returned")
				}
			} else if err != nil || result.Body.(map[string]any)["nextPageToken"] != "opaque-page" {
				t.Fatal("safe response data was lost")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "synthetic-token") {
				t.Fatal("injected credential leaked through response")
			}
		})
	}
}
func TestPrivateAndSpecialDNSAddressesRejected(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.100.100.200", "192.168.1.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::a00:1", "0.0.0.0"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Errorf("accepted %s", ip)
		}
	}
	if !publicIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("rejected public address")
	}
	if _, err := publicDial(context.Background(), "tcp", "evil.example:443"); err == nil {
		t.Fatal("unconfigured host accepted")
	}
}
