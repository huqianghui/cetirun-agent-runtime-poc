// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errSandboxIdentityPending = errors.New("dedicated ServiceAccount bootstrap is pending")

func (r *AgentReconciler) bootstrapPendingIdentity(ctx context.Context, agent *api.Agent, pending bool) error {
	if !pending || agent.Spec.Suspended {
		return nil
	}
	common, err := r.usesReconciledSandbox(ctx, agent)
	if err != nil || !common {
		return err
	}
	if agent.Spec.Helm.Version == "" || agent.Spec.Agent.Image == nil ||
		agent.Spec.Agent.Type != api.AgentTypeCustom && agent.Spec.Agent.Port == nil {
		attempt, err := r.resolveSandboxInputs(ctx, agent, false, true)
		if err != nil {
			return err
		}
		if _, err := r.bootstrapIdentityAccountName(attempt); err != nil {
			return err
		}
		before := agent.DeepCopy()
		if agent.Spec.Helm.Version == "" {
			if r.ChartVersion == "" {
				return fmt.Errorf("Entra bootstrap requires a configured runtime chart version")
			}
			agent.Spec.Helm.Version = r.ChartVersion
		} else {
			prepared, err := r.prepareAgent(agent, attempt.team)
			if err != nil {
				return fmt.Errorf("prepare Entra runtime defaults: %w", err)
			}
			if !pinAgentDefaults(&agent.Spec.Agent, prepared.Agent) {
				return fmt.Errorf("Entra runtime chart did not resolve the omitted image or port")
			}
		}
		// Persist only omitted coordinates; bootstrap observes the new generation.
		if err := r.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("pin Entra runtime defaults: %w", err)
		}
		return nil
	}
	return r.bootstrapReconciledSandbox(ctx, agent)
}

func (r *AgentReconciler) bootstrapIdentityAccountName(attempt *sandboxPreparationAttempt) (string, error) {
	if !attempt.resolution.AgentPolicy.EntraID.Enabled || !r.SandboxPreparation.NativeExecution {
		return "", nil
	}
	if !attempt.reconciled || r.SandboxNativeCoordination == nil {
		return "", fmt.Errorf("%w: Entra identity requires common execution and configuration coordination", sandboxpolicy.ErrUnavailable)
	}
	approval, err := sandboxpolicy.IdentityApproval(attempt.agent.UID, instanceNamespace(attempt.agent.Name),
		attempt.configuration.Platforms[attempt.resolution.Sandbox.Type].Identities)
	if err != nil {
		return "", err
	}
	if approval.Entra == nil {
		return "", fmt.Errorf("%w: native Entra identity requires an explicit approval", sandboxpolicy.ErrUnapprovedGrant)
	}
	if access := attempt.resolution.AgentPolicy.KubernetesAccess; access != nil &&
		(!approval.Kubernetes || access.ServiceAccountName != approval.ServiceAccountName) {
		return "", fmt.Errorf("%w: combined Kubernetes identity must use the same approved ServiceAccount", sandboxpolicy.ErrUnapprovedGrant)
	}
	return approval.ServiceAccountName, nil
}

func (r *AgentReconciler) sandboxIdentityPin(ctx context.Context, attempt *sandboxPreparationAttempt) (*sandboxpolicy.IdentityPin, error) {
	name, err := r.bootstrapIdentityAccountName(attempt)
	if err != nil {
		return nil, err
	}
	optional := false
	if name == "" {
		access := attempt.resolution.AgentPolicy.KubernetesAccess
		if access == nil || !r.SandboxPreparation.NativeExecution {
			return nil, nil
		}
		name, optional = access.ServiceAccountName, true
	}
	state, receipt, err := r.readSandboxBootstrap(ctx, attempt.agent)
	if err != nil {
		if optional && apierrors.IsNotFound(err) && !slices.Contains(attempt.agent.Finalizers, sandbox.BootstrapFinalizer) {
			return nil, nil
		}
		return nil, err
	}
	account := receipt.ServiceAccount
	if optional && account == nil {
		return nil, nil
	}
	if account == nil || account.Name != name || account.UID == "" || state.DeletionTimestamp != nil {
		return nil, fmt.Errorf("%w: bootstrap ServiceAccount identity is not durably recorded", sandboxpolicy.ErrUnavailable)
	}
	return &sandboxpolicy.IdentityPin{Name: name, UID: account.UID, BootstrapUID: state.UID}, nil
}

