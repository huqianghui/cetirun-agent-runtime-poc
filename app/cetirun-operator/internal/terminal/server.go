// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
)

//go:embed reconnect.js
var reconnectScript []byte

const sessionHeader = "X-CetiRun-Terminal-Session"

type Server struct {
	Config   Config
	Reader   AgentReader
	Sessions *SessionStore
	Proxy    *httputil.ReverseProxy
	Ready    func() bool
}

func NewServer(cfg Config, reader AgentReader, socket string) *Server {
	target := &url.URL{Scheme: "http", Host: "ttyd"}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = request.In.Host
			// HTML injection needs an uncompressed response from the private ttyd hop.
			request.Out.Header.Set("Accept-Encoding", "identity")
			request.Out.Header.Del("Cookie")
			request.Out.Header.Del("Authorization")
			for key := range request.Out.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-auth-request-") ||
					strings.HasPrefix(strings.ToLower(key), "x-internal-") {
					request.Out.Header.Del(key)
				}
			}
		},
		Transport: &http.Transport{
			DisableCompression:    true,
			ResponseHeaderTimeout: 10 * time.Second,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
			},
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Terminal is temporarily unavailable", http.StatusServiceUnavailable)
		},
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode == http.StatusSwitchingProtocols && strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
			stream, ok := response.Body.(io.ReadWriteCloser)
			if !ok {
				return fmt.Errorf("terminal upgrade has no duplex stream")
			}
			response.Body = &terminalWebSocket{ReadWriteCloser: stream}
			return nil
		}
		if response.StatusCode != http.StatusOK ||
			!strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
			return nil
		}
		content, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
		_ = response.Body.Close()
		if err != nil || len(content) > 4*1024*1024 || !bytes.Contains(content, []byte("<head>")) {
			return fmt.Errorf("unexpected terminal page")
		}
		// Load before ttyd's application code so even a failed first WebSocket
		// handshake is observed. The script is same-origin, not a CDN dependency.
		tag := `<head><script src="./cetirun-reconnect.js" data-agent-uid="` + cfg.AgentUID + `"></script>`
		content = bytes.Replace(content, []byte("<head>"), []byte(tag), 1)
		response.Body = io.NopCloser(bytes.NewReader(content))
		response.ContentLength = int64(len(content))
		response.Header.Set("Content-Length", strconv.Itoa(len(content)))
		response.Header.Set("Cache-Control", "no-store")
		return nil
	}
	return &Server{
		Config: cfg, Reader: reader, Sessions: NewSessionStore(), Proxy: proxy,
		Ready: func() bool {
			connection, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
			if err != nil {
				return false
			}
			_ = connection.Close()
			return true
		},
	}
}

func identityFrom(request *http.Request) Identity {
	for _, name := range []string{"X-Auth-Request-Email", "X-Auth-Request-Preferred-Username"} {
		if len(request.Header.Values(name)) > 1 {
			return Identity{}
		}
	}
	// Use Portal's UPN-first alias and retain full-email ownership matching.
	// Only the authenticated gateway may supply these identity headers.
	identity := Identity{
		Value:             request.Header.Get("X-Auth-Request-Email"),
		PreferredUsername: request.Header.Get("X-Auth-Request-Preferred-Username"),
	}
	if identity.Value == "" {
		identity.Value = identity.PreferredUsername
	}
	return identity
}

