// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAgentPlacementUsesExistingExpectationWithoutReleasingHold(t *testing.T) {
	f := newFixture(t)
	pod := f.agentPod.DeepCopy()
	pod.Spec.NodeName = f.reserved.Name
	if !f.expected.ExecutionHeld {
		t.Fatal("fixture must start held")
	}
	expectAllowed(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)))
	current := &corev1.ConfigMap{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.expectation), current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != f.expectation.ResourceVersion || current.Data[sandboxworkload.PodExpectationKey] != f.expectation.Data[sandboxworkload.PodExpectationKey] {
		t.Fatal("placement admission wrote or released the held expectation")
	}
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, f.agentPod, f.reserved.Name)), "ExecutionHeld")
}

func TestAgentTemplateAndCanonicalOwnersAreRequired(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"forged owner alone": func(p *corev1.Pod) {
			p.Spec.Containers = ordinaryPod().Spec.Containers
		},
		"wrong StatefulSet UID": func(p *corev1.Pod) { p.OwnerReferences[0].UID = "different-workload-uid" },
		"noncontroller owner":   func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) },
		"wrong Agent UID":       func(p *corev1.Pod) { p.Annotations[sandboxworkload.AgentUIDAnnotation] = "different-agent-uid" },
		"same UID changed image": func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = "registry.example.com/different:1.0"
		},
		"wrong ServiceAccount": func(p *corev1.Pod) { p.Spec.ServiceAccountName = "different-account" },
		"changed selector":     func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"example.com/pool": "sandbox"} },
		"extra label":          func(p *corev1.Pod) { p.Labels["example.com/extra"] = "true" },
		"extra annotation":     func(p *corev1.Pod) { p.Annotations["example.com/extra"] = "true" },
		"wrong revision":       func(p *corev1.Pod) { p.Labels["controller-revision-hash"] = "different-revision" },
		"ephemeral container": func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "registry.example.com/debug:1.0"}}}
		},
		"resize": func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("4")
		},
	} {
		for _, subresource := range []string{"", "ephemeralcontainers", "resize"} {
			t.Run(name+"/"+subresource, func(t *testing.T) {
				f := newFixture(t)
				f.agentPod.Spec.NodeName = f.reserved.Name
				if err := f.client.Update(context.Background(), f.agentPod); err != nil {
					t.Fatal(err)
				}
				pod := f.agentPod.DeepCopy()
				mutate(pod)
				request := podRequest(t, pod, f.agentPod)
				request.SubResource = subresource
				expectDenied(t, f.handler.Validate(context.Background(), request), "AgentTemplateMismatch")
			})
		}
	}
}

func TestAgentAuthorityIsFreshAndServerOwned(t *testing.T) {
	for name, mutate := range map[string]func(client.Object){
		"namespace replaced": func(object client.Object) {
			if namespace, ok := object.(*corev1.Namespace); ok {
				namespace.UID = "replacement-namespace-uid"
			}
		},
		"namespace owner removed": func(object client.Object) {
			if _, ok := object.(*corev1.Namespace); ok {
				object.SetOwnerReferences(nil)
			}
		},
		"namespace owner noncanonical": func(object client.Object) {
			if namespace, ok := object.(*corev1.Namespace); ok {
				namespace.OwnerReferences[0].BlockOwnerDeletion = ptr.To(false)
			}
		},
		"namespace Agent annotation changed": func(object client.Object) {
			if namespace, ok := object.(*corev1.Namespace); ok {
				namespace.Annotations[sandboxworkload.AgentNameAnnotation] = "another-agent"
			}
		},
		"namespace marker removed": func(object client.Object) {
			if namespace, ok := object.(*corev1.Namespace); ok {
				namespace.Labels = nil
			}
		},
		"Agent replaced": func(object client.Object) {
			if _, ok := object.(*api.Agent); ok {
				object.SetUID("replacement-agent-uid")
			}
		},
		"Agent authorization removed": func(object client.Object) {
			if _, ok := object.(*api.Agent); ok {
				object.SetAnnotations(nil)
			}
		},
		"Agent intent changed": func(object client.Object) {
			if agent, ok := object.(*api.Agent); ok {
				agent.Generation++
			}
		},
		"workload replaced": func(object client.Object) {
			if _, ok := object.(*appsv1.StatefulSet); ok {
				object.SetUID("replacement-workload-uid")
			}
		},
		"workload owner removed": func(object client.Object) {
			if _, ok := object.(*appsv1.StatefulSet); ok {
				object.SetOwnerReferences(nil)
			}
		},
		"same UID different workload template": func(object client.Object) {
			if workload, ok := object.(*appsv1.StatefulSet); ok {
				workload.Spec.Template.Spec.Containers[0].Image = "registry.example.com/different:1.0"
			}
		},
		"workload selector changed": func(object client.Object) {
			if workload, ok := object.(*appsv1.StatefulSet); ok {
				workload.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "different"}}
			}
		},
		"workload revision changed": func(object client.Object) {
			if workload, ok := object.(*appsv1.StatefulSet); ok {
				workload.Status.UpdateRevision = "different-revision"
			}
		},
		"expectation owner removed": func(object client.Object) {
			if state, ok := object.(*corev1.ConfigMap); ok && state.Data[sandboxworkload.PodExpectationKey] != "" {
				state.OwnerReferences = nil
			}
		},
		"expectation revision changed": func(object client.Object) {
			if state, ok := object.(*corev1.ConfigMap); ok && state.Data[sandboxworkload.PodExpectationKey] != "" {
				state.Annotations[sandboxworkload.RevisionAnnotation] = "different-revision"
			}
		},
		"ServiceAccount terminating": func(object client.Object) {
			if _, ok := object.(*corev1.ServiceAccount); ok {
				object.SetDeletionTimestamp(ptr.To(metav1.Now()))
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.handler.reader = mutateReader{Reader: f.client, mutate: mutate}
			pod := f.agentPod.DeepCopy()
			pod.Spec.NodeName = f.reserved.Name
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "")
		})
	}
}

