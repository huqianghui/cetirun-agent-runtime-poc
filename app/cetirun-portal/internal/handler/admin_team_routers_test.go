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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func testModelRouterDTO(name string) TeamModelRouterDTO {
	return TeamModelRouterDTO{Name: name, Simple: "small-model", Medium: "medium-model", Complex: "copilot/large-model"}
}

func TestAdminTeamKeepsModelRoutersAndReportsTheirStatus(t *testing.T) {
	team := &cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "research"},
		Spec: cetirunv1alpha1.TeamSpec{AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard",
			ModelRouters: []cetirunv1alpha1.TeamModelRouter{{Name: "auto", Classifier: cetirunv1alpha1.TeamModelRouterClassifier{Type: "heuristic"},
				Tiers: cetirunv1alpha1.TeamModelRouterTiers{Simple: "small-model", Medium: "medium-model", Complex: "large-model"}}}},
		Status: cetirunv1alpha1.TeamStatus{ModelRouters: []cetirunv1alpha1.TeamModelRouterStatus{
			{Name: "auto", GatewayModel: "cetirun-router-0123456789abcdef", Ready: false, Message: "router auto: tier medium names medium-model, which is not a shared model of the AI Gateway"},
		}},
	}
	h := newAdminTeamHandler(team)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams/research", nil)
	req.SetPathValue("name", "research")
	w := httptest.NewRecorder()
	h.GetTeam(w, req)
	var got TeamDTO
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status %d: %v", w.Code, err)
	}
	if len(got.ModelRouters) != 1 || got.ModelRouters[0].Name != "auto" || got.ModelRouters[0].Classifier != "heuristic" ||
		got.ModelRouters[0].Complex != "large-model" || len(got.ModelRouterStatus) != 1 || got.ModelRouterStatus[0].Ready ||
		!strings.Contains(got.ModelRouterStatus[0].Message, "not a shared model") {
		t.Fatalf("unexpected DTO: %+v", got)
	}
	if strings.Contains(w.Body.String(), "cetirun-router-") {
		t.Fatal("the internal Gateway name stays out of the Portal")
	}

	// An edit that keeps the routers in the body keeps them; one that changes
	// them stores the change.
	current, _ := h.Kube.GetTeam(context.Background(), "research")
	routers := append(got.ModelRouters, TeamModelRouterDTO{Name: "auto-llm", Classifier: "llm", ClassifierModel: "small-model",
		Simple: "small-model", Medium: "medium-model", Complex: "large-model", Reasoning: "copilot/reasoning-model"},
		TeamModelRouterDTO{Name: "auto-decision", Classifier: "jev", ClassifierModel: " decision-model ",
			Simple: "small-model", Medium: "medium-model", Complex: "large-model"})
	body, _ := json.Marshal(TeamUpsertRequest{Role: "new", UID: string(current.UID), ResourceVersion: current.ResourceVersion,
		AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard", ModelRouters: routers})
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/teams/research", bytes.NewReader(body))
	req.SetPathValue("name", "research")
	w = httptest.NewRecorder()
	h.UpdateTeam(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}
	stored, _ := h.Kube.GetTeam(context.Background(), "research")
	if len(stored.Spec.ModelRouters) != 3 || stored.Spec.ModelRouters[1].Classifier.Type != "llm" ||
		stored.Spec.ModelRouters[1].Classifier.Model != "small-model" || stored.Spec.ModelRouters[1].Tiers.Reasoning != "copilot/reasoning-model" {
		t.Fatalf("stored routers = %+v", stored.Spec.ModelRouters)
	}
	// The decision classifier keeps its decision model, and reads back as it
	// was saved.
	if classifier := stored.Spec.ModelRouters[2].Classifier; classifier.Type != cetirunv1alpha1.ModelRouterClassifierJev || classifier.Model != "decision-model" {
		t.Fatalf("stored jev classifier = %+v", classifier)
	}
	if dto := toTeamDTO(stored).ModelRouters[2]; dto.Classifier != "jev" || dto.ClassifierModel != "decision-model" {
		t.Fatalf("jev router DTO = %+v", dto)
	}
}

// The model of a classifier is kept for the llm and jev classifiers only.
func TestTeamModelRoutersKeepTheClassifierModelOfModelClassifiers(t *testing.T) {
	for classifier, model := range map[string]string{"": "", "heuristic": "", "llm": "small-model", "jev": "decision-model"} {
		router := testModelRouterDTO("auto")
		router.Classifier, router.ClassifierModel = classifier, " "+model+" "
		if model == "" {
			router.ClassifierModel = "leftover-model"
		}
		spec := toTeamSpec(TeamUpsertRequest{ModelRouters: []TeamModelRouterDTO{router}}).ModelRouters[0].Classifier
		want := classifier
		if want == "" {
			want = cetirunv1alpha1.ModelRouterClassifierHeuristic
		}
		if spec.Type != want || spec.Model != model {
			t.Errorf("classifier %q: stored %+v, want type %q model %q", classifier, spec, want, model)
		}
	}
}

func TestAdminTeamRejectsInvalidModelRouters(t *testing.T) {
	llmWithoutModel := testModelRouterDTO("auto")
	llmWithoutModel.Classifier = "llm"
	copilotClassifier := testModelRouterDTO("auto")
	copilotClassifier.Classifier, copilotClassifier.ClassifierModel = "llm", "copilot/small-model"
	jevWithoutModel := testModelRouterDTO("auto")
	jevWithoutModel.Classifier = "jev"
	copilotDecision := testModelRouterDTO("auto")
	copilotDecision.Classifier, copilotDecision.ClassifierModel = "jev", "copilot/decision-model"
	spacedDecision := testModelRouterDTO("auto")
	spacedDecision.Classifier, spacedDecision.ClassifierModel = "jev", "decision model"
	missingTier := testModelRouterDTO("auto")
	missingTier.Medium = ""
	spacedTier := testModelRouterDTO("auto")
	spacedTier.Simple = "small model"
	licensed := testModelRouterDTO("auto")
	licensed.Classifier = "heuristic_v2"
	tooMany := make([]TeamModelRouterDTO, 0, 11)
	for i := 0; i < 11; i++ {
		tooMany = append(tooMany, testModelRouterDTO("auto-"+string(rune('a'+i))))
	}
	for _, tc := range []struct {
		routers []TeamModelRouterDTO
		want    string
	}{
		{[]TeamModelRouterDTO{testModelRouterDTO("cetirun-auto")}, "reserved"},
		{[]TeamModelRouterDTO{testModelRouterDTO("copilot/auto")}, "without spaces or slashes"},
		{[]TeamModelRouterDTO{testModelRouterDTO("auto"), testModelRouterDTO("auto")}, "defined twice"},
		{[]TeamModelRouterDTO{llmWithoutModel}, "needs a shared model"},
		{[]TeamModelRouterDTO{copilotClassifier}, "needs a shared model"},
		{[]TeamModelRouterDTO{jevWithoutModel}, "the decision classifier needs a shared decision model"},
		{[]TeamModelRouterDTO{copilotDecision}, "the decision classifier needs a shared decision model"},
		{[]TeamModelRouterDTO{spacedDecision}, "the decision classifier needs a shared decision model"},
		{[]TeamModelRouterDTO{missingTier}, "the medium tier"},
		{[]TeamModelRouterDTO{spacedTier}, "the simple tier"},
		{[]TeamModelRouterDTO{licensed}, "the classifier must be heuristic, llm or jev"},
		{tooMany, "at most 10"},
	} {
		h := newAdminTeamHandler()
		body, _ := json.Marshal(TeamUpsertRequest{Name: "research", AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard"},
			DefaultSandboxRef: "standard", ModelRouters: tc.routers})
		w := httptest.NewRecorder()
		h.CreateTeam(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body)))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("routers %+v: status %d body %s, want %q", tc.routers, w.Code, w.Body.String(), tc.want)
		}
	}
	// A jev router with a decision model is accepted; whether the model is
	// one is the operator's to check.
	jev := testModelRouterDTO("auto")
	jev.Classifier, jev.ClassifierModel = "jev", "decision-model"
	if message := validateTeamModelRouters([]TeamModelRouterDTO{jev}); message != "" {
		t.Fatalf("a jev router with a model is refused: %s", message)
	}
}

