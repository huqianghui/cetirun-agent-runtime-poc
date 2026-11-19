// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxSuspensionCompletesAfterPolicyRevokesItsKey(t *testing.T) {
	ctx := context.Background()
	f := readyPolicyRateLimitFixture(t, "openclaw", true)

	initial := f.current(t)
	keyID := initial.Status.AIGateway.KeyID
	generated := len(f.gateway.generated)
	team := &api.Team{}
	if err := f.kube.Get(ctx, types.NamespacedName{Name: initial.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	team.Spec.SandboxRefs = nil
	if err := f.kube.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	team.Status.ObservedGeneration = team.Generation
	for index := range team.Status.Conditions {
		if team.Status.Conditions[index].Type == api.ConditionTeamAccepted {
			team.Status.Conditions[index].ObservedGeneration = team.Generation
		}
	}
	if err := f.kube.Status().Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err != nil {
		t.Fatal("policy denial did not hold the running Agent:", err)
	}
	denied := f.current(t)
	if denied.Status.AIGateway == nil || denied.Status.AIGateway.Phase != api.AIGatewayPhaseDeleting ||
		denied.Status.AIGateway.KeyID != keyID || len(f.gateway.keys) != 0 {
		t.Fatalf("policy denial did not preserve the revoked-key identity: status=%+v keys=%d", denied.Status.AIGateway, len(f.gateway.keys))
	}

	denied.Spec.Suspended = true
	denied.Generation++
	if err := f.kube.Update(ctx, denied); err != nil {
		t.Fatal(err)
	}
	// The Deleting receipt must survive an operator restart: it is the only
	// proof that the next suspension pass should retry revocation, not recreate
	// the revoked key.
	f.r = restartEntraReconciler(f.r)
	if _, err := f.reconcile(); err != nil {
		t.Fatal("suspension did not confirm the prior revocation:", err)
	}
	current := f.current(t)
	if current.Status.Phase != api.PhaseSuspended || current.Status.AccessURL != "" ||
		current.Status.Policy == nil || current.Status.Policy.Phase != "Denied" {
		t.Fatalf("confirmed suspended denial was not retained: phase=%s access=%q policy=%+v", current.Status.Phase, current.Status.AccessURL, current.Status.Policy)
	}
	if current.Status.ObservedGeneration != current.Generation {
		t.Fatalf("confirmed suspension did not observe the current generation: observed=%d generation=%d", current.Status.ObservedGeneration, current.Generation)
	}
	for _, kind := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady, api.ConditionIngressReady} {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, kind)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "Suspended" ||
			condition.ObservedGeneration != current.Generation {
			t.Fatalf("confirmed suspension did not record current %s state: %+v", kind, condition)
		}
	}
	if condition := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionAIGatewayReady); condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != "Suspended" ||
		condition.ObservedGeneration != current.Generation {
		t.Fatalf("confirmed suspension did not record revoked Gateway state: %+v", condition)
	}
	if replicas := ptr.Deref(f.workload(t).Spec.Replicas, -1); replicas != 0 {
		t.Fatalf("confirmed suspension did not stop workload: replicas=%d", replicas)
	}
	if current.Status.AIGateway == nil || current.Status.AIGateway.KeyID != keyID ||
		current.Status.AIGateway.Phase != api.AIGatewayPhaseDeleting || len(f.gateway.generated) != generated || len(f.gateway.keys) != 0 {
		t.Fatalf("suspension recreated or forgot the revoked key: status=%+v generated=%d keys=%d", current.Status.AIGateway, len(f.gateway.generated), len(f.gateway.keys))
	}

	if _, err := f.reconcile(); err != nil {
		t.Fatal("repeated suspension did not retain the confirmed stop:", err)
	}
	if current = f.current(t); current.Status.Phase != api.PhaseSuspended || len(f.gateway.generated) != generated {
		t.Fatalf("repeated suspension changed the confirmed revoked lifetime: phase=%s generated=%d", current.Status.Phase, len(f.gateway.generated))
	}

	current.Spec.Suspended = false
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err != nil {
		t.Fatal("denied resume did not remain held:", err)
	}
	if current = f.current(t); current.Status.Phase == api.PhaseReady || len(f.gateway.generated) != generated || len(f.gateway.keys) != 0 {
		t.Fatalf("denied resume recreated or enabled the revoked key: phase=%s generated=%d keys=%d", current.Status.Phase, len(f.gateway.generated), len(f.gateway.keys))
	}

	if err := f.kube.Get(ctx, types.NamespacedName{Name: current.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	team.Spec.SandboxRefs = []string{testSandboxName}
	team.Spec.DefaultSandboxRef = testSandboxName
	if err := f.kube.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	team.Status.ObservedGeneration = team.Generation
	for index := range team.Status.Conditions {
		if team.Status.Conditions[index].Type == api.ConditionTeamAccepted {
			team.Status.Conditions[index].ObservedGeneration = team.Generation
		}
	}
	if err := f.kube.Status().Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err != nil {
		t.Fatal("policy recovery did not retain the revoked-key recovery hold:", err)
	}
	if current = f.current(t); current.Status.Phase == api.PhaseReady ||
		current.Status.AIGateway == nil || current.Status.AIGateway.KeyID != keyID ||
		len(f.gateway.generated) != generated || len(f.gateway.keys) != 0 {
		t.Fatalf("policy recovery recreated or forgot the revoked key: phase=%s status=%+v generated=%d keys=%d", current.Status.Phase, current.Status.AIGateway, len(f.gateway.generated), len(f.gateway.keys))
	}
}

func TestReconciledSandboxSuspensionStillRejectsAnUnrecordedMissingKey(t *testing.T) {
	ctx := context.Background()
	f := readyPolicyRateLimitFixture(t, "openclaw", true)

	current := f.current(t)
	generated := len(f.gateway.generated)
	for token := range f.gateway.keys {
		delete(f.gateway.keys, token)
	}
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("an unrecorded missing key completed suspension")
	}
	current = f.current(t)
	if current.Status.Phase != api.PhasePending || len(f.gateway.generated) != generated {
		t.Fatalf("an unrecorded missing key was not held fail-closed: phase=%s generated=%d", current.Status.Phase, len(f.gateway.generated))
	}
}

