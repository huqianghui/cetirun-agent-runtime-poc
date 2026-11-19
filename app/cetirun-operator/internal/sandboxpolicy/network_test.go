// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRejectUnsupportedFQDNCapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture func() CompileInput
	}{
		{"qualified candidate", compilerFixture},
		{"native", nativeCompilerFixture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.fixture()
			before := mustCompile(t, input)
			policy := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{UID: input.Resolution.Binding.AgentPolicyUID},
				Spec:       input.Resolution.AgentPolicy,
			}
			if err := ValidatePlatform(input.Platform, input.Resolution.Capabilities); err != nil {
				t.Fatalf("baseline platform rejected: %v", err)
			}
			if err := ValidatePolicyConfiguration(policy, input.Resolution.Capabilities, input.Platform); err != nil {
				t.Fatalf("baseline policy rejected: %v", err)
			}

			input.Resolution.Capabilities.BlockedFQDNs = true
			if err := ValidatePlatform(input.Platform, input.Resolution.Capabilities); !errors.Is(err, ErrUnsupportedControl) {
				t.Errorf("platform accepted unsupported FQDN capability: %v", err)
			}
			if err := ValidatePolicyConfiguration(policy, input.Resolution.Capabilities, input.Platform); !errors.Is(err, ErrUnsupportedControl) {
				t.Errorf("policy accepted unsupported FQDN capability: %v", err)
			}
			if plan, err := Compile(input); plan != nil || !errors.Is(err, ErrUnsupportedControl) {
				t.Errorf("compiler accepted unsupported FQDN capability: plan=%t, error=%v", plan != nil, err)
			}

			input.Resolution.Capabilities.BlockedFQDNs = false
			after := mustCompile(t, input)
			if before.Revision != after.Revision || !reflect.DeepEqual(before.Resources, after.Resources) {
				t.Fatal("rejecting an unsupported capability changed the supported controls")
			}
		})
	}
}

func TestCompileDNSResolversDoNotExpandWithBackends(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resolvers []string
		backends  []string
	}{
		{"IPv4", []string{"10.96.0.10"}, []string{"10.244.0.11", "10.244.0.12", "10.244.0.13"}},
		{"IPv6", []string{"fd00:96::53"}, []string{"fd00:244::11", "fd00:244::12", "fd00:244::13"}},
		{"dual stack", []string{"10.96.0.10", "fd00:96::53"}, []string{"10.244.0.11", "fd00:244::11", "10.244.0.12", "fd00:244::12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := compilerFixture()
			input.Platform.Topology.DNSResolverIPs = slices.Clone(tc.resolvers)
			input.Platform.Topology.DNS[0].IPs = append(slices.Clone(tc.resolvers), tc.backends...)
			plan := mustCompile(t, input)
			if !slices.Equal(plan.Network.DNSIPs, tc.resolvers) {
				t.Fatalf("backend membership changed Pod nameservers: %v", plan.Network.DNSIPs)
			}
			dns := input.Platform.Topology.DNS[0]
			for _, ip := range dns.IPs {
				for _, peer := range []*Peer{nil, &dns} {
					for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
						if !syntheticEgressAllowed(plan.Network.Policy.Spec, ip, peer, 53, protocol) {
							t.Fatalf("required DNS endpoint %s/%s was denied", ip, protocol)
						}
					}
					if syntheticEgressAllowed(plan.Network.Policy.Spec, ip, peer, 443, corev1.ProtocolTCP) {
						t.Fatal("DNS backend membership granted an unrelated port")
					}
				}
			}
			for _, ip := range []string{"198.51.100.53", "2001:db8::53"} {
				for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
					if syntheticEgressAllowed(plan.Network.Policy.Spec, ip, nil, 53, protocol) {
						t.Fatal("resolver/backend separation opened arbitrary DNS")
					}
				}
			}
			before := input
			before.Platform.Topology.DNS = []Peer{dns}
			before.Platform.Topology.DNS[0].IPs = slices.Clone(tc.resolvers)
			initial := mustCompile(t, before)
			if !slices.Equal(initial.Network.DNSIPs, plan.Network.DNSIPs) || initial.Revision == plan.Revision {
				t.Fatal("backend scaling must change enforcement revision without changing Pod resolvers")
			}
		})
	}
}

