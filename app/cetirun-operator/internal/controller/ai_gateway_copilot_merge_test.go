// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"hash/fnv"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
)

func TestCopilotModelMatchingNormalizesNamesAndRejectsSnapshots(t *testing.T) {
	for model, want := range map[string]string{
		"azure/GPT-4.1": "gpt-4-1", "openai/gpt-5.5": "gpt-5-5", "claude-sonnet-4.5": "claude-sonnet-4-5",
		"vertex_ai/anthropic/claude-sonnet-4-5": "claude-sonnet-4-5", "": "",
	} {
		if got := realModelKey(model); got != want {
			t.Errorf("realModelKey(%q) = %q, want %q", model, got, want)
		}
	}
	for id, dated := range map[string]bool{
		"gpt-4.1-2025-04-14": true, "gpt-4-0613": true, "claude-3-5-sonnet-20241022": true, "gpt-4-1106-preview": true,
		"gpt-4.1": false, "claude-sonnet-4.5": false, "gemini-2.5-pro": false, "o4-mini": false, "grok-code-fast-1": false,
	} {
		if got := datedModelID(id); got != dated {
			t.Errorf("datedModelID(%q) = %v, want %v", id, got, dated)
		}
	}
	priced := map[string]bool{"azure/gpt-4.1": true}
	for name, test := range map[string]struct {
		deployments []aigateway.Deployment
		want        string
	}{
		"base model wins":           {[]aigateway.Deployment{{Model: "azure/my-deployment", BaseModel: "azure/gpt-4.1"}}, "gpt-4-1"},
		"priced provider model":     {[]aigateway.Deployment{{Model: "azure/gpt-4.1"}}, "gpt-4-1"},
		"unpriced model is unknown": {[]aigateway.Deployment{{Model: "azure/my-deployment"}}, ""},
		"deployments agree":         {[]aigateway.Deployment{{Model: "azure/gpt-4.1"}, {BaseModel: "openai/gpt-4.1"}}, "gpt-4-1"},
		"deployments disagree":      {[]aigateway.Deployment{{Model: "azure/gpt-4.1"}, {BaseModel: "gpt-4.1-mini"}}, ""},
		"one unknown":               {[]aigateway.Deployment{{Model: "azure/gpt-4.1"}, {Model: "azure/other"}}, ""},
	} {
		if got := sharedRealModel(test.deployments, priced); got != test.want {
			t.Errorf("%s: sharedRealModel = %q, want %q", name, got, test.want)
		}
	}
}

func TestCopilotRoutingCopiesThePrimaryLevel(t *testing.T) {
	order := func(v int64) *int64 { return &v }
	weight := func(v float64) *float64 { return &v }
	routing := copilotRouting([]aigateway.Deployment{
		{Routing: aigateway.Routing{Order: order(2), Weight: weight(9), RPM: order(900)}},
		{Routing: aigateway.Routing{Order: order(1), Weight: weight(2), TPM: order(1000)}},
		{Routing: aigateway.Routing{Order: order(1), Weight: weight(3)}},
	})
	if routing.String() != "order=1,weight=3,tpm=1000" {
		t.Fatalf("a route joins the primary level with its largest weight: %s", routing)
	}
	if unordered := copilotRouting([]aigateway.Deployment{{Routing: aigateway.Routing{RPM: order(60)}}, {}}); unordered.String() != "rpm=60" {
		t.Fatalf("without order every deployment counts: %s", unordered)
	}
	if none := copilotRouting([]aigateway.Deployment{{}, {}}); !none.IsZero() {
		t.Fatalf("deployments without routing fields give none: %s", none)
	}
}

// sharedDeployment is a chart-configured shared deployment: its ID and group
// take the form the Gateway chart renders for name, and suffix stands for its
// configuration, so entries of one name share the group.
func sharedDeployment(suffix, name string, mutate func(*aigateway.Deployment)) aigateway.Deployment {
	entry := fnv.New32a()
	_, _ = entry.Write([]byte(suffix))
	deployment := aigateway.Deployment{
		ID: managedTestDeploymentID(name, int(entry.Sum32())), ModelName: name, DatabaseSourceKnown: true, Priced: true,
		AccessGroups: []string{aiGatewaySharedAccessGroup, aiGatewayModelAccessGroupPrefix + aiGatewayModelBinding(name)},
	}
	if mutate != nil {
		mutate(&deployment)
	}
	return deployment
}

// routesByModel maps each registered Copilot model of the test Agent to its
// route in the Gateway.
func routesByModel(gateway *fakeAIGateway) map[string]aigateway.ModelRequest {
	routes := map[string]aigateway.ModelRequest{}
	for _, route := range gateway.models {
		routes[route.Model[len("litellm_proxy/"):]] = route
	}
	return routes
}

