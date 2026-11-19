// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// PinMigrationNode intersects, rather than replaces, existing scheduling rules.
// Empty required terms remain unsatisfiable.
func PinMigrationNode(spec *corev1.PodSpec, node string) error {
	if len(validation.IsDNS1123Subdomain(node)) != 0 || spec.NodeName != "" {
		return fmt.Errorf("migration requires an explicit valid destination through the scheduler")
	}
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	affinity := spec.Affinity.NodeAffinity
	pin := corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}
	if affinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		affinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{pin}}},
		}
		return nil
	}
	eligible := false
	for i := range affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		term := &affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
		if len(term.MatchExpressions)+len(term.MatchFields) != 0 {
			term.MatchFields = append(term.MatchFields, pin)
			eligible = true
		}
	}
	if !eligible {
		return fmt.Errorf("migration cannot relax an empty required node affinity")
	}
	return nil
}

// UnpinMigrationNode removes only the final requirement added by a completed
// migration. Callers must first verify its controller-owned request annotation.
func UnpinMigrationNode(spec *corev1.PodSpec, node string) error {
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return fmt.Errorf("the previous migration's scheduling pin is missing")
	}
	affinity := spec.Affinity.NodeAffinity
	terms := affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	found := false
	for i := range terms {
		term := &terms[i]
		if len(term.MatchExpressions)+len(term.MatchFields) == 0 {
			continue
		}
		if len(term.MatchFields) == 0 {
			return fmt.Errorf("the previous migration's scheduling pin changed")
		}
		last := term.MatchFields[len(term.MatchFields)-1]
		if last.Key != "metadata.name" || last.Operator != corev1.NodeSelectorOpIn || !slices.Equal(last.Values, []string{node}) {
			return fmt.Errorf("the previous migration's scheduling pin changed")
		}
		term.MatchFields = term.MatchFields[:len(term.MatchFields)-1]
		if len(term.MatchFields) == 0 {
			term.MatchFields = nil
		}
		found = true
		if len(term.MatchExpressions)+len(term.MatchFields) == 0 {
			if len(terms) != 1 {
				return fmt.Errorf("the previous migration's scheduling terms are ambiguous")
			}
			affinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
		}
	}
	if !found {
		return fmt.Errorf("the previous migration has no scheduling pin")
	}
	return nil
}
