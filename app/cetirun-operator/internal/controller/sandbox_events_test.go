// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

type capturedSandboxEvent struct {
	Event                  string                 `json:"event"`
	Timestamp              string                 `json:"timestamp"`
	TransitionID           string                 `json:"transitionID"`
	Logger                 string                 `json:"logger"`
	ResourceKind           string                 `json:"resourceKind"`
	ResourceUID            string                 `json:"resourceUID"`
	Generation             int64                  `json:"generation"`
	Condition              string                 `json:"condition"`
	ConditionStatus        metav1.ConditionStatus `json:"conditionStatus"`
	Reason                 string                 `json:"reason"`
	AttemptedBinding       sandboxEventBinding    `json:"attemptedBinding"`
	AttemptedBindingSource string                 `json:"attemptedBindingSource"`
	AttemptedRevision      string                 `json:"attemptedRevision"`
	AppliedBinding         sandboxEventBinding    `json:"appliedBinding"`
	AppliedRevision        string                 `json:"appliedRevision"`
}

type sandboxEventCapture struct {
	events []capturedSandboxEvent
	raw    []string
}

func captureSandboxEvents(t *testing.T) (context.Context, *sandboxEventCapture) {
	t.Helper()
	capture := &sandboxEventCapture{}
	logger := funcr.NewJSON(func(raw string) {
		var event capturedSandboxEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event != "" {
			capture.events = append(capture.events, event)
			capture.raw = append(capture.raw, raw)
		}
	}, funcr.Options{})
	return log.IntoContext(context.Background(), logger), capture
}

