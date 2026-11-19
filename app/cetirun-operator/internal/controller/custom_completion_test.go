// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type completionOperation struct {
	agentUID  types.UID
	operation controller.CustomApplyOperation
	done      bool
	outcome   controller.CustomApplyOutcome
}

// Models the provider's durable store, independently of the reconciler process.
type completionBackend struct {
	held                                      string
	operations                                map[string]*completionOperation
	approvals, resumes, completions, failures int
}

type recoveryProvider struct {
	customProvider
	store         *completionBackend
	beforeApprove func(controller.CustomApplyOperation)
}

type recoveryLease struct {
	store     *completionBackend
	agentUID  types.UID
	operation controller.CustomApplyOperation
}

func (p *recoveryProvider) ApproveApply(ctx context.Context, instance *api.Agent, current *appsv1.StatefulSet, operation controller.CustomApplyOperation) (controller.CustomApplyLease, error) {
	p.store.approvals++
	if p.beforeApprove != nil {
		p.beforeApprove(operation)
	}
	if p.store.held != "" {
		return nil, fmt.Errorf("previous coordination is still held")
	}
	p.store.operations[operation.ID] = &completionOperation{agentUID: instance.UID, operation: operation}
	p.store.held = operation.ID
	if p.approve != nil {
		if err := p.approve(current); err != nil {
			return nil, err
		}
	}
	return recoveryLease{p.store, instance.UID, operation}, nil
}

func (p *recoveryProvider) ResumeApply(_ context.Context, instance *api.Agent, operation controller.CustomApplyOperation) (controller.CustomApplyLease, error) {
	p.store.resumes++
	if previous := p.store.operations[operation.ID]; previous != nil &&
		(previous.agentUID != instance.UID || previous.operation != operation) {
		return nil, fmt.Errorf("operation identity mismatch")
	}
	return recoveryLease{p.store, instance.UID, operation}, nil
}

func (l recoveryLease) Check(context.Context) error {
	if l.store.held != l.operation.ID {
		return fmt.Errorf("lease is not held")
	}
	return nil
}

func (l recoveryLease) Complete(_ context.Context, outcome controller.CustomApplyOutcome) error {
	l.store.completions++
	if l.store.failures > 0 {
		l.store.failures--
		return fmt.Errorf("temporary completion-store failure")
	}
	previous := l.store.operations[l.operation.ID]
	if previous == nil {
		if outcome != controller.CustomApplyNotStarted {
			return fmt.Errorf("unknown operation cannot be reported as applied")
		}
		previous = &completionOperation{agentUID: l.agentUID, operation: l.operation}
		l.store.operations[l.operation.ID] = previous
	}
	if previous.done && previous.outcome != outcome {
		return fmt.Errorf("completion outcome changed")
	}
	previous.done, previous.outcome = true, outcome
	if l.store.held == l.operation.ID {
		l.store.held = ""
	}
	return nil
}

type completionClient struct {
	client.Client
	failCreate, failDelete bool
	failOutcome            controller.CustomApplyOutcome
}

func isCompletion(obj client.Object) bool {
	record, ok := obj.(*corev1.ConfigMap)
	return ok && record.Data["completion"] != ""
}

func (c *completionClient) Create(ctx context.Context, obj client.Object, options ...client.CreateOption) error {
	if c.failCreate && isCompletion(obj) {
		c.failCreate = false
		return fmt.Errorf("synthetic outbox create failure")
	}
	// The Gateway key is a versioned Custom binding. The API server allocates
	// its UID; mirror that behavior in this fake client before persisting it.
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("synthetic-" + obj.GetName()))
	}
	return c.Client.Create(ctx, obj, options...)
}

func (c *completionClient) Update(ctx context.Context, obj client.Object, options ...client.UpdateOption) error {
	if c.failOutcome != "" && isCompletion(obj) {
		var value struct{ Outcome controller.CustomApplyOutcome }
		_ = json.Unmarshal([]byte(obj.(*corev1.ConfigMap).Data["completion"]), &value)
		if value.Outcome == c.failOutcome {
			c.failOutcome = ""
			return fmt.Errorf("synthetic outcome write failure")
		}
	}
	return c.Client.Update(ctx, obj, options...)
}

func (c *completionClient) Delete(ctx context.Context, obj client.Object, options ...client.DeleteOption) error {
	if c.failDelete && isCompletion(obj) {
		c.failDelete = false
		return fmt.Errorf("synthetic outbox delete failure")
	}
	return c.Client.Delete(ctx, obj, options...)
}

type completionScenario struct {
	t          *testing.T
	client     *completionClient
	agent      *api.Agent
	provider   *recoveryProvider
	helm       *fakeHelmClient
	reconciler *controller.AgentReconciler
}