func TestReconcileAIGatewayCopilotJoinsTheSharedModelOfTheSameRealModel(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Spec.Model = "copilot/gpt-5.5"
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	one, two := int64(1), int64(2)
	gateway.shared = append(gateway.shared,
		sharedDeployment("team-gpt-a", "team-gpt", func(d *aigateway.Deployment) {
			d.BaseModel, d.MaxInputTokens, d.MaxOutputTokens = "openai/gpt-5.5", 1000000, 64000
			d.Routing = aigateway.Routing{Order: &one, Weight: ptrFloat(2)}
		}),
		sharedDeployment("team-gpt-b", "team-gpt", func(d *aigateway.Deployment) {
			d.BaseModel = "gpt-5.5"
			d.Routing = aigateway.Routing{Order: &two, Weight: ptrFloat(9)}
		}),
		// The Copilot model's own ID, but another model.
		sharedDeployment("claude-other", "claude-sonnet-4.5", func(d *aigateway.Deployment) { d.BaseModel = "anthropic/claude-opus-4" }),
		// The same model, but the Agent's policy does not allow it.
		sharedDeployment("gpt41", "gpt-4.1", func(d *aigateway.Deployment) { d.Model = "azure/gpt-4.1" }),
		// A snapshot is never merged, even into the same snapshot.
		sharedDeployment("team-4o", "team-4o", func(d *aigateway.Deployment) { d.BaseModel = "gpt-4o-2024-11-20" }),
	)
	gateway.priced["azure/gpt-4.1"], gateway.priced["gpt-4o-2024-11-20"] = true, true
	allowTestPolicyModels(t, kube, "team-gpt", "claude-sonnet-4.5", "team-4o")
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	signIn(t, kube, agent, `[{"id":"gpt-5.5","name":"GPT-5.5","contextWindow":400000,"maxOutputTokens":128000},`+
		`{"id":"claude-sonnet-4.5"},{"id":"gpt-4.1"},{"id":"gpt-4o-2024-11-20","name":"GPT-4o"},{"id":"grok-code-fast-1"}]`)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	copilot := agent.Status.AIGateway.Copilot
	if copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady {
		t.Fatalf("copilot status %+v", copilot)
	}
	routes := routesByModel(gateway)
	merged, own := routes["gpt-5.5"], routes["gpt-4o-2024-11-20"]
	if len(routes) != 2 || merged.ModelName != "team-gpt" || merged.NoCooldown || merged.Routing.String() != "order=1,weight=2" ||
		!slices.Equal(merged.AccessGroups, []string{copilotGroup()}) || merged.BaseModel != "gpt-5.5" {
		t.Fatalf("the Copilot model joins the shared model at its primary level: %+v", routes)
	}
	if own.ModelName != "gpt-4o-2024-11-20" || !own.NoCooldown || !own.Routing.IsZero() {
		t.Fatalf("a dated model keeps its own name and is never cooled down: %+v", own)
	}
	for _, route := range routes {
		if !route.ProbeWithoutRetries || !route.NoBackgroundHealthCheck {
			t.Fatalf("every Copilot route is probed without the provider client's retries, and by default not at all: %+v", route)
		}
	}
	// The same model as a shared model the Agent's policy does not allow:
	// the policy decides that model, also from the owner's Copilot, so the
	// Copilot model is left out and reported.
	wantRoutes := []cetirunv1alpha1.AIGatewayCopilotRoute{
		{Model: "gpt-4o-2024-11-20", Name: "gpt-4o-2024-11-20"}, {Model: "gpt-5.5", Name: "team-gpt", Shared: true},
	}
	if !slices.Equal(copilot.Routes, wantRoutes) || !slices.Equal(copilot.Models, []string{"gpt-4o-2024-11-20", "team-gpt"}) ||
		!slices.Equal(copilot.Skipped, []string{"claude-sonnet-4.5"}) || !slices.Equal(copilot.Unpriced, []string{"grok-code-fast-1"}) ||
		!slices.Equal(copilot.NotAllowed, []string{"gpt-4.1"}) {
		t.Fatalf("copilot status %+v", copilot)
	}
	// The Agent sees one entry per model: the shared model stands for the
	// Copilot model, with the smaller limits. The model the policy does not
	// allow is not among them.
	wantModels := []string{"claude-sonnet-4.5", testSharedModelOne, "team-4o", "team-gpt", "gpt-4o-2024-11-20"}
	if !slices.Equal(agent.Status.AIGateway.Models, wantModels) || agent.Status.AIGateway.DefaultModel != "team-gpt" {
		t.Fatalf("models %v default %q", agent.Status.AIGateway.Models, agent.Status.AIGateway.DefaultModel)
	}
	entries, _, _ := r.aiGatewayModelEntries(agent)
	for _, entry := range entries {
		if entry.ID == "team-gpt" && (entry.ContextWindow != 400000 || entry.MaxTokens != 64000 || entry.Name != "team-gpt") {
			t.Fatalf("a merged entry takes the smaller limits: %+v", entry)
		}
		if entry.ID == "gpt-4o-2024-11-20" && entry.Name != "gpt-4o-2024-11-20" {
			t.Fatalf("a Copilot entry is named like any other model: %+v", entry)
		}
	}
	// The key gets no copilot/<ID> aliases: each would list the Copilot model
	// a second time in the key's model list.
	key := gateway.keys[aigateway.Token(string(readSecret(t, kube, aiGatewayKeySecretName).Data[aiGatewayKeySecretField]))]
	if len(key.Aliases) != 0 {
		t.Fatalf("the key has no copilot/ aliases: %v", key.Aliases)
	}
	// A route under a shared name changes neither the shared models nor the
	// policy's view of them.
	if r.refreshAIGatewayCatalogWithin(ctx, gateway, 0) {
		t.Fatal("a Copilot route sharing a name must not count as a catalog change")
	}

	// Steady state: nothing is registered again, the aliases are not rewritten.
	added, aliasUpdates := len(gateway.added), len(gateway.aliasUpdates)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.added) != added || len(gateway.aliasUpdates) != aliasUpdates {
		t.Fatal("a steady reconcile must not re-register routes or rewrite aliases")
	}

	// The shared deployments change their routing: the route follows.
	gateway.shared[2].Routing.Weight = ptrFloat(5)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if routes := routesByModel(gateway); routes["gpt-5.5"].Routing.String() != "order=1,weight=5" || routes["gpt-5.5"].ID == merged.ID ||
		!slices.Contains(gateway.deletedModels, merged.ID) {
		t.Fatalf("a changed routing replaces the route: %+v deleted %v", routes, gateway.deletedModels)
	}

	// The deployment turns the probes of Copilot routes on: every route is
	// registered again, probed and under the ID it had before the probes
	// could be turned off, and the unprobed one is removed. Then off again.
	// The Agent's models and configuration, and its policy's Gateway
	// projection, stay as they are each time, so the Agent is not restarted.
	unprobed := maps.Clone(gateway.models)
	routesBefore := slices.Clone(agent.Status.AIGateway.Copilot.Routes)
	modelsBefore := slices.Clone(agent.Status.AIGateway.Copilot.Models)
	policyRevision := func() string {
		t.Helper()
		resolution, err := r.resolvePolicy(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		return resolution.gatewayRevision
	}
	revision, hash := r.aiGatewayRevision(agent), r.configHash(agent, nil, "", "")
	projection := policyRevision()
	proxyKey := string(readSecret(t, kube, aiGatewayProxySecretName).Data[aiGatewayProxySecretField])
	unchanged := func(step string) {
		t.Helper()
		current := agent.Status.AIGateway.Copilot
		if current.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady || !slices.Equal(current.Routes, routesBefore) ||
			!slices.Equal(current.Models, modelsBefore) {
			t.Fatalf("%s: the Copilot routes keep their models and names: %+v", step, current)
		}
		if r.aiGatewayRevision(agent) != revision || r.configHash(agent, nil, "", "") != hash {
			t.Fatalf("%s: the Agent's configuration must not change", step)
		}
		if policyRevision() != projection {
			t.Fatalf("%s: the policy's Gateway projection must not change", step)
		}
	}
	r.AIGateway.CopilotHealthChecks = true
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	unchanged("probes on")
	if len(gateway.models) != len(unprobed) {
		t.Fatalf("each route is registered once: %v", gateway.modelNames())
	}
	for id, route := range gateway.models {
		placement := copilotPlacement{name: route.ModelName, shared: !route.NoCooldown, routing: route.Routing}
		earlier := copilotRegistrationID(agent, route.Model[len("litellm_proxy/"):], copilotRevision(append([]string{"probe-without-retries",
			copilotProxyURL(agent), proxyKey, route.BaseModel}, copilotPlacementRevision(placement)...)...))
		if route.NoBackgroundHealthCheck || !route.ProbeWithoutRetries || id != earlier {
			t.Fatalf("a probed route is probed without retries, under its earlier ID %s: %+v", earlier, route)
		}
	}
	for id := range unprobed {
		if _, kept := gateway.models[id]; kept || !slices.Contains(gateway.deletedModels, id) {
			t.Fatalf("the unprobed route %s is replaced: deleted %v", id, gateway.deletedModels)
		}
	}
	probed := maps.Clone(gateway.models)
	r.AIGateway.CopilotHealthChecks = false
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	unchanged("probes off")
	if ids := slices.Sorted(maps.Keys(gateway.models)); !slices.Equal(ids, slices.Sorted(maps.Keys(unprobed))) {
		t.Fatalf("turned off again, the routes are registered unprobed as before: %v", ids)
	}
	for id, route := range gateway.models {
		if !route.NoBackgroundHealthCheck || !route.ProbeWithoutRetries {
			t.Fatalf("turned off again, route %s is left out of the probes: %+v", id, route)
		}
	}
	for id := range probed {
		if !slices.Contains(gateway.deletedModels, id) {
			t.Fatalf("the probed route %s is replaced: deleted %v", id, gateway.deletedModels)
		}
	}

	// A deployment default may name a Copilot model the earlier way too.
	agent.Spec.Model = ""
	r.AIGateway.DefaultModel = "copilot/gpt-4o-2024-11-20"
	if _, def, _ := r.aiGatewayModelEntries(agent); def != "gpt-4o-2024-11-20" {
		t.Fatalf("a copilot/<ID> deployment default names the model's route: %q", def)
	}
}

