// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

func gatewayConfig() config.Config {
	return config.Config{AIGatewayEnabled: true, AIGatewayCatalogNamespace: "cetirun-system"}
}

func gatewayTeam(name string) *cetirunv1alpha1.Team {
	return &cetirunv1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// catalogConfigMap publishes the shared models of a deployment that offers
// GitHub Copilot.
func catalogConfigMap(models ...string) *corev1.ConfigMap {
	return publishedCatalog(true, models...)
}

func publishedCatalog(copilot bool, models ...string) *corev1.ConfigMap {
	return routingCatalog(&cetirunv1alpha1.AIGatewayRouting{Chart: testAgentChart,
		AgentTypes: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeHermes, cetirunv1alpha1.AgentTypeCustom}}, copilot, models...)
}

const testAgentChart = "oci://registry.example.com/helm/cetirun:1.0.0"

// routingCatalog publishes a catalog with the given routing: nil is an
// earlier operator's catalog, which has none; null agent types are a chart
// the operator has not read yet.
func routingCatalog(routing *cetirunv1alpha1.AIGatewayRouting, copilot bool, models ...string) *corev1.ConfigMap {
	catalog := cetirunv1alpha1.AIGatewayCatalog{SharedModels: []cetirunv1alpha1.AIGatewayCatalogModel{}, GitHubCopilot: copilot, Routing: routing}
	for _, name := range models {
		catalog.SharedModels = append(catalog.SharedModels, cetirunv1alpha1.AIGatewayCatalogModel{Name: name, ContextWindow: 200000})
	}
	raw, _ := json.Marshal(catalog)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cetirunv1alpha1.AIGatewayCatalogConfigMap, Namespace: "cetirun-system"},
		Data:       map[string]string{cetirunv1alpha1.AIGatewayCatalogKey: string(raw)},
	}
}

func createAgent(t *testing.T, cfg config.Config, body string, objs ...client.Object) (*httptest.ResponseRecorder, client.Client) {
	t.Helper()
	fc := newFakeClient(objs...)
	h := &InstanceHandler{Kube: kube.NewClient(fc), Config: cfg}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instances", strings.NewReader(body))
	req = withClaims(req, "alice", "Alice", "alice@example.com")
	w := httptest.NewRecorder()
	h.CreateInstance(w, req)
	return w, fc
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return resp.Error.Code
}

func TestCreateInstanceAIGatewaySupportsOptionalTeamAndCustom(t *testing.T) {
	w, fc := createAgent(t, gatewayConfig(), `{"agent":{"type":"openclaw"}}`, catalogConfigMap("gpt-6"))
	if w.Code != http.StatusCreated {
		t.Fatalf("no team: status %d body %s", w.Code, w.Body.String())
	}
	created := &cetirunv1alpha1.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, created); err != nil {
		t.Fatal(err)
	}
	if created.Spec.Team != "" || created.Spec.SandboxRef != "" || created.Spec.ModelAccess == nil {
		t.Fatalf("unbound gateway agent = %+v", created.Spec)
	}
	w, _ = createAgent(t, gatewayConfig(), `{"agent":{"type":"openclaw"},"team":"nope"}`, gatewayTeam("aks"), catalogConfigMap("gpt-6"))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "unknown_team" {
		t.Fatalf("unknown team: status %d body %s", w.Code, w.Body.String())
	}
	// Gateway-only creation rejects deployments where it is disabled.
	w, _ = createAgent(t, config.Config{}, `{"agent":{"type":"openclaw"}}`, gatewayTeam("aks"))
	if w.Code != http.StatusConflict || errorCode(t, w) != "gateway_required" {
		t.Fatalf("feature off: status %d body %s", w.Code, w.Body.String())
	}
	// Custom receives the same Gateway model source selection without a Team.
	custom := gatewayConfig()
	custom.AllowedAgentTypes = []string{"openclaw", "custom"}
	customAgent := `"agent":{"type":"custom","image":"` + managementImage + `","username":"assistant"}`
	w, fc = createAgent(t, custom, `{`+customAgent+`,"modelAccess":{"githubCopilot":false}}`, catalogConfigMap("gpt-6"))
	if w.Code != http.StatusCreated {
		t.Fatalf("custom: status %d body %s", w.Code, w.Body.String())
	}
	agent := &cetirunv1alpha1.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.Team != "" || agent.Spec.ModelAccess == nil || *agent.Spec.ModelAccess.GitHubCopilot {
		t.Fatalf("custom: team %q modelAccess %+v, want no team and Copilot off", agent.Spec.Team, agent.Spec.ModelAccess)
	}
}

