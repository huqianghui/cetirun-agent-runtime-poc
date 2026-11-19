// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chattest

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"maps"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed testdata/custom-mtls.json
var compiledMTLS []byte

func MTLSObjects(t *testing.T) (*api.Agent, []client.Object) {
	t.Helper()
	var compiled struct {
		Revision         string                      `json:"revision"`
		CustomIngressTLS json.RawMessage             `json:"customIngressTLS"`
		Controls         []unstructured.Unstructured `json:"controls"`
	}
	if err := json.Unmarshal(compiledMTLS, &compiled); err != nil {
		t.Fatal(err)
	}
	var ingress struct {
		ChatClients []struct {
			Secret map[string]any `json:"secret"`
		} `json:"chatClients"`
	}
	if err := json.Unmarshal(compiled.CustomIngressTLS, &ingress); err != nil {
		t.Fatal(err)
	}
	agent, objects := Objects()
	agent.Spec.SandboxRef = "sandbox-example"
	agent.Status.Sandbox = &api.SandboxStatus{
		TeamUID: "synthetic-team", SandboxUID: "synthetic-sandbox", AgentPolicyUID: "synthetic-policy", AppliedRevision: compiled.Revision,
	}
	for _, kind := range []string{api.ConditionSandboxReady, api.ConditionPolicyApplied} {
		agent.Status.Conditions = append(agent.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionTrue, ObservedGeneration: agent.Generation, Reason: "SyntheticReady", LastTransitionTime: metav1.Now(),
		})
	}
	team := objects[6].(*api.Team)
	team.Spec.AgentPolicyRef = "example-policy"
	team.Spec.SandboxRefs, team.Spec.DefaultSandboxRef = []string{agent.Spec.SandboxRef}, agent.Spec.SandboxRef
	owner := *metav1.NewControllerRef(agent, api.GroupVersion.WithKind("Agent"))
	ns := objects[1].(*corev1.Namespace)
	ns.OwnerReferences = []metav1.OwnerReference{owner}
	svc := objects[4].(*corev1.Service)
	svc.OwnerReferences = []metav1.OwnerReference{owner}
	svc.Spec.Ports[0].TargetPort = intstr.FromInt32(chataccess.MTLSAPIPort)
	sts := objects[2].(*appsv1.StatefulSet)
	sts.Spec.Template.Annotations = map[string]string{
		"sandbox.cetirun.microsoft.com/reconciled": "true", "cetirun.microsoft.com/sandbox-revision": compiled.Revision,
		chataccess.AgentUIDAnnotation: string(agent.UID),
	}
	pod := objects[3].(*corev1.Pod)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "ingress-mtls", Image: "registry.example.com/envoy@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Ports: []corev1.ContainerPort{{Name: "mtls-http", ContainerPort: chataccess.MTLSAPIPort, Protocol: corev1.ProtocolTCP}},
	})
	sts.Spec.Template.Spec = *pod.Spec.DeepCopy()
	pod.Annotations = maps.Clone(sts.Spec.Template.Annotations)
	intent, err := agent.SandboxExecutionIntentDigest()
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"version": "v2", "agent": map[string]any{"name": agent.Name, "uid": agent.UID}, "agentGeneration": agent.Generation,
		"namespace": map[string]any{"name": ns.Name, "uid": ns.UID},
		"binding": map[string]any{"agentUID": agent.UID, "teamUID": agent.Status.Sandbox.TeamUID,
			"sandboxUID": agent.Status.Sandbox.SandboxUID, "agentPolicyUID": agent.Status.Sandbox.AgentPolicyUID},
		"revision": compiled.Revision, "planHash": "synthetic-plan",
		"workloads": []any{map[string]any{"kind": "StatefulSet", "identity": map[string]any{"name": sts.Name, "uid": sts.UID},
			"replicas": 1, "template": sts.Spec.Template}},
		"sources": []any{map[string]any{"apiVersion": api.GroupVersion.String(), "kind": "Agent", "name": agent.Name,
			"uid": agent.UID, "generation": agent.Generation, "intentDigest": intent}},
		"controls": compiled.Controls, "customIngressTLS": compiled.CustomIngressTLS,
	}
	sources := expected["sources"].([]any)
	for _, approved := range ingress.ChatClients {
		source := maps.Clone(approved.Secret)
		source["apiVersion"], source["kind"] = "v1", "Secret"
		sources = append(sources, source)
	}
	expected["sources"] = sources
	raw, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(ns.Name + "\x00" + string(ns.UID)))
	state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "sandbox-pod-" + hex.EncodeToString(sum[:20]), Namespace: "cetirun-system", UID: "synthetic-expectation",
		Annotations: map[string]string{chataccess.AgentUIDAnnotation: string(agent.UID),
			"cetirun.microsoft.com/sandbox-revision": compiled.Revision},
		OwnerReferences: []metav1.OwnerReference{owner},
	}, Data: map[string]string{"expectation.json": string(raw)}}
	objects = append(objects, state)
	for index := range compiled.Controls {
		actual := compiled.Controls[index].DeepCopy()
		actual.SetUID("synthetic-control")
		objects = append(objects, actual)
	}
	return agent, objects
}
