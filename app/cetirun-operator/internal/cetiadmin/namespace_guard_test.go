// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"context"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func aksSelectorFixture() []metav1.LabelSelectorRequirement {
	return []metav1.LabelSelectorRequirement{
		{Key: "control-plane", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"true"}},
		{Key: "kubernetes.azure.com/managedby", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"aks"}},
	}
}

func TestNamespaceSelectorAcceptsOnlyExactAKSExclusions(t *testing.T) {
	for _, expected := range []*metav1.LabelSelector{nil, {MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}} {
		actual := &metav1.LabelSelector{MatchExpressions: aksSelectorFixture()}
		if expected != nil {
			actual.MatchLabels = expected.DeepCopy().MatchLabels
		}
		if !AdmissionNamespaceSelectorMatches(actual, expected) || AdmissionSelectorMatches(actual, expected) {
			t.Fatal("known namespace exclusions were rejected or also allowed as object selectors")
		}
		actual.MatchExpressions[0], actual.MatchExpressions[1] = actual.MatchExpressions[1], actual.MatchExpressions[0]
		if !AdmissionNamespaceSelectorMatches(actual, expected) {
			t.Fatal("selector order changed equivalent namespace matching")
		}
		for _, change := range []string{"extra", "duplicate", "value", "operator", "labels", "missing"} {
			t.Run(change, func(t *testing.T) {
				wrong := actual.DeepCopy()
				switch change {
				case "extra":
					wrong.MatchExpressions = append(wrong.MatchExpressions, metav1.LabelSelectorRequirement{Key: "skip", Operator: metav1.LabelSelectorOpDoesNotExist})
				case "duplicate":
					wrong.MatchExpressions[1] = wrong.MatchExpressions[0]
				case "value":
					wrong.MatchExpressions[0].Values = append(wrong.MatchExpressions[0].Values, "tenant")
				case "operator":
					wrong.MatchExpressions[0].Operator = metav1.LabelSelectorOpIn
				case "labels":
					wrong.MatchLabels = map[string]string{"skip": "true"}
				case "missing":
					wrong.MatchExpressions = wrong.MatchExpressions[:1]
				}
				if AdmissionNamespaceSelectorMatches(wrong, expected) {
					t.Fatal("unknown namespace exclusion accepted")
				}
			})
		}
	}
}

func TestNamespaceAdmissionGuardReadback(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := admissionv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"none", "policy absent", "binding absent", "fail open", "selector", "false validation", "warn only", "source excluded", "admin excluded"} {
		t.Run(change, func(t *testing.T) {
			policy, binding := ExpectedNamespaceAdmissionGuard("platform-test", "cetirun-admin")
			platform := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "platform-test"}}
			admin := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-admin"}}
			objects := []client.Object{platform, admin}
			switch change {
			case "fail open":
				policy.Spec.FailurePolicy = ptr.To(admissionv1.Ignore)
			case "selector":
				policy.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"skip": "false"}}
			case "false validation":
				policy.Spec.Validations[0].Expression = "true"
			case "warn only":
				binding.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Warn}
			case "source excluded":
				platform.Labels = map[string]string{"control-plane": "true"}
			case "admin excluded":
				admin.Labels = map[string]string{"kubernetes.azure.com/managedby": "aks"}
			}
			if change != "policy absent" {
				objects = append(objects, policy)
			}
			if change != "binding absent" {
				objects = append(objects, binding)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			err := CheckNamespaceAdmissionGuard(context.Background(), kube, platform.Name, admin.Name)
			if (err == nil) != (change == "none") {
				t.Fatalf("changed guard %s: %v", change, err)
			}
		})
	}
}

func TestHookAKSExclusionsRequireIndependentNamespaceGuard(t *testing.T) {
	kube, options, _, state := hookTestFixture(t, true)
	validation := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin-admission"}}
	hookTestUpdate(t, kube, validation, func() {
		for i := range validation.Webhooks {
			if validation.Webhooks[i].NamespaceSelector == nil {
				validation.Webhooks[i].NamespaceSelector = &metav1.LabelSelector{}
			}
			validation.Webhooks[i].NamespaceSelector.MatchExpressions = aksSelectorFixture()
		}
	})
	mutating := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: validation.Name}}
	hookTestUpdate(t, kube, mutating, func() {
		mutating.Webhooks[0].NamespaceSelector = &metav1.LabelSelector{MatchExpressions: aksSelectorFixture()}
	})
	if err := RunHook(context.Background(), kube, options); err == nil {
		t.Fatal("AKS exclusions were accepted without an independent namespace guard")
	}
	policy, binding := ExpectedNamespaceAdmissionGuard(options.Namespace, state.Namespace.Name)
	for _, object := range []client.Object{policy, binding, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: options.Namespace}}} {
		if err := kube.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	if err := RunHook(context.Background(), kube, options); err != nil {
		t.Fatal(err)
	}
	hookTestUpdate(t, kube, validation, func() {
		validation.Webhooks[0].ObjectSelector = &metav1.LabelSelector{MatchExpressions: aksSelectorFixture()}
	})
	if err := RunHook(context.Background(), kube, options); err == nil {
		t.Fatal("namespace compatibility incorrectly admitted an object selector bypass")
	}
}
