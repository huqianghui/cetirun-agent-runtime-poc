// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
)

// Team model routers ("smart routing"). Administrators define them on a Team;
// the operator registers each one in the AI Gateway as a LiteLLM complexity
// router under an internal model name, and grants it to the Team's Agents
// whose own entitlement covers every model the router uses, so that a router
// never widens an Agent's models. Routers configured in the Gateway itself
// follow the same rule (ai_gateway_shared_routers.go).
const (
	// aiGatewayRouterPrefix prefixes the internal model name, access group
	// and deployment ID of every Team model router.
	aiGatewayRouterPrefix = "cetirun-router-"
	// aiGatewayRouterInterval is how often registrations are checked again.
	aiGatewayRouterInterval = 5 * time.Minute
	// The router's own name has to carry a price for the Gateway's check for
	// unpriced models. Each request is charged at the price of the tier model
	// it goes to; this price is never charged.
	aiGatewayRouterInputCost  = 0.00001
	aiGatewayRouterOutputCost = 0.00005
	// copilotTierPrefix marks a tier that names the GitHub Copilot model of
	// the Agent's owner, the name the Agent's Copilot routes have.
	copilotTierPrefix = api.AIGatewayCopilotModelPrefix
)

// routerTierGatewayName is the Gateway name a tier is registered with. A
// copilot/<id> tier names the owner's Copilot model, which the Gateway serves
// under the model's own ID unless it joins a shared model: then the tier is
// registered as that shared model (mergedName), which also reaches the
// owner's Copilot route under that name.
func routerTierGatewayName(model string, mergedName func(string) string) string {
	id, copilot := strings.CutPrefix(model, copilotTierPrefix)
	if copilot && mergedName != nil {
		if name := mergedName(id); name != "" {
			return name
		}
	}
	return id
}

// aiGatewayRouterReminderMarkers are the blocks a router leaves out when it
// classifies a request: OpenClaw's own context, the system reminders LiteLLM
// recognizes by default, and the label of the user message in which OpenClaw
// passes on the images of a tool result, such as a screenshot of the computer
// tool, to a model that accepts images: "Image(s) from tool result #1
// (computer):". Read as the newest ask, that label put a request for a design
// in the simple tier. The Gateway chart gives its own routers the same pairs.
var aiGatewayRouterReminderMarkers = []map[string]string{
	{"open": "<<<BEGIN_OPENCLAW_INTERNAL_CONTEXT>>>", "close": "<<<END_OPENCLAW_INTERNAL_CONTEXT>>>"},
	{"open": "<system-reminder>", "close": "</system-reminder>"},
	{"open": "Image(s) from tool result #", "close": ":"},
}

// modelRouterGatewayName is the internal model name of a Team's router. It
// stays the same while the router's configuration changes, so Agents keep
// the model they were configured with.
func modelRouterGatewayName(team, name string) string {
	sum := sha256.Sum256([]byte(team + "/" + name))
	return aiGatewayRouterPrefix + hex.EncodeToString(sum[:])[:16]
}

// modelRouterPlan is a router's desired registration, or why it has none.
type modelRouterPlan struct {
	router      api.TeamModelRouter
	gatewayName string
	request     aigateway.RouterRequest
	// problem is a configuration error that keeps the router out of the
	// Gateway; it names the router and what is wrong.
	problem string
	// shared are the shared models the router sends requests to: those its
	// tiers are registered as, and the model of an llm or jev classifier, a
	// chat model or a decision model that classifies the requests. copilot
	// are its copilot/<ID> tiers registered under the ID.
	shared  []string
	copilot []string
	// tierNames are the Gateway names the tier models are registered
	// with: a copilot/<ID> tier of a Copilot model that joins a shared model
	// is registered as that shared model, which is then one of shared.
	tierNames map[string]string
}

// tierModels are the shared models the router's tiers are registered as and
// its copilot/<ID> tiers registered under the ID, each sorted, for the Team's
// status: the Portal offers the router by the same rule the operator grants
// it.
func (plan modelRouterPlan) tierModels() ([]string, []string) {
	var shared, copilot []string
	for _, model := range plan.router.Tiers.Models() {
		id, isCopilot := strings.CutPrefix(model, copilotTierPrefix)
		switch {
		case !isCopilot:
			shared = append(shared, model)
		case plan.tierNames[model] != id:
			shared = append(shared, plan.tierNames[model])
		default:
			copilot = append(copilot, model)
		}
	}
	slices.Sort(shared)
	slices.Sort(copilot)
	return slices.Compact(shared), slices.Compact(copilot)
}

