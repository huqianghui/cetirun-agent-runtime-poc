// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func creationPolicy() (*api.Team, *api.AgentPolicy) {
	team := gatewayTeam("policy-team")
	team.Spec.AgentPolicyRef = "hermes-policy"
	team.Spec.SandboxRefs = []string{"test-sandbox"}
	team.Spec.DefaultSandboxRef = "test-sandbox"
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "hermes-policy"}, Spec: api.AgentPolicySpec{
		Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}},
		ModelAccess: api.AgentPolicyModelAccess{SharedModels: api.AgentPolicySharedModelsAccess{Allowed: []string{"gpt-6-mini"}},
			GatewayBudget: &api.AgentPolicyGatewayBudget{DefaultMonthlyUSD: "10", MaxMonthlyUSD: "20"}},
	}}
	return team, policy
}

func creationPolicyHandler(team *api.Team, policy *api.AgentPolicy) (*InstanceHandler, client.Client) {
	fc := newFakeClient(team, policy, catalogConfigMap("gpt-6", "gpt-6-mini"))
	cfg := gatewayConfig()
	cfg.AllowedAgentTypes = []string{"hermes", "openclaw", "custom"}
	cfg.AgentIdentityEnabled = true
	cfg.SandboxRuntimes = map[string]config.SandboxRuntime{
		"hermes":   {ChartVersion: "1.2.3", Image: "registry.example.com/hermes@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Port: 8642},
		"openclaw": {ChartVersion: "1.2.3", Image: "registry.example.com/openclaw@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Port: 18789},
	}
	return &InstanceHandler{Kube: kube.NewClient(fc), Config: cfg}, fc
}

func readCreateOptions(t *testing.T, h *InstanceHandler, team string) CreateOptionsResponse {
	t.Helper()
	r := managementRequest(http.MethodGet, "")
	r.URL.Path = "/api/v1/instance/create-options"
	r.URL.RawQuery = "team=" + team
	w := httptest.NewRecorder()
	h.GetCreateOptions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("options: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("options must not be cached")
	}
	var options CreateOptionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	return options
}

func TestCreationOptionsProjectPolicyAndRecheckBeforeWrite(t *testing.T) {
	team, policy := creationPolicy()
	h, fc := creationPolicyHandler(team, policy)
	options := readCreateOptions(t, h, team.Name)
	if options.PolicyName != policy.Name || options.Blocked != nil || !slices.Equal(options.AllowedAgentTypes, []api.AgentType{api.AgentTypeHermes}) ||
		len(options.Gateway.SharedModels) != 1 || options.Gateway.SharedModels[0].Name != "gpt-6-mini" || options.Gateway.GitHubCopilot || options.AgentIdentity.Allowed {
		t.Fatalf("unexpected options: %+v", options)
	}
	// The page has already loaded. Revoke Hermes before POST; no write may occur,
	// including the optional native Secrets that would otherwise precede Agent creation.
	current, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	current.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
	if err := fc.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team","gatewayToken":"synthetic-token"}`))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "agent_type_not_allowed" {
		t.Fatalf("stale policy: %d %s", w.Code, w.Body.String())
	}
	assertNoCreationWrites(t, fc)
}

func assertNoCreationWrites(t *testing.T, fc client.Client) {
	t.Helper()
	for _, list := range []client.ObjectList{&api.AgentList{}, &corev1.SecretList{}, &corev1.NamespaceList{}} {
		if err := fc.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		var data struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(encoded, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Items) != 0 {
			t.Fatalf("rejected creation wrote %T", list)
		}
	}
}

func TestCreateRejectsPolicyViolationsWithoutWrites(t *testing.T) {
	cases := []struct {
		name, body, code string
		change           func(*InstanceHandler, *api.Team, *api.AgentPolicy)
	}{
		{name: "harness", body: `"agent":{"type":"openclaw"}`, code: "agent_type_not_allowed"},
		{name: "custom", body: `"agent":{"type":"custom","image":"registry.example.com/test:1"}`, code: "agent_type_not_allowed"},
		{name: "copilot", body: `"agent":{"type":"hermes"},"modelAccess":{"githubCopilot":true}`, code: "invalid_model_access"},
		{name: "default model", body: `"agent":{"type":"hermes"},"model":"gpt-6"`, code: "model_not_allowed"},
		{name: "copilot default", body: `"agent":{"type":"hermes"},"model":"github-copilot/gpt-6"`, code: "model_not_allowed"},
		{name: "no model source", body: `"agent":{"type":"hermes"},"modelAccess":{"sharedModels":false,"githubCopilot":false}`, code: "invalid_model_access"},
		{name: "identity", body: `"agent":{"type":"hermes"},"agentIdentityEnabled":true`, code: "policy_identity_not_allowed"},
		{name: "skills", body: `"agent":{"type":"hermes"}`, code: "policy_skills_conflict", change: func(_ *InstanceHandler, t *api.Team, _ *api.AgentPolicy) { t.Spec.Skills = []string{"npm:unapproved"} }},
		{name: "missing budget", body: `"agent":{"type":"hermes"}`, code: "invalid_policy", change: func(_ *InstanceHandler, _ *api.Team, p *api.AgentPolicy) { p.Spec.ModelAccess.GatewayBudget = nil }},
		{name: "disabled gateway", body: `"agent":{"type":"hermes"}`, code: "gateway_required", change: func(h *InstanceHandler, _ *api.Team, _ *api.AgentPolicy) { h.Config.AIGatewayEnabled = false }},
		{name: "empty intersection", body: `"agent":{"type":"openclaw"}`, code: "agent_type_not_allowed", change: func(h *InstanceHandler, _ *api.Team, _ *api.AgentPolicy) {
			h.Config.AllowedAgentTypes = []string{"openclaw"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			team, policy := creationPolicy()
			h, _ := creationPolicyHandler(team, policy)
			if tc.change != nil {
				tc.change(h, team, policy)
			}
			fc := newFakeClient(team, policy, catalogConfigMap("gpt-6", "gpt-6-mini"))
			h.Kube = kube.NewClient(fc)
			w := httptest.NewRecorder()
			h.CreateInstance(w, managementRequest(http.MethodPost, `{"team":"policy-team",`+tc.body+`}`))
			if w.Code < 400 || errorCode(t, w) != tc.code {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			assertNoCreationWrites(t, fc)
		})
	}
}

func TestCreatePersistsEffectiveModelDefaultsAndPolicyBinding(t *testing.T) {
	for _, body := range []string{``, `,"modelAccess":{}`, `,"modelAccess":{"sharedModels":true}`, `,"model":"gpt-6-mini"`} {
		t.Run(body, func(t *testing.T) {
			team, policy := creationPolicy()
			h, fc := creationPolicyHandler(team, policy)
			w := httptest.NewRecorder()
			h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team"`+body+`}`))
			if w.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", w.Code, w.Body.String())
			}
			agent := &api.Agent{}
			if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, agent); err != nil {
				t.Fatal(err)
			}
			if agent.Spec.SandboxRef != "test-sandbox" || agent.Spec.Team != team.Name || agent.Spec.ModelAccess == nil || agent.Spec.ModelAccess.SharedModels == nil || agent.Spec.ModelAccess.GitHubCopilot == nil || !*agent.Spec.ModelAccess.SharedModels || *agent.Spec.ModelAccess.GitHubCopilot || agent.Spec.ModelAccess.MonthlyBudgetUSD != "" {
				t.Fatalf("invalid persisted defaults: %+v", agent.Spec)
			}
		})
	}
}

func TestCreateCustomSandboxModelSelection(t *testing.T) {
	tests := []struct {
		name, fields, code       string
		status                   int
		shared, copilot, unknown bool
		change                   func(*api.AgentPolicy)
	}{
		{name: "policy defaults", status: http.StatusCreated, shared: true},
		{name: "explicit shared", fields: `,"modelAccess":{"sharedModels":true,"githubCopilot":false}`, status: http.StatusCreated, shared: true},
		{name: "allowed default model", fields: `,"model":"gpt-6-mini"`, status: http.StatusCreated, shared: true},
		{name: "copilot only", status: http.StatusCreated, copilot: true, change: func(p *api.AgentPolicy) {
			p.Spec.ModelAccess.SharedModels.Allowed = nil
			p.Spec.ModelAccess.Copilot.Allowed = true
		}},
		{name: "owner disables shared", fields: `,"modelAccess":{"sharedModels":false,"githubCopilot":true}`, status: http.StatusCreated, copilot: true, change: func(p *api.AgentPolicy) {
			p.Spec.ModelAccess.Copilot.Allowed = true
		}},
		{name: "forbidden copilot", fields: `,"modelAccess":{"githubCopilot":true}`, status: http.StatusBadRequest, code: "invalid_model_access"},
		{name: "forbidden default model", fields: `,"model":"gpt-6"`, status: http.StatusForbidden, code: "model_not_allowed"},
		{name: "no requested source", fields: `,"modelAccess":{"sharedModels":false,"githubCopilot":false}`, status: http.StatusBadRequest, code: "invalid_model_access"},
		{name: "no permitted source", status: http.StatusConflict, code: "no_model_source", change: func(p *api.AgentPolicy) {
			p.Spec.ModelAccess.SharedModels.Allowed = []string{"unavailable-model"}
		}},
		{name: "unknown catalog", status: http.StatusServiceUnavailable, code: "model_sources_unknown", unknown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			team, policy := creationPolicy()
			policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeCustom}
			if tt.change != nil {
				tt.change(policy)
			}
			h, fc := creationPolicyHandler(team, policy)
			if tt.unknown {
				if err := fc.Delete(context.Background(), catalogConfigMap("gpt-6", "gpt-6-mini")); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			body := `{"agent":{"type":"custom","image":"` + managementImage + `","username":"assistant"},"team":"policy-team"` + tt.fields + `}`
			h.CreateInstance(w, managementRequest(http.MethodPost, body))
			if w.Code != tt.status {
				t.Fatalf("create: %d %s, want %d", w.Code, w.Body.String(), tt.status)
			}
			if tt.status != http.StatusCreated {
				if code := errorCode(t, w); code != tt.code {
					t.Fatalf("error %q, want %q", code, tt.code)
				}
				assertNoCreationWrites(t, fc)
				return
			}
			agent := &api.Agent{}
			if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, agent); err != nil {
				t.Fatal(err)
			}
			selection := agent.Spec.ModelAccess
			if agent.Spec.SandboxRef != "test-sandbox" || selection == nil || selection.SharedModels == nil ||
				selection.GitHubCopilot == nil || *selection.SharedModels != tt.shared ||
				*selection.GitHubCopilot != tt.copilot || selection.MonthlyBudgetUSD != "" {
				t.Fatalf("model selection was not recorded explicitly: %+v", agent.Spec)
			}
		})
	}
}

