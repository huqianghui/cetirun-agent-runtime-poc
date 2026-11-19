// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
)

func TestGetFeatures(t *testing.T) {
	h := &FeaturesHandler{
		Config: config.Config{
			TailscaleEnabled:        true,
			AgentIdentityEnabled:    true,
			AgentToolsEnabled:       true,
			MaxInstancesPerUser:     2,
			AllowedAgentTypes:       []string{"hermes"},
			SkillMarketplaceEnabled: true,
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/features", nil)
	w := httptest.NewRecorder()

	h.GetFeatures(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), `"skillMarketplaceEnabled":true`) {
		t.Errorf("response does not report the Skill Marketplace: %s", w.Body.String())
	}
	var resp FeaturesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.TailscaleEnabled {
		t.Errorf("tailscaleEnabled = false, want true")
	}
	if !resp.AgentIdentityEnabled {
		t.Errorf("agentIdentityEnabled = false, want true")
	}
	if !resp.AgentToolsEnabled {
		t.Errorf("agentToolsEnabled = false, want true")
	}
	if resp.MaxInstancesPerUser != 2 {
		t.Errorf("maxInstancesPerUser = %d, want 2", resp.MaxInstancesPerUser)
	}
	if !reflect.DeepEqual(resp.AllowedAgentTypes, []string{"hermes"}) {
		t.Errorf("allowedAgentTypes = %#v, want %#v", resp.AllowedAgentTypes, []string{"hermes"})
	}
	if !resp.SkillMarketplaceEnabled {
		t.Errorf("skillMarketplaceEnabled = false, want true")
	}

	w = httptest.NewRecorder()
	(&FeaturesHandler{}).GetFeatures(w, req)
	if !strings.Contains(w.Body.String(), `"skillMarketplaceEnabled":false`) {
		t.Errorf("a default deployment does not report the Skill Marketplace as disabled: %s", w.Body.String())
	}
}