func TestAgentExpectationRecheckedAndCannotComeFromTenant(t *testing.T) {
	f := newFixture(t)
	pod := f.agentPod.DeepCopy()
	pod.Spec.NodeName = f.reserved.Name
	var reads int
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		if state, ok := object.(*corev1.ConfigMap); ok && state.Name == f.expectation.Name {
			reads++
			if reads > 1 {
				state.ResourceVersion = "changed-version"
			}
		}
	}}
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "ExpectationChanged")
	f.handler.reader = f.client
	if err := f.client.Delete(context.Background(), f.expectation); err != nil {
		t.Fatal(err)
	}
	foreign := f.expectation.DeepCopy()
	foreign.Namespace, foreign.ResourceVersion = f.agent.Namespace.Name, ""
	if err := f.client.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "ExpectationMissing")
}

func TestAgentBindingRetainsCurrentLifecycleHold(t *testing.T) {
	for name, mutate := range map[string]func(*api.Agent){
		"suspended":      func(a *api.Agent) { a.Spec.Suspended = true },
		"hold finalizer": func(a *api.Agent) { a.Finalizers = append(a.Finalizers, sandbox.HoldFinalizer) },
	} {
		t.Run(name, func(t *testing.T) {
			f := unheldBindingFixture(t)
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if agent, ok := object.(*api.Agent); ok {
					mutate(agent)
				}
			}}
			expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, f.agentPod, f.reserved.Name)), "ExecutionHeld")
		})
	}
}

func TestAgentBindingReadsTemplateAndExactPodUID(t *testing.T) {
	f := unheldBindingFixture(t)
	request := bindingRequest(t, f.agentPod, f.reserved.Name)
	expectAllowed(t, f.handler.Validate(context.Background(), request))
	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.UID = "replacement-pod-uid" },
		func(p *corev1.Pod) { p.Spec.ServiceAccountName = "different-account" },
		func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.example.com/other:1.0" },
		func(p *corev1.Pod) { p.OwnerReferences[0].UID = "different-workload-uid" },
	} {
		f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
			if pod, ok := object.(*corev1.Pod); ok {
				mutate(pod)
			}
		}}
		expectDenied(t, f.handler.Validate(context.Background(), request), "")
	}
}

func TestAgentBindingAndUpdatesAcceptVerifiedNodeTopology(t *testing.T) {
	f := unheldBindingFixture(t)
	ctx := context.Background()
	topology := map[string]string{corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone"}
	for key, value := range topology {
		f.reserved.Labels[key] = value
	}
	if err := f.client.Update(ctx, f.reserved); err != nil {
		t.Fatal(err)
	}
	request := bindingRequest(t, f.agentPod, f.reserved.Name)
	var binding corev1.Binding
	if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
		t.Fatal(err)
	}
	binding.Labels = topology
	request.Object.Raw = marshal(t, binding)
	expectAllowed(t, f.handler.Validate(ctx, request))
	f.agentPod.Spec.NodeName = f.reserved.Name
	for key, value := range topology {
		f.agentPod.Labels[key] = value
	}
	if err := f.client.Update(ctx, f.agentPod); err != nil {
		t.Fatal(err)
	}
	expectAllowed(t, f.handler.Validate(ctx, podRequest(t, f.agentPod, f.agentPod)))
	for key := range topology {
		if _, present := f.expected.Template.Labels[key]; present {
			t.Fatal("topology validation changed the protected template")
		}
	}
}

