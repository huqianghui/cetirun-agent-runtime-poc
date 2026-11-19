// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewNativeSandboxCoordination consumes the admission authority identity
// conveyed through source configuration. It neither installs state nor enrolls
// a qualified execution. Keep it configured after native execution is disabled
// so existing native reservations can finish ordinary cleanup.
func NewNativeSandboxCoordination(config SandboxSourcesConfig, writer client.Client, reader client.Reader, namespace string) (*sandboxcoordination.Store, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.CoordinationConfigMapName == "" {
		return nil, nil
	}
	if writer == nil || reader == nil || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, fmt.Errorf("native Sandbox coordination requires a writer, authoritative reader and operator namespace")
	}
	return &sandboxcoordination.Store{
		Client: writer, APIReader: reader, Namespace: namespace,
		Identity: w.ObjectIdentity{Name: config.CoordinationConfigMapName, UID: types.UID(config.CoordinationConfigMapUID)},
	}, nil
}

// Reserve after selected digests are captured, before the one-shot marker,
// hold removal, replica increase or admission release. Failures retain the
// reservation until the ordinary stop path has observed quiescence.
func (r *AgentReconciler) reserveNativeSandboxSources(ctx context.Context, agent *api.Agent, expected *w.Expectation) error {
	if r.SandboxNativeCoordination == nil {
		return nil
	}
	if r.SandboxPreparation == nil || !r.SandboxPreparation.NativeExecution || agent == nil || expected == nil ||
		!expected.ExecutionHeld || agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() ||
		expected.Agent != (w.ObjectIdentity{Name: agent.Name, UID: agent.UID}) || expected.AgentGeneration != agent.Generation ||
		expected.Namespace.Name != instanceNamespace(agent.Name) || expected.Workload.Name != instanceServiceName(agent.Name) {
		return fmt.Errorf("native Sandbox reservation requires the current held single-start lifetime")
	}
	intent, err := sandboxcoordination.NativeIntentFor(*expected)
	if err != nil {
		return err
	}
	if intent.SelectedSandbox() != agent.Spec.SandboxRef || intent.Sources[0].Name != agent.Spec.Team {
		return fmt.Errorf("native Sandbox reservation differs from the Agent's selected configuration")
	}
	if _, err := r.SandboxNativeCoordination.AcquireNative(ctx, intent); err != nil {
		return fmt.Errorf("reserve native Sandbox sources: %w", err)
	}
	return r.checkNativeSandboxReservation(ctx, expected)
}

// Recheck at startup/admission-release boundaries and before publishing native
// readiness. This never acquires or repairs missing/stale reservations.
func (r *AgentReconciler) checkNativeSandboxReservation(ctx context.Context, expected *w.Expectation) error {
	if r.SandboxNativeCoordination == nil {
		return nil
	}
	if expected == nil {
		return fmt.Errorf("native Sandbox expectation is required for coordination")
	}
	intent, err := sandboxcoordination.NativeIntentFor(*expected)
	if err != nil {
		return err
	}
	if err := r.SandboxNativeCoordination.AuthorizeNativeStart(ctx, intent); err != nil {
		return err
	}
	if r.APIReader == nil {
		return fmt.Errorf("native Sandbox coordination requires authoritative source reads")
	}
	for _, source := range intent.Sources {
		current, err := r.executionSourceDigest(ctx, source, intent.SelectedSandbox(), false)
		if err != nil {
			return err
		}
		if current != intent.SourceDigests[w.SourceKey(source)] {
			return fmt.Errorf("%w: selected native configuration changed: %s", errSandboxSourceChanged, w.SourceKey(source))
		}
	}
	return r.SandboxNativeCoordination.AuthorizeNativeStart(ctx, intent)
}

// Stop is independent of current Team/policy inputs and of native enablement.
// It disables further starts before checking ordinary Kubernetes quiescence;
// neither a zero-replica write nor entering Stopping is sufficient to release.
func (r *AgentReconciler) stopNativeSandboxReservation(ctx context.Context, agent *api.Agent) error {
	store := r.SandboxNativeCoordination
	if store == nil {
		return nil
	}
	if agent == nil || r.APIReader == nil {
		return fmt.Errorf("native Sandbox cleanup requires the Agent and authoritative reader")
	}
	ticket, found, err := store.ReadNative(ctx, agent.UID)
	if err != nil || !found {
		return err
	}
	intent := ticket.Reservation.Intent
	if intent.Agent != (w.ObjectIdentity{Name: agent.Name, UID: agent.UID}) ||
		intent.Namespace.Name != instanceNamespace(agent.Name) || intent.Workload.Name != instanceServiceName(agent.Name) {
		return fmt.Errorf("native Sandbox cleanup differs from its reserved lifetime")
	}
	ticket, err = store.BeginNativeStop(ctx, ticket)
	if err != nil {
		return err
	}
	current := &api.Agent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: agent.Name}, current); err != nil {
		return err
	}
	if current.UID != agent.UID || (current.DeletionTimestamp.IsZero() && !current.Spec.Suspended &&
		!slices.Contains(current.Finalizers, sandbox.HoldFinalizer)) {
		return fmt.Errorf("native Sandbox cleanup requires an observed hold, suspension or deletion")
	}
	if intent.Reconciled {
		return r.stopReconciledReservation(ctx, current, ticket)
	}
	_, expected, err := r.readNativeSandboxExpectation(ctx, current, true)
	if err != nil {
		return fmt.Errorf("read native admission outcome before releasing configuration: %w", err)
	}
	if expected.Agent != intent.Agent || expected.Namespace != intent.Namespace ||
		expected.Home != intent.Home || expected.Workload != intent.Workload || expected.Binding != intent.Binding {
		return fmt.Errorf("native admission outcome differs from the reserved lifetime")
	}
	// SinglePod is persisted with admission release and never cleared by a hold.
	// An empty API list cannot exclude a CREATE that already passed admission.
	if (expected.SinglePod && expected.PodUID == "") || (!expected.SinglePod && !expected.ExecutionHeld) {
		return fmt.Errorf("%w: native reservation waits for the submitted first Pod outcome", sandboxcoordination.ErrUnavailable)
	}
	workload := &appsv1.StatefulSet{}
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}, workload)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && workload.UID != intent.Workload.UID {
		return fmt.Errorf("native Sandbox workload differs from the reserved lifetime")
	}
	quiescence := &KubernetesNativeQuiescence{APIReader: r.APIReader}
	if err := quiescence.CheckQuiesced(ctx, current, SandboxPrerequisites{
		AgentUID: intent.Agent.UID, AgentGeneration: intent.AgentGeneration,
		NamespaceUID: intent.Namespace.UID, Home: intent.Home,
	}); err != nil {
		return fmt.Errorf("native Sandbox reservation waits for ordinary quiescence: %w", err)
	}
	return store.ReleaseNative(ctx, ticket)
}
