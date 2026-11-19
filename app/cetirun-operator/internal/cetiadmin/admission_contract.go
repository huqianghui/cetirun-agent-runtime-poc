// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"maps"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Kubernetes defaults an absent webhook selector to an empty selector.
// Both select everything; a requested nonempty selector must still match.
func AdmissionSelectorMatches(actual, expected *metav1.LabelSelector) bool {
	if expected == nil {
		return actual == nil || (len(actual.MatchLabels) == 0 && len(actual.MatchExpressions) == 0)
	}
	return reflect.DeepEqual(actual, expected)
}

// AKS adds these exclusions to namespace selectors, never object selectors.
// Callers must verify the independent namespace guard before accepting them.
func AdmissionNamespaceSelectorMatches(actual, expected *metav1.LabelSelector) bool {
	if AdmissionSelectorMatches(actual, expected) {
		return true
	}
	if actual == nil {
		return false
	}
	if expected == nil {
		expected = &metav1.LabelSelector{}
	}
	if !maps.Equal(actual.MatchLabels, expected.MatchLabels) || len(actual.MatchExpressions) != len(expected.MatchExpressions)+2 {
		return false
	}
	required := append([]metav1.LabelSelectorRequirement(nil), expected.MatchExpressions...)
	required = append(required,
		metav1.LabelSelectorRequirement{Key: "control-plane", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"true"}},
		metav1.LabelSelectorRequirement{Key: "kubernetes.azure.com/managedby", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"aks"}})
	for _, want := range required {
		count := 0
		for _, got := range actual.MatchExpressions {
			if reflect.DeepEqual(want, got) {
				count++
			}
		}
		if count != 1 {
			return false
		}
	}
	return true
}

func NamespaceExcludedFromAdmission(labels map[string]string) bool {
	return labels["control-plane"] == "true" || labels["kubernetes.azure.com/managedby"] == "aks"
}