func TestCreateInstanceAIGatewayModelAccessRules(t *testing.T) {
	const openclaw = `{"agent":{"type":"openclaw"},"team":"aks"`
	const hermes = `{"agent":{"type":"hermes"},"team":"aks"}`
	routes := func(agentTypes ...cetirunv1alpha1.AgentType) *cetirunv1alpha1.AIGatewayRouting {
		return &cetirunv1alpha1.AIGatewayRouting{Chart: testAgentChart, AgentTypes: append([]cetirunv1alpha1.AgentType{}, agentTypes...)}
	}
	unread := &cetirunv1alpha1.AIGatewayRouting{Chart: testAgentChart}
	cases := []struct {
		name        string
		body        string
		catalog     *corev1.ConfigMap
		wantStatus  int
		wantAccess  *[2]bool
		wantErrCode string
		wantErrText string
	}{
		{name: "defaults", body: `{"agent":{"type":"openclaw"},"team":" aks "}`, catalog: catalogConfigMap("gpt-6"), wantStatus: http.StatusCreated, wantAccess: &[2]bool{true, true}},
		{name: "both on", body: openclaw + `,"modelAccess":{"sharedModels":true,"githubCopilot":true}}`,
			catalog: catalogConfigMap("gpt-6"), wantStatus: http.StatusCreated, wantAccess: &[2]bool{true, true}},
		{name: "both off", body: openclaw + `,"modelAccess":{"sharedModels":false,"githubCopilot":false}}`,
			catalog: catalogConfigMap("gpt-6"), wantStatus: http.StatusBadRequest, wantErrCode: "invalid_model_access"},
		{name: "shared off", body: openclaw + `,"modelAccess":{"sharedModels":false}}`,
			catalog: catalogConfigMap(), wantStatus: http.StatusCreated, wantAccess: &[2]bool{false, true}},
		{name: "copilot off without shared models", body: openclaw + `,"modelAccess":{"githubCopilot":false}}`,
			catalog: catalogConfigMap(), wantStatus: http.StatusBadRequest, wantErrCode: "invalid_model_access"},
		{name: "copilot off with shared models", body: openclaw + `,"modelAccess":{"githubCopilot":false}}`,
			catalog: catalogConfigMap("gpt-6"), wantStatus: http.StatusCreated, wantAccess: &[2]bool{true, false}},
		// The catalog says which sources exist; until it is published the
		// Portal cannot tell whether the Agent would have a model.
		{name: "unpublished catalog", body: openclaw + `}`, wantStatus: http.StatusServiceUnavailable, wantErrCode: "model_sources_unknown"},
		{name: "copilot off with an unpublished catalog", body: openclaw + `,"modelAccess":{"githubCopilot":false}}`,
			wantStatus: http.StatusServiceUnavailable, wantErrCode: "model_sources_unknown"},
		// A deployment without the Copilot proxy offers only shared models.
		{name: "no copilot, shared models", body: openclaw + `}`, catalog: publishedCatalog(false, "gpt-6"), wantStatus: http.StatusCreated, wantAccess: &[2]bool{true, false}},
		{name: "no copilot, shared off", body: openclaw + `,"modelAccess":{"sharedModels":false}}`,
			catalog: publishedCatalog(false, "gpt-6"), wantStatus: http.StatusBadRequest, wantErrCode: "invalid_model_access"},
		{name: "no copilot, no shared models", body: openclaw + `}`, catalog: publishedCatalog(false),
			wantStatus: http.StatusConflict, wantErrCode: "no_model_source"},
		{name: "no copilot, no shared models, copilot asked for", body: openclaw + `,"modelAccess":{"sharedModels":false,"githubCopilot":true}}`,
			catalog: publishedCatalog(false), wantStatus: http.StatusConflict, wantErrCode: "no_model_source"},
		// Hermes takes its models from the Gateway too, under the same rules.
		{name: "hermes copilot off with shared models", body: `{"agent":{"type":"hermes"},"team":"aks","modelAccess":{"githubCopilot":false}}`,
			catalog: catalogConfigMap("gpt-6"), wantStatus: http.StatusCreated, wantAccess: &[2]bool{true, false}},
		{name: "hermes copilot off without shared models", body: `{"agent":{"type":"hermes"},"team":"aks","modelAccess":{"githubCopilot":false}}`,
			catalog: catalogConfigMap(), wantStatus: http.StatusBadRequest, wantErrCode: "invalid_model_access"},
		{name: "hermes with an unpublished catalog", body: `{"agent":{"type":"hermes"},"team":"aks"}`,
			wantStatus: http.StatusServiceUnavailable, wantErrCode: "model_sources_unknown"},
		// Only the agent types the operator's default agent chart routes
		// through the Gateway take its models: the operator would leave
		// another one off the Gateway, with the Gateway's model names on the
		// agent's own provider.
		{name: "hermes on a chart that routes only OpenClaw", body: hermes, catalog: routingCatalog(routes(cetirunv1alpha1.AgentTypeOpenClaw), true, "gpt-6"),
			wantStatus: http.StatusConflict, wantErrCode: "agent_chart_unsupported", wantErrText: "does not route Hermes agents"},
		{name: "hermes with missing routing", body: hermes, catalog: routingCatalog(nil, true, "gpt-6"),
			wantStatus: http.StatusServiceUnavailable, wantErrCode: "agent_types_unknown"},
		{name: "openclaw with missing routing", body: openclaw + `}`, catalog: routingCatalog(nil, true, "gpt-6"),
			wantStatus: http.StatusServiceUnavailable, wantErrCode: "agent_types_unknown"},
		{name: "openclaw on a chart older than the Gateway", body: openclaw + `}`, catalog: routingCatalog(routes(), true, "gpt-6"),
			wantStatus: http.StatusConflict, wantErrCode: "agent_chart_unsupported", wantErrText: "upgrade the agent chart"},
		// Until the operator has read its default chart, which a new Agent
		// is pinned to, no Agent is created on the Gateway.
		{name: "hermes before the operator has read its chart", body: hermes, catalog: routingCatalog(unread, true, "gpt-6"),
			wantStatus: http.StatusServiceUnavailable, wantErrCode: "agent_types_unknown", wantErrText: "Try again in a minute"},
		{name: "openclaw before the operator has read its chart", body: openclaw + `}`, catalog: routingCatalog(unread, true, "gpt-6"),
			wantStatus: http.StatusServiceUnavailable, wantErrCode: "agent_types_unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{gatewayTeam("aks")}
			if tc.catalog != nil {
				objs = append(objs, tc.catalog)
			}
			w, fc := createAgent(t, gatewayConfig(), tc.body, objs...)
			if w.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantErrCode != "" {
				if code := errorCode(t, w); code != tc.wantErrCode {
					t.Fatalf("error code %q, want %q", code, tc.wantErrCode)
				}
				if !strings.Contains(w.Body.String(), tc.wantErrText) {
					t.Fatalf("error %s, want it to say %q", w.Body.String(), tc.wantErrText)
				}
				return
			}
			agent := &cetirunv1alpha1.Agent{}
			if err := fc.Get(context.Background(), client.ObjectKey{Name: "alice"}, agent); err != nil {
				t.Fatal(err)
			}
			if agent.Spec.Team != "aks" || agent.Spec.SandboxRef != "" {
				t.Fatalf("binding = %q/%q, want aks with no Sandbox binding", agent.Spec.Team, agent.Spec.SandboxRef)
			}
			switch {
			case tc.wantAccess == nil && agent.Spec.ModelAccess != nil:
				t.Fatalf("modelAccess = %+v, want unset", agent.Spec.ModelAccess)
			case tc.wantAccess != nil:
				access := agent.Spec.ModelAccess
				if access == nil || access.SharedModels == nil || access.GitHubCopilot == nil ||
					*access.SharedModels != tc.wantAccess[0] || *access.GitHubCopilot != tc.wantAccess[1] {
					t.Fatalf("modelAccess = %+v, want %v", access, *tc.wantAccess)
				}
			}
		})
	}
}

