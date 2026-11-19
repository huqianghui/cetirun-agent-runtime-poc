// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

const (
	instanceSandboxIndex = "spec.sandboxRef"
	teamSandboxIndex     = "spec.sandboxRefs"
	teamPolicyIndex      = "sandbox.agentPolicyRefs"
)

func indexSandboxDependencies(ctx context.Context, indexer client.FieldIndexer) error {
	for _, index := range []struct {
		object  client.Object
		field   string
		extract client.IndexerFunc
	}{
		{&api.Agent{}, instanceSandboxIndex, agentSandboxReferences},
		{&api.Team{}, teamSandboxIndex, teamSandboxReferences},
		{&api.Team{}, teamPolicyIndex, teamPolicyReferences},
	} {
		if err := indexer.IndexField(ctx, index.object, index.field, index.extract); err != nil {
			return fmt.Errorf("index Sandbox dependency %s: %w", index.field, err)
		}
	}
	return nil
}

func agentSandboxReferences(object client.Object) []string {
	agent, ok := object.(*api.Agent)
	if !ok || agent.Spec.SandboxRef == "" {
		return nil
	}
	return []string{agent.Spec.SandboxRef}
}

func teamSandboxReferences(object client.Object) []string {
	team, ok := object.(*api.Team)
	if !ok {
		return nil
	}
	return uniqueReferences(append(slices.Clone(team.Spec.SandboxRefs), team.Spec.DefaultSandboxRef))
}

func teamPolicyReferences(object client.Object) []string {
	team, ok := object.(*api.Team)
	if !ok {
		return nil
	}
	return uniqueReferences([]string{team.Spec.AgentPolicyRef})
}

func uniqueReferences(names []string) []string {
	names = slices.DeleteFunc(names, func(name string) bool { return name == "" })
	slices.Sort(names)
	return slices.Compact(names)
}

func (r *AgentReconciler) enqueueInstancesForSandbox(ctx context.Context, object client.Object) []reconcile.Request {
	if _, ok := object.(*api.Sandbox); !ok {
		return nil
	}
	return r.enqueueInstancesForConfiguration(ctx, object.GetName(), teamSandboxIndex, true)
}

func (r *AgentReconciler) enqueueInstancesForAgentPolicy(ctx context.Context, object client.Object) []reconcile.Request {
	if _, ok := object.(*api.AgentPolicy); !ok {
		return nil
	}
	return r.enqueueInstancesForConfiguration(ctx, object.GetName(), teamPolicyIndex, false)
}

func (r *AgentReconciler) enqueueInstancesForConfiguration(ctx context.Context, name, teamIndex string, directSandbox bool) []reconcile.Request {
	names := make(map[string]struct{})
	if directSandbox {
		var agents api.AgentList
		if err := r.List(ctx, &agents, client.MatchingFields{instanceSandboxIndex: name}); err != nil {
			log.FromContext(ctx).Error(err, "list directly bound Agents for Sandbox watch", "sandbox", name)
		} else {
			for _, agent := range agents.Items {
				names[agent.Name] = struct{}{}
			}
		}
	}
	var teams api.TeamList
	if err := r.List(ctx, &teams, client.MatchingFields{teamIndex: name}); err != nil {
		log.FromContext(ctx).Error(err, "list Teams for Sandbox configuration watch", "configuration", name, "index", teamIndex)
	} else {
		for i := range teams.Items {
			for _, request := range r.enqueueInstancesForTeam(ctx, &teams.Items[i]) {
				names[request.Name] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	slices.Sort(ordered)
	requests := make([]reconcile.Request, 0, len(ordered))
	for _, name := range ordered {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	}
	return requests
}
