// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SandboxExecutionStatusWriter is a status component for independently qualified
// runtime adapters, not an installed adapter or an execution authorization. Its
// APIReader must be uncached. Status CAS is not a transaction with coordination,
// route publication or physical writer exclusion.
type SandboxExecutionStatusWriter struct {
	Reconciler   *AgentReconciler
	Coordination sandboxcoordination.Store
	Now          func() time.Time
	MaxAge       time.Duration
}

// CommitReadiness records only verified Sandbox enforcement and applied history.
// It neither lifts holds nor publishes global readiness, PhaseReady or a URL.
func (w SandboxExecutionStatusWriter) CommitReadiness(ctx context.Context, ticket sandboxcoordination.Ticket,
	observation sandbox.Observation, guard func(context.Context) error,
) error {
	ticket = ticket.DeepCopy()
	if err := w.validate(ctx, guard); err != nil {
		return err
	}
	if w.MaxAge <= 0 || w.MaxAge > time.Minute {
		return fmt.Errorf("%w: observation freshness must be explicitly bounded by one minute", sandbox.ErrObservationUnavailable)
	}
	if ticket.Execution.Phase != sandboxcoordination.Running {
		return fmt.Errorf("%w: Sandbox status application requires a running ticket", sandboxcoordination.ErrConflict)
	}
	if err := w.checkTicket(ctx, ticket, guard); err != nil {
		return err
	}
	before, err := w.readAgent(ctx, ticket)
	if err != nil {
		return err
	}
	if before.Generation != ticket.Execution.Intent.AgentGeneration ||
		before.Spec.Suspended || !before.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: current Agent is not eligible for Sandbox status application", sandboxcoordination.ErrConflict)
	}
	if _, err := w.runningInput(ctx, ticket, observation); err != nil {
		return err
	}

	// Resolve again rather than reusing the first read's authorization or plan.
	input, err := w.runningInput(ctx, ticket, observation)
	if err != nil {
		return err
	}
	if err := w.checkAgentRevision(ctx, ticket, before); err != nil {
		return err
	}
	input.Now = w.Now()
	if err := sandbox.VerifyObservation(input.Expected, &observation, input.Now, w.MaxAge); err != nil {
		return err
	}
	after := before.DeepCopy()
	result, err := UpdateSandboxAgentStatus(after, input)
	if err != nil {
		return err
	}
	if !result.Verified || result.Hold {
		return fmt.Errorf("%w: Sandbox status is not verified", sandbox.ErrObservationUnavailable)
	}
	check := func(ctx context.Context) error {
		if err := w.checkTicket(ctx, ticket, guard); err != nil {
			return err
		}
		return sandbox.VerifyObservation(ticket.Execution.Intent.Expected, &observation, w.Now(), w.MaxAge)
	}
	return w.persistStatus(ctx, before, after, check)
}

// ClearReadiness records restrictive status for an exact stopping operation,
// even if its Agent generation or sources have changed. It does not stop or
// fence execution; the coordinator must still withdraw routes and quiesce writers.
func (w SandboxExecutionStatusWriter) ClearReadiness(ctx context.Context, ticket sandboxcoordination.Ticket,
	guard func(context.Context) error,
) error {
	ticket = ticket.DeepCopy()
	if err := w.validate(ctx, guard); err != nil {
		return err
	}
	if ticket.Execution.Phase != sandboxcoordination.Stopping {
		return fmt.Errorf("%w: clearing Sandbox status requires a stopping ticket", sandboxcoordination.ErrConflict)
	}
	if err := w.checkTicket(ctx, ticket, guard); err != nil {
		return err
	}
	before, err := w.readAgent(ctx, ticket)
	if err != nil {
		return err
	}
	if err := w.checkAgentRevision(ctx, ticket, before); err != nil {
		return err
	}
	now := w.Now()
	if now.IsZero() {
		return fmt.Errorf("%w: an explicit status clock is required", sandboxcoordination.ErrUnavailable)
	}
	after := before.DeepCopy()
	reason, message := "Stopping", "Sandbox execution cleanup is requested."
	after.Status.Phase = api.PhasePending
	switch {
	case !after.DeletionTimestamp.IsZero():
		reason, message, after.Status.Phase = "Deleting", "Agent deletion is in progress.", api.PhaseDeleting
	case after.Spec.Suspended:
		reason, message, after.Status.Phase = "Suspended", "Agent suspension is requested.", api.PhaseSuspended
	}
	// A new stopping operation must change the Agent RV even when readiness was
	// already false, invalidating a pending positive write using the old RV.
	marker := sandboxEventDigest("sandbox-status-stop-v1\x00" + string(ticket.AuthorityUID) + "\x00" + ticket.Execution.ID)
	message += " Route withdrawal and writer exclusion remain independent of status. Stop request sha256:" + marker + "."
	after.Status.AccessURL = ""
	after.Status.ObservedGeneration = after.Generation
	for _, condition := range []string{api.ConditionReady, api.ConditionSandboxReady} {
		apimeta.SetStatusCondition(&after.Status.Conditions, metav1.Condition{
			Type: condition, Status: metav1.ConditionFalse, Reason: reason, Message: message,
			ObservedGeneration: after.Generation, LastTransitionTime: metav1.NewTime(now),
		})
	}
	return w.persistStatus(ctx, before, after, func(ctx context.Context) error {
		return w.checkTicket(ctx, ticket, guard)
	})
}

