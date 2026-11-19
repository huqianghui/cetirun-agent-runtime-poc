// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const maxRequestBody = 64 << 10

type Server struct {
	logger    *slog.Logger
	cfg       Config
	store     *kubeClient
	oauth     *oauthClient
	authSDK   *authSDKClient
	providers *providerClient
	mux       *http.ServeMux
	tlsMux    *http.ServeMux
	now       func() time.Time
}

func NewServer(cfg Config) (*Server, error) {
	wrapper := newKeyVaultWrapper(cfg)
	store, err := newKubeClient(cfg, wrapper)
	if err != nil {
		return nil, err
	}
	server := &Server{cfg: cfg, store: store, oauth: newOAuthClient(cfg), authSDK: newAuthSDKClient(cfg), providers: newProviderClient(cfg), mux: http.NewServeMux(), tlsMux: http.NewServeMux()}
	server.routes()
	return server, nil
}

func (s *Server) Handler() http.Handler { return requestSecurityHeaders(s.auditManagement(s.mux)) }

func (s *Server) TLSHandler() http.Handler {
	return requestSecurityHeaders(s.auditManagement(s.tlsMux))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// Provider availability is checked by the relevant request, not Pod readiness.
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("DELETE /operator/v1/instances/{uid}/authorization", s.operatorDeleteAuthorization)
	s.mux.HandleFunc("PUT /operator/v1/instances/{uid}/credential-policy", s.operatorCredentialPolicy)
	s.mux.HandleFunc("GET /operator/v1/instances/{uid}/credential-policy", s.operatorCredentialPolicy)
	s.mux.HandleFunc("DELETE /operator/v1/instances/{uid}/credential-policy", s.operatorCredentialPolicy)
	s.mux.Handle("GET /v1/connections", s.portalOnly(s.connectionsList))
	s.mux.Handle("POST /v1/connections/start", s.portalOnly(s.connectionStart))
	s.mux.Handle("GET /v1/connections/github-app/config", s.portalOnly(s.githubAppConfig))
	s.mux.Handle("GET /v1/connections/{id}/github/installations", s.portalOnly(s.githubInstallations))
	s.mux.Handle("GET /v1/connections/{id}/github/repositories", s.portalOnly(s.githubRepositories))
	s.mux.Handle("POST /v1/connections/{id}/github/installation", s.portalOnly(s.githubInstallationSelect))
	s.mux.Handle("GET /v1/connection-authorizations/{id}", s.portalOnly(s.authorizationGet))
	s.mux.Handle("GET /v1/connections/{id}/usage", s.portalOnly(s.connectionUsage))
	s.mux.Handle("GET /v1/connections/{id}", s.portalOnly(s.connectionGet))
	s.mux.Handle("DELETE /v1/connections/{id}", s.portalOnly(s.connectionDelete))
	s.mux.Handle("POST /v1/connections/{id}/reconnect", s.portalOnly(s.connectionReconnect))
	s.mux.HandleFunc("GET /api/v1/connections/callback", s.connectionCallback)
	s.mux.HandleFunc("POST /api/v1/connections/callback", s.connectionCallback)
	s.mux.Handle("POST /v1/instances/{name}/tool-identities", s.portalOnly(s.agentIdentityCreate))
	s.mux.Handle("GET /v1/instances/{name}/connection-bindings", s.portalOnly(s.bindingsList))
	s.mux.Handle("POST /v1/instances/{name}/connection-bindings", s.portalOnly(s.bindingCreate))
	s.mux.Handle("PUT /v1/instances/{name}/connection-bindings/{id}", s.portalOnly(s.bindingUpdate))
	s.mux.Handle("DELETE /v1/instances/{name}/connection-bindings/{id}", s.portalOnly(s.bindingDelete))
	s.tlsMux.Handle("POST /internal/v1/service-authorizations/list", s.toolOnly(s.toolAuthorizationsList))
	s.tlsMux.Handle("POST /internal/v1/service-authorizations/check", s.toolOnly(s.toolAuthorizationsCheck))
	s.tlsMux.Handle("POST /internal/v1/service-credentials/issue", s.toolOnly(s.toolCredentialsIssue))
}

func requestSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		r.Header.Set("X-Correlation-ID", correlationID(r))
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) portalOnly(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !portalPrincipalAllowed(r.Header.Get("X-Forwarded-Client-Cert"), s.cfg) {
			writeAPIError(w, http.StatusForbidden, "forbidden", "Verified Portal workload identity is required", correlationID(r))
			return
		}
		if owner, err := ownerFromPortal(r); err == nil {
			auditOwner(r, owner)
		}
		next(w, r)
	})
}

func (s *Server) toolOnly(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !toolServiceTLSPrincipalAllowed(r, s.cfg) {
			writeAPIError(w, http.StatusForbidden, "permission_denied", "Verified tool service TLS identity is required", correlationID(r))
			return
		}
		next(w, r)
	})
}

