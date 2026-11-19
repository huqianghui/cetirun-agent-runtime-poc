// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestRenderedPodOnNodeKeepsExactTemplateAuthority(t *testing.T) {
	for _, kind := range []string{"StatefulSet", "Deployment"} {
		t.Run(kind, func(t *testing.T) {
			workload := RenderedWorkload{
				Kind: kind, Identity: ObjectIdentity{Name: "example-agent", UID: "workload-uid"},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "approved"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "cetirun", Image: "registry.example.com/agent:example",
					}}},
				},
			}
			owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: kind, Name: workload.Identity.Name,
				UID: workload.Identity.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "example-node", UID: "node-uid",
				Labels: map[string]string{corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone"}}}
			pod := &corev1.Pod{ObjectMeta: *workload.Template.ObjectMeta.DeepCopy(), Spec: *workload.Template.Spec.DeepCopy()}
			pod.OwnerReferences, pod.Spec.NodeName = []metav1.OwnerReference{owner}, node.Name
			pod.Labels[corev1.LabelTopologyRegion], pod.Labels[corev1.LabelTopologyZone] = "example-region", "example-zone"
			if kind == "StatefulSet" {
				pod.Name = workload.Identity.Name + "-0"
				pod.Spec.Hostname, pod.Spec.Subdomain = pod.Name, workload.Identity.Name
				pod.Labels["statefulset.kubernetes.io/pod-name"], pod.Labels["apps.kubernetes.io/pod-index"] = pod.Name, "0"
				pod.Labels["controller-revision-hash"] = "current-revision"
			} else {
				pod.Name = "example-agent-current-revision-pod"
				pod.Labels["pod-template-hash"] = "current-revision"
			}
			before := workload.Template.DeepCopy()
			if err := ValidateRenderedPodOnNode(pod, node, workload, owner, "current-revision"); err != nil {
				t.Fatal("current assigned-node topology was rejected:", err)
			}
			if !equality.Semantic.DeepEqual(before, &workload.Template) {
				t.Fatal("node observation changed the persisted template")
			}
			if err := ValidateRenderedPod(pod, workload, owner, "current-revision"); err == nil {
				t.Fatal("unobserved topology or placement was accepted")
			}
			if err := ValidateRenderedPodOnNode(pod, nil, workload, owner, "current-revision"); err == nil {
				t.Fatal("missing node observation was accepted")
			}
			withoutLabels := pod.DeepCopy()
			delete(withoutLabels.Labels, corev1.LabelTopologyRegion)
			delete(withoutLabels.Labels, corev1.LabelTopologyZone)
			if err := ValidateRenderedPodOnNode(withoutLabels, node, workload, owner, "current-revision"); err != nil {
				t.Fatal("bound Pod without copied topology was rejected:", err)
			}
			for name, change := range map[string]func(*corev1.Pod, *corev1.Node, *RenderedWorkload){
				"region": func(p *corev1.Pod, _ *corev1.Node, _ *RenderedWorkload) {
					p.Labels[corev1.LabelTopologyRegion] = "other"
				},
				"zone":               func(p *corev1.Pod, _ *corev1.Node, _ *RenderedWorkload) { p.Labels[corev1.LabelTopologyZone] = "other" },
				"unrelated label":    func(p *corev1.Pod, _ *corev1.Node, _ *RenderedWorkload) { p.Labels["unapproved"] = "true" },
				"wrong node":         func(_ *corev1.Pod, n *corev1.Node, _ *RenderedWorkload) { n.Name = "other-node" },
				"missing node label": func(_ *corev1.Pod, n *corev1.Node, _ *RenderedWorkload) { delete(n.Labels, corev1.LabelTopologyZone) },
				"pinned placement":   func(_ *corev1.Pod, _ *corev1.Node, w *RenderedWorkload) { w.Template.Spec.NodeName = "other-node" },
				"pinned label": func(_ *corev1.Pod, _ *corev1.Node, w *RenderedWorkload) {
					w.Template.Labels[corev1.LabelTopologyZone] = "pinned-zone"
				},
				"image": func(p *corev1.Pod, _ *corev1.Node, _ *RenderedWorkload) {
					p.Spec.Containers[0].Image = "registry.example.com/unapproved:example"
				},
			} {
				t.Run(name, func(t *testing.T) {
					changed, observed, wanted := pod.DeepCopy(), node.DeepCopy(), workload
					wanted.Template = *workload.Template.DeepCopy()
					change(changed, observed, &wanted)
					if err := ValidateRenderedPodOnNode(changed, observed, wanted, owner, "current-revision"); err == nil {
						t.Fatal("unapproved Pod or Node change was accepted")
					}
				})
			}
		})
	}
}
