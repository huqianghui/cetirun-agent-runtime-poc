// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func unconfirmedKubernetesGrants(bindings []sandboxpolicy.BindingReference) []sandboxpolicy.BindingReference {
	var pending []sandboxpolicy.BindingReference
	for _, ref := range bindings {
		if ref.RoleRef.Kind == "ClusterRole" && ref.UID == "" {
			pending = append(pending, ref)
		}
	}
	return pending
}

const sandboxGrantSubmissionsAnnotation = "sandbox.cetirun.microsoft.com/grant-submissions"

func reconciledGrantSubmissions(state *corev1.ConfigMap) (map[string]bool, bool, error) {
	raw, present := state.Annotations[sandboxGrantSubmissionsAnnotation]
	if !present {
		return nil, false, nil
	}
	var digests []string
	if len(raw) > sandboxpolicy.MaxGrantJournalReferences*68 || json.Unmarshal([]byte(raw), &digests) != nil ||
		digests == nil || len(digests) > sandboxpolicy.MaxGrantJournalReferences {
		return nil, true, fmt.Errorf("Kubernetes grant submission inventory is invalid")
	}
	result := make(map[string]bool, len(digests))
	for _, digest := range digests {
		if !w.ValidInputDigest(digest) || result[digest] {
			return nil, true, fmt.Errorf("Kubernetes grant submission inventory is ambiguous")
		}
		result[digest] = true
	}
	return result, true, nil
}

func reconciledGrantDigest(ref sandboxpolicy.BindingReference) (string, error) {
	ref.UID = ""
	return sandboxPreparationDigest(ref)
}

func submittedReconciledGrants(state *corev1.ConfigMap, bindings []sandboxpolicy.BindingReference) ([]sandboxpolicy.BindingReference, error) {
	pending := unconfirmedKubernetesGrants(bindings)
	submissions, present, err := reconciledGrantSubmissions(state)
	if err != nil {
		return nil, err
	}
	if !present {
		if phase := state.Annotations[sandboxApplyStateAnnotation]; phase == sandboxApplyPending || phase == sandboxApplyCancelled {
			return nil, nil
		}
		return pending, nil
	}
	var submitted []sandboxpolicy.BindingReference
	for _, ref := range pending {
		digest, err := reconciledGrantDigest(ref)
		if err != nil {
			return nil, err
		}
		if submissions[digest] {
			submitted = append(submitted, ref)
		}
	}
	return submitted, nil
}

func (r *AgentReconciler) confirmReconciledGrantOutcomes(ctx context.Context, agent *api.Agent, state *corev1.ConfigMap, bindings []sandboxpolicy.BindingReference) error {
	submitted, err := submittedReconciledGrants(state, bindings)
	if err != nil || len(submitted) == 0 {
		return err
	}
	return r.sandboxGrantJournal().ConfirmBindingIntents(ctx, agent.Name, agent.UID, submitted)
}

func (r *AgentReconciler) recordReconciledGrantSubmission(ctx context.Context, agent *api.Agent, state *corev1.ConfigMap, requested sandboxpolicy.BindingReference) error {
	digests, _, err := reconciledGrantSubmissions(state)
	if err != nil {
		return err
	}
	if digests == nil {
		digests = map[string]bool{}
	}
	bindings, err := r.sandboxGrantJournal().Read(ctx, agent.Name, agent.UID)
	if err != nil {
		return err
	}
	if !slices.Contains(unconfirmedKubernetesGrants(bindings), requested) {
		return fmt.Errorf("Kubernetes grant creation requires its unconfirmed journal intent")
	}
	digest, err := reconciledGrantDigest(requested)
	if err != nil {
		return err
	}
	if digests[digest] {
		return fmt.Errorf("%w: Kubernetes grant creation is already submitted", sandboxpolicy.ErrRevocationPending)
	}
	digests[digest] = true
	return setReconciledGrantSubmissions(state, digests)
}

