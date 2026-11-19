// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
)

// Routers configured in the Gateway itself, such as the auto routers of the
// Gateway chart's models: shared models that are LiteLLM auto routers. The
// Gateway serves such a router, and through it every model the router sends
// requests to, to any key granted the router's name, so the operator grants
// one only under the rule of a Team's routers (ai_gateway_routers.go): the
// Agent must be able to use every model the router uses. The rule reads the
// router's configuration from the Gateway's model list, never from the
// AgentPolicy: a router change is applied to keys in place, and is not part
// of an AgentPolicy's Gateway revision, so it does not fence any Agent.

// mergeRouters combines the router deployments behind one model name.
func mergeRouters(a, b aigateway.Router) aigateway.Router {
	merged := aigateway.Router{Models: slices.Clone(a.Models), Unverifiable: a.Unverifiable}
	for _, model := range b.Models {
		if !slices.Contains(merged.Models, model) {
			merged.Models = append(merged.Models, model)
		}
	}
	if b.Unverifiable != "" && !strings.Contains(merged.Unverifiable, b.Unverifiable) {
		merged.Unverifiable = strings.TrimPrefix(merged.Unverifiable+"; "+b.Unverifiable, "; ")
	}
	return merged
}

// aiGatewayRouterFingerprint identifies the shared routers and the models
// each sends requests to, whatever order the Gateway lists deployments in. It
// is empty without routers, so a Gateway without them changes nothing.
func aiGatewayRouterFingerprint(routers map[string]aigateway.Router) string {
	if len(routers) == 0 {
		return ""
	}
	canonical := make(map[string]aigateway.Router, len(routers))
	for name, router := range routers {
		models := slices.Clone(router.Models)
		slices.SortFunc(models, func(a, b aigateway.RouterModel) int {
			return strings.Compare(string(a.Use)+"\x00"+a.Tier+"\x00"+a.Name, string(b.Use)+"\x00"+b.Tier+"\x00"+b.Name)
		})
		reasons := strings.Split(router.Unverifiable, "; ")
		slices.Sort(reasons)
		canonical[name] = aigateway.Router{Models: models, Unverifiable: strings.Join(reasons, "; ")}
	}
	// encoding/json writes map keys in sorted order.
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// aiGatewaySharedRouterView is one listing's shared routers, with what the
// rule reads besides them: the shared models, and each router name's managed
// groups. The rule of a Team's routers (planModelRouters) reads the same view.
type aiGatewaySharedRouterView struct {
	known   bool
	routers map[string]aigateway.Router
	shared  map[string]bool
	// nonChat are the shared models that answer no conversation: a router
	// may classify requests with one, but not send it requests to answer.
	nonChat map[string]bool
	// decision are the shared models that are decision models, which a
	// Team router's jev classifier may name; they are among nonChat.
	decision map[string]bool
	groups   map[string][]string
}

// sharedRouters returns the shared routers of the last listing. It knows none
// without a listing: a catalog seeded from the published one names shared and
// decision models only.
func (c *aiGatewayCatalog) sharedRouters() aiGatewaySharedRouterView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// update replaces c.routers, c.nonChat and c.decision rather than
	// changing them, so they can be shared.
	view := aiGatewaySharedRouterView{known: c.known, routers: c.routers, shared: map[string]bool{}, nonChat: c.nonChat,
		decision: c.decision, groups: map[string][]string{}}
	for _, model := range c.sharedSet {
		view.shared[model.Name] = true
	}
	for name := range c.routers {
		view.groups[name] = slices.Clone(c.modelGroups[name])
	}
	return view
}

