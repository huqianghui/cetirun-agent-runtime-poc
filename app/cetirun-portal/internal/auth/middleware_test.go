// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusUnauthorized, "test_code", "test message")

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}

	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("response missing 'error' field")
	}
	if errObj["code"] != "test_code" {
		t.Errorf("error code = %v, want %q", errObj["code"], "test_code")
	}
	if errObj["message"] != "test message" {
		t.Errorf("error message = %v, want %q", errObj["message"], "test message")
	}
}

func TestMiddlewareUsesPortalOnlyIdentityTokenHeader(t *testing.T) {
	tenant := "11111111-1111-1111-1111-111111111111"
	client := "22222222-2222-2222-2222-222222222222"
	object := "33333333-3333-3333-3333-333333333333"
	verifier, err := NewOIDCVerifier(tenant, client)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier.keys["test-key"] = &key.PublicKey
	verifier.fetched = time.Now()
	token := signedToken(t, key, map[string]any{
		"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"aud": client, "tid": tenant, "oid": object,
		"login_hint": "opaque-login-hint==",
		"nbf":        time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	})
	var captured Claims
	handler := Middleware(nil, verifier)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Auth-Request-Preferred-Username", "alice@example.com")
	req.Header.Set(portalIdentityTokenHeader, "Bearer "+token)
	req.Header.Set("Authorization", "Bearer client-controlled-value")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || captured.TenantID != tenant || captured.ObjectID != object || captured.LoginHint != "opaque-login-hint==" {
		t.Fatalf("Portal-only identity token was not verified: status=%d claims=%#v", w.Code, captured)
	}
}

func TestMiddleware_MissingAuthHeaders(t *testing.T) {
	handler := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestMiddleware_ValidAuthHeaders(t *testing.T) {
	var capturedClaims Claims
	handler := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		if !ok {
			t.Error("claims not found in context")
		}
		capturedClaims = claims
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Auth-Request-Preferred-Username", "alice@example.com")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if capturedClaims.Alias != "alice" {
		t.Errorf("alias = %q, want %q", capturedClaims.Alias, "alice")
	}
	if capturedClaims.Email != "alice@example.com" {
		t.Errorf("email = %q, want %q", capturedClaims.Email, "alice@example.com")
	}
	if capturedClaims.DisplayName != "alice@example.com" {
		t.Errorf("displayName = %q, want %q", capturedClaims.DisplayName, "alice@example.com")
	}
	if capturedClaims.LoginHint != "alice@example.com" {
		t.Errorf("loginHint = %q, want %q", capturedClaims.LoginHint, "alice@example.com")
	}
}

func TestMiddleware_LoginHintPrefersTenantUsernameOverEmail(t *testing.T) {
	var capturedClaims Claims
	handler := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims, _ = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Auth-Request-Preferred-Username", "tenant-user@example.onmicrosoft.com")
	req.Header.Set("X-Auth-Request-Email", "alternate@example.com")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if capturedClaims.LoginHint != "tenant-user@example.onmicrosoft.com" {
		t.Errorf("loginHint = %q, want tenant preferred username", capturedClaims.LoginHint)
	}
}

func TestMiddleware_InvalidAlias(t *testing.T) {
	handler := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Auth-Request-Preferred-Username", "123invalid@example.com") // starts with number
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}