// requireLegacyCustomRuntime identifies compatibility coverage for Custom
// Agents that have not opted into Sandbox or AgentPolicy execution.
func requireLegacyCustomRuntime(t *testing.T) {
	t.Helper()
}

func newCompletionScenario(t *testing.T) *completionScenario {
	t.Helper()
	instance, bindings, objects := customFixture("completion", nil)
	scheme := testScheme()
	c := &completionClient{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
	p := &recoveryProvider{customProvider: customProvider{bindings: bindings},
		store: &completionBackend{operations: map[string]*completionOperation{}}}
	s := &completionScenario{t: t, client: c, agent: instance, provider: p, helm: &fakeHelmClient{useHarness: true}}
	s.restart()
	return s
}

func (s *completionScenario) restart() {
	previous := s.reconciler
	s.reconciler = &controller.AgentReconciler{Client: s.client, Scheme: testScheme(),
		HelmClient: s.helm, CustomBindings: s.provider, EventRecorder: record.NewFakeRecorder(100)}
	if previous == nil {
		controller.ConfigureTestGateway(s.t, s.reconciler)
	} else {
		controller.ReuseTestGateway(s.t, previous, s.reconciler)
	}
}

func (s *completionScenario) step() error {
	_, err := s.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: s.agent.Name}})
	return err
}

func (s *completionScenario) current() *api.Agent {
	value := &api.Agent{}
	if err := s.client.Get(context.Background(), types.NamespacedName{Name: s.agent.Name}, value); err != nil {
		s.t.Fatal(err)
	}
	return value
}

func (s *completionScenario) pending() *corev1.ConfigMap {
	list := &corev1.ConfigMapList{}
	if err := s.client.List(context.Background(), list, client.InNamespace("cetirun-system")); err != nil {
		s.t.Fatal(err)
	}
	for i := range list.Items {
		if isCompletion(&list.Items[i]) {
			return &list.Items[i]
		}
	}
	return nil
}

func (s *completionScenario) observeApplied() {
	sts := statefulSet(s.agent.Name, 1)
	sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: s.agent.Name, UID: s.agent.UID}}
	sts.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": string(s.agent.UID),
		"cetirun.microsoft.com/custom-plan-hash": s.helm.lastValues["runtime"].(map[string]any)["workloadHash"].(string)}
	markGatewayCustomWorkload(sts, s.reconciler.AIGateway.URL)
	sts.Status.CurrentRevision, sts.Status.UpdateRevision = "applied", "applied"
	if err := s.client.Create(context.Background(), sts); err != nil {
		s.t.Fatal(err)
	}
}

// markGatewayCustomWorkload represents an already applied Custom template in
// tests that need to observe it after Gateway-only migration.
func markGatewayCustomWorkload(sts *appsv1.StatefulSet, gatewayURL string) {
	sts.Spec.Template.Spec.Containers = []corev1.Container{{Env: []corev1.EnvVar{
		{Name: "OPENAI_BASE_URL", Value: gatewayURL + "/v1"},
		{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "ai-gateway-virtual-key"}, Key: "api-key"}}},
	}}}
}

