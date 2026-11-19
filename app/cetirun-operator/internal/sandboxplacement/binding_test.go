// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func recordedPlacementFixture(t *testing.T) *placementFixture {
	t.Helper()
	f := unheldBindingFixture(t)
	reserveOrdinaryNode(t, f)
	binding, _, err := f.handler.ReadAssignment(context.Background(), assignmentExpectation(f))
	if err != nil {
		t.Fatal(err)
	}
	f.expected.Placement = binding
	savePlacementExpectation(t, f)
	return f
}

func savePlacementExpectation(t *testing.T, f *placementFixture) {
	t.Helper()
	f.expectation.Data[sandboxworkload.PodExpectationKey] = string(marshal(t, f.expected))
	if err := f.client.Update(context.Background(), f.expectation); err != nil {
		t.Fatal(err)
	}
}

func recordedPlacementRequest(t *testing.T, f *placementFixture, mode string) admission.Response {
	t.Helper()
	pod := f.agentPod.DeepCopy()
	switch mode {
	case "unscheduled":
		return f.handler.Validate(context.Background(), podRequest(t, pod, nil))
	case "binding":
		return f.handler.Validate(context.Background(), bindingRequest(t, pod, f.reserved.Name))
	case "direct":
		pod.Spec.NodeName = f.reserved.Name
		return f.handler.Validate(context.Background(), podRequest(t, pod, nil))
	case "scheduled":
		pod.Spec.NodeName = f.reserved.Name
		expected := f.expected
		expected.Template = *expected.Template.DeepCopy()
		expected.Template.Spec.NodeName = pod.Spec.NodeName
		return f.handler.ValidateScheduledPod(context.Background(), podRequest(t, pod, pod), pod, expected)
	default:
		t.Fatalf("unsupported fixture mode %q", mode)
		return admission.Response{}
	}
}

func TestRecordedPlacementRequiresTheWholeLifetimeBinding(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *placementFixture){
		"selected reservation removed": func(t *testing.T, f *placementFixture) {
			f.registry.Nodes[0].Reservation = nil
			f.saveRegistry(t)
		},
		"other reservation removed": func(t *testing.T, f *placementFixture) {
			f.registry.Nodes[1].Reservation = nil
			f.saveRegistry(t)
		},
		"all reservations removed": func(t *testing.T, f *placementFixture) {
			for i := range f.registry.Nodes {
				f.registry.Nodes[i].Reservation = nil
			}
			f.saveRegistry(t)
		},
		"additional reservation": func(t *testing.T, f *placementFixture) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "additional-worker", UID: "additional-uid"},
				Spec: corev1.NodeSpec{ProviderID: "example://additional-worker"}}
			if err := f.client.Create(context.Background(), node); err != nil {
				t.Fatal(err)
			}
			reservation := *f.registry.Nodes[0].Reservation
			f.registry.Nodes = append(f.registry.Nodes, registeredNode{
				Name: node.Name, UID: node.UID, ProviderID: node.Spec.ProviderID, Reservation: &reservation,
			})
			f.saveRegistry(t)
		},
		"reassigned reservation": func(t *testing.T, f *placementFixture) {
			f.registry.Nodes[0].Reservation = &nodeReservation{
				Agent:       sandboxworkload.ObjectIdentity{Name: "other-agent", UID: "other-agent-uid"},
				Namespace:   sandboxworkload.ObjectIdentity{Name: "other-namespace", UID: "other-namespace-uid"},
				StatefulSet: sandboxworkload.ObjectIdentity{Name: "other-workload", UID: "other-workload-uid"},
			}
			f.saveRegistry(t)
		},
		"registry recreated": func(_ *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if object.GetName() == f.state.Name {
					object.SetUID("replacement-registry-uid")
				}
			}}
		},
		"Node replacement reenrolled": func(t *testing.T, f *placementFixture) {
			f.registry.Nodes[0].UID = "replacement-node-uid"
			f.registry.Nodes[0].ProviderID = "example://replacement-worker"
			f.saveRegistry(t)
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok && node.Name == f.reserved.Name {
					node.UID, node.Spec.ProviderID = "replacement-node-uid", "example://replacement-worker"
				}
			}}
		},
		"other recorded Node missing": func(t *testing.T, f *placementFixture) {
			if err := f.client.Delete(context.Background(), f.ordinary); err != nil {
				t.Fatal(err)
			}
		},
		"other recorded Node replaced": func(_ *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok && node.Name == f.ordinary.Name {
					node.UID = "replacement-node-uid"
				}
			}}
		},
		"other recorded provider changed": func(_ *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok && node.Name == f.ordinary.Name {
					node.Spec.ProviderID = "example://changed-worker"
				}
			}}
		},
		"other recorded Node terminating": func(_ *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok && node.Name == f.ordinary.Name {
					node.DeletionTimestamp = ptr.To(metav1.Unix(1, 0))
				}
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := recordedPlacementFixture(t)
			mutate(t, f)
			for _, mode := range []string{"unscheduled", "binding", "direct", "scheduled"} {
				t.Run(mode, func(t *testing.T) {
					expectDenied(t, recordedPlacementRequest(t, f, mode), "")
				})
			}
		})
	}
}