func routedAgent() *cetirunv1alpha1.Agent {
	agent := sampleInstance()
	agent.Spec.ModelAccess = &cetirunv1alpha1.ModelAccessSpec{SharedModels: ptr.To(true), GitHubCopilot: ptr.To(true)}
	agent.Name = "alice-gw"
	agent.UID = "uid-1"
	agent.Spec.GatewayTokenSecretRef = "gateway-token"
	agent.Status.Namespace = "cetirun-alice-gw"
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{
		Phase: "Ready", Routed: true, KeyID: "abc", KeySecretName: "ai-gateway-virtual-key",
		Models:       []string{"gpt-6", cetirunv1alpha1.AIGatewaySignInRequiredModel},
		DefaultModel: "gpt-6",
		Copilot:      &cetirunv1alpha1.AIGatewayCopilotStatus{Phase: "SignInRequired", AccessGroup: "copilot-uid-1"},
	}
	return agent
}

func gatewayTokenSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-token", Namespace: namespace},
		Data:       map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte("tok"), "CETIRUN_AUTH_KEY": []byte("helper-key")},
	}
}

func TestInstanceResponseDescribesAIGateway(t *testing.T) {
	routed := routedAgent()
	fc := newFakeClient(routed, gatewayTokenSecret("cetirun-alice-gw"))
	h := &InstanceHandler{Kube: kube.NewClient(fc), Config: gatewayConfig()}
	resp, err := h.toInstanceResponse(context.Background(), routed)
	if err != nil {
		t.Fatal(err)
	}
	gw := resp.AIGateway
	if gw == nil || !gw.Routed || !gw.SharedModels || !gw.GitHubCopilot {
		t.Fatalf("aiGateway = %+v", gw)
	}
	if strings.Join(gw.Models, ",") != "gpt-6" || gw.DefaultModel != "gpt-6" {
		t.Fatalf("models = %v default %q; placeholders must be hidden", gw.Models, gw.DefaultModel)
	}
	if gw.Copilot == nil || gw.Copilot.Phase != "SignInRequired" || gw.Copilot.Models == nil {
		t.Fatalf("copilot = %+v", gw.Copilot)
	}

	// A placeholder default is not reported as a model.
	onlyPlaceholder := routedAgent()
	onlyPlaceholder.Status.AIGateway.Models = []string{cetirunv1alpha1.AIGatewayNoModelsModel}
	onlyPlaceholder.Status.AIGateway.DefaultModel = cetirunv1alpha1.AIGatewayNoModelsModel
	if gw := aiGatewayResponse(onlyPlaceholder); len(gw.Models) != 0 || gw.DefaultModel != "" {
		t.Fatalf("placeholder-only response = %+v", gw)
	}

	// Copilot models that are a shared model too are reported as such, once;
	// those whose name a shared model already has are reported as skipped,
	// and those the policy does not allow as not allowed.
	merged := routedAgent()
	merged.Status.AIGateway.Models = []string{"gpt-6", "o-mini"}
	merged.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{
		Phase: cetirunv1alpha1.AIGatewayCopilotPhaseReady, Models: []string{"gpt-6", "o-mini"}, Skipped: []string{"gpt-5"},
		NotAllowed: []string{"gpt-6-astra"},
		Routes: []cetirunv1alpha1.AIGatewayCopilotRoute{
			{Model: "gpt-6", Name: "gpt-6", Shared: true}, {Model: "gpt-6-preview", Name: "gpt-6", Shared: true}, {Model: "o-mini", Name: "o-mini"},
		},
	}
	if gw := aiGatewayResponse(merged); strings.Join(gw.Models, ",") != "gpt-6,o-mini" || !slices.Equal(gw.Copilot.Shared, []string{"gpt-6"}) ||
		!slices.Equal(gw.Copilot.Skipped, []string{"gpt-5"}) || !slices.Equal(gw.Copilot.NotAllowed, []string{"gpt-6-astra"}) {
		t.Fatalf("merged response = %+v copilot %+v", gw, gw.Copilot)
	}

	unrouted := sampleInstance()
	unrouted.Spec.GatewayTokenSecretRef = "gateway-token"
	fc = newFakeClient(unrouted, gatewayTokenSecret("cetirun-alice"))
	h = &InstanceHandler{Kube: kube.NewClient(fc), Config: gatewayConfig()}
	resp, err = h.toInstanceResponse(context.Background(), unrouted)
	if err != nil {
		t.Fatal(err)
	}
	if resp.AIGateway != nil {
		t.Fatalf("unrouted: aiGateway %+v", resp.AIGateway)
	}
}