func ptrFloat(value float64) *float64 { return &value }

func TestReconcileAIGatewayCopilotFollowsSharedModelsOfALegacyAgent(t *testing.T) {
	r, agent, gateway := legacyGatewayFixture(t)
	r.AIGateway.CopilotProxyImage = testProxyImage
	ctx := context.Background()
	if err := r.Create(ctx, readyProxy(1)); err != nil {
		t.Fatal(err)
	}
	base := slices.Clone(gateway.shared)
	shared := aigateway.Deployment{ID: "legacy-gpt", ModelName: "gpt-x", AccessGroups: []string{aiGatewaySharedAccessGroup},
		BaseModel: "gpt-5.5", Routing: aigateway.Routing{Weight: ptrFloat(3)}}
	gateway.shared = append(gateway.shared, shared)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	signIn(t, r.Client, agent, `[{"id":"gpt-5.5"}]`)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	route := routesByModel(gateway)["gpt-5.5"]
	if route.ModelName != "gpt-x" || route.Routing.String() != "weight=3" || route.NoCooldown {
		t.Fatalf("a legacy Agent's Copilot model joins every shared model of the same real model: %+v", route)
	}
	if slices.Contains(agent.Status.AIGateway.Models, "gpt-5.5") || !slices.Contains(agent.Status.AIGateway.Models, "gpt-x") {
		t.Fatalf("the Agent lists the model once: %v", agent.Status.AIGateway.Models)
	}

	// The shared model goes: the route takes the Copilot model's own name.
	gateway.shared = slices.Clone(base)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	moved := routesByModel(gateway)["gpt-5.5"]
	if len(gateway.models) != 1 || moved.ModelName != "gpt-5.5" || !moved.NoCooldown || !slices.Contains(gateway.deletedModels, route.ID) {
		t.Fatalf("the route moves to the model's own name: %+v", gateway.models)
	}
	key := gateway.keys[aigateway.Token(string(readSecret(t, r.Client, aiGatewayKeySecretName).Data[aiGatewayKeySecretField]))]
	if len(key.Aliases) != 0 || !slices.Contains(agent.Status.AIGateway.Models, "gpt-5.5") {
		t.Fatalf("aliases %v models %v", key.Aliases, agent.Status.AIGateway.Models)
	}

	// With shared models off the Agent is granted none, so a model the shared
	// deployments also serve keeps its own name.
	gateway.shared = append(slices.Clone(base), shared)
	agent.Spec.ModelAccess = modelAccess(false, true)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if own := routesByModel(gateway)["gpt-5.5"]; own.ModelName != "gpt-5.5" || !own.NoCooldown {
		t.Fatalf("an Agent without shared models keeps the Copilot model's own name: %+v", own)
	}

	// Copilot off: routes and aliases go.
	agent.Spec.ModelAccess = modelAccess(true, false)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.models) != 0 || len(key.Aliases) != 0 {
		t.Fatalf("turning Copilot off removes routes and aliases: %v %v", gateway.modelNames(), key.Aliases)
	}
}

