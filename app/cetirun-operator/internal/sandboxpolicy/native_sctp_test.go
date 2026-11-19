// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nativeSCTPCompilerFixture() CompileInput {
	input := nativeCompilerFixture()
	input.Resolution.Capabilities.Protocols = append(input.Resolution.Capabilities.Protocols, corev1.ProtocolSCTP)
	input.Resolution.Capabilities.BlockedPorts = true
	return input
}

func TestCompileNativeSCTPPortRules(t *testing.T) {
	input := nativeSCTPCompilerFixture()
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
		{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{
			{Port: 3868, Protocol: corev1.ProtocolSCTP}, {Port: 3869, Protocol: corev1.ProtocolSCTP},
		}},
		{CIDR: "10.20.1.0/24", Ports: []api.AgentPolicyPort{
			{Port: 3868}, {Port: 3868, Protocol: corev1.ProtocolUDP}, {Port: 8080, Protocol: corev1.ProtocolSCTP},
		}},
		{CIDR: "fd00:20::/64", Ports: []api.AgentPolicyPort{
			{Port: 1, Protocol: corev1.ProtocolSCTP}, {Port: 65535, Protocol: corev1.ProtocolSCTP},
		}},
		{CIDR: "10.244.0.40/32", Ports: []api.AgentPolicyPort{
			{Port: 3868, Protocol: corev1.ProtocolSCTP}, {Port: 3868, Protocol: corev1.ProtocolUDP},
		}},
	}
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"10.20.2.0/24", "fd00:20::40/128"}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{
		{Port: 3868}, {Port: 3869, Protocol: corev1.ProtocolSCTP},
		{Port: 53, Protocol: corev1.ProtocolSCTP}, {Port: 8443, Protocol: corev1.ProtocolSCTP},
	}
	peer := Peer{Name: "internal-service", Namespace: "applications", UID: "internal-service-uid", IPs: []string{"10.244.0.40"}}
	input.Platform.Topology.Peers = []Peer{peer}
	foreign := peer
	foreign.UID = "other-service-uid"
	wrongNamespace := peer
	wrongNamespace.Namespace = "other-applications"
	plan := mustCompile(t, input)
	for _, tc := range []struct {
		name     string
		ip       string
		peer     *Peer
		port     int32
		protocol corev1.Protocol
		allowed  bool
	}{
		{"SCTP grant", "10.20.0.7", nil, 3868, corev1.ProtocolSCTP, true},
		{"same port not UDP", "10.20.0.7", nil, 3868, corev1.ProtocolUDP, false},
		{"other SCTP port", "10.20.0.7", nil, 3867, corev1.ProtocolSCTP, false},
		{"SCTP deny beats grant", "10.20.0.7", nil, 3869, corev1.ProtocolSCTP, false},
		{"overlap retains SCTP", "10.20.1.7", nil, 3868, corev1.ProtocolSCTP, true},
		{"overlap adds UDP", "10.20.1.7", nil, 3868, corev1.ProtocolUDP, true},
		{"TCP deny beats overlap", "10.20.1.7", nil, 3868, corev1.ProtocolTCP, false},
		{"overlap adds SCTP", "10.20.1.7", nil, 8080, corev1.ProtocolSCTP, true},
		{"overlap not other port", "10.20.1.7", nil, 8081, corev1.ProtocolSCTP, false},
		{"overlap not TCP", "10.20.1.7", nil, 8080, corev1.ProtocolTCP, false},
		{"outside grant", "10.21.0.7", nil, 3868, corev1.ProtocolSCTP, false},
		{"CIDR deny beats SCTP", "10.20.2.7", nil, 3868, corev1.ProtocolSCTP, false},
		{"IPv6 first port", "fd00:20::7", nil, 1, corev1.ProtocolSCTP, true},
		{"IPv6 last port", "fd00:20::7", nil, 65535, corev1.ProtocolSCTP, true},
		{"IPv6 gap", "fd00:20::7", nil, 65534, corev1.ProtocolSCTP, false},
		{"IPv6 not TCP", "fd00:20::7", nil, 1, corev1.ProtocolTCP, false},
		{"IPv6 outside grant", "fd00:21::7", nil, 65535, corev1.ProtocolSCTP, false},
		{"IPv6 CIDR deny", "fd00:20::40", nil, 65535, corev1.ProtocolSCTP, false},
		{"managed peer SCTP", peer.IPs[0], &peer, 3868, corev1.ProtocolSCTP, true},
		{"managed peer UDP", peer.IPs[0], &peer, 3868, corev1.ProtocolUDP, true},
		{"managed peer other port", peer.IPs[0], &peer, 3867, corev1.ProtocolSCTP, false},
		{"wrong peer identity", peer.IPs[0], &foreign, 3868, corev1.ProtocolSCTP, false},
		{"wrong namespace", peer.IPs[0], &wrongNamespace, 3868, corev1.ProtocolSCTP, false},
		{"public SCTP baseline", "203.0.113.42", nil, 3868, corev1.ProtocolSCTP, true},
		{"public TCP deny", "203.0.113.42", nil, 3868, corev1.ProtocolTCP, false},
		{"public UDP unaffected", "203.0.113.42", nil, 3868, corev1.ProtocolUDP, true},
		{"public SCTP deny", "203.0.113.42", nil, 3869, corev1.ProtocolSCTP, false},
		{"node isolation", "192.168.0.42", nil, 3868, corev1.ProtocolSCTP, false},
		{"management isolation", "10.1.0.42", nil, 3868, corev1.ProtocolSCTP, false},
		{"metadata isolation", "169.254.169.254", nil, 3868, corev1.ProtocolSCTP, false},
		{"control plane isolation", "203.0.113.10", nil, 3868, corev1.ProtocolSCTP, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := plan.Network.Policy.Spec
			if got := syntheticEgressAllowed(spec, tc.ip, tc.peer, tc.port, tc.protocol); got != tc.allowed {
				t.Fatalf("allowed=%t, want %t", got, tc.allowed)
			}
			spec.Egress = append(slices.Clone(spec.Egress), EgressRule{ToEntities: []string{"all"}})
			if !tc.allowed && syntheticEgressAllowed(spec, tc.ip, tc.peer, tc.port, tc.protocol) {
				t.Fatal("an overlapping allow bypassed a protocol, port, address or identity deny")
			}
		})
	}
	for _, destination := range append([]Destination{{Peer: input.Platform.Topology.DNS[0], Ports: dnsPorts}}, input.Platform.Topology.Gateway...) {
		for _, port := range destination.Ports {
			if !syntheticEgressAllowed(plan.Network.Policy.Spec, destination.Peer.IPs[0], &destination.Peer, port.Port, port.Protocol) {
				t.Fatal("same-number SCTP denies blocked required TCP/UDP DNS or Gateway traffic")
			}
		}
	}
}

