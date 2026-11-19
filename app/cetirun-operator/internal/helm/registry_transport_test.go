// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stalledRegistry is a registry address that accepts connections and never
// answers, not even the TLS handshake.
func stalledRegistry(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}

// Helm's registry client has no timeout of its own: reading a chart from a
// registry that stalls ends at the time limit, or when the caller's context
// is cancelled, whichever comes first.
func TestInspectChartEndsWhenTheRegistryStalls(t *testing.T) {
	client := NewClient(ChartReference{RepoURL: "oci://" + stalledRegistry(t) + "/helm/cetirun", Version: "1.0.0"})
	previous := chartInspectionTimeout
	t.Cleanup(func() { chartInspectionTimeout = previous })

	chartInspectionTimeout = 500 * time.Millisecond
	start := time.Now()
	if _, err := client.InspectChart(context.Background(), ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stalled registry must end the read at the time limit, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the read took %s", elapsed)
	}

	chartInspectionTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start = time.Now()
	if _, err := client.InspectChart(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelling the caller's context must end the read, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the read took %s after cancellation", elapsed)
	}
}

// Like Helm's default registry transport, the inspection transport retries
// over plain HTTP when an HTTPS request reaches a plain HTTP registry, and
// keeps using HTTP.
func TestPlainHTTPFallbackReachesAPlainHTTPRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok " + r.URL.Path))
	}))
	defer server.Close()
	transport := &plainHTTPFallback{base: http.DefaultTransport.(*http.Transport).Clone()}
	client := &http.Client{Transport: transport}
	for _, path := range []string{"/v2/", "/v2/helm/cetirun/tags/list"} {
		resp, err := client.Get(strings.Replace(server.URL, "http://", "https://", 1) + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
	}
	if !transport.forceHTTP.Load() {
		t.Fatal("the transport must remember that the registry answers plain HTTP")
	}
}