func sandboxEventAgentFixture(t *testing.T) (*api.Agent, SandboxStatusInput) {
	t.Helper()
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "event-example", UID: "00000000-0000-4000-8000-000000000001", Generation: 3, ResourceVersion: "1",
			Finalizers: []string{sandbox.HoldFinalizer},
		},
		Spec: api.AgentSpec{
			Team: "event-team", SandboxRef: "event-sandbox",
			Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "event-example"},
		},
		Status: api.AgentStatus{Phase: api.PhasePending, ObservedGeneration: 2},
	}
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Team, UID: "00000000-0000-4000-8000-000000000002"},
		Spec:       api.TeamSpec{SandboxRefs: []string{agent.Spec.SandboxRef}, AgentPolicyRef: "event-policy"},
	}
	box := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.SandboxRef, UID: "00000000-0000-4000-8000-000000000003"}}
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "event-policy", UID: "00000000-0000-4000-8000-000000000004"}}
	record, err := sandboxbinding.Snapshot(agent, team, sandbox.Resources{
		Sandboxes: map[string]*api.Sandbox{box.Name: box}, AgentPolicies: map[string]*api.AgentPolicy{policy.Name: policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations = map[string]string{sandboxbinding.AuthorizationAnnotation: record}
	binding, err := sandboxbinding.AuthorizedBinding(agent)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	expected := sandbox.ExecutionIdentity{
		Binding: binding, Revision: strings.Repeat("b", 64),
		PodUID: "synthetic-pod-uid", NodeUID: "synthetic-node-uid", EnforcementEpoch: "synthetic-epoch",
	}
	return agent, SandboxStatusInput{
		AgentGeneration: agent.Generation, Resolution: &sandbox.Resolution{Binding: binding, Revision: strings.Repeat("a", 64)},
		Plan: &sandboxpolicy.Plan{
			Binding: binding, AgentName: agent.Name, Namespace: "cetirun-event-example",
			ResolutionRevision: strings.Repeat("a", 64), Revision: expected.Revision,
			Network: sandboxpolicy.NetworkPlan{IngressMode: api.AgentPolicyIngressDisabled},
		},
		Expected: expected,
		Observation: &sandbox.Observation{
			ExecutionIdentity: expected, ObservedAt: now, RuntimeVerified: true, PodVerified: true, ControlsVerified: true,
		},
		Now: now, MaxAge: 15 * time.Second,
	}
}

func sandboxEventClient(t *testing.T, agent *api.Agent) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
}

func TestSandboxEventsAppliedOnlyAfterCommittedVerifiedStatus(t *testing.T) {
	agent, input := sandboxEventAgentFixture(t)
	ctx, capture := captureSandboxEvents(t)
	backing := sandboxEventClient(t, agent)
	reads, writes := 0, 0
	c := interceptor.NewClient(backing, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			reads++
			return c.Get(ctx, key, object, options...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			writes++
			if len(capture.events) != 0 {
				t.Fatal("event preceded the status commit")
			}
			data, err := patch.Data(object)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
				Spec json.RawMessage `json:"spec"`
			}
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			if subresource != "status" || body.Metadata.ResourceVersion != "1" || body.Spec != nil {
				t.Fatal("event wiring changed the optimistic status-only patch")
			}
			err = c.SubResource(subresource).Patch(ctx, object, patch, options...)
			if len(capture.events) != 0 {
				t.Fatal("event was emitted before the writer returned success")
			}
			return err
		},
	})
	r := &AgentReconciler{Client: c, APIReader: c}
	if result, err := UpdateSandboxAgentStatus(agent, input); err != nil || !result.Verified || result.Hold {
		t.Fatalf("synthetic verification failed: %+v %v", result, err)
	}
	if len(capture.events) != 0 {
		t.Fatal("status calculation emitted a committed event")
	}
	start := time.Now()
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if len(capture.events) != 1 || reads != 1 || writes != 1 {
		t.Fatalf("expected one committed event without extra API operations: events=%d reads=%d writes=%d", len(capture.events), reads, writes)
	}
	event := capture.events[0]
	expectedBinding := sandboxEventBinding{
		AgentUID: string(agent.UID), TeamUID: string(input.Expected.Binding.TeamUID),
		SandboxUID: string(input.Expected.Binding.SandboxUID), AgentPolicyUID: string(input.Expected.Binding.AgentPolicyUID),
	}
	if event.Event != "sandbox.policy.applied" || event.Logger != "sandbox-policy" ||
		event.Reason != "PolicyApplied" || event.Condition != api.ConditionSandboxReady ||
		event.ConditionStatus != metav1.ConditionTrue || event.Generation != agent.Generation ||
		event.ResourceKind != "Agent" || event.ResourceUID != string(agent.UID) ||
		event.AttemptedBinding != expectedBinding || event.AppliedBinding != expectedBinding ||
		event.AttemptedBindingSource != "authorization" ||
		event.AttemptedRevision != input.Plan.Revision || event.AppliedRevision != input.Plan.Revision {
		t.Fatalf("unexpected committed applied event: %+v", event)
	}
	timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil || timestamp.Before(start) || timestamp.After(time.Now()) || !strings.HasSuffix(event.Timestamp, "Z") {
		t.Fatalf("event has no bounded UTC emission timestamp: %q %v", event.Timestamp, err)
	}
	stored := &api.Agent{}
	if err := backing.Get(ctx, client.ObjectKeyFromObject(agent), stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(agent.Status, stored.Status) || agent.ResourceVersion != stored.ResourceVersion {
		t.Fatal("the event and caller did not follow the committed status response")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(capture.raw[0]), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"event", "timestamp", "transitionID", "resourceKind", "resourceUID", "generation", "condition", "conditionStatus", "reason",
		"attemptedBinding", "attemptedBindingSource", "attemptedRevision", "appliedBinding", "appliedRevision",
	} {
		if len(fields[key]) == 0 {
			t.Errorf("missing structured field %s", key)
		}
	}
}

