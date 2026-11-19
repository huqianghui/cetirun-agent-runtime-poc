// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func rolloutEgressFixture() CompileInput {
	input := nativeCompilerFixture()
	input.Platform.Topology.DNS[0].PodSelector = map[string]string{"k8s-app": "kube-dns"}
	input.Platform.Topology.Gateway[0].Peer.PodSelector = map[string]string{
		"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway",
		"app.kubernetes.io/instance": "example",
	}
	input.Platform.Topology.Gateway[0].Ports = []api.AgentPolicyPort{{Port: 4000, Protocol: corev1.ProtocolTCP}}
	return input
}

func TestNativeServicePeersSurvivePodReplacement(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		t.Run(string(backend), func(t *testing.T) {
			input := rolloutEgressFixture()
			input.Resolution.Sandbox.Type = backend
			if backend == api.SandboxContainer {
				input.Resolution.Capabilities.RuntimeClassName = "synthetic-runc"
				input.Resolution.Capabilities.PerContainerResourceLimits = true
			}
			input.Platform.Topology.PodCIDRs = append(input.Platform.Topology.PodCIDRs, "fd00:244::/64")
			input.Platform.Topology.ServiceCIDRs = append(input.Platform.Topology.ServiceCIDRs, "fd00:96::/64")
			input.Platform.Topology.DNSResolverIPs = append(input.Platform.Topology.DNSResolverIPs, "fd00:96::53")
			input.Platform.Topology.DNS[0].IPs = append(input.Platform.Topology.DNS[0].IPs, "fd00:96::53")
			input.Platform.Topology.Gateway[0].Peer.IPs = append(input.Platform.Topology.Gateway[0].Peer.IPs, "fd00:96::50")
			plan := mustCompile(t, input)
			if err := ValidatePlatform(input.Platform, input.Resolution.Capabilities); err != nil {
				t.Fatal("platform assessment rejected service selectors:", err)
			}
			if !slices.Equal(plan.Network.DNSIPs, input.Platform.Topology.DNSResolverIPs) {
				t.Fatal("backend selectors changed the configured Pod resolvers")
			}
			destinations := append([]Destination{{Peer: input.Platform.Topology.DNS[0], Ports: dnsPorts}},
				input.Platform.Topology.Gateway...)
			for _, destination := range destinations {
				pod := Peer{Namespace: destination.Peer.Namespace, PodSelector: maps.Clone(destination.Peer.PodSelector)}
				for _, ip := range []string{"10.244.0.10", "10.244.1.25", "fd00:244::10", "fd00:244::25"} {
					for _, port := range destination.Ports {
						if !syntheticEgressAllowed(plan.Network.Policy.Spec, ip, &pod, port.Port, port.Protocol) {
							t.Fatalf("replacement %s Pod at %s lost access without a peer-uid label", destination.Peer.Name, ip)
						}
					}
				}
				for _, ip := range destination.Peer.IPs {
					for _, port := range destination.Ports {
						if !syntheticEgressAllowed(plan.Network.Policy.Spec, ip, nil, port.Port, port.Protocol) {
							t.Fatalf("registered Service address %s lost access", ip)
						}
					}
				}
			}
			again := mustCompile(t, input)
			if again.Revision != plan.Revision || !reflect.DeepEqual(again.Resources, plan.Resources) {
				t.Fatal("Pod replacement must not require new controls or an Agent restart")
			}
		})
	}
}

