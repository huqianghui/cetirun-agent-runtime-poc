// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func assignmentExpectation(f *placementFixture) sandboxworkload.Expectation {
	return sandboxworkload.Expectation{
		Agent: f.expected.Agent, Namespace: f.expected.Namespace, Workload: f.expected.Workload,
		Home: f.expected.Home, Template: *f.expected.Template.DeepCopy(),
	}
}

func reserveOrdinaryNode(t *testing.T, f *placementFixture) {
	t.Helper()
	reservation := *f.registry.Nodes[0].Reservation
	f.registry.Nodes[1].Reservation = &reservation
	f.saveRegistry(t)
}

func expectAssignmentFailure(t *testing.T, handler *Handler, expected sandboxworkload.Expectation, reason string) {
	t.Helper()
	binding, source, err := handler.ReadAssignment(context.Background(), expected)
	if err == nil || binding != nil || source != (sandboxworkload.SourceReference{}) {
		t.Fatalf("assignment failure returned usable authority: binding=%v source=%v err=%v", binding, source, err)
	}
	if len(err.Error()) > 320 || strings.Contains(err.Error(), "synthetic-private") || !strings.Contains(err.Error(), reason) {
		t.Fatalf("assignment error is not bounded or did not contain %q: %v", reason, err)
	}
}

func TestReadAssignmentCanonicalSnapshotAndFreshSource(t *testing.T) {
	f := newFixture(t)
	reserveOrdinaryNode(t, f)
	expected := assignmentExpectation(f)
	var reads []string
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		reads = append(reads, object.GetName())
	}}
	binding, source, err := f.handler.ReadAssignment(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	want := &sandboxworkload.PlacementBinding{
		Registry: sandboxworkload.ObjectIdentity{Name: f.state.Name, UID: f.state.UID},
		Agent:    expected.Agent, Namespace: expected.Namespace, Workload: expected.Workload, Home: expected.Home,
		Nodes: []sandboxworkload.PlacementNode{
			{Name: f.ordinary.Name, UID: f.ordinary.UID, ProviderID: f.ordinary.Spec.ProviderID},
			{Name: f.reserved.Name, UID: f.reserved.UID, ProviderID: f.reserved.Spec.ProviderID},
		},
	}
	wantSource := sandboxworkload.SourceReference{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: f.state.Namespace, Name: f.state.Name,
		UID: f.state.UID, ResourceVersion: f.state.ResourceVersion,
	}
	if !reflect.DeepEqual(binding, want) || source != wantSource {
		t.Fatalf("incorrect canonical binding or source: binding=%+v source=%+v", binding, source)
	}
	if !slices.Equal(reads, []string{f.state.Name, f.ordinary.Name, f.reserved.Name, f.ordinary.Name, f.reserved.Name, f.state.Name}) {
		t.Fatalf("all selected Nodes and the registry must be rechecked, with the registry last: %v", reads)
	}

	slices.Reverse(f.registry.Nodes)
	f.saveRegistry(t)
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		object.SetAnnotations(map[string]string{"example.com/metadata": "refreshed"})
		if node, ok := object.(*corev1.Node); ok {
			node.ResourceVersion = "new-node-version"
			node.Labels["example.com/unrelated"] = "refreshed"
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		}
	}}
	next, freshSource, err := f.handler.ReadAssignment(context.Background(), expected)
	if err != nil || !reflect.DeepEqual(binding, next) {
		t.Fatalf("metadata, status, Node RV or registry order changed the immutable binding: %v", err)
	}
	wantSource.ResourceVersion = f.state.ResourceVersion
	if freshSource != wantSource || freshSource.ResourceVersion == source.ResourceVersion {
		t.Fatal("the stable binding did not retain a separate fresh registry source")
	}
	expected.Placement = binding.DeepCopy()
	if _, _, err := f.handler.ReadAssignment(context.Background(), expected); err != nil {
		t.Fatalf("historical binding rejected harmless source churn: %v", err)
	}
}

