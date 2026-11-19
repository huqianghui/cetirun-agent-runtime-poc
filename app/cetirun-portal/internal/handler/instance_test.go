// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- Helper functions ---

func withClaims(r *http.Request, alias, displayName, email string) *http.Request {
	claims := auth.Claims{
		Alias:       alias,
		DisplayName: displayName,
		Email:       email,
	}
	ctx := auth.NewContext(r.Context(), claims)
	return r.WithContext(ctx)
}

func withAgentIdentityClaims(r *http.Request, alias, tenantID, objectID string) *http.Request {
	claims := auth.Claims{
		Alias:       alias,
		DisplayName: alias + "@example.com",
		Email:       alias + "@example.com",
		TenantID:    tenantID,
		ObjectID:    objectID,
	}
	return r.WithContext(auth.NewContext(r.Context(), claims))
}

func sampleInstance() *cetirunv1alpha1.Agent {
	reconcileTime := metav1.NewTime(time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC))
	return &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "alice",
			UID:               "agent-alice",
			ResourceVersion:   "1",
			CreationTimestamp: metav1.NewTime(time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)),
		},
		Spec: cetirunv1alpha1.AgentSpec{
			Agent:      cetirunv1alpha1.HarnessSpec{Type: cetirunv1alpha1.AgentTypeOpenClaw},
			Team:       "aks",
			SandboxRef: "standard",
			User: cetirunv1alpha1.UserSpec{
				Alias:       "alice",
				DisplayName: "Alice Smith",
				Email:       "alice@microsoft.com",
			},
		},
		Status: cetirunv1alpha1.AgentStatus{
			Phase:             "Ready",
			Namespace:         "cetirun-alice",
			AccessURL:         "https://alice.cetirun.example.com",
			LastReconcileTime: &reconcileTime,
			Conditions: []metav1.Condition{
				{
					Type:    "Ready",
					Status:  metav1.ConditionTrue,
					Reason:  "AllReady",
					Message: "All components ready",
				},
			},
		},
	}
}

func TestToInstanceResponse(t *testing.T) {
	instance := sampleInstance()
	// Change phase to Pending to avoid token fetch in test
	instance.Status.Phase = "Pending"

	h := &InstanceHandler{
		Kube: kube.NewClient(newFakeClient()),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}

	if resp.Alias != "alice" {
		t.Errorf("Alias = %q, want %q", resp.Alias, "alice")
	}
	if resp.InstanceName != "alice" {
		t.Errorf("InstanceName = %q, want %q", resp.InstanceName, "alice")
	}
	if resp.DisplayName != "Alice Smith" {
		t.Errorf("DisplayName = %q, want %q", resp.DisplayName, "Alice Smith")
	}
	if resp.Team != "aks" {
		t.Errorf("Team = %q, want %q", resp.Team, "aks")
	}
	if resp.Phase != "Pending" {
		t.Errorf("Phase = %q, want %q", resp.Phase, "Pending")
	}
	if resp.AccessURL != "https://alice.cetirun.example.com" {
		t.Errorf("AccessURL = %q, want %q", resp.AccessURL, "https://alice.cetirun.example.com")
	}
	if resp.CreatedAt != "2024-01-15T10:00:00Z" {
		t.Errorf("CreatedAt = %q, want %q", resp.CreatedAt, "2024-01-15T10:00:00Z")
	}
	if resp.LastReconciled != "2024-01-15T12:00:00Z" {
		t.Errorf("LastReconciled = %q, want %q", resp.LastReconciled, "2024-01-15T12:00:00Z")
	}
	if len(resp.Conditions) != 1 {
		t.Fatalf("Conditions length = %d, want 1", len(resp.Conditions))
	}
	if resp.Conditions[0].Type != "Ready" {
		t.Errorf("Conditions[0].Type = %q, want %q", resp.Conditions[0].Type, "Ready")
	}
	if resp.Conditions[0].Status != "True" {
		t.Errorf("Conditions[0].Status = %q, want %q", resp.Conditions[0].Status, "True")
	}
}

func TestToInstanceResponseExposesOnlyAgentIdentityLifecycle(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Pending"
	instance.Spec.AgentIdentity = &cetirunv1alpha1.AgentIdentitySpec{Enabled: true}
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		Phase:    cetirunv1alpha1.AgentIdentityPhaseReady,
		Reason:   "AgentIdentityReady",
		ClientID: "11111111-1111-1111-1111-111111111111",
		ObjectID: "22222222-2222-2222-2222-222222222222",
	}
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient())}

	response, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if response.AgentIdentity == nil || !response.AgentIdentity.Enabled ||
		response.AgentIdentity.Phase != cetirunv1alpha1.AgentIdentityPhaseReady ||
		response.AgentIdentity.Reason != "AgentIdentityReady" {
		t.Fatalf("AgentIdentity response = %#v", response.AgentIdentity)
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), instance.Status.AgentIdentity.ClientID) ||
		strings.Contains(string(payload), instance.Status.AgentIdentity.ObjectID) {
		t.Fatalf("response exposed directory identifiers: %s", payload)
	}
}

