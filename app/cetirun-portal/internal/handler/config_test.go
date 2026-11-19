// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetConfig_WithBanner(t *testing.T) {
	h := &ConfigHandler{NoticeBanner: "Scheduled maintenance tonight"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	w := httptest.NewRecorder()

	h.GetConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	var resp ConfigResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NoticeBanner != "Scheduled maintenance tonight" {
		t.Errorf("noticeBanner = %q, want %q", resp.NoticeBanner, "Scheduled maintenance tonight")
	}
}

func TestGetConfig_EmptyBanner(t *testing.T) {
	h := &ConfigHandler{NoticeBanner: ""}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	w := httptest.NewRecorder()

	h.GetConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp ConfigResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NoticeBanner != "" {
		t.Errorf("noticeBanner = %q, want empty", resp.NoticeBanner)
	}
}