func TestSandboxEventsFailedAndLostRetainPriorAppliedBinding(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		failure      error
		status       metav1.ConditionStatus
	}{
		{"lost", "EnforcementLost", sandbox.ErrEnforcementPending, metav1.ConditionFalse},
		{"arbitrary error", "PolicyApplyFailed", errors.New("sk-dummy provider response https://provider.example.com/private?token=synthetic"), metav1.ConditionFalse},
		{"unsupported", "UnsupportedControl", sandboxpolicy.ErrUnsupportedControl, metav1.ConditionFalse},
		{"conflict", "PolicyConflict", sandboxpolicy.ErrConflict, metav1.ConditionFalse},
		{"unauthorized", "UnauthorizedBinding", sandbox.ErrUnauthorizedBinding, metav1.ConditionFalse},
		{"grant", "UnauthorizedBinding", sandboxpolicy.ErrUnapprovedGrant, metav1.ConditionFalse},
		{"runtime", "RuntimeUnavailable", sandbox.ErrRuntimeUnavailable, metav1.ConditionFalse},
		{"missing reference", "ReferenceMissing", apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxes"}, "synthetic-missing"), metav1.ConditionFalse},
		{"observation unavailable", "ObservationUnavailable", sandbox.ErrObservationUnavailable, metav1.ConditionUnknown},
		{"dependencies unavailable", "ObservationUnavailable", sandboxpolicy.ErrUnavailable, metav1.ConditionUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, input := sandboxEventAgentFixture(t)
			backing := sandboxEventClient(t, agent)
			r := &AgentReconciler{Client: backing, APIReader: backing}
			if _, err := UpdateSandboxAgentStatus(agent, input); err != nil {
				t.Fatal(err)
			}
			if err := r.updateStatus(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			history := *agent.Status.Sandbox
			agent.Annotations[sandboxbinding.AuthorizationAnnotation] = strings.ReplaceAll(
				agent.Annotations[sandboxbinding.AuthorizationAnnotation],
				string(history.AgentPolicyUID), "00000000-0000-4000-8000-000000000014")
			if err := backing.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			ctx, capture := captureSandboxEvents(t)
			input.Failure = tc.failure
			if _, err := UpdateSandboxAgentStatus(agent, input); !errors.Is(err, tc.failure) {
				t.Fatalf("original status error was lost: %v", err)
			}
			if err := r.updateStatus(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if len(capture.events) != 1 {
				t.Fatalf("missing committed failure event: %+v", capture.events)
			}
			event := capture.events[0]
			expectedType := "sandbox.enforcement.failed"
			if tc.reason == "EnforcementLost" {
				expectedType = "sandbox.enforcement.lost"
			}
			if event.Event != expectedType || event.Reason != tc.reason || event.ConditionStatus != tc.status ||
				event.AttemptedBinding.AgentPolicyUID != "00000000-0000-4000-8000-000000000014" ||
				event.AppliedBinding.AgentPolicyUID != string(history.AgentPolicyUID) ||
				event.AppliedBinding.TeamUID != string(history.TeamUID) ||
				event.AppliedBinding.SandboxUID != string(history.SandboxUID) ||
				event.AttemptedRevision != sandboxEventUnresolved || event.AppliedRevision != history.AppliedRevision {
				t.Fatalf("attempt or prior application was mislabeled: %+v", event)
			}
			if *agent.Status.Sandbox != history {
				t.Fatal("event wiring modified applied history")
			}
			if strings.Contains(capture.raw[0], "sk-dummy") || strings.Contains(capture.raw[0], "provider.example.com") {
				t.Fatal("event exposed arbitrary provider error content")
			}
			if err := r.updateStatus(ctx, agent); err != nil || len(capture.events) != 1 {
				t.Fatalf("unchanged failure retry emitted again: %v %+v", err, capture.events)
			}
		})
	}
}

func TestSandboxEventsRetrySuppressionAndStableCorrelation(t *testing.T) {
	agent, input := sandboxEventAgentFixture(t)
	ctx, capture := captureSandboxEvents(t)
	backing := sandboxEventClient(t, agent)
	r := &AgentReconciler{Client: backing}
	base := agent.DeepCopy()
	if _, err := UpdateSandboxAgentStatus(agent, input); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	first := capture.events[0].TransitionID
	if len(first) != 64 || !sandboxEventHex(first) {
		t.Fatalf("correlation is not a fixed digest: %q", first)
	}
	for range 4 {
		input.Now = input.Now.Add(time.Second)
		input.Observation.ObservedAt = input.Now
		if result, err := UpdateSandboxAgentStatus(agent, input); err != nil || result.Changed {
			t.Fatalf("fresh unchanged observation fabricated a status transition: %+v %v", result, err)
		}
		r = &AgentReconciler{Client: backing}
		agent.Status.Phase = api.PhaseReady
		if err := r.updateStatus(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	if len(capture.events) != 1 {
		t.Fatalf("restart or ordinary status writes repeated the event: %+v", capture.events)
	}
	replay := agent.DeepCopy()
	replay.ResourceVersion = "synthetic-different-storage-version"
	condition := apimeta.FindStatusCondition(replay.Status.Conditions, api.ConditionSandboxReady)
	condition.LastTransitionTime = metav1.NewTime(input.Now.Add(time.Hour))
	condition.Message = "synthetic-free-form-message"
	emitSandboxAgentTransition(ctx, base, replay)
	if len(capture.events) != 2 || capture.events[1].TransitionID != first {
		t.Fatal("the same semantic transition invented a new correlation ID")
	}
	input.Plan.Revision, input.Expected.Revision = strings.Repeat("c", 64), strings.Repeat("c", 64)
	input.Observation.ExecutionIdentity = input.Expected
	if _, err := UpdateSandboxAgentStatus(agent, input); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if len(capture.events) != 3 || capture.events[2].TransitionID == first {
		t.Fatal("a new verified revision at the same generation was not distinguishable")
	}
}

func TestSandboxEventsDoNotCommitOnStatusWriteFailure(t *testing.T) {
	for _, mode := range []string{"read error", "missing resource version", "stale resource version", "wrong UID", "CAS race", "write error"} {
		t.Run(mode, func(t *testing.T) {
			agent, input := sandboxEventAgentFixture(t)
			ctx, capture := captureSandboxEvents(t)
			backing := sandboxEventClient(t, agent)
			injected := errors.New("sk-dummy synthetic persistence failure")
			writes := 0
			writer := interceptor.NewClient(backing, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if mode == "read error" {
						return injected
					}
					return c.Get(ctx, key, object, options...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
					writes++
					if mode == "write error" {
						return injected
					}
					if mode == "CAS race" {
						concurrent := &api.Agent{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(object), concurrent); err != nil {
							t.Fatal(err)
						}
						concurrent.Annotations["example.com/concurrent"] = "synthetic"
						if err := c.Update(ctx, concurrent); err != nil {
							t.Fatal(err)
						}
					}
					return c.SubResource(subresource).Patch(ctx, object, patch, options...)
				},
			})
			r := &AgentReconciler{Client: writer, APIReader: writer}
			if _, err := UpdateSandboxAgentStatus(agent, input); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing resource version":
				agent.ResourceVersion = ""
			case "stale resource version":
				agent.ResourceVersion = "0"
			case "wrong UID":
				agent.UID = "00000000-0000-4000-8000-000000000099"
			}
			err := r.updateStatus(ctx, agent)
			if mode == "read error" || mode == "write error" {
				if !errors.Is(err, injected) {
					t.Fatalf("persistence error was hidden: %v", err)
				}
			} else if !apierrors.IsConflict(err) {
				t.Fatalf("expected a real optimistic conflict: %v", err)
			}
			if len(capture.events) != 0 {
				t.Fatal("an unsuccessful status write emitted a committed event")
			}
			if mode != "CAS race" && mode != "write error" && writes != 0 {
				t.Fatal("an invalid authoritative read still attempted a status write")
			}
			stored := &api.Agent{}
			if err := backing.Get(ctx, client.ObjectKeyFromObject(agent), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.Sandbox != nil {
				t.Fatal("failed write advanced the persisted applied binding")
			}
		})
	}
}

func TestSandboxEventsMissingReferencesAndOpaqueValues(t *testing.T) {
	for _, authorization := range []string{"absent", "malformed", "valid opaque IDs"} {
		t.Run(authorization, func(t *testing.T) {
			agent, input := sandboxEventAgentFixture(t)
			switch authorization {
			case "absent":
				agent.Annotations = nil
			case "malformed":
				agent.Annotations[sandboxbinding.AuthorizationAnnotation] = "sk-dummy authorization https://identity.example.com/"
			case "valid opaque IDs":
				agent.Annotations[sandboxbinding.AuthorizationAnnotation] = strings.ReplaceAll(
					agent.Annotations[sandboxbinding.AuthorizationAnnotation],
					string(input.Expected.Binding.TeamUID), "sk-dummy-opaque-team")
			}
			agent.Spec.User.DisplayName = "sk-dummy conversation text"
			agent.Spec.GatewayTokenSecretRef = "synthetic-secret-name"
			agent.Status.AccessURL = "https://agent.example.com/synthetic-private-endpoint"
			agent.Status.Sandbox = &api.SandboxStatus{
				TeamUID: "synthetic-historical-team", SandboxUID: "synthetic-historical-sandbox",
				AgentPolicyUID: "synthetic-historical-policy",
				AppliedRevision: "sk-dummy " + strings.Repeat("synthetic-opaque-revision", 200),
			}
			ctx, capture := captureSandboxEvents(t)
			backing := sandboxEventClient(t, agent)
			r := &AgentReconciler{Client: backing}
			input.Failure = fmt.Errorf("sk-dummy %s", agent.Status.AccessURL)
			if _, err := UpdateSandboxAgentStatus(agent, input); err == nil {
				t.Fatal("expected a sanitized failed status")
			}
			if err := r.updateStatus(ctx, agent); err != nil {
				t.Fatal(err)
			}
			event := capture.events[0]
			if event.AttemptedBinding.AgentUID != string(agent.UID) || event.AttemptedRevision != sandboxEventUnresolved {
				t.Fatal("missing attempt revision was filled from historical status")
			}
			if authorization == "valid opaque IDs" {
				if event.AttemptedBinding.TeamUID != "sha256:"+sandboxEventDigest("sk-dummy-opaque-team") ||
					event.AttemptedBindingSource != "authorization" {
					t.Fatal("known opaque authorization UID was neither safely fingerprinted nor attributed")
				}
			} else if event.AttemptedBinding.TeamUID != sandboxEventUnresolved ||
				event.AttemptedBinding.SandboxUID != sandboxEventUnresolved ||
				event.AttemptedBinding.AgentPolicyUID != sandboxEventUnresolved ||
				event.AttemptedBindingSource != sandboxEventUnresolved {
				t.Fatalf("missing authorization was fabricated from spec names or applied history: %+v", event)
			}
			if !strings.HasPrefix(event.AppliedRevision, "sha256:") || len(capture.raw[0]) > 2500 {
				t.Fatal("opaque revision was not bounded")
			}
			for _, secret := range []string{
				"sk-dummy", "example.com", "conversation text", "synthetic-secret-name", "synthetic-historical", "synthetic-opaque-revision",
			} {
				if strings.Contains(capture.raw[0], secret) {
					t.Fatalf("event leaked an opaque value or free-form content: %s", secret)
				}
			}
		})
	}
	agent, input := sandboxEventAgentFixture(t)
	agent.Annotations = nil
	input.Failure = sandbox.ErrEnforcementPending
	ctx, capture := captureSandboxEvents(t)
	r := &AgentReconciler{Client: sandboxEventClient(t, agent)}
	if _, err := UpdateSandboxAgentStatus(agent, input); err == nil {
		t.Fatal("pending enforcement did not hold")
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	event := capture.events[0]
	if event.Reason != "EnforcementPending" || event.Event != "sandbox.enforcement.failed" ||
		event.AppliedBinding != unresolvedSandboxEventBinding() || event.AppliedRevision != sandboxEventUnresolved {
		t.Fatal("unresolved identities received invented defaults")
	}
}

func TestSandboxEventsExcludeHeldAndUnverifiedApplication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.Agent)
	}{
		{"preparing", func(a *api.Agent) {
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "SandboxPreparationPending"
		}},
		{"prepared", func(a *api.Agent) {
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "SandboxPrepared"
		}},
		{"execution held", func(a *api.Agent) {
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "ExecutionHeld"
		}},
		{"suspended", func(a *api.Agent) {
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "Suspended"
		}},
		{"deleting", func(a *api.Agent) {
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "Deleting"
		}},
		{"legacy deletion reason", func(a *api.Agent) {
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "InstanceDeleting"
		}},
		{"unknown reason", func(a *api.Agent) {
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "sk-dummy-arbitrary-reason"
		}},
		{"stale condition", func(a *api.Agent) {
			apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady).ObservedGeneration--
		}},
		{"stale applied generation", func(a *api.Agent) { a.Status.ObservedGeneration-- }},
		{"missing applied", func(a *api.Agent) { a.Status.Sandbox = nil }},
		{"missing revision", func(a *api.Agent) { a.Status.Sandbox.AppliedRevision = "" }},
		{"wrong applied binding", func(a *api.Agent) { a.Status.Sandbox.TeamUID = "synthetic-unrelated" }},
		{"missing authorization", func(a *api.Agent) { a.Annotations = nil }},
		{"suspension with stale true condition", func(a *api.Agent) { a.Spec.Suspended = true }},
		{"suspension with stale failure condition", func(a *api.Agent) {
			a.Spec.Suspended = true
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "EnforcementLost"
		}},
		{"deletion with stale failure condition", func(a *api.Agent) {
			a.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
			c := apimeta.FindStatusCondition(a.Status.Conditions, api.ConditionSandboxReady)
			c.Status, c.Reason = metav1.ConditionFalse, "EnforcementLost"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, input := sandboxEventAgentFixture(t)
			if _, err := UpdateSandboxAgentStatus(agent, input); err != nil {
				t.Fatal(err)
			}
			tc.mutate(agent)
			desired := agent.DeepCopy()
			agent.Status = api.AgentStatus{}
			ctx, capture := captureSandboxEvents(t)
			r := &AgentReconciler{Client: sandboxEventClient(t, agent)}
			if err := r.updateStatus(ctx, desired); err != nil {
				t.Fatal(err)
			}
			if len(capture.events) != 0 {
				t.Fatalf("held, stale, or unbound status claimed a policy event: %+v", capture.events)
			}
		})
	}
}

