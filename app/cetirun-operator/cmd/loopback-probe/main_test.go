// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeAcceptsOnlySuccessfulNumericLoopbackHTTP(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/redirect":
			http.Redirect(w, r, target.URL, http.StatusFound)
		case "/slow":
			time.Sleep(3 * time.Second)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	if err := probe(server.URL + "/healthz"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/bad", "/redirect", "/slow"} {
		if err := probe(server.URL + path); err == nil {
			t.Fatalf("accepted failed health endpoint %s", path)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("followed a redirect beyond the original loopback endpoint")
	}
	for _, endpoint := range []string{
		"https://127.0.0.1:8080/healthz", "http://localhost:8080/healthz", "http://example.test:8080/healthz",
		"http://127.0.0.1/healthz", "http://127.0.0.1:0/healthz", "http://127.0.0.1:65536/healthz",
		"http://127.0.0.1:18446744073709551616/healthz", "http://127.0.0.1:8080/healthz?query",
		"http://user@127.0.0.1:8080/healthz", "http://127.0.0.1:8080/healthz#fragment", "http://[bad",
	} {
		if err := probe(endpoint); err == nil {
			t.Fatalf("accepted invalid probe endpoint %q", endpoint)
		}
	}
}

func TestProbeDoesNotTreatHTTPOnlyReadinessAsMTLSReadiness(t *testing.T) {
	t.Setenv("CETIRUN_INGRESS_MTLS", "true")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := run([]string{"--url", server.URL + "/healthz"}); err == nil {
		t.Fatal("HTTP success alone satisfied the mTLS readiness contract")
	}
}

func TestProbeCommandRejectsUnexpectedArgumentsAndDestinations(t *testing.T) {
	for _, arguments := range [][]string{nil, {"--url"}, {"install", "/tmp/other"}, {"--url", "http://127.0.0.1:8080/healthz", "extra"}} {
		if err := run(arguments); err == nil {
			t.Fatalf("accepted invalid probe invocation %v", arguments)
		}
	}
}

func TestProbeRequiresExplicitEgressCheckWithOrWithoutMTLS(t *testing.T) {
	t.Setenv("CETIRUN_EGRESS_SNI", "true")
	for _, arguments := range [][]string{
		{"--url", "http://127.0.0.1:8080/healthz"},
		{"--url", "http://127.0.0.1:8080/healthz", "--mtls-server-name", "agent.example.com"},
	} {
		if err := run(arguments); err == nil || !strings.Contains(err.Error(), "explicit managed-proxy probe") {
			t.Fatalf("missing egress probe did not fail before other health checks: %v", err)
		}
	}
}

func TestHeadlessAndHTTPProbeRequireManagedProxyReadiness(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:15440")
	if err != nil {
		t.Fatal(err)
	}
	var status atomic.Int32
	status.Store(http.StatusOK)
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			t.Error("probe used a non-readiness proxy endpoint")
		}
		w.WriteHeader(int(status.Load()))
	}))
	proxy.Listener.Close()
	proxy.Listener = listener
	proxy.Start()
	defer proxy.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer app.Close()
	t.Setenv("CETIRUN_EGRESS_SNI", "true")
	for _, arguments := range [][]string{
		{"--egress-sni"},
		{"--url", app.URL + "/healthz", "--egress-sni"},
	} {
		status.Store(http.StatusOK)
		if err := run(arguments); err != nil {
			t.Fatal("ready managed proxy did not satisfy the explicit probe:", err)
		}
		status.Store(http.StatusServiceUnavailable)
		if err := run(arguments); err == nil {
			t.Fatal("application HTTP success masked unavailable egress capture")
		}
	}
}