func TestReadAssignmentSupportsTupleOnlyRechecks(t *testing.T) {
	f := newFixture(t)
	reserveOrdinaryNode(t, f)
	initial := assignmentExpectation(f)
	initial.Template.Spec.NodeName = f.reserved.Name
	binding, previousSource, err := f.handler.ReadAssignment(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(f.registry.Nodes)
	f.saveRegistry(t)
	recheck := sandboxworkload.Expectation{
		Agent: initial.Agent, Namespace: initial.Namespace, Workload: initial.Workload, Home: initial.Home,
	}
	reader := &countingReader{Reader: f.client}
	f.handler.reader = reader
	for _, recorded := range []*sandboxworkload.PlacementBinding{nil, binding.DeepCopy()} {
		recheck.Placement = recorded
		current, source, err := f.handler.ReadAssignment(context.Background(), recheck)
		if err != nil || !reflect.DeepEqual(current, binding) {
			t.Fatalf("tuple-only recheck with zero Template changed the binding: %v", err)
		}
		wantSource := previousSource
		wantSource.ResourceVersion = f.state.ResourceVersion
		if source != wantSource || source.ResourceVersion == previousSource.ResourceVersion {
			t.Fatal("tuple-only recheck did not return the fresh registry source")
		}
	}
	if reader.gets.Load() != 12 || reader.lists.Load() != 0 {
		t.Fatal("tuple-only rechecks must still read and recheck both Nodes and the registry")
	}
	for name, mutate := range map[string]func(*corev1.Node){
		"UID":      func(node *corev1.Node) { node.UID = "replacement-node-uid" },
		"provider": func(node *corev1.Node) { node.Spec.ProviderID = "example://replacement-worker" },
	} {
		t.Run(name, func(t *testing.T) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok && node.Name == f.ordinary.Name {
					mutate(node)
				}
			}}
			for _, recorded := range []*sandboxworkload.PlacementBinding{nil, binding.DeepCopy()} {
				recheck.Placement = recorded
				expectAssignmentFailure(t, f.handler, recheck, "NodeIdentityMismatch")
			}
		})
	}
}

func TestReadAssignmentUsesCallerAssertionsWithoutVerifyingOwnershipOrStarting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, object := range []client.Object{f.agent.Agent, f.agent.Namespace, f.workload, f.agent.Input.Home} {
		if err := f.client.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	reader := &countingReader{Reader: f.client}
	f.handler.reader = reader
	expected := assignmentExpectation(f)
	if expected.Version != "" || expected.AgentGeneration != 0 || expected.Sources != nil || expected.WorkloadKind != "" {
		t.Fatal("reader fixture must contain only caller-verified identities and the template")
	}
	binding, _, err := f.handler.ReadAssignment(ctx, expected)
	if err != nil || binding.Home != expected.Home || reader.gets.Load() != 4 || reader.lists.Load() != 0 {
		t.Fatalf("reader treated asserted ownership as its responsibility or changed the HOME: %v", err)
	}
	current := &corev1.ConfigMap{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.expectation), current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != f.expectation.ResourceVersion ||
		current.Data[sandboxworkload.PodExpectationKey] != f.expectation.Data[sandboxworkload.PodExpectationKey] || !f.expected.ExecutionHeld {
		t.Fatal("reading an assignment changed controller state or the execution hold")
	}
}

func TestReadAssignmentRejectsIncompleteAndMismatchedAssertions(t *testing.T) {
	for name, mutate := range map[string]func(*sandboxworkload.Expectation){
		"missing Agent":     func(e *sandboxworkload.Expectation) { e.Agent.UID = "" },
		"missing namespace": func(e *sandboxworkload.Expectation) { e.Namespace.Name = "" },
		"missing workload":  func(e *sandboxworkload.Expectation) { e.Workload.UID = "" },
		"missing HOME":      func(e *sandboxworkload.Expectation) { e.Home.UID = "" },
		"unsafe HOME":       func(e *sandboxworkload.Expectation) { e.Home.UID = "synthetic-private\nuid" },
		"other Agent name":  func(e *sandboxworkload.Expectation) { e.Agent.Name = "other-agent" },
		"other Agent UID":   func(e *sandboxworkload.Expectation) { e.Agent.UID = "other-agent-uid" },
		"other namespace":   func(e *sandboxworkload.Expectation) { e.Namespace.UID = "other-namespace-uid" },
		"other workload":    func(e *sandboxworkload.Expectation) { e.Workload.UID = "other-workload-uid" },
		"other workload name": func(e *sandboxworkload.Expectation) {
			e.Workload.Name = "other-workload"
		},
		"unrecorded explicit destination": func(e *sandboxworkload.Expectation) { e.Template.Spec.NodeName = "unrecorded-worker" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			expected := assignmentExpectation(f)
			mutate(&expected)
			expectAssignmentFailure(t, f.handler, expected, "")
		})
	}
	f := newFixture(t)
	binding, _, err := f.handler.ReadAssignment(context.Background(), assignmentExpectation(f))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*sandboxworkload.PlacementBinding){
		func(b *sandboxworkload.PlacementBinding) { b.Home.UID = "other-home-uid" },
		func(b *sandboxworkload.PlacementBinding) { b.Agent.UID = "other-agent-uid" },
		func(b *sandboxworkload.PlacementBinding) { b.Workload.UID = "other-workload-uid" },
		func(b *sandboxworkload.PlacementBinding) { b.Registry.UID = "other-registry-uid" },
	} {
		expected := assignmentExpectation(f)
		expected.Placement = binding.DeepCopy()
		mutate(expected.Placement)
		expectAssignmentFailure(t, f.handler, expected, "PlacementBinding")
	}
}

