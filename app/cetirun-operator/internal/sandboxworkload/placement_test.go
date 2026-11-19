// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/types"
)

func placementBinding() *w.PlacementBinding {
	return &w.PlacementBinding{
		Registry:  w.ObjectIdentity{Name: "sandbox-placement", UID: "registry-uid"},
		Agent:     w.ObjectIdentity{Name: "example", UID: "agent-uid"},
		Namespace: w.ObjectIdentity{Name: "example-workloads", UID: "namespace-uid"},
		Workload:  w.ObjectIdentity{Name: "example-runtime", UID: "workload-uid"},
		Home:      w.ObjectIdentity{Name: "example-home", UID: "home-uid"},
		Nodes: []w.PlacementNode{
			{Name: "node-a", UID: "node-a-uid", ProviderID: "example://node-a"},
			{Name: "node-b", UID: "node-b-uid", ProviderID: "example://node-b"},
		},
	}
}

func TestPlacementBindingRequiresCompleteCanonicalIdentities(t *testing.T) {
	if err := placementBinding().Validate(); err != nil {
		t.Fatal(err)
	}
	var absent *w.PlacementBinding
	if err := absent.Validate(); err == nil {
		t.Fatal("nil binding was accepted")
	}
	for name, mutate := range map[string]func(*w.PlacementBinding){
		"registry name":   func(b *w.PlacementBinding) { b.Registry.Name = "" },
		"registry UID":    func(b *w.PlacementBinding) { b.Registry.UID = "" },
		"Agent name":      func(b *w.PlacementBinding) { b.Agent.Name = "" },
		"Agent UID":       func(b *w.PlacementBinding) { b.Agent.UID = "" },
		"namespace name":  func(b *w.PlacementBinding) { b.Namespace.Name = "" },
		"namespace UID":   func(b *w.PlacementBinding) { b.Namespace.UID = "" },
		"workload name":   func(b *w.PlacementBinding) { b.Workload.Name = "" },
		"workload UID":    func(b *w.PlacementBinding) { b.Workload.UID = "" },
		"HOME name":       func(b *w.PlacementBinding) { b.Home.Name = "" },
		"HOME UID":        func(b *w.PlacementBinding) { b.Home.UID = "" },
		"invalid name":    func(b *w.PlacementBinding) { b.Home.Name = "synthetic-sensitive/path" },
		"invalid NS name": func(b *w.PlacementBinding) { b.Namespace.Name = "not.a.namespace" },
		"unsafe UID":      func(b *w.PlacementBinding) { b.Agent.UID = "synthetic-sensitive\nuid" },
		"oversized UID":   func(b *w.PlacementBinding) { b.Workload.UID = types.UID(strings.Repeat("u", 129)) },
		"nil nodes":       func(b *w.PlacementBinding) { b.Nodes = nil },
		"empty nodes":     func(b *w.PlacementBinding) { b.Nodes = []w.PlacementNode{} },
		"node name":       func(b *w.PlacementBinding) { b.Nodes[0].Name = "" },
		"node UID":        func(b *w.PlacementBinding) { b.Nodes[0].UID = "" },
		"uppercase node":  func(b *w.PlacementBinding) { b.Nodes[0].Name = "Node-A" },
		"duplicate name":  func(b *w.PlacementBinding) { b.Nodes[1].Name = b.Nodes[0].Name },
		"duplicate UID":   func(b *w.PlacementBinding) { b.Nodes[1].UID = b.Nodes[0].UID },
		"duplicate provider": func(b *w.PlacementBinding) {
			b.Nodes[1].ProviderID = b.Nodes[0].ProviderID
		},
		"unsorted nodes": func(b *w.PlacementBinding) { slices.Reverse(b.Nodes) },
	} {
		t.Run(name, func(t *testing.T) {
			binding := placementBinding()
			mutate(binding)
			err := binding.Validate()
			if err == nil {
				t.Fatal("incomplete or ambiguous placement binding was accepted")
			}
			if len(err.Error()) > 256 || strings.Contains(err.Error(), "synthetic-sensitive") {
				t.Fatal("binding validation disclosed input or exceeded its diagnostic bound")
			}
		})
	}
}