func TestCompileRequiresExplicitApprovedDNSResolvers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resolvers []string
		want      error
	}{
		{"missing", nil, ErrUnavailable},
		{"empty", []string{""}, ErrUnavailable},
		{"hostname", []string{"dns.example.com"}, ErrUnavailable},
		{"port suffix", []string{"10.96.0.10:53"}, ErrUnavailable},
		{"subnet", []string{"10.96.0.0/24"}, ErrUnavailable},
		{"mapped IPv4", []string{"::ffff:10.96.0.10"}, ErrUnavailable},
		{"scoped IPv6", []string{"fe80::53%eth0"}, ErrUnavailable},
		{"unapproved IP", []string{"10.96.0.11"}, ErrUnavailable},
		{"Gateway is not DNS", []string{"10.96.1.50"}, ErrUnavailable},
		{"four approved resolvers", []string{"10.96.0.10", "10.96.0.12", "10.96.0.13", "10.96.0.14"}, ErrUnsupportedControl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := compilerFixture()
			input.Platform.Topology.DNS[0].IPs = append(input.Platform.Topology.DNS[0].IPs, "10.96.0.12", "10.96.0.13", "10.96.0.14")
			input.Platform.Topology.DNSResolverIPs = tc.resolvers
			plan, err := Compile(input)
			if plan != nil || !errors.Is(err, tc.want) {
				t.Fatalf("invalid resolver selection returned a plan: %v, %v", plan, err)
			}
			if err := ValidatePlatform(input.Platform, input.Resolution.Capabilities); !errors.Is(err, tc.want) {
				t.Fatalf("platform assessment accepted invalid resolvers: %v", err)
			}
			policy := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{UID: input.Resolution.Binding.AgentPolicyUID},
				Spec:       input.Resolution.AgentPolicy,
			}
			if err := ValidatePolicyConfiguration(policy, input.Resolution.Capabilities, input.Platform); !errors.Is(err, tc.want) {
				t.Fatalf("policy assessment accepted invalid resolvers: %v", err)
			}
		})
	}
	input := compilerFixture()
	input.Platform.Topology.DNS[0].IPs = append(input.Platform.Topology.DNS[0].IPs, "fd00:96::53")
	input.Platform.Topology.DNSResolverIPs = []string{"fd00:96::53"}
	input.Resolution.Capabilities.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
	if plan, err := Compile(input); plan != nil || !errors.Is(err, ErrUnsupportedControl) {
		t.Fatalf("unqualified resolver family was accepted: %v", err)
	}
}

func TestCompileCanonicalDNSResolverSelection(t *testing.T) {
	input := compilerFixture()
	input.Platform.Topology.DNS[0].IPs = append(input.Platform.Topology.DNS[0].IPs, "fd00:0096::53", "10.244.0.11")
	input.Platform.Topology.DNSResolverIPs = []string{"fd00:0096::53", "10.96.0.10", "10.96.0.10"}
	before, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	plan := mustCompile(t, input)
	after, err := json.Marshal(input)
	if err != nil || string(before) != string(after) {
		t.Fatalf("resolver normalization modified the input: %v", err)
	}
	if !slices.Equal(plan.Network.DNSIPs, []string{"10.96.0.10", "fd00:96::53"}) {
		t.Fatalf("unexpected canonical Pod resolvers: %v", plan.Network.DNSIPs)
	}
	slices.Reverse(input.Platform.Topology.DNS[0].IPs)
	input.Platform.Topology.DNSResolverIPs = []string{"10.96.0.10", "fd00:96::53"}
	reordered := mustCompile(t, input)
	if plan.Revision != reordered.Revision || !reflect.DeepEqual(plan.Resources, reordered.Resources) {
		t.Fatal("resolver ordering or duplicates changed effective controls")
	}
	input.Platform.Topology.DNSResolverIPs = []string{"10.96.0.10"}
	if mustCompile(t, input).Revision == plan.Revision {
		t.Fatal("a changed resolver selection did not change the effective revision")
	}
	input.Platform.Topology.DNSResolverIPs = []string{"10.96.0.10", "fd00:96::53", "10.244.0.11"}
	if len(mustCompile(t, input).Network.DNSIPs) != 3 {
		t.Fatal("three explicitly approved resolvers must be accepted")
	}
}

func requireBoundedPortArrays(t *testing.T, plan *Plan) {
	t.Helper()
	raw, err := json.Marshal(plan.Resources[0].Object)
	if err != nil {
		t.Fatal(err)
	}
	var policy CiliumNetworkPolicy
	if err := json.Unmarshal(raw, &policy); err != nil || policy.Kind != "CiliumNetworkPolicy" {
		t.Fatalf("invalid emitted policy: %v", err)
	}
	check := func(groups []PortRule) {
		t.Helper()
		for _, group := range groups {
			if len(group.Ports) == 0 || len(group.Ports) > 40 {
				t.Fatalf("emitted port array has %d entries; Cilium permits at most 40", len(group.Ports))
			}
		}
	}
	for _, rule := range policy.Spec.Egress {
		check(rule.ToPorts)
	}
	for _, rule := range policy.Spec.Ingress {
		check(rule.ToPorts)
	}
	for _, rule := range policy.Spec.EgressDeny {
		check(rule.ToPorts)
	}
	for _, rule := range policy.Spec.IngressDeny {
		check(rule.ToPorts)
	}
}

