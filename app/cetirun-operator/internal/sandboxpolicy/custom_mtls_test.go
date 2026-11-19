// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func customMTLSFixture(t *testing.T) CompileInput {
	t.Helper()
	input := ingressMTLSFixture(t, "custom")
	input.Resolution.AgentPolicy.Harnesses.Allowed = []api.AgentType{api.AgentTypeCustom}
	input.CustomIngress = &CustomIngress{Port: 8080, ChatCallers: []IngressCaller{{
		Namespace: "portal", PodLabels: map[string]string{"app": "portal"},
	}}}
	binding := &input.Platform.IngressMTLS.Bindings[0]
	binding.Port, binding.Terminal = 8080, false
	binding.ChatClients = []ingressmtls.ChatClient{{
		Secret: ingressmtls.Reference{Namespace: "portal", Name: "chat-client", UID: "chat-client-uid", ResourceVersion: "1"},
		URI:    "spiffe://example.com/portal/" + string(binding.AgentUID), PodLabels: map[string]string{"app": "portal"},
	}}
	return input
}

func TestCustomMTLSRequiresExactClientAndBlocksPlaintext(t *testing.T) {
	input := customMTLSFixture(t)
	binding := &input.Platform.IngressMTLS.Bindings[0]
	plan := mustCompile(t, input)
	spec := plan.Network.Policy.Spec
	spec.Ingress = append(slices.Clone(spec.Ingress), IngressRule{FromEntities: []string{"all"}})
	portal := map[string]string{"k8s:io.kubernetes.pod.namespace": "portal", "k8s:app": "portal"}
	if !syntheticIngressAllowed(spec, portal, "", ingressmtls.APIPort, corev1.ProtocolTCP) ||
		syntheticIngressAllowed(spec, portal, "", 8080, corev1.ProtocolTCP) {
		t.Fatal("Custom Chat can bypass TLS or cannot reach its approved listener")
	}
	portal["k8s:app"] = "other"
	if syntheticIngressAllowed(spec, portal, "", ingressmtls.APIPort, corev1.ProtocolTCP) {
		t.Fatal("unapproved Portal workload acquired ingress")
	}
	binding.ChatClients[0].PodLabels["app"] = "other"
	if _, err := Compile(input); err == nil {
		t.Fatal("mismatched client approval accepted")
	}
	binding.ChatClients = nil
	if _, err := Compile(input); err == nil {
		t.Fatal("Custom Chat was allowed to borrow Gateway credentials")
	}
}

func TestCustomMTLSPortalFixture(t *testing.T) {
	input := customMTLSFixture(t)
	input.AgentName, input.Namespace = "chat-example", "cetirun-chat-example"
	input.ServiceName = chataccess.ServiceName(input.AgentName)
	input.Resolution.Binding.AgentUID = "11111111-1111-4111-8111-111111111111"
	input.CustomIngress.ChatCallers[0] = IngressCaller{
		Namespace: "test-platform", PodLabels: map[string]string{"app": "test-portal"},
	}
	binding := &input.Platform.IngressMTLS.Bindings[0]
	binding.AgentUID, binding.Namespace = input.Resolution.Binding.AgentUID, input.Namespace
	binding.ServerSecret.Namespace = input.Namespace
	binding.ClientURI = "spiffe://example.com/gateway/" + string(binding.AgentUID)
	binding.ChatClients[0] = ingressmtls.ChatClient{
		Secret: ingressmtls.Reference{Namespace: "test-platform", Name: "chat-client", UID: "chat-client-uid", ResourceVersion: "1"},
		URI:    "spiffe://example.com/portal/" + string(binding.AgentUID), PodLabels: input.CustomIngress.ChatCallers[0].PodLabels,
	}
	plan := mustCompile(t, input)
	fixture := struct {
		Revision         string                      `json:"revision"`
		CustomIngressTLS *ingressmtls.Plan           `json:"customIngressTLS"`
		Controls         []unstructured.Unstructured `json:"controls"`
	}{Revision: plan.Revision, CustomIngressTLS: plan.IngressMTLS}
	for _, control := range plan.Resources {
		if control.GetKind() != "ConfigMap" {
			fixture.Controls = append(fixture.Controls, control)
		}
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("..", "..", "..", "cetirun-portal", "internal", "chattest", "testdata", "custom-mtls.json")
	if os.Getenv("CETIRUN_UPDATE_CHAT_FIXTURE") == "true" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("Portal mTLS fixture differs from compiler output; regenerate with CETIRUN_UPDATE_CHAT_FIXTURE=true:", err)
	}
}