func TestToInstanceResponse_NilLastReconcileTime(t *testing.T) {
	instance := sampleInstance()
	instance.Status.LastReconcileTime = nil
	instance.Status.Phase = "Pending"

	h := &InstanceHandler{
		Kube: kube.NewClient(newFakeClient()),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.LastReconciled != "" {
		t.Errorf("LastReconciled = %q, want empty", resp.LastReconciled)
	}
}

func TestToInstanceResponse_NoConditions(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Conditions = nil
	instance.Status.Phase = "Pending"

	h := &InstanceHandler{
		Kube: kube.NewClient(newFakeClient()),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.Conditions != nil {
		t.Errorf("Conditions = %v, want nil", resp.Conditions)
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]string{"key": "value"}
	writeJSON(w, http.StatusOK, data)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if body["key"] != "value" {
		t.Errorf("body key = %q, want %q", body["key"], "value")
	}
}

func TestWriteError_Handler(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "bad_request", "something went wrong")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if errResp.Error.Code != "bad_request" {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, "bad_request")
	}
	if errResp.Error.Message != "something went wrong" {
		t.Errorf("error message = %q, want %q", errResp.Error.Message, "something went wrong")
	}
}

// --- Integration tests using fake controller-runtime client ---

func TestGetInstance_NotFound(t *testing.T) {
	h := &InstanceHandler{
		Kube: kube.NewClient(newFakeClient()),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instance", nil)
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.GetInstance(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestGetInstance_Success(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instance", nil)
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.GetInstance(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Alias != "alice" {
		t.Errorf("Alias = %q, want %q", resp.Alias, "alice")
	}
	if resp.Phase != "Ready" {
		t.Errorf("Phase = %q, want %q", resp.Phase, "Ready")
	}
}

func TestCreateInstance_Success(t *testing.T) {
	fc := newGatewayFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc), Config: gatewayTestConfig(),
	}

	body := CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Alias != "alice" {
		t.Errorf("Alias = %q, want %q", resp.Alias, "alice")
	}
	if resp.Agent.Type != "hermes" {
		t.Errorf("AgentType = %q, want default hermes", resp.Agent.Type)
	}
}

func TestCreateInstance_ExplicitOpenClaw(t *testing.T) {
	h := &InstanceHandler{Kube: kube.NewClient(newGatewayFakeClient()), Config: gatewayTestConfig()}
	body := strings.NewReader(`{"agent":{"type":"openclaw"},"team":"aks"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", body)
	req = withClaims(req, "alice", "Example User", "alice@example.com")
	w := httptest.NewRecorder()
	h.CreateInstance(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Agent.Type != "openclaw" {
		t.Fatalf("explicit AgentType changed: %q", resp.Agent.Type)
	}
}

func TestCreateInstanceAgentIdentityRequiresEnabledFeature(t *testing.T) {
	h := &InstanceHandler{Kube: kube.NewClient(newGatewayFakeClient()), Config: gatewayTestConfig()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", strings.NewReader(`{"agent":{},"team":"aks","agentIdentityEnabled":true}`))
	req = withAgentIdentityClaims(req, "alice", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "feature_disabled") {
		t.Fatalf("response = %d %s, want feature_disabled", w.Code, w.Body.String())
	}
}

func TestCreateInstanceAgentIdentityRequiresVerifiedOwner(t *testing.T) {
	h := &InstanceHandler{
		Kube:   kube.NewClient(newGatewayFakeClient()),
		Config: func() config.Config { cfg := gatewayTestConfig(); cfg.AgentIdentityEnabled = true; return cfg }(),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", strings.NewReader(`{"agent":{},"team":"aks","agentIdentityEnabled":true}`))
	req = withClaims(req, "alice", "Alice Smith", "alice@example.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "verified_owner_required") {
		t.Fatalf("response = %d %s, want verified_owner_required", w.Code, w.Body.String())
	}
}

func TestCreateInstanceAgentIdentityBindsVerifiedOwner(t *testing.T) {
	const tenantID = "11111111-1111-1111-1111-111111111111"
	const objectID = "22222222-2222-2222-2222-222222222222"
	h := &InstanceHandler{
		Kube:   kube.NewClient(newGatewayFakeClient()),
		Config: func() config.Config { cfg := gatewayTestConfig(); cfg.AgentIdentityEnabled = true; return cfg }(),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", strings.NewReader(`{"agent":{},"team":"aks","agentIdentityEnabled":true}`))
	req = withAgentIdentityClaims(req, "alice", tenantID, objectID)
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("response = %d %s, want created", w.Code, w.Body.String())
	}
	instance, err := h.Kube.GetInstance(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if instance.Spec.AgentIdentity == nil || !instance.Spec.AgentIdentity.Enabled {
		t.Fatal("Agent Identity was not enabled on the created instance")
	}
	if got := instance.Annotations[cetirunv1alpha1.AnnotationOwnerTenantID]; got != tenantID {
		t.Fatalf("owner tenant annotation = %q, want %q", got, tenantID)
	}
	if got := instance.Annotations[cetirunv1alpha1.AnnotationOwnerObjectID]; got != objectID {
		t.Fatalf("owner object annotation = %q, want %q", got, objectID)
	}
}

func TestCreateInstance_ModelRoundTrip(t *testing.T) {
	fc := newGatewayFakeClient()
	h := &InstanceHandler{Kube: kube.NewClient(fc), Config: gatewayTestConfig()}

	body := CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks", Model: " gpt-6-astra "}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()
	h.CreateInstance(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Model != "gpt-6-astra" {
		t.Errorf("response model = %q, want gpt-6-astra", resp.Model)
	}
	instance, err := h.Kube.GetInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if instance.Spec.Model != "gpt-6-astra" {
		t.Errorf("stored model = %q, want gpt-6-astra", instance.Spec.Model)
	}
}

func TestCreateInstance_ModelTooLongRejected(t *testing.T) {
	h := &InstanceHandler{Kube: kube.NewClient(newGatewayFakeClient()), Config: gatewayTestConfig()}
	bodyBytes, _ := json.Marshal(CreateInstanceRequest{Agent: &agentconfig.Input{}, Model: strings.Repeat("a", 257)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()
	h.CreateInstance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error.Code != "invalid_model" {
		t.Errorf("error code = %q, want invalid_model", resp.Error.Code)
	}
}

func TestCreateInstance_NameLength(t *testing.T) {
	// "alice-" is 6 characters, so the suffix length sets the total.
	cases := []struct {
		name   string
		alias  string
		suffix string
		want   int
	}{
		{name: "35 characters with suffix", alias: "alice", suffix: strings.Repeat("a", 29), want: http.StatusCreated},
		{name: "36 characters with suffix", alias: "alice", suffix: strings.Repeat("a", 30), want: http.StatusBadRequest},
		{name: "55 characters with suffix", alias: "alice", suffix: strings.Repeat("a", 49), want: http.StatusBadRequest},
		{name: "35-character alias alone", alias: strings.Repeat("b", 35), want: http.StatusCreated},
		{name: "36-character alias alone", alias: strings.Repeat("b", 36), want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &InstanceHandler{Kube: kube.NewClient(newGatewayFakeClient()), Config: gatewayTestConfig()}
			bodyBytes, _ := json.Marshal(CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks", InstanceName: tc.suffix})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
			req = withClaims(req, tc.alias, "Test User", tc.alias+"@microsoft.com")
			w := httptest.NewRecorder()
			h.CreateInstance(w, req)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusBadRequest {
				return
			}
			var resp ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error.Code != "name_too_long" {
				t.Errorf("error code = %q, want name_too_long", resp.Error.Code)
			}
		})
	}
}

func TestCreateInstance_ReservedName(t *testing.T) {
	// Each reserved name collides with either an EnvoyFilter gateway path token or
	// a portal SPA top-level route. Prefixing or suffixing a reserved token remains
	// valid because the gateway matches complete path segments, not string prefixes.
	reserved := []string{"oauth2", "api", "internal", "healthz", "readyz", "assets", "portal", "instances", "admin", "connections", "ui"}
	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			fc := newGatewayFakeClient()
			h := &InstanceHandler{Kube: kube.NewClient(fc), Config: gatewayTestConfig()}

			body := CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks", InstanceName: name}
			bodyBytes, _ := json.Marshal(body)

			// Use a single-letter alias so the combined name is short enough to pass
			// the 63-char namespace check, isolating the reserved-name rejection.
			req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
			req = withClaims(req, "a", "A", "a@microsoft.com")
			w := httptest.NewRecorder()

			h.CreateInstance(w, req)

			// The combined name is "a-<reserved>", which is not itself reserved,
			// so this should succeed — proving that only bare reserved names are rejected.
			if w.Code != http.StatusCreated {
				t.Errorf("suffix form: status = %d, want %d (body=%s)", w.Code, http.StatusCreated, w.Body.String())
			}
		})
	}

	// Now exercise the actual rejection path: an alias that itself equals a reserved name,
	// with no suffix, so instanceName == reserved.
	for _, name := range reserved {
		t.Run("bare-"+name, func(t *testing.T) {
			fc := newGatewayFakeClient()
			h := &InstanceHandler{Kube: kube.NewClient(fc), Config: gatewayTestConfig()}

			body := CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks"}
			bodyBytes, _ := json.Marshal(body)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
			req = withClaims(req, name, "X", name+"@microsoft.com")
			w := httptest.NewRecorder()

			h.CreateInstance(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error body: %v (body=%s)", err, w.Body.String())
			}
			if resp.Error.Code != "reserved_name" {
				t.Errorf("error.code = %q, want %q (body=%s)", resp.Error.Code, "reserved_name", w.Body.String())
			}
		})
	}
}

func TestCreateLegacyInstanceWithoutTeam(t *testing.T) {
	fc := newGatewayFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc), Config: gatewayTestConfig(),
	}

	body := CreateInstanceRequest{Agent: &agentconfig.Input{}}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (body=%s)", w.Code, http.StatusCreated, w.Body.String())
	}
}

func TestCreateInstance_InvalidBody(t *testing.T) {
	fc := newGatewayFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc), Config: gatewayTestConfig(),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader([]byte("invalid")))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCreateInstance_AlreadyExists(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestCreateInstance_CustomDisplayName(t *testing.T) {
	fc := newGatewayFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc), Config: gatewayTestConfig(),
	}

	body := CreateInstanceRequest{Agent: &agentconfig.Input{}, DisplayName: "Custom Name", Team: "aks"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.DisplayName != "Custom Name" {
		t.Errorf("DisplayName = %q, want %q", resp.DisplayName, "Custom Name")
	}
}

func TestCreateInstance_WithGatewayToken(t *testing.T) {
	fc := newGatewayFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc), Config: gatewayTestConfig(),
	}

	body := CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks", GatewayToken: "my-secret-token"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Alias != "alice" {
		t.Errorf("Alias = %q, want %q", resp.Alias, "alice")
	}

	// Verify gateway-token secret was created in the instance namespace.
	secret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice", Name: "gateway-token"}, secret); err != nil {
		t.Fatalf("expected gateway-token secret to exist: %v", err)
	}
	if string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]) != "my-secret-token" {
		t.Errorf("OPENCLAW_GATEWAY_TOKEN = %q, want %q", string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]), "my-secret-token")
	}
}

func TestCreateInstance_WithTailscaleAuthKey(t *testing.T) {
	fc := newGatewayFakeClient()
	h := &InstanceHandler{
		Kube:   kube.NewClient(fc),
		Config: func() config.Config { cfg := gatewayTestConfig(); cfg.TailscaleEnabled = true; return cfg }(),
	}

	body := CreateInstanceRequest{Agent: &agentconfig.Input{}, Team: "aks", TailscaleAuthKey: "tskey-auth-xxxxx"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.CreateInstance(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Alias != "alice" {
		t.Errorf("Alias = %q, want %q", resp.Alias, "alice")
	}

	// Verify tailscale-auth secret was created.
	secret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice", Name: "tailscale-auth"}, secret); err != nil {
		t.Fatalf("expected tailscale-auth secret to exist: %v", err)
	}
	if string(secret.Data["TAILSCALE_AUTH_KEY"]) != "tskey-auth-xxxxx" {
		t.Errorf("TAILSCALE_AUTH_KEY = %q, want %q", string(secret.Data["TAILSCALE_AUTH_KEY"]), "tskey-auth-xxxxx")
	}

	// Verify the CR has Tailscale spec set.
	instance := &cetirunv1alpha1.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, instance); err != nil {
		t.Fatalf("expected instance to exist: %v", err)
	}
	if instance.Spec.Tailscale == nil {
		t.Fatal("expected Tailscale spec to be set")
	}
	if instance.Spec.Tailscale.AuthKeySecretRef != "tailscale-auth" {
		t.Errorf("AuthKeySecretRef = %q, want %q", instance.Spec.Tailscale.AuthKeySecretRef, "tailscale-auth")
	}
}

func TestToInstanceResponse_WithTailscaleAuthKey(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Ready"
	instance.Spec.GatewayTokenSecretRef = "gateway-token"
	instance.Spec.Tailscale = &cetirunv1alpha1.TailscaleSpec{
		AuthKeySecretRef: "tailscale-auth",
	}

	// Create the gateway-token and tailscale-auth secrets in the instance namespace.
	gwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "gateway-token"},
		Data:       map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte("gw-token-val")},
	}
	tsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "tailscale-auth"},
		Data:       map[string][]byte{"TAILSCALE_AUTH_KEY": []byte("tskey-auth-xxx")},
	}

	fc := newFakeClient(instance, gwSecret, tsSecret)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.GatewayToken != "gw-token-val" {
		t.Errorf("GatewayToken = %q, want %q", resp.GatewayToken, "gw-token-val")
	}
	// AccessURL must remain exactly as the instance status (no token appended).
	if resp.AccessURL != "https://alice.cetirun.example.com" {
		t.Errorf("AccessURL = %q, want %q", resp.AccessURL, "https://alice.cetirun.example.com")
	}
	if resp.TailscaleAuthKey != "tskey-auth-xxx" {
		t.Errorf("TailscaleAuthKey = %q, want %q", resp.TailscaleAuthKey, "tskey-auth-xxx")
	}
}

func TestToInstanceResponse_HermesAPIServerKey(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Agent.Type = cetirunv1alpha1.AgentTypeHermes
	instance.Status.Phase = "Ready"
	instance.Spec.GatewayTokenSecretRef = "gateway-token"

	gwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "gateway-token"},
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte("gw-token-val"),
			"API_SERVER_KEY":         []byte("api-server-key-val"),
			"CETIRUN_AUTH_KEY":       []byte("cetirun-auth-key-val"),
		},
	}

	fc := newFakeClient(instance, gwSecret)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.GatewayToken != "gw-token-val" {
		t.Errorf("GatewayToken = %q, want %q", resp.GatewayToken, "gw-token-val")
	}
	if resp.APIServerKey != "api-server-key-val" {
		t.Errorf("APIServerKey = %q, want %q", resp.APIServerKey, "api-server-key-val")
	}
}

func TestCustomAccessService(t *testing.T) {
	for _, test := range []struct {
		name     string
		desktop  metav1.ConditionStatus
		terminal metav1.ConditionStatus
		want     string
	}{
		{"legacy desktop", "", "", "desktop"},
		{"terminal only", metav1.ConditionFalse, metav1.ConditionTrue, "term"},
		{"both ready", metav1.ConditionTrue, metav1.ConditionTrue, "desktop"},
		{"both disabled", metav1.ConditionFalse, metav1.ConditionFalse, ""},
		{"terminal pending", metav1.ConditionFalse, metav1.ConditionUnknown, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var conditions []metav1.Condition
			if test.desktop != "" {
				conditions = append(conditions, metav1.Condition{Type: cetirunv1alpha1.ConditionDesktopConfigured, Status: test.desktop})
			}
			if test.terminal != "" {
				conditions = append(conditions, metav1.Condition{Type: cetirunv1alpha1.ConditionTerminalConfigured, Status: test.terminal})
			}
			if got := customAccessService(conditions, true); got != test.want {
				t.Fatalf("service=%q, want %q", got, test.want)
			}
		})
	}
}

func TestToInstanceResponse_CustomDesktopWithoutNativeCredentials(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Agent.Type = cetirunv1alpha1.AgentTypeCustom
	instance.Spec.GatewayTokenSecretRef = "native-secret"
	instance.Status.AccessURL = ""
	instance.Status.Conditions = append(instance.Status.Conditions, metav1.Condition{
		Type: cetirunv1alpha1.ConditionIngressReady, Status: metav1.ConditionTrue,
	})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "native-secret"},
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte("dummy-native-token"),
			"API_SERVER_KEY":         []byte("dummy-api-key"),
			"CETIRUN_AUTH_KEY":       []byte("dummy-auth-key"),
		},
	}
	operator := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-system", Name: "cetirun-operator"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "manager", Args: []string{"--chart-version=1.0.0", "--domain-suffix=platform.example.com"}},
		}}}}}
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(instance, secret, operator))}
	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if resp.AccessURL != "https://platform.example.com/alice/desktop/" || resp.Agent.Type != "custom" || instance.Status.AccessURL != "" {
		t.Fatal("Custom platform access was lost")
	}
	if resp.GatewayToken != "" || resp.APIServerKey != "" {
		t.Fatal("Custom response exposed native credentials")
	}
}

func TestToInstanceResponse_OpenClawDoesNotExposeNativeCredentials(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Agent.Type = cetirunv1alpha1.AgentTypeOpenClaw
	instance.Status.Phase = "Ready"
	instance.Spec.GatewayTokenSecretRef = "gateway-token"

	gwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "gateway-token"},
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte("gw-token-val"),
			"CETIRUN_AUTH_KEY":       []byte("openclaw-auth-key-val"),
		},
	}

	fc := newFakeClient(instance, gwSecret)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.APIServerKey != "" {
		t.Errorf("APIServerKey should be empty for OpenClaw, got %q", resp.APIServerKey)
	}
}

func TestToInstanceResponse_TailscaleSecretMissing(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Ready"
	instance.Spec.GatewayTokenSecretRef = "gateway-token"
	instance.Spec.Tailscale = &cetirunv1alpha1.TailscaleSpec{
		AuthKeySecretRef: "tailscale-auth",
	}

	// Only create gateway-token, no tailscale-auth secret.
	gwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "gateway-token"},
		Data:       map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte("gw-token-val")},
	}

	fc := newFakeClient(instance, gwSecret)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	// GatewayToken should still be returned.
	if resp.GatewayToken != "gw-token-val" {
		t.Errorf("GatewayToken = %q, want %q", resp.GatewayToken, "gw-token-val")
	}
	// TailscaleAuthKey should be empty (failed to fetch, but no error returned).
	if resp.TailscaleAuthKey != "" {
		t.Errorf("TailscaleAuthKey = %q, want empty", resp.TailscaleAuthKey)
	}
}

func TestToInstanceResponse_NoTailscale(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Ready"
	instance.Spec.GatewayTokenSecretRef = "gateway-token"
	// Tailscale is nil (not enabled).

	gwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "gateway-token"},
		Data:       map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte("gw-token-val")},
	}

	fc := newFakeClient(instance, gwSecret)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.GatewayToken != "gw-token-val" {
		t.Errorf("GatewayToken = %q, want %q", resp.GatewayToken, "gw-token-val")
	}
	// AccessURL must remain exactly as the instance status (no token appended).
	if resp.AccessURL != "https://alice.cetirun.example.com" {
		t.Errorf("AccessURL = %q, want %q", resp.AccessURL, "https://alice.cetirun.example.com")
	}
	// TailscaleAuthKey should be empty when Tailscale is not configured.
	if resp.TailscaleAuthKey != "" {
		t.Errorf("TailscaleAuthKey = %q, want empty", resp.TailscaleAuthKey)
	}
}

func TestDeleteInstance_Success(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/instance", strings.NewReader(`{"uid":"agent-alice","resourceVersion":"1"}`))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.DeleteInstance(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
}

func TestDeleteInstance_NotFound(t *testing.T) {
	fc := newFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/instance", strings.NewReader(`{"uid":"agent-alice","resourceVersion":"1"}`))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.DeleteInstance(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// --- UpdateInstance tests ---

func TestUpdateInstance_Success(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Suspended = true
	platform := gatewayTeam("platform")
	platform.Spec.AgentPolicyRef = "standard"
	platform.Spec.SandboxRefs, platform.Spec.DefaultSandboxRef = []string{"restricted"}, "restricted"
	fc := newFakeClient(instance, platform)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1",
		DisplayName: strPtr("Alice Updated"),
		Team:        strPtr("platform"),
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	claims, _ := auth.ClaimsFromContext(req.Context())
	claims.IsAdmin = true
	req = req.WithContext(auth.NewContext(req.Context(), claims))
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.DisplayName != "Alice Updated" {
		t.Errorf("DisplayName = %q, want %q", resp.DisplayName, "Alice Updated")
	}
	stored, err := h.Kube.GetInstance(context.Background(), "alice")
	if err != nil || stored.Spec.Team != "platform" || stored.Spec.SandboxRef != "restricted" {
		t.Fatalf("updated binding = %q/%q, err=%v", stored.Spec.Team, stored.Spec.SandboxRef, err)
	}
	if resp.Team != "platform" {
		t.Errorf("Team = %q, want %q", resp.Team, "platform")
	}
}

func TestUpdateInstance_ModelRoundTripAndClear(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Model = "gpt-5.4"
	fc := newFakeClient(instance)
	h := &InstanceHandler{Kube: kube.NewClient(fc)}

	for _, model := range []string{"gpt-6-astra", ""} {
		current, err := h.Kube.GetInstance(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		bodyBytes, _ := json.Marshal(UpdateInstanceRequest{UID: string(current.UID), ResourceVersion: current.ResourceVersion, Model: &model})
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
		req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
		w := httptest.NewRecorder()
		h.UpdateInstance(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("model %q: status = %d, want %d (body=%s)", model, w.Code, http.StatusOK, w.Body.String())
		}
		var resp InstanceResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("model %q: decode response: %v", model, err)
		}
		if resp.Model != model {
			t.Errorf("model %q: response model = %q", model, resp.Model)
		}
	}

	updated, err := h.Kube.GetInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if updated.Spec.Model != "" {
		t.Errorf("stored model = %q, want deployment-default empty override", updated.Spec.Model)
	}
}

func TestUpdateInstance_ModelTooLongRejected(t *testing.T) {
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(sampleInstance()))}
	bodyBytes, _ := json.Marshal(UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", Model: strPtr(strings.Repeat("a", 257))})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()
	h.UpdateInstance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error.Code != "invalid_model" {
		t.Errorf("error code = %q, want invalid_model", resp.Error.Code)
	}
}

func TestUpdateInstanceByName_ModelRoundTrip(t *testing.T) {
	instance := sampleInstance()
	instance.Name = "alice-work"
	fc := newFakeClient(instance)
	h := &InstanceHandler{Kube: kube.NewClient(fc)}

	bodyBytes, _ := json.Marshal(UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", Model: strPtr("gpt-6-astra")})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instances/alice-work", bytes.NewReader(bodyBytes))
	req.SetPathValue("name", "alice-work")
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()
	h.UpdateInstanceByName(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", w.Code, http.StatusOK, w.Body.String())
	}
	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Model != "gpt-6-astra" {
		t.Errorf("response model = %q, want gpt-6-astra", resp.Model)
	}
}

func TestUpdateInstance_NotFound(t *testing.T) {
	fc := newFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", DisplayName: strPtr("X")}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestUpdateInstance_InvalidBody(t *testing.T) {
	fc := newFakeClient()
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader([]byte("invalid")))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestUpdateInstance_MemberCannotClearTeam(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", Team: strPtr("")}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusForbidden || errorCode(t, w) != "team_change_not_allowed" {
		t.Errorf("status = %d, want %d team_change_not_allowed", w.Code, http.StatusForbidden)
	}
}

func TestUpdateInstance_EmptyGatewayTokenRejected(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", GatewayToken: strPtr("")}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestUpdateInstance_EmptyBodyRejected(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	bodyBytes, _ := json.Marshal(UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1"})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error.Code != "no_fields" {
		t.Errorf("error code = %q, want %q", resp.Error.Code, "no_fields")
	}
}

func TestUpdateInstance_WithGatewayToken(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", GatewayToken: strPtr("new-token-val")}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify gateway-token secret.
	secret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice", Name: "gateway-token"}, secret); err != nil {
		t.Fatalf("expected gateway-token secret to exist: %v", err)
	}
	if string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]) != "new-token-val" {
		t.Errorf("OPENCLAW_GATEWAY_TOKEN = %q, want %q", string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]), "new-token-val")
	}
}

func TestUpdateInstance_EmptyDisplayNameUsesNameFallback(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", DisplayName: strPtr("")}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	stored, err := h.Kube.GetInstance(context.Background(), "alice")
	if err != nil || stored.Spec.User.DisplayName != "" {
		t.Fatalf("empty display name was not preserved: %v", err)
	}
}

func TestUpdateInstance_WithTailscaleAuthKey(t *testing.T) {
	instance := sampleInstance()
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube:   kube.NewClient(fc),
		Config: config.Config{TailscaleEnabled: true},
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", TailscaleAuthKey: strPtr("tskey-new")}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify tailscale-auth secret was created.
	secret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice", Name: "tailscale-auth"}, secret); err != nil {
		t.Fatalf("expected tailscale-auth secret to exist: %v", err)
	}
	if string(secret.Data["TAILSCALE_AUTH_KEY"]) != "tskey-new" {
		t.Errorf("TAILSCALE_AUTH_KEY = %q, want %q", string(secret.Data["TAILSCALE_AUTH_KEY"]), "tskey-new")
	}
}

func TestUpdateInstance_DisableTailscale(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Tailscale = &cetirunv1alpha1.TailscaleSpec{AuthKeySecretRef: "tailscale-auth"}
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	body := UpdateInstanceRequest{UID: "agent-alice", ResourceVersion: "1", TailscaleAuthKey: strPtr("")}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewReader(bodyBytes))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpdateInstance(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify the CR no longer has Tailscale spec.
	updated := &cetirunv1alpha1.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, updated); err != nil {
		t.Fatalf("failed to get updated instance: %v", err)
	}
	if updated.Spec.Tailscale != nil {
		t.Error("expected Tailscale spec to be nil after disabling")
	}
}

func strPtr(s string) *string { return &s }

// --- Helper: operator deployment ---

func operatorDeployment(chartVersion string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cetirun-system",
			Name:      "cetirun-operator",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "operator"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "operator"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "manager",
							Image: "cetirun-operator:latest",
							Args:  []string{"--leader-elect", "--chart-version=" + chartVersion},
						},
					},
				},
			},
		},
	}
}

// --- UpgradeInstance handler tests ---

func TestUpgradeInstance_Success(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Helm.Version = "0.1.17"
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(instance, deploy)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance/upgrade", strings.NewReader(`{"uid":"agent-alice","resourceVersion":"1"}`))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpgradeInstance(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp InstanceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Alias != "alice" {
		t.Errorf("Alias = %q, want %q", resp.Alias, "alice")
	}

	// Verify the CR was updated.
	updated := &cetirunv1alpha1.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, updated); err != nil {
		t.Fatalf("failed to get updated instance: %v", err)
	}
	if updated.Spec.Helm.Version != "0.1.18" {
		t.Errorf("version = %q, want %q", updated.Spec.Helm.Version, "0.1.18")
	}
}

func TestUpgradeInstance_NotFound(t *testing.T) {
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(deploy)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance/upgrade", strings.NewReader(`{"uid":"agent-alice","resourceVersion":"1"}`))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpgradeInstance(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestUpgradeInstance_NoOperator(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Helm.Version = "0.1.17"
	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instance/upgrade", strings.NewReader(`{"uid":"agent-alice","resourceVersion":"1"}`))
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.UpgradeInstance(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// --- UpgradeAvailable in toInstanceResponse ---

func TestToInstanceResponse_UpgradeAvailable(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Pending"
	instance.Spec.Helm.Version = "0.1.17"
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(instance, deploy)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if !resp.UpgradeAvailable {
		t.Error("expected UpgradeAvailable=true when versions differ")
	}
}

func TestToInstanceResponse_UpgradeNotAvailable_SameVersion(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Pending"
	instance.Spec.Helm.Version = "0.1.18"
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(instance, deploy)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.UpgradeAvailable {
		t.Error("expected UpgradeAvailable=false when versions match")
	}
}

func TestToInstanceResponse_UpgradeNotAvailable_EmptyVersion(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Pending"
	instance.Spec.Helm.Version = "" // not yet backfilled
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(instance, deploy)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	resp, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatalf("toInstanceResponse() error = %v", err)
	}
	if resp.UpgradeAvailable {
		t.Error("expected UpgradeAvailable=false when instance version is empty")
	}
}

// --- GetInstanceStatus tests ---

func TestGetInstanceStatus_NotFound(t *testing.T) {
	h := &InstanceHandler{
		Kube: kube.NewClient(newFakeClient()),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instance/status", nil)
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.GetInstanceStatus(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestGetInstanceStatus_Success(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Phase = "Ready"
	instance.Status.Conditions = []metav1.Condition{
		{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "AllReady",
			Message: "All components ready",
		},
		{
			Type:    "HelmDeployed",
			Status:  metav1.ConditionTrue,
			Reason:  "Deployed",
			Message: "Helm chart deployed",
		},
	}

	// Create a pod in the user namespace.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cetirun-alice",
			Name:      "openclaw-alice-0",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "openclaw",
					Ready:        true,
					RestartCount: 2,
					State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	fc := newFakeClient(instance, pod)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instance/status", nil)
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.GetInstanceStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp StatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	// Conditions
	if len(resp.Conditions) != 2 {
		t.Fatalf("Conditions length = %d, want 2", len(resp.Conditions))
	}
	if resp.Conditions[0].Type != "Ready" {
		t.Errorf("Conditions[0].Type = %q, want %q", resp.Conditions[0].Type, "Ready")
	}
	if resp.Conditions[0].Status != "True" {
		t.Errorf("Conditions[0].Status = %q, want %q", resp.Conditions[0].Status, "True")
	}

	// Pods
	if len(resp.Pods) != 1 {
		t.Fatalf("Pods length = %d, want 1", len(resp.Pods))
	}
	if resp.Pods[0].Name != "openclaw-alice-0" {
		t.Errorf("Pods[0].Name = %q, want %q", resp.Pods[0].Name, "openclaw-alice-0")
	}
	if resp.Pods[0].Phase != "Running" {
		t.Errorf("Pods[0].Phase = %q, want %q", resp.Pods[0].Phase, "Running")
	}
	if !resp.Pods[0].Ready {
		t.Error("expected Pods[0].Ready=true")
	}
	if resp.Pods[0].RestartCount != 2 {
		t.Errorf("Pods[0].RestartCount = %d, want 2", resp.Pods[0].RestartCount)
	}

	// Events and NsEvents should be empty slices (not null).
	if resp.Events == nil {
		t.Error("Events should be empty slice, not nil")
	}
	if resp.NsEvents == nil {
		t.Error("NsEvents should be empty slice, not nil")
	}
}

func TestGetInstanceStatus_EmptySlicesWhenNoData(t *testing.T) {
	instance := sampleInstance()
	instance.Status.Conditions = nil

	fc := newFakeClient(instance)
	h := &InstanceHandler{
		Kube: kube.NewClient(fc),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instance/status", nil)
	req = withClaims(req, "alice", "Alice Smith", "alice@microsoft.com")
	w := httptest.NewRecorder()

	h.GetInstanceStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp StatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	// All fields should be empty arrays, not null.
	if resp.Conditions == nil {
		t.Error("Conditions should be empty slice, not nil")
	}
	if len(resp.Conditions) != 0 {
		t.Errorf("Conditions length = %d, want 0", len(resp.Conditions))
	}
	if resp.Events == nil {
		t.Error("Events should be empty slice, not nil")
	}
	if resp.NsEvents == nil {
		t.Error("NsEvents should be empty slice, not nil")
	}
	if resp.Pods == nil {
		t.Error("Pods should be empty slice, not nil")
	}
}

func TestListInstances(t *testing.T) {
	alice1 := sampleInstance()
	alice1.Name = "alice-work"

	alice2 := sampleInstance()
	alice2.Name = "alice-exp"
	alice2.Spec.User.Alias = "alice"

	bob := sampleInstance()
	bob.Name = "bob-default"
	bob.Spec.User.Alias = "bob"

	h := &InstanceHandler{Kube: kube.NewClient(newFakeClientWith(alice1, alice2, bob))}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instances", nil)
	req = withClaims(req, "alice", "Alice", "alice@example.com")
	w := httptest.NewRecorder()
	h.ListInstances(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp) != 2 {
		t.Errorf("got %d instances, want 2 (only alice's)", len(resp))
	}
}

func TestGetInstanceByName_Forbidden(t *testing.T) {
	inst := sampleInstance()
	inst.Name = "alice-work"
	inst.Spec.User.Alias = "alice"

	h := &InstanceHandler{Kube: kube.NewClient(newFakeClientWith(inst))}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instances/alice-work", nil)
	req.SetPathValue("name", "alice-work")
	req = withClaims(req, "bob", "Bob", "bob@example.com")
	w := httptest.NewRecorder()
	h.GetInstanceByName(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestInternalAuth_Allow(t *testing.T) {
	inst := sampleInstance()
	inst.Name = "alice"
	inst.Spec.User.Alias = "alice"
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClientWith(inst))}

	req := httptest.NewRequest(http.MethodGet, "/internal/auth", nil)
	req.Header.Set("X-Internal-Instance", "alice")
	req.Header.Set("X-Internal-Alias", "alice")
	w := httptest.NewRecorder()
	h.InternalAuth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestInternalAuth_Deny(t *testing.T) {
	inst := sampleInstance()
	inst.Name = "alice"
	inst.Spec.User.Alias = "alice"
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClientWith(inst))}

	req := httptest.NewRequest(http.MethodGet, "/internal/auth", nil)
	req.Header.Set("X-Internal-Instance", "alice")
	req.Header.Set("X-Internal-Alias", "bob")
	w := httptest.NewRecorder()
	h.InternalAuth(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestInternalAuth_NotFound(t *testing.T) {
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient())}
	req := httptest.NewRequest(http.MethodGet, "/internal/auth", nil)
	req.Header.Set("X-Internal-Instance", "nobody")
	req.Header.Set("X-Internal-Alias", "bob")
	w := httptest.NewRecorder()
	h.InternalAuth(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (instance not found treated as deny)", w.Code)
	}
}
