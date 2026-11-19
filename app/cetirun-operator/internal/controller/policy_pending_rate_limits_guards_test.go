// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type policyMutationPrerequisites struct {
	*KubernetesSandboxPrerequisites
	mutate func()
}

func (p *policyMutationPrerequisites) ResolveCurrent(ctx context.Context, agent *api.Agent, team *api.Team) (SandboxPrerequisites, error) {
	input, err := p.KubernetesSandboxPrerequisites.ResolveCurrent(ctx, agent, team)
	if err == nil && p.mutate != nil {
		mutate := p.mutate
		p.mutate = nil
		mutate()
	}
	return input, err
}

func TestReconciledSandboxPolicyDigestRemainsBoundToResolvedInputs(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "limits only"
		if mixed {
			name = "mixed policy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := pendingPolicyLimitFixture(t, "openclaw", corev1.ReadWriteOncePod, "initializing", "add")
			agent, stages := f.current(t), f.installer.stages
			changed := false
			f.r.SandboxPreparation.Prerequisites = &policyMutationPrerequisites{
				KubernetesSandboxPrerequisites: f.r.SandboxPreparation.Prerequisites.(*KubernetesSandboxPrerequisites),
				mutate: func() {
					changed = true
					policy := &api.AgentPolicy{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
						t.Fatal(err)
					}
					policy.Spec.ModelAccess.RateLimits = exampleRateLimits()
					if mixed {
						policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
					}
					policy.Generation++
					if err := f.kube.Update(ctx, policy); err != nil {
						t.Fatal(err)
					}
				},
			}
			_, err := f.reconcile()
			if !changed || f.installer.stages != stages {
				t.Fatal("preparation source race applied a workload:", err)
			}
			if mixed {
				if err == nil || !strings.Contains(err.Error(), "Sandbox input changed before resource reconciliation") ||
					ptr.Deref(f.workload(t).Spec.Replicas, 1) != 0 {
					t.Fatal("mixed policy was recaptured as authority for the old resolved inputs:", err)
				}
			} else if err != nil || ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
				t.Fatal("limits-only drift during preparation interrupted execution:", err)
			}
		})
	}
}

func TestReconciledSandboxPendingPolicySourceChangesStillFence(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*api.AgentPolicy)
	}{
		{"limits only", func(*api.AgentPolicy) {}},
		{"network", func(p *api.AgentPolicy) { p.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"} }},
		{"security", func(p *api.AgentPolicy) {
			p.Spec.Security.ReadOnlyRootFilesystem = ptr.To(!ptr.Deref(p.Spec.Security.ReadOnlyRootFilesystem, false))
		}},
		{"roles", func(p *api.AgentPolicy) {
			p.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "reader", RoleBindings: []api.AgentPolicyRoleBinding{
					{Namespace: "example-data", ClusterRoleName: "view"},
				},
			}
		}},
		{"identity", func(p *api.AgentPolicy) { p.Spec.AgentIdentity.Allowed = !p.Spec.AgentIdentity.Allowed }},
		{"selection", func(p *api.AgentPolicy) {
			p.Spec.SandboxSelection.AllowMultiple = !p.Spec.SandboxSelection.AllowMultiple
		}},
		{"harness", func(p *api.AgentPolicy) { p.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes} }},
		{"skills", func(p *api.AgentPolicy) { p.Spec.Skills.Allowed = []string{"another-skill"} }},
		{"budget", func(p *api.AgentPolicy) { p.Spec.ModelAccess.GatewayBudget.DefaultMonthlyUSD = "15" }},
		{"models", func(p *api.AgentPolicy) { p.Spec.ModelAccess.SharedModels.Allowed = nil }},
		{"control metadata", func(p *api.AgentPolicy) {
			p.Annotations = map[string]string{"sandbox.cetirun.microsoft.com/control": "changed"}
		}},
		{"replacement UID", func(*api.AgentPolicy) {}},
	} {
		t.Run(change.name, func(t *testing.T) {
			ctx := context.Background()
			f := pendingPolicyLimitFixture(t, "openclaw", corev1.ReadWriteOncePod, "initializing", "add")
			agent, workload := f.current(t), f.workload(t)
			workload.Status.ObservedGeneration, workload.Status.Replicas = workload.Generation, 1
			if err := f.kube.Status().Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			before, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
			if err != nil || !found {
				t.Fatal(err)
			}
			gateway := &policyLimitBlockRecorder{fakeAIGateway: f.gateway}
			f.r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
			stages, changed, policyRead := f.installer.stages, false, false
			f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*api.Team); !changed && policyRead && ok && key.Name == agent.Spec.Team {
						changed = true
						policy := &api.AgentPolicy{}
						if err := c.Get(ctx, client.ObjectKey{Name: agent.Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
							return err
						}
						policy.Spec.ModelAccess.RateLimits = exampleRateLimits()
						change.edit(policy)
						policy.Generation++
						if change.name == "replacement UID" {
							if err := c.Delete(ctx, policy); err != nil {
								return err
							}
							policy.UID, policy.ResourceVersion = "replacement-policy-uid", ""
							if err := c.Create(ctx, policy); err != nil {
								return err
							}
						} else if err := c.Update(ctx, policy); err != nil {
							return err
						}
					}
					err := c.Get(ctx, key, object, opts...)
					if policy, ok := object.(*unstructured.Unstructured); err == nil && ok &&
						policy.GetKind() == "AgentPolicy" && key.Name == agent.Status.Policy.EffectivePolicyRef.Name {
						policyRead = true
					}
					return err
				},
			})
			_, reconcileErr := f.reconcile()
			current, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
			if !changed || err != nil || !found || current.Reservation.ID != before.Reservation.ID ||
				f.installer.stages != stages || f.workload(t).UID != workload.UID {
				t.Fatal("source race lost the original workload or reservation:", reconcileErr, err)
			}
			if change.name == "limits only" || change.name == "skills" {
				// Neither Gateway limits nor the skill allowlist are execution inputs.
				if reconcileErr != nil || current.Reservation.Phase != sandboxcoordination.Reserved ||
					gateway.blocks != 0 || ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
					t.Fatalf("a %s write during bootstrap fenced execution: %v", change.name, reconcileErr)
				}
				return
			}
			if !errors.Is(reconcileErr, errSandboxSourceChanged) || current.Reservation.Phase != sandboxcoordination.Stopping ||
				gateway.blocks != 1 || ptr.Deref(f.workload(t).Spec.Replicas, 1) != 0 {
				t.Fatalf("mixed %s edit bypassed the source fence: err=%v phase=%s blocks=%d",
					change.name, reconcileErr, current.Reservation.Phase, gateway.blocks)
			}
		})
	}
}