func TestSandboxEventsProductionHoldAndPreparation(t *testing.T) {
	agent, _ := sandboxEventAgentFixture(t)
	ctx, capture := captureSandboxEvents(t)
	backing := sandboxEventClient(t, agent)
	r := &AgentReconciler{Client: backing, APIReader: backing}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
	for range 3 {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if len(capture.events) != 1 || capture.events[0].Event != "sandbox.enforcement.failed" ||
		capture.events[0].Reason != "UnsupportedControl" {
		t.Fatalf("production hold did not emit one committed failure: %+v", capture.events)
	}
	for _, prepared := range []bool{false, true, true} {
		if err := r.sandboxPreparationStatus(ctx, agent, prepared, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(capture.events) != 1 {
		t.Fatal("held preparation emitted applied or failed enforcement events")
	}
	stored := &api.Agent{}
	if err := backing.Get(ctx, request.NamespacedName, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Sandbox != nil || apimeta.IsStatusConditionTrue(stored.Status.Conditions, api.ConditionSandboxReady) {
		t.Fatal("event wiring loosened the execution hold")
	}
}

func TestSandboxEventsPreparationFailureRequiresCommittedStatus(t *testing.T) {
	for _, mode := range []string{"committed", "read error", "write error", "CAS conflict"} {
		t.Run(mode, func(t *testing.T) {
			agent, _ := sandboxEventAgentFixture(t)
			ctx, capture := captureSandboxEvents(t)
			backing := sandboxEventClient(t, agent)
			injected := errors.New("synthetic preparation status failure")
			reads, writes := 0, 0
			writer := interceptor.NewClient(backing, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					reads++
					if mode == "read error" {
						return injected
					}
					return c.Get(ctx, key, object, options...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
					writes++
					if len(capture.events) != 0 {
						t.Fatal("preparation emitted before status persistence or rewrote unchanged status")
					}
					if mode == "write error" {
						return injected
					}
					if mode == "CAS conflict" {
						concurrent := &api.Agent{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(object), concurrent); err != nil {
							t.Fatal(err)
						}
						concurrent.Annotations["example.com/concurrent"] = "synthetic"
						if err := c.Update(ctx, concurrent); err != nil {
							t.Fatal(err)
						}
					}
					err := c.SubResource(subresource).Patch(ctx, object, patch, options...)
					if len(capture.events) != 0 {
						t.Fatal("preparation emitted before the writer returned success")
					}
					return err
				},
			})
			r := &AgentReconciler{Client: writer, APIReader: writer}
			err := r.sandboxPreparationStatus(ctx, agent, false, sandboxpolicy.ErrUnapprovedGrant)
			stored := &api.Agent{}
			if getErr := backing.Get(ctx, client.ObjectKeyFromObject(agent), stored); getErr != nil {
				t.Fatal(getErr)
			}
			if mode != "committed" {
				if mode == "CAS conflict" && !apierrors.IsConflict(err) ||
					mode != "CAS conflict" && !errors.Is(err, injected) {
					t.Fatalf("preparation persistence error was hidden: %v", err)
				}
				if len(capture.events) != 0 || !equality.Semantic.DeepEqual(stored.Status, agent.Status) {
					t.Fatal("failed preparation persistence committed status or emitted an event")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := r.sandboxPreparationStatus(ctx, agent, false, sandboxpolicy.ErrUnapprovedGrant); err != nil {
					t.Fatal(err)
				}
			}
			if len(capture.events) != 1 || reads != 3 || writes != 1 {
				t.Fatalf("expected one committed preparation failure without extra operations: events=%d reads=%d writes=%d",
					len(capture.events), reads, writes)
			}
			event := capture.events[0]
			condition := apimeta.FindStatusCondition(stored.Status.Conditions, api.ConditionSandboxReady)
			if event.Event != "sandbox.enforcement.failed" || event.Reason != "UnauthorizedBinding" ||
				condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != event.Reason ||
				stored.Status.Sandbox != nil || stored.Status.AccessURL != "" ||
				!equality.Semantic.DeepEqual(stored.Spec, agent.Spec) || !reflect.DeepEqual(stored.Finalizers, agent.Finalizers) {
				t.Fatal("preparation event disagreed with the persisted hold or changed unrelated state")
			}
		})
	}
}

func TestSandboxEventsPreserveOrdinaryUngovernedStatusWrites(t *testing.T) {
	agent, _ := sandboxEventAgentFixture(t)
	agent.Spec.Team, agent.Spec.SandboxRef = "", ""
	agent.Annotations, agent.Finalizers = nil, nil
	ctx, capture := captureSandboxEvents(t)
	backing := sandboxEventClient(t, agent)
	reads, writes := 0, 0
	c := interceptor.NewClient(backing, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			reads++
			return c.Get(ctx, key, object, options...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			writes++
			return c.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	r := &AgentReconciler{Client: c, APIReader: c}
	agent.Status.Phase = api.PhaseReady
	agent.Status.AccessURL = "https://agent.example.com/"
	agent.Status.Conditions = []metav1.Condition{{
		Type: api.ConditionReady, Status: metav1.ConditionTrue, Reason: "HelmReady",
		LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)), ObservedGeneration: agent.Generation,
	}}
	expected := *agent.Status.DeepCopy()
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	stored := &api.Agent{}
	if err := backing.Get(ctx, client.ObjectKeyFromObject(agent), stored); err != nil {
		t.Fatal(err)
	}
	if len(capture.events) != 0 || reads != 1 || writes != 1 || !equality.Semantic.DeepEqual(stored.Status, expected) {
		t.Fatalf("ordinary status behavior or request count changed: events=%d reads=%d writes=%d\nstatus: %#v\nexpected: %#v",
			len(capture.events), reads, writes, stored.Status, expected)
	}
}
