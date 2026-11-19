// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type executionStatusFixture struct {
	*preparationFixture
	writer      controller.SandboxExecutionStatusWriter
	ticket      sandboxcoordination.Ticket
	observation sandbox.Observation
}

func newExecutionStatusFixture(t *testing.T, typ string, configure ...func(*preparationFixture)) *executionStatusFixture {
	t.Helper()
	f, _, intent := runningExecutionFixture(t, typ, configure...)
	state, err := sandboxcoordination.NewState("cetirun-system", "status-coordination")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.client.Create(ctx, state); err != nil {
		t.Fatal(err)
	}
	store := sandboxcoordination.Store{Client: f.client, APIReader: f.client, Namespace: state.Namespace,
		Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID}}
	ticket, err := store.AcquireExecution(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = store.BeginStart(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = store.MarkRunning(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	return &executionStatusFixture{
		preparationFixture: f, ticket: ticket,
		writer: controller.SandboxExecutionStatusWriter{
			Reconciler: f.reconciler, Coordination: store, Now: func() time.Time { return f.now }, MaxAge: 15 * time.Second,
		},
		observation: sandbox.Observation{ExecutionIdentity: intent.Expected, ObservedAt: f.now,
			RuntimeVerified: true, PodVerified: true, ControlsVerified: true},
	}
}

func statusTestGuard(ctx context.Context) error { return ctx.Err() }

func (f *executionStatusFixture) stopping(t *testing.T) sandboxcoordination.Ticket {
	t.Helper()
	ticket, err := f.writer.Coordination.BeginStop(context.Background(), f.ticket)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func (f *executionStatusFixture) historicalStatus(t *testing.T, ready bool) *api.Agent {
	t.Helper()
	agent := f.current(t)
	agent.Status.Sandbox = &api.SandboxStatus{
		TeamUID: "historical-team", SandboxUID: "historical-sandbox", AgentPolicyUID: "historical-policy",
		AppliedRevision: "historical-applied-revision",
	}
	agent.Status.ObservedGeneration = agent.Generation - 1
	for _, kind := range []string{api.ConditionReady, api.ConditionSandboxReady} {
		status := metav1.ConditionFalse
		if ready {
			status = metav1.ConditionTrue
		}
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: kind, Status: status, Reason: "HistoricalObservation", Message: "Synthetic previous observation.",
			ObservedGeneration: agent.Generation - 1, LastTransitionTime: metav1.NewTime(f.now.Add(-time.Minute)),
		})
	}
	if ready {
		agent.Status.Phase, agent.Status.AccessURL = api.PhaseReady, "https://agent.example.com/"
	}
	if err := f.client.Status().Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestSandboxExecutionStatusWriterAppliesOnlyVerifiedSandboxStatus(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f := newExecutionStatusFixture(t, typ)
			before := f.historicalStatus(t, false)
			expected, stages := f.expectation(t), f.installer.stages
			ctx, capture := capturePolicyEvents(t)
			writes := 0
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption,
				) error {
					writes++
					candidate, ok := object.(*api.Agent)
					if !ok || subresource != "status" || len(capture.events) != 0 {
						t.Fatal("status application wrote another resource or emitted before acknowledgement")
					}
					assertAppliedExecutionStatus(t, candidate, before, f.ticket.Execution.Intent.Expected)
					raw, err := patch.Data(object)
					if err != nil {
						t.Fatal(err)
					}
					var delta struct {
						Metadata metav1.ObjectMeta `json:"metadata"`
						Status   api.AgentStatus   `json:"status"`
					}
					if err := json.Unmarshal(raw, &delta); err != nil {
						t.Fatal(err)
					}
					if delta.Metadata.UID != before.UID || delta.Metadata.ResourceVersion != before.ResourceVersion ||
						delta.Status.Sandbox == nil || delta.Status.Sandbox.AppliedRevision != f.observation.Revision ||
						!apimeta.IsStatusConditionTrue(delta.Status.Conditions, api.ConditionSandboxReady) ||
						delta.Status.ObservedGeneration != before.Generation {
						t.Fatal("history, condition and observed generation were not in one identity-pinned CAS")
					}
					if !reflect.DeepEqual(f.current(t).Status, before.Status) {
						t.Fatal("status changed before the single atomic patch")
					}
					return c.Status().Patch(ctx, object, patch, opts...)
				},
			})
			if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
				t.Fatal(err)
			}
			after := f.current(t)
			assertAppliedExecutionStatus(t, after, before, f.observation.ExecutionIdentity)
			if writes != 1 || after.ResourceVersion == before.ResourceVersion || len(capture.events) != 1 ||
				capture.events[0]["event"] != "sandbox.policy.applied" {
				t.Fatalf("one acknowledged transition was not persisted: writes=%d events=%d", writes, len(capture.events))
			}
			if !reflect.DeepEqual(expected, f.expectation(t)) || !reflect.DeepEqual(before.Spec, after.Spec) ||
				!reflect.DeepEqual(before.Annotations, after.Annotations) || !reflect.DeepEqual(before.Finalizers, after.Finalizers) ||
				f.installer.stages != stages {
				t.Fatal("status persistence modified a hold, source, workload or Agent spec")
			}
			for range 3 {
				f.now = f.now.Add(time.Second)
				if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
					t.Fatal(err)
				}
			}
			if writes != 1 || len(capture.events) != 1 || !reflect.DeepEqual(after, f.current(t)) {
				t.Fatal("unchanged enforcement rewrote status or duplicated the trusted event")
			}
		})
	}
}

