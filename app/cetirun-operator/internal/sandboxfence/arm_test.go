// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

type measuredBody struct {
	io.ReadCloser
	read   *atomic.Int64
	closed *atomic.Bool
}

func (b measuredBody) Read(buffer []byte) (int, error) {
	count, err := b.ReadCloser.Read(buffer)
	b.read.Add(int64(count))
	return count, err
}

func (b measuredBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

type measuredTransport struct {
	base   http.RoundTripper
	read   atomic.Int64
	closed atomic.Bool
	calls  atomic.Int64
}

func (t *measuredTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	response, err := t.base.RoundTrip(request)
	if err == nil {
		response.Body = measuredBody{ReadCloser: response.Body, read: &t.read, closed: &t.closed}
	}
	return response, err
}

func TestResponseBoundPrecedesSDKBuffering(t *testing.T) {
	for _, test := range []struct {
		method  string
		status  int
		size    int
		chunked bool
	}{
		{http.MethodGet, 200, 16, false},
		{http.MethodGet, 200, maxResponseBytes, false},
		{http.MethodGet, 200, maxResponseBytes + 1, false},
		{http.MethodGet, 200, maxResponseBytes + 65536, true},
		{http.MethodGet, 500, maxResponseBytes + 65536, false},
		{http.MethodGet, 500, maxResponseBytes + 65536, true},
		{http.MethodDelete, 202, maxResponseBytes + 65536, false},
		{http.MethodDelete, 503, maxResponseBytes + 65536, true},
	} {
		name := fmt.Sprintf("%s-%d-%d-chunked=%t", test.method, test.status, test.size, test.chunked)
		t.Run(name, func(t *testing.T) {
			payload := strings.Repeat("x", test.size)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != test.method {
					t.Errorf("unexpected method %q", r.Method)
				}
				if !test.chunked {
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				}
				w.WriteHeader(test.status)
				if test.chunked {
					w.(http.Flusher).Flush()
				}
				// A bounded client may close the connection before this write completes.
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			httpClient := server.Client()
			measured := &measuredTransport{base: httpClient.Transport}
			httpClient.Transport = measured
			client, err := makeARMClient(fixtureCredential{}, server.URL, httpClient)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if test.method == http.MethodDelete {
				ctx = context.WithValue(ctx, attemptKey{}, &atomic.Bool{})
			}
			request, err := runtime.NewRequest(ctx, test.method, client.resourceURL(fixturePool, APIVersion))
			if err != nil {
				t.Fatal(err)
			}
			if test.method == http.MethodDelete {
				request.Raw().Header.Set("If-Match", fixtureRun)
			}
			// Bypass the application's post-pipeline reader: the SDK itself must be bounded.
			response, err := client.pipeline.Do(request)
			if test.size > maxResponseBytes {
				var limitError *http.MaxBytesError
				if !errors.As(err, &limitError) || limitError.Limit != maxResponseBytes {
					t.Fatalf("SDK buffered an oversized response without a size error: %v", err)
				}
				if measured.read.Load() > maxResponseBytes+1 {
					t.Fatalf("SDK consumed %d bytes before the limit", measured.read.Load())
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, err := runtime.Payload(response)
				if err != nil || len(body) != test.size || measured.read.Load() != int64(test.size) {
					t.Fatalf("in-bound response changed: bytes=%d consumed=%d error=%v", len(body), measured.read.Load(), err)
				}
			}

			if response != nil {
				response.Body.Close()
			}
			if measured.calls.Load() != 1 || !measured.closed.Load() {
				t.Fatalf("response was retried or leaked: calls=%d closed=%t", measured.calls.Load(), measured.closed.Load())
			}
		})
	}
}

func TestOperationMonitorURLKeepsScopeAndIndependentVersion(t *testing.T) {
	f := newARMFixture(t)
	base := f.server.URL + fixtureOperation
	for _, test := range []struct {
		name  string
		url   string
		valid bool
	}{
		{"resource version", base + "?api-version=" + APIVersion, true},
		{"independent monitor version", base + "?api-version=2025-03-01&h=synthetic%2Bcursor", true},
		{"preview monitor", base + "?api-version=2025-03-01-preview", true},
		{"missing version", base, false},
		{"empty version", base + "?api-version=", false},
		{"duplicate version", base + "?api-version=2025-03-01&api-version=2026-05-01", false},
		{"ambiguous casing", base + "?api-version=2025-03-01&API-VERSION=2026-05-01", false},
		{"invalid version", base + "?api-version=latest", false},
		{"invalid calendar", base + "?api-version=2025-13-01", false},
		{"invalid query encoding", base + "?api-version=2025-03-01&h=%ZZ", false},
		{"fragment", base + "?api-version=2025-03-01#unexpected", false},
		{"foreign subscription", strings.Replace(base, fixtureSubscription, fixtureReplacement, 1) + "?api-version=2025-03-01", false},
		{"foreign provider", strings.Replace(base, "Microsoft.ContainerService", "Microsoft.Compute", 1) + "?api-version=2025-03-01", false},
		{"foreign host", "https://example.com" + fixtureOperation + "?api-version=2025-03-01", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := f.client.operationURL(test.url, f.reservation()); (err == nil) != test.valid {
				t.Fatalf("monitor URL valid=%t, want %t: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestAuthoritativeAbsenceResponseShapes(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		absent bool
	}{
		{"bare AKS error", 404, `{"code":"NotFound","message":"Synthetic missing pool","details":[{"code":"Unspecified"}]}`, true},
		{"ARM envelope", 404, `{"error":{"code":"ResourceNotFound","message":"Synthetic missing VM"}}`, true},
		{"missing parent", 404, `{"error":{"code":"ParentResourceNotFound"}}`, true},
		{"wrong status", 403, `{"code":"NotFound"}`, false},
		{"authorization", 404, `{"code":"AuthorizationFailed"}`, false},
		{"enveloped authorization", 404, `{"error":{"code":"Forbidden"}}`, false},
		{"conflicting shapes", 404, `{"code":"NotFound","error":{"code":"AuthorizationFailed"}}`, false},
		{"ambiguous matching shapes", 404, `{"code":"NotFound","error":{"code":"NotFound"}}`, false},
		{"null envelope with code", 404, `{"code":"NotFound","error":null}`, false},
		{"duplicate bare code", 404, `{"code":"AuthorizationFailed","code":"NotFound"}`, false},
		{"duplicate root message", 404, `{"code":"NotFound","message":"first","message":"second"}`, false},
		{"duplicate nested code", 404, `{"error":{"code":"Forbidden","code":"NotFound"}}`, false},
		{"duplicate envelope message", 404, `{"error":{"code":"NotFound","message":"first","message":"second"}}`, false},
		{"duplicate detail field", 404, `{"code":"NotFound","details":[{"code":"Forbidden","code":"NotFound"}]}`, false},
		{"duplicate envelope", 404, `{"error":{"code":"Forbidden"},"error":{"code":"NotFound"}}`, false},
		{"null", 404, `null`, false},
		{"null code", 404, `{"code":null}`, false},
		{"invalid code type", 404, `{"code":42}`, false},
		{"missing code", 404, `{"message":"not found"}`, false},
		{"invalid envelope", 404, `{"error":"NotFound"}`, false},
		{"HTML", 404, `<html>Not found</html>`, false},
		{"empty", 404, ``, false},
		{"trailing JSON", 404, `{"code":"NotFound"}{"code":"Forbidden"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := authoritativeAbsent(response{status: test.status, body: []byte(test.body)}); got != test.absent {
				t.Fatalf("authoritative absence=%t, want %t", got, test.absent)
			}
		})
	}
}