// An AgentPolicy's shared models decide the owner's Copilot models that are
// the same models: such a Copilot model is offered only when the policy
// allows a shared model that is that model, whether its name in the Gateway
// is the Copilot model's ID or another name, and also for the same dated
// version. A Copilot model that no shared model is needs only Copilot.
func TestAgentPolicySharedModelsDecideTheSameCopilotModels(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	gateway.shared = append(gateway.shared,
		// Named as the Copilot model is.
		sharedDeployment("astra", "gpt-6-astra", func(d *aigateway.Deployment) { d.BaseModel = "azure/gpt-6-astra" }),
		// The same model as a Copilot model, under another name.
		sharedDeployment("sol", "enterprise-sol", func(d *aigateway.Deployment) { d.BaseModel = "azure/gpt-5.6-sol" }),
		// The same dated version as a Copilot model.
		sharedDeployment("dated", "team-4o", func(d *aigateway.Deployment) { d.BaseModel = "gpt-4o-2024-11-20" }),
	)
	for _, name := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-4o-2024-11-20"} {
		gateway.priced[name] = true
	}
	ctx := context.Background()
	reconcile := func(step string) (*cetirunv1alpha1.AIGatewayCopilotStatus, map[string]aigateway.ModelRequest) {
		t.Helper()
		if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		copilot := agent.Status.AIGateway.Copilot
		if copilot == nil || copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady {
			t.Fatalf("%s: copilot status %+v", step, copilot)
		}
		return copilot, routesByModel(gateway)
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	signIn(t, kube, agent, `[{"id":"gpt-6-astra"},{"id":"gpt-5.6-sol"},{"id":"gpt-4o-2024-11-20"},{"id":"claude-sonnet-4.5"}]`)

	// The policy allows none of these shared models.
	copilot, routes := reconcile("policy without them")
	if !slices.Equal(copilot.NotAllowed, []string{"gpt-4o-2024-11-20", "gpt-5.6-sol", "gpt-6-astra"}) || len(copilot.Skipped) != 0 ||
		len(routes) != 1 || routes["claude-sonnet-4.5"].ModelName != "claude-sonnet-4.5" {
		t.Fatalf("only the model that no shared model is is offered: %+v routes %+v", copilot, routes)
	}
	for _, name := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-4o-2024-11-20", "enterprise-sol"} {
		if slices.Contains(agent.Status.AIGateway.Models, name) {
			t.Fatalf("the Agent is configured with %s: %v", name, agent.Status.AIGateway.Models)
		}
	}

	// The policy allows the shared model of another name: the Copilot model
	// joins it.
	allowTestPolicyModels(t, kube, "enterprise-sol")
	copilot, routes = reconcile("policy with enterprise-sol")
	if joined := routes["gpt-5.6-sol"]; joined.ModelName != "enterprise-sol" || joined.NoCooldown ||
		!slices.Equal(copilot.NotAllowed, []string{"gpt-4o-2024-11-20", "gpt-6-astra"}) {
		t.Fatalf("the Copilot model joins the shared model the policy allows: %+v routes %+v", copilot, routes)
	}

	// The owner turns shared models off. The policy still decides: a model it
	// allows keeps its own name in the Agent's own group, as for an Agent
	// without a policy, and one it does not allow stays out.
	allowTestPolicyModels(t, kube, "gpt-6-astra")
	agent.Spec.ModelAccess = modelAccess(false, true)
	copilot, routes = reconcile("shared models off")
	own, joined := routes["gpt-6-astra"], routes["gpt-5.6-sol"]
	if own.ModelName != "gpt-6-astra" || !own.NoCooldown || !slices.Equal(own.AccessGroups, []string{copilotGroup()}) ||
		joined.ModelName != "gpt-5.6-sol" || !slices.Equal(copilot.NotAllowed, []string{"gpt-4o-2024-11-20"}) {
		t.Fatalf("with shared models off: %+v routes %+v", copilot, routes)
	}
}