func TestReadAssignmentRejectsConflictingOwnershipAndMissingReservations(t *testing.T) {
	for _, overlap := range []string{"Agent name", "Agent UID", "namespace name", "namespace UID", "workload UID", "none"} {
		t.Run(overlap, func(t *testing.T) {
			f := newFixture(t)
			other := &nodeReservation{
				Agent:       sandboxworkload.ObjectIdentity{Name: "other-agent", UID: "other-agent-uid"},
				Namespace:   sandboxworkload.ObjectIdentity{Name: "other-workloads", UID: "other-namespace-uid"},
				StatefulSet: sandboxworkload.ObjectIdentity{Name: f.expected.Workload.Name, UID: "other-workload-uid"},
			}
			switch overlap {
			case "Agent name":
				other.Agent.Name = f.expected.Agent.Name
			case "Agent UID":
				other.Agent.UID = f.expected.Agent.UID
			case "namespace name":
				other.Namespace = f.expected.Namespace
			case "namespace UID":
				other.Namespace.UID = f.expected.Namespace.UID
			case "workload UID":
				other.StatefulSet.UID = f.expected.Workload.UID
			}
			f.registry.Nodes[1].Reservation = other
			f.saveRegistry(t)
			if overlap != "none" {
				expectAssignmentFailure(t, f.handler, assignmentExpectation(f), "AgentAssignmentAmbiguous")
			} else if _, _, err := f.handler.ReadAssignment(context.Background(), assignmentExpectation(f)); err != nil {
				t.Fatalf("same StatefulSet name in an unrelated namespace created false ambiguity: %v", err)
			}
		})
	}
	f := newFixture(t)
	f.registry.Nodes[0].Reservation = nil
	f.saveRegistry(t)
	expectAssignmentFailure(t, f.handler, assignmentExpectation(f), "AgentAssignmentMissing")
}

func TestReadAssignmentRejectsInvalidAuthority(t *testing.T) {
	for _, target := range []string{"Registry", "Node"} {
		for name, mutate := range map[string]func(client.Object){
			"missing UID":     func(o client.Object) { o.SetUID("") },
			"missing version": func(o client.Object) { o.SetResourceVersion("") },
			"wrong key":       func(o client.Object) { o.SetName("synthetic-private-other-name") },
			"terminating":     func(o client.Object) { o.SetDeletionTimestamp(ptr.To(metav1.Unix(1, 0))) },
		} {
			t.Run(target+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
					if stageForObject(object, f.state.Name) == target {
						mutate(object)
					}
				}}
				expectAssignmentFailure(t, f.handler, assignmentExpectation(f), target+"Invalid")
			})
		}
		t.Run(target+"/missing", func(t *testing.T) {
			f := newFixture(t)
			var object client.Object = f.reserved
			if target == "Registry" {
				object = f.state
			}
			if err := f.client.Delete(context.Background(), object); err != nil {
				t.Fatal(err)
			}
			expectAssignmentFailure(t, f.handler, assignmentExpectation(f), target+"Missing")
		})
	}
	for name, mutate := range map[string]func(*corev1.Node){
		"UID replacement":      func(n *corev1.Node) { n.UID = "replacement-uid" },
		"provider replacement": func(n *corev1.Node) { n.Spec.ProviderID = "example://replacement" },
		"provider missing":     func(n *corev1.Node) { n.Spec.ProviderID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			reserveOrdinaryNode(t, f)
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok && node.Name == f.ordinary.Name {
					mutate(node)
				}
			}}
			expectAssignmentFailure(t, f.handler, assignmentExpectation(f), "NodeIdentityMismatch")
		})
	}
	for _, raw := range []string{
		`{"version":"v1","version":"v1","nodes":[],"platformDaemonSets":[]}`,
		`{"version":"v2","nodes":[],"platformDaemonSets":[]}`,
		`{"version":"v1","nodes":[],"platformDaemonSets":[],"home":{"name":"not-registry-authority","uid":"example"}}`,
		`{"version":"v1","nodes":[null],"platformDaemonSets":[]}`,
	} {
		f := newFixture(t)
		f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
			if state, ok := object.(*corev1.ConfigMap); ok {
				state.Data[ConfigMapKey] = raw
			}
		}}
		expectAssignmentFailure(t, f.handler, assignmentExpectation(f), "RegistryInvalid")
	}
}

