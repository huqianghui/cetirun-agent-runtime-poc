// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	strictjson "sigs.k8s.io/json"
)

// SandboxRunningStatusInput rechecks a completed Kubernetes handoff without
// re-running held preparation. It is read-only and supplies no runtime proof,
// start permit, observation or clock. A qualified caller must also check its
// current coordination ticket and independent guest-external evidence.
func (r *AgentReconciler) SandboxRunningStatusInput(ctx context.Context, intent sandboxcoordination.Intent) (SandboxStatusInput, error) {
	var empty SandboxStatusInput
	if r == nil {
		return empty, fmt.Errorf("%w: a configured reconciler is required", sandbox.ErrObservationUnavailable)
	}
	attempt, err := r.checkSandboxExecutionSources(ctx, intent, true)
	if err != nil {
		return empty, err
	}
	expected, err := r.readSandboxRunningExpectation(ctx, intent)
	if err != nil {
		return empty, err
	}
	if err := r.verifySandboxRunningPod(ctx, intent, *expected); err != nil {
		return empty, err
	}
	if err := r.verifySandboxExecutionSources(ctx, intent, true); err != nil {
		return empty, err
	}
	expected, err = r.readSandboxRunningExpectation(ctx, intent)
	if err != nil {
		return empty, err
	}
	if err := r.verifySandboxRunningPod(ctx, intent, *expected); err != nil {
		return empty, err
	}
	for _, observed := range []time.Time{attempt.configuration.ObservedAt, attempt.prerequisites.ObservedAt} {
		if err := configurationSnapshotTime(observed, configurationNow(r.SandboxPreparation.Now),
			r.SandboxPreparation.MaxSnapshotAge); err != nil {
			return empty, err
		}
	}
	return SandboxStatusInput{
		AgentGeneration: attempt.agent.Generation, Resolution: attempt.resolution,
		Plan: attempt.plan, Expected: intent.Expected,
	}, ctx.Err()
}

func (r *AgentReconciler) verifySandboxRunningPod(ctx context.Context, intent sandboxcoordination.Intent, expected w.Expectation) error {
	pod := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Writer.Pod.Name}, pod); err != nil {
		return err
	}
	if pod.UID != intent.Expected.PodUID || pod.ResourceVersion == "" || !pod.DeletionTimestamp.IsZero() ||
		pod.Spec.NodeName != intent.Writer.Node.Name {
		return fmt.Errorf("%w: running Pod differs from the enrolled identity or Node", errSandboxSourceChanged)
	}
	node := &corev1.Node{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: intent.Writer.Node.Name}, node); err != nil {
		return err
	}
	if node.UID != intent.Expected.NodeUID || node.Spec.ProviderID != intent.Writer.Node.ProviderID ||
		node.ResourceVersion == "" || !node.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: running Node differs from lifetime placement", errSandboxSourceChanged)
	}
	if err := r.SandboxPreparation.Placement.CheckRunningPod(ctx, pod, expected); err != nil {
		return err
	}
	return ctx.Err()
}

func (r *AgentReconciler) verifyRunningAgentSource(ctx context.Context, source w.SourceReference) error {
	_, err := r.readRunningAgentSource(ctx, source)
	return err
}

func (r *AgentReconciler) readRunningAgentSource(ctx context.Context, source w.SourceReference) (w.SourceReference, error) {
	agent := &api.Agent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: source.Name}, agent); err != nil {
		return w.SourceReference{}, err
	}
	if controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) || agent.Spec.Suspended ||
		!agent.DeletionTimestamp.IsZero() {
		return w.SourceReference{}, fmt.Errorf("%w: running source is held, suspended or deleting", errSandboxSourceChanged)
	}
	normalized := agent.DeepCopy()
	controllerutil.AddFinalizer(normalized, sandbox.HoldFinalizer)
	current, err := w.AgentSourceReference(normalized)
	if err != nil {
		return w.SourceReference{}, err
	}
	if current != source {
		return w.SourceReference{}, fmt.Errorf("%w: running Agent inputs differ beyond the hold transition", errSandboxSourceChanged)
	}
	return w.AgentSourceReference(agent)
}

