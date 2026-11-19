// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
)

// visionDeployment is one deployment of a shared model of the Gateway
// chart's models, entry telling deployments of one name apart.
func visionDeployment(name string, entry int, vision bool) aigateway.Deployment {
	return aigateway.Deployment{ID: managedTestDeploymentID(name, entry), ModelName: name, AccessGroups: []string{gatewayRouterGroup(name)},
		DatabaseSourceKnown: true, Priced: true, MaxInputTokens: 400000, MaxOutputTokens: 16384, Vision: vision}
}

// entryVision reports, by ID, whether the Agent is configured to send images
// to each of its models.
func entryVision(t *testing.T, r *AgentReconciler, agent *api.Agent) map[string]bool {
	t.Helper()
	entries, _, _ := r.aiGatewayModelEntries(agent)
	vision := map[string]bool{}
	for _, entry := range entries {
		vision[entry.ID] = entry.Vision
	}
	return vision
}

// A name accepts images only when every deployment behind it does, and a
// router only when every model it sends requests to answer does. The router's
// own model info does not count.
func TestAIGatewayCatalogMarksTheModelsThatAcceptImages(t *testing.T) {
	classifier := aigateway.RouterModel{Use: aigateway.RouterClassifier, Name: "blind"}
	unverifiable := gatewayRouter("auto-unverifiable", tierModel("SIMPLE", "sees"))
	unverifiable.Router.Unverifiable = "it runs routing plugins, code the operator cannot check"
	claims := gatewayRouter("auto-claims", tierModel("SIMPLE", "blind"))
	claims.Vision = true
	listing := []aigateway.Deployment{
		visionDeployment("sees", 1, true), visionDeployment("sees", 2, true),
		visionDeployment("mixed", 1, true), visionDeployment("mixed", 2, false),
		visionDeployment("blind", 1, false),
		// A classifier only reads the request's text.
		gatewayRouter("auto-sees", tierModel("SIMPLE", "sees"), tierModel("MEDIUM", "sees"), classifier),
		gatewayRouter("auto-mixed-tier", tierModel("SIMPLE", "sees"), tierModel("MEDIUM", "mixed")),
		gatewayRouter("auto-blind-default", tierModel("SIMPLE", "sees"), aigateway.RouterModel{Use: aigateway.RouterDefault, Name: "blind"}),
		gatewayRouter("auto-blind-compactor", tierModel("SIMPLE", "sees"), aigateway.RouterModel{Use: aigateway.RouterCompactor, Name: "blind"}),
		gatewayRouter("auto-nested", tierModel("SIMPLE", "auto-sees")),
		gatewayRouter("auto-missing", tierModel("SIMPLE", "sees"), tierModel("MEDIUM", "not-in-the-gateway")),
		gatewayRouter("auto-classifier-only", classifier),
		unverifiable, claims,
	}
	want := map[string]bool{
		"sees": true, "mixed": false, "blind": false,
		"auto-sees": true, "auto-mixed-tier": false, "auto-blind-default": false, "auto-blind-compactor": false,
		"auto-nested": false, "auto-missing": false, "auto-classifier-only": false, "auto-unverifiable": false,
		"auto-claims": false,
	}
	for _, order := range []string{"listed", "reversed"} {
		ordered := slices.Clone(listing)
		if order == "reversed" {
			slices.Reverse(ordered)
		}
		var catalog aiGatewayCatalog
		catalog.update(ordered, false, time.Now())
		shared, _ := catalog.shared()
		got := map[string]bool{}
		for _, model := range shared {
			got[model.Name] = model.Vision
		}
		for name, vision := range want {
			if accepts, listed := got[name]; !listed || accepts != vision {
				t.Errorf("%s: %s vision = %v (listed %v), want %v", order, name, accepts, listed, vision)
			}
		}
		// The published catalog carries it, and a catalog seeded from that
		// keeps it.
		var seeded aiGatewayCatalog
		seeded.seed(catalog.sharedRouters().describe(shared), nil)
		seededShared, _ := seeded.shared()
		if len(seededShared) != len(want) {
			t.Fatalf("%s: seeded %+v", order, seededShared)
		}
		for _, model := range seededShared {
			if model.Vision != want[model.Name] {
				t.Errorf("%s: seeded %s vision = %v", order, model.Name, model.Vision)
			}
		}
	}
}

