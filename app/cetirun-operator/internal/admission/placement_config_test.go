// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPlacementConfigRequiresLiteralName(t *testing.T) {
	for _, name := range []string{"", "sandbox-placement", "pilot.placement"} {
		config := testConfig()
		config.PlacementConfigMapName = name
		if err := config.Validate(); err != nil {
			t.Fatalf("valid placement name %q: %v", name, err)
		}
	}
	for _, name := range []string{" ", "Invalid", "../placement", "state/placement", strings.Repeat("x", 254)} {
		config := testConfig()
		config.PlacementConfigMapName = name
		if err := config.Validate(); err == nil {
			t.Fatalf("invalid placement name %q was accepted", name)
		}
		if _, err := NewPodHandler(&unavailablePodReader{}, PodConfig{StateNamespace: "operator-state", PlacementConfigMapName: name}); err == nil {
			t.Fatalf("protected Pod handler accepted invalid placement name %q", name)
		}
	}

	config, err := ParseConfig(`{"adminUsers":["administrator","system:serviceaccount:operator-test:portal"],"controllerUsername":"system:serviceaccount:operator-test:controller","placementConfigMapName":"sandbox-placement"}`)
	if err != nil || config.PlacementConfigMapName != "sandbox-placement" {
		t.Fatalf("placement selection failed to roundtrip: %v", err)
	}
}

func placementRegistryForPod(t *testing.T, namespace string, expected w.Expectation, node *corev1.Node) *corev1.ConfigMap {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"version": sandboxplacement.RegistryVersion,
		"nodes": []any{map[string]any{
			"name": node.Name, "uid": node.UID, "providerID": node.Spec.ProviderID,
			"reservation": map[string]any{
				"agent": expected.Agent, "namespace": expected.Namespace, "statefulSet": expected.Workload,
			},
		}},
		"platformDaemonSets": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "placement-registry", Namespace: namespace},
		Data:       map[string]string{sandboxplacement.ConfigMapKey: string(raw)},
	}
}

func newScheduledPodFixture(t *testing.T) (podFixture, *corev1.Node, *corev1.ConfigMap) {
	t.Helper()
	f := newPodFixture(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduled-worker", UID: "node-uid", Labels: map[string]string{
			corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone",
		}},
		Spec: corev1.NodeSpec{ProviderID: "example://scheduled-worker"},
	}
	registry := placementRegistryForPod(t, f.handler.config.StateNamespace, f.expected, node)
	registry.UID = "placement-registry-uid"
	for _, object := range []client.Object{node, registry} {
		if err := f.client.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.handler, err = NewPodHandler(f.client, PodConfig{StateNamespace: registry.Namespace, PlacementConfigMapName: registry.Name})
	if err != nil {
		t.Fatal(err)
	}
	f.pod.UID, f.pod.ResourceVersion, f.pod.Spec.NodeName = "pod-uid", "", node.Name
	for key, value := range node.Labels {
		f.pod.Labels[key] = value
	}
	if err := f.client.Create(context.Background(), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	return f, node, registry
}

func TestProtectedPodTopologyRemainsExact(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"matching topology": func(*corev1.Pod) {},
		"wrong region":      func(p *corev1.Pod) { p.Labels[corev1.LabelTopologyRegion] = "wrong-region" },
		"wrong zone":        func(p *corev1.Pod) { p.Labels[corev1.LabelTopologyZone] = "wrong-zone" },
		"extra topology":    func(p *corev1.Pod) { p.Labels["topology.kubernetes.io/rack"] = "example-rack" },
		"extra label":       func(p *corev1.Pod) { p.Labels["example.com/unapproved"] = "true" },
		"extra annotation":  func(p *corev1.Pod) { p.Annotations["example.com/unapproved"] = "true" },
		"changed image":     func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.example.com/other:v1" },
		"changed node":      func(p *corev1.Pod) { p.Spec.NodeName = "another-worker" },
	} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := newScheduledPodFixture(t)
			pod := f.pod.DeepCopy()
			mutate(pod)
			response := f.handler.Validate(context.Background(), podRequest(t, pod, admissionv1.Update, f.pod))
			if response.Allowed != (name == "matching topology") {
				t.Fatalf("unexpected protected Pod result: %+v", response.Result)
			}
		})
	}
	f, _, registry := newScheduledPodFixture(t)
	baseline, err := NewPodHandler(f.client, PodConfig{StateNamespace: registry.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	if response := baseline.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Update, f.pod)); response.Allowed {
		t.Fatal("topology additions were accepted without the placement opt-in")
	}
	unscheduled := f.pod.DeepCopy()
	unscheduled.Spec.NodeName = ""
	if response := f.handler.Validate(context.Background(), podRequest(t, unscheduled, admissionv1.Create, nil)); response.Allowed {
		t.Fatal("unscheduled Pod gained topology authority")
	}
	if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil)); response.Allowed {
		t.Fatal("topology handling authorized a direct assignment absent from the protected template")
	}
}