// decisionModels are the decision models' names, in order, for the published
// catalog; nil without any.
func (v aiGatewaySharedRouterView) decisionModels() []string {
	var names []string
	for name, isDecision := range v.decision {
		if isDecision && v.shared[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// describe returns the shared models with the models each router among them
// sends requests to answer, so that the Portal offers a router only to Teams
// whose AgentPolicy lists all of them, as this rule grants it.
func (v aiGatewaySharedRouterView) describe(shared []aiGatewayCatalogModel) []aiGatewayCatalogModel {
	described := make([]aiGatewayCatalogModel, len(shared))
	for i, model := range shared {
		described[i] = model
		described[i].RouterModels, described[i].RouterUnverifiable = nil, false
		router, ok := v.routers[model.Name]
		if !ok {
			continue
		}
		names := []string{}
		for _, used := range router.Models {
			if used.Use != aigateway.RouterClassifier && !slices.Contains(names, used.Name) {
				names = append(names, used.Name)
			}
		}
		slices.Sort(names)
		described[i].RouterModels, described[i].RouterUnverifiable = names, router.Unverifiable != ""
	}
	return described
}

// routerSet are the shared routers' names, the models a Team's router may not
// use: a router is not a model.
func (v aiGatewaySharedRouterView) routerSet() map[string]bool {
	set := make(map[string]bool, len(v.routers))
	for name := range v.routers {
		set[name] = true
	}
	return set
}

// names are the shared routers' names, in order.
func (v aiGatewaySharedRouterView) names() []string {
	names := make([]string, 0, len(v.routers))
	for name := range v.routers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// refusal is why an Agent may not use the shared router name, or "" when it
// may: the one rule for routers configured in the Gateway. allowed holds the
// shared models an AgentPolicy allows; nil stands for an Agent without a
// policy, which may use every shared model.
//
// Each model the router sends a request to answer, in a tier, as its default
// model or to compact a conversation, must be a shared model the Agent may
// use: with a policy, one the policy lists, as it must list the router, and
// one that answers conversations. A
// classifier, which the router calls to classify the Agent's request, must be
// a shared model but need not be listed, as for a Team's routers, and may be
// one that answers none, such as a decision model; the Gateway
// does not hold a router's classifier calls to the calling key. A router whose
// configuration the operator cannot read completely is refused to Agents with
// a policy. An Agent without one may use it: it may use every shared model,
// and the Gateway refuses the requests a router sends to models outside the
// calling key's access groups. A model that is itself a router, or not a
// shared model, makes the router misconfigured, for every Agent.
func (v aiGatewaySharedRouterView) refusal(name string, allowed map[string]bool) string {
	if allowed != nil && !allowed[name] {
		return modelRouterNotAllowed
	}
	router := v.routers[name]
	var problems []string
	for _, model := range router.Models {
		if _, isRouter := v.routers[model.Name]; isRouter {
			problems = append(problems, routerModelIsARouter(model))
		} else if !v.shared[model.Name] {
			problems = append(problems, routerModelNotShared(model))
		} else if model.Use != aigateway.RouterClassifier && v.nonChat[model.Name] {
			problems = append(problems, routerModelNotChat(model))
		}
	}
	switch {
	case len(problems) > 0:
		return modelRouterMisconfigured(problems...)
	case allowed == nil:
		return ""
	case router.Unverifiable != "":
		return "the operator cannot check every model the router uses: " + router.Unverifiable
	}
	for _, model := range router.Models {
		if model.Use != aigateway.RouterClassifier && !allowed[model.Name] {
			return routerModelNotAllowed(model)
		}
	}
	return ""
}

// aiGatewaySharedRouterDecision is how the rule applies to one Agent.
type aiGatewaySharedRouterDecision struct {
	// refused are the routers the Agent may not use, and refusedGroups their
	// managed groups, which its key does not get.
	refused       map[string]bool
	refusedGroups []string
	// statuses report, for an Agent with a policy, each router the policy
	// lists.
	statuses []api.AIGatewayModelRouterStatus
}

// aiGatewaySharedRouterGrant applies the shared-router rule to the Agent with
// the given AgentPolicy, nil for an Agent without one, using the last
// listing.
func (r *AgentReconciler) aiGatewaySharedRouterGrant(instance *api.Agent, policy *api.AgentPolicy) aiGatewaySharedRouterDecision {
	decision := aiGatewaySharedRouterDecision{refused: map[string]bool{}}
	if !instance.Spec.SharedModelsEnabled() {
		return decision
	}
	view := r.aiGatewayCatalog.sharedRouters()
	if !view.known {
		return decision
	}
	var allowed map[string]bool
	if policy != nil {
		allowed = map[string]bool{}
		for _, name := range policy.Spec.ModelAccess.SharedModels.Allowed {
			allowed[name] = true
		}
	}
	limits := r.aiGatewaySharedRouterLimits(instance)
	vision := r.aiGatewaySharedRouterVision(instance)
	for _, name := range view.names() {
		reason := view.refusal(name, allowed)
		if reason != "" {
			decision.refused[name] = true
			decision.refusedGroups = append(decision.refusedGroups, view.groups[name]...)
		}
		if allowed[name] {
			status := api.AIGatewayModelRouterStatus{Name: name, Granted: reason == "", Reason: reason}
			if status.Granted {
				status.Model = name
				status.ContextWindow, status.MaxTokens = limits[name][0], limits[name][1]
				status.Vision = vision[name]
			}
			decision.statuses = append(decision.statuses, status)
		}
	}
	return decision
}

// aiGatewaySharedRouterLimits are the limits the Agent is configured with for
// each router configured in the Gateway, by name: those of the router's entry
// in the catalog (fillRouterLimits), lowered by the Agent's own Copilot routes
// registered under the names of the router's models
// (aiGatewayRouterLimitFolds), as in the Agent's configuration.
func (r *AgentReconciler) aiGatewaySharedRouterLimits(instance *api.Agent) map[string][2]int64 {
	shared, known := r.aiGatewayCatalog.shared()
	if !known {
		return nil
	}
	limits := make(map[string][2]int64, len(shared))
	for _, model := range shared {
		limits[model.Name] = [2]int64{model.ContextWindow, model.MaxOutputTokens}
	}
	for name, fold := range r.aiGatewayRouterLimitFolds(instance, shared, r.aiGatewayConfiguredCopilotRoutes(instance)) {
		limits[name] = fold
	}
	return limits
}

// aiGatewaySharedRouterVision reports, by name, whether the Agent is
// configured to send images to each router configured in the Gateway: the
// router's entry in the catalog accepts them (fillRouterVision), and none of
// the Agent's own Copilot routes under the names of the router's models
// refuses them (aiGatewayRouterVisionFolds), as in the Agent's configuration.
func (r *AgentReconciler) aiGatewaySharedRouterVision(instance *api.Agent) map[string]bool {
	shared, known := r.aiGatewayCatalog.shared()
	if !known {
		return nil
	}
	vision := make(map[string]bool, len(shared))
	for _, model := range shared {
		vision[model.Name] = model.Vision
	}
	for name, accepts := range r.aiGatewayRouterVisionFolds(shared, r.aiGatewayConfiguredCopilotRoutes(instance)) {
		vision[name] = accepts
	}
	return vision
}

// withoutGroups leaves the given groups out of groups.
func withoutGroups(groups, drop []string) []string {
	if len(drop) == 0 {
		return groups
	}
	return slices.DeleteFunc(slices.Clone(groups), func(group string) bool { return slices.Contains(drop, group) })
}

// currentSharedRouterStatuses gives the granted routers configured in the
// Gateway the limits and the image support that follow from the Agent's
// GitHub Copilot routes as they are registered now. The grant decided them
// before the Copilot registration of the same pass, which can add or remove
// a route under the name of a router's model.
func (r *AgentReconciler) currentSharedRouterStatuses(instance *api.Agent, statuses []api.AIGatewayModelRouterStatus) []api.AIGatewayModelRouterStatus {
	if len(statuses) == 0 {
		return statuses
	}
	limits, vision := r.aiGatewaySharedRouterLimits(instance), r.aiGatewaySharedRouterVision(instance)
	current := slices.Clone(statuses)
	for i := range current {
		if current[i].Granted {
			name := current[i].Name
			current[i].ContextWindow, current[i].MaxTokens = limits[name][0], limits[name][1]
			current[i].Vision = vision[name]
		}
	}
	return current
}

// withSharedRouterStatuses lists the shared routers' statuses after those of
// the Agent's Team. status.modelRouters is keyed by name, so a Team router
// that takes a shared router's name, which makes the Team router
// misconfigured, keeps its entry.
func withSharedRouterStatuses(team, shared []api.AIGatewayModelRouterStatus) []api.AIGatewayModelRouterStatus {
	statuses := slices.Clone(team)
	for _, status := range shared {
		if !slices.ContainsFunc(team, func(other api.AIGatewayModelRouterStatus) bool { return other.Name == status.Name }) {
			statuses = append(statuses, status)
		}
	}
	if len(statuses) == 0 {
		return nil
	}
	return statuses
}

// aiGatewaySharedRoutersStale reports whether the key or models of an Agent
// without a policy no longer follow the shared routers: a router it may not
// use is still on its key or among its models, or one it may use is not on
// its key. A changed router queues the Agents with shared models (the catalog
// counts it as a change), and this makes such an Agent take the full
// reconcile, which updates its key in place, rather than the lightweight
// pass. Its configuration hash, and so its workload, changes only when its
// models do. An Agent with a policy takes the full Gateway reconcile whenever
// it is queued.
func (r *AgentReconciler) aiGatewaySharedRoutersStale(instance *api.Agent) bool {
	status := instance.Status.AIGateway
	if status == nil || status.KeyID == "" || !instance.Spec.SharedModelsEnabled() || sandboxRequested(instance, nil) {
		return false
	}
	view := r.aiGatewayCatalog.sharedRouters()
	if !view.known {
		return false
	}
	for _, name := range view.names() {
		refused := view.refusal(name, nil) != ""
		if refused && slices.Contains(status.Models, name) {
			return true
		}
		for _, group := range view.groups[name] {
			if slices.Contains(status.AccessGroups, group) == refused {
				return true
			}
		}
	}
	return false
}