func TestNativeServiceSelectorsRetainComplementaryDenies(t *testing.T) {
	input := rolloutEgressFixture()
	input.Platform.Topology.DNS = append(input.Platform.Topology.DNS, Peer{
		Name: "legacy-dns", Namespace: "legacy-system", UID: "legacy-dns-uid", IPs: []string{"10.96.0.11"},
	})
	input.Platform.Topology.Gateway = append(input.Platform.Topology.Gateway, Destination{
		Peer:  Peer{Name: "legacy-gateway", Namespace: "legacy-platform", UID: "legacy-gateway-uid", IPs: []string{"10.96.1.51"}},
		Ports: []api.AgentPolicyPort{{Port: 4000, Protocol: corev1.ProtocolTCP}},
	})
	plan := mustCompile(t, input)
	spec := plan.Network.Policy.Spec
	spec.Egress = append(slices.Clone(spec.Egress), EgressRule{ToEntities: []string{"all"}})
	gateway := input.Platform.Topology.Gateway[0].Peer
	for _, tc := range []struct {
		name     string
		pod      Peer
		port     int32
		protocol corev1.Protocol
		allowed  bool
	}{
		{"DNS TCP", Peer{Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}}, 53, corev1.ProtocolTCP, true},
		{"DNS UDP", Peer{Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}}, 53, corev1.ProtocolUDP, true},
		{"DNS other port", Peer{Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}}, 443, corev1.ProtocolTCP, false},
		{"DNS wrong namespace", Peer{Namespace: "tenant", PodSelector: map[string]string{"k8s-app": "kube-dns"}}, 53, corev1.ProtocolUDP, false},
		{"DNS wrong label", Peer{Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "other"}}, 53, corev1.ProtocolUDP, false},
		{"DNS missing label", Peer{Namespace: "kube-system"}, 53, corev1.ProtocolUDP, false},
		{"DNS old peer label only", Peer{Namespace: "kube-system", UID: input.Platform.Topology.DNS[0].UID}, 53, corev1.ProtocolUDP, false},
		{"Gateway TCP", Peer{Namespace: gateway.Namespace, PodSelector: gateway.PodSelector}, 4000, corev1.ProtocolTCP, true},
		{"Gateway UDP", Peer{Namespace: gateway.Namespace, PodSelector: gateway.PodSelector}, 4000, corev1.ProtocolUDP, false},
		{"Gateway other port", Peer{Namespace: gateway.Namespace, PodSelector: gateway.PodSelector}, 4001, corev1.ProtocolTCP, false},
		{"Gateway cannot act as DNS", Peer{Namespace: gateway.Namespace, PodSelector: gateway.PodSelector}, 53, corev1.ProtocolUDP, false},
		{"Gateway wrong namespace", Peer{Namespace: "tenant", PodSelector: gateway.PodSelector}, 4000, corev1.ProtocolTCP, false},
		{"Gateway missing component", Peer{Namespace: gateway.Namespace, PodSelector: map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/instance": "example"}}, 4000, corev1.ProtocolTCP, false},
		{"Gateway different release", Peer{Namespace: gateway.Namespace, PodSelector: map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway", "app.kubernetes.io/instance": "other"}}, 4000, corev1.ProtocolTCP, false},
		{"Gateway old peer label only", Peer{Namespace: gateway.Namespace, UID: gateway.UID}, 4000, corev1.ProtocolTCP, false},
		{"legacy DNS", input.Platform.Topology.DNS[1], 53, corev1.ProtocolUDP, true},
		{"legacy Gateway", input.Platform.Topology.Gateway[1].Peer, 4000, corev1.ProtocolTCP, true},
		{"legacy UID in wrong namespace", Peer{Namespace: gateway.Namespace, UID: "legacy-gateway-uid"}, 4000, corev1.ProtocolTCP, false},
		{"unrelated Pod", Peer{Namespace: "tenant"}, 443, corev1.ProtocolTCP, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syntheticEgressAllowed(spec, "10.244.1.25", &tc.pod, tc.port, tc.protocol); got != tc.allowed {
				t.Fatalf("selector union/complement mismatch: allowed=%t, want %t", got, tc.allowed)
			}
		})
	}
	for _, ip := range []string{"203.0.113.53", "2001:db8::53"} {
		if syntheticEgressAllowed(spec, ip, nil, 53, corev1.ProtocolUDP) {
			t.Fatal("service selectors permitted an arbitrary DNS resolver")
		}
	}
}

func TestNativeServiceSelectorsPreserveTerminalIsolation(t *testing.T) {
	input := rolloutEgressFixture()
	input.TerminalIngress = &TerminalIngress{Port: 7681}
	plan := mustCompile(t, input)
	terminal := plan.Network.TerminalPolicy
	if terminal == nil {
		t.Fatal("terminal did not receive its separate policy")
	}
	dns := Peer{Namespace: input.Platform.Topology.DNS[0].Namespace, PodSelector: input.Platform.Topology.DNS[0].PodSelector}
	for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
		if !syntheticEgressAllowed(terminal.Spec, "10.244.1.25", &dns, 53, protocol) {
			t.Fatal("replacement DNS Pod lost terminal access")
		}
	}
	gateway := Peer{Namespace: input.Platform.Topology.Gateway[0].Peer.Namespace, PodSelector: input.Platform.Topology.Gateway[0].Peer.PodSelector}
	if syntheticEgressAllowed(terminal.Spec, "10.244.1.26", &gateway, 4000, corev1.ProtocolTCP) ||
		syntheticEgressAllowed(terminal.Spec, "203.0.113.50", nil, 443, corev1.ProtocolTCP) {
		t.Fatal("service selectors gave the terminal model or internet access")
	}
	dns.Namespace = "tenant"
	if syntheticEgressAllowed(terminal.Spec, "10.244.1.25", &dns, 53, corev1.ProtocolUDP) {
		t.Fatal("terminal DNS selector lost its namespace boundary")
	}
}