// copilotRevocationTest is a policy-bound Agent signed in to GitHub Copilot
// with gpt-5.6-sol, the same model as the shared model enterprise-sol that
// its policy allows, and claude-sonnet-4.5, which no shared model is.
type copilotRevocationTest struct {
	r       *AgentReconciler
	kube    client.Client
	gateway *fakeAIGateway
	agent   *cetirunv1alpha1.Agent
	token   string
	// route is the gpt-5.6-sol route.
	route aigateway.ModelRequest
}

func newCopilotRevocationTest(t *testing.T, shared bool) *copilotRevocationTest {
	t.Helper()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Spec.ModelAccess = modelAccess(shared, true)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	gateway.shared = append(gateway.shared,
		sharedDeployment("sol", "enterprise-sol", func(d *aigateway.Deployment) { d.BaseModel = "azure/gpt-5.6-sol" }))
	gateway.priced["gpt-5.6-sol"] = true
	allowTestPolicyModels(t, kube, "enterprise-sol")
	test := &copilotRevocationTest{r: r, kube: kube, gateway: gateway, agent: agent}
	test.reconcile(t, "before the sign-in")
	signIn(t, kube, agent, `[{"id":"gpt-5.6-sol"},{"id":"claude-sonnet-4.5"}]`)
	test.reconcile(t, "signed in")
	test.token = aigateway.Token(string(readSecret(t, r.Client, aiGatewayKeySecretName).Data[aiGatewayKeySecretField]))
	test.route = routesByModel(gateway)["gpt-5.6-sol"]
	name := map[bool]string{false: "gpt-5.6-sol", true: "enterprise-sol"}[shared]
	if test.route.ModelName != name || !test.keyHoldsCopilot() {
		t.Fatalf("the test needs the route under %s and the Copilot group on the key: %+v, key %v", name, test.route, gateway.keys[test.token].Models)
	}
	copilotWithheldEvents(r)
	return test
}

// reconcile runs a full pass and requires the key to be read back.
func (c *copilotRevocationTest) reconcile(t *testing.T, step string) {
	t.Helper()
	if err := c.r.reconcileAIGateway(context.Background(), c.agent, true); err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	if c.agent.Status.AIGateway.Phase != cetirunv1alpha1.AIGatewayPhaseReady {
		t.Fatalf("%s: AI Gateway status %+v", step, c.agent.Status.AIGateway)
	}
}

// keyHoldsCopilot reports whether the key, and the status, hold the Agent's
// Copilot group.
func (c *copilotRevocationTest) keyHoldsCopilot() bool {
	return slices.Contains(c.gateway.keys[c.token].Models, copilotGroup()) && slices.Contains(c.agent.Status.AIGateway.AccessGroups, copilotGroup())
}

// routeThere reports whether the Gateway still has the gpt-5.6-sol route.
func (c *copilotRevocationTest) routeThere() bool {
	_, there := c.gateway.models[c.route.ID]
	return there
}

// copilotWithheldEvents drains the recorded events and counts the
// AIGatewayCopilotWithheld ones.
func copilotWithheldEvents(r *AgentReconciler) int {
	events := r.EventRecorder.(*record.FakeRecorder).Events
	count := 0
	for len(events) > 0 {
		if strings.Contains(<-events, "AIGatewayCopilotWithheld") {
			count++
		}
	}
	return count
}