func TestCommonCustomDeniedSuspensionClearsAccessConditions(t *testing.T) {
	ctx := context.Background()
	f := customTerminalFixture(t, "kata")
	readyCustomWorkload(t, f)
	terminal := customTerminalDeployment(t, f)
	terminal.Status = appsv1.DeploymentStatus{
		ObservedGeneration: terminal.Generation, Replicas: 1, ReadyReplicas: 1,
		UpdatedReplicas: 1, AvailableReplicas: 1,
	}
	if err := f.kube.Status().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.reconcileOK(t)
	}
	current := f.current(t)
	if current.Status.Phase != api.PhaseReady ||
		!apimeta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionTerminalConfigured) ||
		!apimeta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionAIGatewayReady) {
		t.Fatalf("Custom fixture did not reach actual Ready state: %+v", current.Status)
	}
	// Desktop is disabled by this current Custom fixture, but an older
	// configuration can have left its successful condition in status. Verify
	// the denied-suspension path withdraws that stale access receipt too.
	setCondition(&current.Status.Conditions, api.ConditionDesktopConfigured, metav1.ConditionTrue,
		"HistoricalDesktopConfigured", "Synthetic prior desktop configuration")
	for index := range current.Status.Conditions {
		if current.Status.Conditions[index].Type == api.ConditionDesktopConfigured {
			current.Status.Conditions[index].ObservedGeneration = current.Generation
		}
	}
	if err := f.kube.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if condition := apimeta.FindStatusCondition(f.current(t).Status.Conditions, api.ConditionDesktopConfigured); condition == nil ||
		condition.Status != metav1.ConditionTrue || condition.Reason != "HistoricalDesktopConfigured" {
		t.Fatalf("Custom fixture did not retain the seeded historical desktop state: %+v", condition)
	}

	team := &api.Team{}
	if err := f.kube.Get(ctx, types.NamespacedName{Name: current.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	team.Spec.SandboxRefs = nil
	if err := f.kube.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	team.Status.ObservedGeneration = team.Generation
	for index := range team.Status.Conditions {
		if team.Status.Conditions[index].Type == api.ConditionTeamAccepted {
			team.Status.Conditions[index].ObservedGeneration = team.Generation
		}
	}
	if err := f.kube.Status().Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("denied Custom Agent did not wait for workload shutdown")
	}
	main := f.workload(t)
	main.Status = appsv1.StatefulSetStatus{ObservedGeneration: main.Generation}
	if err := f.kube.Status().Update(ctx, main); err != nil {
		t.Fatal(err)
	}
	terminal = customTerminalDeployment(t, f)
	terminal.Status = appsv1.DeploymentStatus{ObservedGeneration: terminal.Generation}
	if err := f.kube.Status().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}

	current = f.current(t)
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	current = f.current(t)
	if current.Status.Phase != api.PhaseSuspended || current.Status.ObservedGeneration != current.Generation {
		t.Fatalf("denied Custom suspension was not confirmed at its current generation: %+v", current.Status)
	}
	for _, kind := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady,
		api.ConditionIngressReady, api.ConditionAIGatewayReady, api.ConditionDesktopConfigured, api.ConditionTerminalConfigured} {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, kind)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "Suspended" ||
			condition.ObservedGeneration != current.Generation {
			t.Fatalf("denied Custom suspension left stale %s state: %+v", kind, condition)
		}
	}
}