func TestTeamValidationMessageNamesModelRouterFields(t *testing.T) {
	if !teamValidationFieldPattern.MatchString("spec.modelRouters[2].tiers.medium") ||
		!teamValidationFieldPattern.MatchString("spec.modelRouters[0].classifier.model") ||
		teamValidationFieldPattern.MatchString("spec.modelRouters[0].tiers.other") {
		t.Fatal("the model router field paths must be reported, and only those")
	}
}

func TestTeamModelRouterNamesFollowThePolicy(t *testing.T) {
	team := &cetirunv1alpha1.Team{Spec: cetirunv1alpha1.TeamSpec{ModelRouters: []cetirunv1alpha1.TeamModelRouter{{Name: "auto"}, {Name: "auto-claude"}}}}
	if got := teamModelRouterNames(team, nil); !slices.Equal(got, []string{"auto", "auto-claude"}) {
		t.Fatalf("without a policy: %v", got)
	}
	policy := &cetirunv1alpha1.AgentPolicy{Spec: cetirunv1alpha1.AgentPolicySpec{ModelAccess: cetirunv1alpha1.AgentPolicyModelAccess{
		SharedModels: cetirunv1alpha1.AgentPolicySharedModelsAccess{Allowed: []string{"small-model", "auto-claude"}}}}}
	if got := teamModelRouterNames(team, policy); !slices.Equal(got, []string{"auto-claude"}) {
		t.Fatalf("with a policy: %v", got)
	}
	if got := teamModelRouterNames(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("without a Team: %v", got)
	}
}

