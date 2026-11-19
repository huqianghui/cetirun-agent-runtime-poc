// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func rolloutIngressFixture(t *testing.T) CompileInput {
	t.Helper()
	input := nativeCompilerFixture()
	input.Platform.Topology.Ingress = nil
	if err := json.Unmarshal([]byte(`[{
		"peer": {
			"name": "aks-istio-ingressgateway-external",
			"namespace": "aks-istio-ingress",
			"uid": "example-ingress-deployment-uid",
			"podSelector": {"istio": "aks-istio-ingressgateway-external"}
		},
		"trustedProxyCIDRs": ["10.244.0.0/16"],
		"ports": [{"port": 8080, "protocol": "TCP"}]
	}]`), &input.Platform.Topology.Ingress); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestNativeIngressSurvivesGatewayPodReplacement(t *testing.T) {
	input := rolloutIngressFixture(t)
	plan := mustCompile(t, input)
	if !slices.Equal(plan.Network.TrustedProxyIPs, []string{"10.244.0.0/16"}) {
		t.Fatalf("proxy trust must follow the registered Pod network, not a Pod IP: %v", plan.Network.TrustedProxyIPs)
	}
	labels := map[string]string{
		"k8s:io.kubernetes.pod.namespace": "aks-istio-ingress",
		"k8s:istio":                       "aks-istio-ingressgateway-external",
	}
	for _, ip := range []string{"10.244.0.10", "10.244.1.25", "10.244.255.250"} {
		if !syntheticIngressAllowed(plan.Network.Policy.Spec, labels, "", 8080, corev1.ProtocolTCP) ||
			!netip.MustParsePrefix(plan.Network.TrustedProxyIPs[0]).Contains(netip.MustParseAddr(ip)) {
			t.Fatalf("replacement gateway at %s lost ingress or proxy trust", ip)
		}
	}
	if again := mustCompile(t, input); again.Revision != plan.Revision || !reflect.DeepEqual(again.Resources, plan.Resources) {
		t.Fatal("gateway Pod replacement must not require a new control revision")
	}
	spec := plan.Network.Policy.Spec
	spec.Ingress = append(slices.Clone(spec.Ingress), IngressRule{FromEntities: []string{"all"}})
	for name, source := range map[string]map[string]string{
		"same namespace without gateway label": {"k8s:io.kubernetes.pod.namespace": "aks-istio-ingress"},
		"same label in another namespace": {
			"k8s:io.kubernetes.pod.namespace": "tenant",
			"k8s:istio":                       "aks-istio-ingressgateway-external",
		},
		"old peer label without gateway label": {
			"k8s:io.kubernetes.pod.namespace": "aks-istio-ingress",
			"k8s:" + PeerUIDLabel:             "example-ingress-deployment-uid",
		},
	} {
		if syntheticIngressAllowed(spec, source, "", 8080, corev1.ProtocolTCP) {
			t.Fatalf("%s gained native ingress", name)
		}
	}
	for _, entity := range []string{"world", "host", "remote-node"} {
		if syntheticIngressAllowed(spec, nil, entity, 8080, corev1.ProtocolTCP) {
			t.Fatalf("%s gained native ingress", entity)
		}
	}
	for _, port := range []int32{53, 443, 8081} {
		if syntheticIngressAllowed(spec, labels, "", port, corev1.ProtocolTCP) {
			t.Fatalf("gateway gained unregistered ingress port %d", port)
		}
	}
	if syntheticIngressAllowed(spec, labels, "", 8080, corev1.ProtocolUDP) {
		t.Fatal("gateway gained unregistered ingress protocol")
	}
}

func TestNativeIngressSelectorsRetainComplementaryDenies(t *testing.T) {
	input := rolloutIngressFixture(t)
	input.Platform.Topology.Ingress[0].Peer.PodSelector["component"] = "ingress"
	input.Platform.Topology.Ingress = append(input.Platform.Topology.Ingress, Destination{
		Peer: Peer{
			Name: "second-ingress", Namespace: "second-platform", UID: "second-ingress-uid",
			PodSelector: map[string]string{"app": "gateway", "component": "external"},
		},
		Ports: []api.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolTCP}}, TrustedProxyCIDRs: []string{"10.244.0.0/16"},
	}, compilerFixture().Platform.Topology.Ingress[0])
	plan := mustCompile(t, input)
	spec := plan.Network.Policy.Spec
	spec.Ingress = append(slices.Clone(spec.Ingress), IngressRule{FromEntities: []string{"all"}})
	for _, tc := range []struct {
		namespace, istio, app, component, uid string
		allowed                               bool
	}{
		{"aks-istio-ingress", "aks-istio-ingressgateway-external", "", "ingress", "", true},
		{"second-platform", "", "gateway", "external", "", true},
		{"platform", "", "", "", "example-ingress-uid", true},
		{"aks-istio-ingress", "aks-istio-ingressgateway-external", "", "", "", false},
		{"aks-istio-ingress", "", "gateway", "external", "", false},
		{"second-platform", "aks-istio-ingressgateway-external", "", "ingress", "", false},
		{"platform", "aks-istio-ingressgateway-external", "gateway", "external", "", false},
		{"tenant", "aks-istio-ingressgateway-external", "gateway", "external", "example-ingress-uid", false},
		{"aks-istio-ingress", "", "", "", "example-ingress-uid", false},
		{"second-platform", "", "gateway", "ingress", "", false},
	} {
		labels := map[string]string{"k8s:io.kubernetes.pod.namespace": tc.namespace}
		for key, value := range map[string]string{"istio": tc.istio, "app": tc.app, "component": tc.component, PeerUIDLabel: tc.uid} {
			if value != "" {
				labels["k8s:"+key] = value
			}
		}
		if got := syntheticIngressAllowed(spec, labels, "", 8080, corev1.ProtocolTCP); got != tc.allowed {
			t.Fatalf("selector union/complement mismatch for %v: got %v, want %v", labels, got, tc.allowed)
		}
	}
}

