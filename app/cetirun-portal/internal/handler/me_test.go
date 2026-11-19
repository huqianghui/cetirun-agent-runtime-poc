// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
)

func TestMeHandler_ReturnsClaims(t *testing.T) {
	h := &MeHandler{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	ctx := auth.NewContext(req.Context(), auth.Claims{
		Alias: "alice", DisplayName: "Alice", Email: "alice@example.com", IsAdmin: true,
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.GetMe(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got MeResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := MeResponse{Alias: "alice", DisplayName: "Alice", Email: "alice@example.com", IsAdmin: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestMeHandler_NoClaims(t *testing.T) {
	h := &MeHandler{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	w := httptest.NewRecorder()
	h.GetMe(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
