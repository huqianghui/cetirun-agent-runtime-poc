// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"

	"testing"
)

func TestOldAgentRoutesAreNotRegistered(t *testing.T) {
	server := &Server{mux: http.NewServeMux(), tlsMux: http.NewServeMux()}
	server.routes()
	for _, path := range []string{"/v1/auth/status", "/v1/auth/start-link", "/v1/credentials/issue"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.Code)
		}
	}
}

func TestListenersExposeSeparateRouteSets(t *testing.T) {
	server := &Server{mux: http.NewServeMux(), tlsMux: http.NewServeMux()}
	server.routes()

	httpResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(httpResponse, httptest.NewRequest(http.MethodPost, "/internal/v1/service-authorizations/list", nil))
	if httpResponse.Code != http.StatusNotFound {
		t.Fatalf("HTTP tool route status = %d, want 404", httpResponse.Code)
	}

	tlsResponse := httptest.NewRecorder()
	server.TLSHandler().ServeHTTP(tlsResponse, httptest.NewRequest(http.MethodGet, "/v1/connections", nil))
	if tlsResponse.Code != http.StatusNotFound {
		t.Fatalf("TLS Portal route status = %d, want 404", tlsResponse.Code)
	}

	callbackResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(callbackResponse, httptest.NewRequest(http.MethodGet, "/api/v1/connections/callback", nil))
	if callbackResponse.Code != http.StatusOK {
		t.Fatalf("anonymous callback status = %d, want 200", callbackResponse.Code)
	}
}

func TestToolServiceTLSPrincipalIsExact(t *testing.T) {
	cfg := Config{ToolServiceNamespace: "cetirun-system", ToolServiceAccount: "cetirun-connections-mcp"}
	valid, err := url.Parse("spiffe://cluster.local/ns/cetirun-system/sa/cetirun-connections-mcp")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/service-authorizations/list", nil)
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{valid}}}, VerifiedChains: [][]*x509.Certificate{{{URIs: []*url.URL{valid}}}}}
	if !toolServiceTLSPrincipalAllowed(request, cfg) {
		t.Fatal("valid tool service TLS identity was rejected")
	}
	other, err := url.Parse("spiffe://cluster.local/ns/cetirun-system/sa/other")
	if err != nil {
		t.Fatal(err)
	}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{valid, other}}}, VerifiedChains: [][]*x509.Certificate{{{URIs: []*url.URL{valid, other}}}}}
	if toolServiceTLSPrincipalAllowed(request, cfg) {
		t.Fatal("multiple tool service URI SANs were accepted")
	}
	request.TLS = nil
	request.Header.Set("X-Forwarded-Client-Cert", "URI=spiffe://cluster.local/ns/cetirun-system/sa/cetirun-connections-mcp")
	if toolServiceTLSPrincipalAllowed(request, cfg) {
		t.Fatal("XFCC was accepted without a verified TLS client certificate")
	}
}

func TestTLSListenerUsesMCPPermissionDeniedCode(t *testing.T) {
	server := &Server{cfg: Config{ToolServiceNamespace: "cetirun-system", ToolServiceAccount: "cetirun-connections-mcp"}, mux: http.NewServeMux(), tlsMux: http.NewServeMux()}
	server.routes()
	response := httptest.NewRecorder()
	server.TLSHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/internal/v1/service-authorizations/list", nil))
	var payload apiError
	if err := json.NewDecoder(response.Result().Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusForbidden || payload.Error.Code != "permission_denied" {
		t.Fatalf("TLS identity error = %d/%q, want %d/%q", response.Code, payload.Error.Code, http.StatusForbidden, "permission_denied")
	}
}

func TestIssuedMicrosoftTokenNeedsAnExpiry(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`))
	if _, err := jwtExpiry("header." + payload + ".signature"); err != nil {
		t.Fatalf("JWT expiry was rejected: %v", err)
	}
	if _, err := jwtExpiry("opaque-token"); err == nil {
		t.Fatal("opaque downstream token was accepted without an expiry")
	}
}

func TestToolAuthorizationErrorsUseMCPClientContract(t *testing.T) {
	for _, expected := range []toolAuthorizationError{
		{code: "unauthenticated", status: http.StatusUnauthorized},
		{code: "grant_required", status: http.StatusForbidden},
		{code: "permission_denied", status: http.StatusForbidden},
		{code: "interaction_required", status: http.StatusUnauthorized},
		{code: "invalid_request", status: http.StatusBadRequest},
		{code: "authorization_changed", status: http.StatusConflict},
		{code: "dependency_unavailable", status: http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/service-authorizations/check", nil)
		writeToolAuthorizationError(response, request, expected)
		var payload apiError
		if err := json.NewDecoder(response.Result().Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if response.Code != expected.status || payload.Error.Code != expected.code {
			t.Fatalf("tool error = %d/%q, want %d/%q", response.Code, payload.Error.Code, expected.status, expected.code)
		}
	}
}