func setReconciledGrantSubmissions(state *corev1.ConfigMap, digests map[string]bool) error {
	if len(digests) > sandboxpolicy.MaxGrantJournalReferences {
		return fmt.Errorf("Kubernetes grant submission inventory exceeds its bound")
	}
	values := make([]string, 0, len(digests))
	for digest := range digests {
		values = append(values, digest)
	}
	slices.Sort(values)
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	state.Annotations[sandboxGrantSubmissionsAnnotation] = string(raw)
	return nil
}

func (r *AgentReconciler) rejectReconciledGrantSubmission(ctx context.Context, submitted *corev1.ConfigMap, requested sandboxpolicy.BindingReference) error {
	if submitted.UID == "" || submitted.ResourceVersion == "" {
		return fmt.Errorf("Kubernetes grant rejection requires its persisted submission identity")
	}
	digests, present, err := reconciledGrantSubmissions(submitted)
	if err != nil {
		return err
	}
	digest, err := reconciledGrantDigest(requested)
	if err != nil {
		return err
	}
	if !present || !digests[digest] {
		return fmt.Errorf("Kubernetes grant rejection has no matching submission")
	}
	delete(digests, digest)
	rejected := submitted.DeepCopy()
	if err := setReconciledGrantSubmissions(rejected, digests); err != nil {
		return err
	}
	// Fence this terminal outcome to the original submission, never a newer
	// claim or a same-name replacement expectation.
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": submitted.UID, "resourceVersion": submitted.ResourceVersion,
		"annotations": map[string]string{
			sandboxGrantSubmissionsAnnotation: rejected.Annotations[sandboxGrantSubmissionsAnnotation],
		},
	}})
	if err != nil {
		return err
	}
	return r.Patch(ctx, rejected, client.RawPatch(types.MergePatchType, patch))
}

func (r *AgentReconciler) prepareReconciledGrantRevocation(ctx context.Context, agent *api.Agent, bindings []sandboxpolicy.BindingReference) error {
	if len(unconfirmedKubernetesGrants(bindings)) == 0 {
		return nil
	}
	reconciled, err := r.recordedReconciledSandbox(ctx, agent)
	if err != nil || !reconciled {
		return err
	}
	pending := unconfirmedKubernetesGrants(bindings)
	confirmWithoutProof := func(proofErr error) error {
		if err := r.sandboxGrantJournal().ConfirmBindingIntents(ctx, agent.Name, agent.UID, pending); err != nil {
			return errors.Join(err, proofErr)
		}
		return nil
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		return confirmWithoutProof(err)
	}
	if !sandboxBuiltinOwned(namespace, agent) {
		return confirmWithoutProof(fmt.Errorf("Kubernetes grant cleanup requires the owned namespace"))
	}
	state := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(namespace.Name, namespace.UID),
	}, state); err != nil {
		return confirmWithoutProof(err)
	}
	expected, err := w.ReadReconcileExpectationForCleanup(state)
	if err != nil || expected.Agent != (w.ObjectIdentity{Name: agent.Name, UID: agent.UID}) ||
		expected.Namespace != (w.ObjectIdentity{Name: namespace.Name, UID: namespace.UID}) {
		return confirmWithoutProof(fmt.Errorf("Kubernetes grant cleanup requires its recorded lifetime"))
	}
	submitted, err := submittedReconciledGrants(state, pending)
	if err != nil {
		return err
	}
	if len(submitted) != 0 {
		if err := r.sandboxGrantJournal().ConfirmBindingIntents(ctx, agent.Name, agent.UID, submitted); err != nil {
			return err
		}
	}
	if len(submitted) == len(pending) {
		return nil
	}
	cancelled, err := r.cancelUnsubmittedReconciledApply(ctx, state, expected)
	if err != nil {
		return err
	}
	if !cancelled {
		return r.fenceReconciledGrantRevocation(ctx, state)
	}
	return nil
}

