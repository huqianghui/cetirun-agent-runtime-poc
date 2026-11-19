// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

type sandboxFinalizerPatchClient struct {
	client.Client
	patches int
}

func (c *sandboxFinalizerPatchClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if _, ok := object.(*cetirunv1alpha1.Agent); ok {
		return fmt.Errorf("Sandbox finalizers must not rewrite the complete Agent")
	}
	return c.Client.Update(ctx, object, options...)
}

func (c *sandboxFinalizerPatchClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if _, ok := object.(*cetirunv1alpha1.Agent); ok {
		raw, err := patch.Data(object)
		if err != nil {
			return err
		}
		var body struct {
			Metadata struct {
				ResourceVersion string   `json:"resourceVersion"`
				Finalizers      []string `json:"finalizers"`
			} `json:"metadata"`
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return err
		}
		if len(fields) != 1 || fields["metadata"] == nil ||
			body.Metadata.ResourceVersion == "" || body.Metadata.ResourceVersion != object.GetResourceVersion() {
			return fmt.Errorf("Sandbox finalizer patch is not metadata-only and optimistic")
		}
		c.patches++
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func TestSandboxFinalizersUseOptimisticMetadataOnlyPatches(t *testing.T) {
	for _, typ := range []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeHermes, cetirunv1alpha1.AgentTypeCustom} {
		t.Run(string(typ), func(t *testing.T) {
			ctx := context.Background()
			agent := newCR("patch-example", cetirunv1alpha1.PhasePending, nil, 1, 0, "")
			agent.Spec.SandboxRef, agent.Spec.Agent.Type = "standard", typ
			agent.Finalizers = append(agent.Finalizers, "example.com/retain")
			backing := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(agent).Build()
			writer := &sandboxFinalizerPatchClient{Client: backing}
			reconciler := &controller.AgentReconciler{Client: writer, EventRecorder: record.NewFakeRecorder(10)}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
			if _, err := reconciler.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			current := &cetirunv1alpha1.Agent{}
			if err := backing.Get(ctx, request.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(current.Finalizers, sandbox.HoldFinalizer) || writer.patches != 1 {
				t.Fatal("hold did not persist through a metadata-only patch")
			}
			if err := backing.Delete(ctx, current); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			if err := backing.Get(ctx, request.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if writer.patches != 2 || !slices.Equal(current.Finalizers, []string{"example.com/retain"}) ||
				current.Spec.SandboxRef != "standard" {
				t.Fatal("finalizer cleanup rewrote policy or removed a foreign finalizer")
			}
		})
	}
}

func TestSandboxOptInDoesNotRunLegacyHelm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cetirunv1alpha1.Agent, *cetirunv1alpha1.Team)
	}{
		{"selected Sandbox", func(a *cetirunv1alpha1.Agent, _ *cetirunv1alpha1.Team) { a.Spec.SandboxRef = "standard" }},
		{"governed Team", func(_ *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team) {
			team.Spec.AgentPolicyRef = "normal"
			team.Spec.SandboxRefs = []string{"standard"}
			team.Spec.DefaultSandboxRef = "standard"
		}},
		{"partial Team binding", func(_ *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team) {
			team.Spec.AgentPolicyRef = "operations"
		}},
		{"prior applied binding", func(a *cetirunv1alpha1.Agent, _ *cetirunv1alpha1.Team) {
			a.Status.Sandbox = &cetirunv1alpha1.SandboxStatus{AppliedRevision: "previous"}
		}},
		{"sticky hold after fields cleared", func(a *cetirunv1alpha1.Agent, _ *cetirunv1alpha1.Team) {
			a.Status.Conditions = []metav1.Condition{{Type: cetirunv1alpha1.ConditionSandboxReady, Status: metav1.ConditionFalse}}
		}},
		{"authorization metadata without selection", func(a *cetirunv1alpha1.Agent, _ *cetirunv1alpha1.Team) {
			a.Annotations = map[string]string{"cetirun.microsoft.com/sandbox-authorization": ""}
		}},
		{"reauthorization metadata without selection", func(a *cetirunv1alpha1.Agent, _ *cetirunv1alpha1.Team) {
			a.Annotations = map[string]string{"cetirun.microsoft.com/sandbox-reauthorize": "malformed"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newCR("alice-work", cetirunv1alpha1.PhasePending, nil, 1, 0, "")
			agent.Spec.Team = "engineering"
			team := &cetirunv1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering"}}
			tc.mutate(agent, team)
			client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(agent, team).Build()
			helm := &fakeHelmClient{}
			reconciler := &controller.AgentReconciler{Client: client, HelmClient: helm, EventRecorder: record.NewFakeRecorder(10)}
			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}})
			if err != nil || result.RequeueAfter <= 0 {
				t.Fatalf("hold should be explicit and retriable: result=%+v err=%v", result, err)
			}
			if helm.installCalled || helm.prepareCalls != 0 {
				t.Fatal("sandbox-bound Agent reached runtime preparation or application")
			}
			actual := &cetirunv1alpha1.Agent{}
			if err := client.Get(context.Background(), types.NamespacedName{Name: agent.Name}, actual); err != nil {
				t.Fatal(err)
			}
			condition := findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "UnsupportedControl" ||
				condition.ObservedGeneration != agent.Generation {
				t.Fatalf("expected explicit unsupported, unready state: %+v", condition)
			}
			if actual.Status.Phase != cetirunv1alpha1.PhaseFailed || actual.Status.AccessURL != "" ||
				findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionReady) == nil ||
				findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionReady).Status != metav1.ConditionFalse {
				t.Fatalf("hold advertised success: %+v", actual.Status)
			}
			if (actual.Status.Sandbox == nil) != (agent.Status.Sandbox == nil) {
				t.Fatal("hold must not advance applied state")
			}
			if err := client.Get(context.Background(), types.NamespacedName{Name: "cetirun-" + agent.Name}, instanceNamespace(agent.Name)); !apierrors.IsNotFound(err) {
				t.Fatalf("hold unexpectedly created a namespace: %v", err)
			}
		})
	}
}

