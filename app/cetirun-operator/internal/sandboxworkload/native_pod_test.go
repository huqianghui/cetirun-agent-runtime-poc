// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"slices"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNativePodObservationPinsUIDAndNodeTopology(t *testing.T) {
	f := testfixture.New("openclaw")
	f.Input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
	f.Input.AuthorizedSecrets = append(f.Input.AuthorizedSecrets, "gateway-key")
	bundle, err := w.Build(f.Input)
	if err != nil {
		t.Fatal(err)
	}
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := helm.PrepareSandboxChart(chart, map[string]any{
		"agentType": "openclaw", "agentRequest": map[string]any{"type": "openclaw"},
		"secrets":   map[string]any{"existingSecret": "runtime-secret", "provider": "openai", "model": "example-model", "apiBase": "http://gateway.example-platform.svc.cluster.local:4000/v1"},
		"aiGateway": map[string]any{"enabled": true, "keySecretName": "gateway-key", "keySecretKey": "api-key", "models": []any{map[string]any{"id": "example-model", "name": "Example"}}},
	}, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	workload := testfixture.Workload(f.Input, *template)
	expected, err := bundle.Expect(*template, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected.SinglePod, expected.PodUID = true, "first-pod-uid"
	expected.Template.Finalizers = []string{w.NativePodFinalizer}
	pod := testfixture.Pod(expected)
	pod.UID, pod.Spec.NodeName = expected.PodUID, "example-node"
	pod.Labels[corev1.LabelTopologyZone] = "example-zone"
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "example-node", UID: "node-uid", Labels: map[string]string{corev1.LabelTopologyZone: "example-zone"},
	}}
	if err := w.ValidatePodOnNode(pod, node, expected); err != nil {
		t.Fatal(err)
	}
	if expected.Template.Spec.NodeName != "" || expected.Template.Labels[corev1.LabelTopologyZone] != "" {
		t.Fatal("observation mutated the persisted expectation")
	}
	for _, change := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.UID = "replacement-pod-uid" },
		func(p *corev1.Pod) { p.Spec.NodeName = "different-node" },
		func(p *corev1.Pod) { p.Labels[corev1.LabelTopologyZone] = "different-zone" },
		func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.example.com/unapproved:1" },
		func(p *corev1.Pod) { p.Spec.InitContainers[0].Command = p.Spec.InitContainers[0].Command[3:] },
	} {
		changed := pod.DeepCopy()
		change(changed)
		if err := w.ValidatePodOnNode(changed, node, expected); err == nil {
			t.Fatal("changed Pod identity, placement or runtime was accepted")
		}
	}

}

func TestNativeInitProcessKeepsExactHelperBoundaries(t *testing.T) {
	for _, agentType := range []string{"openclaw", "hermes"} {
		t.Run(agentType, func(t *testing.T) {
			f := testfixture.New(agentType)
			f.Input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
			f.Input.AuthorizedSecrets = append(f.Input.AuthorizedSecrets, "gateway-key")
			bundle, err := w.Build(f.Input)
			if agentType == "hermes" {
				if err == nil {
					t.Fatal("unreconciled Hermes unexpectedly accepted a Gateway key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			chart, err := loader.Load("../../../cetirun-chart")
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := helm.PrepareSandboxChart(chart, map[string]any{
				"agentType": agentType, "agentRequest": map[string]any{"type": agentType},
				"secrets":   map[string]any{"existingSecret": "runtime-secret", "provider": "openai", "model": "example-model", "apiBase": "http://gateway.example-platform.svc.cluster.local:4000/v1"},
				"aiGateway": map[string]any{"enabled": true, "keySecretName": "gateway-key", "keySecretKey": "api-key", "models": []any{map[string]any{"id": "example-model", "name": "Example"}}},
			}, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			image := template.Spec.Containers[0].Image
			for index, container := range template.Spec.InitContainers {
				t.Run(container.Name, func(t *testing.T) {
					original := slices.Clone(container.Command)
					if agentType == "openclaw" {
						if len(original) < 4 || !slices.Equal(original[:3], []string{"/usr/bin/tini", "-g", "--"}) {
							t.Fatalf("native initializer has no approved init process: %v", original)
						}
						legacy := template.DeepCopy()
						legacy.Spec.InitContainers[index].Command = original[3:]
						if err := bundle.ValidateTemplate(legacy, image); err != nil {
							t.Fatalf("existing exact helper entrypoint was rejected: %v", err)
						}
						for _, command := range [][]string{
							append([]string{"/usr/bin/tini", "--"}, original[3:]...),
							append([]string{"/usr/bin/tini", "-g", "-e", "1", "--"}, original[3:]...),
							append([]string{"/unapproved/tini", "-g", "--"}, original[3:]...),
							append(slices.Clone(original), "unapproved-argument"),
						} {
							changed := template.DeepCopy()
							changed.Spec.InitContainers[index].Command = command
							if err := bundle.ValidateTemplate(changed, image); err == nil {
								t.Fatalf("unapproved initializer accepted: %v", command)
							}
						}
					} else {
						changed := template.DeepCopy()
						changed.Spec.InitContainers[index].Command = append([]string{"/usr/bin/tini", "-g", "--"}, original...)
						if err := bundle.ValidateTemplate(changed, image); err == nil {
							t.Fatal("an unapproved init process was accepted for another harness")
						}
					}
				})
			}
		})
	}
}
