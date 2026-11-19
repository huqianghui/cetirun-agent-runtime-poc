// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewAdminSet_NormalizesEntries(t *testing.T) {
	s := NewAdminSet([]string{" Alice ", "BOB", "", "alice", "  "})
	if !s.Contains("alice") {
		t.Errorf("expected alice in set")
	}
	if !s.Contains("Alice") {
		t.Errorf("expected case-insensitive match for Alice")
	}
	if !s.Contains("bob") {
		t.Errorf("expected bob in set")
	}
	if len(s) != 2 {
		t.Errorf("expected 2 entries after dedup, got %d", len(s))
	}
}

func TestAdminSet_Contains_Empty(t *testing.T) {
	var s AdminSet
	if s.Contains("alice") {
		t.Errorf("nil set should not contain anyone")
	}
	if NewAdminSet(nil).Contains("alice") {
		t.Errorf("empty set should not contain anyone")
	}
	if NewAdminSet([]string{"alice"}).Contains("") {
		t.Errorf("empty alias should never match")
	}
}

func TestRequireAdmin_NoClaims(t *testing.T) {
	h := RequireAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without claims")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestRequireAdmin_NonAdmin(t *testing.T) {
	h := RequireAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for non-admin")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams", nil)
	ctx := NewContext(req.Context(), Claims{Alias: "alice", IsAdmin: false})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestRequireAdmin_Admin(t *testing.T) {
	called := false
	h := RequireAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams", nil)
	ctx := NewContext(req.Context(), Claims{Alias: "alice", IsAdmin: true})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !called {
		t.Errorf("admin handler should have been called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestMiddleware_SetsIsAdminFromAllowlist(t *testing.T) {
	admins := NewAdminSet([]string{"alice"})
	var captured Claims
	h := Middleware(admins)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// Admin user.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("X-Auth-Request-Preferred-Username", "alice@example.com")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !captured.IsAdmin {
		t.Errorf("alice should be admin")
	}

	// Non-admin user.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("X-Auth-Request-Preferred-Username", "bob@example.com")
	req.Header.Set("X-Auth-Request-Email", "bob@example.com")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured.IsAdmin {
		t.Errorf("bob should not be admin")
	}
}