func TestCreationIdentityRequiredAndAvailable(t *testing.T) {
	team, policy := creationPolicy()
	policy.Spec.AgentIdentity = api.AgentPolicyAgentIdentity{Allowed: true, Required: true, CredentialServices: []string{"azure-resource-manager"}}
	h, fc := creationPolicyHandler(team, policy)
	options := readCreateOptions(t, h, team.Name)
	if !options.AgentIdentity.Required || !options.AgentIdentity.Allowed || options.Blocked != nil {
		t.Fatalf("identity options: %+v", options)
	}
	w := httptest.NewRecorder()
	h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team"}`))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "policy_identity_required" {
		t.Fatalf("required: %d %s", w.Code, w.Body.String())
	}
	assertNoCreationWrites(t, fc)
	w = httptest.NewRecorder()
	h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team","agentIdentityEnabled":true}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("identity: %d %s", w.Code, w.Body.String())
	}
	// A required identity cannot be offered when the capability or owner is absent.
	h.Config.AgentIdentityEnabled = false
	if o := readCreateOptions(t, h, team.Name); o.Blocked == nil || o.AgentIdentity.Allowed {
		t.Fatalf("disabled identity: %+v", o)
	}
	h.Config.AgentIdentityEnabled = true
	r := withClaims(httptest.NewRequest(http.MethodGet, "/api/v1/instance/create-options?team=policy-team", nil), "alice", "Alice", "alice@example.com")
	w = httptest.NewRecorder()
	h.GetCreateOptions(w, r)
	var o CreateOptionsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &o)
	if o.Blocked == nil || o.AgentIdentity.Allowed {
		t.Fatalf("unverified owner: %s", w.Body.String())
	}
}