func sameOrigin(request *http.Request) bool {
	value := request.Header.Get("Origin")
	if value == "" {
		return !strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	}
	origin, err := url.Parse(value)
	return err == nil && (origin.Scheme == "https" || origin.Scheme == "http") &&
		origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" &&
		strings.EqualFold(origin.Host, request.Host)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if request.Method == http.MethodGet && request.URL.Path == "/livez" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		if !s.Ready() {
			http.Error(w, "Terminal is starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if request.Method != http.MethodGet ||
		(request.URL.Path != BasePath && !strings.HasPrefix(request.URL.Path, BasePath+"/")) ||
		request.URL.RawPath != "" || request.URL.Query().Has("arg") || !sameOrigin(request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	identity := identityFrom(request)
	if !identity.Valid() {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	err := Authorize(ctx, s.Reader, s.Config, identity)
	cancel()
	if err != nil {
		status := http.StatusServiceUnavailable
		if denied(err) {
			status = http.StatusForbidden
		}
		http.Error(w, "Terminal access is unavailable", status)
		return
	}
	switch request.URL.Path {
	case BasePath + "/cetirun-reconnect.js":
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write(reconnectScript)
		return
	case BasePath + "/readyz":
		if !s.Ready() {
			http.Error(w, "Terminal is starting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "agentUID": s.Config.AgentUID})
		return
	}
	upstream := request.Clone(request.Context())
	upstream.Header.Set(sessionHeader, "authenticated")
	if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		key, err := s.Sessions.Create(Session{
			Identity: identity, AgentUID: s.Config.AgentUID, Expires: time.Now().Add(s.Config.SessionTTL),
		})
		if err != nil {
			http.Error(w, "Too many terminal connections", http.StatusTooManyRequests)
			return
		}
		defer s.Sessions.Delete(key)
		upstream.Header.Set(sessionHeader, key)
		slog.Info("Terminal connection authorized", "agent", s.Config.AgentName, "user", identity.Value)
		defer slog.Info("Terminal connection closed", "agent", s.Config.AgentName, "user", identity.Value)
	}
	s.Proxy.ServeHTTP(w, upstream)
}

func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /claim/{id}", func(w http.ResponseWriter, r *http.Request) {
		session, err := s.Sessions.Claim(r.PathValue("id"))
		if err != nil {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(session)
	})
	return mux
}

func ClaimSession(ctx context.Context, socket, key string) (Session, error) {
	if !filepath.IsAbs(socket) || len(key) != 22 {
		return Session{}, ErrDenied
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/claim/"+url.PathEscape(key), nil)
	if err != nil {
		return Session{}, ErrDenied
	}
	response, err := client.Do(request)
	if err != nil {
		return Session{}, ErrDenied
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Session{}, ErrDenied
	}
	var session Session
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&session); err != nil {
		return Session{}, ErrDenied
	}
	return session, nil
}

func superviseTTYD(ctx context.Context, socket, control string) {
	delay := time.Second
	for ctx.Err() == nil {
		_ = os.Remove(socket) // Socket inside this process's private temporary directory.
		command := exec.CommandContext(ctx, "/ttyd",
			"--interface", socket, "--base-path", BasePath,
			"--auth-header", sessionHeader, "--writable", "--check-origin",
			"--max-clients", "4", "--signal", "15", "--debug", "4", "/terminal", "exec")
		command.Env = append(os.Environ(), "CETIRUN_TERMINAL_CONTROL_SOCKET="+control)
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
		command.WaitDelay = 5 * time.Second
		started := time.Now()
		if err := command.Run(); ctx.Err() == nil {
			slog.Warn("ttyd stopped; restarting", "error", err)
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		if wait(ctx, delay) != nil {
			return
		}
		delay = min(delay*2, 10*time.Second)
	}
}

// watchShell is part of the container entrypoint. It waits through Pod
// replacement, probes each new container, and fails explicitly if none of the
// supported shells can run. It never starts an interactive session by itself.
func watchShell(ctx context.Context, cfg Config, backend Backend, interval time.Duration, ready func(bool)) error {
	var checked Snapshot
	for {
		request, cancel := context.WithTimeout(ctx, 10*time.Second)
		agent, err := backend.Agent(request)
		var target Snapshot
		if err == nil {
			identity := Identity{Value: agent.Spec.User.Alias}
			target, err = backend.Resolve(request, identity)
			if err == nil && target != checked {
				_, err = backend.DetectShell(request)
				current, checkErr := backend.Resolve(request, identity)
				if checkErr != nil || current != target {
					err = ErrWaiting
				}
			}
		}
		cancel()
		ready(err == nil)
		if errors.Is(err, terminalpolicy.ErrNoShell) || denied(err) {
			return err
		}
		if err == nil {
			checked = target
		}
		if err := wait(ctx, interval); err != nil {
			return err
		}
	}
}

func Serve(parent context.Context, cfg Config, backend Backend) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	directory, err := os.MkdirTemp("", "cetirun-terminal-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	socket, control := filepath.Join(directory, "ttyd.sock"), filepath.Join(directory, "control.sock")
	server := NewServer(cfg, backend, socket)
	socketReady := server.Ready
	var shellReady atomic.Bool
	server.Ready = func() bool { return shellReady.Load() && socketReady() }
	controlListener, err := net.Listen("unix", control)
	if err != nil {
		return err
	}
	defer controlListener.Close()
	if err := os.Chmod(control, 0600); err != nil {
		return err
	}
	controlServer := &http.Server{Handler: server.ControlHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = controlServer.Serve(controlListener) }()
	supervisorDone := make(chan struct{})
	go func() {
		defer close(supervisorDone)
		superviseTTYD(ctx, socket, control)
	}()

	var connections sync.Map
	public := &http.Server{
		Addr: net.JoinHostPort(cfg.BindHost, strconv.Itoa(cfg.Port)), Handler: server,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024,
		ConnState: func(connection net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				connections.Delete(connection)
			} else {
				connections.Store(connection, true)
			}
		},
	}
	results := make(chan error, 2)
	go func() { results <- public.ListenAndServe() }()
	go func() { results <- watchShell(ctx, cfg, backend, 3*time.Second, shellReady.Store) }()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-results:
	}
	cancel()
	_ = public.Close()
	_ = controlServer.Close()
	connections.Range(func(key, _ any) bool { _ = key.(net.Conn).Close(); return true })
	<-supervisorDone
	if err == http.ErrServerClosed || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