// A model that starts or stops accepting images changes the catalog, which
// reconfigures the Agents.
func TestAIGatewayCatalogSeesAModelStartAcceptingImages(t *testing.T) {
	var catalog aiGatewayCatalog
	catalog.update([]aigateway.Deployment{visionDeployment("sees", 1, false)}, false, time.Now())
	if changed, _, _ := catalog.update([]aigateway.Deployment{visionDeployment("sees", 1, false)}, false, time.Now()); changed {
		t.Fatal("the same listing changes nothing")
	}
	if !catalog.changedBy([]aigateway.Deployment{visionDeployment("sees", 1, true)}, false) {
		t.Fatal("a model that starts accepting images must change the catalog")
	}
	if changed, _, _ := catalog.update([]aigateway.Deployment{visionDeployment("sees", 1, true)}, false, time.Now()); !changed {
		t.Fatal("a model that starts accepting images must change the catalog")
	}
}

// A Copilot route accepts images as the price table entry it is billed as
// says. A shared model that an Agent's Copilot route joins accepts them for
// that Agent only when the route does too, as the Agent's requests for the
// name may reach it.
func TestCopilotModelsAcceptImagesAsThePriceTableSays(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	gateway.shared = append(gateway.shared, sharedDeployment("team-gpt-a", "team-gpt", func(d *aigateway.Deployment) {
		d.BaseModel, d.Vision = "openai/gpt-5.5", true
	}))
	gateway.vision = map[string]bool{"claude-sonnet-4-5": true}
	allowTestPolicyModels(t, kube, "team-gpt")
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if vision := entryVision(t, r, agent); !vision["team-gpt"] {
		t.Fatalf("before sign-in: %v", vision)
	}
	signIn(t, kube, agent, `[{"id":"gpt-5.5"},{"id":"claude-sonnet-4.5"},{"id":"gpt-4.1"}]`)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	routes := map[string]api.AIGatewayCopilotRoute{}
	for _, route := range agent.Status.AIGateway.Copilot.Routes {
		routes[route.Model] = route
	}
	if len(routes) != 3 || !routes["claude-sonnet-4.5"].Vision || routes["gpt-5.5"].Vision || routes["gpt-4.1"].Vision ||
		routes["gpt-5.5"].Name != "team-gpt" {
		t.Fatalf("routes = %+v", routes)
	}
	vision := entryVision(t, r, agent)
	if !vision["claude-sonnet-4.5"] || vision["team-gpt"] || vision["gpt-4.1"] {
		t.Fatalf("entries: %v", vision)
	}
	before, added := r.aiGatewayRevision(agent), len(gateway.added)

	// The price table now says GPT-5.5 accepts images. Once the cached table
	// expires, the next pass records it and the Agent is reconfigured; the
	// routes keep their IDs.
	gateway.vision["gpt-5.5"] = true
	r.aiGatewayPrices.table = aigateway.PriceTable{}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if vision := entryVision(t, r, agent); !vision["team-gpt"] {
		t.Fatalf("after: %v", vision)
	}
	if r.aiGatewayRevision(agent) == before {
		t.Fatal("a route that starts accepting images must reconfigure the Agent")
	}
	if len(gateway.added) != added {
		t.Fatalf("no route is registered again: %v", gateway.added[added:])
	}
}

// A Team's router accepts images only when every tier does: the shared model
// it is registered as and the Agent's own Copilot routes under that name.
func TestTeamRouterAcceptsImagesOnlyWhenEveryTierDoes(t *testing.T) {
	team := routerTeam(testRouter(testRouterName, testSmallModel, testSharedModelOne, testSharedModelOne),
		testRouter("auto-sees", testSmallModel, testSmallModel, testSmallModel))
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, team, aiGatewayNamespace())
	addSmallSharedModel(gateway)
	small := len(gateway.shared) - 1
	gateway.shared[small].Vision = true
	allowTestPolicyModels(t, kube, testRouterName, "auto-sees", testSmallModel)
	reconcileRouters(t, r, kube, testAIGatewayTeam)
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	statuses := routerStatuses(agent)
	if !statuses["auto-sees"].Granted || !statuses["auto-sees"].Vision || !statuses[testRouterName].Granted || statuses[testRouterName].Vision {
		t.Fatalf("routers = %+v", statuses)
	}
	sees, mixed := modelRouterGatewayName(testAIGatewayTeam, "auto-sees"), modelRouterGatewayName(testAIGatewayTeam, testRouterName)
	if vision := entryVision(t, r, agent); !vision[sees] || vision[mixed] {
		t.Fatalf("entries: %v", vision)
	}
	before := r.aiGatewayRevision(agent)

	// The shared model of the medium and complex tiers starts accepting
	// images.
	gateway.shared[0].Vision = true
	r.refreshAIGatewayCatalogWithin(ctx, gateway, 0)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if !routerStatuses(agent)[testRouterName].Vision || !entryVision(t, r, agent)[mixed] {
		t.Fatalf("after: %+v", routerStatuses(agent))
	}
	if after := r.aiGatewayRevision(agent); after == before || !strings.Contains(after, mixed+"=auto:400000:16384:vision") {
		t.Fatalf("revision %s", after)
	}
}