func assertAppliedExecutionStatus(t *testing.T, after, before *api.Agent, expected sandbox.ExecutionIdentity) {
	t.Helper()
	history := &api.SandboxStatus{TeamUID: expected.Binding.TeamUID, SandboxUID: expected.Binding.SandboxUID,
		AgentPolicyUID: expected.Binding.AgentPolicyUID, AppliedRevision: expected.Revision}
	if !reflect.DeepEqual(after.Status.Sandbox, history) || after.Status.ObservedGeneration != after.Generation ||
		!apimeta.IsStatusConditionTrue(after.Status.Conditions, api.ConditionSandboxReady) ||
		!reflect.DeepEqual(apimeta.FindStatusCondition(before.Status.Conditions, api.ConditionReady),
			apimeta.FindStatusCondition(after.Status.Conditions, api.ConditionReady)) ||
		after.Status.Phase != before.Status.Phase || after.Status.AccessURL != "" {
		t.Fatalf("Sandbox-only status application changed global readiness or failed atomic history: %+v", after.Status)
	}
	condition := apimeta.FindStatusCondition(after.Status.Conditions, api.ConditionSandboxReady)
	if condition.Reason != "PolicyApplied" || condition.ObservedGeneration != after.Generation {
		t.Fatal("Sandbox condition does not describe the current applied revision")
	}
}

func TestSandboxExecutionStatusWriterRejectsUnverifiedObservations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sandbox.Observation, time.Time)
	}{
		{"absent", func(o *sandbox.Observation, _ time.Time) { *o = sandbox.Observation{} }},
		{"missing timestamp", func(o *sandbox.Observation, _ time.Time) { o.ObservedAt = time.Time{} }},
		{"expired", func(o *sandbox.Observation, now time.Time) { o.ObservedAt = now.Add(-16 * time.Second) }},
		{"future", func(o *sandbox.Observation, now time.Time) { o.ObservedAt = now.Add(time.Second) }},
		{"Agent", func(o *sandbox.Observation, _ time.Time) { o.Binding.AgentUID = "other-agent" }},
		{"Team", func(o *sandbox.Observation, _ time.Time) { o.Binding.TeamUID = "other-team" }},
		{"Sandbox", func(o *sandbox.Observation, _ time.Time) { o.Binding.SandboxUID = "other-sandbox" }},
		{"policy", func(o *sandbox.Observation, _ time.Time) { o.Binding.AgentPolicyUID = "other-policy" }},
		{"revision", func(o *sandbox.Observation, _ time.Time) { o.Revision = "other-revision" }},
		{"Pod", func(o *sandbox.Observation, _ time.Time) { o.PodUID = "other-pod" }},
		{"node", func(o *sandbox.Observation, _ time.Time) { o.NodeUID = "other-node" }},
		{"epoch", func(o *sandbox.Observation, _ time.Time) { o.EnforcementEpoch = "other-epoch" }},
		{"runtime incomplete", func(o *sandbox.Observation, _ time.Time) { o.RuntimeVerified = false }},
		{"Pod incomplete", func(o *sandbox.Observation, _ time.Time) { o.PodVerified = false }},
		{"controls incomplete", func(o *sandbox.Observation, _ time.Time) { o.ControlsVerified = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			before := f.historicalStatus(t, false)
			ctx, capture := capturePolicyEvents(t)
			tc.mutate(&f.observation, f.now)
			if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard); err == nil {
				t.Fatal("unverified observation was accepted")
			}
			if !reflect.DeepEqual(before, f.current(t)) || len(capture.events) != 0 {
				t.Fatal("unverified evidence wrote status/history or emitted an application")
			}
		})
	}
	t.Run("inclusive freshness bound", func(t *testing.T) {
		f := newExecutionStatusFixture(t, "hermes")
		f.observation.ObservedAt = f.now.Add(-f.writer.MaxAge)
		if err := f.writer.CommitReadiness(context.Background(), f.ticket, f.observation, statusTestGuard); err != nil {
			t.Fatal("observation at the configured freshness bound was rejected:", err)
		}
	})
	for _, during := range []string{"source reads", "final guard"} {
		t.Run("expires during "+during, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			before := f.current(t)
			calls := 0
			guard := func(ctx context.Context) error {
				calls++
				if during == "final guard" && calls == 2 {
					f.now = f.now.Add(16 * time.Second)
				}
				return ctx.Err()
			}
			if during == "source reads" {
				f.source.onResolve = func() { f.now = f.now.Add(16 * time.Second) }
			}
			if err := f.writer.CommitReadiness(context.Background(), f.ticket, f.observation, guard); err == nil {
				t.Fatal("evidence that expired during authoritative reads was applied")
			}
			if !reflect.DeepEqual(before, f.current(t)) {
				t.Fatal("expired evidence changed the Agent")
			}
		})
	}
}