func TestPlacementBindingRejectsUnsafeProviderAndNodeUIDValues(t *testing.T) {
	for _, value := range []string{"", " ", "example://node a", "example://node\nsecret", "\tvalue",
		"example://node\x00", "example://node\x7f", "example://node\u200b", string([]byte{255})} {
		for _, field := range []string{"provider", "UID"} {
			t.Run(fmt.Sprintf("%s/%q", field, value), func(t *testing.T) {
				binding := placementBinding()
				if field == "provider" {
					binding.Nodes[0].ProviderID = value
				} else {
					binding.Nodes[0].UID = types.UID(value)
				}
				if err := binding.Validate(); err == nil {
					t.Fatal("unsafe opaque identity accepted")
				}
			})
		}
	}
	binding := placementBinding()
	binding.Nodes[0].ProviderID = strings.Repeat("p", 2048)
	binding.Nodes[0].UID = types.UID(strings.Repeat("u", 128))
	if err := binding.Validate(); err != nil {
		t.Fatalf("inclusive identity limits rejected: %v", err)
	}
	binding.Nodes[0].ProviderID += "p"
	if err := binding.Validate(); err == nil {
		t.Fatal("oversized provider accepted")
	}
	binding.Nodes[0].ProviderID = "example://node-a"
	binding.Nodes[0].UID += "u"
	if err := binding.Validate(); err == nil {
		t.Fatal("oversized Node UID accepted")
	}
}

func TestPlacementBindingNodeCountLimit(t *testing.T) {
	binding := placementBinding()
	binding.Nodes = nil
	for i := range w.MaxPlacementNodes {
		binding.Nodes = append(binding.Nodes, w.PlacementNode{
			Name: fmt.Sprintf("node-%03d", i), UID: types.UID(fmt.Sprintf("node-uid-%03d", i)), ProviderID: fmt.Sprintf("example://node-%03d", i),
		})
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("256 Nodes rejected: %v", err)
	}
	binding.Nodes = append(binding.Nodes, w.PlacementNode{Name: "node-256", UID: "node-uid-256", ProviderID: "example://node-256"})
	if err := binding.Validate(); err == nil {
		t.Fatal("257 Nodes accepted")
	}
}

func TestPlacementBindingMatchesExactExpectationTuple(t *testing.T) {
	binding := placementBinding()
	expected := w.Expectation{Agent: binding.Agent, Namespace: binding.Namespace, Workload: binding.Workload, Home: binding.Home}
	if err := binding.MatchesExpectation(expected); err != nil {
		t.Fatalf("partial caller-verified expectation rejected: %v", err)
	}
	for _, name := range []string{binding.Nodes[0].Name, binding.Nodes[1].Name} {
		expected.Template.Spec.NodeName = name
		if err := binding.MatchesExpectation(expected); err != nil {
			t.Fatal(err)
		}
	}
	expected.Template.Spec.NodeName = "not-recorded"
	if err := binding.MatchesExpectation(expected); err == nil {
		t.Fatal("unrecorded explicit template destination accepted")
	}
	expected.Template.Spec.NodeName = ""
	for _, field := range []string{"Agent", "Namespace", "Workload", "Home"} {
		for _, part := range []string{"Name", "UID"} {
			t.Run(field+"/"+part, func(t *testing.T) {
				changed := expected
				value := reflect.ValueOf(&changed).Elem().FieldByName(field).FieldByName(part)
				value.SetString("synthetic-different-identity")
				err := binding.MatchesExpectation(changed)
				if err == nil || strings.Contains(err.Error(), "synthetic-different-identity") {
					t.Fatal("identity mismatch accepted or disclosed")
				}
			})
		}
	}
	invalid := binding.DeepCopy()
	invalid.Registry.UID = ""
	if err := invalid.MatchesExpectation(expected); err == nil {
		t.Fatal("tuple match waived binding validation")
	}
}

func TestPlacementBindingDeepCopyAndOptionalJSONContract(t *testing.T) {
	var absent *w.PlacementBinding
	if absent.DeepCopy() != nil {
		t.Fatal("nil copy changed optional legacy behavior")
	}
	binding := placementBinding()
	copied := binding.DeepCopy()
	if !reflect.DeepEqual(binding, copied) {
		t.Fatal("copy changed binding values")
	}
	copied.Nodes[0].UID = "changed-node-uid"
	copied.Home.UID = "changed-home-uid"
	if binding.Nodes[0].UID != "node-a-uid" || binding.Home.UID != "home-uid" {
		t.Fatal("copy aliased binding identity storage")
	}
	for _, placement := range []*w.PlacementBinding{nil, binding} {
		expected := w.Expectation{Placement: placement}
		raw, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		_, present := fields["placement"]
		if present != (placement != nil) {
			t.Fatal("placement must be omitted for legacy expectations")
		}
		var decoded w.Expectation
		if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(expected, decoded) {
			t.Fatalf("expectation placement did not round-trip: %v", err)
		}
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"registry":{"name":"sandbox-placement","uid":"registry-uid"},"agent":{"name":"example","uid":"agent-uid"},"namespace":{"name":"example-workloads","uid":"namespace-uid"},"workload":{"name":"example-runtime","uid":"workload-uid"},"home":{"name":"example-home","uid":"home-uid"},"nodes":[{"name":"node-a","uid":"node-a-uid","providerID":"example://node-a"},{"name":"node-b","uid":"node-b-uid","providerID":"example://node-b"}]}`
	if string(raw) != want {
		t.Fatalf("immutable binding JSON shape changed: %s", raw)
	}
}