// modelRouterTierNames are the CRD's tier names, in tier order.
var modelRouterTierNames = []string{"simple", "medium", "complex", "reasoning"}

// The wording of router refusals, for a Team's routers and for the routers
// configured in the Gateway (ai_gateway_shared_routers.go) alike.
const modelRouterNotAllowed = "the AgentPolicy does not allow this router"

// modelRouterMisconfigured refuses a router whose configuration has the given
// problems.
func modelRouterMisconfigured(problems ...string) string {
	return "the router is misconfigured: " + strings.Join(problems, "; ")
}

// routerModelNames says which model a router names for a use, to begin a
// problem with it.
func routerModelNames(model aigateway.RouterModel) string {
	switch model.Use {
	case aigateway.RouterTier:
		return fmt.Sprintf("tier %s names %s", strings.ToLower(model.Tier), model.Name)
	case aigateway.RouterDefault:
		return fmt.Sprintf("the default model is %s", model.Name)
	case aigateway.RouterCompactor:
		return fmt.Sprintf("context compaction uses %s", model.Name)
	}
	return fmt.Sprintf("the classifier is %s", model.Name)
}

// routerModelNotShared is the problem of a router model that is not a shared
// model of the Gateway.
func routerModelNotShared(model aigateway.RouterModel) string {
	if model.Use == aigateway.RouterClassifier {
		return fmt.Sprintf("the classifier %s is not a shared model of the AI Gateway", model.Name)
	}
	return routerModelNames(model) + ", which is not a shared model of the AI Gateway"
}

// routerModelNotChat is the problem of a model a router sends requests to
// answer that answers no conversation, such as a decision model.
func routerModelNotChat(model aigateway.RouterModel) string {
	return routerModelNames(model) + ", which is not a chat model"
}

// routerModelNotDecision is the problem of a jev classifier that names a
// shared model that is not a decision model.
func routerModelNotDecision(model aigateway.RouterModel) string {
	return routerModelNames(model) + ", which is not a decision model (a shared model whose model_info.mode is evaluation)"
}

// routerModelIsARouter is the problem of a router model that is a router
// itself.
func routerModelIsARouter(model aigateway.RouterModel) string {
	if model.Use == aigateway.RouterClassifier {
		return fmt.Sprintf("the classifier %s is a router, not a model", model.Name)
	}
	return routerModelNames(model) + ", which is a router, not a model"
}

// routerModelNotAllowed refuses a router that uses a model the Agent's
// AgentPolicy does not allow.
func routerModelNotAllowed(model aigateway.RouterModel) string {
	uses := routerModelNames(model)
	if model.Use == aigateway.RouterTier {
		uses = fmt.Sprintf("tier %s uses %s", strings.ToLower(model.Tier), model.Name)
	}
	return uses + ", which the AgentPolicy does not allow"
}

