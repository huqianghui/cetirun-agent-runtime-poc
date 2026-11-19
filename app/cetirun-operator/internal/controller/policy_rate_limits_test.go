// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func readyPolicyRateLimitFixture(t *testing.T, kind string, reconciled bool) *nativeSandboxFixture {
	t.Helper()
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, reconciled, kind, true, false,
		configureRolloutStableIngress(t, kind))
	if reconciled {
		workload := f.workload(t)
		workload.Status.ObservedGeneration = workload.Generation
		workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1
		if err := f.kube.Status().Update(context.Background(), workload); err != nil {
			t.Fatal(err)
		}
		f.reconcileOK(t)
	} else {
		f.ready(t)
	}
	if f.current(t).Status.Phase != api.PhaseReady {
		t.Fatal("fixture must be running before the policy changes")
	}
	return f
}

func TestNativePolicyRateLimitsDoNotRestartRunningAgents(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, reconciled := range []bool{false, true} {
			if kind == "hermes" && !reconciled {
				continue // Retained single-start lifetimes support OpenClaw only.
			}
			lifetime := "single-start"
			if reconciled {
				lifetime = "reconciled"
			}
			t.Run(kind+"/"+lifetime, func(t *testing.T) {
				ctx := context.Background()
				f := readyPolicyRateLimitFixture(t, kind, reconciled)
				beforeAgent, beforeWorkload := f.current(t), f.workload(t)
				expectation := func() any {
					if reconciled {
						expected := reconciledExpectationForTest(t, f)
						for i := range expected.Sources {
							source := &expected.Sources[i]
							if source.Kind == "AgentPolicy" && source.UID == beforeAgent.Status.Policy.EffectivePolicyRef.UID {
								source.ResourceVersion = ""
							}
						}
						return expected
					}
					_, expected := f.expectation(t)
					return *expected
				}
				beforeExpectation, beforeStages := expectation(), f.installer.stages
				token := aigateway.Token(f.gateway.generated[0].Key)
				f.gateway.keys[token].Spend = ptr.To(2.5)
				var stopWrites int
				checkScale := func(object client.Object) {
					if workload, ok := object.(*appsv1.StatefulSet); ok && workload.Name == beforeWorkload.Name &&
						workload.Namespace == beforeWorkload.Namespace && ptr.Deref(workload.Spec.Replicas, 1) == 0 {
						stopWrites++
					}
				}
				writer := interceptor.NewClient(f.kube, interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
						checkScale(object)
						return c.Update(ctx, object, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						checkScale(object)
						return c.Patch(ctx, object, patch, opts...)
					},
				})
				f.r.Client, f.r.APIReader = writer, writer
				policy := &api.AgentPolicy{}
				policyKey := client.ObjectKey{Name: beforeAgent.Status.Policy.EffectivePolicyRef.Name}
				config := f.r.SandboxPreparation
				policyController := &AgentPolicyReconciler{
					Client: f.kube, APIReader: f.kube, NativeExecution: true,
					Source: config.Configuration, Now: config.Now, MaxSnapshotAge: config.MaxSnapshotAge,
				}
				for _, step := range []struct {
					name   string
					limits *api.AgentPolicyRateLimits
					want   aigateway.KeyLimits
				}{
					{"add", exampleRateLimits(), exampleKeyLimits()},
					{"change and partly remove", &api.AgentPolicyRateLimits{
						RPM: ptr.To[int64](30), PerModel: map[string]api.AgentPolicyModelRateLimit{"auto": {TPM: ptr.To[int64](8192)}},
					}, aigateway.KeyLimits{RPM: ptr.To[int64](30), ModelTPM: map[string]int64{"auto": 8192}}},
					{"per-model only", &api.AgentPolicyRateLimits{
						RPM: ptr.To[int64](30), PerModel: map[string]api.AgentPolicyModelRateLimit{testSharedModelOne: {RPM: ptr.To[int64](10)}},
					}, aigateway.KeyLimits{RPM: ptr.To[int64](30), ModelRPM: map[string]int64{testSharedModelOne: 10}}},
					{"remove", nil, aigateway.KeyLimits{}},
				} {
					if err := f.kube.Get(ctx, policyKey, policy); err != nil {
						t.Fatal(err)
					}
					policy.Spec.ModelAccess.RateLimits = step.limits
					policy.Generation++
					if err := f.kube.Update(ctx, policy); err != nil {
						t.Fatal(err)
					}
					for _, observed := range []bool{false, true} {
						if observed {
							if _, err := policyController.Reconcile(ctx, ctrl.Request{NamespacedName: policyKey}); err != nil {
								t.Fatal("refresh policy acceptance:", err)
							}
						}
						f.reconcileOK(t)
						afterAgent, afterWorkload := f.current(t), f.workload(t)
						key := f.gateway.keys[token]
						if afterAgent.Status.Phase != api.PhaseReady || afterAgent.Status.AccessURL != beforeAgent.Status.AccessURL ||
							afterAgent.Status.AIGateway.KeyID != beforeAgent.Status.AIGateway.KeyID ||
							afterAgent.Status.Sandbox.AppliedRevision != beforeAgent.Status.Sandbox.AppliedRevision ||
							!reflect.DeepEqual(afterAgent.Spec, beforeAgent.Spec) ||
							afterWorkload.UID != beforeWorkload.UID || !reflect.DeepEqual(afterWorkload.Spec, beforeWorkload.Spec) ||
							!reflect.DeepEqual(expectation(), beforeExpectation) || f.installer.stages != beforeStages || stopWrites != 0 {
							t.Fatalf("%s (observed=%t) changed the running Agent: phase=%s stopWrites=%d access=%t key=%t sandbox=%t agentSpec=%t workload=%t expectation=%t stages=%d/%d",
								step.name, observed, afterAgent.Status.Phase, stopWrites,
								afterAgent.Status.AccessURL == beforeAgent.Status.AccessURL,
								afterAgent.Status.AIGateway.KeyID == beforeAgent.Status.AIGateway.KeyID,
								afterAgent.Status.Sandbox.AppliedRevision == beforeAgent.Status.Sandbox.AppliedRevision,
								reflect.DeepEqual(afterAgent.Spec, beforeAgent.Spec),
								afterWorkload.UID == beforeWorkload.UID && reflect.DeepEqual(afterWorkload.Spec, beforeWorkload.Spec),
								reflect.DeepEqual(expectation(), beforeExpectation), f.installer.stages, beforeStages)
						}
						if !reconciled {
							f.assertReady(t)
						}
						if key == nil || key.Blocked || !key.Limits.Equal(step.want) || ptr.Deref(key.Spend, 0) != 2.5 ||
							len(f.gateway.generated) != 1 || len(f.gateway.deletedTokens) != 0 || len(f.gateway.deletedAliases) != 0 {
							t.Fatalf("%s (observed=%t) did not update the same unblocked Gateway key in place", step.name, observed)
						}
					}
				}
			})
		}
	}
}