func TestSandboxExecutionStatusWriterRequiresDependenciesAndActualTicket(t *testing.T) {
	for _, clear := range []bool{false, true} {
		method := "commit"
		if clear {
			method = "clear"
		}
		for _, tc := range []struct {
			name   string
			mutate func(*executionStatusFixture, *sandboxcoordination.Ticket, *func(context.Context) error)
		}{
			{"reconciler", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.writer.Reconciler = nil
			}},
			{"writer", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.reconciler.Client = nil
			}},
			{"uncached reader", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.reconciler.APIReader = nil
			}},
			{"clock", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.writer.Now = nil
			}},
			{"zero clock", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.writer.Now = func() time.Time { return time.Time{} }
			}},
			{"guard", func(_ *executionStatusFixture, _ *sandboxcoordination.Ticket, g *func(context.Context) error) {
				*g = nil
			}},
			{"store writer", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.writer.Coordination.Client = nil
			}},
			{"store reader", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.writer.Coordination.APIReader = nil
			}},
			{"store identity", func(f *executionStatusFixture, _ *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				f.writer.Coordination.Identity.UID = "other-authority"
			}},
			{"authority", func(_ *executionStatusFixture, ticket *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				ticket.AuthorityUID = "other-authority"
			}},
			{"operation", func(_ *executionStatusFixture, ticket *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				ticket.Execution.ID = "other-operation"
			}},
			{"phase", func(_ *executionStatusFixture, ticket *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				ticket.Execution.Phase = sandboxcoordination.Reserved
			}},
			{"epoch", func(_ *executionStatusFixture, ticket *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				ticket.Execution.Intent.Expected.EnforcementEpoch = "other-epoch"
				ticket.Execution.Intent.Writer.Epoch = "other-epoch"
			}},
			{"incomplete intent", func(_ *executionStatusFixture, ticket *sandboxcoordination.Ticket, _ *func(context.Context) error) {
				ticket.Execution.Intent.Expected.PodUID = ""
			}},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				f := newExecutionStatusFixture(t, "hermes")
				ticket := f.ticket.DeepCopy()
				if clear {
					ticket = f.stopping(t)
				}
				before := f.current(t)
				guard := statusTestGuard
				tc.mutate(f, &ticket, &guard)
				var err error
				if clear {
					err = f.writer.ClearReadiness(context.Background(), ticket, guard)
				} else {
					err = f.writer.CommitReadiness(context.Background(), ticket, f.observation, guard)
				}
				if err == nil || !reflect.DeepEqual(before, f.current(t)) {
					t.Fatal("missing dependency or mismatched actual authority did not fail closed:", err)
				}
			})
		}
	}
	for _, maxAge := range []time.Duration{0, -time.Second, time.Minute + time.Nanosecond} {
		t.Run("freshness/"+maxAge.String(), func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			before := f.current(t)
			f.writer.MaxAge = maxAge
			if err := f.writer.CommitReadiness(context.Background(), f.ticket, f.observation, statusTestGuard); err == nil ||
				!reflect.DeepEqual(before, f.current(t)) {
				t.Fatal("positive status accepted an unbounded freshness configuration")
			}
		})
	}
}