// planModelRouters checks a Team's routers against the shared models of a
// listed catalog, as the view of the shared routers gives them, and builds
// their registrations.
func planModelRouters(team *api.Team, view aiGatewaySharedRouterView, mergedName func(string) string) []modelRouterPlan {
	routers := view.routerSet()
	plans := make([]modelRouterPlan, 0, len(team.Spec.ModelRouters))
	for _, router := range team.Spec.ModelRouters {
		plan := modelRouterPlan{router: router, gatewayName: modelRouterGatewayName(team.Name, router.Name), tierNames: map[string]string{}}
		var problems []string
		// Any shared model's name, whether it answers conversations or not.
		if view.shared[router.Name] {
			problems = append(problems, fmt.Sprintf("the name %s is also a shared model", router.Name))
		}
		for i, model := range router.Tiers.Models() {
			tier := modelRouterTierNames[i]
			use := aigateway.RouterModel{Use: aigateway.RouterTier, Tier: tier, Name: model}
			switch {
			case strings.HasPrefix(model, copilotTierPrefix):
				if !copilotModelID.MatchString(strings.TrimPrefix(model, copilotTierPrefix)) {
					problems = append(problems, fmt.Sprintf("tier %s names %s, which is not a GitHub Copilot model ID", tier, model))
					continue
				}
				name := routerTierGatewayName(model, mergedName)
				plan.tierNames[model] = name
				if name != strings.TrimPrefix(model, copilotTierPrefix) {
					// Joins a shared model: registered and granted as that model.
					if !slices.Contains(plan.shared, name) {
						plan.shared = append(plan.shared, name)
					}
				} else if !slices.Contains(plan.copilot, model) {
					plan.copilot = append(plan.copilot, model)
				}
			case routers[model]:
				// A router of the Gateway picks a model of its own for the request.
				problems = append(problems, routerModelIsARouter(use))
			case !view.shared[model]:
				problems = append(problems, routerModelNotShared(use))
			case view.nonChat[model]:
				problems = append(problems, routerModelNotChat(use))
			case !slices.Contains(plan.shared, model):
				plan.shared = append(plan.shared, model)
			}
		}
		classifier := router.Classifier.Type
		if classifier == "" {
			classifier = api.ModelRouterClassifierHeuristic
		}
		if classifier == api.ModelRouterClassifierLLM || classifier == api.ModelRouterClassifierJev {
			// A router as the classifier would send the request on to a model
			// of its own choice: the classifier call is made on the Agent's
			// behalf without its key's checks, so that model could be one the
			// Agent may not use.
			use := aigateway.RouterModel{Use: aigateway.RouterClassifier, Name: router.Classifier.Model}
			switch {
			case routers[use.Name]:
				problems = append(problems, routerModelIsARouter(use))
			case !view.shared[use.Name]:
				problems = append(problems, routerModelNotShared(use))
			case classifier == api.ModelRouterClassifierLLM && view.nonChat[use.Name]:
				// The llm classifier asks its model in a chat request.
				problems = append(problems, routerModelNotChat(use))
			case classifier == api.ModelRouterClassifierJev && !view.decision[use.Name]:
				// The jev classifier asks LiteLLM's Decisions API, which only a
				// decision model answers.
				problems = append(problems, routerModelNotDecision(use))
			case !slices.Contains(plan.shared, use.Name):
				plan.shared = append(plan.shared, use.Name)
			}
		}
		if len(problems) > 0 {
			plan.problem = fmt.Sprintf("router %s: %s", router.Name, strings.Join(problems, "; "))
			plans = append(plans, plan)
			continue
		}
		config := map[string]any{
			"classifier_type": classifier,
			"tiers": map[string]any{
				"SIMPLE": routerTierGatewayName(router.Tiers.Simple, mergedName), "MEDIUM": routerTierGatewayName(router.Tiers.Medium, mergedName),
				"COMPLEX": routerTierGatewayName(router.Tiers.Complex, mergedName), "REASONING": routerTierGatewayName(router.Tiers.ReasoningModel(), mergedName),
			},
			"reminder_markers":                 aiGatewayRouterReminderMarkers,
			"context_compaction":               false,
			"max_tokens_from_tier_model":       false,
			"enable_context_window_escalation": true,
		}
		switch classifier {
		case api.ModelRouterClassifierLLM:
			config["classifier_llm_config"] = map[string]any{"model": router.Classifier.Model}
		case api.ModelRouterClassifierJev:
			// The decision model, and nothing else: without a licence, LiteLLM
			// allows only one router per Gateway whose jev classifier sets its
			// own instructions, so the classifier keeps LiteLLM's instructions
			// and its other defaults.
			config["jev_classifier_config"] = map[string]any{"deployment_name": router.Classifier.Model}
			// A classification that fails, for example while the decision
			// model is unavailable, goes to the router's default model, its
			// medium tier. LiteLLM's default would classify the request with
			// its heuristic scorer instead, which placed 11 of 40 Chinese and
			// 15 of 40 English prompts on the expected tier in our tests.
			config["classifier_fallback"] = "default_model"
		}
		plan.request = aigateway.RouterRequest{
			ModelName: plan.gatewayName, AccessGroups: []string{plan.gatewayName}, Config: config,
			DefaultModel:      routerTierGatewayName(router.Tiers.Medium, mergedName),
			InputCostPerToken: aiGatewayRouterInputCost, OutputCostPerToken: aiGatewayRouterOutputCost,
		}
		// The ID changes with the configuration, so a changed router replaces
		// the registration instead of keeping the old one under the same ID.
		raw, _ := json.Marshal(aigateway.RouterBody(plan.request))
		sum := sha256.Sum256(raw)
		plan.request.ID = plan.gatewayName + "-" + hex.EncodeToString(sum[:])[:8]
		plans = append(plans, plan)
	}
	return plans
}

// aiGatewayRouterClient registers routers. The Gateway client implements it.
type aiGatewayRouterClient interface {
	AddRouter(context.Context, aigateway.RouterRequest) error
}

