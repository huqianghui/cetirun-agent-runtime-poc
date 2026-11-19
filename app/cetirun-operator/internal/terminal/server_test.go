// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func proxyFixture(t *testing.T, handler http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	// A Unix socket path must fit in 108 bytes. t.TempDir() embeds the test
	// name, which makes the path longer than that on Windows.
	dir, err := os.MkdirTemp("", "tty")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "ttyd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	upstream := &http.Server{Handler: handler}
	go func() { _ = upstream.Serve(listener) }()
	t.Cleanup(func() { _ = upstream.Close() })
	proxy := NewServer(testConfig(), &fakeBackend{agent: testAgent()}, socket)
	proxy.Ready = func() bool { return true }
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return proxy, server
}

func TestTerminalProxyChecksIdentityOriginAndPaths(t *testing.T) {
	_, server := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	for _, test := range []struct{ path, identity, origin string }{
		{"/term/", "", ""}, {"/term/", "bob@example.com", ""},
		{"/term/", "alice@example.com", "https://foreign.example"},
		{"/term/", "alice@example.com", "null"},
		{"/term/?arg=other-pod", "alice@example.com", ""},
		{"/other/", "alice@example.com", ""},
	} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+test.path, nil)
		if test.identity != "" {
			request.Header.Set("X-Auth-Request-Email", test.identity)
		}
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%+v: %d", test, response.StatusCode)
		}
	}
}

func TestTerminalAliasUsesPreferredUsername(t *testing.T) {
	proxy, server := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	proxy.Reader.(*fakeBackend).agent.Spec.User.Email = ""
	for _, test := range []struct {
		name     string
		email    []string
		username []string
		want     int
	}{
		{"UPN differs from email", []string{"Alice.Smith@example.com"}, []string{"alice@example.com"}, 200},
		{"email alias cannot replace UPN", []string{"alice@example.com"}, []string{"bob@example.com"}, 403},
		{"email fallback", []string{"alice@example.com"}, nil, 200},
		{"UPN only", nil, []string{"alice@example.com"}, 200},
		{"duplicate email", []string{"alice@example.com", "bob@example.com"}, []string{"alice@example.com"}, 403},
		{"duplicate UPN", []string{"alice@example.com"}, []string{"alice@example.com", "bob@example.com"}, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodGet, server.URL+"/term/", nil)
			for _, value := range test.email {
				request.Header.Add("X-Auth-Request-Email", value)
			}
			for _, value := range test.username {
				request.Header.Add("X-Auth-Request-Preferred-Username", value)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("status=%d, want %d", response.StatusCode, test.want)
			}
		})
	}
}

func TestTerminalProxyDoesNotForwardBrowserCredentials(t *testing.T) {
	_, server := proxyFixture(t, func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" ||
			request.Header.Get("X-Auth-Request-Email") != "" || request.Header.Get("X-Internal-Instance") != "" {
			t.Error("browser identity/credentials leaked into ttyd")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head></head><body>Terminal</body></html>"))
	})
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/term/", nil)
	request.Header.Set("X-Auth-Request-Email", "alice@example.com")
	request.Header.Set("X-Internal-Instance", "foreign")
	request.Header.Set("Cookie", "synthetic-cookie")
	request.Header.Set("Authorization", "Bearer sk-dummy")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(body), `data-agent-uid="example-uid"`) ||
		!strings.Contains(string(body), "cetirun-reconnect.js") || strings.Contains(string(body), "sk-dummy") {
		t.Fatalf("unexpected proxy response: %d %s", response.StatusCode, body)
	}
}

func TestTerminalProxyInjectsReconnectScriptForCompressedBrowserRequests(t *testing.T) {
	for _, encoding := range []string{"gzip, deflate, br, zstd", "gzip", "identity"} {
		t.Run(encoding, func(t *testing.T) {
			_, server := proxyFixture(t, func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				body := []byte("<html><head></head><body>Terminal</body></html>")
				if strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
					w.Header().Set("Content-Encoding", "gzip")
					compressed := gzip.NewWriter(w)
					_, _ = compressed.Write(body)
					_ = compressed.Close()
					return
				}
				_, _ = w.Write(body)
			})
			request, _ := http.NewRequest(http.MethodGet, server.URL+"/term/", nil)
			request.Header.Set("X-Auth-Request-Email", "alice@example.com")
			request.Header.Set("Accept-Encoding", encoding)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" ||
				!strings.Contains(string(body), `<head><script src="./cetirun-reconnect.js"`) ||
				!strings.Contains(string(body), `<body>Terminal</body>`) {
				t.Fatalf("unexpected HTML response: status=%d encoding=%q body=%s", response.StatusCode,
					response.Header.Get("Content-Encoding"), body)
			}
		})
	}
}

func TestTerminalWebSocketReceivesOnlyServerIssuedHandle(t *testing.T) {
	var proxy *Server
	claims := make(chan Session, 1)
	proxy, server := proxyFixture(t, func(w http.ResponseWriter, request *http.Request) {
		handle := request.Header.Get(sessionHeader)
		if handle == "client-supplied" || len(handle) != 22 {
			t.Error("client controlled exec handle")
		}
		var err error
		claimed, err := proxy.Sessions.Claim(handle)
		if err != nil {
			t.Error(err)
		}
		claims <- claimed
		_, _ = w.Write([]byte("handshake fixture"))
	})
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/term/ws", nil)
	request.Header.Set("X-Auth-Request-Email", "alice@example.com")
	request.Header.Set(sessionHeader, "client-supplied")
	request.Header.Set("Origin", server.URL)
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	claimed := <-claims
	if claimed.AgentUID != "example-uid" || claimed.Identity.Value != "alice@example.com" {
		t.Fatal("exec handle was not bound to the authorized session")
	}
}

func TestTerminalReadinessDoesNotClaimAgentModelHealth(t *testing.T) {
	proxy, server := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {})
	proxy.Ready = func() bool { return false }
	for path, want := range map[string]int{"/healthz": 503, "/livez": 200} {
		response, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s: %d", path, response.StatusCode)
		}
	}
}