// With an AgentPolicy, the route of a Copilot model that the policy no longer
// allows goes before the key is written. While the Gateway cannot remove it,
// the key does not hold the Agent's Copilot group, which reaches every route
// of the Agent: the route stays out of reach, under its own name with shared
// models off, and under the shared model's name it joined with them on. The
// Agent's configuration does not change for it, and the key gets the group
// back once the route is removed.
func TestAgentPolicyWithholdsCopilotWhileANotAllowedRouteRemains(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared models off", true: "shared models on"}[shared], func(t *testing.T) {
			c := newCopilotRevocationTest(t, shared)
			disallowTestPolicyModels(t, c.kube, "enterprise-sol")
			c.gateway.deleteModelErr = errors.New("synthetic outage")

			// A pass that fails before the key is written reports nothing.
			c.gateway.getErr = errors.New("synthetic key API outage")
			if err := c.r.reconcileAIGateway(context.Background(), c.agent, true); err == nil {
				t.Fatal("the pass must fail while the key cannot be read")
			}
			if events, copilot := copilotWithheldEvents(c.r), c.agent.Status.AIGateway.Copilot; events != 0 ||
				copilot.Reason == aiGatewayCopilotReasonNotAllowedRouteRemovalFailed {
				t.Fatalf("%d events, copilot status %+v: nothing is reported before the key is written", events, copilot)
			}
			c.gateway.getErr = nil

			c.reconcile(t, "removal fails")
			copilot := c.agent.Status.AIGateway.Copilot
			if !c.routeThere() || c.keyHoldsCopilot() {
				t.Fatalf("the key must not hold the group of a route that is still there: key %v, status %v",
					c.gateway.keys[c.token].Models, c.agent.Status.AIGateway.AccessGroups)
			}
			if copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseDegraded || copilot.Reason != aiGatewayCopilotReasonNotAllowedRouteRemovalFailed ||
				!slices.Equal(copilot.NotAllowed, []string{"gpt-5.6-sol"}) || !slices.Contains(copilot.Registered, c.route.ID) {
				t.Fatalf("copilot status %+v", copilot)
			}
			// The key withholds the other Copilot model; the configuration
			// keeps it.
			if models := c.agent.Status.AIGateway.Models; !slices.Contains(models, "claude-sonnet-4.5") || slices.Contains(models, c.route.ModelName) {
				t.Fatalf("models %v", models)
			}
			if events := copilotWithheldEvents(c.r); events != 1 {
				t.Fatalf("%d AIGatewayCopilotWithheld events, want 1", events)
			}

			// Another failing pass leaves the key and reports nothing new.
			updates := len(c.gateway.updates)
			c.reconcile(t, "removal fails again")
			if len(c.gateway.updates) != updates || c.keyHoldsCopilot() || copilotWithheldEvents(c.r) != 0 {
				t.Fatalf("key updates %d -> %d, key %v", updates, len(c.gateway.updates), c.gateway.keys[c.token].Models)
			}

			// The Gateway removes the route: the key gets the group back.
			c.gateway.deleteModelErr = nil
			c.reconcile(t, "removal works")
			copilot = c.agent.Status.AIGateway.Copilot
			if c.routeThere() || !c.keyHoldsCopilot() || copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady ||
				slices.Contains(copilot.Registered, c.route.ID) {
				t.Fatalf("key %v, copilot status %+v", c.gateway.keys[c.token].Models, copilot)
			}
			if _, kept := routesByModel(c.gateway)["claude-sonnet-4.5"]; !kept {
				t.Fatal("the other Copilot route must stay")
			}
		})
	}
}

// When the Gateway removes the route of a Copilot model that the policy no
// longer allows, the key keeps the Agent's Copilot group, and the Agent's
// other Copilot models are not interrupted.
func TestAgentPolicyRemovesANotAllowedRouteWithoutWithholdingCopilot(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared models off", true: "shared models on"}[shared], func(t *testing.T) {
			c := newCopilotRevocationTest(t, shared)
			disallowTestPolicyModels(t, c.kube, "enterprise-sol")
			updates := len(c.gateway.updates)
			c.reconcile(t, "revoked")
			for _, models := range c.gateway.updates[updates:] {
				if !slices.Contains(models, copilotGroup()) {
					t.Fatalf("the key lost the Copilot group: %v", c.gateway.updates[updates:])
				}
			}
			copilot := c.agent.Status.AIGateway.Copilot
			if c.routeThere() || !c.keyHoldsCopilot() || copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady ||
				!slices.Equal(copilot.NotAllowed, []string{"gpt-5.6-sol"}) || copilotWithheldEvents(c.r) != 0 {
				t.Fatalf("key %v, copilot status %+v", c.gateway.keys[c.token].Models, copilot)
			}
			if _, kept := routesByModel(c.gateway)["claude-sonnet-4.5"]; !kept {
				t.Fatal("the other Copilot route must stay")
			}
		})
	}
}