func TestSandboxExecutionStatusWriterCopiesCallerEvidence(t *testing.T) {
	f := newExecutionStatusFixture(t, "hermes")
	ticket, observation := f.ticket.DeepCopy(), f.observation
	calls := 0
	guard := func(ctx context.Context) error {
		calls++
		if calls == 1 {
			ticket.Execution.Intent.Sources[0].UID = "caller-mutated-source"
			for key := range ticket.Execution.Intent.SourceDigests {
				ticket.Execution.Intent.SourceDigests[key] = strings.Repeat("f", 64)
			}
			ticket.Execution.Intent.Placement.Nodes[0].UID = "caller-mutated-node"
			observation.PodUID, observation.ControlsVerified = "caller-mutated-pod", false
		}
		return ctx.Err()
	}
	if err := f.writer.CommitReadiness(context.Background(), ticket, observation, guard); err != nil {
		t.Fatal("caller mutation leaked into the writer's captured intent/evidence:", err)
	}
	if f.current(t).Status.Sandbox.AppliedRevision != f.observation.Revision || calls != 3 {
		t.Fatal("immutable evidence was not guarded at both sides of the status write")
	}
}

func TestSandboxExecutionStatusWriterPropagatesSourceAndAgentDrift(t *testing.T) {
	for _, drift := range []string{
		"provider error", "held Agent", "held expectation", "missing policy", "policy changes on recheck",
		"Agent generation", "Agent UID", "Agent spec during reads", "Agent RV during reads",
	} {
		t.Run(drift, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			before := f.historicalStatus(t, false)
			injected := errors.New("sk-dummy synthetic provider failure at https://provider.example.com/")
			switch drift {
			case "provider error":
				f.source.resolveErr = injected
			case "held Agent":
				current := f.current(t)
				current.Finalizers = append(current.Finalizers, sandbox.HoldFinalizer)
				if err := f.client.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			case "held expectation":
				expected := f.expectation(t)
				expected.ExecutionHeld = true
				state := &corev1.ConfigMap{}
				if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: f.ticket.Execution.Intent.Preparation.Namespace,
					Name: f.ticket.Execution.Intent.Preparation.Name}, state); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(expected)
				if err != nil {
					t.Fatal(err)
				}
				state.Data[w.PodExpectationKey] = string(raw)
				if err := f.client.Update(context.Background(), state); err != nil {
					t.Fatal(err)
				}
			case "missing policy":
				if err := f.client.Delete(context.Background(), f.fixture.Policy); err != nil {
					t.Fatal(err)
				}
			case "policy changes on recheck":
				reads := 0
				f.source.onResolve = func() {
					reads++
					if reads != 2 {
						return
					}
					policy := &api.AgentPolicy{}
					if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Policy), policy); err != nil {
						t.Fatal(err)
					}
					policy.Spec.Network.BlockedFQDNs = []string{"changed.example.com"}
					if err := f.client.Update(context.Background(), policy); err != nil {
						t.Fatal(err)
					}
				}
			case "Agent generation":
				current := f.current(t)
				current.Generation++
				if err := f.client.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			case "Agent UID":
				replaceStatusAgent(t, f.preparationFixture)
			case "Agent spec during reads", "Agent RV during reads":
				changed := false
				f.source.onResolve = func() {
					if changed {
						return
					}
					changed = true
					current := f.current(t)
					if drift == "Agent spec during reads" {
						current.Spec.Suspended = true
						current.Generation++
						if err := f.client.Update(context.Background(), current); err != nil {
							t.Fatal(err)
						}
					} else {
						current.Status.AccessURL = "https://concurrent.example.com/"
						if err := f.client.Status().Update(context.Background(), current); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			ctx, capture := capturePolicyEvents(t)
			counts := &preparationRequestCounts{Client: f.client}
			f.reconciler.Client = counts
			err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard)
			if err == nil || counts.writes != 0 || len(capture.events) != 0 {
				t.Fatal("drift or unavailable input persisted status or was swallowed:", err)
			}
			if drift == "provider error" && !errors.Is(err, injected) {
				t.Fatal("source rejection was not propagated:", err)
			}
			if drift == "Agent RV during reads" && !apierrors.IsConflict(err) {
				t.Fatal("changed Agent RV was rebased rather than rejected:", err)
			}
			if drift == "held Agent" || drift == "held expectation" {
				if drift == "held Agent" {
					found := false
					for _, finalizer := range f.current(t).Finalizers {
						found = found || finalizer == sandbox.HoldFinalizer
					}
					if !found {
						t.Fatal("status application removed an Agent execution hold")
					}
				} else if !f.expectation(t).ExecutionHeld {
					t.Fatal("status application removed the expectation execution hold")
				}
			}
			if drift != "Agent UID" && !reflect.DeepEqual(before.Status.Sandbox, f.current(t).Status.Sandbox) {
				t.Fatal("failed status application advanced history")
			}
			raw, marshalErr := json.Marshal(f.current(t).Status)
			if marshalErr != nil || strings.Contains(string(raw), "sk-dummy") {
				t.Fatal("provider error was copied into persisted status")
			}
		})
	}
}

