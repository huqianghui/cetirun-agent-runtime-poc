// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	corev1 "k8s.io/api/core/v1"
)

func TestIngressMTLSDesktopRequiresRegisteredTraversal(t *testing.T) {
	for _, harness := range []string{"openclaw", "hermes"} {
		t.Run(harness, func(t *testing.T) {
			input := ingressMTLSFixture(t, harness)
			input.Platform.IngressMTLS.Bindings[0].Desktop = true
			if _, err := Compile(input); err == nil {
				t.Fatal("desktop acquired an unregistered application ingress port")
			}
			input.Platform.Topology.Ingress[0].Ports = append(input.Platform.Topology.Ingress[0].Ports,
				api.AgentPolicyPort{Port: 6080, Protocol: corev1.ProtocolTCP})
			plan := mustCompile(t, input)
			labels := map[string]string{"k8s:io.kubernetes.pod.namespace": "aks-istio-ingress",
				"k8s:istio": "aks-istio-ingressgateway-external"}
			spec := plan.Network.Policy.Spec
			spec.Ingress = append(spec.Ingress, IngressRule{FromEntities: []string{"all"}})
			if !syntheticIngressAllowed(spec, labels, "", ingressmtls.DesktopPort, corev1.ProtocolTCP) {
				t.Fatal("registered noVNC TLS path is absent")
			}
			for _, port := range []int32{5900, 6080, 18800} {
				if syntheticIngressAllowed(spec, labels, "", port, corev1.ProtocolTCP) {
					t.Fatal("desktop plaintext/backend port bypassed TLS:", port)
				}
			}
		})
	}
}