func TestCompileNativeSCTPQualificationAndConflicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CompileInput)
		want   error
	}{
		{"unregistered SCTP", func(i *CompileInput) {
			i.Resolution.Capabilities.Protocols = []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}
		}, ErrUnsupportedControl},
		{"unregistered blocked ports", func(i *CompileInput) {
			i.Resolution.Capabilities.BlockedPorts = false
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 3869, Protocol: corev1.ProtocolSCTP}}
		}, ErrUnsupportedControl},
		{"unregistered deny precedence", func(i *CompileInput) {
			i.Resolution.Capabilities.DenyPrecedence = false
		}, ErrUnsupportedControl},
		{"lowercase protocol", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].Ports[0].Protocol = "sctp"
		}, nil},
		{"unknown protocol", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 3869, Protocol: "OTHER"}}
		}, nil},
		{"zero port", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].Ports[0].Port = 0
		}, nil},
		{"oversized port", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 65536, Protocol: corev1.ProtocolSCTP}}
		}, nil},
		{"empty ports", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].Ports = nil
		}, nil},
		{"malformed CIDR", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "not-a-cidr"
		}, nil},
		{"IPv6 not registered", func(i *CompileInput) {
			i.Resolution.Capabilities.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "fd00:20::/64"
		}, ErrUnsupportedControl},
		{"node", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "192.168.0.0/16"
		}, ErrConflict},
		{"management", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "10.1.0.0/16"
		}, ErrConflict},
		{"metadata", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "169.254.169.254/32"
		}, ErrConflict},
		{"IPv6 node", func(i *CompileInput) {
			i.Platform.Topology.NodeCIDRs = append(i.Platform.Topology.NodeCIDRs, "fd00:192::/64")
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "fd00:192::/64"
		}, ErrConflict},
		{"IPv6 metadata", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "fe80::/64"
		}, ErrConflict},
		{"control plane", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "203.0.113.10/32"
		}, ErrConflict},
		{"IPv6 control plane", func(i *CompileInput) {
			i.Platform.Topology.KubernetesAPI[0].Peer.IPs = []string{"fd00:100::10"}
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "fd00:100::10/128"
		}, ErrConflict},
		{"Service VIP", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "10.96.2.20/32"
		}, ErrUnsupportedControl},
		{"overlapping Service range", func(i *CompileInput) {
			i.Platform.Topology.ServiceCIDRs = append(i.Platform.Topology.ServiceCIDRs, "10.20.1.0/24")
		}, ErrUnsupportedControl},
		{"IPv6 Service VIP", func(i *CompileInput) {
			i.Platform.Topology.ServiceCIDRs = append(i.Platform.Topology.ServiceCIDRs, "fd00:96::/64")
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "fd00:96::20/128"
		}, ErrUnsupportedControl},
		{"DNS TCP deny", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53}}
		}, ErrConflict},
		{"DNS UDP deny", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolUDP}}
		}, ErrConflict},
		{"Gateway deny", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8443}}
		}, ErrConflict},
		{"incomplete inventory", func(i *CompileInput) {
			i.Platform.Topology.ManagedPeersComplete = false
		}, ErrUnavailable},
		{"inconsistent peer permissions", func(i *CompileInput) {
			i.Platform.Topology.Peers = []Peer{{
				Name: "service", Namespace: "applications", UID: "service-uid", IPs: []string{"10.20.0.7", "10.21.0.7"},
			}}
		}, ErrUnsupportedControl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := nativeSCTPCompilerFixture()
			input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{
				CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 3868, Protocol: corev1.ProtocolSCTP}},
			}}
			tc.change(&input)
			if plan, err := Compile(input); plan != nil || err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("invalid SCTP request returned plan=%t error=%v, want %v", plan != nil, err, tc.want)
			}
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{UID: "policy-uid"}, Spec: input.Resolution.AgentPolicy}
			if err := ValidatePolicyConfiguration(policy, input.Resolution.Capabilities, input.Platform); err == nil {
				t.Fatal("standalone configuration validation accepted the invalid SCTP request")
			}
		})
	}
}

