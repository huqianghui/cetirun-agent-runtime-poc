// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestMigrationSchedulingIntersectsEveryEligibleTerm(t *testing.T) {
	spec := &corev1.PodSpec{NodeSelector: map[string]string{"example.com/runtime": "approved"},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "example.com/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}},
				{},
				{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"excluded"}}}},
			}},
		}}}
	before := spec.DeepCopy()
	if err := PinMigrationNode(spec, "node-b"); err != nil {
		t.Fatal(err)
	}
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 3 || len(terms[1].MatchFields) != 0 || len(terms[1].MatchExpressions) != 0 ||
		terms[0].MatchExpressions[0].Values[0] != "a" || len(terms[2].MatchFields) != 2 ||
		terms[0].MatchFields[0].Values[0] != "node-b" || terms[2].MatchFields[1].Values[0] != "node-b" ||
		spec.NodeName != "" || !reflect.DeepEqual(spec.NodeSelector, before.NodeSelector) {
		t.Fatal("migration broadened scheduling or bypassed the scheduler")
	}
	if err := UnpinMigrationNode(spec, "node-b"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, spec) {
		t.Fatal("removing the migration pin changed pre-existing scheduling")
	}
}

func TestMigrationSchedulingDoesNotLiftEmptyOrExplicitPlacement(t *testing.T) {
	for _, spec := range []*corev1.PodSpec{
		{NodeName: "node-a"},
		{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{}}}},
		{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{}},
		}}}},
	} {
		if err := PinMigrationNode(spec, "node-b"); err == nil {
			t.Fatal("migration relaxed an explicit or unsatisfiable placement")
		}
	}
	spec := &corev1.PodSpec{}
	if err := PinMigrationNode(spec, "node-b"); err != nil {
		t.Fatal(err)
	}
	if err := UnpinMigrationNode(spec.DeepCopy(), "different-node"); err == nil {
		t.Fatal("a different migration could discard scheduling requirements")
	}
	if err := UnpinMigrationNode(spec, "node-b"); err != nil {
		t.Fatal(err)
	}
	if spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Fatal("a first migration left an unsatisfiable empty selector")
	}
}