func TestReadAssignmentRechecksEverySelectedSnapshot(t *testing.T) {
	for _, target := range []string{"sandbox-placement", "ordinary-worker", "reserved-worker"} {
		for _, drift := range []string{"UID", "version", "metadata", "content", "termination", "missing"} {
			t.Run(target+"/"+drift, func(t *testing.T) {
				f := newFixture(t)
				reserveOrdinaryNode(t, f)
				var reads int
				f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
					if object.GetName() != target {
						return
					}
					reads++
					if drift == "missing" && reads == 1 {
						if err := f.client.Delete(context.Background(), object); err != nil {
							t.Fatal(err)
						}
					}
					if reads < 2 {
						return
					}
					switch drift {
					case "UID":
						object.SetUID("changed-uid")
					case "version":
						object.SetResourceVersion("changed-version")
					case "metadata":
						object.SetAnnotations(map[string]string{"example.com/drift": "changed-without-RV"})
					case "content":
						switch object := object.(type) {
						case *corev1.Node:
							object.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
						case *corev1.ConfigMap:
							reordered := f.registry
							reordered.Nodes = slices.Clone(reordered.Nodes)
							slices.Reverse(reordered.Nodes)
							object.Data[ConfigMapKey] = string(marshal(t, reordered))
						}
					case "termination":
						object.SetDeletionTimestamp(ptr.To(metav1.Unix(1, 0)))
					}
				}}
				reason := "Changed"
				if drift == "missing" {
					reason = "Missing"
				}
				expectAssignmentFailure(t, f.handler, assignmentExpectation(f), reason)
			})
		}
	}
}

func TestReadAssignmentIsBoundedReadOnlyAndConcurrent(t *testing.T) {
	f := newFixture(t)
	reader := &countingReader{Reader: f.client}
	f.handler.reader = reader
	expected := assignmentExpectation(f)
	binding, source, err := f.handler.ReadAssignment(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			copy, freshSource, err := f.handler.ReadAssignment(context.Background(), expected)
			if err != nil || !reflect.DeepEqual(copy, binding) || source != freshSource {
				t.Errorf("concurrent immutable assignment differs: %v", err)
				return
			}
			copy.Nodes[0].UID = "mutated-returned-copy"
		})
	}
	workers.Wait()
	if reader.gets.Load() != 9*4 || reader.lists.Load() != 0 {
		t.Fatal("assignment reader did not use only bounded GETs")
	}
	f.handler.reader = errorReader{Reader: f.client, err: fmt.Errorf("synthetic-private-backend-error")}
	expectAssignmentFailure(t, f.handler, expected, "ReadFailed")
	budget := &budgetReader{Reader: f.client}
	f.handler, err = NewHandler(budget, Config{Namespace: f.state.Namespace, ConfigMapName: f.state.Name, RequestTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	expectAssignmentFailure(t, f.handler, expected, "ReadFailed")
	if time.Since(start) > time.Second || budget.gets != 2 {
		t.Fatal("assignment reader reset or exceeded its total read budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	copy, current, err := f.handler.ReadAssignment(ctx, expected)
	if err == nil || copy != nil || current != (sandboxworkload.SourceReference{}) {
		t.Fatal("canceled assignment read returned authority")
	}
}

func TestReadAssignmentSupportsFullBoundedReservation(t *testing.T) {
	f := newFixture(t)
	reservation := *f.registry.Nodes[0].Reservation
	f.registry.Nodes = nil
	for i := range MaxNodes {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("worker-%03d", i), UID: types.UID(fmt.Sprintf("worker-uid-%03d", i))},
			Spec:       corev1.NodeSpec{ProviderID: fmt.Sprintf("example://worker-%03d", i)},
		}
		if err := f.client.Create(context.Background(), node); err != nil {
			t.Fatal(err)
		}
		f.registry.Nodes = append(f.registry.Nodes, registeredNode{
			Name: node.Name, UID: node.UID, ProviderID: node.Spec.ProviderID, Reservation: &reservation,
		})
	}
	f.saveRegistry(t)
	reader := &countingReader{Reader: f.client}
	f.handler.reader = reader
	binding, _, err := f.handler.ReadAssignment(context.Background(), assignmentExpectation(f))
	if err != nil || len(binding.Nodes) != MaxNodes || reader.gets.Load() != 2*(MaxNodes+1) {
		t.Fatalf("full 256-Node reservation was not completely verified: %v", err)
	}
}