func TestRecordedPlacementRejectsExactTupleAndCanonicalShapeChanges(t *testing.T) {
	for name, mutate := range map[string]func(*sandboxworkload.Expectation){
		"binding Agent":      func(e *sandboxworkload.Expectation) { e.Placement.Agent.UID = "other-agent-uid" },
		"binding namespace":  func(e *sandboxworkload.Expectation) { e.Placement.Namespace.UID = "other-namespace-uid" },
		"binding workload":   func(e *sandboxworkload.Expectation) { e.Placement.Workload.UID = "other-workload-uid" },
		"binding HOME":       func(e *sandboxworkload.Expectation) { e.Placement.Home.UID = "other-home-uid" },
		"expected HOME name": func(e *sandboxworkload.Expectation) { e.Home.Name = "other-home" },
		"expected HOME UID":  func(e *sandboxworkload.Expectation) { e.Home.UID = "other-home-uid" },
		"registry name":      func(e *sandboxworkload.Expectation) { e.Placement.Registry.Name = "other-registry" },
		"registry UID":       func(e *sandboxworkload.Expectation) { e.Placement.Registry.UID = "other-registry-uid" },
		"Node UID":           func(e *sandboxworkload.Expectation) { e.Placement.Nodes[0].UID = "other-node-uid" },
		"Node provider":      func(e *sandboxworkload.Expectation) { e.Placement.Nodes[0].ProviderID = "example://other-worker" },
		"noncanonical order": func(e *sandboxworkload.Expectation) { slices.Reverse(e.Placement.Nodes) },
		"duplicate identity": func(e *sandboxworkload.Expectation) { e.Placement.Nodes[0].UID = e.Placement.Nodes[1].UID },
		"empty binding":      func(e *sandboxworkload.Expectation) { e.Placement = &sandboxworkload.PlacementBinding{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := recordedPlacementFixture(t)
			mutate(&f.expected)
			savePlacementExpectation(t, f)
			for _, mode := range []string{"unscheduled", "binding", "direct", "scheduled"} {
				t.Run(mode, func(t *testing.T) {
					expectDenied(t, recordedPlacementRequest(t, f, mode), "")
				})
			}
		})
	}
}