func (w SandboxExecutionStatusWriter) validate(ctx context.Context, guard func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.Reconciler == nil || w.Reconciler.Client == nil || w.Reconciler.APIReader == nil ||
		w.Coordination.Client == nil || w.Now == nil || w.Now().IsZero() || guard == nil {
		return fmt.Errorf("%w: status and coordination writers, an uncached reader, explicit clock and guard are required", sandboxcoordination.ErrUnavailable)
	}
	return nil
}

func (w SandboxExecutionStatusWriter) checkTicket(ctx context.Context, ticket sandboxcoordination.Ticket,
	guard func(context.Context) error,
) error {
	if err := guard(ctx); err != nil {
		return fmt.Errorf("check Sandbox status guard: %w", err)
	}
	return w.Coordination.Check(ctx, ticket)
}

func (w SandboxExecutionStatusWriter) readAgent(ctx context.Context, ticket sandboxcoordination.Ticket) (*api.Agent, error) {
	agent := &api.Agent{}
	if err := w.Reconciler.APIReader.Get(ctx, client.ObjectKey{Name: ticket.Execution.Intent.Agent.Name}, agent); err != nil {
		return nil, fmt.Errorf("read Sandbox status Agent: %w", err)
	}
	if agent.Name != ticket.Execution.Intent.Agent.Name || agent.Namespace != "" ||
		agent.UID != ticket.Execution.Intent.Agent.UID || agent.UID == "" || agent.ResourceVersion == "" {
		return nil, fmt.Errorf("%w: Sandbox status Agent identity or resource version is unavailable", sandboxcoordination.ErrConflict)
	}
	return agent, nil
}

func (w SandboxExecutionStatusWriter) checkAgentRevision(ctx context.Context, ticket sandboxcoordination.Ticket, before *api.Agent) error {
	current, err := w.readAgent(ctx, ticket)
	if err != nil {
		return err
	}
	if current.ResourceVersion != before.ResourceVersion || current.Generation != before.Generation {
		return apierrors.NewConflict(api.GroupVersion.WithResource("agents").GroupResource(), before.Name,
			fmt.Errorf("Agent changed before Sandbox status could be committed"))
	}
	return nil
}

func (w SandboxExecutionStatusWriter) runningInput(ctx context.Context, ticket sandboxcoordination.Ticket,
	observation sandbox.Observation,
) (SandboxStatusInput, error) {
	input, err := w.Reconciler.SandboxRunningStatusInput(ctx, ticket.Execution.Intent)
	if err != nil {
		return SandboxStatusInput{}, err
	}
	if input.Expected != ticket.Execution.Intent.Expected || input.AgentGeneration != ticket.Execution.Intent.AgentGeneration {
		return SandboxStatusInput{}, fmt.Errorf("%w: running status inputs differ from the enrolled execution", sandbox.ErrObservationUnavailable)
	}
	input.Observation, input.Now, input.MaxAge = &observation, w.Now(), w.MaxAge
	if err := sandbox.VerifyObservation(input.Expected, input.Observation, input.Now, input.MaxAge); err != nil {
		return SandboxStatusInput{}, err
	}
	return input, nil
}

func (w SandboxExecutionStatusWriter) persistStatus(ctx context.Context,
	before, after *api.Agent, check func(context.Context) error,
) error {
	if reflect.DeepEqual(before.Status, after.Status) {
		return check(ctx)
	}
	delta, err := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}).Data(after)
	if err != nil {
		return fmt.Errorf("encode Sandbox status delta: %w", err)
	}
	// Preserve unknown status fields through a merge delta, but also pin the UID
	// explicitly so this patch cannot target a same-name replacement.
	var patch struct {
		Metadata struct {
			UID             types.UID `json:"uid"`
			ResourceVersion string    `json:"resourceVersion"`
		} `json:"metadata"`
		Status json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(delta, &patch); err != nil {
		return fmt.Errorf("decode Sandbox status delta: %w", err)
	}
	patch.Metadata.UID = before.UID
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encode UID-pinned Sandbox status delta: %w", err)
	}
	if err := check(ctx); err != nil {
		return err
	}
	if err := w.Reconciler.Status().Patch(ctx, after, client.RawPatch(types.MergePatchType, data)); err != nil {
		return fmt.Errorf("commit Sandbox status: %w", err)
	}
	if err := check(ctx); err != nil {
		return fmt.Errorf("Sandbox status was acknowledged but its post-write guard failed: %w", err)
	}
	emitSandboxAgentTransition(ctx, before, after)
	return nil
}
