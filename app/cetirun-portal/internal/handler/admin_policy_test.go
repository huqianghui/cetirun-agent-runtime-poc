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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/configurationcoordination"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

func newAdminPolicyHandler(objs ...client.Object) *AdminPolicyHandler {
	return &AdminPolicyHandler{Kube: kube.NewClient(newFakeClient(objs...))}
}

func validPolicyRequest() AgentPolicyUpsertRequest {
	return AgentPolicyUpsertRequest{
		Name:      "standard",
		Harnesses: AgentPolicyHarnessesDTO{Allowed: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw}},
		Skills:    AgentPolicySkillsDTO{Allowed: []string{"npm:@scope/skill"}},
		ModelAccess: AgentPolicyModelAccessDTO{
			Copilot:      AgentPolicyCopilotAccessDTO{Allowed: true},
			SharedModels: AgentPolicySharedModelsAccessDTO{Allowed: []string{"gpt-4.1"}},
			GatewayBudget: &AgentPolicyGatewayBudgetDTO{
				DefaultMonthlyUSD: "20",
				MaxMonthlyUSD:     "50",
			},
		},
	}
}

func TestAdminPolicyCreateAndListOnlyV1Fields(t *testing.T) {
	h := newAdminPolicyHandler()
	body, _ := json.Marshal(validPolicyRequest())
	create := httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.CreatePolicy(w, create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", w.Code, w.Body.String())
	}
	var created AgentPolicyDTO
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "standard" || !created.ModelAccess.Copilot.Allowed || created.ModelAccess.GatewayBudget == nil ||
		created.AgentIdentity.Allowed || created.AgentIdentity.Required || len(created.AgentIdentity.CredentialServices) != 0 || created.EntraID.Enabled {
		t.Fatalf("unexpected policy response: %+v", created)
	}

	list := httptest.NewRequest(http.MethodGet, "/api/v1/admin/policies", nil)
	w = httptest.NewRecorder()
	h.ListPolicies(w, list)
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Policies []AgentPolicyDTO `json:"policies"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Policies) != 1 || response.Policies[0].Name != "standard" || len(response.Policies[0].Skills.Allowed) != 1 {
		t.Fatalf("unexpected policies: %+v", response.Policies)
	}
}

func TestAdminPolicyWritesIdentityPolicies(t *testing.T) {
	for _, test := range []struct {
		name            string
		identity        AgentPolicyAgentIdentityDTO
		entraID         bool
		updatedIdentity AgentPolicyAgentIdentityDTO
		updatedEntraID  bool
	}{
		{name: "disabled without Entra", identity: AgentPolicyAgentIdentityDTO{}, entraID: false, updatedIdentity: AgentPolicyAgentIdentityDTO{}, updatedEntraID: true},
		{name: "disabled with Entra", identity: AgentPolicyAgentIdentityDTO{}, entraID: true, updatedIdentity: AgentPolicyAgentIdentityDTO{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}, updatedEntraID: false},
		{name: "optional without Entra", identity: AgentPolicyAgentIdentityDTO{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}, entraID: false, updatedIdentity: AgentPolicyAgentIdentityDTO{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}, updatedEntraID: true},
		{name: "optional with Entra", identity: AgentPolicyAgentIdentityDTO{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}, entraID: true, updatedIdentity: AgentPolicyAgentIdentityDTO{Allowed: true, Required: true, CredentialServices: []string{"azure-resource-manager"}}, updatedEntraID: false},
		{name: "required without Entra", identity: AgentPolicyAgentIdentityDTO{Allowed: true, Required: true, CredentialServices: []string{"azure-resource-manager"}}, entraID: false, updatedIdentity: AgentPolicyAgentIdentityDTO{Allowed: true, Required: true, CredentialServices: []string{"azure-resource-manager"}}, updatedEntraID: true},
		{name: "required with Entra", identity: AgentPolicyAgentIdentityDTO{Allowed: true, Required: true, CredentialServices: []string{"azure-resource-manager"}}, entraID: true, updatedIdentity: AgentPolicyAgentIdentityDTO{}, updatedEntraID: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAdminPolicyHandler()
			req := validPolicyRequest()
			req.AgentIdentity = &test.identity
			req.EntraID = &AgentPolicyEntraIDDTO{Enabled: test.entraID}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
			if w.Code != http.StatusCreated {
				t.Fatalf("create status %d: %s", w.Code, w.Body.String())
			}
			current, err := h.Kube.GetAgentPolicy(context.Background(), req.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Spec.AgentIdentity, cetirunv1alpha1.AgentPolicyAgentIdentity(test.identity)) || current.Spec.EntraID.Enabled != test.entraID {
				t.Fatalf("create did not persist identities: %+v", current.Spec)
			}

			req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
			req.Name = ""
			req.AgentIdentity = &test.updatedIdentity
			req.EntraID = &AgentPolicyEntraIDDTO{Enabled: test.updatedEntraID}
			body, err = json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
			update.SetPathValue("name", "standard")
			w = httptest.NewRecorder()
			h.UpdatePolicy(w, update)
			if w.Code != http.StatusOK {
				t.Fatalf("update status %d: %s", w.Code, w.Body.String())
			}
			stored, err := h.Kube.GetAgentPolicy(context.Background(), "standard")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Spec.AgentIdentity, cetirunv1alpha1.AgentPolicyAgentIdentity(test.updatedIdentity)) || stored.Spec.EntraID.Enabled != test.updatedEntraID {
				t.Fatalf("update did not persist identities: %+v", stored.Spec)
			}
		})
	}
}

func TestAdminPolicyIdentityOmissionAndNullDisableCapabilities(t *testing.T) {
	for _, test := range []struct {
		name string
		null bool
	}{
		{name: "omitted"},
		{name: "null", null: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			initial := validPolicyRequest()
			initial.AgentIdentity = &AgentPolicyAgentIdentityDTO{Allowed: true, Required: true, CredentialServices: []string{"azure-resource-manager"}}
			initial.EntraID = &AgentPolicyEntraIDDTO{Enabled: true}
			policy := &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: initial.Name, UID: "policy-uid"}, Spec: toAgentPolicySpec(initial)}
			h := newAdminPolicyHandler(policy)
			current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
			if err != nil {
				t.Fatal(err)
			}
			req := validPolicyRequest()
			req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if test.null {
				var raw map[string]any
				if err := json.Unmarshal(body, &raw); err != nil {
					t.Fatal(err)
				}
				raw["agentIdentity"] = nil
				raw["entraID"] = nil
				body, err = json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
			update.SetPathValue("name", "standard")
			w := httptest.NewRecorder()
			h.UpdatePolicy(w, update)
			if w.Code != http.StatusOK {
				t.Fatalf("update status %d: %s", w.Code, w.Body.String())
			}
			stored, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Spec.AgentIdentity, cetirunv1alpha1.AgentPolicyAgentIdentity{}) || stored.Spec.EntraID.Enabled {
				t.Fatalf("identity capabilities were not disabled: %+v", stored.Spec)
			}
		})
	}
}

func TestAdminPolicyRejectsInvalidAgentIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		identity AgentPolicyAgentIdentityDTO
		message  string
	}{
		{name: "required without allowed", identity: AgentPolicyAgentIdentityDTO{Required: true}, message: "agentIdentity.required"},
		{name: "allowed without service", identity: AgentPolicyAgentIdentityDTO{Allowed: true}, message: "azure-resource-manager"},
		{name: "service without allowed", identity: AgentPolicyAgentIdentityDTO{CredentialServices: []string{"azure-resource-manager"}}, message: "agentIdentity.credentialServices"},
		{name: "unknown service", identity: AgentPolicyAgentIdentityDTO{Allowed: true, CredentialServices: []string{"azure-resource-manager", "unknown"}}, message: "may contain only"},
		{name: "duplicate service", identity: AgentPolicyAgentIdentityDTO{Allowed: true, CredentialServices: []string{"azure-resource-manager", "azure-resource-manager"}}, message: "duplicate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAdminPolicyHandler()
			req := validPolicyRequest()
			req.AgentIdentity = &test.identity
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), test.message) {
				t.Fatalf("create status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAdminPolicyRejectsInvalidBudget(t *testing.T) {
	h := newAdminPolicyHandler()
	req := validPolicyRequest()
	req.ModelAccess.GatewayBudget.DefaultMonthlyUSD = "51"
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestAdminPolicyRequiresMatchingModelAccessAndPositiveBudget(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*AgentPolicyUpsertRequest)
	}{
		{"copilot without budget", func(req *AgentPolicyUpsertRequest) { req.ModelAccess.GatewayBudget = nil }},
		{"shared model without budget", func(req *AgentPolicyUpsertRequest) {
			req.ModelAccess.Copilot.Allowed = false
			req.ModelAccess.GatewayBudget = nil
		}},
		{"budget without model access", func(req *AgentPolicyUpsertRequest) {
			req.ModelAccess.Copilot.Allowed = false
			req.ModelAccess.SharedModels.Allowed = nil
		}},
		{"zero default budget", func(req *AgentPolicyUpsertRequest) { req.ModelAccess.GatewayBudget.DefaultMonthlyUSD = "0" }},
		{"zero maximum budget", func(req *AgentPolicyUpsertRequest) { req.ModelAccess.GatewayBudget.MaxMonthlyUSD = "0" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAdminPolicyHandler()
			req := validPolicyRequest()
			test.edit(&req)
			body, _ := json.Marshal(req)
			w := httptest.NewRecorder()
			h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAdminPolicySharedModelNameLength(t *testing.T) {
	for _, test := range []struct {
		name       string
		model      string
		wantStatus int
	}{
		{"ASCII at limit", strings.Repeat("a", 256), http.StatusCreated},
		{"ASCII over limit", strings.Repeat("a", 257), http.StatusBadRequest},
		{"multibyte at limit", strings.Repeat("\u6a21", 256), http.StatusCreated},
		{"multibyte over limit", strings.Repeat("\u6a21", 257), http.StatusBadRequest},
		{"supplementary at limit", strings.Repeat("\U0001f916", 256), http.StatusCreated},
		{"supplementary over limit", strings.Repeat("\U0001f916", 257), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAdminPolicyHandler()
			req := validPolicyRequest()
			req.ModelAccess.SharedModels.Allowed = []string{test.model}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
			if w.Code != test.wantStatus {
				t.Fatalf("status %d, want %d: %s", w.Code, test.wantStatus, w.Body.String())
			}
			stored, err := h.Kube.GetAgentPolicy(context.Background(), req.Name)
			if test.wantStatus == http.StatusBadRequest {
				if !kube.IsNotFound(err) {
					t.Fatalf("rejected model created a policy: policy=%+v, err=%v", stored, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Spec.ModelAccess.SharedModels.Allowed, req.ModelAccess.SharedModels.Allowed) {
				t.Fatalf("model name changed: %+v", stored.Spec.ModelAccess.SharedModels)
			}
		})
	}
}

func TestAdminPolicyPreservesUnicodeModelDuringUnrelatedUpdate(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*AgentPolicyUpsertRequest)
	}{
		{"budget", func(req *AgentPolicyUpsertRequest) { req.ModelAccess.GatewayBudget.MaxMonthlyUSD = "60" }},
		{"skills", func(req *AgentPolicyUpsertRequest) { req.Skills.Allowed = []string{"npm:@scope/another-skill"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := validPolicyRequest()
			req.ModelAccess.SharedModels.Allowed = []string{strings.Repeat("\u6a21\U0001f916", 128)}
			policy := &cetirunv1alpha1.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: req.Name, UID: "policy-uid"},
				Spec:       toAgentPolicySpec(req),
			}
			h := newAdminPolicyHandler(policy)
			current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
			if err != nil {
				t.Fatal(err)
			}
			req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
			test.edit(&req)
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/"+policy.Name, bytes.NewReader(body))
			update.SetPathValue("name", policy.Name)
			w := httptest.NewRecorder()
			h.UpdatePolicy(w, update)
			if w.Code != http.StatusOK {
				t.Fatalf("unrelated update status %d: %s", w.Code, w.Body.String())
			}
			stored, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Spec, toAgentPolicySpec(req)) {
				t.Fatalf("update did not preserve the model and apply the requested edit: %+v", stored.Spec)
			}
		})
	}
}

func TestAdminPolicyUpdateAndDelete(t *testing.T) {
	const policyName = "standard.policy"
	policy := &cetirunv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, UID: "policy-uid"},
		Spec:       toAgentPolicySpec(validPolicyRequest()),
	}
	policy.Spec.Security.ReadOnlyRootFilesystem = new(bool)
	policy.Spec.Network.BlockedCIDRs = []string{"10.0.0.0/8"}
	policy.Spec.AgentIdentity = cetirunv1alpha1.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}
	policy.Spec.EntraID.Enabled = true
	policy.Spec.SandboxSelection.AllowMultiple = true
	policy.Spec.KubernetesAccess = &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName:  "agent",
		ClusterRoleBindings: []cetirunv1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: "view"}},
	}
	preserved := *policy.Spec.DeepCopy()
	h := newAdminPolicyHandler(policy)
	get := httptest.NewRequest(http.MethodGet, "/api/v1/admin/policies/"+policyName, nil)
	get.SetPathValue("name", policyName)
	w := httptest.NewRecorder()
	h.GetPolicy(w, get)
	if w.Code != http.StatusOK {
		t.Fatalf("get status %d: %s", w.Code, w.Body.String())
	}
	current, err := h.Kube.GetAgentPolicy(context.Background(), policyName)
	if err != nil {
		t.Fatal(err)
	}
	req := validPolicyRequest()
	req.Name = ""
	req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
	req.Harnesses.Allowed = []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeHermes}
	body, _ := json.Marshal(req)
	update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/"+policyName, bytes.NewReader(body))
	update.SetPathValue("name", policyName)
	w = httptest.NewRecorder()
	h.UpdatePolicy(w, update)
	if w.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", w.Code, w.Body.String())
	}
	stored, err := h.Kube.GetAgentPolicy(context.Background(), policyName)
	if err != nil || len(stored.Spec.Harnesses.Allowed) != 1 || stored.Spec.Harnesses.Allowed[0] != cetirunv1alpha1.AgentTypeHermes {
		t.Fatalf("stored policy = %+v, err=%v", stored, err)
	}
	if stored.Spec.Security.ReadOnlyRootFilesystem == nil ||
		*stored.Spec.Security.ReadOnlyRootFilesystem != *preserved.Security.ReadOnlyRootFilesystem ||
		len(stored.Spec.Network.BlockedCIDRs) != 1 || stored.Spec.Network.BlockedCIDRs[0] != preserved.Network.BlockedCIDRs[0] ||
		stored.Spec.AgentIdentity.Allowed || stored.Spec.AgentIdentity.Required || len(stored.Spec.AgentIdentity.CredentialServices) != 0 || stored.Spec.EntraID.Enabled ||
		stored.Spec.SandboxSelection != preserved.SandboxSelection ||
		!reflect.DeepEqual(stored.Spec.KubernetesAccess, preserved.KubernetesAccess) {
		t.Fatalf("Portal update did not retain read-only controls or reset identities: before=%+v after=%+v", preserved, stored.Spec)
	}
	deleteBody, _ := json.Marshal(map[string]string{"uid": string(stored.UID), "resourceVersion": stored.ResourceVersion})
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/policies/"+policyName, bytes.NewReader(deleteBody))
	deleteRequest.SetPathValue("name", policyName)
	w = httptest.NewRecorder()
	h.DeletePolicy(w, deleteRequest)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminPolicyExposesStatusAndUpdatesPortalManagedFields(t *testing.T) {
	readOnly := true
	policy := &cetirunv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid", Generation: 7},
		Spec:       toAgentPolicySpec(validPolicyRequest()),
		Status: cetirunv1alpha1.AgentPolicyStatus{
			ObservedGeneration: 6,
			Conditions:         []metav1.Condition{{Type: cetirunv1alpha1.ConditionAgentPolicyAccepted, Status: metav1.ConditionFalse, Reason: "Pending", LastTransitionTime: metav1.Now()}},
		},
	}
	policy.Spec.AgentIdentity = cetirunv1alpha1.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}
	policy.Spec.EntraID.Enabled = true
	policy.Spec.Security.ReadOnlyRootFilesystem = &readOnly
	policy.Spec.Network.BlockedFQDNs = []string{"example.com"}
	h := newAdminPolicyHandler(policy)
	current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	falseValue := false
	trueValue := true
	mode := cetirunv1alpha1.AgentPolicyIngressDisabled
	blockedCIDRs := []string{"192.0.2.0/24"}
	blockedPorts := []AgentPolicyPortDTO{{Port: 53, Protocol: corev1.ProtocolUDP}}
	internal := []AgentPolicyCIDRRuleDTO{{CIDR: "198.51.100.0/24", Ports: []AgentPolicyPortDTO{{Port: 443, Protocol: corev1.ProtocolTCP}}}}
	req := validPolicyRequest()
	req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
	req.SandboxSelection = &AgentPolicySandboxSelectionRequestDTO{AllowMultiple: &trueValue}
	req.Security = &AgentPolicySecurityDTO{ReadOnlyRootFilesystem: &falseValue}
	req.Network = &AgentPolicyNetworkRequestDTO{
		BlockedCIDRs:         &blockedCIDRs,
		BlockedFQDNs:         &[]string{"example.com"},
		BlockedPorts:         &blockedPorts,
		AllowedInternalCIDRs: &internal,
		Ingress:              &AgentPolicyIngressRequestDTO{Mode: &mode, RequireMTLS: &falseValue},
	}
	req.AgentIdentity = &AgentPolicyAgentIdentityDTO{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}
	req.EntraID = &AgentPolicyEntraIDDTO{Enabled: true}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
	request.SetPathValue("name", "standard")
	w := httptest.NewRecorder()
	h.UpdatePolicy(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var response AgentPolicyDTO
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Generation < 7 || response.ObservedGeneration != 6 || len(response.Conditions) != 1 || !response.AgentIdentity.Allowed || !response.EntraID.Enabled {
		t.Fatalf("status or identity fields missing from DTO: %+v", response)
	}
	stored, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Spec.SandboxSelection.AllowMultiple || stored.Spec.Security.ReadOnlyRootFilesystem == nil || *stored.Spec.Security.ReadOnlyRootFilesystem ||
		len(stored.Spec.Network.BlockedCIDRs) != 1 || stored.Spec.Network.BlockedCIDRs[0] != "192.0.2.0/24" ||
		len(stored.Spec.Network.BlockedPorts) != 1 || stored.Spec.Network.Ingress.Mode != cetirunv1alpha1.AgentPolicyIngressDisabled ||
		stored.Spec.KubernetesAccess != nil {
		t.Fatalf("editable fields were not persisted: %+v", stored.Spec)
	}
	if !reflect.DeepEqual(stored.Spec.AgentIdentity, policy.Spec.AgentIdentity) || stored.Spec.EntraID != policy.Spec.EntraID ||
		!reflect.DeepEqual(stored.Spec.Network.BlockedFQDNs, policy.Spec.Network.BlockedFQDNs) || stored.Spec.Network.Ingress.RequireMTLS != policy.Spec.Network.Ingress.RequireMTLS {
		t.Fatalf("identity or read-only fields changed: %+v", stored.Spec)
	}
}

func TestAdminPolicyPreservesOmittedFieldsIncludingKubernetesAccess(t *testing.T) {
	for _, test := range []struct {
		name         string
		emptyNetwork bool
	}{
		{name: "network omitted"},
		{name: "empty network object", emptyNetwork: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: toAgentPolicySpec(validPolicyRequest())}
			policy.Spec.SandboxSelection.AllowMultiple = true
			policy.Spec.Network.BlockedCIDRs = []string{"10.0.0.0/8", "10.0.0.0/8"}
			policy.Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}, {Port: 443, Protocol: corev1.ProtocolTCP}}
			policy.Spec.Network.AllowedInternalCIDRs = []cetirunv1alpha1.AgentPolicyCIDRRule{
				{CIDR: "198.51.100.0/24", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}}},
				{CIDR: "198.51.100.0/24", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolUDP}}},
			}
			policy.Spec.Network.BlockedFQDNs = []string{"example.com"}
			policy.Spec.Network.Ingress.RequireMTLS = true
			policy.Spec.KubernetesAccess = &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent", ClusterRoleBindings: []cetirunv1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: "view"}}}
			preservedNetwork := *policy.Spec.Network.DeepCopy()
			preservedKubernetesAccess := policy.Spec.KubernetesAccess.DeepCopy()
			h := newAdminPolicyHandler(policy)
			current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
			if err != nil {
				t.Fatal(err)
			}
			req := validPolicyRequest()
			req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
			req.ModelAccess.GatewayBudget.MaxMonthlyUSD = "60"
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if test.emptyNetwork {
				var partial map[string]any
				if err := json.Unmarshal(body, &partial); err != nil {
					t.Fatal(err)
				}
				partial["network"] = map[string]any{}
				body, err = json.Marshal(partial)
				if err != nil {
					t.Fatal(err)
				}
			}
			update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
			update.SetPathValue("name", "standard")
			w := httptest.NewRecorder()
			h.UpdatePolicy(w, update)
			if w.Code != http.StatusOK {
				t.Fatalf("omitted-field update status %d: %s", w.Code, w.Body.String())
			}
			stored, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Spec.ModelAccess.GatewayBudget == nil || stored.Spec.ModelAccess.GatewayBudget.MaxMonthlyUSD != "60" ||
				!stored.Spec.SandboxSelection.AllowMultiple || !reflect.DeepEqual(stored.Spec.Network, preservedNetwork) ||
				!reflect.DeepEqual(stored.Spec.KubernetesAccess, preservedKubernetesAccess) {
				t.Fatalf("model update discarded retained fields: %+v", stored.Spec)
			}
		})
	}
}

func TestAdminPolicyPreservesLegacyNetworkRulesDuringPartialNetworkUpdate(t *testing.T) {
	policy := &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: toAgentPolicySpec(validPolicyRequest())}
	policy.Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}, {Port: 443, Protocol: corev1.ProtocolTCP}}
	policy.Spec.Network.AllowedInternalCIDRs = []cetirunv1alpha1.AgentPolicyCIDRRule{
		{CIDR: "198.51.100.0/24", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}}},
		{CIDR: "198.51.100.0/24", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolUDP}}},
	}
	h := newAdminPolicyHandler(policy)
	current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	blockedCIDRs := []string{"192.0.2.0/24"}
	req := validPolicyRequest()
	req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
	req.Network = &AgentPolicyNetworkRequestDTO{BlockedCIDRs: &blockedCIDRs}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
	update.SetPathValue("name", "standard")
	w := httptest.NewRecorder()
	h.UpdatePolicy(w, update)
	if w.Code != http.StatusOK {
		t.Fatalf("partial network update status %d: %s", w.Code, w.Body.String())
	}
	stored, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Spec.Network.BlockedCIDRs, blockedCIDRs) ||
		!reflect.DeepEqual(stored.Spec.Network.BlockedPorts, policy.Spec.Network.BlockedPorts) ||
		!reflect.DeepEqual(stored.Spec.Network.AllowedInternalCIDRs, policy.Spec.Network.AllowedInternalCIDRs) {
		t.Fatalf("partial network update changed retained legacy rules: %+v", stored.Spec.Network)
	}

	invalidCIDRs := []string{"not-a-cidr"}
	req = validPolicyRequest()
	req.UID, req.ResourceVersion = string(stored.UID), stored.ResourceVersion
	req.Network = &AgentPolicyNetworkRequestDTO{BlockedCIDRs: &invalidCIDRs}
	body, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	update = httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
	update.SetPathValue("name", "standard")
	w = httptest.NewRecorder()
	h.UpdatePolicy(w, update)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "network.blockedCIDRs") {
		t.Fatalf("invalid supplied network field status %d: %s", w.Code, w.Body.String())
	}
	afterInvalid, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterInvalid.Spec, stored.Spec) {
		t.Fatalf("invalid network update changed policy: before=%+v after=%+v", stored.Spec, afterInvalid.Spec)
	}

	duplicateCIDRs := []string{"192.0.2.0/24", "192.0.2.0/24"}
	req = validPolicyRequest()
	req.UID, req.ResourceVersion = string(stored.UID), stored.ResourceVersion
	req.Network = &AgentPolicyNetworkRequestDTO{BlockedCIDRs: &duplicateCIDRs}
	body, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	update = httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
	update.SetPathValue("name", "standard")
	w = httptest.NewRecorder()
	h.UpdatePolicy(w, update)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "must not contain duplicate entries") {
		t.Fatalf("duplicate supplied network field status %d: %s", w.Code, w.Body.String())
	}
	afterDuplicate, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterDuplicate.Spec, stored.Spec) {
		t.Fatalf("duplicate network update changed policy: before=%+v after=%+v", stored.Spec, afterDuplicate.Spec)
	}
}

func TestAdminPolicyRejectsKubernetesAccessOnCreate(t *testing.T) {
	h := newAdminPolicyHandler()
	req := validPolicyRequest()
	req.KubernetesAccess = &AgentPolicyKubernetesAccessDTO{
		ServiceAccountName:  "agent-access",
		ClusterRoleBindings: []AgentPolicyClusterRoleBindingDTO{{ClusterRoleName: "view"}},
	}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	h.CreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kubernetesAccess is read-only") {
		t.Fatalf("create status %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminPolicyRejectsKubernetesAccessChanges(t *testing.T) {
	access := &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName:  "agent-access",
		ClusterRoleBindings: []cetirunv1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: "view"}},
	}
	for _, test := range []struct {
		name   string
		policy *cetirunv1alpha1.AgentPolicy
		body   func(t *testing.T, current *cetirunv1alpha1.AgentPolicy) []byte
	}{
		{
			name: "change existing access",
			policy: &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: func() cetirunv1alpha1.AgentPolicySpec {
				spec := toAgentPolicySpec(validPolicyRequest())
				spec.KubernetesAccess = access.DeepCopy()
				return spec
			}()},
			body: func(t *testing.T, current *cetirunv1alpha1.AgentPolicy) []byte {
				t.Helper()
				req := validPolicyRequest()
				req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
				req.KubernetesAccess = &AgentPolicyKubernetesAccessDTO{ServiceAccountName: "agent-access", ClusterRoleBindings: []AgentPolicyClusterRoleBindingDTO{{ClusterRoleName: "edit"}}}
				body, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				return body
			},
		},
		{
			name: "clear existing access",
			policy: &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: func() cetirunv1alpha1.AgentPolicySpec {
				spec := toAgentPolicySpec(validPolicyRequest())
				spec.KubernetesAccess = access.DeepCopy()
				return spec
			}()},
			body: func(t *testing.T, current *cetirunv1alpha1.AgentPolicy) []byte {
				t.Helper()
				req := validPolicyRequest()
				req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
				body, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				var raw map[string]any
				if err := json.Unmarshal(body, &raw); err != nil {
					t.Fatal(err)
				}
				raw["kubernetesAccess"] = nil
				body, err = json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
				return body
			},
		},
		{
			name:   "set access when none exists",
			policy: &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: toAgentPolicySpec(validPolicyRequest())},
			body: func(t *testing.T, current *cetirunv1alpha1.AgentPolicy) []byte {
				t.Helper()
				req := validPolicyRequest()
				req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
				req.KubernetesAccess = &AgentPolicyKubernetesAccessDTO{ServiceAccountName: "agent-access", ClusterRoleBindings: []AgentPolicyClusterRoleBindingDTO{{ClusterRoleName: "view"}}}
				body, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				return body
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAdminPolicyHandler(test.policy)
			current, err := h.Kube.GetAgentPolicy(context.Background(), test.policy.Name)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(test.body(t, current)))
			request.SetPathValue("name", "standard")
			w := httptest.NewRecorder()
			h.UpdatePolicy(w, request)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kubernetesAccess is read-only") {
				t.Fatalf("update status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAdminPolicyAllowsIdenticalKubernetesAccessWhileUpdatingOtherFields(t *testing.T) {
	policy := &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: toAgentPolicySpec(validPolicyRequest())}
	policy.Spec.KubernetesAccess = &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName:  "agent-access",
		RoleBindings:        []cetirunv1alpha1.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "view"}},
		ClusterRoleBindings: []cetirunv1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: "cluster-reader"}},
	}
	h := newAdminPolicyHandler(policy)
	current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	req := validPolicyRequest()
	req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
	req.Harnesses.Allowed = []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeHermes}
	req.KubernetesAccess = toAgentPolicyKubernetesAccessDTO(current.Spec.KubernetesAccess)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
	request.SetPathValue("name", "standard")
	w := httptest.NewRecorder()
	h.UpdatePolicy(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", w.Code, w.Body.String())
	}
	stored, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Harnesses.Allowed) != 1 || stored.Spec.Harnesses.Allowed[0] != cetirunv1alpha1.AgentTypeHermes || !reflect.DeepEqual(stored.Spec.KubernetesAccess, policy.Spec.KubernetesAccess) {
		t.Fatalf("update did not preserve Kubernetes access: %+v", stored.Spec)
	}
}

func TestAdminPolicyRejectsRemainingReadOnlyChanges(t *testing.T) {
	blockedFQDNs := []string{"example.com"}
	requireMTLS := true
	for _, test := range []struct {
		name    string
		network AgentPolicyNetworkRequestDTO
	}{
		{name: "network.blockedFQDNs", network: AgentPolicyNetworkRequestDTO{BlockedFQDNs: &blockedFQDNs}},
		{name: "network.ingress.requireMTLS", network: AgentPolicyNetworkRequestDTO{Ingress: &AgentPolicyIngressRequestDTO{RequireMTLS: &requireMTLS}}},
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			t.Run(test.name+"/"+method, func(t *testing.T) {
				policy := &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: toAgentPolicySpec(validPolicyRequest())}
				h := newAdminPolicyHandler(policy)
				current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
				if err != nil {
					t.Fatal(err)
				}
				req := validPolicyRequest()
				req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
				req.Network = &test.network
				body, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(method, "/api/v1/admin/policies/standard", bytes.NewReader(body))
				request.SetPathValue("name", "standard")
				w := httptest.NewRecorder()
				if method == http.MethodPost {
					h.CreatePolicy(w, request)
				} else {
					h.UpdatePolicy(w, request)
				}
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), test.name+" is read-only") {
					t.Fatalf("read-only change status %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestAdminPolicyMapsKubernetesInvalidToUnprocessableEntity(t *testing.T) {
	policy := &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"}, Spec: toAgentPolicySpec(validPolicyRequest())}
	base := newFakeClient(policy)
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: cetirunv1alpha1.GroupVersion.Group, Kind: "AgentPolicy"}, policy.Name, nil)
	storage := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return invalid
		},
	})
	h := &AdminPolicyHandler{Kube: kube.NewClient(storage)}
	current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	req := validPolicyRequest()
	req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
	req.Harnesses.Allowed = []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeHermes}
	body, _ := json.Marshal(req)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
	request.SetPathValue("name", "standard")
	w := httptest.NewRecorder()
	h.UpdatePolicy(w, request)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "configuration was rejected") {
		t.Fatalf("invalid API response status %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminPolicyRejectsStaleUpdateAndDelete(t *testing.T) {
	policy := &cetirunv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid"},
		Spec:       toAgentPolicySpec(validPolicyRequest()),
	}
	h := newAdminPolicyHandler(policy)
	stale, err := h.Kube.GetAgentPolicy(context.Background(), "standard")
	if err != nil {
		t.Fatal(err)
	}
	version := kube.AgentPolicyVersion{UID: stale.UID, ResourceVersion: stale.ResourceVersion}
	changed := *stale.Spec.DeepCopy()
	changed.Harnesses.Allowed = []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeHermes}
	if _, err := h.Kube.UpdateAgentPolicySpec(context.Background(), "standard", changed, version); err != nil {
		t.Fatal(err)
	}
	req := validPolicyRequest()
	req.Name = ""
	req.UID, req.ResourceVersion = string(stale.UID), stale.ResourceVersion
	body, _ := json.Marshal(req)
	update := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
	update.SetPathValue("name", "standard")
	w := httptest.NewRecorder()
	h.UpdatePolicy(w, update)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale update status %d, want 409: %s", w.Code, w.Body.String())
	}
	deleteBody, _ := json.Marshal(map[string]string{"uid": string(stale.UID), "resourceVersion": stale.ResourceVersion})
	delete := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/policies/standard", bytes.NewReader(deleteBody))
	delete.SetPathValue("name", "standard")
	w = httptest.NewRecorder()
	h.DeletePolicy(w, delete)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale delete status %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestAdminPolicyClassifiesCoordinatedUnavailableErrors(testContext *testing.T) {
	for _, test := range []struct {
		name          string
		mutationErr   error
		initialGetErr error
		wantStatus    int
		wantCode      string
	}{
		{
			name: "coordinated compatibility rejection", mutationErr: compatibilityAdmissionError(testContext),
			wantStatus: http.StatusServiceUnavailable, wantCode: "policy_change_unavailable",
		},
		{
			name:        "coordinated not found",
			mutationErr: apierrors.NewNotFound(schema.GroupResource{Group: "cetirun.microsoft.com", Resource: "agentpolicies"}, "standard"),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    "policy_change_unavailable",
		},
		{
			name:        "coordinated forbidden",
			mutationErr: apierrors.NewForbidden(schema.GroupResource{Group: "cetirun.microsoft.com", Resource: "agentpolicies"}, "standard", nil),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    "policy_change_unavailable",
		},
		{
			name:        "coordinated conflict",
			mutationErr: apierrors.NewConflict(schema.GroupResource{Group: "cetirun.microsoft.com", Resource: "agentpolicies"}, "standard", nil),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    "policy_change_unavailable",
		},
		{
			name:          "initial not found",
			initialGetErr: apierrors.NewNotFound(schema.GroupResource{Group: "cetirun.microsoft.com", Resource: "agentpolicies"}, "standard"),
			wantStatus:    http.StatusNotFound,
			wantCode:      "policy_not_found",
		},
		{
			name:          "initial forbidden",
			initialGetErr: apierrors.NewForbidden(schema.GroupResource{Group: "cetirun.microsoft.com", Resource: "agentpolicies"}, "standard", nil),
			wantStatus:    http.StatusForbidden,
			wantCode:      "policy_forbidden",
		},
	} {
		for _, operation := range []struct {
			name   string
			invoke func(*AdminPolicyHandler, kube.AgentPolicyVersion) *httptest.ResponseRecorder
		}{
			{
				name: "update",
				invoke: func(handler *AdminPolicyHandler, version kube.AgentPolicyVersion) *httptest.ResponseRecorder {
					req := validPolicyRequest()
					req.Name = ""
					req.Harnesses.Allowed = []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeHermes}
					req.UID = string(version.UID)
					req.ResourceVersion = version.ResourceVersion
					body, err := json.Marshal(req)
					if err != nil {
						testContext.Fatal(err)
					}
					request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/policies/standard", bytes.NewReader(body))
					request.SetPathValue("name", "standard")
					response := httptest.NewRecorder()
					handler.UpdatePolicy(response, request)
					return response
				},
			},
			{
				name: "delete",
				invoke: func(handler *AdminPolicyHandler, version kube.AgentPolicyVersion) *httptest.ResponseRecorder {
					body, err := json.Marshal(map[string]string{"uid": string(version.UID), "resourceVersion": version.ResourceVersion})
					if err != nil {
						testContext.Fatal(err)
					}
					request := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/policies/standard", bytes.NewReader(body))
					request.SetPathValue("name", "standard")
					response := httptest.NewRecorder()
					handler.DeletePolicy(response, request)
					return response
				},
			},
		} {
			testContext.Run(test.name+"/"+operation.name, func(testContext *testing.T) {
				handler, version := newCoordinatedAdminPolicyHandler(testContext, test.mutationErr, test.initialGetErr)
				response := operation.invoke(handler, version)
				if response.Code != test.wantStatus {
					testContext.Fatalf("status %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
				}
				var payload ErrorResponse
				if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
					testContext.Fatal(err)
				}
				if payload.Error.Code != test.wantCode {
					testContext.Fatalf("error code %q, want %q", payload.Error.Code, test.wantCode)
				}
			})
		}
	}
}

func newCoordinatedAdminPolicyHandler(testContext *testing.T, mutationErr, initialGetErr error) (*AdminPolicyHandler, kube.AgentPolicyVersion) {
	testContext.Helper()
	authority := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "cetirun-system",
			Name:       "configuration-coordination",
			UID:        types.UID("authority-uid"),
			Finalizers: []string{"sandbox.cetirun.microsoft.com/coordination"},
		},
		Data: map[string]string{"coordination.json": `{"version":1,"executions":[],"changes":[]}`},
	}
	policy := &cetirunv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: types.UID("policy-uid")},
		Spec:       toAgentPolicySpec(validPolicyRequest()),
	}
	storage := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().WithObjects(authority, policy).Build()
	current := &cetirunv1alpha1.AgentPolicy{}
	if err := storage.Get(context.Background(), client.ObjectKeyFromObject(policy), current); err != nil {
		testContext.Fatal(err)
	}
	writer := interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(ctx context.Context, storage client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*cetirunv1alpha1.AgentPolicy); ok && initialGetErr != nil {
				return initialGetErr
			}
			return storage.Get(ctx, key, obj, opts...)
		},
		Update: func(ctx context.Context, storage client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*cetirunv1alpha1.AgentPolicy); ok && mutationErr != nil {
				return mutationErr
			}
			return storage.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, storage client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*cetirunv1alpha1.AgentPolicy); ok && mutationErr != nil {
				return mutationErr
			}
			return storage.Delete(ctx, obj, opts...)
		},
	})
	coordination, err := configurationcoordination.New(writer, storage, configurationcoordination.Authority{
		Namespace: authority.Namespace,
		Name:      authority.Name,
		UID:       authority.UID,
	})
	if err != nil {
		testContext.Fatal(err)
	}
	portal := kube.NewClient(writer)
	portal.SetConfigurationCoordination(coordination)
	return &AdminPolicyHandler{Kube: portal}, kube.AgentPolicyVersion{UID: current.UID, ResourceVersion: current.ResourceVersion}
}
