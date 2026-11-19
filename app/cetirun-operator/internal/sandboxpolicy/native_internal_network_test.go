// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func nativeInternalNetworkFixture() CompileInput {
	input := nativeCompilerFixture()
	input.Resolution.Capabilities.BlockedPorts = true
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
		{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}},
		{CIDR: "10.20.1.0/24", Ports: []api.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolUDP}}},
		{CIDR: "fd00:20::/64", Ports: []api.AgentPolicyPort{{Port: 65535}}},
		{CIDR: "10.244.0.40/32", Ports: []api.AgentPolicyPort{{Port: 8080}}},
	}
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"10.20.2.0/24"}
	input.Platform.Topology.Peers = []Peer{{
		Name: "internal-service", Namespace: "applications", UID: "internal-service-uid", IPs: []string{"10.244.0.40"},
	}}
	return input
}

func TestCompileNativeInternalGrants(t *testing.T) {
	input := nativeInternalNetworkFixture()
	before := mustCompile(t, input)
	peer := input.Platform.Topology.Peers[0]
	foreign := peer
	foreign.UID = "other-service-uid"
	wrongNamespace := peer
	wrongNamespace.Namespace = "other-applications"
	for _, tc := range []struct {
		name     string
		ip       string
		peer     *Peer
		port     int32
		protocol corev1.Protocol
		allowed  bool
	}{
		{"default TCP", "10.20.0.7", nil, 443, corev1.ProtocolTCP, true},
		{"different protocol", "10.20.0.7", nil, 443, corev1.ProtocolUDP, false},
		{"outside CIDR", "10.21.0.7", nil, 443, corev1.ProtocolTCP, false},
		{"overlap retains parent", "10.20.1.7", nil, 443, corev1.ProtocolTCP, true},
		{"overlap adds UDP", "10.20.1.7", nil, 8080, corev1.ProtocolUDP, true},
		{"overlap not TCP", "10.20.1.7", nil, 8080, corev1.ProtocolTCP, false},
		{"CIDR deny wins", "10.20.2.7", nil, 443, corev1.ProtocolTCP, false},
		{"IPv6 boundary port", "fd00:20::7", nil, 65535, corev1.ProtocolTCP, true},
		{"IPv6 other port", "fd00:20::7", nil, 1, corev1.ProtocolTCP, false},
		{"IPv6 outside CIDR", "fd00:21::7", nil, 65535, corev1.ProtocolTCP, false},
		{"registered Pod", peer.IPs[0], &peer, 8080, corev1.ProtocolTCP, true},
		{"other Pod port", peer.IPs[0], &peer, 8081, corev1.ProtocolTCP, false},
		{"different Pod identity", peer.IPs[0], &foreign, 8080, corev1.ProtocolTCP, false},
		{"different namespace", peer.IPs[0], &wrongNamespace, 8080, corev1.ProtocolTCP, false},
		{"public baseline", "203.0.113.42", nil, 443, corev1.ProtocolTCP, true},
		{"node isolation", "192.168.0.42", nil, 443, corev1.ProtocolTCP, false},
		{"management isolation", "10.1.0.42", nil, 443, corev1.ProtocolTCP, false},
		{"metadata isolation", "169.254.169.254", nil, 80, corev1.ProtocolTCP, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syntheticEgressAllowed(before.Network.Policy.Spec, tc.ip, tc.peer, tc.port, tc.protocol); got != tc.allowed {
				t.Fatalf("allowed=%t, want %t", got, tc.allowed)
			}
			spec := before.Network.Policy.Spec
			spec.Egress = append(slices.Clone(spec.Egress), EgressRule{ToEntities: []string{"all"}})
			if !tc.allowed && syntheticEgressAllowed(spec, tc.ip, tc.peer, tc.port, tc.protocol) {
				t.Fatal("an overlapping allow widened the internal grant")
			}
		})
	}
	for _, destination := range append([]Destination{{Peer: input.Platform.Topology.DNS[0], Ports: dnsPorts}}, input.Platform.Topology.Gateway...) {
		for _, port := range destination.Ports {
			if !syntheticEgressAllowed(before.Network.Policy.Spec, destination.Peer.IPs[0], &destination.Peer, port.Port, port.Protocol) {
				t.Fatal("internal grants removed a required platform path")
			}
		}
	}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8080}}
	denied := mustCompile(t, input)
	if syntheticEgressAllowed(denied.Network.Policy.Spec, peer.IPs[0], &peer, 8080, corev1.ProtocolTCP) {
		t.Fatal("an internal grant bypassed an explicit port deny")
	}
	input.Resolution.AgentPolicy.Network.BlockedPorts = nil
	for index := range input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs {
		input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[index].Ports = append(
			input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[index].Ports, dnsPorts...)
	}
	dnsRestricted := mustCompile(t, input)
	for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
		for _, destination := range []struct {
			ip   string
			peer *Peer
		}{{"10.20.1.7", nil}, {peer.IPs[0], &peer}} {
			if syntheticEgressAllowed(dnsRestricted.Network.Policy.Spec, destination.ip, destination.peer, 53, protocol) {
				t.Fatal("an internal grant bypassed reserved DNS peer restrictions")
			}
		}
	}
}

