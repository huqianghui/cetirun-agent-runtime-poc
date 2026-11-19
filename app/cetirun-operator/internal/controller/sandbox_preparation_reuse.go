// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	strictjson "sigs.k8s.io/json"
)

const sandboxPreparationDigestAnnotation = "cetirun.microsoft.com/sandbox-preparation-digest"

func (r *AgentReconciler) sandboxPreparationReuseDigest(ctx context.Context, attempt *sandboxPreparationAttempt,
	expected sandboxworkload.Expectation, bindings []sandboxpolicy.BindingReference, stageDigest string,
) (string, error) {
	if stageDigest == "" {
		return "", fmt.Errorf("Sandbox held release identity is unavailable")
	}
	// The native Service is not a Pod input, but its exact staged identity must
	// also remain unchanged. No extra admission permission is needed to read it.
	service, err := r.readSandboxSource(ctx, sandboxworkload.SourceReference{
		APIVersion: "v1", Kind: "Service", Namespace: attempt.input.Namespace.Name, Name: attempt.input.WorkloadName,
	})
	if err != nil {
		return "", err
	}
	snapshot, prerequisites := attempt.configuration, attempt.prerequisites
	snapshot.ObservedAt, prerequisites.ObservedAt = time.Time{}, time.Time{}
	inventory, err := sandboxConfirmedBindingInventory(bindings)
	if err != nil {
		return "", err
	}
	return sandboxPreparationDigest(struct {
		Version                 string
		Configuration           SandboxConfigurationSnapshot
		Prerequisites           SandboxPrerequisites
		Resolution              *sandbox.Resolution
		Inputs                  []sandboxworkload.SourceReference
		Platform, User          map[string]any
		StateNamespace, Account string
		Bindings                map[string]sandboxpolicy.BindingReference
		Expected                sandboxworkload.Expectation
		Service                 sandboxworkload.SourceReference
		StageDigest             string
	}{
		"held-reuse-v1", snapshot, prerequisites, attempt.resolution, attempt.sources,
		attempt.platformValues, attempt.userValues, r.OperatorNamespace, r.SandboxPreparation.AdmissionServiceAccount,
		inventory, expected, service, stageDigest,
	})
}

func sandboxConfirmedBindingInventory(bindings []sandboxpolicy.BindingReference) (map[string]sandboxpolicy.BindingReference, error) {
	inventory := make(map[string]sandboxpolicy.BindingReference, len(bindings))
	for _, binding := range bindings {
		key := binding.Kind + "/" + binding.Namespace + "/" + binding.Name
		if _, duplicate := inventory[key]; duplicate || binding.UID == "" {
			return nil, fmt.Errorf("Sandbox preparation requires unique, confirmed grant identities")
		}
		inventory[key] = binding
	}
	return inventory, nil
}