// The rule for a Team router on its own: every tier, as the Gateway name it
// is registered as, must accept images, the shared deployments of that name
// and the Agent's own Copilot routes under it alike.
func TestModelRouterVisionNeedsEveryTier(t *testing.T) {
	shared := []aiGatewayCatalogModel{{Name: "sees", Vision: true}, {Name: "blind"}, {Name: "joined", Vision: true}}
	plan := func(simple, medium, complex string) modelRouterPlan {
		return modelRouterPlan{router: testRouter(testRouterName, simple, medium, complex), tierNames: map[string]string{
			"copilot/gpt-5.5": "joined", "copilot/own-1": "own-1", "copilot/blind-own": "blind-own",
		}}
	}
	routes := []api.AIGatewayCopilotRoute{
		{Model: "gpt-5.5", Name: "joined", Shared: true, Vision: true},
		{Model: "own-1", Name: "own-1", Vision: true},
		{Model: "blind-own", Name: "blind-own"},
	}
	for name, test := range map[string]struct {
		plan   modelRouterPlan
		routes []api.AIGatewayCopilotRoute
		want   bool
	}{
		"every shared tier sees":               {plan("sees", "sees", "sees"), nil, true},
		"a shared tier is blind":               {plan("sees", "blind", "sees"), nil, false},
		"a Copilot tier under its ID sees":     {plan("sees", "copilot/own-1", "sees"), routes, true},
		"a Copilot tier under its ID is blind": {plan("sees", "copilot/blind-own", "sees"), routes, false},
		"a joined tier whose route sees":       {plan("copilot/gpt-5.5", "sees", "sees"), routes, true},
		"a joined tier whose route is blind": {plan("copilot/gpt-5.5", "sees", "sees"), []api.AIGatewayCopilotRoute{
			{Model: "gpt-5.5", Name: "joined", Shared: true}}, false},
		"a tier that is neither":   {plan("sees", "unknown", "sees"), routes, false},
		"a Copilot tier not there": {plan("sees", "copilot/own-1", "sees"), nil, false},
	} {
		if got := modelRouterVision(test.plan, shared, test.routes); got != test.want {
			t.Errorf("%s: vision = %v, want %v", name, got, test.want)
		}
	}
}

// A router configured in the Gateway that accepts images does not for an
// Agent whose own Copilot route under one of its models does not. The
// router's status says so after the pass that registers the route, with the
// limits the route lowers.
func TestGatewayRouterRefusesImagesWhenTheAgentsCopilotRouteUnderATierDoes(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	gateway.shared = append(gateway.shared, sharedDeployment("team-gpt-a", "team-gpt", func(d *aigateway.Deployment) {
		d.BaseModel, d.Vision, d.MaxInputTokens, d.MaxOutputTokens = "openai/gpt-5.5", true, 1000000, 64000
	}), gatewayRouter("auto-gpt", tierModel("SIMPLE", "team-gpt")))
	allowTestPolicyModels(t, kube, "team-gpt", "auto-gpt")
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if vision := entryVision(t, r, agent); !vision["auto-gpt"] || !vision["team-gpt"] {
		t.Fatalf("before sign-in: %v", vision)
	}
	if router := routerStatuses(agent)["auto-gpt"]; !router.Granted || !router.Vision || router.ContextWindow != 1000000 {
		t.Fatalf("router = %+v", router)
	}
	if strings.Contains(r.aiGatewayRevision(agent), "gateway-router-vision") {
		t.Fatal("nothing lowers the router's vision yet")
	}

	// The owner's GPT-5.5, which the price table does not say accepts
	// images, joins the router's tier model.
	signIn(t, kube, agent, `[{"id":"gpt-5.5","contextWindow":400000}]`)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if vision := entryVision(t, r, agent); vision["auto-gpt"] || vision["team-gpt"] {
		t.Fatalf("after sign-in: %v", vision)
	}
	if router := routerStatuses(agent)["auto-gpt"]; !router.Granted || router.Vision || router.ContextWindow != 400000 || router.MaxTokens != 64000 {
		t.Fatalf("router = %+v", router)
	}
	if revision := r.aiGatewayRevision(agent); !strings.Contains(revision, "gateway-router-vision|auto-gpt") {
		t.Fatalf("revision %s", revision)
	}
}