// routerDeployments returns the IDs of the router deployments the catalog
// lists, and whether the catalog was listed.
func (c *aiGatewayCatalog) routerDeployments() ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var ids []string
	for id := range c.ids {
		if strings.HasPrefix(id, aiGatewayRouterPrefix) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, c.known
}

// TeamModelRouterReconciler registers the model routers Teams define in the
// AI Gateway, removes the registrations no Team defines any more and reports
// each Team's routers on its status. It uses the Agent reconciler's Gateway
// client and catalog, and runs on the leader like every controller.
type TeamModelRouterReconciler struct {
	client.Client
	Agents   *AgentReconciler
	Interval time.Duration
}

// SetupWithManager registers the reconciler; it reconciles one Team at a
// time, since each pass also removes the registrations of every Team.
func (r *TeamModelRouterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("team-model-routers").
		For(&api.Team{}).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *TeamModelRouterReconciler) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return aiGatewayRouterInterval
}

// Reconcile brings the Gateway's router registrations in line with every
// Team's routers, then reports the requested Team's.
func (r *TeamModelRouterReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	team := &api.Team{}
	if err := r.Get(ctx, request.NamespacedName, team); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	} else if err != nil {
		team = nil
	}
	if !r.Agents.AIGateway.Enabled {
		if team == nil {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.report(ctx, team, nil, nil, "The AI Gateway is not enabled, so model routers are not registered")
	}
	gateway, err := r.Agents.aiGatewayClient(ctx)
	if err == nil {
		// Coalesce with the catalog publisher's and the Agents' listings; a
		// catalog older than twice that is not trusted to add or remove
		// registrations.
		r.Agents.refreshAIGatewayCatalog(ctx, gateway)
		if r.Agents.aiGatewayCatalog.age(time.Now()) > 2*aiGatewayCatalogMaxAge {
			err = fmt.Errorf("the AI Gateway's models could not be listed")
		}
	}
	if err != nil {
		if team != nil {
			_ = r.report(ctx, team, nil, nil, "The AI Gateway cannot be reached; model routers are checked again shortly")
		}
		return ctrl.Result{}, err
	}
	view := r.Agents.aiGatewayCatalog.sharedRouters()
	teams := &api.TeamList{}
	if err := r.List(ctx, teams); err != nil {
		return ctrl.Result{}, err
	}
	desired := map[string]modelRouterPlan{}
	teamOf := map[string]string{}
	var requested []modelRouterPlan
	for i := range teams.Items {
		item := &teams.Items[i]
		if !item.DeletionTimestamp.IsZero() {
			continue
		}
		plans := planModelRouters(item, view, r.Agents.copilotTierSharedNames())
		if team != nil && item.Name == team.Name {
			requested = plans
		}
		for _, plan := range plans {
			if plan.problem == "" {
				desired[plan.request.ID] = plan
				teamOf[plan.request.ID] = item.Name
			}
		}
	}
	changedTeams, removed, failures := r.sync(ctx, gateway, desired, teamOf)
	if len(changedTeams) > 0 || len(removed) > 0 {
		// Agents whose routers changed take them up, or drop them, now
		// rather than at their next verification.
		r.Agents.notifyAIGatewayAgents(ctx, "a model router change", func(agent *api.Agent) bool {
			if changedTeams[agent.Spec.Team] {
				return true
			}
			if status := agent.Status.AIGateway; status != nil {
				for _, router := range status.ModelRouters {
					if removed[router.Model] {
						return true
					}
				}
			}
			return false
		})
	}
	if team != nil {
		if err := r.report(ctx, team, requested, failures, ""); err != nil {
			return ctrl.Result{}, err
		}
	}
	switch {
	case len(failures) > 0:
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	case team != nil && len(team.Spec.ModelRouters) > 0:
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}
	return ctrl.Result{}, nil
}