func TestAgentTopologyCannotOverrideAuthorityOrOtherFields(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Binding){
		"wrong region": func(b *corev1.Binding) { b.Labels[corev1.LabelTopologyRegion] = "other-region" },
		"wrong zone":   func(b *corev1.Binding) { b.Labels[corev1.LabelTopologyZone] = "other-zone" },
		"extra topology": func(b *corev1.Binding) {
			b.Labels["topology.kubernetes.io/rack"] = "example-rack"
		},
		"extra label": func(b *corev1.Binding) { b.Labels["example.com/unapproved"] = "true" },
	} {
		t.Run(name, func(t *testing.T) {
			f := unheldBindingFixture(t)
			f.reserved.Labels[corev1.LabelTopologyRegion] = "example-region"
			f.reserved.Labels[corev1.LabelTopologyZone] = "example-zone"
			if err := f.client.Update(context.Background(), f.reserved); err != nil {
				t.Fatal(err)
			}
			request := bindingRequest(t, f.agentPod, f.reserved.Name)
			var binding corev1.Binding
			if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
				t.Fatal(err)
			}
			binding.Labels = map[string]string{
				corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone",
			}
			mutate(&binding)
			request.Object.Raw = marshal(t, binding)
			expectDenied(t, f.handler.Validate(context.Background(), request), "")
		})
	}
	f := unheldBindingFixture(t)
	f.expected.Template.Labels[corev1.LabelTopologyRegion] = "template-region"
	f.workload.Spec.Template.Labels[corev1.LabelTopologyRegion] = "template-region"
	f.reserved.Labels[corev1.LabelTopologyRegion] = "node-region"
	f.expectation.Data[sandboxworkload.PodExpectationKey] = string(marshal(t, f.expected))
	for _, object := range []client.Object{f.workload, f.reserved, f.expectation} {
		if err := f.client.Update(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	pod := f.agentPod.DeepCopy()
	pod.Spec.NodeName = f.reserved.Name
	pod.Labels[corev1.LabelTopologyRegion] = "node-region"
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "AgentTemplateMismatch")
}

func TestMissingOrNullHoldCannotBecomeExecutionPermission(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprint(omit), func(t *testing.T) {
			f := newFixture(t)
			var document map[string]json.RawMessage
			if err := json.Unmarshal([]byte(f.expectation.Data[sandboxworkload.PodExpectationKey]), &document); err != nil {
				t.Fatal(err)
			}
			if omit {
				delete(document, "executionHeld")
			} else {
				document["executionHeld"] = json.RawMessage("null")
			}
			f.expectation.Data[sandboxworkload.PodExpectationKey] = string(marshal(t, document))
			if err := f.client.Update(context.Background(), f.expectation); err != nil {
				t.Fatal(err)
			}
			expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, f.agentPod, f.reserved.Name)), "ExpectationInvalid")
		})
	}
}

func TestAssignedAgentCannotLeaveItsReservation(t *testing.T) {
	for _, destination := range []string{"unreserved", "another reservation"} {
		for _, clearOwner := range []bool{false, true} {
			for _, binding := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/cleared=%t/binding=%t", destination, clearOwner, binding), func(t *testing.T) {
					f := unheldBindingFixture(t)
					if destination == "another reservation" {
						f.registry.Nodes[1].Reservation = &nodeReservation{
							Agent:       sandboxworkload.ObjectIdentity{Name: "other-agent", UID: "other-agent-uid"},
							Namespace:   sandboxworkload.ObjectIdentity{Name: "other-workloads", UID: "other-namespace-uid"},
							StatefulSet: sandboxworkload.ObjectIdentity{Name: "other-workload", UID: "other-workload-uid"},
						}
						f.saveRegistry(t)
					}
					pod := f.agentPod.DeepCopy()
					if clearOwner {
						pod.OwnerReferences, pod.Labels, pod.Annotations = nil, nil, nil
					}
					if binding {
						if err := f.client.Update(context.Background(), pod); err != nil {
							t.Fatal(err)
						}
						expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.ordinary.Name)), "AgentPlacementMismatch")
					} else {
						pod.Spec.NodeName = f.ordinary.Name
						expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "AgentPlacementMismatch")
					}
				})
			}
		}
	}
}

