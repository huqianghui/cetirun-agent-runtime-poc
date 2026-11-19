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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxTeamRoutersPreserveUnchangedAgents(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, backend := range []string{"kata", "container"} {
			t.Run(kind+"/"+backend, func(t *testing.T) {
				ctx := context.Background()
				setup := []func(*AgentReconciler, client.Client){configureRolloutStableIngress(t, kind), withReconciledCoordination(t)}
				if backend == "container" {
					setup = append(setup, withContainerRuntime(t))
				}
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, setup...)
				workload := f.workload(t)
				workload.Status.ObservedGeneration = workload.Generation
				workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				before, workload := f.current(t), f.workload(t)
				if before.Status.Phase != api.PhaseReady {
					t.Fatal("fixture is not Ready")
				}
				token := aigateway.Token(f.gateway.generated[0].Key)
				f.gateway.keys[token].Spend = ptr.To(2.5)
				stages, stopWrites := f.installer.stages, 0
				checkStop := func(object client.Object) {
					if current, ok := object.(*appsv1.StatefulSet); ok && current.Name == workload.Name &&
						current.Namespace == workload.Namespace && ptr.Deref(current.Spec.Replicas, 1) == 0 {
						stopWrites++
					}
				}
				writer := interceptor.NewClient(f.kube, interceptor.Funcs{
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
				router := testRouter("ungranted-router", testSharedModelOne, testSharedModelOne, testSharedModelOne)
				changed := router
				changed.Tiers.Simple = testSmallModel
				for _, routers := range [][]api.TeamModelRouter{{router}, {changed}, nil} {
					team := &api.Team{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: before.Spec.Team}, team); err != nil {
						t.Fatal(err)
					}
					team.Spec.ModelRouters = routers
					team.Generation++
					if err := f.kube.Update(ctx, team); err != nil {
						t.Fatal(err)
					}
					for _, observed := range []bool{false, true} {
						if observed {
							assessor := &TeamReconciler{Client: f.kube, APIReader: f.kube}
							if _, err := assessor.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)}); err != nil {
								t.Fatal(err)
							}
						}
						f.reconcileOK(t)
					}
					current, currentWorkload := f.current(t), f.workload(t)
					if current.Status.Phase != api.PhaseReady || current.Status.AccessURL != before.Status.AccessURL ||
						current.Status.Sandbox.AppliedRevision != before.Status.Sandbox.AppliedRevision ||
						currentWorkload.UID != workload.UID || !reflect.DeepEqual(currentWorkload.Spec, workload.Spec) ||
						stopWrites != 0 {
						t.Fatalf("ungranted router changed a running Agent: phase=%s stages=%d/%d stops=%d access=%t revision=%t workload=%t",
							current.Status.Phase, f.installer.stages, stages, stopWrites,
							current.Status.AccessURL == before.Status.AccessURL,
							current.Status.Sandbox.AppliedRevision == before.Status.Sandbox.AppliedRevision,
							currentWorkload.UID == workload.UID && reflect.DeepEqual(currentWorkload.Spec, workload.Spec))
					}
					key := f.gateway.keys[token]
					if key == nil || key.Blocked || ptr.Deref(key.Spend, 0) != 2.5 ||
						current.Status.AIGateway.KeyID != before.Status.AIGateway.KeyID ||
						len(f.gateway.generated) != 1 || len(f.gateway.deletedTokens) != 0 {
						t.Fatal("ungranted router replaced or blocked the retained Gateway key")
					}
				}
				team := &api.Team{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: before.Spec.Team}, team); err != nil {
					t.Fatal(err)
				}
				team.Spec.MemberContext.Role = "changed-runtime-context"
				team.Generation++
				if err := f.kube.Update(ctx, team); err != nil {
					t.Fatal(err)
				}
				stages = f.installer.stages
				_, err := f.reconcile()
				if err == nil || !strings.Contains(err.Error(), sandboxcoordination.ErrConflict.Error()) || f.installer.stages != stages ||
					f.current(t).Status.Sandbox.AppliedRevision != before.Status.Sandbox.AppliedRevision {
					t.Fatalf("changed shared context bypassed its execution reservation: %v", err)
				}
			})
		}
	}
}

func TestReconciledTeamAcceptanceRequiresVerifiedBindings(t *testing.T) {
	for _, change := range []string{"router-only", "missing-acceptance", "unknown", "denied", "unobserved", "wrong-generation", "missing-policy", "missing-sandbox", "terminating"} {
		t.Run(change, func(t *testing.T) {
			f := readyPolicyRateLimitFixture(t, "hermes", true)
			ctx := context.Background()
			team := &api.Team{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: f.current(t).Spec.Team}, team); err != nil {
				t.Fatal(err)
			}
			team.Generation++
			team.Spec.ModelRouters = []api.TeamModelRouter{testRouter("ungranted-router", testSharedModelOne, testSharedModelOne, testSharedModelOne)}
			accepted := meta.FindStatusCondition(team.Status.Conditions, api.ConditionTeamAccepted)
			switch change {
			case "missing-acceptance":
				team.Status.Conditions = nil
			case "unknown":
				accepted.Status = metav1.ConditionUnknown
			case "denied":
				accepted.Status = metav1.ConditionFalse
			case "unobserved":
				team.Status.ObservedGeneration = 0
			case "wrong-generation":
				accepted.ObservedGeneration = team.Generation
			case "missing-policy":
				team.Spec.AgentPolicyRef = "missing-policy"
			case "missing-sandbox":
				team.Spec.SandboxRefs = append(team.Spec.SandboxRefs, "missing-sandbox")
			case "terminating":
				team.DeletionTimestamp = ptr.To(metav1.Now())
			}
			if err := f.r.verifyTeamAcceptance(ctx, team); (err == nil) != (change == "router-only") {
				t.Fatalf("pending Team acceptance: change=%s err=%v", change, err)
			}
		})
	}
}

func TestReconciledBootstrapTeamGuardUsesExecutionInputs(t *testing.T) {
	for _, change := range []string{"routers", "status", "context", "policy", "selected-sandbox", "uid"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f := readyPolicyRateLimitFixture(t, "hermes", true)
			attempt, err := f.r.resolveSandboxInputs(ctx, f.current(t), false, true)
			if err != nil {
				t.Fatal(err)
			}
			team := attempt.team.DeepCopy()
			switch change {
			case "routers":
				team.Spec.ModelRouters = []api.TeamModelRouter{testRouter("ungranted-router", testSharedModelOne, testSharedModelOne, testSharedModelOne)}
			case "status":
				team.Status.Conditions = append(team.Status.Conditions, metav1.Condition{
					Type: "SyntheticRouterObservation", Status: metav1.ConditionTrue, Reason: "Observed",
					Message: "Synthetic router observation", LastTransitionTime: metav1.Now(),
				})
			case "context":
				team.Spec.MemberContext.Role = "changed-runtime-context"
			case "policy":
				team.Spec.AgentPolicyRef = "other-policy"
			case "selected-sandbox":
				team.Spec.SandboxRefs = []string{"unselected-sandbox"}
			case "uid":
				team.UID = "replacement-team-uid"
			}
			if change == "status" {
				err = f.kube.Status().Update(ctx, team)
			} else {
				err = f.kube.Update(ctx, team)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = f.r.sandboxBootstrapGuard(ctx, attempt)
			allowed := change == "routers" || change == "status"
			if (err == nil) != allowed {
				t.Fatalf("bootstrap Team fence: change=%s err=%v", change, err)
			}
		})
	}
}