func TestNativeServiceSelectorRegistrationValidation(t *testing.T) {
	for name, mutate := range map[string]func(*CompileInput){
		"DNS missing namespace":       func(i *CompileInput) { i.Platform.Topology.DNS[0].Namespace = "" },
		"Gateway missing namespace":   func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.Namespace = "" },
		"DNS missing Service IPs":     func(i *CompileInput) { i.Platform.Topology.DNS[0].IPs = nil },
		"Gateway missing Service IPs": func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.IPs = nil },
		"DNS backend IP pin": func(i *CompileInput) {
			i.Platform.Topology.DNS[0].IPs = append(i.Platform.Topology.DNS[0].IPs, "10.244.0.10")
		},
		"Gateway backend IP pin": func(i *CompileInput) {
			i.Platform.Topology.Gateway[0].Peer.IPs = append(i.Platform.Topology.Gateway[0].Peer.IPs, "10.244.0.20")
		},
		"external IP": func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.IPs = []string{"203.0.113.10"} },
		"invalid IP":  func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.IPs = []string{"invalid"} },
		"overlapping Pod range": func(i *CompileInput) {
			i.Platform.Topology.PodCIDRs = append(i.Platform.Topology.PodCIDRs, "10.96.0.0/12")
		},
		"namespace override": func(i *CompileInput) {
			i.Platform.Topology.DNS[0].PodSelector["io.kubernetes.pod.namespace"] = "tenant"
		},
		"Cilium label prefix": func(i *CompileInput) {
			i.Platform.Topology.DNS[0].PodSelector = map[string]string{"k8s:k8s-app": "kube-dns"}
		},
		"invalid label value": func(i *CompileInput) { i.Platform.Topology.DNS[0].PodSelector["k8s-app"] = "not a label" },
		"unapproved resolver": func(i *CompileInput) { i.Platform.Topology.DNSResolverIPs = []string{"10.96.0.11"} },
		"non-native profile": func(i *CompileInput) {
			i.Resolution.Capabilities = compilerFixture().Resolution.Capabilities
			i.Platform.Adapter = compilerFixture().Platform.Adapter
		},
		"arbitrary inventory selector": func(i *CompileInput) { i.Platform.Topology.Peers = []Peer{i.Platform.Topology.Gateway[0].Peer} },
		"API selector": func(i *CompileInput) {
			i.Platform.Topology.KubernetesAPI = []Destination{i.Platform.Topology.Gateway[0]}
		},
		"proxy CIDRs on egress": func(i *CompileInput) { i.Platform.Topology.Gateway[0].TrustedProxyCIDRs = []string{"10.244.0.0/16"} },
		"inconsistent UID": func(i *CompileInput) {
			peer := i.Platform.Topology.DNS[0]
			peer.PodSelector = map[string]string{"k8s-app": "other-dns"}
			i.Platform.Topology.DNS = append(i.Platform.Topology.DNS, peer)
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := rolloutEgressFixture()
			mutate(&input)
			if plan, err := Compile(input); err == nil || plan != nil {
				t.Fatalf("invalid service registration returned a plan: %v", err)
			}
			if err := ValidatePlatform(input.Platform, input.Resolution.Capabilities); err == nil {
				t.Fatal("platform assessment accepted invalid service registration")
			}
		})
	}
}

