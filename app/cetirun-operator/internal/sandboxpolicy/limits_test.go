// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

func TestCompileRejectsWholeAddressSpaceTopology(t *testing.T) {
	for _, inventory := range []string{"node", "pod", "service"} {
		for _, root := range []string{"0.0.0.0/0", "::/0"} {
			t.Run(inventory+"/"+root, func(t *testing.T) {
				input := compilerFixture()
				switch inventory {
				case "node":
					input.Platform.Topology.NodeCIDRs = []string{root}
				case "pod":
					input.Platform.Topology.PodCIDRs = []string{root}
				case "service":
					input.Platform.Topology.ServiceCIDRs = []string{root}
				}
				plan, err := Compile(input)
				if plan != nil || !errors.Is(err, ErrConflict) {
					t.Fatalf("whole-family exclusions returned a plan: %v, %v", plan, err)
				}
				message := "internal CIDRs cover the entire " + root
				if inventory == "node" && root == "0.0.0.0/0" {
					message = "a required destination overlaps"
				}
				if !strings.Contains(err.Error(), message) {
					t.Fatalf("wrong rejection path: %v", err)
				}
			})
		}
	}
}

func TestControlInputBudgetThresholds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input func(int) (api.AgentPolicySpec, Platform)
	}{
		{"CIDRs", func(count int) (api.AgentPolicySpec, Platform) {
			return api.AgentPolicySpec{Network: api.AgentPolicyNetwork{BlockedCIDRs: make([]string, count)}}, Platform{}
		}},
		{"nested ports", func(count int) (api.AgentPolicySpec, Platform) {
			return api.AgentPolicySpec{Network: api.AgentPolicyNetwork{AllowedInternalCIDRs: []api.AgentPolicyCIDRRule{{
				Ports: make([]api.AgentPolicyPort, count-1),
			}}}}, Platform{}
		}},
		{"peer IPs", func(count int) (api.AgentPolicySpec, Platform) {
			return api.AgentPolicySpec{}, Platform{Topology: Topology{Peers: []Peer{{IPs: make([]string, count-1)}}}}
		}},
		{"peer selector labels", func(count int) (api.AgentPolicySpec, Platform) {
			selected := make(map[string]string, count-1)
			for i := range count - 1 {
				selected[fmt.Sprintf("label-%d", i)] = "gateway"
			}
			return api.AgentPolicySpec{}, Platform{Topology: Topology{Peers: []Peer{{PodSelector: selected}}}}
		}},
		{"ingress proxy CIDRs", func(count int) (api.AgentPolicySpec, Platform) {
			return api.AgentPolicySpec{}, Platform{Topology: Topology{Ingress: []Destination{{TrustedProxyCIDRs: make([]string, count-1)}}}}
		}},
		{"DNS resolver IPs", func(count int) (api.AgentPolicySpec, Platform) {
			return api.AgentPolicySpec{}, Platform{Topology: Topology{DNSResolverIPs: make([]string, count)}}
		}},
		{"role contents", func(count int) (api.AgentPolicySpec, Platform) {
			return api.AgentPolicySpec{}, Platform{Grants: []ApprovedGrant{{Rules: []rbacv1.PolicyRule{{Verbs: make([]string, count-2)}}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, count := range []int{MaxControlInputEntries - 1, MaxControlInputEntries, MaxControlInputEntries + 1} {
				policy, platform := tc.input(count)
				err := validateControlInput(policy, platform)
				if count <= MaxControlInputEntries && err != nil || count > MaxControlInputEntries && !errors.Is(err, ErrConflict) {
					t.Fatalf("input count %d: %v", count, err)
				}
			}
		})
	}
}

func TestCompileBoundsRawInputsBeforeNormalization(t *testing.T) {
	input := compilerFixture()
	// The fixture supplies five CIDRs, DNS (resolver+peer+IP), and three
	// destinations (peer+IP+port). Duplicates still consume the raw-input budget.
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = slices.Repeat([]string{"198.51.100.0/24"}, MaxControlInputEntries-17)
	mustCompile(t, input)
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = append(input.Resolution.AgentPolicy.Network.BlockedCIDRs, "198.51.100.0/24")
	if plan, err := Compile(input); plan != nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "control input") {
		t.Fatalf("raw duplicates bypassed the input budget: %v, %v", plan, err)
	}
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{UID: input.Resolution.Binding.AgentPolicyUID}, Spec: input.Resolution.AgentPolicy}
	if err := ValidatePolicyConfiguration(policy, input.Resolution.Capabilities, input.Platform); !errors.Is(err, ErrConflict) {
		t.Fatalf("standalone policy assessment bypassed the input budget: %v", err)
	}
	input.Platform.Topology.Peers = make([]Peer, MaxControlInputEntries+1)
	if err := ValidatePlatform(input.Platform, input.Resolution.Capabilities); !errors.Is(err, ErrConflict) {
		t.Fatalf("platform assessment bypassed the input budget: %v", err)
	}
}

func TestNetworkRuleBudgetThresholds(t *testing.T) {
	budget := networkRuleBudget{}
	var rules []EgressRule
	for i := 0; i < MaxNetworkRules; i++ {
		if err := budget.egress(&rules, EgressRule{ToEntities: []string{"world"}}); err != nil {
			t.Fatalf("rule %d within budget: %v", i, err)
		}
	}
	if err := budget.egress(&rules, EgressRule{ToEntities: []string{"world"}}); !errors.Is(err, ErrConflict) || len(rules) != MaxNetworkRules {
		t.Fatalf("rule limit did not reject before append: count=%d, %v", len(rules), err)
	}
	budget, rules = networkRuleBudget{}, nil
	if err := budget.egress(&rules, EgressRule{ToCIDRSet: []CIDRRule{{
		CIDR: "::/0", Except: make([]string, MaxNetworkRuleEntries-1),
	}}}); err != nil {
		t.Fatal("exact expanded-entry budget must be accepted", err)
	}
	var ingress []IngressRule
	if err := budget.ingress(&ingress, IngressRule{FromEntities: []string{"all"}}); !errors.Is(err, ErrConflict) || len(ingress) != 0 {
		t.Fatal("allow/deny and ingress/egress must share the expanded-entry budget", err)
	}
}

