// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestCompileCustomIngressBindsEndpointAndCaller(t *testing.T) {
	input := compilerFixture()
	input.CustomIngress = &CustomIngress{
		Port: 8088, WebUI: true,
		ChatCallers: []IngressCaller{{Namespace: "portal-system", PodLabels: map[string]string{"app": "portal"}}},
	}
	plan := mustCompile(t, input)
	gateway := map[string]string{"k8s:io.kubernetes.pod.namespace": "platform", "k8s:" + PeerUIDLabel: "example-ingress-uid"}
	caller := map[string]string{"k8s:io.kubernetes.pod.namespace": "portal-system", "k8s:app": "portal"}
	if !syntheticIngressAllowed(plan.Network.Policy.Spec, gateway, "", 8088, corev1.ProtocolTCP) ||
		!syntheticIngressAllowed(plan.Network.Policy.Spec, caller, "", 8088, corev1.ProtocolTCP) {
		t.Fatal("declared Custom callers cannot reach the requested endpoint")
	}
	if syntheticIngressAllowed(plan.Network.Policy.Spec, caller, "", 8089, corev1.ProtocolTCP) ||
		syntheticIngressAllowed(plan.Network.Policy.Spec, map[string]string{"k8s:io.kubernetes.pod.namespace": "portal-system", "k8s:app": "foreign"}, "", 8088, corev1.ProtocolTCP) {
		t.Fatal("Custom ingress widened its caller or port")
	}
	if plan.Revision == mustCompile(t, compilerFixture()).Revision {
		t.Fatal("Custom ingress was omitted from the control revision")
	}
	input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressDisabled
	if got := mustCompile(t, input).Network.Policy.Spec.Ingress; len(got) != 0 {
		t.Fatal("disabled ingress retained Custom exposure")
	}
	invalid := compilerFixture()
	invalid.CustomIngress = &CustomIngress{Port: 8088, ChatCallers: []IngressCaller{{Namespace: "portal-system"}}}
	if _, err := Compile(invalid); !errors.Is(err, ErrUnsupportedControl) {
		t.Fatal("unselected Custom Chat caller accepted:", err)
	}
}

func TestCompileCustomTerminalIsSeparateFromAgentPrivileges(t *testing.T) {
	input := compilerFixture()
	input.TerminalIngress = &TerminalIngress{Port: 7681}
	plan := mustCompile(t, input)
	terminal := plan.Network.TerminalPolicy
	if terminal == nil || len(plan.Resources) != 2 {
		t.Fatal("terminal did not receive a separate compiled policy")
	}
	labels := map[string]string{
		"k8s:" + AgentUIDLabel: "example-agent-uid", "k8s:io.kubernetes.pod.namespace": "cetirun-example-agent",
		"k8s:cetirun.microsoft.com/terminal": "true",
	}
	if syntheticSelectorMatches(plan.Network.Policy.Spec.EndpointSelector, labels) ||
		!syntheticSelectorMatches(terminal.Spec.EndpointSelector, labels) {
		t.Fatal("main policy would union its privileges into the terminal")
	}
	gateway := map[string]string{"k8s:" + PeerUIDLabel: "example-ingress-uid", "k8s:io.kubernetes.pod.namespace": "platform"}
	if !syntheticIngressAllowed(terminal.Spec, gateway, "", 7681, corev1.ProtocolTCP) ||
		syntheticIngressAllowed(terminal.Spec, gateway, "", 7682, corev1.ProtocolTCP) {
		t.Fatal("terminal ingress did not preserve the exact authenticated port")
	}
	dns := input.Platform.Topology.DNS[0]
	if !syntheticEgressAllowed(terminal.Spec, "10.96.0.10", &dns, 53, corev1.ProtocolUDP) ||
		!syntheticEgressAllowed(terminal.Spec, "203.0.113.10", nil, 6443, corev1.ProtocolTCP) ||
		syntheticEgressAllowed(terminal.Spec, "198.51.100.10", nil, 443, corev1.ProtocolTCP) {
		t.Fatal("terminal egress exceeded DNS and Kubernetes API authority")
	}
	if plan.Revision == mustCompile(t, compilerFixture()).Revision {
		t.Fatal("terminal exposure was omitted from the control revision")
	}
	input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressDisabled
	if got := mustCompile(t, input).Network.TerminalPolicy.Spec.Ingress; len(got) != 0 {
		t.Fatal("disabled ingress retained terminal exposure")
	}
	input.Platform.Topology.KubernetesAPI = nil
	if _, err := Compile(input); !errors.Is(err, ErrUnavailable) {
		t.Fatal("terminal without registered API endpoints compiled:", err)
	}
}