// While the operator cannot list the Gateway's models, as at a start during a
// Gateway outage, or cannot read its price table, it cannot tell which of the
// Agent's routes the policy allows. The key does not hold the Agent's Copilot
// group until it can.
func TestAgentPolicyWithholdsCopilotWhileTheRoutesCannotBeChecked(t *testing.T) {
	c := newCopilotRevocationTest(t, false)
	checkWithheld := func(step string) {
		t.Helper()
		copilot := c.agent.Status.AIGateway.Copilot
		if c.keyHoldsCopilot() || copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseDegraded ||
			copilot.Reason != aiGatewayCopilotReasonPolicyCheckUnavailable || !c.routeThere() {
			t.Fatalf("%s: key %v, copilot status %+v", step, c.gateway.keys[c.token].Models, copilot)
		}
	}
	checkRestored := func(step string) {
		t.Helper()
		if copilot := c.agent.Status.AIGateway.Copilot; !c.keyHoldsCopilot() || copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady || !c.routeThere() {
			t.Fatalf("%s: key %v, copilot status %+v", step, c.gateway.keys[c.token].Models, copilot)
		}
	}
	c.r.aiGatewayCatalog = aiGatewayCatalog{}
	c.gateway.listErr = errors.New("synthetic outage")
	c.reconcile(t, "models not listed")
	checkWithheld("models not listed")
	c.gateway.listErr = nil
	c.reconcile(t, "models listed")
	checkRestored("models listed")

	c.r.aiGatewayPrices = aiGatewayPriceCache{}
	c.gateway.priceErr = errors.New("synthetic outage")
	c.reconcile(t, "prices not read")
	checkWithheld("prices not read")
	c.gateway.priceErr = nil
	c.reconcile(t, "prices read")
	checkRestored("prices read")
}

func TestCopilotRouteModelReadsTheModelFromTheRouteID(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	for _, model := range []string{"gpt-5.5", "claude-sonnet-4.5", "gpt-4.1-2025-04-14", "o3", "a"} {
		id := copilotRegistrationID(agent, model, copilotRevision("format", "proxy", model))
		if got, ok := copilotRouteModel(agent, id); !ok || got != model {
			t.Errorf("copilotRouteModel(%q) = %q, %v; want %q", id, got, ok, model)
		}
	}
	other := aiGatewayTestAgent(testAIGatewayTeam)
	other.UID = "another-uid"
	prefix := copilotAccessGroup(agent) + "-"
	for _, id := range []string{
		copilotRegistrationID(other, "gpt-5.5", copilotRevision("x")), // another Agent's route
		prefix + "gpt-5.5",                    // no revision
		prefix + "gpt-5.5-0123456",            // a revision of another length
		prefix + "gpt-5.5-0123456g",           // a revision that is not hexadecimal
		prefix + "bad/model-01234567",         // not a Copilot model ID
		"cetirun-shared-gpt-5.5-01234567",     // not a Copilot route
		copilotAccessGroup(agent) + "gpt-5.5", // no separator after the group
	} {
		if got, ok := copilotRouteModel(agent, id); ok {
			t.Errorf("copilotRouteModel(%q) = %q, want none", id, got)
		}
	}
}

func TestPlaceCopilotModelsPicksTheFirstGrantedSharedName(t *testing.T) {
	r, _, _ := aiGatewayTestReconciler(t)
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Spec.SandboxRef, agent.Spec.ModelAccess, agent.Status.Policy = "", nil, nil
	deployments := []aigateway.Deployment{
		{ID: "b", ModelName: "b-gpt", AccessGroups: []string{aiGatewaySharedAccessGroup}, BaseModel: "gpt-5.5"},
		{ID: "a", ModelName: "a-gpt", AccessGroups: []string{aiGatewaySharedAccessGroup}, BaseModel: "openai/gpt-5.5"},
	}
	models := []cetirunv1alpha1.CopilotModel{{ID: "gpt-5.5"}}
	if _, _, _, ok := r.placeCopilotModels(agent, models, nil, nil); ok {
		t.Fatal("placements need a listed catalog")
	}
	r.aiGatewayCatalog.update(deployments, false, time.Now())
	placements, skipped, notAllowed, ok := r.placeCopilotModels(agent, models, nil, nil)
	if !ok || len(skipped) != 0 || len(notAllowed) != 0 || placements["gpt-5.5"].name != "a-gpt" || !placements["gpt-5.5"].shared {
		t.Fatalf("several matching shared names: the first in sort order wins: %+v %v %v", placements, skipped, notAllowed)
	}
}

func TestCopilotDefaultModelNamesAndAliases(t *testing.T) {
	agent := routedAgent()
	agent.Spec.Model = "copilot/gpt-5.5"
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{Models: []string{"copilot/gpt-5.5"}}
	if got := aiGatewayRequestedModel(agent); got != "copilot/gpt-5.5" {
		t.Fatalf("a status recorded before routes keeps the earlier name: %q", got)
	}
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{
		Models: []string{"team-gpt"}, Routes: []cetirunv1alpha1.AIGatewayCopilotRoute{{Model: "gpt-5.5", Name: "team-gpt", Shared: true}},
	}
	if got := aiGatewayRequestedModel(agent); got != "team-gpt" {
		t.Fatalf("copilot/<ID> names the model's route: %q", got)
	}
	agent.Spec.Model = "copilot/unknown"
	if got := aiGatewayRequestedModel(agent); got != "copilot/unknown" {
		t.Fatalf("an unknown Copilot model is left as it is: %q", got)
	}
	aliases := aiGatewayKeyAliases(map[string]string{"copilot/gone": "gone", "copilot/gpt-5.5": "team-gpt", "admin-alias": "team-gpt"})
	if !maps.Equal(aliases, map[string]string{"admin-alias": "team-gpt"}) {
		t.Fatalf("earlier copilot/ aliases go, others stay: %v", aliases)
	}
	for model, allowed := range map[string]bool{"copilot/gpt-5.5": true, "gpt-5.5": true, "team-gpt": true, "has space": false, "a/b": false} {
		if got := copilotDefaultModelAllowed(model); got != allowed {
			t.Errorf("copilotDefaultModelAllowed(%q) = %v", model, got)
		}
	}
	policy := aiGatewayTestPolicy()
	policyAgent := aiGatewayTestAgent(testAIGatewayTeam)
	policyAgent.Spec.Model = "gpt-5.5"
	if _, _, err := effectivePolicyModels(policyAgent, policy); err != nil {
		t.Fatalf("a Copilot model's own name is a valid default under a policy allowing Copilot: %v", err)
	}
	policyAgent.Spec.ModelAccess = modelAccess(true, false)
	if _, _, err := effectivePolicyModels(policyAgent, policy); err == nil {
		t.Fatal("without Copilot a default outside the policy's shared models is refused")
	}
}