func TestAIGatewayResponseNamesRoutersByTheirOwnNames(t *testing.T) {
	agent := &cetirunv1alpha1.Agent{Status: cetirunv1alpha1.AgentStatus{AIGateway: &cetirunv1alpha1.AIGatewayStatus{
		Routed: true, Models: []string{"small-model", "cetirun-router-0123456789abcdef"}, DefaultModel: "cetirun-router-0123456789abcdef",
		ModelRouters: []cetirunv1alpha1.AIGatewayModelRouterStatus{
			{Name: "auto", Model: "cetirun-router-0123456789abcdef", Granted: true},
			{Name: "auto-copilot", Reason: "tier simple uses the GitHub Copilot model fast-model: sign in to GitHub Copilot with an account that offers it"},
		},
	}}}
	resp := aiGatewayResponse(agent)
	if !slices.Equal(resp.Models, []string{"small-model", "auto"}) || resp.DefaultModel != "auto" || len(resp.ModelRouters) != 2 ||
		!resp.ModelRouters[0].Granted || resp.ModelRouters[1].Granted || !strings.Contains(resp.ModelRouters[1].Reason, "sign in") {
		t.Fatalf("response = %+v", resp)
	}
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "cetirun-router-") {
		t.Fatalf("the internal Gateway name stays out of the response: %s", raw)
	}
	if agent.Status.AIGateway.Models[1] != "cetirun-router-0123456789abcdef" {
		t.Fatal("the Agent's status must not be changed")
	}
}

// A router configured in the Gateway is granted under its own name, which is
// also the model the Agent sends; a refused one keeps its reason.
func TestAIGatewayResponseReportsTheGatewaysRouters(t *testing.T) {
	agent := &cetirunv1alpha1.Agent{Status: cetirunv1alpha1.AgentStatus{AIGateway: &cetirunv1alpha1.AIGatewayStatus{
		Routed: true, Models: []string{"auto-shared", "small-model"}, DefaultModel: "auto-shared",
		ModelRouters: []cetirunv1alpha1.AIGatewayModelRouterStatus{
			{Name: "auto-shared", Model: "auto-shared", Granted: true},
			{Name: "auto-wide", Reason: "tier medium uses admin-wide, which the AgentPolicy does not allow"},
		},
	}}}
	resp := aiGatewayResponse(agent)
	if !slices.Equal(resp.Models, []string{"auto-shared", "small-model"}) || resp.DefaultModel != "auto-shared" || len(resp.ModelRouters) != 2 ||
		!resp.ModelRouters[0].Granted || resp.ModelRouters[1].Granted || !strings.Contains(resp.ModelRouters[1].Reason, "does not allow") {
		t.Fatalf("response = %+v", resp)
	}
}

func TestCreationOptionsOfferThePolicysModelRouters(t *testing.T) {
	team, policy := creationPolicy()
	team.Spec.ModelRouters = []cetirunv1alpha1.TeamModelRouter{{Name: "auto"}, {Name: "auto-unlisted"}}
	policy.Spec.ModelAccess.SharedModels.Allowed = append(policy.Spec.ModelAccess.SharedModels.Allowed, "auto")
	h, fc := creationPolicyHandler(team, policy)
	options := readCreateOptions(t, h, team.Name)
	if !slices.Equal(options.ModelRouters, []string{"auto"}) {
		t.Fatalf("model routers = %v", options.ModelRouters)
	}
	// A router the policy does not allow is refused as a default model before
	// any write; an allowed one passes the model check.
	w := httptest.NewRecorder()
	h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team","model":"auto-unlisted","gatewayToken":"synthetic-token"}`))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "model_not_allowed" {
		t.Fatalf("unlisted router: %d %s", w.Code, w.Body.String())
	}
	assertNoCreationWrites(t, fc)
	w = httptest.NewRecorder()
	h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team","model":"auto","gatewayToken":"synthetic-token"}`))
	if w.Code == http.StatusForbidden && errorCode(t, w) == "model_not_allowed" {
		t.Fatalf("an allowed router must pass the model check: %s", w.Body.String())
	}
}