func TestReconciledSandboxPolicySourcePinsWireImageAndIdentity(t *testing.T) {
	for _, change := range []string{"unknown control removed", "missing resource version", "wrong source key", "missing digest"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f := pendingPolicyLimitFixture(t, "openclaw", corev1.ReadWriteOncePod, "initializing", "add")
			captured := false
			f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					if policy, ok := object.(*unstructured.Unstructured); ok && policy.GetKind() == "AgentPolicy" {
						if !captured && change == "unknown control removed" {
							return unstructured.SetNestedField(policy.Object, true, "spec", "futureExecutionControl")
						}
						if captured && change == "missing resource version" {
							policy.SetResourceVersion("")
						}
						if captured && change == "wrong source key" {
							policy.SetName("different-policy")
						}
					}
					return nil
				},
			})
			attempt, err := f.r.resolveSandboxInputs(ctx, f.current(t), false, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.r.sandboxBootstrapGuard(ctx, attempt); err != nil {
				t.Fatal("captured wire source must initially verify:", err)
			}
			captured = true
			if change == "missing digest" {
				attempt.policyDigest = ""
			}
			if err := f.r.sandboxBootstrapGuard(ctx, attempt); !errors.Is(err, errSandboxSourceChanged) {
				t.Fatal("changed or incomplete source proof authorized bootstrap:", err)
			}
		})
	}
}

func TestReconciledSandboxRateLimitRefreshPreservesFinalApplyGuards(t *testing.T) {
	for _, change := range []string{"limits only", "mixed policy", "expectation version", "expectation UID", "cancelled", "stopping"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f, input := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false, withReconciledCoordination(t))
			installer := f.r.HelmClient.(*sharedSandboxInstaller)
			interrupted := errors.New("synthetic pause before apply")
			var prepared *helm.PreparedRelease
			installer.beforeApply = func(value *helm.PreparedRelease) error {
				prepared = value
				return interrupted
			}
			if _, err := f.reconcile(); !errors.Is(err, interrupted) || prepared == nil {
				t.Fatal("fixture did not reach unsubmitted apply:", err)
			}
			expected := reconciledExpectationForTest(t, f)
			state := &corev1.ConfigMap{}
			stateKey := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}
			if err := f.kube.Get(ctx, stateKey, state); err != nil || state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyPending {
				t.Fatal("fixture lost its non-submission proof:", err)
			}
			policy := &api.AgentPolicy{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(input.Policy), policy); err != nil {
				t.Fatal(err)
			}
			policy.Spec.ModelAccess.RateLimits = exampleRateLimits()
			policy.Generation++
			if change == "mixed policy" {
				policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
			}
			if err := f.kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "expectation version", "cancelled":
				state.Annotations["example.com/observation"] = "new"
				if change == "cancelled" {
					state.Annotations[sandboxApplyStateAnnotation] = sandboxApplyCancelled
				}
				if err := f.kube.Update(ctx, state); err != nil {
					t.Fatal(err)
				}
			case "expectation UID":
				if err := f.kube.Delete(ctx, state); err != nil {
					t.Fatal(err)
				}
				state.UID, state.ResourceVersion = "replacement-expectation-uid", ""
				if err := f.kube.Create(ctx, state); err != nil {
					t.Fatal(err)
				}
			case "stopping":
				ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, input.Agent.UID)
				if err != nil || !found {
					t.Fatal(err)
				}
				if _, err := f.r.SandboxNativeCoordination.BeginNativeStop(ctx, ticket); err != nil {
					t.Fatal(err)
				}
			}
			err := prepared.AuthorizeSandboxApply(ctx)
			if (err == nil) != (change == "limits only") {
				t.Fatalf("final apply guard for %s: %v", change, err)
			}
			if err := f.kube.Get(ctx, stateKey, state); err != nil {
				t.Fatal(err)
			}
			want := sandboxApplyPending
			if change == "limits only" {
				want = sandboxApplySubmitted
			} else if change == "cancelled" {
				want = sandboxApplyCancelled
			}
			if state.Annotations[sandboxApplyStateAnnotation] != want || f.installer.stages != 0 {
				t.Fatal("source refresh changed submission certainty or applied a workload")
			}
		})
	}
}