func testTCPPorts(first, count int) []api.AgentPolicyPort {
	ports := make([]api.AgentPolicyPort, count)
	for i := range count {
		ports[i] = api.AgentPolicyPort{Port: int32(first + i), Protocol: corev1.ProtocolTCP}
	}
	return ports
}

func TestCompileBoundsAllowPortGroupsWithoutChangingPermissions(t *testing.T) {
	protocols := []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP}
	for _, count := range []int{40, 41, 80, 81} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			input := compilerFixture()
			input.Resolution.Capabilities.Protocols = protocols
			ports := make([]api.AgentPolicyPort, count)
			wanted := map[api.AgentPolicyPort]bool{}
			for i := range count {
				ports[i] = api.AgentPolicyPort{Port: int32(1000 + i/3), Protocol: protocols[i%3]}
				wanted[ports[i]] = true
			}
			input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "10.20.0.0/16", Ports: ports}}
			peer := Peer{Name: "internal", Namespace: "applications", UID: "example-internal", IPs: []string{"10.20.1.7"}}
			input.Platform.Topology.Peers = []Peer{peer}
			plan := mustCompile(t, input)
			requireBoundedPortArrays(t, plan)
			found := false
			for _, rule := range plan.Network.Policy.Spec.Egress {
				if len(rule.ToCIDRSet) == 1 && rule.ToCIDRSet[0].CIDR == "10.20.0.0/16" {
					found = true
					if len(rule.ToPorts) != (count+39)/40 {
						t.Fatal("internal allow ports were not divided into bounded groups")
					}
				}
			}
			if !found {
				t.Fatal("compiled internal CIDR allow is missing")
			}
			for _, identity := range []*Peer{nil, &peer} {
				for port := int32(999); port <= int32(1001+count/3); port++ {
					for _, protocol := range protocols {
						got := syntheticEgressAllowed(plan.Network.Policy.Spec, peer.IPs[0], identity, port, protocol)
						if got != wanted[api.AgentPolicyPort{Port: port, Protocol: protocol}] {
							t.Fatalf("grouping changed permission for %d/%s", port, protocol)
						}
					}
				}
			}
			slices.Reverse(input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].Ports)
			reordered := mustCompile(t, input)
			if plan.Revision != reordered.Revision || !reflect.DeepEqual(plan.Resources, reordered.Resources) {
				t.Fatal("port ordering changed grouped output or revision")
			}
			input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{ports[0]}
			blocked := mustCompile(t, input)
			requireBoundedPortArrays(t, blocked)
			if syntheticEgressAllowed(blocked.Network.Policy.Spec, peer.IPs[0], &peer, ports[0].Port, ports[0].Protocol) {
				t.Fatal("port grouping bypassed explicit deny precedence")
			}
		})
	}
}

func TestCompileBoundsFragmentedDenyPortGroups(t *testing.T) {
	for _, count := range []int{37, 38, 40, 77, 78} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			input := compilerFixture()
			ports := testTCPPorts(1000, count)
			for i := range ports {
				ports[i].Port += int32(i)
			}
			input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "10.20.0.0/16", Ports: ports}}
			peer := Peer{Name: "internal", Namespace: "applications", UID: "example-internal", IPs: []string{"10.20.1.7"}}
			input.Platform.Topology.Peers = []Peer{peer}
			plan := mustCompile(t, input)
			requireBoundedPortArrays(t, plan)
			found := false
			for _, rule := range plan.Network.Policy.Spec.EgressDeny {
				if len(rule.ToCIDRSet) == 1 && rule.ToCIDRSet[0].CIDR == "10.20.0.0/16" {
					found = true
					entries := 0
					for _, group := range rule.ToPorts {
						entries += len(group.Ports)
					}
					if entries != count+3 || len(rule.ToPorts) != (count+3+39)/40 {
						t.Fatalf("fragmented complement was lost or incorrectly grouped: %+v", rule.ToPorts)
					}
				}
			}
			if !found {
				t.Fatal("internal CIDR complementary deny is missing")
			}
			probes := []int32{1, 65535}
			for port := int32(999); port <= 1000+2*int32(count); port++ {
				probes = append(probes, port)
			}
			for _, identity := range []*Peer{nil, &peer} {
				for _, port := range probes {
					for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP} {
						want := protocol == corev1.ProtocolTCP && port >= 1000 && port < 1000+2*int32(count) && port%2 == 0
						if syntheticEgressAllowed(plan.Network.Policy.Spec, peer.IPs[0], identity, port, protocol) != want {
							t.Fatalf("grouping changed fragmented permission for %d/%s", port, protocol)
						}
					}
				}
			}
			slices.Reverse(input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].Ports)
			reordered := mustCompile(t, input)
			if plan.Revision != reordered.Revision || !reflect.DeepEqual(plan.Resources, reordered.Resources) {
				t.Fatal("fragmented port ordering changed output or revision")
			}
			input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 1000, Protocol: corev1.ProtocolTCP}}
			blocked := mustCompile(t, input)
			requireBoundedPortArrays(t, blocked)
			for _, identity := range []*Peer{nil, &peer} {
				if syntheticEgressAllowed(blocked.Network.Policy.Spec, peer.IPs[0], identity, 1000, corev1.ProtocolTCP) ||
					!syntheticEgressAllowed(blocked.Network.Policy.Spec, peer.IPs[0], identity, 1002, corev1.ProtocolTCP) {
					t.Fatal("explicit deny changed neighboring fragmented permissions")
				}
			}
		})
	}
}