func TestAssignedNamespaceCannotBecomeUnreservedWhenStale(t *testing.T) {
	for _, condition := range []string{"missing", "replaced and unmarked", "missing UID", "terminating"} {
		t.Run(condition, func(t *testing.T) {
			f := unheldBindingFixture(t)
			if condition == "missing" {
				if err := f.client.Delete(context.Background(), f.agent.Namespace); err != nil {
					t.Fatal(err)
				}
			} else {
				f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
					if namespace, ok := object.(*corev1.Namespace); ok {
						switch condition {
						case "replaced and unmarked":
							namespace.UID = "replacement-namespace-uid"
							namespace.OwnerReferences, namespace.Labels, namespace.Annotations = nil, nil, nil
						case "missing UID":
							namespace.UID = ""
						case "terminating":
							namespace.DeletionTimestamp = ptr.To(metav1.Now())
						}
					}
				}}
			}
			for _, node := range []string{f.ordinary.Name, f.reserved.Name} {
				pod := f.agentPod.DeepCopy()
				pod.Spec.NodeName = node
				expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "")
				expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, f.agentPod, node)), "")
			}
		})
	}
}

func TestControllerOwnershipCannotEscapeThroughAnotherNamespace(t *testing.T) {
	for _, ownerKind := range []string{"StatefulSet", "Agent"} {
		t.Run(ownerKind, func(t *testing.T) {
			f := newFixture(t)
			pod := f.agentPod.DeepCopy()
			pod.Name, pod.Namespace = "owned-pod", "ordinary-workloads"
			pod.UID, pod.ResourceVersion = "owned-pod-uid", ""
			if ownerKind == "Agent" {
				pod.OwnerReferences = []metav1.OwnerReference{sandboxworkload.AgentOwner(f.expected.Agent.Name, f.expected.Agent.UID)}
			}
			direct := pod.DeepCopy()
			direct.Spec.NodeName = f.ordinary.Name
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, direct, nil)), "AgentPlacementMismatch")
			if err := f.client.Create(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.ordinary.Name)), "AgentPlacementMismatch")
			// Classification must use the authoritative previous Pod, not owner
			// fields cleared in either admission payload.
			old := pod.DeepCopy()
			old.OwnerReferences, old.Labels, old.Annotations = nil, nil, nil
			updated := old.DeepCopy()
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, updated, old)), "AgentAssignmentChanged")
			updated.Spec.NodeName = f.ordinary.Name
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, updated, old)), "AgentAssignmentChanged")
		})
	}
}

func TestAssignedAgentMayUseMultipleExactlyMatchingReservedNodes(t *testing.T) {
	f := unheldBindingFixture(t)
	reservation := *f.registry.Nodes[0].Reservation
	f.registry.Nodes[1].Reservation = &reservation
	f.saveRegistry(t)
	for _, node := range []string{f.reserved.Name, f.ordinary.Name} {
		pod := f.agentPod.DeepCopy()
		pod.Spec.NodeName = node
		expectAllowed(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)))
		expectAllowed(t, f.handler.Validate(context.Background(), bindingRequest(t, f.agentPod, node)))
	}
}

func TestConflictingAgentAssignmentsFailClosed(t *testing.T) {
	f := unheldBindingFixture(t)
	reservation := *f.registry.Nodes[0].Reservation
	reservation.StatefulSet = sandboxworkload.ObjectIdentity{Name: "other-workload", UID: "other-workload-uid"}
	f.registry.Nodes[1].Reservation = &reservation
	f.saveRegistry(t)
	pod := f.agentPod.DeepCopy()
	pod.Spec.NodeName = f.reserved.Name
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "AgentAssignmentAmbiguous")
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, f.agentPod, f.reserved.Name)), "AgentAssignmentAmbiguous")
}

func unheldBindingFixture(t *testing.T) *placementFixture {
	t.Helper()
	f := newFixture(t)
	// Synthetic future controller state exercises the binding path. Production
	// placement admission has no writer and never constructs this state.
	f.expected.ExecutionHeld = false
	f.expectation.Data[sandboxworkload.PodExpectationKey] = string(marshal(t, f.expected))
	if err := f.client.Update(context.Background(), f.expectation); err != nil {
		t.Fatal(err)
	}
	return f
}