func replaceStatusAgent(t *testing.T, f *preparationFixture) {
	t.Helper()
	current := f.current(t)
	current.Finalizers = nil
	if err := f.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	replacement := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: current.Name, UID: "replacement-agent", Generation: current.Generation},
		Spec: *current.Spec.DeepCopy()}
	if err := f.client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxExecutionStatusWriterAllowsPriorMetadataChurn(t *testing.T) {
	f := newExecutionStatusFixture(t, "hermes")
	ctx := context.Background()
	if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	agent.Labels = map[string]string{"example.com/metadata-only": "change"}
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: "SyntheticOtherStatus", Status: metav1.ConditionTrue, Reason: "IndependentObservation",
		Message: "Synthetic status-only churn.", ObservedGeneration: agent.Generation, LastTransitionTime: metav1.NewTime(f.now),
	})
	if err := f.client.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	before := f.current(t)
	counts := &preparationRequestCounts{Client: f.client}
	f.reconciler.Client = counts
	if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
		t.Fatal("prior unrelated status/metadata churn invalidated unchanged inputs:", err)
	}
	if counts.writes != 0 || !reflect.DeepEqual(before, f.current(t)) {
		t.Fatal("unchanged Sandbox status replaced unrelated status or rewrote the Agent")
	}
}

func TestSandboxExecutionStatusWriterClearUsesOnlyStoppingAuthorityAndCurrentAgent(t *testing.T) {
	for _, state := range []string{"stopping", "suspended", "deleting", "suspended and deleting", "rebound"} {
		t.Run(state, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			f.historicalStatus(t, true)
			ticket := f.stopping(t)
			ctx := context.Background()
			agent := f.current(t)
			agent.Generation++
			if strings.Contains(state, "suspended") {
				agent.Spec.Suspended = true
			}
			if state == "rebound" {
				agent.Spec.SandboxRef = "replacement-sandbox"
			}
			if err := f.client.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(state, "deleting") {
				if err := f.client.Delete(ctx, agent); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.client.Delete(ctx, f.fixture.Policy); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Delete(ctx, f.fixture.Namespace); err != nil {
				t.Fatal(err)
			}
			f.reconciler.SandboxPreparation = nil
			f.reconciler.APIReader = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*api.Agent); !ok {
						return errors.New("synthetic unavailable policy/source reader")
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			before := f.current(t)
			counts := &preparationRequestCounts{Client: f.client}
			f.reconciler.Client = counts
			// Cleanup does not consume an enforcement observation or its freshness bound.
			f.writer.MaxAge = 0
			if err := f.writer.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
				t.Fatal("cleanup depended on current sources or the old Agent generation:", err)
			}
			after := f.current(t)
			if counts.writes != 1 || after.ResourceVersion == before.ResourceVersion ||
				!reflect.DeepEqual(before.Status.Sandbox, after.Status.Sandbox) ||
				!reflect.DeepEqual(before.Spec, after.Spec) || !reflect.DeepEqual(before.Annotations, after.Annotations) ||
				!reflect.DeepEqual(before.Finalizers, after.Finalizers) || after.Status.ObservedGeneration != after.Generation ||
				after.Status.AccessURL != "" {
				t.Fatal("cleanup changed historical binding/holds or failed to clear current-generation access")
			}
			reason, phase := "Stopping", api.PhasePending
			if strings.Contains(state, "deleting") {
				reason, phase = "Deleting", api.PhaseDeleting
			} else if strings.Contains(state, "suspended") {
				reason, phase = "Suspended", api.PhaseSuspended
			}
			for _, kind := range []string{api.ConditionReady, api.ConditionSandboxReady} {
				condition := apimeta.FindStatusCondition(after.Status.Conditions, kind)
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reason ||
					condition.ObservedGeneration != after.Generation || !strings.Contains(condition.Message, "sha256:") ||
					strings.Contains(condition.Message, ticket.Execution.ID) || strings.Contains(condition.Message, string(ticket.AuthorityUID)) {
					t.Fatalf("cleanup condition or opaque stop marker is incorrect: %+v", condition)
				}
			}
			if after.Status.Phase != phase {
				t.Fatal("cleanup phase does not reflect actual Agent lifecycle")
			}
			f.now = f.now.Add(time.Second)
			if err := f.writer.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
				t.Fatal(err)
			}
			if counts.writes != 1 || !reflect.DeepEqual(after, f.current(t)) {
				t.Fatal("identical cleanup was not an exact no-op")
			}
		})
	}
}