func TestCreationOptionsDependenciesFailClosed(t *testing.T) {
	for _, kind := range []string{"missing policy", "read failure", "deleting policy", "partial binding", "no deployment harness"} {
		t.Run(kind, func(t *testing.T) {
			team, policy := creationPolicy()
			h, _ := creationPolicyHandler(team, policy)
			switch kind {
			case "missing policy":
				team.Spec.AgentPolicyRef = "missing"
			case "deleting policy":
				now := metav1.Now()
				policy.DeletionTimestamp = &now
				policy.Finalizers = []string{"example.com/test"}
			case "partial binding":
				team.Spec.DefaultSandboxRef = ""
			case "no deployment harness":
				h.Config.AllowedAgentTypes = []string{}
			}
			fc := newFakeClient(team, policy, catalogConfigMap("gpt-6-mini"))
			if kind == "read failure" {
				fc = interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*api.AgentPolicy); ok {
						return errors.New("synthetic dependency outage")
					}
					return c.Get(ctx, key, obj, opts...)
				}})
			}
			h.Kube = kube.NewClient(fc)
			w := httptest.NewRecorder()
			h.GetCreateOptions(w, managementOptionsRequest())
			if w.Code == http.StatusOK {
				var o CreateOptionsResponse
				_ = json.Unmarshal(w.Body.Bytes(), &o)
				if o.Blocked == nil {
					t.Fatalf("failure fell back: %s", w.Body.String())
				}
			}
			w = httptest.NewRecorder()
			h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team"}`))
			if w.Code < 400 {
				t.Fatalf("failure allowed creation: %d", w.Code)
			}
			assertNoCreationWrites(t, fc)
		})
	}
	h, _ := creationPolicyHandler(creationPolicy())
	w := httptest.NewRecorder()
	h.GetCreateOptions(w, httptest.NewRequest(http.MethodGet, "/api/v1/instance/create-options?team=policy-team", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
}

func managementOptionsRequest() *http.Request {
	r := managementRequest(http.MethodGet, "")
	r.URL.Path = "/api/v1/instance/create-options"
	r.URL.RawQuery = "team=policy-team"
	return r
}

func TestRegistryCreationRetryRechecksPolicy(t *testing.T) {
	failing := true
	h, fc := registryHandler(t, &failing)
	original := createdPartialRegistryForPolicy(t, h)
	team, policy := creationPolicy()
	team.Name = "aks"
	policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
	existing, err := h.Kube.GetTeam(context.Background(), "aks")
	if err != nil {
		t.Fatal(err)
	}
	existing.Spec = team.Spec
	if err := fc.Update(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	if err := fc.Create(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	replaceTestCatalog(t, fc, catalogConfigMap("gpt-6-mini"))
	h.Config.AIGatewayEnabled = true
	h.Config.AIGatewayCatalogNamespace = "cetirun-system"
	failing = false
	w := httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "agent_type_not_allowed" {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	current, err := h.Kube.GetInstanceByName(context.Background(), original.Name)
	if err != nil {
		t.Fatal(err)
	}
	if h.Kube.RegistryAuthConfigured(context.Background(), current) {
		t.Fatal("retry wrote credentials after policy revoked harness")
	}
}

func createdPartialRegistryForPolicy(t *testing.T, h *InstanceHandler) *api.Agent {
	t.Helper()
	w := httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("partial create: %d", w.Code)
	}
	a, err := h.Kube.GetInstanceByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRegistryRetryValidatesPersistedSourcesRatherThanNewDefaults(t *testing.T) {
	failing := true
	h, fc := registryHandler(t, &failing)
	team, policy := creationPolicy()
	team.Name = "aks"
	policy.Spec.ModelAccess.Copilot.Allowed = true
	existingTeam, err := h.Kube.GetTeam(context.Background(), "aks")
	if err != nil {
		t.Fatal(err)
	}
	existingTeam.Spec = team.Spec
	if err := fc.Update(context.Background(), existingTeam); err != nil {
		t.Fatal(err)
	}
	if err := fc.Create(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	replaceTestCatalog(t, fc, catalogConfigMap("gpt-6-mini"))
	h.Config.AIGatewayEnabled = true
	h.Config.AIGatewayCatalogNamespace = "cetirun-system"
	configured, _ := creationPolicyHandler(team, policy)
	h.Config.SandboxRuntimes = configured.Config.SandboxRuntimes
	original := createdPartialRegistryForPolicy(t, h)
	if !original.Spec.GitHubCopilotEnabled() {
		t.Fatal("fixture must persist Copilot on")
	}
	currentPolicy, err := h.Kube.GetAgentPolicy(context.Background(), policy.Name)
	if err != nil {
		t.Fatal(err)
	}
	currentPolicy.Spec.ModelAccess.Copilot.Allowed = false
	if err := fc.Update(context.Background(), currentPolicy); err != nil {
		t.Fatal(err)
	}
	failing = false
	w := httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "invalid_model_access" {
		t.Fatalf("persisted-source retry: %d %s", w.Code, w.Body.String())
	}
	if h.Kube.RegistryAuthConfigured(context.Background(), original) {
		t.Fatal("retry wrote credentials for a now-forbidden persisted source")
	}
}