func TestSandboxHoldStopsExistingWorkloadAndPreservesHome(t *testing.T) {
	agent := newCR("alice-work", cetirunv1alpha1.PhaseReady, []metav1.Condition{
		{Type: cetirunv1alpha1.ConditionReady, Status: metav1.ConditionTrue},
		{Type: cetirunv1alpha1.ConditionHelmDeployed, Status: metav1.ConditionTrue},
	}, 3, 2, "existing")
	agent.Spec.SandboxRef = "standard"
	agent.Status.AccessURL = "https://portal.example.com/ui/alice-work/"
	agent.Status.Sandbox = &cetirunv1alpha1.SandboxStatus{
		TeamUID: "team-uid", SandboxUID: "sandbox-uid",
		AgentPolicyUID: "policy-uid", AppliedRevision: "previous",
	}
	sts := statefulSet(agent.Name, 1)
	sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenScaled:  appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
		WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-" + agent.Name, Namespace: "cetirun-" + agent.Name}}
	ownSandboxResources(agent, sts, route)
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(agent, instanceNamespace(agent.Name), sts, route).Build()
	helm := &fakeHelmClient{}
	reconciler := &controller.AgentReconciler{Client: client, HelmClient: helm, EventRecorder: record.NewFakeRecorder(10)}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	actualSTS := &appsv1.StatefulSet{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, actualSTS); err != nil {
		t.Fatal(err)
	}
	if actualSTS.Spec.Replicas == nil || *actualSTS.Spec.Replicas != 0 ||
		actualSTS.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatal("hold must stop existing compute without deleting home PVCs")
	}
	if err := client.Get(context.Background(), types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatalf("held workload route was not removed: %v", err)
	}
	actual := &cetirunv1alpha1.Agent{}
	if err := client.Get(context.Background(), request.NamespacedName, actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.Sandbox.AppliedRevision != "previous" || actual.Status.ObservedGeneration != 2 ||
		findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionHelmDeployed) == nil || helm.installCalled {
		t.Fatal("hold changed applied state, dropped unrelated conditions, or ran Helm")
	}
	rv := actual.ResourceVersion
	transition := findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady).LastTransitionTime
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), request.NamespacedName, actual); err != nil {
		t.Fatal(err)
	}
	if rv != actual.ResourceVersion || !transition.Equal(&findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady).LastTransitionTime) {
		t.Fatal("stable hold must not repeatedly rewrite status or fabricate transitions")
	}
}