// sync deletes the router deployments no Team wants, then adds the missing
// ones. LiteLLM keeps one router per model name, and a changed router keeps
// its model name, so a replacement is added only once its predecessor is
// gone. It returns the Teams whose registrations changed, the model names
// whose registration was removed, and the failures by deployment ID.
func (r *TeamModelRouterReconciler) sync(ctx context.Context, gateway aiGatewayKeyClient, desired map[string]modelRouterPlan, teamOf map[string]string) (map[string]bool, map[string]bool, map[string]string) {
	logger := log.FromContext(ctx).WithName("model-routers")
	catalog := &r.Agents.aiGatewayCatalog
	changed := map[string]bool{}
	removed := map[string]bool{}
	failures := map[string]string{}
	existing, _ := catalog.routerDeployments()
	blocked := map[string]bool{}
	for _, id := range existing {
		if _, ok := desired[id]; ok {
			continue
		}
		if err := gateway.DeleteModel(ctx, id); err != nil {
			logger.Error(err, "removing a model router registration failed", "id", id)
			blocked[routerGatewayNameOf(id)] = true
			// Retried like a failed registration, also when no Team wants a
			// router any more and nothing else would bring the pass back.
			failures[id] = err.Error()
			continue
		}
		catalog.noteDeleted(id)
		removed[routerGatewayNameOf(id)] = true
		logger.Info("removed a model router registration", "id", id)
	}
	routers, ok := gateway.(aiGatewayRouterClient)
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		plan := desired[id]
		if present, _ := catalog.has(id); present {
			continue
		}
		switch {
		case !ok:
			failures[id] = "the AI Gateway client cannot register routers"
		case blocked[plan.gatewayName]:
			failures[id] = "the previous registration could not be removed"
		default:
			err := routers.AddRouter(ctx, plan.request)
			if err != nil {
				// An earlier add may have succeeded without being listed yet.
				if deleteErr := gateway.DeleteModel(ctx, id); deleteErr == nil {
					err = routers.AddRouter(ctx, plan.request)
				}
			}
			if err != nil {
				logger.Error(err, "registering a model router failed", "router", plan.router.Name, "team", teamOf[id])
				failures[id] = err.Error()
				continue
			}
			catalog.noteAdded(id, plan.request.AccessGroups)
			changed[teamOf[id]] = true
			logger.Info("registered a model router", "router", plan.router.Name, "team", teamOf[id], "id", id)
		}
	}
	return changed, removed, failures
}

// routerGatewayNameOf is the model name part of a router deployment ID.
func routerGatewayNameOf(id string) string {
	if i := strings.LastIndex(id, "-"); i > len(aiGatewayRouterPrefix) {
		return id[:i]
	}
	return id
}

// report writes the Team's router status and its ModelRoutersReady
// condition. unavailable, when set, explains why nothing could be checked.
func (r *TeamModelRouterReconciler) report(ctx context.Context, team *api.Team, plans []modelRouterPlan, failures map[string]string, unavailable string) error {
	before := team.DeepCopy()
	if len(team.Spec.ModelRouters) == 0 {
		team.Status.ModelRouters = nil
		apimeta.RemoveStatusCondition(&team.Status.Conditions, api.ConditionTeamModelRoutersReady)
		return r.patchStatus(ctx, before, team)
	}
	var statuses []api.TeamModelRouterStatus
	var problems []string
	if unavailable != "" {
		for _, router := range team.Spec.ModelRouters {
			statuses = append(statuses, api.TeamModelRouterStatus{
				Name: router.Name, GatewayModel: modelRouterGatewayName(team.Name, router.Name), Message: unavailable,
			})
		}
	}
	for _, plan := range plans {
		status := api.TeamModelRouterStatus{Name: plan.router.Name, GatewayModel: plan.gatewayName, Ready: true}
		switch {
		case plan.problem != "":
			status.Ready, status.Message = false, plan.problem
		case failures[plan.request.ID] != "":
			status.Ready = false
			status.Message = fmt.Sprintf("router %s: registration in the AI Gateway failed: %s", plan.router.Name, failures[plan.request.ID])
		}
		if !status.Ready {
			problems = append(problems, status.Message)
		} else {
			status.SharedModels, status.CopilotModels = plan.tierModels()
		}
		statuses = append(statuses, status)
	}
	team.Status.ModelRouters = statuses
	condition := metav1.Condition{
		Type: api.ConditionTeamModelRoutersReady, Status: metav1.ConditionTrue, Reason: "Registered",
		Message: fmt.Sprintf("%d model routers are registered in the AI Gateway", len(statuses)), ObservedGeneration: team.Generation,
	}
	switch {
	case unavailable != "":
		condition.Status, condition.Reason, condition.Message = metav1.ConditionUnknown, "GatewayUnavailable", unavailable
	case len(problems) > 0:
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "RouterNotRegistered", strings.Join(problems, ". ")
	}
	apimeta.SetStatusCondition(&team.Status.Conditions, condition)
	return r.patchStatus(ctx, before, team)
}