func TestNetworkPartitionWorkThresholds(t *testing.T) {
	capabilities := sandbox.Capabilities{IPFamilies: []corev1.IPFamily{corev1.IPv6Protocol}}
	for _, work := range []int{MaxNetworkPartitionWork - 1, MaxNetworkPartitionWork, MaxNetworkPartitionWork + 1} {
		// One root visit and one candidate comparison leave work-2 port copies.
		grants := []addressGrant{{
			Prefix: netip.MustParsePrefix("::/0"),
			Ports:  slices.Repeat([]api.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}}, work-2),
		}}
		sectors, err := partitionGrants(grants, capabilities)
		if work <= MaxNetworkPartitionWork {
			if err != nil || len(sectors) != 1 || len(sectors[0].Ports) != 1 {
				t.Fatalf("partition work %d within budget: %v", work, err)
			}
		} else if sectors != nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "partition work") {
			t.Fatalf("excess work returned partial sectors: %v, %v", sectors, err)
		}
	}
}

func TestCompileBoundsAdversarialOverlappingIPv6(t *testing.T) {
	for name, fixture := range map[string]func() CompileInput{
		"qualified": compilerFixture, "native": nativeCompilerFixture, "native SCTP": nativeSCTPCompilerFixture,
	} {
		t.Run(name, func(t *testing.T) {
			input := fixture()
			protocol := corev1.ProtocolTCP
			if name == "native SCTP" {
				protocol = corev1.ProtocolSCTP
			}
			for bits := 8; bits <= 128; bits++ {
				input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = append(input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs, api.AgentPolicyCIDRRule{
					CIDR: fmt.Sprintf("fd00::/%d", bits), Ports: []api.AgentPolicyPort{{Port: 443, Protocol: protocol}},
				})
			}
			if err := validateControlInput(input.Resolution.AgentPolicy, input.Platform); err != nil {
				t.Fatal("adversarial fixture must fit the input budget", err)
			}
			plan, err := Compile(input)
			if plan != nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "network partition work") {
				t.Fatalf("overlapping IPv6 recursion was not bounded: %v, %v", plan, err)
			}
		})
	}
}

func TestCompileBoundsExpandedPeerPorts(t *testing.T) {
	for name, fixture := range map[string]func() CompileInput{
		"qualified": compilerFixture, "native": nativeCompilerFixture, "native SCTP": nativeSCTPCompilerFixture,
	} {
		t.Run(name, func(t *testing.T) {
			input := fixture()
			protocol := corev1.ProtocolTCP
			if name == "native SCTP" {
				protocol = corev1.ProtocolSCTP
			}
			var ports []api.AgentPolicyPort
			for i := 0; i < 400; i++ {
				ports = append(ports, api.AgentPolicyPort{Port: int32(1000 + 2*i), Protocol: protocol})
			}
			input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "fd00::/8", Ports: ports}}
			for i := 1; i <= 140; i++ {
				input.Platform.Topology.Peers = append(input.Platform.Topology.Peers, Peer{
					Name: fmt.Sprintf("peer-%d", i), Namespace: "applications", UID: types.UID(fmt.Sprintf("example-peer-%d", i)),
					IPs: []string{fmt.Sprintf("fd00::%x", i)},
				})
			}
			if err := validateControlInput(input.Resolution.AgentPolicy, input.Platform); err != nil {
				t.Fatal("peer fixture must fit the input budget", err)
			}
			plan, err := Compile(input)
			if plan != nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "expanded network rule entries") {
				t.Fatalf("peer/port expansion was not bounded: %v, %v", plan, err)
			}
		})
	}
}

func TestCompileSerializedNetworkPolicyThresholds(t *testing.T) {
	input := compilerFixture()
	baseline := mustCompile(t, input)
	data, err := json.Marshal(baseline.Network.Policy)
	if err != nil {
		t.Fatal(err)
	}
	// Pad synthetic binding metadata, not spec, to exercise the complete CNP
	// check after the final revision annotation has been attached.
	length := len(input.Resolution.Binding.TeamUID) + MaxNetworkPolicyBytes - len(data)
	for _, delta := range []int{-1, 0, 1} {
		input.Resolution.Binding.TeamUID = types.UID(strings.Repeat("a", length+delta))
		plan, err := Compile(input)
		if delta > 0 {
			if plan != nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "bytes; limit") {
				t.Fatalf("oversized serialized policy returned a plan: %v, %v", plan, err)
			}
			continue
		}
		if err != nil {
			t.Fatal("serialized policy within budget was rejected", err)
		}
		data, err := json.Marshal(plan.Network.Policy)
		if err != nil || len(data) != MaxNetworkPolicyBytes+delta {
			t.Fatalf("wrong exact-size fixture: %d, %v", len(data), err)
		}
	}
}

func TestNetworkPolicySizeSerializationError(t *testing.T) {
	policy := CiliumNetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		ManagedFields: []metav1.ManagedFieldsEntry{{FieldsV1: &metav1.FieldsV1{Raw: []byte("invalid-json")}}},
	}}
	if err := validateNetworkPolicySize(policy); err == nil || errors.Is(err, ErrConflict) {
		t.Fatal("serialization failure must not become an accepted or size-limited policy", err)
	}
}