func TestCompileChunksPortsAfterOverlappingCIDRUnion(t *testing.T) {
	input := compilerFixture()
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
		{CIDR: "10.20.0.0/16", Ports: testTCPPorts(1000, 21)},
		{CIDR: "10.20.1.0/24", Ports: testTCPPorts(1021, 21)},
	}
	peer := Peer{Name: "internal", Namespace: "applications", UID: "example-internal", IPs: []string{"10.20.1.7"}}
	input.Platform.Topology.Peers = []Peer{peer}
	plan := mustCompile(t, input)
	requireBoundedPortArrays(t, plan)
	for port := int32(999); port <= 1042; port++ {
		for _, identity := range []*Peer{nil, &peer} {
			want := port >= 1000 && port <= 1041
			if syntheticEgressAllowed(plan.Network.Policy.Spec, peer.IPs[0], identity, port, corev1.ProtocolTCP) != want {
				t.Fatalf("overlapping port union lost or added port %d", port)
			}
		}
		want := port >= 1000 && port <= 1020
		if syntheticEgressAllowed(plan.Network.Policy.Spec, "10.20.2.7", nil, port, corev1.ProtocolTCP) != want {
			t.Fatal("nested CIDR ports leaked into the broader CIDR")
		}
	}
}

func TestCompileChunksMergedGatewayAPIAndIngressPorts(t *testing.T) {
	for _, direction := range []string{"Gateway", "KubernetesAPI", "Ingress"} {
		t.Run(direction, func(t *testing.T) {
			input := adminFixture()
			destinations := &input.Platform.Topology.Gateway
			switch direction {
			case "KubernetesAPI":
				destinations = &input.Platform.Topology.KubernetesAPI
			case "Ingress":
				destinations = &input.Platform.Topology.Ingress
			}
			ports := testTCPPorts(1000, 41)
			for i := range ports {
				ports[i].Port += int32(i)
			}
			first := (*destinations)[0]
			first.Ports = ports[:21]
			second := first
			second.Ports = ports[21:]
			*destinations = []Destination{first, second}
			plan := mustCompile(t, input)
			requireBoundedPortArrays(t, plan)
			if direction == "Ingress" {
				rules := plan.Network.Policy.Spec.Ingress
				if len(rules) != 1 || len(rules[0].ToPorts) != 2 {
					t.Fatal("merged ingress ports were not split into two groups")
				}
				got := map[PortRange]bool{}
				for _, group := range rules[0].ToPorts {
					for _, port := range group.Ports {
						got[port] = true
					}
				}
				if len(got) != 41 {
					t.Fatal("ingress grouping lost or duplicated ports")
				}
				for _, port := range ports {
					if !got[PortRange{Port: fmt.Sprint(port.Port), Protocol: port.Protocol}] {
						t.Fatal("ingress grouping changed the allowed port set")
					}
				}
				var denies []PortRule
				for _, rule := range plan.Network.Policy.Spec.IngressDeny {
					denies = append(denies, rule.ToPorts...)
				}
				if len(denies) != 2 {
					t.Fatal("fragmented ingress complement was not split into two groups")
				}
				for port := int32(999); port <= 1081; port++ {
					for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP} {
						want := protocol == corev1.ProtocolTCP && port >= 1000 && port <= 1080 && port%2 == 0
						allowed := syntheticPortsMatch(rules[0].ToPorts, port, protocol)
						denied := syntheticPortsMatch(denies, port, protocol)
						if allowed != want || denied == want {
							t.Fatalf("ingress grouping changed fragmented permission for %d/%s", port, protocol)
						}
					}
				}
			} else {
				var peer *Peer
				if first.Peer.Namespace != "" {
					peer = &first.Peer
				}
				for port := int32(999); port <= 1081; port++ {
					want := port >= 1000 && port <= 1080 && port%2 == 0
					if syntheticEgressAllowed(plan.Network.Policy.Spec, first.Peer.IPs[0], peer, port, corev1.ProtocolTCP) != want {
						t.Fatalf("merged required destination changed permission for %d", port)
					}
				}
			}
		})
	}
}