// A router is offered only with every model it sends requests to: a Team's
// router by its tiers, a router configured in the Gateway by the models the
// operator publishes for it, as the operator grants them.
func TestCreationOptionsOfferARouterOnlyWithEveryModelItUses(t *testing.T) {
	tiers := func(simple, medium string) cetirunv1alpha1.TeamModelRouterTiers {
		return cetirunv1alpha1.TeamModelRouterTiers{Simple: simple, Medium: medium, Complex: simple}
	}
	policy := &cetirunv1alpha1.AgentPolicy{Spec: cetirunv1alpha1.AgentPolicySpec{ModelAccess: cetirunv1alpha1.AgentPolicyModelAccess{
		SharedModels: cetirunv1alpha1.AgentPolicySharedModelsAccess{Allowed: []string{"small-model", "auto", "auto-wide", "auto-copilot"}}}}}
	team := &cetirunv1alpha1.Team{Spec: cetirunv1alpha1.TeamSpec{ModelRouters: []cetirunv1alpha1.TeamModelRouter{
		{Name: "auto", Tiers: tiers("small-model", "small-model")},
		{Name: "auto-wide", Tiers: tiers("small-model", "wide-model")},
		{Name: "auto-copilot", Tiers: tiers("small-model", "copilot/fast-model")},
	}}}
	if got := teamModelRouterNames(team, policy); !slices.Equal(got, []string{"auto"}) {
		t.Fatalf("without GitHub Copilot: %v", got)
	}
	policy.Spec.ModelAccess.Copilot.Allowed = true
	if got := teamModelRouterNames(team, policy); !slices.Equal(got, []string{"auto", "auto-copilot"}) {
		t.Fatalf("with GitHub Copilot: %v", got)
	}

	allowed := []string{"small-model", "gateway-auto", "gateway-wide", "gateway-unverifiable"}
	policy.Spec.ModelAccess.SharedModels.Allowed = allowed
	for model, want := range map[string]bool{
		"small-model": true, "gateway-auto": true, "gateway-wide": false, "gateway-unverifiable": false, "unlisted": false,
	} {
		catalogModel := cetirunv1alpha1.AIGatewayCatalogModel{Name: model}
		switch model {
		case "gateway-auto":
			catalogModel.RouterModels = []string{"small-model"}
		case "gateway-wide":
			catalogModel.RouterModels = []string{"small-model", "wide-model"}
		case "gateway-unverifiable":
			catalogModel.RouterModels, catalogModel.RouterUnverifiable = []string{"small-model"}, true
		}
		if got := sharedModelAllowed(catalogModel, policy); got != want {
			t.Errorf("%s: allowed = %v, want %v", model, got, want)
		}
	}
}

// A Copilot tier that the operator registered as a shared model needs that
// shared model in the policy, as the operator grants it (review).
func TestCreationOptionsFollowTheRegisteredModelsOfACopilotTier(t *testing.T) {
	router := cetirunv1alpha1.TeamModelRouter{Name: "auto-copilot",
		Tiers: cetirunv1alpha1.TeamModelRouterTiers{Simple: "copilot/gpt-5.5", Medium: "small-model", Complex: "small-model"}}
	team := &cetirunv1alpha1.Team{Spec: cetirunv1alpha1.TeamSpec{ModelRouters: []cetirunv1alpha1.TeamModelRouter{router}}}
	team.Status.ModelRouters = []cetirunv1alpha1.TeamModelRouterStatus{{
		Name: "auto-copilot", Ready: true, SharedModels: []string{"small-model", "team-gpt"},
	}}
	policy := &cetirunv1alpha1.AgentPolicy{Spec: cetirunv1alpha1.AgentPolicySpec{ModelAccess: cetirunv1alpha1.AgentPolicyModelAccess{
		Copilot:      cetirunv1alpha1.AgentPolicyCopilotAccess{Allowed: true},
		SharedModels: cetirunv1alpha1.AgentPolicySharedModelsAccess{Allowed: []string{"small-model", "auto-copilot"}}}}}
	if got := teamModelRouterNames(team, policy); len(got) != 0 {
		t.Fatalf("Copilot allowed but not the shared model the tier is registered as: %v", got)
	}
	policy.Spec.ModelAccess.SharedModels.Allowed = append(policy.Spec.ModelAccess.SharedModels.Allowed, "team-gpt")
	policy.Spec.ModelAccess.Copilot.Allowed = false
	if got := teamModelRouterNames(team, policy); !slices.Equal(got, []string{"auto-copilot"}) {
		t.Fatalf("every registered model allowed, no Copilot tier under its ID: %v", got)
	}
	// A tier registered under the Copilot model's ID still needs Copilot.
	team.Status.ModelRouters[0].SharedModels = []string{"small-model"}
	team.Status.ModelRouters[0].CopilotModels = []string{"copilot/gpt-5.5"}
	if got := teamModelRouterNames(team, policy); len(got) != 0 {
		t.Fatalf("Copilot tier under its ID without Copilot: %v", got)
	}
}
