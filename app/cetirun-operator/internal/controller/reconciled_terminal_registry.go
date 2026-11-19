// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Registry isolation can verify this exact CEL gate without relying on a
// webhook's implementation. Common Sandbox admission still applies as well.
func (r *AgentReconciler) ensureCommonTerminalExecPolicy(ctx context.Context, agent *api.Agent) error {
	if err := r.checkAgentGeneration(ctx, agent); err != nil {
		return err
	}
	namespace, workload := instanceNamespace(agent.Name), instanceServiceName(agent.Name)
	name := workload + "-" + namespace + "-deny-exec"
	metadata := metav1.ObjectMeta{
		Name: name, OwnerReferences: []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)},
		Annotations: map[string]string{customAgentUID: string(agent.UID)},
	}
	policy := &admissionv1.ValidatingAdmissionPolicy{
		ObjectMeta: metadata,
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: ptr.To(admissionv1.Fail),
			MatchConstraints: &admissionv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
				ObjectSelector:    &metav1.LabelSelector{},
				MatchPolicy:       ptr.To(admissionv1.Equivalent),
				ResourceRules: []admissionv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionv1.RuleWithOperations{
						Operations: []admissionv1.OperationType{admissionv1.Connect},
						Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/exec"}, Scope: ptr.To(admissionv1.AllScopes)},
					},
				}},
			},
			Validations: []admissionv1.Validation{{
				Expression: terminalpolicy.AdmissionExpression(namespace, workload+"-0", "cetirun",
					terminalpolicy.WorkloadName(workload), string(agent.UID)),
				Reason:  ptr.To(metav1.StatusReasonForbidden),
				Message: "kubectl exec is not allowed on openclaw pods",
			}},
		},
	}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: *metadata.DeepCopy(),
		Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
		},
	}
	for _, desired := range []client.Object{policy, binding} {
		current := desired.DeepCopyObject().(client.Object)
		err := r.customReader().Get(ctx, client.ObjectKeyFromObject(desired), current)
		if apierrors.IsNotFound(err) {
			if err := r.checkAgentGeneration(ctx, agent); err != nil {
				return err
			}
			if err := r.Create(ctx, desired); err != nil {
				return fmt.Errorf("create common terminal exec policy: %w", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		if !current.GetDeletionTimestamp().IsZero() ||
			!equality.Semantic.DeepEqual(current.GetOwnerReferences(), desired.GetOwnerReferences()) {
			return fmt.Errorf("common terminal exec policy has unconfirmed ownership")
		}
		before := current.DeepCopyObject().(client.Object)
		switch value := current.(type) {
		case *admissionv1.ValidatingAdmissionPolicy:
			if equality.Semantic.DeepEqual(value.Spec, policy.Spec) {
				continue
			}
			value.Spec = *policy.Spec.DeepCopy()
		case *admissionv1.ValidatingAdmissionPolicyBinding:
			if equality.Semantic.DeepEqual(value.Spec, binding.Spec) {
				continue
			}
			value.Spec = *binding.Spec.DeepCopy()
		}
		if err := r.checkAgentGeneration(ctx, agent); err != nil {
			return err
		}
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("update common terminal exec policy: %w", err)
		}
	}
	return nil
}