func TestGetAIGatewayCatalog(t *testing.T) {
	get := func(cfg config.Config, objs ...client.Object) *httptest.ResponseRecorder {
		h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(objs...)), Config: cfg}
		req := withClaims(httptest.NewRequest(http.MethodGet, "/api/v1/ai-gateway/catalog", nil), "alice", "Alice", "alice@example.com")
		w := httptest.NewRecorder()
		h.GetAIGatewayCatalog(w, req)
		return w
	}
	if w := get(config.Config{}); w.Code != http.StatusNotFound {
		t.Fatalf("feature off: status %d", w.Code)
	}
	var resp AIGatewayCatalogResponse
	w := get(gatewayConfig())
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || w.Code != http.StatusOK || resp.Known || resp.SharedModels == nil {
		t.Fatalf("unpublished: status %d resp %+v err %v", w.Code, resp, err)
	}
	w = get(gatewayConfig(), catalogConfigMap("gpt-6", "gpt-6-mini"))
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil || !resp.Known || len(resp.SharedModels) != 2 ||
		resp.SharedModels[0].Name != "gpt-6" || resp.SharedModels[0].ContextWindow != 200000 || !resp.GitHubCopilot {
		t.Fatalf("published: resp %+v err %v", resp, err)
	}
	if strings.Contains(w.Body.String(), "decisionModels") {
		t.Fatalf("a catalog without decision models lists none: %s", w.Body.String())
	}
	resp = AIGatewayCatalogResponse{}
	w = get(gatewayConfig(), publishedCatalog(false, "gpt-6"))
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil || !resp.Known || resp.GitHubCopilot {
		t.Fatalf("published without GitHub Copilot: resp %+v err %v", resp, err)
	}
	if !slices.Equal(resp.AgentTypes, []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeHermes, cetirunv1alpha1.AgentTypeCustom}) {
		t.Fatalf("the published agent types must reach the form, got %v", resp.AgentTypes)
	}
	// Missing routing is unknown and must not imply support for a harness.
	resp = AIGatewayCatalogResponse{}
	w = get(gatewayConfig(), routingCatalog(nil, true, "gpt-6"))
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil ||
		resp.AgentTypes != nil {
		t.Fatalf("missing routing: resp %+v err %v", resp, err)
	}
	// Until the operator has read its default chart, the agent types are
	// null, which the create form reads as unknown.
	w = get(gatewayConfig(), routingCatalog(&cetirunv1alpha1.AIGatewayRouting{Chart: testAgentChart}, true, "gpt-6"))
	if !strings.Contains(w.Body.String(), `"agentTypes":null`) {
		t.Fatalf("unread chart: body %s, want null agent types", w.Body.String())
	}

	// The decision models a Team router's jev classifier may use pass
	// through apart from the shared models.
	withDecision := publishedCatalog(true, "gpt-6")
	var published cetirunv1alpha1.AIGatewayCatalog
	if err := json.Unmarshal([]byte(withDecision.Data[cetirunv1alpha1.AIGatewayCatalogKey]), &published); err != nil {
		t.Fatal(err)
	}
	published.DecisionModels = []string{"decision-1", "decision-2"}
	raw, _ := json.Marshal(published)
	withDecision.Data[cetirunv1alpha1.AIGatewayCatalogKey] = string(raw)
	resp = AIGatewayCatalogResponse{}
	w = get(gatewayConfig(), withDecision)
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil || !resp.Known ||
		!slices.Equal(resp.DecisionModels, []string{"decision-1", "decision-2"}) || len(resp.SharedModels) != 1 || resp.SharedModels[0].Name != "gpt-6" {
		t.Fatalf("decision models: resp %+v err %v", resp, err)
	}
	// No Agent is created with a decision model: the create options leave
	// them out.
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(gatewayTeam("research"), withDecision)), Config: gatewayConfig()}
	if options := readCreateOptions(t, h, "research"); options.Gateway.DecisionModels != nil || len(options.Gateway.SharedModels) != 1 {
		t.Fatalf("create options: %+v", options.Gateway)
	}
}