func TestNativePendingPolicyStillFencesExecutionChanges(t *testing.T) {
	for _, reconciled := range []bool{false, true} {
		lifetime := "single-start"
		if reconciled {
			lifetime = "reconciled"
		}
		for _, change := range []struct {
			name string
			edit func(*api.AgentPolicy)
		}{
			{"budget", func(p *api.AgentPolicy) { p.Spec.ModelAccess.GatewayBudget.DefaultMonthlyUSD = "15" }},
			{"model access", func(p *api.AgentPolicy) {
				p.Spec.ModelAccess.SharedModels.Allowed = nil
				p.Spec.ModelAccess.GatewayBudget = nil
			}},
			{"Sandbox control", func(p *api.AgentPolicy) { p.Spec.Network.Ingress.Mode = api.AgentPolicyIngressDisabled }},
		} {
			t.Run(lifetime+"/"+change.name, func(t *testing.T) {
				ctx := context.Background()
				f := readyPolicyRateLimitFixture(t, "openclaw", reconciled)
				agent := f.current(t)
				policy := &api.AgentPolicy{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Spec.ModelAccess.RateLimits = exampleRateLimits()
				change.edit(policy)
				policy.Generation++
				if err := f.kube.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if f.current(t).Status.Phase == api.PhaseReady || ptr.Deref(f.workload(t).Spec.Replicas, 1) != 0 {
					t.Fatal("pending rate-limit update bypassed the simultaneous execution-control fence")
				}
			})
		}
	}
}

func TestNativePendingPolicyRequiresCurrentAssessment(t *testing.T) {
	ctx := context.Background()
	f := readyPolicyRateLimitFixture(t, "openclaw", true)
	agent := f.current(t)
	policy := &api.AgentPolicy{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
		t.Fatal(err)
	}
	before := policy.DeepCopy()
	for _, scenario := range []string{
		"unknown acceptance", "denied acceptance", "missing acceptance", "inconsistent observation",
		"invalid limits", "unsupported control", "unavailable inventory", "inventory read failure", "stale inventory",
	} {
		t.Run(scenario, func(t *testing.T) {
			current := before.DeepCopy()
			current.Generation++
			current.Spec.ModelAccess.RateLimits = exampleRateLimits()
			switch scenario {
			case "unknown acceptance":
				current.Status.Conditions[0].Status = metav1.ConditionUnknown
			case "denied acceptance":
				current.Status.Conditions[0].Status = metav1.ConditionFalse
			case "missing acceptance":
				current.Status.Conditions = nil
			case "inconsistent observation":
				current.Status.Conditions[0].ObservedGeneration++
			case "invalid limits":
				current.Spec.ModelAccess.RateLimits.RPM = ptr.To[int64](0)
			case "unsupported control":
				current.Spec.Network.Ingress.RequireMTLS = true
			case "unavailable inventory":
				original := f.r.SandboxPreparation.Configuration
				f.r.SandboxPreparation.Configuration = nil
				t.Cleanup(func() { f.r.SandboxPreparation.Configuration = original })
			case "inventory read failure", "stale inventory":
				original := f.r.SandboxPreparation.Configuration
				f.r.SandboxPreparation.Configuration = SandboxConfigurationSourceFunc(func(ctx context.Context) (SandboxConfigurationSnapshot, error) {
					if scenario == "inventory read failure" {
						return SandboxConfigurationSnapshot{}, errors.New("synthetic inventory failure")
					}
					snapshot, err := original.Snapshot(ctx)
					snapshot.ObservedAt = snapshot.ObservedAt.Add(-time.Hour)
					return snapshot, err
				})
				t.Cleanup(func() { f.r.SandboxPreparation.Configuration = original })
			}
			status := current.Status.DeepCopy()
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(current), policy); err != nil {
				t.Fatal(err)
			}
			current.ResourceVersion = policy.ResourceVersion
			if err := f.kube.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			current.Status = *status
			if err := f.kube.Status().Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.resolvePolicy(ctx, agent); err == nil || !isPolicyDependencyError(err) {
				t.Fatalf("pending acceptance did not preserve a fail-closed dependency hold: %v", err)
			}
		})
	}
}
