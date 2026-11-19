// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"errors"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestCompileNativeNetworkRestrictions(t *testing.T) {
	input := nativeCompilerFixture()
	input.Resolution.Capabilities.BlockedPorts = true
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"198.51.100.0/25", "2001:db8:1::/48"}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{
		{Port: 18080, Protocol: corev1.ProtocolTCP},
		{Port: 18081, Protocol: corev1.ProtocolUDP},
	}
	restricted := mustCompile(t, input)
	control := nativeCompilerFixture()
	control.Resolution.Capabilities.BlockedPorts = true
	control.AgentName, control.Namespace = "control-agent", "cetirun-control-agent"
	control.Resolution.Binding.AgentUID = "control-agent-uid"
	unrestricted := mustCompile(t, control)
	for _, tc := range []struct {
		name     string
		ip       string
		port     int32
		protocol corev1.Protocol
		allowed  bool
	}{
		{"IPv4 deny", "198.51.100.42", 443, corev1.ProtocolTCP, false},
		{"IPv4 outside deny", "198.51.100.200", 443, corev1.ProtocolTCP, true},
		{"IPv6 deny", "2001:db8:1::42", 443, corev1.ProtocolTCP, false},
		{"IPv6 outside deny", "2001:db8:2::42", 443, corev1.ProtocolTCP, true},
		{"TCP deny", "203.0.113.42", 18080, corev1.ProtocolTCP, false},
		{"same port UDP", "203.0.113.42", 18080, corev1.ProtocolUDP, true},
		{"UDP deny", "203.0.113.42", 18081, corev1.ProtocolUDP, false},
		{"same port TCP", "203.0.113.42", 18081, corev1.ProtocolTCP, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syntheticEgressAllowed(restricted.Network.Policy.Spec, tc.ip, nil, tc.port, tc.protocol); got != tc.allowed {
				t.Fatalf("restricted policy allowed=%t, want %t", got, tc.allowed)
			}
			if !syntheticEgressAllowed(unrestricted.Network.Policy.Spec, tc.ip, nil, tc.port, tc.protocol) {
				t.Fatal("one Agent's restrictions changed the unrestricted control")
			}
		})
	}
	dns := input.Platform.Topology.DNS[0]
	gateway := input.Platform.Topology.Gateway[0]
	for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
		if !syntheticEgressAllowed(restricted.Network.Policy.Spec, dns.IPs[0], &dns, 53, protocol) {
			t.Fatal("custom restrictions blocked approved DNS")
		}
	}

	if !syntheticEgressAllowed(restricted.Network.Policy.Spec, gateway.Peer.IPs[0], &gateway.Peer,
		gateway.Ports[0].Port, gateway.Ports[0].Protocol) {
		t.Fatal("custom restrictions blocked the model Gateway")
	}
	for _, ip := range []string{"169.254.169.254", "192.168.0.10", "10.1.0.10", "10.244.0.20"} {
		if syntheticEgressAllowed(restricted.Network.Policy.Spec, ip, nil, 443, corev1.ProtocolTCP) {
			t.Fatal("custom restrictions weakened mandatory network isolation")
		}
	}
	selector := restricted.Network.Policy.Spec.EndpointSelector.MatchLabels
	if selector["k8s:"+AgentUIDLabel] != string(input.Resolution.Binding.AgentUID) ||
		selector["k8s:io.kubernetes.pod.namespace"] != input.Namespace ||
		restricted.Network.Policy.Namespace != input.Namespace ||
		reflect.DeepEqual(selector, unrestricted.Network.Policy.Spec.EndpointSelector.MatchLabels) {
		t.Fatal("network restrictions are not scoped to the selected Agent and namespace")
	}
}

func TestCompileNativeNetworkRestrictionsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CompileInput)
		want   error
	}{
		{"DNS CIDR", func(i *CompileInput) { i.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"10.96.0.0/24"} }, ErrConflict},
		{"Gateway CIDR", func(i *CompileInput) { i.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"10.96.1.0/24"} }, ErrConflict},
		{"DNS TCP port", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolTCP}}
		}, ErrConflict},
		{"DNS UDP port", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolUDP}}
		}, ErrConflict},
		{"Gateway port", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}}
		}, ErrConflict},
		{"unqualified family", func(i *CompileInput) {
			i.Resolution.Capabilities.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
			i.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"2001:db8::/32"}
		}, ErrUnsupportedControl},
		{"unqualified ports", func(i *CompileInput) {
			i.Resolution.Capabilities.BlockedPorts = false
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 18080}}
		}, ErrUnsupportedControl},
		{"unregistered SCTP", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 18080, Protocol: corev1.ProtocolSCTP}}
		}, ErrUnsupportedControl},
		{"FQDN", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedFQDNs = []string{"blocked.example.com"}
		}, ErrUnsupportedControl},
		{"missing baseline", func(i *CompileInput) {
			i.Resolution.Capabilities.DenyPrecedence = false
		}, ErrUnsupportedControl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := nativeCompilerFixture()
			input.Resolution.Capabilities.BlockedPorts = true
			tc.mutate(&input)
			if plan, err := Compile(input); plan != nil || !errors.Is(err, tc.want) {
				t.Fatalf("invalid native network configuration returned a plan: plan=%v error=%v, want %v", plan != nil, err, tc.want)
			}
		})
	}
}

func TestCompileNativeNetworkCanonicalRevision(t *testing.T) {
	input := nativeCompilerFixture()
	input.Resolution.Capabilities.BlockedPorts = true
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"198.51.100.1/24", "203.0.113.128/25"}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 18080}, {Port: 18081, Protocol: corev1.ProtocolUDP}}
	before := mustCompile(t, input)
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"203.0.113.128/25", "198.51.100.0/24", "198.51.100.0/24"}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{
		{Port: 18081, Protocol: corev1.ProtocolUDP}, {Port: 18080, Protocol: corev1.ProtocolTCP}, {Port: 18080},
	}
	after := mustCompile(t, input)
	if before.Revision != after.Revision || !reflect.DeepEqual(before.Resources, after.Resources) {
		t.Fatal("equivalent native deny rules changed the effective revision")
	}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 18080, Protocol: corev1.ProtocolUDP}}
	if mustCompile(t, input).Revision == before.Revision {
		t.Fatal("a changed native protocol did not change the effective revision")
	}
}