func TestRecordedPlacementCannotBeHiddenByRemovingReservationOrMarkers(t *testing.T) {
	for _, removeNamespaceMarkers := range []bool{false, true} {
		f := recordedPlacementFixture(t)
		for i := range f.registry.Nodes {
			f.registry.Nodes[i].Reservation = nil
		}
		f.saveRegistry(t)
		if removeNamespaceMarkers {
			f.agent.Namespace.Labels, f.agent.Namespace.Annotations, f.agent.Namespace.OwnerReferences = nil, nil, nil
			if err := f.client.Update(context.Background(), f.agent.Namespace); err != nil {
				t.Fatal(err)
			}
		}
		f.agentPod.OwnerReferences, f.agentPod.Labels, f.agentPod.Annotations = nil, nil, nil
		if err := f.client.Update(context.Background(), f.agentPod); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"unscheduled", "binding", "direct"} {
			expectDenied(t, recordedPlacementRequest(t, f, mode), "")
		}
	}
}

func TestRecordedPlacementOptionalJSONCannotHideInvalidBindings(t *testing.T) {
	for _, placement := range []string{
		`null`, `{}`, `{"registry":null}`, `{"unexpected":true}`,
		`{"agent":{"name":"example","name":"example","uid":"agent-uid"}}`,
	} {
		f := recordedPlacementFixture(t)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(f.expectation.Data[sandboxworkload.PodExpectationKey]), &fields); err != nil {
			t.Fatal(err)
		}
		fields["placement"] = json.RawMessage(placement)
		f.expectation.Data[sandboxworkload.PodExpectationKey] = string(marshal(t, fields))
		if err := f.client.Update(context.Background(), f.expectation); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"unscheduled", "binding", "direct"} {
			expectDenied(t, recordedPlacementRequest(t, f, mode), "")
		}
	}
}

func TestRecordedPlacementPodContractRetainsUnscheduledTupleAndBoundMembership(t *testing.T) {
	f := recordedPlacementFixture(t)
	if err := sandboxworkload.ValidatePod(f.agentPod, f.expected); err != nil {
		t.Fatalf("valid unscheduled bound expectation was rejected: %v", err)
	}
	changed := f.expected
	changed.Placement = changed.Placement.DeepCopy()
	changed.Placement.Home.UID = "other-home-uid"
	if err := sandboxworkload.ValidatePod(f.agentPod, changed); err == nil {
		t.Fatal("unscheduled Pod validation lost the HOME tuple binding")
	}
	changed = f.expected
	changed.Template = *changed.Template.DeepCopy()
	pod := f.agentPod.DeepCopy()
	for _, node := range []string{f.reserved.Name, f.ordinary.Name} {
		pod.Spec.NodeName, changed.Template.Spec.NodeName = node, node
		if err := sandboxworkload.ValidatePod(pod, changed); err != nil {
			t.Fatalf("recorded structural membership rejected: %v", err)
		}
	}
	pod.Spec.NodeName, changed.Template.Spec.NodeName = "unrecorded-worker", "unrecorded-worker"
	if err := sandboxworkload.ValidatePod(pod, changed); err == nil {
		t.Fatal("matching Pod/template destinations waived recorded membership")
	}
	changed.Placement = nil
	if err := sandboxworkload.ValidatePod(pod, changed); err != nil {
		t.Fatalf("nil placement changed legacy structural comparison: %v", err)
	}
}

func TestRecordedPlacementMetadataChurnAndLegacyCompatibility(t *testing.T) {
	f := recordedPlacementFixture(t)
	original := f.expected.Placement.DeepCopy()
	slices.Reverse(f.registry.Nodes)
	f.saveRegistry(t)
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		switch object := object.(type) {
		case *corev1.Node:
			object.ResourceVersion = "new-node-version"
			object.Labels["example.com/metadata"] = "changed"
			object.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		case *corev1.ConfigMap:
			if object.Name == f.state.Name {
				object.Annotations = map[string]string{"example.com/metadata": "changed"}
			}
		}
	}}
	for _, mode := range []string{"unscheduled", "binding", "direct", "scheduled"} {
		expectAllowed(t, recordedPlacementRequest(t, f, mode))
	}
	if !reflect.DeepEqual(original, f.expected.Placement) {
		t.Fatal("admission mutated the immutable binding")
	}
	f.handler.reader = f.client
	f.expected.Placement = nil
	savePlacementExpectation(t, f)
	f.registry.Nodes[0].Reservation = nil
	f.saveRegistry(t)
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		if object.GetName() == f.state.Name {
			object.SetUID("recreated-legacy-registry")
		}
	}}
	for _, mode := range []string{"unscheduled", "binding", "direct", "scheduled"} {
		expectAllowed(t, recordedPlacementRequest(t, f, mode))
	}
}