func TestReconciledSandboxSuspensionRetriesRecordedGatewayRevocation(t *testing.T) {
	ctx := context.Background()
	f := readyPolicyRateLimitFixture(t, "openclaw", true)

	current := f.current(t)
	current.Status.AIGateway.Phase = api.AIGatewayPhaseDeleting
	if err := f.kube.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	current = f.current(t)
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	f.gateway.deleteErr = errors.New("synthetic Gateway revoke failure")
	if _, err := f.reconcile(); err == nil {
		t.Fatal("an unconfirmed recorded revocation completed suspension")
	}
	current = f.current(t)
	if current.Status.Phase != api.PhasePending || current.Status.AIGateway.KeyID == "" {
		t.Fatalf("failed recorded revocation was not retained pending: phase=%s status=%+v", current.Status.Phase, current.Status.AIGateway)
	}
	if replicas := ptr.Deref(f.workload(t).Spec.Replicas, -1); replicas != 0 {
		t.Fatalf("Gateway revoke failure skipped independent workload stop: replicas=%d", replicas)
	}
}

func TestPolicyDeniedSuspensionWaitsForGatewayRevocation(t *testing.T) {
	ctx := context.Background()
	f := readyPolicyRateLimitFixture(t, "openclaw", true)

	current := f.current(t)
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	current = f.current(t)
	failure := errors.New("synthetic Gateway revoke failure")
	f.gateway.deleteErr = failure
	if _, err := f.r.holdPolicyDenied(ctx, current); !errors.Is(err, failure) {
		t.Fatalf("Gateway revoke failure was hidden: %v", err)
	}

	current = f.current(t)
	if current.Status.Phase != api.PhasePending || current.Status.AIGateway == nil ||
		current.Status.AIGateway.Phase != api.AIGatewayPhaseDeleting || current.Status.AIGateway.KeyID == "" {
		t.Fatalf("failed policy revocation did not remain pending with its identity: phase=%s gateway=%+v", current.Status.Phase, current.Status.AIGateway)
	}
	if len(f.gateway.keys) != 1 || len(f.gateway.generated) != 1 {
		t.Fatalf("failed policy revocation changed the Gateway lifetime: keys=%d generated=%d", len(f.gateway.keys), len(f.gateway.generated))
	}
	if replicas := ptr.Deref(f.workload(t).Spec.Replicas, -1); replicas != 0 {
		t.Fatalf("Gateway revoke failure skipped independent workload stop: replicas=%d", replicas)
	}
}

func TestPolicyDeniedSuspensionWaitsForWorkloadStop(t *testing.T) {
	ctx := context.Background()
	f := readyPolicyRateLimitFixture(t, "openclaw", true)

	current := f.current(t)
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	current = f.current(t)
	failure := errors.New("synthetic workload stop failure")
	f.r.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, ok := object.(*appsv1.StatefulSet); ok {
				return failure
			}
			return c.Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.r.holdPolicyDenied(ctx, current); !errors.Is(err, failure) {
		t.Fatalf("workload stop failure was hidden: %v", err)
	}

	current = f.current(t)
	if current.Status.Phase != api.PhasePending || current.Status.AIGateway == nil ||
		current.Status.AIGateway.Phase != api.AIGatewayPhaseDeleting || current.Status.AIGateway.KeyID == "" {
		t.Fatalf("failed workload stop incorrectly completed suspension: phase=%s gateway=%+v", current.Status.Phase, current.Status.AIGateway)
	}
	condition := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionPodReady)
	if current.Status.ObservedGeneration != current.Generation || condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != "SuspensionPending" ||
		condition.ObservedGeneration != current.Generation {
		t.Fatalf("failed workload stop did not record current pending stop state: observed=%d generation=%d pod=%+v", current.Status.ObservedGeneration, current.Generation, condition)
	}
	if gateway := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionAIGatewayReady); gateway == nil ||
		gateway.Status != metav1.ConditionFalse || gateway.Reason != "SuspensionPending" ||
		gateway.ObservedGeneration != current.Generation {
		t.Fatalf("failed workload stop did not record the completed key revocation: %+v", gateway)
	}
	if len(f.gateway.keys) != 0 || len(f.gateway.generated) != 1 {
		t.Fatalf("workload stop failure recreated or retained a usable key: keys=%d generated=%d", len(f.gateway.keys), len(f.gateway.generated))
	}
	if replicas := ptr.Deref(f.workload(t).Spec.Replicas, -1); replicas != 1 {
		t.Fatalf("injected workload-stop failure did not leave the workload unchanged: replicas=%d", replicas)
	}
}