func TestNativeServiceSelectorsPreserveExplicitDenies(t *testing.T) {
	for _, cidr := range []string{"10.96.0.10/32", "10.96.1.50/32", "10.244.1.25/32", "10.244.0.0/16"} {
		t.Run(cidr, func(t *testing.T) {
			input := rolloutEgressFixture()
			input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{cidr}
			if plan, err := Compile(input); err == nil || plan != nil {
				t.Fatal("a mandatory service or changing backend cannot silently bypass a CIDR deny")
			}
		})
	}
	for _, port := range []api.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolTCP}, {Port: 53, Protocol: corev1.ProtocolUDP}, {Port: 4000, Protocol: corev1.ProtocolTCP}} {
		input := rolloutEgressFixture()
		input.Resolution.Capabilities.BlockedPorts = true
		input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{port}
		if plan, err := Compile(input); !errors.Is(err, ErrConflict) || plan != nil {
			t.Fatalf("required service bypassed an explicit port deny: %v", err)
		}
	}
}

func TestNativeServiceSelectorsBindStableRegistration(t *testing.T) {
	input := rolloutEgressFixture()
	plan := mustCompile(t, input)
	input.Platform.Topology.Gateway[0].Peer.IPs = append(input.Platform.Topology.Gateway[0].Peer.IPs, input.Platform.Topology.Gateway[0].Peer.IPs...)
	if again := mustCompile(t, input); again.Revision != plan.Revision || !reflect.DeepEqual(again.Resources, plan.Resources) {
		t.Fatal("duplicate Service addresses changed effective controls")
	}
	for name, mutate := range map[string]func(*CompileInput){
		"DNS selector": func(i *CompileInput) { i.Platform.Topology.DNS[0].PodSelector["k8s-app"] = "other-dns" },
		"Gateway selector": func(i *CompileInput) {
			i.Platform.Topology.Gateway[0].Peer.PodSelector["app.kubernetes.io/component"] = "other"
		},
		"namespace":  func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.Namespace = "other-platform" },
		"identity":   func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.UID = "other-gateway-uid" },
		"Service IP": func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.IPs = []string{"10.96.1.51"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := rolloutEgressFixture()
			mutate(&changed)
			if mustCompile(t, changed).Revision == plan.Revision {
				t.Fatal("changed service authorization retained the old revision")
			}
		})
	}
}

func TestNativeServiceSelectorExpansionIsBounded(t *testing.T) {
	input := rolloutEgressFixture()
	input.Platform.Topology.Gateway = nil
	for i := range 24 {
		input.Platform.Topology.Gateway = append(input.Platform.Topology.Gateway, Destination{
			Peer: Peer{
				Name: fmt.Sprintf("gateway-%d", i), Namespace: fmt.Sprintf("platform-%d", i), UID: types.UID(fmt.Sprintf("gateway-uid-%d", i)),
				IPs: []string{fmt.Sprintf("10.96.1.%d", i+1)}, PodSelector: map[string]string{fmt.Sprintf("gateway-%d", i): "model"},
			},
			Ports: []api.AgentPolicyPort{{Port: 4000, Protocol: corev1.ProtocolTCP}},
		})
	}
	if plan, err := Compile(input); !errors.Is(err, ErrConflict) || plan != nil {
		t.Fatalf("selector expansion must reject excess rather than omit denies: %v", err)
	}
}