func TestCompileNativeSCTPChanges(t *testing.T) {
	input := nativeSCTPCompilerFixture()
	baseline := nativeCompilerFixture()
	baseline.Resolution.Capabilities.BlockedPorts = true
	withoutRegistration, withRegistration := mustCompile(t, baseline), mustCompile(t, input)
	if !reflect.DeepEqual(withoutRegistration.Network.Policy.Spec, withRegistration.Network.Policy.Spec) ||
		withoutRegistration.Revision == withRegistration.Revision {
		t.Fatal("registration alone must not widen the baseline and must change the bound revision")
	}
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{
		CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 3868, Protocol: corev1.ProtocolSCTP}},
	}}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 3869, Protocol: corev1.ProtocolSCTP}}
	original := mustCompile(t, input)
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "10.20.0.7/16"
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = slices.Repeat(input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs, 2)
	input.Resolution.AgentPolicy.Network.BlockedPorts = slices.Repeat(input.Resolution.AgentPolicy.Network.BlockedPorts, 2)
	slices.Reverse(input.Resolution.Capabilities.Protocols)
	equivalent := mustCompile(t, input)
	if original.Revision != equivalent.Revision || !reflect.DeepEqual(original.Resources, equivalent.Resources) {
		t.Fatal("canonical SCTP duplicates or registration ordering changed the plan")
	}
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{
		CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 3868, Protocol: corev1.ProtocolUDP}},
	}}
	changed := mustCompile(t, input)
	if changed.Revision == original.Revision ||
		syntheticEgressAllowed(changed.Network.Policy.Spec, "10.20.0.7", nil, 3868, corev1.ProtocolSCTP) ||
		!syntheticEgressAllowed(changed.Network.Policy.Spec, "10.20.0.7", nil, 3868, corev1.ProtocolUDP) {
		t.Fatal("protocol replacement retained the old permission or revision")
	}
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = nil
	input.Resolution.AgentPolicy.Network.BlockedPorts = nil
	removed := mustCompile(t, input)
	if removed.Revision == original.Revision ||
		syntheticEgressAllowed(removed.Network.Policy.Spec, "10.20.0.7", nil, 3868, corev1.ProtocolSCTP) ||
		!syntheticEgressAllowed(removed.Network.Policy.Spec, "203.0.113.42", nil, 3869, corev1.ProtocolSCTP) {
		t.Fatal("removing SCTP requests retained old controls")
	}
}