func TestNativeIngressCanonicalizesAndBindsStableRegistration(t *testing.T) {
	input := rolloutIngressFixture(t)
	input.Platform.Topology.PodCIDRs = append(input.Platform.Topology.PodCIDRs, "fd00:244::/64")
	input.Platform.Topology.Ingress[0].TrustedProxyCIDRs = []string{"fd00:244::1/64", "10.244.0.1/16", "10.244.0.0/16"}
	plan := mustCompile(t, input)
	if !slices.Equal(plan.Network.TrustedProxyIPs, []string{"10.244.0.0/16", "fd00:244::/64"}) {
		t.Fatal("Pod CIDRs were not canonicalized and deduplicated")
	}
	input.Platform.Topology.Ingress[0].TrustedProxyCIDRs = []string{"10.244.0.0/16", "fd00:244::/64"}
	if mustCompile(t, input).Revision != plan.Revision {
		t.Fatal("CIDR ordering, host bits or duplicates changed effective controls")
	}
	for name, mutate := range map[string]func(*CompileInput){
		"selector":  func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.PodSelector["istio"] = "other-gateway" },
		"namespace": func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.Namespace = "other-platform" },
		"identity":  func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.UID = "other-deployment-uid" },
		"CIDR":      func(i *CompileInput) { i.Platform.Topology.Ingress[0].TrustedProxyCIDRs = []string{"10.244.0.0/24"} },
	} {
		t.Run(name, func(t *testing.T) {
			original := rolloutIngressFixture(t)
			before := mustCompile(t, original)
			mutate(&original)
			if mustCompile(t, original).Revision == before.Revision {
				t.Fatal("changed ingress authorization did not change the control revision")
			}
		})
	}
	first := input.Platform.Topology.Ingress[0]
	first.TrustedProxyCIDRs = []string{"10.244.1.0/24"}
	first.Ports = []api.AgentPolicyPort{{Port: 9090, Protocol: corev1.ProtocolTCP}}
	input.Platform.Topology.Ingress = append(input.Platform.Topology.Ingress, first)
	merged := mustCompile(t, input)
	if len(merged.Network.Policy.Spec.Ingress) != 1 ||
		len(merged.Network.Policy.Spec.Ingress[0].ToPorts[0].Ports) != 2 ||
		!slices.Equal(merged.Network.TrustedProxyIPs, []string{"10.244.0.0/16", "10.244.1.0/24", "fd00:244::/64"}) {
		t.Fatal("same-peer ports and trusted ranges must merge before complementary denies")
	}
	input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressDisabled
	disabled := mustCompile(t, input)
	if len(disabled.Network.TrustedProxyIPs) != 0 || len(disabled.Network.Policy.Spec.Ingress) != 0 {
		t.Fatal("disabled ingress gained selector-based access or proxy trust")
	}
}