type topologyNodeReader struct {
	client.Reader
	mutate func(*corev1.Node, int)
	reads  int
}

func (r *topologyNodeReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if node, ok := object.(*corev1.Node); ok {
		r.reads++
		r.mutate(node, r.reads)
	}
	return nil
}

func TestProtectedPodTopologyRequiresFreshRegisteredNode(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Node, int){
		"replaced node":    func(n *corev1.Node, _ int) { n.UID = "replacement-node-uid" },
		"missing zone":     func(n *corev1.Node, _ int) { delete(n.Labels, corev1.LabelTopologyZone) },
		"changed zone":     func(n *corev1.Node, _ int) { n.Labels[corev1.LabelTopologyZone] = "other-zone" },
		"changed provider": func(n *corev1.Node, _ int) { n.Spec.ProviderID = "example://other-worker" },
		"changed during validation": func(n *corev1.Node, reads int) {
			if reads > 1 {
				n.Labels[corev1.LabelTopologyRegion] = "another-region"
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := newScheduledPodFixture(t)
			reader := &topologyNodeReader{Reader: f.client, mutate: mutate}
			handler, err := NewPodHandler(reader, f.handler.config)
			if err != nil {
				t.Fatal(err)
			}
			response := handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Update, f.pod))
			if response.Allowed {
				t.Fatal("unverified or stale Node topology was accepted")
			}
		})
	}
	for _, missing := range []string{"node", "registry"} {
		t.Run(missing, func(t *testing.T) {
			f, node, registry := newScheduledPodFixture(t)
			var object client.Object = node
			if missing == "registry" {
				object = registry
			}
			if err := f.client.Delete(context.Background(), object); err != nil {
				t.Fatal(err)
			}
			if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Update, f.pod)); response.Allowed {
				t.Fatal("missing authority accepted for a protected Pod")
			}
		})
	}
}

func TestPlacementServerRequiresProtectedPodStateNamespace(t *testing.T) {
	config := testConfig()
	config.PlacementConfigMapName = "sandbox-placement"
	for _, namespace := range []string{"", "agent-workload", "another-operator"} {
		err := Run(context.Background(), nil, config, ServerOptions{PodStateNamespace: namespace})
		if err == nil || !strings.Contains(err.Error(), "configured controller state namespace") {
			t.Fatalf("placement serving with namespace %q did not reject before startup: %v", namespace, err)
		}
	}

}

func TestRecordedPlacementCannotLoseItsAdmissionReader(t *testing.T) {
	f, _, registry := newScheduledPodFixture(t)
	ctx := context.Background()
	placement, source, err := f.handler.placement.ReadAssignment(ctx, f.expected)
	if err != nil {
		t.Fatal(err)
	}
	f.expected.Placement = placement
	f.expected.Sources = append(f.expected.Sources, source)
	raw, err := json.Marshal(f.expected)
	if err != nil {
		t.Fatal(err)
	}
	f.state.Data[PodExpectationKey] = string(raw)
	if err := f.client.Update(ctx, f.state); err != nil {
		t.Fatal(err)
	}
	request := podRequest(t, f.pod, admissionv1.Update, f.pod)
	if response := f.handler.Validate(ctx, request); !response.Allowed {
		t.Fatalf("recorded placement was not accepted with its verified reader: %+v", response.Result)
	}
	withoutPlacement, err := NewPodHandler(f.client, PodConfig{StateNamespace: registry.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	response := withoutPlacement.Validate(ctx, request)
	if response.Allowed || !strings.Contains(response.Result.Message, "PlacementUnavailable") {
		t.Fatalf("recorded placement silently fell back after reader removal: %+v", response.Result)
	}
	f.expected.Placement.Home.UID = "different-home-uid"
	raw, err = json.Marshal(f.expected)
	if err != nil {
		t.Fatal(err)
	}
	f.state.Data[PodExpectationKey] = string(raw)
	if err := f.client.Update(ctx, f.state); err != nil {
		t.Fatal(err)
	}
	if response := f.handler.Validate(ctx, request); response.Allowed {
		t.Fatal("placement bound to another HOME was accepted")
	}
}
