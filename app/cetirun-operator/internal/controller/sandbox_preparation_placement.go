// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func copySandboxPrerequisites(prerequisites SandboxPrerequisites) SandboxPrerequisites {
	prerequisites.PublicationSources = slices.Clone(prerequisites.PublicationSources)
	prerequisites.Placement = prerequisites.Placement.DeepCopy()
	return prerequisites
}

func validateSandboxPlacementTuple(agent *api.Agent, prerequisites SandboxPrerequisites) error {
	binding := prerequisites.Placement
	if binding == nil {
		return nil
	}
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("Sandbox preparation placement is invalid: %w", err)
	}
	if binding.Agent != (sandboxworkload.ObjectIdentity{Name: agent.Name, UID: agent.UID}) ||
		binding.Namespace != (sandboxworkload.ObjectIdentity{Name: instanceNamespace(agent.Name), UID: prerequisites.NamespaceUID}) ||
		binding.Home != prerequisites.Home || binding.Workload.Name != instanceServiceName(agent.Name) {
		return fmt.Errorf("Sandbox placement does not identify the durable Agent, namespace, HOME and native workload")
	}
	return nil
}

func (r *AgentReconciler) inheritSandboxPlacement(ctx context.Context, agent *api.Agent, prerequisites *SandboxPrerequisites) error {
	if r.SandboxPreparation.Placement == nil && prerequisites.Placement == nil &&
		!controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		return nil
	}
	_, receipt, err := r.readSandboxPreparationReceipt(ctx, agent)
	if apierrors.IsNotFound(err) && !controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) && prerequisites.Placement == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Sandbox placement ownership receipt is unavailable: %w", err)
	}
	recorded := receipt.Prerequisites.Placement
	if prerequisites.Placement != nil && !equality.Semantic.DeepEqual(prerequisites.Placement, recorded) {
		return fmt.Errorf("Sandbox prerequisite source cannot replace the recorded lifetime placement")
	}
	if recorded != nil && r.SandboxPreparation.Placement == nil {
		return fmt.Errorf("Sandbox recorded placement requires its configured reader; execution remains held")
	}
	prerequisites.Placement = recorded.DeepCopy()
	return validateSandboxPlacementTuple(agent, *prerequisites)
}

func (r *AgentReconciler) verifySandboxPlacement(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	binding := attempt.prerequisites.Placement
	if binding == nil {
		return nil
	}
	if r.SandboxPreparation.Placement == nil {
		return fmt.Errorf("Sandbox recorded placement requires its configured reader")
	}
	current, _, err := r.SandboxPreparation.Placement.ReadAssignment(ctx, sandboxworkload.Expectation{
		Agent: binding.Agent, Namespace: binding.Namespace, Workload: binding.Workload, Home: binding.Home,
	})
	if err != nil {
		return fmt.Errorf("recheck Sandbox placement: %w", err)
	}
	if !equality.Semantic.DeepEqual(current, binding) {
		return fmt.Errorf("Sandbox lifetime placement changed; a separately qualified handoff is required")
	}
	return nil
}

func (r *AgentReconciler) captureSandboxPlacement(ctx context.Context, attempt *sandboxPreparationAttempt,
	workload *appsv1.StatefulSet, template *corev1.PodTemplateSpec,
) ([]sandboxworkload.SourceReference, error) {
	if r.SandboxPreparation.Placement == nil {
		if attempt.prerequisites.Placement != nil {
			return nil, fmt.Errorf("Sandbox lifetime placement cannot be discarded by disabling its reader")
		}
		return nil, nil
	}
	placement, registry, err := r.SandboxPreparation.Placement.ReadAssignment(ctx, sandboxworkload.Expectation{
		Agent:     sandboxworkload.ObjectIdentity{Name: attempt.agent.Name, UID: attempt.agent.UID},
		Namespace: attempt.input.Namespace,
		Workload:  sandboxworkload.ObjectIdentity{Name: workload.Name, UID: workload.UID},
		Home:      attempt.prerequisites.Home, Template: *template.DeepCopy(),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve Sandbox placement before expectation publication: %w", err)
	}
	object, receipt, err := r.readSandboxPreparationReceipt(ctx, attempt.agent)
	if err != nil {
		return nil, fmt.Errorf("read Sandbox placement ownership receipt: %w", err)
	}
	if !object.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(object, sandbox.PreparationFinalizer) ||
		!controllerutil.ContainsFinalizer(attempt.agent, sandbox.PreparationFinalizer) ||
		!controllerutil.ContainsFinalizer(attempt.agent, sandbox.HoldFinalizer) ||
		!equality.Semantic.DeepEqual(receipt.Prerequisites.Placement, attempt.prerequisites.Placement) ||
		receipt.Prerequisites.NamespaceUID != placement.Namespace.UID || receipt.Prerequisites.Home != placement.Home {
		return nil, fmt.Errorf("Sandbox placement requires the exact protected held preparation receipt")
	}
	if receipt.Prerequisites.Placement != nil && !equality.Semantic.DeepEqual(receipt.Prerequisites.Placement, placement) {
		return nil, fmt.Errorf("Sandbox lifetime placement cannot be silently reassigned")
	}
	if receipt.Prerequisites.Placement == nil {
		receipt.Version, receipt.Prerequisites.Placement = "v2", placement.DeepCopy()
		raw, err := json.Marshal(receipt)
		if err != nil || len(raw) > 64*1024 {
			return nil, fmt.Errorf("Sandbox placement ownership receipt cannot be encoded within its bound")
		}
		base := object.DeepCopy()
		object.Data[sandboxPreparationReceiptKey] = string(raw)
		if err := r.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, fmt.Errorf("persist Sandbox lifetime placement: %w", err)
		}
	}
	attempt.prerequisites.Placement = placement.DeepCopy()
	return []sandboxworkload.SourceReference{
		registry, sandboxSource(object, "v1", "ConfigMap"),
	}, nil
}