func TestSandboxExecutionStatusWriterRejectsReplacementAndLaterExecution(t *testing.T) {
	for _, when := range []string{"replacement before read", "later execution before read", "later execution at guard"} {
		t.Run(when, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			ticket := f.stopping(t)
			ctx := context.Background()
			later := func() {
				if err := f.writer.Coordination.ReleaseExecution(ctx, ticket); err != nil {
					t.Fatal(err)
				}
				intent := ticket.DeepCopy().Execution.Intent
				intent.Expected.EnforcementEpoch, intent.Writer.Epoch = "later-epoch", "later-epoch"
				if _, err := f.writer.Coordination.AcquireExecution(ctx, intent); err != nil {
					t.Fatal(err)
				}
			}
			switch when {
			case "replacement before read":
				replaceStatusAgent(t, f.preparationFixture)
			case "later execution before read":
				later()
			}
			before := f.current(t)
			calls := 0
			guard := func(ctx context.Context) error {
				calls++
				if when == "later execution at guard" && calls == 2 {
					later()
				}
				return ctx.Err()
			}
			if err := f.writer.ClearReadiness(ctx, ticket, guard); !errors.Is(err, sandboxcoordination.ErrConflict) {
				t.Fatal("stale stopping operation did not reject a replacement or newer epoch:", err)
			}
			if !reflect.DeepEqual(before, f.current(t)) {
				t.Fatal("stale stopping operation changed a replacement or later execution")
			}
		})
	}
}

func TestSandboxExecutionStatusWriterGuardsAfterReadsAndAcknowledgement(t *testing.T) {
	failure := errors.New("synthetic authority guard failure")
	for _, clear := range []bool{false, true} {
		for _, failAt := range []int{1, 2, 3} {
			name := "commit"
			if clear {
				name = "clear"
			}
			t.Run(fmt.Sprintf("%s/%d", name, failAt), func(t *testing.T) {
				f := newExecutionStatusFixture(t, "hermes")
				ticket := f.ticket
				if clear {
					ticket = f.stopping(t)
				}
				before := f.current(t)
				ctx, capture := capturePolicyEvents(t)
				counts := &preparationRequestCounts{Client: f.client}
				f.reconciler.Client = counts
				calls := 0
				guard := func(ctx context.Context) error {
					calls++
					if calls == failAt {
						return failure
					}
					return ctx.Err()
				}
				var err error
				if clear {
					err = f.writer.ClearReadiness(ctx, ticket, guard)
				} else {
					err = f.writer.CommitReadiness(ctx, ticket, f.observation, guard)
				}
				if !errors.Is(err, failure) || len(capture.events) != 0 {
					t.Fatal("guard failure was hidden or a trusted application was emitted:", err)
				}
				if failAt < 3 {
					if counts.writes != 0 || !reflect.DeepEqual(before, f.current(t)) {
						t.Fatal("precommit guard failure allowed a status mutation")
					}
				} else if counts.writes != 1 || f.current(t).ResourceVersion == before.ResourceVersion {
					t.Fatal("fixture did not exercise an acknowledged write followed by a guard failure")
				}
			})
		}
	}
}

func TestSandboxExecutionStatusWriterDoesNotClaimUnacknowledgedWrites(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		name := "write rejected"
		if lostReply {
			name = "write acknowledged only by server"
		}
		t.Run(name, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			before := f.current(t)
			failure := errors.New("synthetic unavailable status acknowledgement")
			ctx, capture := capturePolicyEvents(t)
			calls := 0
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, _ string, object client.Object, patch client.Patch,
					opts ...client.SubResourcePatchOption,
				) error {
					calls++
					if lostReply {
						if err := c.Status().Patch(ctx, object, patch, opts...); err != nil {
							return err
						}
					}
					return failure
				},
			})
			err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard)
			if !errors.Is(err, failure) || calls != 1 || len(capture.events) != 0 {
				t.Fatal("uncertain write was retried, reported as success or emitted a trusted event:", err)
			}
			changed := f.current(t).ResourceVersion != before.ResourceVersion
			if changed != lostReply {
				t.Fatal("fixture did not distinguish rejection from a lost write response")
			}
		})
	}
}

func TestSandboxExecutionStatusWriterRejectsEvidenceExpiredDuringPatch(t *testing.T) {
	f := newExecutionStatusFixture(t, "openclaw")
	ctx, capture := capturePolicyEvents(t)
	writes := 0
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, _ string, object client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			writes++
			if err := c.Status().Patch(ctx, object, patch, opts...); err != nil {
				return err
			}
			f.now = f.now.Add(f.writer.MaxAge + time.Nanosecond)
			return nil
		},
	})
	err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard)
	if !errors.Is(err, sandbox.ErrObservationUnavailable) || writes != 1 || len(capture.events) != 0 {
		t.Fatal("expired evidence was reported as a verified committed status:", err)
	}
}

