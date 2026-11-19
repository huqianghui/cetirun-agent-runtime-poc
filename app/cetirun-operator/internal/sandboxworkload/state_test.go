// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestPodExpectationObjectUsesOnlyV1(t *testing.T) {
	expected := Expectation{
		Version:   ExpectationVersion,
		Agent:     ObjectIdentity{Name: "agent", UID: types.UID("agent-uid")},
		Namespace: ObjectIdentity{Name: "workloads", UID: types.UID("namespace-uid")},
	}
	if expected.Version != "v1" {
		t.Fatalf("expectation must use v1, got %q", expected.Version)
	}
	if _, err := PodExpectationObject("operator-state", expected); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "v2", "v3"} {
		t.Run(version, func(t *testing.T) {
			expected.Version = version
			if _, err := PodExpectationObject("operator-state", expected); err == nil {
				t.Fatal("unsupported expectation version was accepted")
			}
		})
	}
}