type sandboxHoldErrorClient struct {
	client.Client
	err       error
	operation string
}

func (c sandboxHoldErrorClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Namespace); ok && c.operation == "get namespace" {
		return c.err
	}
	if _, ok := object.(*appsv1.StatefulSet); ok && c.operation == "get workload" {
		return c.err
	}
	if _, ok := object.(*istiov1beta1.VirtualService); ok && c.operation == "get route" {
		return c.err
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c sandboxHoldErrorClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if _, ok := object.(*appsv1.StatefulSet); ok && c.operation == "patch workload" {
		return c.err
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c sandboxHoldErrorClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if _, ok := object.(*istiov1beta1.VirtualService); ok && c.operation == "delete route" {
		return c.err
	}
	return c.Client.Delete(ctx, object, options...)
}

func (c sandboxHoldErrorClient) Status() client.SubResourceWriter {
	if c.operation == "update status" {
		return sandboxHoldStatusErrorWriter{SubResourceWriter: c.Client.Status(), err: c.err}
	}
	return c.Client.Status()
}

type sandboxHoldStatusErrorWriter struct {
	client.SubResourceWriter
	err error
}

func (w sandboxHoldStatusErrorWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return w.err
}

func TestSandboxHoldFailureStaysVisible(t *testing.T) {
	t.Skip("obsolete pre-Policy fixture: mandatory Policy is tested by the governed hold suite")
	for _, operation := range []string{"get namespace", "get workload", "patch workload", "get route", "delete route", "update status"} {
		t.Run(operation, func(t *testing.T) {
			agent := newCR("alice-work", cetirunv1alpha1.PhaseReady, nil, 2, 1, "")
			agent.Spec.SandboxRef = "standard"
			sts := statefulSet(agent.Name, 1)
			route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-" + agent.Name, Namespace: "cetirun-" + agent.Name}}
			ownSandboxResources(agent, sts, route)
			fakeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(agent, instanceNamespace(agent.Name), sts, route).Build()
			failure := errors.New("synthetic hold failure")
			helm := &fakeHelmClient{}
			reconciler := &controller.AgentReconciler{
				Client: sandboxHoldErrorClient{Client: fakeClient, err: failure, operation: operation}, HelmClient: helm,
				EventRecorder: record.NewFakeRecorder(10),
			}
			key := types.NamespacedName{Name: agent.Name}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); !errors.Is(err, failure) {
				t.Fatalf("failure was hidden: %v", err)
			}
			if helm.installCalled {
				t.Fatal("hold failure must not fall back to legacy Helm")
			}
			actual := &cetirunv1alpha1.Agent{}
			if err := fakeClient.Get(context.Background(), key, actual); err != nil {
				t.Fatal(err)
			}
			if operation != "update status" {
				condition := findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady)
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "PolicyApplyFailed" {
					t.Fatalf("failure was reported as successful enforcement: %+v", condition)
				}
			}
			if operation != "get namespace" && operation != "get route" && operation != "delete route" {
				if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
					t.Fatalf("route removal was skipped after failure: %v", err)
				}
			}
		})
	}
}

func TestSandboxHoldRetainsHomeForAlreadyScaledWorkload(t *testing.T) {
	agent := newCR("alice-work", cetirunv1alpha1.PhaseSuspended, nil, 1, 1, "")
	agent.Spec.SandboxRef = "standard"
	sts := statefulSetSuspended(agent.Name)
	sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenScaled: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
	}
	ownSandboxResources(agent, sts)
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(agent, instanceNamespace(agent.Name), sts).Build()
	reconciler := &controller.AgentReconciler{Client: client}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatal("already scaled workload still requests PVC deletion")
	}
}