func TestCompileNativeInternalGrantChanges(t *testing.T) {
	input := nativeInternalNetworkFixture()
	original := mustCompile(t, input)
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].CIDR = "10.20.0.7/16"
	slices.Reverse(input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs)
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = append(input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs,
		input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0])
	equivalent := mustCompile(t, input)
	if original.Revision != equivalent.Revision || !reflect.DeepEqual(original.Resources, equivalent.Resources) {
		t.Fatal("equivalent internal grants changed the effective policy")
	}
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = nil
	revoked := mustCompile(t, input)
	peer := input.Platform.Topology.Peers[0]
	if revoked.Revision == original.Revision ||
		syntheticEgressAllowed(revoked.Network.Policy.Spec, "10.20.1.7", nil, 443, corev1.ProtocolTCP) ||
		syntheticEgressAllowed(revoked.Network.Policy.Spec, peer.IPs[0], &peer, 8080, corev1.ProtocolTCP) {
		t.Fatal("removing grants retained old permissions or revision")
	}
}

func TestCompileNativeInternalGrantConflicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cidr   string
		change func(*CompileInput)
		want   error
	}{
		{name: "node", cidr: "192.168.0.0/16", want: ErrConflict},
		{name: "management", cidr: "10.1.0.0/16", want: ErrConflict},
		{name: "metadata", cidr: "169.254.169.254/32", want: ErrConflict},
		{name: "loopback", cidr: "127.0.0.1/32", want: ErrConflict},
		{name: "public", cidr: "203.0.113.0/24", want: ErrUnsupportedControl},
		{name: "unregistered SCTP", cidr: "10.20.0.0/16", change: func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs[0].Ports[0].Protocol = corev1.ProtocolSCTP
		}, want: ErrUnsupportedControl},
		{name: "unqualified IPv6", cidr: "fd00:20::/64", change: func(i *CompileInput) {
			i.Resolution.Capabilities.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
		}, want: ErrUnsupportedControl},
		{name: "incomplete inventory", cidr: "10.20.0.0/16", change: func(i *CompileInput) {
			i.Platform.Topology.ManagedPeersComplete = false
		}, want: ErrUnavailable},
		{name: "inconsistent managed addresses", cidr: "10.244.0.40/32", change: func(i *CompileInput) {
			i.Platform.Topology.Peers = []Peer{{
				Name: "service", Namespace: "applications", UID: "service-uid", IPs: []string{"10.244.0.40", "10.244.0.41"},
			}}
		}, want: ErrUnsupportedControl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := nativeCompilerFixture()
			input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
				{CIDR: tc.cidr, Ports: []api.AgentPolicyPort{{Port: 443}}},
			}
			if tc.change != nil {
				tc.change(&input)
			}
			if plan, err := Compile(input); plan != nil || !errors.Is(err, tc.want) {
				t.Fatalf("conflicting grant produced plan=%t err=%v, want %v", plan != nil, err, tc.want)
			}
		})
	}
}