func TestCompileNativeSCTPRawInputBudget(t *testing.T) {
	input := nativeSCTPCompilerFixture()
	input.Resolution.AgentPolicy.Network.BlockedPorts = slices.Repeat(
		[]api.AgentPolicyPort{{Port: 3869, Protocol: corev1.ProtocolSCTP}}, MaxControlInputEntries-17)
	mustCompile(t, input)
	input.Resolution.AgentPolicy.Network.BlockedPorts = append(input.Resolution.AgentPolicy.Network.BlockedPorts,
		api.AgentPolicyPort{Port: 3869, Protocol: corev1.ProtocolSCTP})
	if plan, err := Compile(input); plan != nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "control input") {
		t.Fatalf("SCTP duplicates bypassed the raw-input budget: plan=%t error=%v", plan != nil, err)
	}
}

func TestCompileNativeSCTPIPv6Only(t *testing.T) {
	input := nativeSCTPCompilerFixture()
	input.Resolution.Capabilities.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}
	topology := &input.Platform.Topology
	topology.PodCIDRs, topology.ServiceCIDRs = []string{"fd00:244::/64"}, []string{"fd00:96::/64"}
	topology.NodeCIDRs, topology.ManagementCIDRs = []string{"fd00:192::/64"}, []string{"fd00:1::/64"}
	topology.MetadataCIDRs = []string{"fd00:169::/64"}
	topology.DNS[0].IPs, topology.DNSResolverIPs = []string{"fd00:96::a"}, []string{"fd00:96::a"}
	topology.Gateway[0].Peer.IPs = []string{"fd00:96::50"}
	topology.Ingress[0].Peer.IPs = []string{"fd00:96::60"}
	topology.KubernetesAPI[0].Peer.IPs = []string{"2001:db8:100::10"}
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{
		CIDR: "fd00:20::/64", Ports: []api.AgentPolicyPort{{Port: 3868, Protocol: corev1.ProtocolSCTP}},
	}}
	plan := mustCompile(t, input)
	if !syntheticEgressAllowed(plan.Network.Policy.Spec, "fd00:20::7", nil, 3868, corev1.ProtocolSCTP) ||
		syntheticEgressAllowed(plan.Network.Policy.Spec, "fd00:20::7", nil, 3868, corev1.ProtocolTCP) ||
		syntheticEgressAllowed(plan.Network.Policy.Spec, "fd00:192::7", nil, 3868, corev1.ProtocolSCTP) {
		t.Fatal("IPv6-only registration lost SCTP protocol or node isolation")
	}
	for _, destination := range append([]Destination{{Peer: topology.DNS[0], Ports: dnsPorts}}, topology.Gateway...) {
		for _, port := range destination.Ports {
			if !syntheticEgressAllowed(plan.Network.Policy.Spec, destination.Peer.IPs[0], &destination.Peer, port.Port, port.Protocol) {
				t.Fatal("IPv6-only SCTP grants removed a required DNS or Gateway path")
			}
		}
	}
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "10.20.0.0/16"
	if plan, err := Compile(input); plan != nil || !errors.Is(err, ErrUnsupportedControl) {
		t.Fatal("IPv6-only registration accepted an unregistered IPv4 SCTP grant:", err)
	}
}

func TestCompileNativeSCTPPortGroups(t *testing.T) {
	for _, count := range []int{40, 41, 80, 81} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			input := nativeSCTPCompilerFixture()
			var ports []api.AgentPolicyPort
			for i := range count {
				ports = append(ports, api.AgentPolicyPort{Port: int32(1000 + 2*i), Protocol: corev1.ProtocolSCTP})
				input.Resolution.AgentPolicy.Network.BlockedPorts = append(input.Resolution.AgentPolicy.Network.BlockedPorts,
					api.AgentPolicyPort{Port: int32(2000 + 2*i), Protocol: corev1.ProtocolSCTP})
			}
			input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "10.20.0.0/16", Ports: ports}}
			plan := mustCompile(t, input)
			requireBoundedPortArrays(t, plan)
			for port := int32(999); port <= int32(1000+2*count); port++ {
				for _, protocol := range input.Resolution.Capabilities.Protocols {
					want := protocol == corev1.ProtocolSCTP && slices.Contains(ports, api.AgentPolicyPort{Port: port, Protocol: protocol})
					if syntheticEgressAllowed(plan.Network.Policy.Spec, "10.20.0.7", nil, port, protocol) != want {
						t.Fatalf("grouping changed internal %d/%s permission", port, protocol)
					}
					denied := slices.Contains(input.Resolution.AgentPolicy.Network.BlockedPorts, api.AgentPolicyPort{Port: port + 1000, Protocol: protocol})
					if syntheticEgressAllowed(plan.Network.Policy.Spec, "203.0.113.42", nil, port+1000, protocol) == denied {
						t.Fatalf("grouping changed public %d/%s deny", port+1000, protocol)
					}
				}
			}
		})
	}
}
