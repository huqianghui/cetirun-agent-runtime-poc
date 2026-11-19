// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type policyLimitBlockRecorder struct {
	*fakeAIGateway
	blocks int
}

func (g *policyLimitBlockRecorder) SetKeyBlocked(ctx context.Context, token string, blocked bool) error {
	if blocked {
		g.blocks++
	}
	return g.fakeAIGateway.SetKeyBlocked(ctx, token, blocked)
}

func pendingPolicyLimitFixture(t *testing.T, kind string, mode corev1.PersistentVolumeAccessMode, lifetime, firstChange string) *nativeSandboxFixture {
	t.Helper()
	ctx := context.Background()
	f, input := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
		configureRolloutStableIngress(t, kind), withNativeHomeAccessMode(t, mode), withReconciledCoordination(t))
	if firstChange == "remove" {
		policy := &api.AgentPolicy{}
		if err := f.kube.Get(ctx, client.ObjectKeyFromObject(input.Policy), policy); err != nil {
			t.Fatal(err)
		}
		policy.Spec.ModelAccess.RateLimits = exampleRateLimits()
		if err := f.kube.Update(ctx, policy); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		f.reconcileOK(t)
	}
	if lifetime != "initializing" {
		workload := f.workload(t)
		workload.Status.ObservedGeneration = workload.Generation
		workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
		if err := f.kube.Status().Update(ctx, workload); err != nil {
			t.Fatal(err)
		}
		f.reconcileOK(t)
		if f.current(t).Status.Phase != api.PhaseReady {
			t.Fatal("retained fixture must first become Ready")
		}
	}
	if lifetime == "retained-restart" {
		agent := f.current(t)
		agent.Spec.RestartRequestID = "pending-restart-0001"
		agent.Generation++
		if err := f.kube.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		for range 4 {
			_, err := f.reconcile()
			if err != nil && !strings.Contains(err.Error(), "source reservation waits for acknowledged controller shutdown") {
				t.Fatal("prepare retained restart:", err)
			}
			workload := f.workload(t)
			if ptr.Deref(workload.Spec.Replicas, 1) == 0 {
				workload.Status = appsv1.StatefulSetStatus{ObservedGeneration: workload.Generation}
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	f.reconcileOK(t)
	if (f.current(t).Status.Phase == api.PhaseReady) != (lifetime == "ready") ||
		ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
		t.Fatal("fixture must have a submitted, active common workload in the requested phase")
	}
	return f
}

func TestReconciledSandboxPendingRateLimitsPreserveLifetime(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
			for _, lifetime := range []string{"initializing", "retained-restart", "ready"} {
				for _, firstChange := range []string{"add", "remove"} {
					for _, acceptance := range []string{"lagged", "current", "catch-up"} {
						t.Run(kind+"/"+string(mode)+"/"+lifetime+"/"+firstChange+"/"+acceptance, func(t *testing.T) {
							ctx := context.Background()
							f := pendingPolicyLimitFixture(t, kind, mode, lifetime, firstChange)
							agent, workload := f.current(t), f.workload(t)
							pod := &corev1.Pod{
								ObjectMeta: metav1.ObjectMeta{
									Name: workload.Name + "-0", Namespace: workload.Namespace,
									OwnerReferences: []metav1.OwnerReference{{
										APIVersion: "apps/v1", Kind: "StatefulSet", Name: workload.Name, UID: workload.UID,
										Controller: ptr.To(true),
									}},
								},
								Spec: *workload.Spec.Template.Spec.DeepCopy(),
								Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
									{Type: corev1.PodInitialized, Status: corev1.ConditionFalse},
								}},
							}
							if err := f.kube.Create(ctx, pod); err != nil {
								t.Fatal(err)
							}
							beforeExpected := reconciledExpectationForTest(t, f)
							beforeTicket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
							if err != nil || !found {
								t.Fatal("missing initial native reservation:", err)
							}
							token := aigateway.Token(f.gateway.generated[0].Key)
							f.gateway.keys[token].Spend = ptr.To(2.5)
							gateway := &policyLimitBlockRecorder{fakeAIGateway: f.gateway}
							f.r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
							stages, stopWrites := f.installer.stages, 0
							checkStop := func(object client.Object) {
								if current, ok := object.(*appsv1.StatefulSet); ok && current.UID == workload.UID &&
									ptr.Deref(current.Spec.Replicas, 1) == 0 {
									stopWrites++
								}
							}
							policies := &AgentPolicyReconciler{
								Client: f.kube, APIReader: f.kube, NativeExecution: true, Source: f.r.SandboxPreparation.Configuration,
							}
							policyKey := client.ObjectKey{Name: agent.Status.Policy.EffectivePolicyRef.Name}
							catchUp, policyRead := false, false
							writer := interceptor.NewClient(f.kube, interceptor.Funcs{
								Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
									if _, ok := object.(*api.Team); catchUp && policyRead && ok && key.Name == agent.Spec.Team {
										catchUp = false
										if _, err := policies.Reconcile(ctx, ctrl.Request{NamespacedName: policyKey}); err != nil {
											return err
										}
									}
									err := c.Get(ctx, key, object, opts...)
									if policy, ok := object.(*unstructured.Unstructured); err == nil && ok &&
										policy.GetKind() == "AgentPolicy" && key == policyKey {
										policyRead = true
									}
									return err
								},
								Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
									checkStop(object)
									return c.Update(ctx, object, opts...)
								},
								Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
									checkStop(object)
									return c.Patch(ctx, object, patch, opts...)
								},
							})
							f.r.Client, f.r.APIReader = writer, writer
							steps := []struct {
								name   string
								limits *api.AgentPolicyRateLimits
								want   aigateway.KeyLimits
							}{
								{"add", exampleRateLimits(), exampleKeyLimits()},
								{"remove", nil, aigateway.KeyLimits{}},
							}
							if firstChange == "remove" {
								steps[0], steps[1] = steps[1], steps[0]
							}
							for _, step := range steps {
								policy := &api.AgentPolicy{}
								if err := f.kube.Get(ctx, policyKey, policy); err != nil {
									t.Fatal(err)
								}
								policy.Spec.ModelAccess.RateLimits = step.limits
								policy.Generation++
								if err := f.kube.Update(ctx, policy); err != nil {
									t.Fatal(err)
								}
								if acceptance == "current" {
									if _, err := policies.Reconcile(ctx, ctrl.Request{NamespacedName: policyKey}); err != nil {
										t.Fatal(err)
									}
								}
								catchUp, policyRead = acceptance == "catch-up", false
								_, reconcileErr := f.reconcile()
								after, actual := f.current(t), f.workload(t)
								ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
								expected := reconciledExpectationForTest(t, f)
								key := f.gateway.keys[token]
								if reconcileErr != nil || err != nil || !found || catchUp || gateway.blocks != 0 || stopWrites != 0 ||
									f.installer.stages != stages || after.Status.Phase != agent.Status.Phase ||
									!reflect.DeepEqual(after.Spec, agent.Spec) || after.UID != agent.UID ||
									after.Status.AIGateway.KeyID != agent.Status.AIGateway.KeyID ||
									actual.UID != workload.UID || !reflect.DeepEqual(actual.Spec, workload.Spec) ||
									f.pod(t).UID != pod.UID || expected.Home != beforeExpected.Home ||
									expected.PlanHash != beforeExpected.PlanHash || expected.Revision != beforeExpected.Revision ||
									ticket.Reservation.ID != beforeTicket.Reservation.ID ||
									ticket.Reservation.Phase != sandboxcoordination.Reserved ||
									!reflect.DeepEqual(ticket.Reservation.Intent.SourceDigests, beforeTicket.Reservation.Intent.SourceDigests) ||
									key == nil || key.Blocked || !key.Limits.Equal(step.want) || ptr.Deref(key.Spend, 0) != 2.5 ||
									len(f.gateway.generated) != 1 || len(f.gateway.deletedTokens) != 0 || len(f.gateway.deletedAliases) != 0 {
									t.Fatalf("%s disturbed initializing lifetime: reconcile=%v reservation=%v phase=%s blocks=%d stops=%d applies=%d/%d reservationPhase=%s catchUpPending=%t",
										step.name, reconcileErr, err, after.Status.Phase, gateway.blocks, stopWrites,
										f.installer.stages, stages, ticket.Reservation.Phase, catchUp)
								}
							}
						})
					}
				}
			}
		}
	}
}
