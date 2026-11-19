// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func ingressMTLSFixture(t *testing.T, harness string) CompileInput {
	t.Helper()
	input := rolloutIngressFixture(t)
	input.ServiceName = "openclaw-example-cetirun"
	input.Resolution.Capabilities.MutualTLS = true
	input.Resolution.AgentPolicy.Network.Ingress.RequireMTLS = true
	port := int32(18789)
	if harness == "hermes" {
		port = 8642
	}
	ref := func(namespace, name string) ingressmtls.Reference {
		return ingressmtls.Reference{Namespace: namespace, Name: name, UID: types.UID("synthetic-" + name), ResourceVersion: "1"}
	}
	binding := ingressmtls.Binding{
		AgentUID: input.Resolution.Binding.AgentUID, Namespace: input.Namespace, Harness: harness, Port: port, Terminal: true,
		ServerSecret: ref(input.Namespace, "agent-tls"), ClientSecret: ref("aks-istio-ingress", "gateway-client"),
		Gateway: ref("aks-istio-ingress", "ingress"), ClientURI: "spiffe://example.com/gateway/" + string(input.Resolution.Binding.AgentUID),
	}
	input.Platform.IngressMTLS = &ingressmtls.Registration{Image: "registry.example.com/envoy@sha256:" + strings.Repeat("a", 64),
		Bindings: []ingressmtls.Binding{binding}}
	input.Platform.Topology.Ingress[0].Ports = nil
	for _, selected := range (ingressmtls.Plan{Binding: binding}).Ports() {
		input.Platform.Topology.Ingress[0].Ports = append(input.Platform.Topology.Ingress[0].Ports,
			api.AgentPolicyPort{Port: selected.Backend, Protocol: corev1.ProtocolTCP})
	}
	return input
}

func TestIngressMTLSCompilerMandatoryTraversal(t *testing.T) {
	for _, harness := range []string{"openclaw", "hermes"} {
		t.Run(harness, func(t *testing.T) {
			input := ingressMTLSFixture(t, harness)
			plan := mustCompile(t, input)
			if plan.IngressMTLS == nil || plan.IngressMTLS.ServerName == plan.IngressMTLS.ServiceHost {
				t.Fatal("mTLS lacks an Agent UID-specific serving identity")
			}
			labels := map[string]string{"k8s:io.kubernetes.pod.namespace": "aks-istio-ingress",
				"k8s:istio": "aks-istio-ingressgateway-external"}
			spec := plan.Network.Policy.Spec
			spec.Ingress = append(slices.Clone(spec.Ingress), IngressRule{FromEntities: []string{"all"}})
			for _, port := range plan.IngressMTLS.Ports() {
				if !syntheticIngressAllowed(spec, labels, "", port.TLS, corev1.ProtocolTCP) ||
					syntheticIngressAllowed(spec, labels, "", port.Backend, corev1.ProtocolTCP) {
					t.Fatal("managed Gateway can bypass TLS or cannot reach its protected endpoint")
				}
				if syntheticIngressAllowed(spec, map[string]string{"k8s:io.kubernetes.pod.namespace": "other"}, "", port.TLS, corev1.ProtocolTCP) {
					t.Fatal("a different peer acquired mTLS ingress")
				}
			}
			input.Resolution.AgentPolicy.Network.Ingress.RequireMTLS = false
			ordinary := mustCompile(t, input)
			if ordinary.IngressMTLS != nil || len(ordinary.Resources) != 1 {
				t.Fatal("requireMTLS=false acquired an endpoint or TLS policy")
			}
			if !syntheticIngressAllowed(ordinary.Network.Policy.Spec, labels, "", plan.IngressMTLS.Port, corev1.ProtocolTCP) {
				t.Fatal("ordinary ingress behavior changed")
			}
		})
	}
}

func TestIngressMTLSCompilerRejectsIncompleteApproval(t *testing.T) {
	for name, mutate := range map[string]func(*CompileInput){
		"missing registration": func(i *CompileInput) { i.Platform.IngressMTLS = nil },
		"bare capability":      func(i *CompileInput) { i.Resolution.Capabilities.MutualTLS = false },
		"other Agent":          func(i *CompileInput) { i.Resolution.Binding.AgentUID = "other-agent" },
		"other namespace":      func(i *CompileInput) { i.Namespace = "other-agent" },
		"tag-only image":       func(i *CompileInput) { i.Platform.IngressMTLS.Image = "envoyproxy/envoy:v1.36.3" },
		"wildcard URI":         func(i *CompileInput) { i.Platform.IngressMTLS.Bindings[0].ClientURI = "spiffe://example.com/*" },
		"wrong client scope": func(i *CompileInput) {
			i.Platform.IngressMTLS.Bindings[0].ClientURI = "spiffe://example.com/gateway/other-agent"
		},
		"missing Secret UID":     func(i *CompileInput) { i.Platform.IngressMTLS.Bindings[0].ServerSecret.UID = "" },
		"missing Secret version": func(i *CompileInput) { i.Platform.IngressMTLS.Bindings[0].ClientSecret.ResourceVersion = "" },
		"client key in Agent":    func(i *CompileInput) { i.Platform.IngressMTLS.Bindings[0].ClientSecret.Namespace = i.Namespace },
		"unapproved port":        func(i *CompileInput) { i.Platform.Topology.Ingress[0].Ports = i.Platform.Topology.Ingress[0].Ports[:1] },
		"disabled ingress":       func(i *CompileInput) { i.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressDisabled },
		"legacy execution":       func(i *CompileInput) { i.NativeExecution = false },
	} {
		t.Run(name, func(t *testing.T) {
			input := ingressMTLSFixture(t, "hermes")
			mutate(&input)
			if plan, err := Compile(input); err == nil || plan != nil {
				t.Fatal("incomplete mTLS approval compiled a partial workload")
			}
		})
	}
}

func TestIngressMTLSRevisionPinsCertificateApproval(t *testing.T) {
	input := ingressMTLSFixture(t, "openclaw")
	before := mustCompile(t, input)
	input.Platform.IngressMTLS.Bindings[0].ServerSecret.ResourceVersion = "2"
	after := mustCompile(t, input)
	if before.Revision == after.Revision {
		t.Fatal("certificate rotation did not change the protected execution revision")
	}

	raw, err := json.Marshal(after.Resources)
	if err != nil || !strings.Contains(string(raw), `"mode":"MUTUAL"`) ||
		!strings.Contains(string(raw), `"subjectAltNames"`) || strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("compiled TLS resources lost exact peer verification or contained key material:", err)
	}
}