// Whether a model accepts images is part of the configuration hash.
func TestAIGatewayRevisionFollowsTheModelsThatAcceptImages(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	before := r.aiGatewayRevision(agent)
	if strings.Contains(before, "|vision|") {
		t.Fatalf("no model accepts images yet: %s", before)
	}
	gateway.shared[0].Vision = true
	r.refreshAIGatewayCatalogWithin(ctx, gateway, 0)
	after := r.aiGatewayRevision(agent)
	if after == before || !strings.Contains(after, "|vision|"+testSharedModelOne) {
		t.Fatalf("revision %s", after)
	}
	r.refreshAIGatewayCatalogWithin(ctx, gateway, 0)
	if r.aiGatewayRevision(agent) != after {
		t.Fatal("the same listing must keep the revision")
	}
}

// The models that accept images reach a chart that takes them, and no other:
// an older chart would refuse the value.
func TestVisionModelsReachOnlyAChartThatTakesThem(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	addSmallSharedModel(gateway)
	small := len(gateway.shared) - 1
	gateway.shared[small].Vision = true
	allowTestPolicyModels(t, kube, testSmallModel)
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{aiGatewayChartValue: map[string]any{}}
	r.applyAIGatewayVisionModels(values, agent, declaredValues{{aiGatewayChartValue, aiGatewayChartModelsValue}})
	if _, set := values[aiGatewayChartValue].(map[string]any)[aiGatewayChartVisionValue]; set {
		t.Fatal("a chart that does not declare the value must not get it")
	}
	takes := declaredValues{{aiGatewayChartValue, aiGatewayChartModelsValue}, {aiGatewayChartValue, aiGatewayChartVisionValue}}
	r.applyAIGatewayVisionModels(values, agent, takes)
	if got := values[aiGatewayChartValue].(map[string]any)[aiGatewayChartVisionValue]; !reflect.DeepEqual(got, []any{testSmallModel}) {
		t.Fatalf("vision models = %#v", got)
	}
	// No model accepts images: the list is empty.
	gateway.shared[small].Vision = false
	r.refreshAIGatewayCatalogWithin(ctx, gateway, 0)
	r.applyAIGatewayVisionModels(values, agent, takes)
	if got := values[aiGatewayChartValue].(map[string]any)[aiGatewayChartVisionValue]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("vision models = %#v", got)
	}
}

// A full reconcile configures the Agent with the models that accept images,
// through the chart's own renderer: OpenClaw sends images to them, and so
// does Hermes.
func TestReconcileConfiguresTheModelsThatAcceptImages(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			r, agent, gateway := legacyGatewayFixture(t)
			agent.Spec.Agent.Type = kind
			ctx := context.Background()
			if err := r.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			gateway.shared[0].Vision = true
			chart := &chartHelm{}
			r.HelmClient = chart
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
			for i := 0; i < 4 && chart.installs == 0; i++ {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatalf("reconcile %d: %v", i, err)
				}
			}
			if chart.installs == 0 {
				t.Fatal("the Agent was never installed")
			}
			gatewayValues := chart.lastValues["aiGateway"].(map[string]any)
			if got := gatewayValues["visionModels"]; !reflect.DeepEqual(got, []any{testSharedModelOne}) {
				t.Fatalf("vision models = %#v", got)
			}
			files := chart.lastValues["runtime"].(map[string]any)["configFiles"].(map[string]any)
			switch kind {
			case api.AgentTypeOpenClaw:
				if config := files["openclaw.json"].(string); !strings.Contains(config, `"input": [`) {
					t.Fatalf("OpenClaw configuration:\n%s", config)
				}
			case api.AgentTypeHermes:
				if config := files["hermes-config.yaml"].(string); !strings.Contains(config, "supports_vision: true") {
					t.Fatalf("Hermes configuration:\n%s", config)
				}
			}
		})
	}
}