func (r *TeamModelRouterReconciler) patchStatus(ctx context.Context, before, team *api.Team) error {
	if reflect.DeepEqual(before.Status, team.Status) {
		return nil
	}
	return r.Status().Patch(ctx, team, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// aiGatewayModelRouterGrant decides which of the Team's routers the Agent's
// key may use. A router is granted only when it is registered with its
// current configuration and the Agent may use every model it sends requests
// to: for a policy-bound Agent, the policy allows the router's name, each
// shared tier and, for GitHub Copilot tiers, Copilot; for every Agent, its
// shared models are on for shared tiers and the classifier, and each Copilot
// tier is one of its owner's registered Copilot models. The classifier, a
// chat model for llm or a decision model for jev, is a shared model the
// router calls on the Agent's behalf; it need not be in the policy's list.
// copilot reports whether the Agent's Copilot models are granted at all.
func (r *AgentReconciler) aiGatewayModelRouterGrant(ctx context.Context, client aiGatewayKeyClient, instance *api.Agent, team *api.Team, policy *api.AgentPolicy, copilot bool) ([]string, []api.AIGatewayModelRouterStatus) {
	if team == nil || len(team.Spec.ModelRouters) == 0 {
		return nil, nil
	}
	if client != nil {
		r.refreshAIGatewayCatalog(ctx, client)
	}
	shared, _ := r.aiGatewayCatalog.shared()
	if _, listed := r.aiGatewayCatalog.has(""); !listed {
		// Nothing can be checked without a listed catalog (a seeded one
		// names shared and decision models only): grant no router until
		// there is one.
		return nil, nil
	}
	// The Gateway name of each of the owner's Copilot models, by copilot/<ID>.
	registeredCopilot := map[string]string{}
	var routes []api.AIGatewayCopilotRoute
	if status := instance.Status.AIGateway; status != nil && status.Copilot != nil && copilot {
		for _, route := range status.Copilot.Routes {
			registeredCopilot[copilotTierPrefix+route.Model] = route.Name
		}
		routes = copilotRoutes(status.Copilot)
	}
	var groups []string
	var statuses []api.AIGatewayModelRouterStatus
	for _, plan := range planModelRouters(team, r.aiGatewayCatalog.sharedRouters(), r.copilotTierSharedNames()) {
		status := api.AIGatewayModelRouterStatus{Name: plan.router.Name}
		status.Reason = r.modelRouterRefusal(instance, plan, policy, copilot, registeredCopilot)
		if status.Reason == "" {
			status.Granted, status.Model = true, plan.gatewayName
			status.ContextWindow, status.MaxTokens = modelRouterLimits(instance, plan, shared, routes)
			status.Vision = modelRouterVision(plan, shared, routes)
			groups = append(groups, plan.gatewayName)
		}
		statuses = append(statuses, status)
	}
	return groups, statuses
}

// modelRouterRefusal is why the Agent may not use the router, or "".
func (r *AgentReconciler) modelRouterRefusal(instance *api.Agent, plan modelRouterPlan, policy *api.AgentPolicy, copilot bool, registeredCopilot map[string]string) string {
	if plan.problem != "" {
		return modelRouterMisconfigured(strings.TrimPrefix(plan.problem, "router "+plan.router.Name+": "))
	}
	if policy != nil && !slices.Contains(policy.Spec.ModelAccess.SharedModels.Allowed, plan.router.Name) {
		return modelRouterNotAllowed
	}
	if len(plan.shared) > 0 && !instance.Spec.SharedModelsEnabled() {
		return "the router uses shared models, which this Agent has turned off"
	}
	for i, model := range plan.router.Tiers.Models() {
		tier := modelRouterTierNames[i]
		if id, isCopilot := strings.CutPrefix(model, copilotTierPrefix); isCopilot {
			if name := plan.tierNames[model]; name != id {
				// The tier joins the shared model name: the Agent needs that
				// shared model, which its own Copilot route then joins too.
				if policy != nil && !slices.Contains(policy.Spec.ModelAccess.SharedModels.Allowed, name) {
					return fmt.Sprintf("tier %s uses the GitHub Copilot model %s, which the AI Gateway serves as the shared model %s, and the AgentPolicy does not allow %s", tier, id, name, name)
				}
				continue
			}
			name, registered := registeredCopilot[model]
			switch {
			case !copilot:
				return fmt.Sprintf("tier %s uses the GitHub Copilot model %s, and GitHub Copilot is not available to this Agent", tier, id)
			case !registered:
				return fmt.Sprintf("tier %s uses the GitHub Copilot model %s: sign in to GitHub Copilot with an account that offers it", tier, id)
			case name != id:
				// The router reaches the model under its ID; this Agent's key
				// reaches it only under another name.
				return fmt.Sprintf("tier %s uses the GitHub Copilot model %s, which this Agent reaches only as %s", tier, id, name)
			}
			continue
		}
		if policy != nil && !slices.Contains(policy.Spec.ModelAccess.SharedModels.Allowed, model) {
			return routerModelNotAllowed(aigateway.RouterModel{Use: aigateway.RouterTier, Tier: tier, Name: model})
		}
	}
	if present, known := r.aiGatewayCatalog.has(plan.request.ID); !known || !present {
		return "the router is not registered in the AI Gateway yet"
	}
	return ""
}

// modelRouterLimits are the smallest context window and output limit among
// the router's tier models that are known, so that a request the Agent sizes
// for the router fits whichever tier it goes to. A tier counts as the Gateway
// name it is registered as, a copilot/<ID> tier's shared model or ID, and the
// Agent's requests for that name may reach its own Copilot routes registered
// under it as well as the shared deployments: the tier fits within the
// smaller limits of both, as the Agent's entry for that model does.
func modelRouterLimits(instance *api.Agent, plan modelRouterPlan, shared []aiGatewayCatalogModel, routes []api.AIGatewayCopilotRoute) (int64, int64) {
	limits := map[string][2]int64{}
	for _, model := range shared {
		limits[model.Name] = [2]int64{model.ContextWindow, model.MaxOutputTokens}
	}
	copilot := aiGatewayCopilotRouteLimits(instance, routes)
	var contextWindow, maxTokens int64
	for _, model := range plan.router.Tiers.Models() {
		if name, ok := plan.tierNames[model]; ok {
			model = name
		}
		for _, limit := range [][2]int64{limits[model], copilot[model]} {
			contextWindow = smallerPositive(contextWindow, limit[0])
			maxTokens = smallerPositive(maxTokens, limit[1])
		}
	}
	return contextWindow, maxTokens
}

// modelRouterVision reports whether the Agent can send images to the router:
// whether every tier accepts them. A tier counts as the Gateway name it is
// registered as, as for the limits: the shared model of that name, when it is
// one, and the Agent's own Copilot routes registered under it, when it has
// any, must all accept images. A tier that is neither does not. The router
// does not look at images when it picks a tier, and a model that refuses
// them fails the request.
func modelRouterVision(plan modelRouterPlan, shared []aiGatewayCatalogModel, routes []api.AIGatewayCopilotRoute) bool {
	sharedVision := map[string]bool{}
	for _, model := range shared {
		sharedVision[model.Name] = model.Vision
	}
	routeVision := aiGatewayCopilotRouteVision(routes)
	for _, model := range plan.router.Tiers.Models() {
		if name, ok := plan.tierNames[model]; ok {
			model = name
		}
		fromShared, isShared := sharedVision[model]
		fromRoutes, isRoute := routeVision[model]
		if !isShared && !isRoute || isShared && !fromShared || isRoute && !fromRoutes {
			return false
		}
	}
	return true
}

// aiGatewayAgentTeam reads the Agent's Team for its routers; nil without one.
func (r *AgentReconciler) aiGatewayAgentTeam(ctx context.Context, instance *api.Agent) (*api.Team, error) {
	if instance.Spec.Team == "" {
		return nil, nil
	}
	team := &api.Team{}
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Spec.Team}, team); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return team, nil
}