// reuseSandboxPreparation is read-only. It skips rendering and all write paths
// only after validating durable state and fresh dependencies; it never skips a
// guard around a mutation or turns a prior successful check into a start permit.
func (r *AgentReconciler) reuseSandboxPreparation(ctx context.Context, attempt *sandboxPreparationAttempt) (bool, error) {
	agent := attempt.agent
	condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionAgentConfigReady)
	if !controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) ||
		condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "SandboxPrepared" ||
		condition.ObservedGeneration != agent.Generation || agent.Status.Phase != api.PhasePending ||
		agent.Status.AccessURL != "" {
		return false, nil
	}
	key := client.ObjectKey{
		Namespace: r.OperatorNamespace,
		Name:      sandboxworkload.PodExpectationName(attempt.input.Namespace.Name, attempt.input.Namespace.UID),
	}
	state := &corev1.ConfigMap{}
	if err := r.APIReader.Get(ctx, key, state); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	owner := []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}
	if state.UID == "" || state.ResourceVersion == "" || !state.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(state.OwnerReferences, owner) ||
		state.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID) {
		return false, fmt.Errorf("Sandbox preparation state ownership is unavailable or changed")
	}
	digest := state.Annotations[sandboxPreparationDigestAnnotation]
	if digest == "" {
		return false, nil
	}
	raw := state.Data[sandboxworkload.PodExpectationKey]
	var expected sandboxworkload.Expectation
	if len(raw) == 0 || len(raw) > 512*1024 || len(state.Data) != 1 || len(state.BinaryData) != 0 {
		return false, fmt.Errorf("Sandbox preparation state is malformed or exceeds its bound")
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &expected)
	if err != nil || len(problems) != 0 {
		return false, fmt.Errorf("Sandbox preparation state cannot be decoded")
	}
	if err := expected.ValidateSkills(); err != nil {
		return false, fmt.Errorf("Sandbox preparation skills source is invalid: %w", err)
	}
	if expected.Version != sandboxworkload.ExpectationVersion || !expected.ExecutionHeld ||
		expected.Agent.Name != agent.Name || expected.Agent.UID != agent.UID || expected.AgentGeneration != agent.Generation ||
		expected.Binding != attempt.resolution.Binding || expected.Namespace != attempt.input.Namespace ||
		expected.SkillsMode != attempt.prerequisites.SkillsMode || expected.Skills != attempt.prerequisites.Skills ||
		expected.Workload.Name != attempt.input.WorkloadName || expected.WorkloadKind != sandboxworkload.AgentWorkload ||
		expected.Revision == "" || state.Annotations[sandboxworkload.RevisionAnnotation] != expected.Revision {
		return false, nil
	}
	if !equality.Semantic.DeepEqual(expected.Placement, attempt.prerequisites.Placement) ||
		(r.SandboxPreparation.Placement != nil && expected.Placement == nil) {
		return false, nil
	}
	journalPinned, revisionPinned := false, expected.ControllerRevision == ""
	for _, source := range expected.Sources {
		journalPinned = journalPinned || (source.APIVersion == "v1" && source.Kind == "ConfigMap" &&
			source.Namespace == r.OperatorNamespace && source.Name == sandboxpolicy.GrantJournalName(agent.UID))
		revisionPinned = revisionPinned || (source.APIVersion == "apps/v1" && source.Kind == "ControllerRevision" &&
			source.Namespace == expected.Namespace.Name && source.Name == expected.ControllerRevision)
	}
	if !journalPinned || !revisionPinned {
		return false, nil
	}
	if expected.Placement != nil {
		placementPinned, receiptPinned := false, false
		for _, source := range expected.Sources {
			if source.APIVersion != "v1" || source.Kind != "ConfigMap" || source.Namespace != r.OperatorNamespace {
				continue
			}
			placementPinned = placementPinned || (source.Name == expected.Placement.Registry.Name && source.UID == expected.Placement.Registry.UID)
			receiptPinned = receiptPinned || source.Name == r.sandboxPreparationKey(agent).Name
		}
		if !placementPinned || !receiptPinned {
			return false, nil
		}
	}
	receiptObject, receipt, err := r.readSandboxPreparationReceipt(ctx, agent)
	if err != nil {
		return false, fmt.Errorf("read Sandbox preparation reuse receipt: %w", err)
	}
	if !receiptObject.DeletionTimestamp.IsZero() ||
		!controllerutil.ContainsFinalizer(receiptObject, sandbox.PreparationFinalizer) {
		return false, fmt.Errorf("Sandbox preparation reuse receipt is not protected")
	}
	previous, current := receipt.Prerequisites, attempt.prerequisites
	previous.ObservedAt, current.ObservedAt = time.Time{}, time.Time{}
	previousKey, err := sandboxPreparationDigest(previous)
	if err != nil {
		return false, err
	}
	currentKey, err := sandboxPreparationDigest(current)
	if err != nil {
		return false, err
	}
	if previousKey != currentKey {
		return false, nil
	}
	bindings, err := r.sandboxGrantJournal().Read(ctx, agent.Name, agent.UID)
	if err != nil {
		return false, fmt.Errorf("read Sandbox preparation reuse inventory: %w", err)
	}
	stageDigest, err := r.SandboxPreparation.Installer.SandboxStageDigest(ctx, attempt.input.Namespace.Name, attempt.input.ReleaseName)
	if err != nil {
		return false, err
	}
	currentDigest, err := r.sandboxPreparationReuseDigest(ctx, attempt, expected, bindings, stageDigest)
	if err != nil {
		if errors.Is(err, errSandboxSourceChanged) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if currentDigest != digest {
		return false, nil
	}
	if err := r.verifySandboxSources(ctx, expected.Sources); err != nil {
		if errors.Is(err, errSandboxSourceChanged) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := r.checkSandboxPreparationQuiesced(ctx, attempt); err != nil {
		return false, err
	}
	currentState := &corev1.ConfigMap{}
	if err := r.APIReader.Get(ctx, key, currentState); err != nil {
		return false, err
	}
	for _, observed := range []time.Time{attempt.configuration.ObservedAt, attempt.prerequisites.ObservedAt} {
		if err := configurationSnapshotTime(observed, configurationNow(r.SandboxPreparation.Now), r.SandboxPreparation.MaxSnapshotAge); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return currentState.UID == state.UID && currentState.ResourceVersion == state.ResourceVersion, nil
}
