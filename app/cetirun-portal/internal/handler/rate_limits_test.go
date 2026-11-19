// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func exampleRateLimitsDTO() *AgentPolicyRateLimitsDTO {
	return &AgentPolicyRateLimitsDTO{
		MaxParallelRequests: ptr.To[int64](4), RPM: ptr.To[int64](60), TPM: ptr.To[int64](200000),
		PerModel: map[string]AgentPolicyModelRateLimitDTO{"gpt-4.1": {RPM: ptr.To[int64](10)}, "auto": {TPM: ptr.To[int64](8192)}},
	}
}

func putPolicy(t *testing.T, h *AdminPolicyHandler, name string, req AgentPolicyUpsertRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/"+name, bytes.NewReader(body))
	update.SetPathValue("name", name)
	w := httptest.NewRecorder()
	h.UpdatePolicy(w, update)
	return w
}

func TestAdminPolicyRateLimitsRoundTrip(t *testing.T) {
	h := newAdminPolicyHandler()
	req := validPolicyRequest()
	req.ModelAccess.RateLimits = exampleRateLimitsDTO()
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"rateLimits":{"maxParallelRequests":4,"rpm":60,"tpm":200000,"perModel":{`) {
		t.Fatalf("response does not carry the limits: %s", w.Body.String())
	}
	stored, err := h.Kube.GetAgentPolicy(context.Background(), "standard")
	if err != nil {
		t.Fatal(err)
	}
	want := &api.AgentPolicyRateLimits{
		MaxParallelRequests: ptr.To[int64](4), RPM: ptr.To[int64](60), TPM: ptr.To[int64](200000),
		PerModel: map[string]api.AgentPolicyModelRateLimit{"gpt-4.1": {RPM: ptr.To[int64](10)}, "auto": {TPM: ptr.To[int64](8192)}},
	}
	if !reflect.DeepEqual(stored.Spec.ModelAccess.RateLimits, want) {
		t.Fatalf("stored limits = %+v, want %+v", stored.Spec.ModelAccess.RateLimits, want)
	}

	// Saving what the edit page loaded keeps the limits.
	loaded := toAgentPolicyDTO(stored)
	save := AgentPolicyUpsertRequest{UID: loaded.UID, ResourceVersion: loaded.ResourceVersion, Harnesses: AgentPolicyHarnessesDTO{Allowed: []api.AgentType{api.AgentTypeHermes}},
		Skills: loaded.Skills, ModelAccess: loaded.ModelAccess}
	if w := putPolicy(t, h, "standard", save); w.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", w.Code, w.Body.String())
	}
	if stored, err = h.Kube.GetAgentPolicy(context.Background(), "standard"); err != nil || !reflect.DeepEqual(stored.Spec.ModelAccess.RateLimits, want) {
		t.Fatalf("an unrelated edit changed the limits: %+v, %v", stored.Spec.ModelAccess.RateLimits, err)
	}

	// Empty fields mean no limit: an empty object stores no rateLimits.
	save.ResourceVersion = stored.ResourceVersion
	save.ModelAccess.RateLimits = &AgentPolicyRateLimitsDTO{}
	if w := putPolicy(t, h, "standard", save); w.Code != http.StatusOK {
		t.Fatalf("clear status %d: %s", w.Code, w.Body.String())
	}
	if stored, err = h.Kube.GetAgentPolicy(context.Background(), "standard"); err != nil || stored.Spec.ModelAccess.RateLimits != nil {
		t.Fatalf("cleared limits were stored: %+v, %v", stored.Spec.ModelAccess.RateLimits, err)
	}
}

func TestAdminPolicyRejectsRateLimitsTheGatewayWouldMisread(t *testing.T) {
	tooMany := map[string]AgentPolicyModelRateLimitDTO{}
	for i := range 65 {
		tooMany["example-model-"+strconv.Itoa(i)] = AgentPolicyModelRateLimitDTO{RPM: ptr.To[int64](1)}
	}
	for name, limits := range map[string]*AgentPolicyRateLimitsDTO{
		"zero parallel":      {MaxParallelRequests: ptr.To[int64](0)},
		"parallel too large": {MaxParallelRequests: ptr.To[int64](2147483648)},
		"zero rpm":           {RPM: ptr.To[int64](0)},
		"negative rpm":       {RPM: ptr.To[int64](-1)},
		"small tpm":          {TPM: ptr.To[int64](4095)},
		"model zero rpm":     {PerModel: map[string]AgentPolicyModelRateLimitDTO{"m": {RPM: ptr.To[int64](0)}}},
		"model small tpm":    {PerModel: map[string]AgentPolicyModelRateLimitDTO{"m": {TPM: ptr.To[int64](100)}}},
		"model without both": {PerModel: map[string]AgentPolicyModelRateLimitDTO{"m": {}}},
		"model with space":   {PerModel: map[string]AgentPolicyModelRateLimitDTO{"a model": {RPM: ptr.To[int64](1)}}},
		"empty model name":   {PerModel: map[string]AgentPolicyModelRateLimitDTO{"": {RPM: ptr.To[int64](1)}}},
		"long model name":    {PerModel: map[string]AgentPolicyModelRateLimitDTO{strings.Repeat("m", 257): {RPM: ptr.To[int64](1)}}},
		"too many models":    {PerModel: tooMany},
	} {
		t.Run(name, func(t *testing.T) {
			h := newAdminPolicyHandler()
			req := validPolicyRequest()
			req.ModelAccess.RateLimits = limits
			body, _ := json.Marshal(req)
			w := httptest.NewRecorder()
			h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "rateLimits") {
				t.Fatalf("status %d, want 400 about rateLimits: %s", w.Code, w.Body.String())
			}
		})
	}
	for name, limits := range map[string]*AgentPolicyRateLimitsDTO{
		"minimums":      {MaxParallelRequests: ptr.To[int64](1), RPM: ptr.To[int64](1), TPM: ptr.To[int64](4096)},
		"no upper cap":  {RPM: ptr.To[int64](9000000000), TPM: ptr.To[int64](9000000000)},
		"longest model": {PerModel: map[string]AgentPolicyModelRateLimitDTO{strings.Repeat("m", 256): {RPM: ptr.To[int64](1)}}},
	} {
		if message := validateAgentPolicyRateLimits(limits); message != "" {
			t.Errorf("%s rejected: %s", name, message)
		}
	}
}

func TestCreateOptionsAndAgentDetailsShowPolicyRateLimits(t *testing.T) {
	team, policy := creationPolicy()
	policy.Spec.ModelAccess.RateLimits = &api.AgentPolicyRateLimits{RPM: ptr.To[int64](30), PerModel: map[string]api.AgentPolicyModelRateLimit{"gpt-6-mini": {TPM: ptr.To[int64](50000)}}}
	h, _ := creationPolicyHandler(team, policy)
	options := readCreateOptions(t, h, team.Name)
	if options.RateLimits == nil || *options.RateLimits.RPM != 30 || *options.RateLimits.PerModel["gpt-6-mini"].TPM != 50000 || options.RateLimits.TPM != nil {
		t.Fatalf("create options limits = %+v", options.RateLimits)
	}

	instance := customManagementInstance()
	instance.Spec.Team = team.Name
	instance.Status.AIGateway = &api.AIGatewayStatus{Routed: true, Phase: api.AIGatewayPhaseReady}
	details := customManagementHandler(instance, team, policy)
	w := httptest.NewRecorder()
	details.GetInstanceByName(w, managementRequest(http.MethodGet, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("details status %d: %s", w.Code, w.Body.String())
	}
	var response InstanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AIGateway == nil || response.AIGateway.RateLimits == nil || *response.AIGateway.RateLimits.RPM != 30 {
		t.Fatalf("Agent details limits = %+v", response.AIGateway)
	}

	// An Agent whose Team has no policy shows no limits.
	legacyTeam := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "legacy-team"}}
	instance = customManagementInstance()
	instance.Spec.Team = legacyTeam.Name
	instance.Status.AIGateway = &api.AIGatewayStatus{Routed: true, Phase: api.AIGatewayPhaseReady}
	w = httptest.NewRecorder()
	customManagementHandler(instance, legacyTeam, policy).GetInstanceByName(w, managementRequest(http.MethodGet, ""))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "rateLimits") {
		t.Fatalf("legacy Agent details: %d %s", w.Code, w.Body.String())
	}
}