// aiGatewayPolicySharedModels leaves the names of model routers out of a
// policy's allowed models: a router is granted by its own checks, never
// resolved as a shared model. A name some Team defines as a router counts as
// a router unless it is also a shared model Agents can hold a conversation
// with, which makes that router misconfigured instead. A router named like a
// shared model that answers no conversation, such as a decision model, is
// misconfigured too, but its name still counts as a router here: a policy
// that lists the name grants neither the router nor the model.
func (r *AgentReconciler) aiGatewayPolicySharedModels(ctx context.Context, models []string) ([]string, error) {
	teams := &api.TeamList{}
	if err := r.List(ctx, teams); err != nil {
		return nil, err
	}
	routers := map[string]bool{}
	for _, team := range teams.Items {
		for _, router := range team.Spec.ModelRouters {
			routers[router.Name] = true
		}
	}
	if len(routers) == 0 {
		return models, nil
	}
	shared, _ := r.aiGatewayCatalog.shared()
	sharedNames := map[string]bool{}
	for _, model := range shared {
		sharedNames[model.Name] = true
	}
	return slices.DeleteFunc(slices.Clone(models), func(name string) bool {
		return routers[name] && !sharedNames[name]
	}), nil
}

// aiGatewayRouterEntries are the Agent's granted Team routers as configured
// models: the name the Agent sends and the router's own name to show. A
// granted router configured in the Gateway is a shared model, which the Agent
// is configured with as such.
//
// The Agent sends the router's internal name, or, for a Hermes Agent whose
// key has the router's alias, the router's name. Hermes shows a model by the
// name it sends. OpenClaw shows the router's name in any case, and keeps the
// internal name, which its sessions hold.
func aiGatewayRouterEntries(instance *api.Agent) []aiGatewayModelEntry {
	status := instance.Status.AIGateway
	if status == nil {
		return nil
	}
	hermes := instance.Spec.Agent.Type == api.AgentTypeHermes
	var entries []aiGatewayModelEntry
	for _, router := range status.ModelRouters {
		if router.Granted && strings.HasPrefix(router.Model, aiGatewayRouterPrefix) {
			id := router.Model
			if hermes && router.Alias != "" {
				id = router.Alias
			}
			entries = append(entries, aiGatewayModelEntry{
				ID: id, Name: router.Name, ContextWindow: router.ContextWindow, MaxTokens: router.MaxTokens, Vision: router.Vision,
			})
		}
	}
	return entries
}