func TestSandboxExecutionStatusWriterClearConflictsWithDelayedPositiveWrite(t *testing.T) {
	f := newExecutionStatusFixture(t, "hermes")
	before := f.historicalStatus(t, false)
	ctx, capture := capturePolicyEvents(t)
	calls := 0
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, _ string, object client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			calls++
			ticket := f.stopping(t)
			stopWriter := f.writer
			stopWriter.Reconciler = &controller.AgentReconciler{Client: f.client, APIReader: f.client}
			if err := stopWriter.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
				t.Fatal(err)
			}
			cleared := f.current(t)
			if cleared.ResourceVersion == before.ResourceVersion ||
				!apimeta.IsStatusConditionFalse(cleared.Status.Conditions, api.ConditionSandboxReady) {
				t.Fatal("first clear of an already nonready Agent did not force a new resource version")
			}
			// Submit the real fake-client CAS only after clear has committed.
			return c.Status().Patch(ctx, object, patch, opts...)
		},
	})
	if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard); !apierrors.IsConflict(err) {
		t.Fatal("delayed old-RV positive status overwrote stopping status:", err)
	}
	after := f.current(t)
	if calls != 1 || len(capture.events) != 0 || !reflect.DeepEqual(before.Status.Sandbox, after.Status.Sandbox) ||
		!apimeta.IsStatusConditionFalse(after.Status.Conditions, api.ConditionReady) ||
		!apimeta.IsStatusConditionFalse(after.Status.Conditions, api.ConditionSandboxReady) || after.Status.AccessURL != "" {
		t.Fatal("a delayed positive write escaped CAS, rebased or changed cleared history/readiness")
	}
}

func TestSandboxExecutionStatusWriterDisabledIngressClearsAnExistingURL(t *testing.T) {
	f := newExecutionStatusFixture(t, "hermes")
	before := f.historicalStatus(t, true)
	if f.ticket.Execution.Intent.IngressMode != api.AgentPolicyIngressDisabled {
		t.Fatal("fixture must describe disabled ingress")
	}
	if err := f.writer.CommitReadiness(context.Background(), f.ticket, f.observation, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	after := f.current(t)
	assertAppliedExecutionStatus(t, after, before, f.observation.ExecutionIdentity)
	if !apimeta.IsStatusConditionTrue(after.Status.Conditions, api.ConditionIngressReady) ||
		apimeta.FindStatusCondition(after.Status.Conditions, api.ConditionIngressReady).Reason != "IngressDisabled" {
		t.Fatal("verified disabled ingress did not preserve the existing status helper's semantics")
	}
}

func TestSandboxExecutionStatusWriterRechecksNoOpAndContext(t *testing.T) {
	for _, scenario := range []string{"cancelled", "expired no-op", "guard rejects no-op", "authority changes before patch"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes")
			ctx := context.Background()
			if scenario == "expired no-op" || scenario == "guard rejects no-op" {
				if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
					t.Fatal(err)
				}
			}

			before := f.current(t)
			counts := &preparationRequestCounts{Client: f.client}
			f.reconciler.Client = counts
			guard := statusTestGuard
			switch scenario {
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "expired no-op":
				f.now = f.now.Add(f.writer.MaxAge + time.Nanosecond)
			case "guard rejects no-op":
				guard = func(context.Context) error { return errors.New("synthetic revoked guard") }
			case "authority changes before patch":
				calls := 0
				guard = func(ctx context.Context) error {
					calls++
					if calls == 2 {
						f.stopping(t)
					}
					return ctx.Err()
				}
			}
			if err := f.writer.CommitReadiness(ctx, f.ticket, f.observation, guard); err == nil {
				t.Fatal("an unchanged status or permissive callback bypassed current checks")
			}
			if counts.writes != 0 || !reflect.DeepEqual(before, f.current(t)) {
				t.Fatal("failed revalidation changed persisted status")
			}
		})
	}
}

