// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

func TestCanonicalObjectsDoesNotSortCallerInput(t *testing.T) {
	input := []api.AgentPolicyPort{{Port: 8080}, {Port: 443}, {Port: 443}}
	before := slices.Clone(input)
	result, err := canonicalObjects(input)
	if err != nil || len(result) != 2 || !reflect.DeepEqual(input, before) {
		t.Fatalf("canonicalization mutated input or failed: %v, %v", result, err)
	}
	result[0].Protocol = corev1.ProtocolUDP
	if !reflect.DeepEqual(input, before) {
		t.Fatal("canonical result shares the caller's backing array")
	}
	for _, empty := range [][]api.AgentPolicyPort{nil, {}} {
		result, err := canonicalObjects(empty)
		if err != nil || !reflect.DeepEqual(result, empty) {
			t.Fatal("canonicalization changed nil/empty serialization", err)
		}
	}
}

func TestCanonicalSerializationErrorsAreNotDiscarded(t *testing.T) {
	unsupported := make(chan struct{})
	if result, err := canonicalObjects([]any{"valid", unsupported}); err == nil || result != nil {
		t.Fatal("unsupported JSON input became a canonical result", err)
	}
	if hash, err := digest(unsupported); err == nil || hash != "" {
		t.Fatal("failed digest supplied a sliceable revision", err)
	}
	if resource, err := manifest(unsupported); err == nil || resource.Object != nil {
		t.Fatal("failed serialization supplied an applyable resource", err)
	}
}

func TestCanonicalBindingReferencesAreIndependent(t *testing.T) {
	input := []BindingReference{{Name: "b"}, {Name: "a"}, {Name: "a"}}
	before := slices.Clone(input)
	got := canonicalBindingReferences(input)
	if len(got) != 2 || got[0].Name != "a" || !slices.Equal(before, input) {
		t.Fatal("binding canonicalization changed caller input or lost ordering")
	}
	got[0].Name = "changed"
	if !slices.Equal(before, input) {
		t.Fatal("binding canonicalization shares the caller's backing array")
	}
}

func TestSupportedPolicyRequiresNormalization(t *testing.T) {
	input := compilerFixture()
	if err := supportedPolicy(input.Resolution.AgentPolicy, input.Resolution.Capabilities); !errors.Is(err, ErrUnsupportedControl) {
		t.Fatal("unnormalized policy must not be dereferenced or silently accepted", err)
	}
	normalized, errs := sandbox.NormalizeAgentPolicy(input.Resolution.AgentPolicy)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := supportedPolicy(normalized, input.Resolution.Capabilities); err != nil {
		t.Fatal("normalized supported policy was rejected", err)
	}
	normalized.Network.Ingress.Mode = ""
	if err := supportedPolicy(normalized, input.Resolution.Capabilities); !errors.Is(err, ErrUnsupportedControl) {
		t.Fatal("missing normalized ingress mode was silently accepted", err)
	}
}