// aiGatewayRouterDefault is the name the Agent sends for the granted router
// it names as its model, by the router's name or its internal name.
func aiGatewayRouterDefault(instance *api.Agent) string {
	model := strings.TrimSpace(instance.Spec.Model)
	if model == "" {
		return ""
	}
	entries := aiGatewayRouterEntries(instance)
	if len(entries) == 0 {
		return ""
	}
	for _, entry := range entries {
		if entry.Name == model || entry.ID == model {
			return entry.ID
		}
	}
	// The internal name of a router the Agent is configured with under its
	// alias.
	for _, router := range instance.Status.AIGateway.ModelRouters {
		if router.Granted && router.Model == model {
			for _, entry := range entries {
				if entry.Name == router.Name {
					return entry.ID
				}
			}
		}
	}
	return ""
}

// aiGatewayRouterRevision is the part of the Agent's model configuration the
// routers contribute beyond their names in status.models: their names, limits
// and whether they accept images, so a change to any rolls the configuration.
func aiGatewayRouterRevision(instance *api.Agent) string {
	var parts []string
	for _, entry := range aiGatewayRouterEntries(instance) {
		part := fmt.Sprintf("%s=%s:%d:%d", entry.ID, entry.Name, entry.ContextWindow, entry.MaxTokens)
		if entry.Vision {
			part += ":vision"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	return "routers:" + strings.Join(parts, ",")
}

// refreshAIGatewayRouterGrant grants or withdraws routers whose GitHub
// Copilot tiers changed with the Copilot registration that followed the key
// update in the same pass: a sign-in grants them without waiting for the
// next verification, and a lost Copilot model withdraws them. The Gateway
// image keeps a router's Copilot tier on the calling Agent's own routes
// either way.
func (r *AgentReconciler) refreshAIGatewayRouterGrant(ctx context.Context, client aiGatewayKeyClient, instance *api.Agent, grant aiGatewayGrant) error {
	status := instance.Status.AIGateway
	if status == nil || !grant.routersEvaluated {
		return nil
	}
	groups, routers := r.aiGatewayModelRouterGrant(ctx, client, instance, grant.team, grant.policy, grant.copilot)
	routers = withSharedRouterStatuses(routers, r.currentSharedRouterStatuses(instance, grant.sharedRouters))
	if slices.Equal(groups, grant.routerGroups) {
		status.ModelRouters = routers
		return nil
	}
	secret, err := r.aiGatewayKeySecret(ctx, instance)
	if err != nil || secret == nil {
		return errors.Join(err, fmt.Errorf("the AI Gateway key Secret is unavailable"))
	}
	token := aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))
	desired := slices.DeleteFunc(slices.Clone(status.AccessGroups), func(group string) bool {
		return strings.HasPrefix(group, aiGatewayRouterPrefix)
	})
	desired = append(desired, groups...)
	slices.Sort(desired)
	desired = slices.Compact(desired)
	models := aiGatewayKeyModels(desired)
	if err := client.UpdateKeyModels(ctx, token, models); err != nil {
		return err
	}
	key, err := client.GetKey(ctx, token)
	if err != nil {
		return err
	}
	if key == nil || !slices.Equal(sortedCopy(key.Models), models) {
		return fmt.Errorf("the AI Gateway key does not match its model routers")
	}
	status.AccessGroups = desired
	status.ModelRouters = routers
	return nil
}