func (r *AgentReconciler) checkMissingReconciledGrantJournal(ctx context.Context, agent *api.Agent) error {
	recorded, err := r.recordedReconciledSandbox(ctx, agent)
	if err != nil || !recorded {
		return err
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		// Journal deletion follows confirmed grant revocation and namespace
		// absence. A later finalizer retry need not recreate an empty journal.
		return client.IgnoreNotFound(err)
	}
	state := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(namespace.Name, namespace.UID),
	}, state); err != nil {
		if apierrors.IsNotFound(err) && agent.Status.Sandbox == nil &&
			!isConditionTrue(agent.Status.Conditions, api.ConditionHelmDeployed) {
			return nil
		}
		return fmt.Errorf("common Sandbox cleanup authority is unavailable: %w", err)
	}
	expected, err := w.ReadReconcileExpectationForCleanup(state)
	if err != nil {
		return err
	}
	if state.Annotations[sandboxApplyStateAnnotation] == sandboxApplyInitializing &&
		!slices.ContainsFunc(expected.Workloads, func(workload w.RenderedWorkload) bool { return workload.Identity.UID != "" }) {
		return nil
	}
	return fmt.Errorf("recorded common Sandbox execution requires its missing grant journal; recovery is required")
}

func reconciledKubernetesRoleSources(plan *sandboxpolicy.Plan, platform sandboxpolicy.Platform) ([]w.SourceReference, error) {
	var sources []w.SourceReference
	for _, binding := range plan.Identity.Bindings {
		for _, approval := range platform.Grants {
			if approval.PolicyUID != plan.Binding.AgentPolicyUID || approval.Namespace != binding.Namespace ||
				approval.RoleRef != binding.RoleRef {
				continue
			}
			source := w.SourceReference{
				APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole", Name: approval.RoleRef.Name,
				UID: approval.RoleUID, ResourceVersion: approval.RoleResourceVersion,
			}
			index := slices.IndexFunc(sources, func(old w.SourceReference) bool { return w.SourceKey(old) == w.SourceKey(source) })
			if index >= 0 {
				if sources[index] != source {
					return nil, fmt.Errorf("Kubernetes role approvals have conflicting source pins")
				}
				continue
			}
			sources = append(sources, source)
		}
	}
	return sources, nil
}

func (r *AgentReconciler) validateReconciledKubernetesCA(ctx context.Context, source w.SourceReference) error {
	ca := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, ca); err != nil {
		return err
	}
	// Kubernetes publishes this one namespaced CA without an Agent owner.
	// Its UID and contents join the normal admission source inventory.
	if source.Name != "kube-root-ca.crt" || ca.UID == "" || ca.ResourceVersion == "" ||
		!ca.DeletionTimestamp.IsZero() || len(ca.OwnerReferences) != 0 || ca.Data["ca.crt"] == "" {
		return fmt.Errorf("Kubernetes API access requires the namespace's published root CA")
	}
	return nil
}

func (r *AgentReconciler) confirmReconciledKubernetesGrants(ctx context.Context, agent *api.Agent, prepared *helm.PreparedRelease) error {
	rendered, err := prepared.ReconcileExpectation()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(rendered.Controls, w.KubernetesBindingResource) {
		return nil
	}
	if agent.Spec.Suspended {
		return fmt.Errorf("suspended reconciliation cannot apply Kubernetes workload grants")
	}
	if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
		return err
	}
	state := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(rendered.Namespace.Name, rendered.Namespace.UID),
	}, state); err != nil {
		return err
	}
	expected, err := w.ReadReconcileExpectation(state)
	if err != nil || expected.Agent != rendered.Agent || expected.PlanHash != rendered.PlanHash ||
		expected.Revision != rendered.Revision || expected.AgentGeneration != agent.Generation {
		return fmt.Errorf("Kubernetes grants require the current reconciled expectation")
	}
	bindings, err := expected.KubernetesBindings()
	if err != nil {
		return err
	}
	confirmed, err := r.sandboxBindingUIDs(ctx, bindings)
	if err != nil {
		return err
	}
	journal := r.sandboxGrantJournal()
	if err := journal.Record(ctx, agent.Name, agent.UID, confirmed); err != nil {
		return err
	}
	if err := journal.VerifyKubernetesBindings(ctx, agent.Name, agent.UID, bindings); err != nil {
		return err
	}
	return prepared.AuthorizeSandboxApply(ctx)
}
