// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// Fixed compiler budgets reject excess rather than truncate controls. Input
// entries count every supplied CIDR, peer/destination, IP, port, identity, grant,
// RBAC rule and rule string, including duplicates, before normalization.
// Partition work counts prefix visits, candidate comparisons and copied ports.
// Expanded rule entries count CIDRs/exclusions, selectors/labels/expressions,
// entities, port groups and port ranges across allow and deny rules.
// The byte limit covers the complete serialized CNP, including its final
// revision annotation, not just spec. These are compiler limits, not claims
// about any Cilium version's acceptance or dataplane capacity.
const (
	MaxControlInputEntries  = 1024
	MaxNetworkPartitionWork = 32768
	MaxNetworkRules         = 2048
	MaxNetworkRuleEntries   = 16384
	MaxNetworkPolicyBytes   = 512 * 1024
)

func consumeBudget(remaining *int, count int, name string) error {
	if count > *remaining {
		return fmt.Errorf("%w: %s budget exceeded", ErrConflict, name)
	}
	*remaining -= count
	return nil
}

func validateControlInput(policy api.AgentPolicySpec, platform Platform) error {
	remaining := MaxControlInputEntries
	take := func(count int) error { return consumeBudget(&remaining, count, "control input") }
	network, topology := policy.Network, platform.Topology
	for _, count := range []int{
		len(network.BlockedCIDRs), len(network.BlockedFQDNs), len(network.BlockedPorts), len(network.AllowedInternalCIDRs),
		len(topology.PodCIDRs), len(topology.ServiceCIDRs), len(topology.NodeCIDRs),
		len(topology.ManagementCIDRs), len(topology.MetadataCIDRs),
		len(topology.DNSResolverIPs),
		len(platform.Identities), len(platform.Grants),
	} {
		if err := take(count); err != nil {
			return err
		}
	}
	for _, rule := range network.AllowedInternalCIDRs {
		if err := take(len(rule.Ports)); err != nil {
			return err
		}
	}
	for _, peers := range [][]Peer{topology.DNS, topology.Peers} {
		if err := take(len(peers)); err != nil {
			return err
		}
		for _, peer := range peers {
			if err := take(len(peer.IPs) + len(peer.PodSelector)); err != nil {
				return err
			}
		}
	}
	for _, destinations := range [][]Destination{topology.Gateway, topology.Ingress, topology.KubernetesAPI} {
		if err := take(len(destinations)); err != nil {
			return err
		}
		for _, destination := range destinations {
			if err := take(len(destination.Peer.IPs) + len(destination.Peer.PodSelector) + len(destination.TrustedProxyCIDRs)); err != nil {
				return err
			}
			if err := take(len(destination.Ports)); err != nil {
				return err
			}
		}
	}
	if access := policy.KubernetesAccess; access != nil {
		if err := take(len(access.RoleBindings)); err != nil {
			return err
		}
		if err := take(len(access.ClusterRoleBindings)); err != nil {
			return err
		}
	}
	for _, grant := range platform.Grants {
		if err := take(len(grant.Rules)); err != nil {
			return err
		}
		for _, rule := range grant.Rules {
			for _, values := range [][]string{rule.APIGroups, rule.Resources, rule.ResourceNames, rule.NonResourceURLs, rule.Verbs} {
				if err := take(len(values)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type networkRuleBudget struct {
	rules, entries int
}

func (b *networkRuleBudget) take(entries int) error {
	if b.rules == MaxNetworkRules {
		return fmt.Errorf("%w: network rule count exceeds %d", ErrConflict, MaxNetworkRules)
	}
	if entries > MaxNetworkRuleEntries-b.entries {
		return fmt.Errorf("%w: expanded network rule entries exceed %d", ErrConflict, MaxNetworkRuleEntries)
	}
	b.rules++
	b.entries += entries
	return nil
}

func (b *networkRuleBudget) egress(target *[]EgressRule, rules ...EgressRule) error {
	for _, rule := range rules {
		if err := b.take(networkRuleEntries(rule.ToCIDRSet, rule.ToEndpoints, rule.ToEntities, rule.ToPorts)); err != nil {
			return err
		}
		*target = append(*target, rule)
	}
	return nil
}

func (b *networkRuleBudget) ingress(target *[]IngressRule, rules ...IngressRule) error {
	for _, rule := range rules {
		if err := b.take(networkRuleEntries(rule.FromCIDRSet, rule.FromEndpoints, rule.FromEntities, rule.ToPorts)); err != nil {
			return err
		}
		*target = append(*target, rule)
	}
	return nil
}

func networkRuleEntries(cidrs []CIDRRule, selectors []metav1.LabelSelector, entities []string, ports []PortRule) int {
	count := len(cidrs) + len(selectors) + len(entities) + len(ports)
	for _, cidr := range cidrs {
		count += len(cidr.Except)
	}
	for _, selector := range selectors {
		count += len(selector.MatchLabels) + len(selector.MatchExpressions)
		for _, expression := range selector.MatchExpressions {
			count += len(expression.Values)
		}
	}
	for _, rule := range ports {
		count += len(rule.Ports)
	}
	return count
}

func validateNetworkPolicySize(policy CiliumNetworkPolicy) error {
	data, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("encode CiliumNetworkPolicy size: %w", err)
	}
	if len(data) > MaxNetworkPolicyBytes {
		return fmt.Errorf("%w: CiliumNetworkPolicy %q is %d bytes; limit is %d", ErrConflict, policy.Name, len(data), MaxNetworkPolicyBytes)
	}
	return nil
}