func TestRecordedPlacementRetainsExecutionHoldsAndReadOnlyBehavior(t *testing.T) {
	f := recordedPlacementFixture(t)
	f.expected.ExecutionHeld = true
	savePlacementExpectation(t, f)
	before := f.expectation.DeepCopy()
	for _, mode := range []string{"unscheduled", "direct", "scheduled"} {
		expectAllowed(t, recordedPlacementRequest(t, f, mode))
	}
	expectDenied(t, recordedPlacementRequest(t, f, "binding"), "ExecutionHeld")
	current := &corev1.ConfigMap{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, current) {
		t.Fatal("placement validation wrote to the held expectation")
	}
	f.expected.ExecutionHeld = false
	savePlacementExpectation(t, f)
	for _, suspended := range []bool{false, true} {
		f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
			if agent, ok := object.(*api.Agent); ok {
				if suspended {
					agent.Spec.Suspended = true
				} else {
					agent.Finalizers = append(agent.Finalizers, sandbox.HoldFinalizer)
				}
			}
		}}
		expectDenied(t, recordedPlacementRequest(t, f, "binding"), "ExecutionHeld")
	}
}

func TestRecordedPlacementPreservesVerifiedAgentTopology(t *testing.T) {
	f := recordedPlacementFixture(t)
	topology := map[string]string{corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone"}
	for key, value := range topology {
		f.reserved.Labels[key] = value
	}
	if err := f.client.Update(context.Background(), f.reserved); err != nil {
		t.Fatal(err)
	}
	request := bindingRequest(t, f.agentPod, f.reserved.Name)
	var binding corev1.Binding
	if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
		t.Fatal(err)
	}
	binding.Labels = topology
	request.Object.Raw = marshal(t, binding)
	expectAllowed(t, f.handler.Validate(context.Background(), request))
	pod := f.agentPod.DeepCopy()
	pod.Spec.NodeName = f.reserved.Name
	for key, value := range topology {
		pod.Labels[key] = value
	}
	expected := f.expected
	expected.Template = *expected.Template.DeepCopy()
	expected.Template.Spec.NodeName = pod.Spec.NodeName
	expectAllowed(t, f.handler.ValidateScheduledPod(context.Background(), podRequest(t, pod, pod), pod, expected))
	pod.Labels[corev1.LabelTopologyZone] = "forged-zone"
	expectDenied(t, f.handler.ValidateScheduledPod(context.Background(), podRequest(t, pod, pod), pod, expected), "AgentTopologyMismatch")
	if _, present := f.expected.Template.Labels[corev1.LabelTopologyZone]; present {
		t.Fatal("verified topology handling changed the protected template")
	}
}

func TestRecordedPlacementAdmissionRechecksNonDestinationAuthority(t *testing.T) {
	for _, mode := range []string{"unscheduled", "binding", "direct", "scheduled"} {
		for _, target := range []string{"ordinary-worker", "sandbox-placement"} {
			t.Run(mode+"/"+target, func(t *testing.T) {
				f := recordedPlacementFixture(t)
				var reads int
				f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
					if object.GetName() == target {
						reads++
						if reads > 1 {
							object.SetAnnotations(map[string]string{"example.com/drift": "changed-without-version"})
						}
					}
				}}
				expectDenied(t, recordedPlacementRequest(t, f, mode), "Changed")
			})
		}
	}
}