// A Copilot route under a shared model's name is priced by its base model. It
// must not make an unpriced shared model look priced, nor become a shared
// model itself, nor change the policy's view of the shared models.
func TestAIGatewayCatalogIgnoresCopilotRoutesUnderSharedNames(t *testing.T) {
	var catalog aiGatewayCatalog
	shared := sharedDeployment("plain", "plain-model", nil)
	shared.Priced = false
	route := aigateway.Deployment{ID: "cetirun-copilot-uid-x-1", ModelName: "plain-model", AccessGroups: []string{"cetirun-copilot-uid"},
		FromDatabase: true, DatabaseSourceKnown: true, Priced: true, BaseModel: "gpt-5.5", MaxInputTokens: 10}
	_, _, unpriced := catalog.update([]aigateway.Deployment{shared, route}, true, time.Now())
	if models, _ := catalog.shared(); len(models) != 0 || !slices.Equal(unpriced, []string{"plain-model"}) {
		t.Fatalf("an unpriced shared model stays out while the Gateway blocks unpriced models: %+v %v", models, unpriced)
	}
	shared.Priced = true
	catalog.update([]aigateway.Deployment{shared}, true, time.Now())
	fingerprint, _ := catalog.managedFingerprint()
	if changed, _, _ := catalog.update([]aigateway.Deployment{shared, route}, true, time.Now()); changed {
		t.Fatal("a Copilot route joining a shared name is no catalog change")
	}
	models, _ := catalog.shared()
	view, _ := catalog.copilotMergeSnapshot()
	after, _ := catalog.managedFingerprint()
	if len(models) != 1 || models[0].ContextWindow != 0 || after != fingerprint || len(view.shared["plain-model"]) != 1 || !view.served["plain-model"] {
		t.Fatalf("the route is not part of the shared model: %+v view %+v", models, view.shared)
	}
}

// Without a catalog, an Agent keeps the shared models last configured; its
// Copilot models under their own IDs are not taken for shared ones.
func TestAIGatewayModelEntriesWithoutCatalogKeepCopilotModelsApart(t *testing.T) {
	r, _, _ := aiGatewayTestReconciler(t)
	agent := routedAgent()
	agent.Spec.SandboxRef, agent.Status.Policy = "", nil
	agent.Status.AIGateway.Models = []string{testSharedModelOne, "team-gpt", "gpt-4o"}
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{
		Models: []string{"gpt-4o", "team-gpt"},
		Routes: []cetirunv1alpha1.AIGatewayCopilotRoute{{Model: "gpt-4o", Name: "gpt-4o"}, {Model: "gpt-5.5", Name: "team-gpt", Shared: true}},
	}
	entries, _, _ := r.aiGatewayModelEntries(agent)
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.ID+"="+entry.Name)
	}
	want := []string{testSharedModelOne + "=" + testSharedModelOne, "team-gpt=team-gpt", "gpt-4o=gpt-4o"}
	if !slices.Equal(got, want) {
		t.Fatalf("entries %v, want %v", got, want)
	}
}

// A key an earlier version gave copilot/<ID> aliases loses them on the next
// reconcile, without a new key; an administrator's alias stays.
func TestEarlierCopilotKeyAliasesAreRemoved(t *testing.T) {
	r, agent, gateway := legacyGatewayFixture(t)
	r.AIGateway.CopilotProxyImage = testProxyImage
	ctx := context.Background()
	if err := r.Create(ctx, readyProxy(1)); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	signIn(t, r.Client, agent, `[{"id":"gpt-5.5"}]`)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	token := aigateway.Token(string(readSecret(t, r.Client, aiGatewayKeySecretName).Data[aiGatewayKeySecretField]))
	gateway.keys[token].Aliases = map[string]string{"copilot/gpt-5.5": "gpt-5.5", "admin-alias": "gpt-5.5"}
	keys := len(gateway.keys)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if key := gateway.keys[token]; key == nil || !maps.Equal(key.Aliases, map[string]string{"admin-alias": "gpt-5.5"}) || len(gateway.keys) != keys {
		t.Fatalf("the same key keeps only the administrator's alias: %+v (keys %d -> %d)", key, keys, len(gateway.keys))
	}
	if phase := agent.Status.AIGateway.Copilot.Phase; phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady {
		t.Fatalf("copilot phase %q", phase)
	}
}
