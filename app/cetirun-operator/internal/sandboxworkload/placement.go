// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"errors"
	"slices"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const MaxPlacementNodes = 256

// PlacementBinding is an immutable identity snapshot for one Agent/HOME
// lifetime. It is not evidence of exclusive placement, fencing or execution.
// Fresh resource versions belong in Expectation.Sources, not in this binding.
type PlacementBinding struct {
	Registry  ObjectIdentity  `json:"registry"`
	Agent     ObjectIdentity  `json:"agent"`
	Namespace ObjectIdentity  `json:"namespace"`
	Workload  ObjectIdentity  `json:"workload"`
	Home      ObjectIdentity  `json:"home"`
	Nodes     []PlacementNode `json:"nodes"`
}

type PlacementNode struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	ProviderID string    `json:"providerID"`
}

func (b *PlacementBinding) DeepCopy() *PlacementBinding {
	if b == nil {
		return nil
	}
	copy := *b
	copy.Nodes = slices.Clone(b.Nodes)
	return &copy
}

func (b *PlacementBinding) Validate() error {
	if b == nil {
		return errors.New("placement binding is required")
	}
	for _, identity := range []ObjectIdentity{b.Registry, b.Agent, b.Namespace, b.Workload, b.Home} {
		if len(validation.IsDNS1123Subdomain(identity.Name)) != 0 || !placementValue(string(identity.UID), 128) {
			return errors.New("placement binding requires complete registry, Agent, namespace, StatefulSet and HOME identities")
		}
	}
	if len(validation.IsDNS1123Label(b.Namespace.Name)) != 0 {
		return errors.New("placement binding requires a valid namespace identity")
	}
	if len(b.Nodes) == 0 || len(b.Nodes) > MaxPlacementNodes {
		return errors.New("placement binding requires between 1 and 256 Nodes")
	}
	uids, providers := map[types.UID]bool{}, map[string]bool{}
	for i, node := range b.Nodes {
		if len(validation.IsDNS1123Subdomain(node.Name)) != 0 ||
			!placementValue(string(node.UID), 128) || !placementValue(node.ProviderID, 2048) ||
			uids[node.UID] || providers[node.ProviderID] || (i > 0 && b.Nodes[i-1].Name >= node.Name) {
			return errors.New("placement Nodes require complete, unique identities in increasing name order")
		}
		uids[node.UID], providers[node.ProviderID] = true, true
	}
	return nil
}

// MatchesExpectation checks only the asserted identity tuple and any explicit
// template destination. The caller must independently verify live ownership.
func (b *PlacementBinding) MatchesExpectation(expected Expectation) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if b.Agent != expected.Agent || b.Namespace != expected.Namespace ||
		b.Workload != expected.Workload || b.Home != expected.Home {
		return errors.New("placement binding differs from the expected Agent, namespace, StatefulSet or HOME identity")
	}
	if expected.Template.Spec.NodeName != "" && !b.containsNode(expected.Template.Spec.NodeName) {
		return errors.New("expected destination is outside the recorded placement binding")
	}
	return nil
}

func (b *PlacementBinding) containsNode(name string) bool {
	return slices.ContainsFunc(b.Nodes, func(node PlacementNode) bool { return node.Name == name })
}

func placementValue(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for _, c := range value {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}