// Bootstrap is the only writer/owner of this retained ServiceAccount. Helm gets
// a read-only source; metadata changes require stopped workloads and UID/RV CAS.
func (r *AgentReconciler) syncSandboxIdentity(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	if !attempt.plan.Identity.BootstrapServiceAccount {
		return nil
	}
	desired := attempt.plan.Identity.ServiceAccount
	state, receipt, err := r.readSandboxBootstrap(ctx, attempt.agent)
	if err != nil {
		return err
	}
	pin := receipt.ServiceAccount
	if pin == nil || pin.UID != desired.UID || pin.Name != desired.Name ||
		string(state.UID) != desired.Annotations[sandboxBootstrapAnnotation] {
		return fmt.Errorf("Entra bootstrap pin changed before metadata reconciliation")
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: receipt.Namespace}, namespace); err != nil {
		return err
	}
	if err := r.validateSandboxBootstrapNamespace(ctx, attempt.agent, namespace); err != nil {
		return err
	}
	current := &corev1.ServiceAccount{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return err
	}
	base := r.sandboxBootstrapObjects(attempt.agent, state, receipt)[len(receipt.Objects)]
	if err := validateSandboxBootstrapObject(current, base, pin.UID, false); err != nil {
		return err
	}
	removedAnnotations := []string{}
	for key := range current.Annotations {
		if strings.HasPrefix(key, "azure.workload.identity/") {
			if _, approved := desired.Annotations[key]; !approved {
				if !attempt.resolution.AgentPolicy.EntraID.Enabled &&
					(key == "azure.workload.identity/client-id" || key == "azure.workload.identity/tenant-id") {
					removedAnnotations = append(removedAnnotations, key)
					continue
				}
				return fmt.Errorf("Entra ServiceAccount contains unapproved identity metadata")
			}
		}
	}
	changed := len(removedAnnotations) != 0
	for key, value := range desired.Annotations {
		changed = changed || current.Annotations[key] != value
	}
	for key, value := range desired.Labels {
		changed = changed || current.Labels[key] != value
	}
	if changed {
		quiescence := &KubernetesNativeQuiescence{APIReader: r.customReader(), Reconciled: true}
		if err := quiescence.CheckQuiesced(ctx, attempt.agent, attempt.prerequisites); err != nil {
			return errors.Join(err, r.holdReconciledPolicyChange(ctx, attempt.agent))
		}
		if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
			return err
		}
		annotations := make(map[string]any, len(desired.Annotations)+len(removedAnnotations))
		for key, value := range desired.Annotations {
			annotations[key] = value
		}
		for _, key := range removedAnnotations {
			annotations[key] = nil
		}
		labels := maps.Clone(current.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, desired.Labels)
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid": pin.UID, "resourceVersion": current.ResourceVersion, "annotations": annotations, "labels": labels,
		}})
		if err != nil {
			return err
		}
		if err := r.Patch(ctx, current, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return err
		}
	}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return err
	}
	expected := w.ReconcileExpectation{
		Version: w.ReconcileExpectationVersion, Agent: w.ObjectIdentity{Name: attempt.agent.Name, UID: attempt.agent.UID},
		Namespace: attempt.input.Namespace, Binding: attempt.resolution.Binding, Revision: attempt.plan.Revision,
		IdentityServiceAccount: desired,
	}
	if err := expected.ValidateIdentityBinding(namespace, current, w.ObjectIdentity{Name: pin.Name, UID: pin.UID}); err != nil {
		return err
	}
	// Skills publication may already pin this same bootstrap record.
	for _, source := range []w.SourceReference{
		sandboxSource(current, "v1", "ServiceAccount"), sandboxSource(state, "v1", "ConfigMap"),
	} {
		if !slices.Contains(attempt.sources, source) {
			attempt.sources = append(attempt.sources, source)
		}
	}
	return nil
}

func (r *AgentReconciler) verifyReconciledIdentity(ctx context.Context, expected w.ReconcileExpectation) error {
	if expected.IdentityServiceAccount == nil {
		return nil
	}
	namespace, account := &corev1.Namespace{}, &corev1.ServiceAccount{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: expected.Namespace.Name}, namespace); err != nil {
		return err
	}
	desired := expected.IdentityServiceAccount
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(desired), account); err != nil {
		return err
	}
	if err := expected.ValidateIdentityBinding(namespace, account, w.ObjectIdentity{Name: desired.Name, UID: desired.UID}); err != nil {
		return err
	}
	for _, source := range expected.Sources {
		if source.APIVersion == "v1" && source.Kind == "ServiceAccount" &&
			source.Name == desired.Name && source.Namespace == desired.Namespace && source.UID == desired.UID {
			digest, err := w.ExecutionObjectDigest(account)
			if err != nil || digest != expected.SourceDigests[w.SourceKey(source)] {
				return fmt.Errorf("current identity source metadata differs from the recorded digest")
			}
			return nil
		}
	}
	return fmt.Errorf("current identity has no recorded ServiceAccount source")
}