func TestSandboxExecutionStatusWriterAuthenticatedIngressDoesNotPublishAccess(t *testing.T) {
	for _, priorURL := range []string{"", "https://existing.example.com/"} {
		t.Run("priorURL="+priorURL, func(t *testing.T) {
			f := newExecutionStatusFixture(t, "hermes", func(f *preparationFixture) {
				registration := f.config.Registrations[api.SandboxKata]
				registration.Capabilities.AuthenticatedIngress = true
				f.config.Registrations[api.SandboxKata] = registration
				policy := &api.AgentPolicy{}
				if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Policy), policy); err != nil {
					t.Fatal(err)
				}
				policy.Spec.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated
				if err := f.client.Update(context.Background(), policy); err != nil {
					t.Fatal(err)
				}
			})
			before := f.current(t)
			before.Status.AccessURL = priorURL
			if err := f.client.Status().Update(context.Background(), before); err != nil {
				t.Fatal(err)
			}
			if f.ticket.Execution.Intent.IngressMode != api.AgentPolicyIngressAuthenticated {
				t.Fatal("fixture must describe authenticated ingress")
			}
			if err := f.writer.CommitReadiness(context.Background(), f.ticket, f.observation, statusTestGuard); err != nil {
				t.Fatal(err)
			}
			after := f.current(t)
			if after.Status.AccessURL != priorURL || after.Status.Phase != before.Status.Phase ||
				!reflect.DeepEqual(apimeta.FindStatusCondition(before.Status.Conditions, api.ConditionReady),
					apimeta.FindStatusCondition(after.Status.Conditions, api.ConditionReady)) ||
				!reflect.DeepEqual(apimeta.FindStatusCondition(before.Status.Conditions, api.ConditionIngressReady),
					apimeta.FindStatusCondition(after.Status.Conditions, api.ConditionIngressReady)) ||
				!apimeta.IsStatusConditionTrue(after.Status.Conditions, api.ConditionSandboxReady) {
				t.Fatal("status writer published authenticated access or global/application readiness")
			}
		})
	}
}

func TestSandboxExecutionStatusWriterEveryNewStopInvalidatesPendingWrites(t *testing.T) {
	f := newExecutionStatusFixture(t, "hermes")
	ctx := context.Background()
	ticket := f.stopping(t)
	if err := f.writer.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	first := f.current(t)
	if err := f.writer.Coordination.ReleaseExecution(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	next, err := f.writer.Coordination.AcquireExecution(ctx, f.ticket.DeepCopy().Execution.Intent)
	if err != nil {
		t.Fatal(err)
	}
	next, err = f.writer.Coordination.BeginStop(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.writer.ClearReadiness(ctx, next, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	second := f.current(t)
	if next.Execution.ID == ticket.Execution.ID || second.ResourceVersion == first.ResourceVersion ||
		apimeta.FindStatusCondition(first.Status.Conditions, api.ConditionSandboxReady).Message ==
			apimeta.FindStatusCondition(second.Status.Conditions, api.ConditionSandboxReady).Message ||
		!reflect.DeepEqual(first.Status.Sandbox, second.Status.Sandbox) {
		t.Fatal("a new exact stop reused the old nonready revision or changed applied history")
	}
}

func TestSandboxExecutionStatusWriterLegacyTicketCanClearButNotApply(t *testing.T) {
	f := newExecutionStatusFixture(t, "hermes")
	ctx, capture := capturePolicyEvents(t)
	if err := f.writer.Coordination.ReleaseExecution(ctx, f.stopping(t)); err != nil {
		t.Fatal(err)
	}
	intent := f.ticket.DeepCopy().Execution.Intent
	intent.RunningPreparationDigest = ""
	legacy, err := f.writer.Coordination.AcquireExecution(ctx, intent)
	if err != nil {
		t.Fatal("the authority no longer accepts a structurally valid legacy intent:", err)
	}
	legacy, err = f.writer.Coordination.BeginStart(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err = f.writer.Coordination.MarkRunning(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	before := f.historicalStatus(t, false)
	if err := f.writer.CommitReadiness(ctx, legacy, f.observation, statusTestGuard); err == nil {
		t.Fatal("a legacy ticket without the running pin authorized positive status")
	}
	if !reflect.DeepEqual(before, f.current(t)) || len(capture.events) != 0 {
		t.Fatal("rejected legacy execution changed history/readiness or emitted an application")
	}
	stopping, err := f.writer.Coordination.BeginStop(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = nil
	if err := f.writer.ClearReadiness(ctx, stopping, statusTestGuard); err != nil {
		t.Fatal("legacy cleanup required a running pin or source availability:", err)
	}
	after := f.current(t)
	if after.ResourceVersion == before.ResourceVersion || !reflect.DeepEqual(before.Status.Sandbox, after.Status.Sandbox) ||
		!apimeta.IsStatusConditionFalse(after.Status.Conditions, api.ConditionSandboxReady) ||
		!apimeta.IsStatusConditionFalse(after.Status.Conditions, api.ConditionReady) || after.Status.AccessURL != "" ||
		!reflect.DeepEqual(before.Spec, after.Spec) || !reflect.DeepEqual(before.Finalizers, after.Finalizers) {
		t.Fatal("legacy cleanup did not preserve history/holds while clearing readiness")
	}
	if err := f.writer.ClearReadiness(ctx, stopping, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, f.current(t)) {
		t.Fatal("repeated legacy cleanup changed the Agent")
	}
}