func TestSandboxNormalDefaultPreservesLegacyReconciliation(t *testing.T) {
	agent := newCR("alice-work", cetirunv1alpha1.PhasePending, nil, 1, 0, "")
	agent.Spec.Agent.Image, agent.Spec.Agent.Port = nil, nil
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).
		WithObjects(agent, instanceNamespace(agent.Name), gatewayTokenSecret(agent.Name), statefulSet(agent.Name, 0)).Build()
	helm := &fakeHelmClient{useHarness: true}
	reconciler := &controller.AgentReconciler{
		Client: client, Scheme: scheme, HelmClient: helm, EventRecorder: record.NewFakeRecorder(10),
		OperatorNamespace: "cetirun-system", OperatorSAName: "cetirun-operator-controller-manager",
		ChartVersion: "0.1.28", DomainSuffix: "example.com", DefaultModel: "test-model",
	}
	controller.ConfigureTestGateway(t, reconciler)
	key := types.NamespacedName{Name: agent.Name}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), key, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.Agent.Image == nil || agent.Spec.Agent.Port == nil || helm.installCalled {
		t.Fatal("unbound built-in must pin its Harness defaults before deployment")
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if !helm.installCalled {
		t.Fatal("default class alone should not hold a legacy unbound Agent")
	}
	if err := client.Get(context.Background(), key, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Status.Sandbox != nil || findCondition(agent.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady) != nil {
		t.Fatal("legacy reconciliation fabricated Sandbox enforcement status")
	}
}

func TestSandboxHoldCustomRuntimePreservesOwnership(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "foreign"}[foreign], func(t *testing.T) {
			agent, bindings, objects := customFixture("sandbox-custom", nil)
			agent.Spec.SandboxRef = "standard"
			sts := statefulSet(agent.Name, 1)
			owner := metav1.OwnerReference{
				APIVersion: cetirunv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID,
			}
			if foreign {
				owner.UID = "another-agent-uid"
			}
			sts.OwnerReferences = []metav1.OwnerReference{owner}
			sts.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": string(owner.UID)}
			route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{
				Name: "cetirun-" + agent.Name, Namespace: bindings.Namespace.Name,
				UID: "route-uid", OwnerReferences: []metav1.OwnerReference{owner},
			}}
			objects = append(objects, sts, route)
			client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(objects...).Build()
			helm := &fakeHelmClient{useHarness: true}
			reconciler := &controller.AgentReconciler{Client: client, HelmClient: helm}
			key := types.NamespacedName{Name: agent.Name}
			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if (err != nil) != foreign {
				t.Fatalf("foreign=%v, unexpected hold result: %v", foreign, err)
			}
			if helm.prepareCalls != 0 || helm.installCalled {
				t.Fatal("Custom Sandbox hold must not prepare or apply a runtime")
			}
			actual := &cetirunv1alpha1.Agent{}
			if err := client.Get(context.Background(), key, actual); err != nil {
				t.Fatal(err)
			}
			condition := findCondition(actual.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady)
			wantReason := "UnsupportedControl"
			wantReplicas := int32(0)
			if foreign {
				wantReason = "PolicyApplyFailed"
				wantReplicas = 1
			}
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != wantReason {
				t.Fatalf("unexpected Sandbox hold status: %+v", condition)
			}
			if err := client.Get(context.Background(), types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, sts); err != nil ||
				*sts.Spec.Replicas != wantReplicas || sts.OwnerReferences[0] != owner {
				t.Fatalf("Custom workload ownership or stop request changed unexpectedly: %v", err)
			}
			err = client.Get(context.Background(), types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, route)
			if (foreign && err != nil) || (!foreign && !apierrors.IsNotFound(err)) {
				t.Fatalf("Custom route ownership was not respected: %v", err)
			}
			home := &corev1.PersistentVolumeClaim{}
			if err := client.Get(context.Background(), types.NamespacedName{Name: bindings.Home.Name, Namespace: bindings.Namespace.Name}, home); err != nil ||
				string(home.UID) != bindings.Home.Name+"-uid" {
				t.Fatalf("Custom hold must retain external HOME: %v", err)
			}
		})
	}
}