func (s *Server) operatorDeleteAuthorization(w http.ResponseWriter, r *http.Request) {
	if !operatorPrincipalAllowed(r.Header.Get("X-Forwarded-Client-Cert"), s.cfg) {
		writeAPIError(w, http.StatusForbidden, "forbidden", "Verified operator workload identity is required", correlationID(r))
		return
	}
	agent, err := s.store.getAgentByUID(r.Context(), r.PathValue("uid"))
	if err != nil || agent.UID != r.PathValue("uid") {
		writeAPIError(w, http.StatusNotFound, "instance_not_found", "Instance not found", correlationID(r))
		return
	}
	auditTarget(r, "", agent.UID, "")
	auditMutation(r)
	if err := s.store.deleteCredentialPolicy(r.Context(), agent); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Credential policy cleanup is pending", correlationID(r))
		return
	}
	if err := s.store.deleteAgentRecords(r.Context(), agent); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Binding state could not be revoked", correlationID(r))
		return
	}
	auditCommit(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) callbackPage(w http.ResponseWriter, success bool, ids ...string) {
	id := ""
	if len(ids) == 1 {
		id = ids[0]
	}
	status, message := "failed", "Sign-in could not be completed. Close this window and retry from the Portal."
	if success {
		status, message = "complete", "Sign-in complete. You may close this window."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	templatePage := template.Must(template.New("callback").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Connection</title></head><body><p>{{.Message}}</p><script>if(window.opener){window.opener.postMessage({type:"cetirun-connection",status:{{.Status}},authorizationId:{{.AuthorizationID}}},{{.Origin}});}window.close();</script></body></html>`))
	_ = templatePage.Execute(w, map[string]string{"AuthorizationID": id, "Status": status, "Origin": s.cfg.PortalOrigin, "Message": message})
}

func (s *Server) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func operatorPrincipalAllowed(xfcc string, cfg Config) bool {
	principal, err := workloadPrincipal(xfcc)
	return err == nil && principal == [2]string{cfg.OperatorNamespace, cfg.OperatorServiceAccount}
}
func portalPrincipalAllowed(xfcc string, cfg Config) bool {
	principal, err := workloadPrincipal(xfcc)
	return err == nil && principal == [2]string{cfg.PortalNamespace, cfg.PortalServiceAccount}
}

func workloadPrincipal(xfcc string) ([2]string, error) {
	var principals [][2]string
	for _, entry := range strings.Split(xfcc, ",") {
		for _, part := range strings.Split(entry, ";") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, "URI=") {
				continue
			}
			raw := strings.Trim(strings.TrimPrefix(part, "URI="), `"`)
			parsed, err := url.Parse(raw)
			if err != nil || parsed.Scheme != "spiffe" || parsed.Host != "cluster.local" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				continue
			}
			segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
			if len(segments) == 4 && segments[0] == "ns" && segments[2] == "sa" && dnsLabel(segments[1]) && dnsLabel(segments[3]) {
				principals = append(principals, [2]string{segments[1], segments[3]})
			}
		}
	}
	if len(principals) != 1 {
		return [2]string{}, errors.New("exactly one verified workload principal is required")
	}
	return principals[0], nil
}

func authorizationRandomValues(generate func(int) (string, error)) (string, string, string, error) {
	state, err := generate(32)
	if err != nil {
		return "", "", "", err
	}
	verifier, err := generate(48)
	if err != nil {
		return "", "", "", err
	}
	nonce, err := generate(32)
	if err != nil {
		return "", "", "", err
	}
	return state, verifier, nonce, nil
}

func validOAuthState(value string) bool {
	if len(value) != 43 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validAuthorizationCode(value string) bool {
	if len(value) == 0 || len(value) > 8192 {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func sameIdentityID(actual, expected string) bool {
	return actual != "" && expected != "" && strings.EqualFold(actual, expected)
}

func accessTokenFromAuthorizationHeader(authorization string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return "", errors.New("Auth SDK returned an invalid credential")
	}
	token := strings.TrimPrefix(authorization, prefix)
	if token == "" || len(token) > 32<<10 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("Auth SDK returned an invalid credential")
	}
	return token, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid", correlationID(r))
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "The request body must contain one JSON value", correlationID(r))
		return false
	}
	return true
}

func correlationID(r *http.Request) string {
	if value := r.Header.Get("X-Correlation-ID"); value != "" && len(value) <= 128 && agenttools.AuditText(value, 128) == value {
		return value
	}
	value, err := randomValue(16)
	if err != nil {
		return "unavailable"
	}
	return value
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeAPIError(w http.ResponseWriter, status int, code, message, correlation string) {
	value := apiError{}
	value.Error.Code, value.Error.Message, value.Error.CorrelationID = code, message, correlation
	writeJSON(w, status, value)
}