func (r *AgentReconciler) readSandboxRunningExpectation(ctx context.Context, intent sandboxcoordination.Intent) (*w.Expectation, error) {
	state := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: intent.Preparation.Namespace, Name: intent.Preparation.Name}
	if err := r.APIReader.Get(ctx, key, state); err != nil {
		return nil, err
	}
	if state.UID != intent.Preparation.UID || state.ResourceVersion == "" || !state.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("%w: running preparation identity changed", errSandboxSourceChanged)
	}
	expected, err := decodeSandboxExecutionExpectation(state)
	if err != nil {
		return nil, err
	}
	if expected.ExecutionHeld || expected.Agent != intent.Agent || expected.AgentGeneration != intent.AgentGeneration ||
		expected.Namespace != intent.Namespace || expected.Home != intent.Home || expected.Workload != intent.Workload ||
		expected.Binding != intent.Expected.Binding || expected.Revision != intent.Expected.Revision {
		return nil, fmt.Errorf("%w: expectation does not describe the unheld execution", errSandboxSourceChanged)
	}
	digest, err := sandboxRunningPreparationDigest(state)
	if err != nil {
		return nil, err
	}
	if digest != intent.RunningPreparationDigest {
		return nil, fmt.Errorf("%w: immutable running preparation inputs changed", errSandboxSourceChanged)
	}
	var agentSource w.SourceReference
	for _, source := range intent.Sources {
		if source.IsAgent() {
			agentSource = source
		}
	}
	if !agentSource.ValidIdentity() {
		return nil, fmt.Errorf("%w: running intent has no Agent source", errSandboxSourceChanged)
	}
	currentAgent, err := r.readRunningAgentSource(ctx, agentSource)
	if err != nil {
		return nil, err
	}
	agents := 0
	for _, source := range expected.Sources {
		if source.IsAgent() {
			agents++
			if source != currentAgent {
				return nil, fmt.Errorf("%w: running expectation has a stale Agent intent", errSandboxSourceChanged)
			}
		}
	}
	if agents != 1 {
		return nil, fmt.Errorf("%w: running expectation must pin one current Agent intent", errSandboxSourceChanged)
	}
	return expected, ctx.Err()
}

func decodeSandboxExecutionExpectation(state *corev1.ConfigMap) (*w.Expectation, error) {
	if state == nil || len(state.Data) != 1 || len(state.BinaryData) != 0 {
		return nil, fmt.Errorf("%w: an exact preparation envelope is required", errSandboxSourceChanged)
	}
	raw := state.Data[w.PodExpectationKey]
	if len(raw) == 0 || len(raw) > 512*1024 {
		return nil, fmt.Errorf("%w: preparation exceeds its content bound", errSandboxSourceChanged)
	}
	var expected w.Expectation
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &expected)
	if err != nil || len(problems) != 0 || expected.Version != w.ExpectationVersion ||
		expected.WorkloadKind != w.AgentWorkload || len(expected.Sources) == 0 || len(expected.Sources) > w.MaxPreparationSources {
		return nil, fmt.Errorf("%w: malformed or unsupported preparation envelope", errSandboxSourceChanged)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, err
	}
	var held *bool
	if err := json.Unmarshal(fields["executionHeld"], &held); err != nil || held == nil {
		return nil, fmt.Errorf("%w: an explicit execution hold boolean is required", errSandboxSourceChanged)
	}
	if err := expected.ValidateSkills(); err != nil {
		return nil, fmt.Errorf("%w: invalid preparation skills source: %w", errSandboxSourceChanged, err)
	}
	return &expected, nil
}

// Capture this only from verified held preparation. Resource versions may be
// refreshed where execution independently pins effective content; Agent intent
// is rechecked separately, including its one permitted hold-bit transition.
func sandboxRunningPreparationDigest(state *corev1.ConfigMap) (string, error) {
	expected, err := decodeSandboxExecutionExpectation(state)
	if err != nil {
		return "", err
	}
	expected.ExecutionHeld = true
	for i := range expected.Sources {
		source := &expected.Sources[i]
		if !source.ValidIdentity() {
			return "", fmt.Errorf("%w: preparation source identity is invalid", errSandboxSourceChanged)
		}
		if source.IsAgent() {
			source.IntentDigest = ""
		}
		if w.EffectiveSource(*source) || (source.APIVersion == "apps/v1" && source.Kind == "StatefulSet" &&
			source.Namespace == expected.Namespace.Name && source.Name == expected.Workload.Name && source.UID == expected.Workload.UID) {
			source.ResourceVersion = ""
		}
	}
	raw, err := json.Marshal(expected)
	if err != nil {
		return "", err
	}
	normalized := state.DeepCopy()
	normalized.Data[w.PodExpectationKey] = string(raw)
	return w.ExecutionObjectDigest(normalized)
}