func TestCustomCompletionRetriesAfterRestartWithoutReapplying(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.provider.store.failures = 1
	if err := s.step(); err == nil || s.pending() == nil || !s.helm.installCalled {
		t.Fatal("expected a successful apply and durable failed completion")
	}
	operationID := s.provider.store.held
	s.observeApplied()
	s.helm.installCalled = false
	s.restart()
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if s.helm.installCalled || s.provider.store.approvals != 1 || s.provider.store.completions != 2 ||
		s.provider.store.held != "" || s.pending() != nil || s.current().Status.Phase != api.PhaseReady {
		t.Fatal("completion was not recovered without another handoff/apply")
	}
	current := s.current()
	current.Spec.Agent.Port = ptr.To[int32](8080)
	current.Generation++
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	s.provider.bindings.Generation = current.Generation
	s.provider.approve = func(sts *appsv1.StatefulSet) error {
		sts.Spec.Replicas = ptr.To[int32](0)
		return s.client.Update(context.Background(), sts)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if !s.helm.installCalled || s.provider.store.approvals != 2 || len(s.provider.store.operations) != 2 ||
		!s.provider.store.operations[operationID].done {
		t.Fatal("completed coordination still blocked a later valid update")
	}
}

func TestCustomCompletionOutboxFailureWindows(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, failure := range []string{"create", "before-apply", "after-apply", "delete-after-ack", "approve-response"} {
		t.Run(failure, func(t *testing.T) {
			s := newCompletionScenario(t)
			switch failure {
			case "create":
				s.client.failCreate = true
			case "before-apply":
				s.client.failOutcome = controller.CustomApplyUnknown
			case "after-apply":
				s.client.failOutcome = controller.CustomApplySucceeded
			case "delete-after-ack":
				s.client.failDelete = true
			case "approve-response":
				s.provider.approve = func(*appsv1.StatefulSet) error { return fmt.Errorf("approval response lost") }
			}
			s.provider.beforeApprove = func(operation controller.CustomApplyOperation) {
				record := s.pending()
				if record == nil || !strings.Contains(record.Data["completion"], operation.ID) {
					t.Fatal("approval preceded durable operation identity")
				}
			}
			_ = s.step()
			if failure == "create" || failure == "before-apply" || failure == "approve-response" {
				if s.helm.installCalled || s.provider.store.held != "" {
					t.Fatal("failed pre-apply checkpoint caused application or stranded coordination")
				}
				if failure == "create" && s.provider.store.approvals != 0 {
					t.Fatal("outbox creation failure reached approval")
				}
				return
			}
			if !s.helm.installCalled || s.pending() == nil {
				t.Fatal("post-apply failure did not retain the outbox")
			}
			s.observeApplied()
			s.helm.installCalled = false
			s.restart()
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			if s.helm.installCalled || s.provider.store.approvals != 1 || s.pending() != nil || s.provider.store.held != "" {
				t.Fatal("post-apply recovery replayed work or lost completion")
			}
		})
	}
}

func TestCustomCompletionRecoveryIsIndependentOfNewConfiguration(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.provider.store.failures = 1
	_ = s.step()
	s.reconciler.CetiRunHelmValues = map[string]any{"runtimeClassName": "INVALID_CLASS"}
	s.helm.installCalled = false
	_ = s.step()
	rejected := false
	for _, condition := range s.current().Status.Conditions {
		if condition.Type == api.ConditionAgentConfigReady && condition.Status == metav1.ConditionFalse && condition.Reason == "AgentConfigInvalid" {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("invalid desired configuration was not reported")
	}
	if s.provider.store.held != "" || s.pending() != nil || s.helm.installCalled || s.provider.store.approvals != 1 {
		t.Fatal("new invalid configuration blocked completion recovery")
	}
}

func TestCustomPendingCompletionPreservesSuspendAndDeletion(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, deleting := range []bool{false, true} {
		s := newCompletionScenario(t)
		s.provider.store.failures = 2
		_ = s.step()
		s.observeApplied()
		current := s.current()
		if deleting {
			if err := s.client.Delete(context.Background(), current); err != nil {
				t.Fatal(err)
			}
		} else {
			current.Spec.Suspended = true
			current.Generation++
			if err := s.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
		}
		_ = s.step()
		if s.pending() == nil {
			t.Fatal("unacknowledged completion was discarded")
		}
		if deleting {
			ns := &corev1.Namespace{}
			if err := s.client.Get(context.Background(), types.NamespacedName{Name: s.provider.bindings.Namespace.Name}, ns); err != nil {
				t.Fatal("cleanup ran before completion acknowledgement")
			}
		} else {
			sts := &appsv1.StatefulSet{}
			if err := s.client.Get(context.Background(), types.NamespacedName{Namespace: s.provider.bindings.Namespace.Name,
				Name: "openclaw-" + s.agent.Name + "-cetirun"}, sts); err != nil || *sts.Spec.Replicas != 0 ||
				s.current().Status.Phase != api.PhaseSuspended {
				t.Fatal("pending completion prevented suspension")
			}
		}
		s.restart()
		if err := s.step(); err != nil {
			t.Fatal(err)
		}
		if s.pending() != nil || s.provider.store.held != "" {
			t.Fatal("lifecycle path did not recover completion")
		}
		if deleting {
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			if err := s.client.Get(context.Background(), types.NamespacedName{Name: s.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("deletion did not finalize after acknowledgement")
			}
		}
	}
}

func TestCustomForeignCompletionRecordCannotBeDelivered(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.provider.store.failures = 1
	_ = s.step()
	record := s.pending()
	if record == nil {
		t.Fatal("missing outbox")
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(record.Data["completion"]), &value); err != nil {
		t.Fatal(err)
	}
	value["agentUID"] = "another-agent-uid"
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	record.Data["completion"] = string(data)
	if err := s.client.Update(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	s.helm.installCalled = false
	_ = s.step()
	if s.provider.store.resumes != 0 || s.provider.store.completions != 1 || s.helm.installCalled || s.pending() == nil {
		t.Fatal("foreign outbox was resumed, removed or allowed new work")
	}
}