func TestSandboxHoldRecoversCustomCompletionWithoutReapplying(t *testing.T) {
	t.Skip("Custom harnesses cannot receive a Policy policy")
	for _, teamBinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "Agent binding", true: "Team binding"}[teamBinding], func(t *testing.T) {
			scenario := newCompletionScenario(t)
			scenario.provider.store.failures = 2
			if err := scenario.step(); err == nil || scenario.pending() == nil {
				t.Fatal("fixture must have a pending Custom completion")
			}
			scenario.observeApplied()
			current := scenario.current()
			if teamBinding {
				team := &cetirunv1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering"},
					Spec: cetirunv1alpha1.TeamSpec{AgentPolicyRef: "normal"}}
				if err := scenario.client.Create(context.Background(), team); err != nil {
					t.Fatal(err)
				}
				current.Spec.Team = team.Name
			} else {
				current.Spec.SandboxRef = "standard"
			}
			current.Generation++
			if err := scenario.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			scenario.helm.installCalled = false
			prepareCalls := scenario.helm.prepareCalls
			if err := scenario.step(); err == nil {
				t.Fatal("pending completion failure must remain visible during the hold")
			}
			condition := findCondition(scenario.current().Status.Conditions, cetirunv1alpha1.ConditionSandboxReady)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "PolicyApplyFailed" {
				t.Fatalf("completion recovery bypassed the Sandbox hold: %+v", condition)
			}
			sts := &appsv1.StatefulSet{}
			key := types.NamespacedName{Name: "openclaw-" + current.Name + "-cetirun", Namespace: "cetirun-" + current.Name}
			if err := scenario.client.Get(context.Background(), key, sts); err != nil || *sts.Spec.Replicas != 0 || scenario.pending() == nil {
				t.Fatalf("pending completion prevented the stop or was discarded: %v", err)
			}
			scenario.restart()
			if err := scenario.step(); err != nil {
				t.Fatal(err)
			}
			if scenario.pending() != nil || scenario.provider.store.held != "" || scenario.provider.store.approvals != 1 ||
				scenario.helm.prepareCalls != prepareCalls || scenario.helm.installCalled ||
				scenario.current().Status.Phase != cetirunv1alpha1.PhaseFailed {
				t.Fatal("hold failed to recover completion without preparing or restarting the runtime")
			}
		})
	}
}

func TestSandboxUngovernedCustomSuspensionDoesNotRequireTeam(t *testing.T) {
	agent, _, objects := customFixture("sandbox-suspended", nil)
	agent.Spec.Team = "unavailable-team"
	agent.Spec.Suspended = true
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(objects...).Build()
	helm := &fakeHelmClient{useHarness: true}
	reconciler := &controller.AgentReconciler{Client: client, HelmClient: helm}
	key := types.NamespacedName{Name: agent.Name}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), key, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Status.Phase != cetirunv1alpha1.PhaseSuspended || helm.prepareCalls != 0 || helm.installCalled ||
		findCondition(agent.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady) != nil {
		t.Fatal("policy lookup blocked or changed ordinary Custom suspension")
	}
}

type sandboxHoldDeletionHelmClient struct {
	fakeHelmClient
	uninstallCalled bool
}

func (h *sandboxHoldDeletionHelmClient) Uninstall(string, string) error {
	h.uninstallCalled = true
	return nil
}

func TestSandboxHoldUsesOwnershipSafeDeletion(t *testing.T) {
	agent := newCR("alice-work", cetirunv1alpha1.PhaseFailed, nil, 1, 0, "")
	agent.Spec.SandboxRef = "standard"
	now := metav1.Now()
	agent.DeletionTimestamp = &now
	namespace := instanceNamespace(agent.Name)
	ownSandboxResources(agent, namespace)
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).
		WithObjects(agent, namespace).Build()
	helm := &sandboxHoldDeletionHelmClient{}
	reconciler := &controller.AgentReconciler{
		Client: client, Scheme: scheme, HelmClient: helm, EventRecorder: record.NewFakeRecorder(10),
	}
	key := types.NamespacedName{Name: agent.Name}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if helm.installCalled || helm.uninstallCalled {
		t.Fatal("sandbox-held deletion must not run Helm or uninstall hooks")
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	if err := client.Get(context.Background(), key, &cetirunv1alpha1.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deletion did not remove the existing finalizer: %v", err)
	}
}

func ownSandboxResources(agent *cetirunv1alpha1.Agent, objects ...client.Object) {
	if agent.UID == "" {
		agent.UID = types.UID(agent.Name + "-uid")
	}
	for _, object := range objects {
		object.SetUID(types.UID(object.GetName() + "-uid"))
		object.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: cetirunv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID,
		}})
	}
}
