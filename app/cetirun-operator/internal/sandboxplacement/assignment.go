// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"cmp"
	"context"
	"slices"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/api/equality"
)

// ReadAssignment snapshots exact reservations for the caller-verified Agent,
// Namespace, Workload and Home. Template may be zero for identity-only rechecks;
// its nodeName, when set, additionally asserts membership. HOME is not part of
// registry v1. This neither verifies controller ownership nor grants permission
// to start. The fresh registry source is separate from the stable lifetime binding.
func (h *Handler) ReadAssignment(ctx context.Context, expected sandboxworkload.Expectation) (*sandboxworkload.PlacementBinding, sandboxworkload.SourceReference, error) {
	ctx, cancel := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancel()
	state := &requestState{reader: h.reader}
	registry, failure := state.readRegistry(ctx, h.config)
	if failure != nil {
		return nil, sandboxworkload.SourceReference{}, failure
	}
	binding, failure := state.readAssignment(ctx, registry, expected)
	if failure != nil {
		return nil, sandboxworkload.SourceReference{}, failure
	}
	if failure := state.recheck(ctx); failure != nil {
		return nil, sandboxworkload.SourceReference{}, failure
	}
	return binding, state.registrySource, nil
}

func (s *requestState) readAssignment(ctx context.Context, registry *registry, expected sandboxworkload.Expectation) (*sandboxworkload.PlacementBinding, *failure) {
	nodes, failure := registry.assignmentNodes(expected)
	if failure != nil {
		return nil, failure
	}
	binding := &sandboxworkload.PlacementBinding{
		Registry: sandboxworkload.ObjectIdentity{Name: s.registrySource.Name, UID: s.registrySource.UID},
		Agent:    expected.Agent, Namespace: expected.Namespace, Workload: expected.Workload, Home: expected.Home, Nodes: nodes,
	}
	if err := binding.MatchesExpectation(expected); err != nil {
		return nil, denied("PlacementBindingInvalid", err.Error())
	}
	if expected.Placement != nil {
		if err := expected.Placement.MatchesExpectation(expected); err != nil {
			return nil, denied("PlacementBindingInvalid", err.Error())
		}
		if !equality.Semantic.DeepEqual(binding, expected.Placement) {
			return nil, denied("PlacementBindingChanged", "current registry identity or complete reservation differs from the recorded placement binding")
		}
	}
	for _, node := range nodes {
		if _, _, failure := s.readNode(ctx, registry, node.Name); failure != nil {
			return nil, failure
		}
	}
	return binding, nil
}

func (r *registry) assignmentNodes(expected sandboxworkload.Expectation) ([]sandboxworkload.PlacementNode, *failure) {
	want := nodeReservation{Agent: expected.Agent, Namespace: expected.Namespace, StatefulSet: expected.Workload}
	if !validIdentity(want.Agent) || !validIdentity(want.Namespace) || !validIdentity(want.StatefulSet) || !validIdentity(expected.Home) {
		return nil, denied("PlacementBindingInvalid", "placement assignment requires complete caller-verified Agent, namespace, StatefulSet and HOME identities")
	}
	var nodes []sandboxworkload.PlacementNode
	for _, node := range r.Nodes {
		reservation := node.Reservation
		if reservation == nil {
			continue
		}
		if *reservation == want {
			nodes = append(nodes, sandboxworkload.PlacementNode{Name: node.Name, UID: node.UID, ProviderID: node.ProviderID})
		} else if reservation.Agent.Name == want.Agent.Name || reservation.Agent.UID == want.Agent.UID ||
			reservation.Namespace.Name == want.Namespace.Name || reservation.Namespace.UID == want.Namespace.UID ||
			reservation.StatefulSet.UID == want.StatefulSet.UID {
			return nil, denied("AgentAssignmentAmbiguous", "registry contains conflicting Agent or namespace ownership assignments")
		}
	}
	if len(nodes) == 0 {
		return nil, denied("AgentAssignmentMissing", "registry has no reservation for the exact expected workload identities")
	}
	slices.SortFunc(nodes, func(a, b sandboxworkload.PlacementNode) int { return cmp.Compare(a.Name, b.Name) })
	return nodes, nil
}