func TestNativeIngressRejectsUnboundedOrAmbiguousRegistration(t *testing.T) {
	for name, mutate := range map[string]func(*CompileInput){
		"missing namespace": func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.Namespace = "" },
		"pinned Pod IPs":    func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.IPs = []string{"10.244.0.10"} },
		"missing selector":  func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.PodSelector = nil },
		"empty selector":    func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.PodSelector = map[string]string{} },
		"invalid label key": func(i *CompileInput) {
			i.Platform.Topology.Ingress[0].Peer.PodSelector = map[string]string{"k8s:istio": "gateway"}
		},
		"invalid label value": func(i *CompileInput) {
			i.Platform.Topology.Ingress[0].Peer.PodSelector["istio"] = "not a label"
		},
		"namespace identity override": func(i *CompileInput) {
			i.Platform.Topology.Ingress[0].Peer.PodSelector["io.kubernetes.pod.namespace"] = "tenant"
		},
		"missing CIDRs": func(i *CompileInput) { i.Platform.Topology.Ingress[0].TrustedProxyCIDRs = nil },
		"outside Pod CIDRs": func(i *CompileInput) {
			i.Platform.Topology.Ingress[0].TrustedProxyCIDRs = []string{"203.0.113.0/24"}
		},
		"service overlap": func(i *CompileInput) {
			i.Platform.Topology.ServiceCIDRs = append(i.Platform.Topology.ServiceCIDRs, "10.244.1.0/24")
		},
		"node overlap": func(i *CompileInput) {
			i.Platform.Topology.NodeCIDRs = append(i.Platform.Topology.NodeCIDRs, "10.244.1.0/24")
		},
		"management overlap": func(i *CompileInput) {
			i.Platform.Topology.ManagementCIDRs = append(i.Platform.Topology.ManagementCIDRs, "10.244.1.0/24")
		},
		"metadata overlap": func(i *CompileInput) {
			i.Platform.Topology.MetadataCIDRs = append(i.Platform.Topology.MetadataCIDRs, "10.244.1.0/24")
		},
		"proxy CIDRs on egress destination": func(i *CompileInput) {
			i.Platform.Topology.Gateway = append(i.Platform.Topology.Gateway, i.Platform.Topology.Ingress[0])
		},
		"DNS selector without Service IPs": func(i *CompileInput) {
			i.Platform.Topology.DNS = append(i.Platform.Topology.DNS, i.Platform.Topology.Ingress[0].Peer)
		},
		"inventory selector": func(i *CompileInput) {
			i.Platform.Topology.Peers = append(i.Platform.Topology.Peers, i.Platform.Topology.Ingress[0].Peer)
		},
		"CIDRs on legacy peer": func(i *CompileInput) {
			i.Platform.Topology.Ingress[0].Peer = compilerFixture().Platform.Topology.Ingress[0].Peer
		},
		"qualified profile": func(i *CompileInput) {
			i.Resolution.Capabilities = compilerFixture().Resolution.Capabilities
			i.Platform.Adapter = compilerFixture().Platform.Adapter
		},
		"inconsistent peer identity": func(i *CompileInput) {
			second := i.Platform.Topology.Ingress[0]
			second.Peer = Peer{Name: second.Peer.Name, Namespace: second.Peer.Namespace, UID: second.Peer.UID, PodSelector: map[string]string{"app": "other-gateway"}}
			i.Platform.Topology.Ingress = append(i.Platform.Topology.Ingress, second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := rolloutIngressFixture(t)
			mutate(&input)
			if plan, err := Compile(input); err == nil || plan != nil {
				t.Fatalf("invalid stable ingress produced a plan: %v", err)
			}
			if err := ValidatePlatform(input.Platform, input.Resolution.Capabilities); err == nil {
				t.Fatal("standalone platform validation accepted invalid stable ingress")
			}
		})
	}
	for _, cidr := range []string{
		"0.0.0.0/0", "::/0", "0.0.0.0/1", "128.0.0.0/1", "127.0.0.0/8", "169.254.0.0/16",
		"224.0.0.0/4", "fe80::/64", "::ffff:10.244.0.0/112", "10.244.0.1", "not-a-cidr", "10.244.0.0/33",
	} {
		t.Run(cidr, func(t *testing.T) {
			input := rolloutIngressFixture(t)
			input.Platform.Topology.Ingress[0].TrustedProxyCIDRs = []string{cidr}
			if plan, err := Compile(input); err == nil || plan != nil {
				t.Fatal("invalid proxy CIDR compiled:", cidr)
			}
		})
	}
}

func TestIngressSelectorExpansionIsBounded(t *testing.T) {
	var peers []Peer
	for i := range 24 {
		peers = append(peers, Peer{
			Name: fmt.Sprintf("gateway-%d", i), Namespace: fmt.Sprintf("platform-%d", i), UID: types.UID(fmt.Sprintf("gateway-uid-%d", i)),
			PodSelector: map[string]string{fmt.Sprintf("gateway-%d", i): "ingress"},
		})
	}
	if selectors, err := excludingPeers(peers); !errors.Is(err, ErrConflict) || selectors != nil {
		t.Fatalf("selector expansion must reject excess, not return partial denies: %v", err)
	}
}

func syntheticIngressAllowed(spec CiliumPolicySpec, labels map[string]string, entity string, port int32, protocol corev1.Protocol) bool {
	matches := func(rule IngressRule) bool {
		selected := slices.Contains(rule.FromEntities, "all") || entity != "" && slices.Contains(rule.FromEntities, entity)
		if labels != nil {
			for _, selector := range rule.FromEndpoints {
				selected = selected || syntheticSelectorMatches(selector, labels)
			}
		}
		return selected && syntheticPortsMatch(rule.ToPorts, port, protocol)
	}
	for _, rule := range spec.IngressDeny {
		if matches(rule) {
			return false
		}
	}
	for _, rule := range spec.Ingress {
		if matches(rule) {
			return true
		}
	}
	return false
}
