// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const holdFinalizer = "cetirun.microsoft.com/sandbox-hold"

func TestSandboxHoldRejectsUnconfirmedBuiltinOwnership(t *testing.T) {
	for _, typ := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		for _, ownership := range []string{"missing", "foreign UID", "foreign annotation", "foreign release", "foreign release namespace"} {
			t.Run(string(typ)+"/"+ownership, func(t *testing.T) {
				agent := newCR("held", api.PhaseReady, nil, 1, 1, "")
				agent.Spec.Agent.Type, agent.Spec.SandboxRef = typ, "standard"
				sts := statefulSet(agent.Name, 1)
				route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-held", Namespace: "cetirun-held"}}
				ownSandboxResources(agent, sts, route)
				for _, object := range []client.Object{sts, route} {
					switch ownership {
					case "missing":
						object.SetOwnerReferences(nil)
					case "foreign UID":
						owners := object.GetOwnerReferences()
						owners[0].UID = "previous-agent"
						object.SetOwnerReferences(owners)
					case "foreign annotation":
						object.SetAnnotations(map[string]string{"cetirun.microsoft.com/agent-uid": "previous-agent"})
					case "foreign release":
						object.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "another-release"})
					case "foreign release namespace":
						object.SetAnnotations(map[string]string{"meta.helm.sh/release-namespace": "another-namespace"})
					}
				}
				c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).
					WithObjects(agent, instanceNamespace(agent.Name), sts, route).Build()
				helm := &fakeHelmClient{}
				r := &controller.AgentReconciler{Client: c, HelmClient: helm}
				key := types.NamespacedName{Name: agent.Name}
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
					t.Fatal("unconfirmed ownership must be surfaced")
				}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), sts); err != nil || *sts.Spec.Replicas != 1 {
					t.Fatalf("unconfirmed workload was changed: %v", err)
				}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(route), route); err != nil {
					t.Fatalf("unconfirmed route was removed: %v", err)
				}
				if err := c.Get(context.Background(), key, agent); err != nil {
					t.Fatal(err)
				}
				condition := findCondition(agent.Status.Conditions, api.ConditionSandboxReady)
				if condition == nil || condition.Reason != "PolicyApplyFailed" || helm.prepareCalls != 0 || helm.installCalled {
					t.Fatal("ownership failure was hidden or runtime preparation ran")
				}
			})
		}
	}
}

func TestSandboxHoldPersistsBeforeEffectsAndSurvivesStatusFailure(t *testing.T) {
	t.Skip("obsolete pre-Policy fixture: mandatory Policy is tested by the governed hold suite")
	for _, failIntent := range []bool{false, true} {
		t.Run(map[bool]string{false: "status failure", true: "intent failure"}[failIntent], func(t *testing.T) {
			agent := newCR("durable", api.PhaseReady, nil, 1, 1, "")
			agent.Spec.SandboxRef = "standard"
			sts := statefulSet(agent.Name, 1)
			route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-durable", Namespace: "cetirun-durable"}}
			ownSandboxResources(agent, sts, route)
			injected := errors.New("synthetic persistence failure")
			var effects int
			c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).
				WithObjects(agent, instanceNamespace(agent.Name), sts, route).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, intent := obj.(*api.Agent); intent {
							if failIntent {
								return injected
							}
							return c.Patch(ctx, obj, patch, opts...)
						}
						effects++
						persisted := &api.Agent{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(agent), persisted); err != nil ||
							!slices.Contains(persisted.Finalizers, holdFinalizer) {
							t.Fatalf("workload side effect preceded durable intent: %v", err)
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						effects++
						return c.Delete(ctx, obj, opts...)
					},
				}).Build()
			helm := &fakeHelmClient{useHarness: true}
			r := &controller.AgentReconciler{Client: sandboxHoldErrorClient{Client: c, operation: "update status", err: injected}, HelmClient: helm}
			key := types.NamespacedName{Name: agent.Name}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); !errors.Is(err, injected) {
				t.Fatalf("persistence failure hidden: %v", err)
			}
			if failIntent {
				if effects != 0 {
					t.Fatal("failed hold persistence must prevent all workload side effects")
				}
				return
			}
			if err := c.Get(context.Background(), key, agent); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(agent.Finalizers, holdFinalizer) || findCondition(agent.Status.Conditions, api.ConditionSandboxReady) != nil {
				t.Fatal("test must retain durable intent but no successfully written status")
			}
			agent.Spec.SandboxRef = ""
			if err := c.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			// A new reconciler must recover solely from persisted state.
			r = &controller.AgentReconciler{Client: c, HelmClient: helm}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if helm.prepareCalls != 0 || helm.installCalled {
				t.Fatal("lost status released the durable hold after restart")
			}
		})
	}
}

func TestSandboxHoldSuspensionAndFailures(t *testing.T) {
	for _, typ := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes, api.AgentTypeCustom} {
		for _, stopFails := range []bool{false, true} {
			t.Run(string(typ)+map[bool]string{false: "/suspended", true: "/stop error"}[stopFails], func(t *testing.T) {
				agent := newCR("paused", api.PhaseSuspended, nil, 1, 1, "")
				agent.Spec.Agent.Type = typ
				agent.Spec.Suspended, agent.Spec.SandboxRef = true, "standard"
				c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).
					WithObjects(agent, instanceNamespace(agent.Name)).Build()
				var runtimeClient client.Client = c
				if stopFails {
					runtimeClient = sandboxHoldErrorClient{Client: c, operation: "get workload", err: errors.New("synthetic read failure")}
				}
				helm := &fakeHelmClient{}
				r := &controller.AgentReconciler{Client: runtimeClient, HelmClient: helm}
				key := types.NamespacedName{Name: agent.Name}
				_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
				if (err != nil) != stopFails {
					t.Fatalf("unexpected suspension result: %v", err)
				}
				if err := c.Get(context.Background(), key, agent); err != nil {
					t.Fatal(err)
				}
				phase, reason := api.PhaseSuspended, "Suspended"
				if stopFails {
					phase, reason = api.PhaseFailed, "PolicyApplyFailed"
				}
				condition := findCondition(agent.Status.Conditions, api.ConditionSandboxReady)
				if agent.Status.Phase != phase || condition == nil || condition.Reason != reason ||
					condition.Status != metav1.ConditionFalse || helm.prepareCalls != 0 || helm.installCalled {
					t.Fatalf("suspension semantics or error visibility lost: %+v", agent.Status)
				}
			})
		}
	}
}

func TestNewTeamHeldAgentDeletionChecksNamespaceOwnership(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "foreign"}[owned], func(t *testing.T) {
			agent := newCR("new-held", api.PhasePending, nil, 1, 0, "")
			agent.Finalizers = nil
			agent.Spec.Team = "governed"
			team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "governed"},
				Spec: api.TeamSpec{AgentPolicyRef: "normal"}}
			namespace := instanceNamespace(agent.Name)
			ownSandboxResources(agent, namespace)
			if !owned {
				namespace.OwnerReferences[0].UID = "previous-agent"
			}
			c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).
				WithObjects(agent, team, namespace, gatewayTokenSecret(agent.Name)).Build()
			helm := &sandboxHoldDeletionHelmClient{}
			r := &controller.AgentReconciler{Client: c, HelmClient: helm}
			key := types.NamespacedName{Name: agent.Name}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), key, agent); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(agent.Finalizers, holdFinalizer) {
				t.Fatal("new held Agent has no deletion finalizer")
			}
			if err := c.Delete(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if (err != nil) == owned {
				t.Fatalf("unexpected deletion result: %v", err)
			}
			if owned {
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(namespace), namespace); !apierrors.IsNotFound(err) {
					t.Fatalf("owned namespace cleanup not requested: %v", err)
				}
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(context.Background(), key, agent); !apierrors.IsNotFound(err) {
					t.Fatalf("held Agent not finalized: %v", err)
				}
			} else {
				if err := c.Get(context.Background(), key, agent); err != nil || !slices.Contains(agent.Finalizers, holdFinalizer) {
					t.Fatal("foreign namespace cleanup lost its recovery finalizer")
				}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(namespace), namespace); err != nil {
					t.Fatalf("foreign namespace removed: %v", err)
				}
			}
			if helm.prepareCalls != 0 || helm.installCalled || helm.uninstallCalled {
				t.Fatal("held lifecycle invoked Helm")
			}
		})
	}
}

func TestSandboxBuiltinDeletePreconditions(t *testing.T) {
	for _, target := range []string{"route", "namespace"} {
		t.Run(target, func(t *testing.T) {
			agent := newCR("replacement", api.PhaseReady, nil, 1, 1, "")
			agent.Spec.SandboxRef = "standard"
			namespace := instanceNamespace(agent.Name)
			route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-replacement", Namespace: namespace.Name}}
			ownSandboxResources(agent, namespace, route)
			if target == "namespace" {
				now := metav1.Now()
				agent.DeletionTimestamp = &now
			}
			replaced := false
			c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).WithObjects(agent, namespace, route).
				WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					_, isRoute := obj.(*istiov1beta1.VirtualService)
					if (target == "route") == isRoute {
						replaced = true
						options := &client.DeleteOptions{}
						options.ApplyOptions(opts)
						if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != obj.GetUID() ||
							options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != obj.GetResourceVersion() {
							t.Fatal("delete was not bound to the verified object")
						}
						if err := c.Delete(ctx, obj); err != nil {
							return err
						}
						replacement := obj.DeepCopyObject().(client.Object)
						replacement.SetUID("replacement-uid")
						replacement.SetResourceVersion("")
						if err := c.Create(ctx, replacement); err != nil {
							return err
						}

					}
					return c.Delete(ctx, obj, opts...)
				}}).Build()
			r := &controller.AgentReconciler{Client: c}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}); err == nil || !replaced {
				t.Fatal("replacement race was not rejected")
			}
			var replacement client.Object = namespace
			if target == "route" {
				replacement = route
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(replacement), replacement); err != nil || replacement.GetUID() != "replacement-uid" {
				t.Fatalf("replacement object was deleted: %v", err)
			}
		})
	}
}

func TestSandboxHoldUsesUncachedOwnership(t *testing.T) {
	agent := newCR("uncached", api.PhaseReady, nil, 1, 1, "")
	agent.Spec.SandboxRef = "standard"
	sts := statefulSet(agent.Name, 1)
	ownSandboxResources(agent, sts)
	namespace := instanceNamespace(agent.Name)
	cached := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).
		WithObjects(agent, namespace, sts).Build()
	foreign := sts.DeepCopy()
	foreign.OwnerReferences[0].UID = "another-agent"
	reader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(namespace, foreign).Build()
	r := &controller.AgentReconciler{Client: cached, APIReader: reader}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}); err == nil {
		t.Fatal("stale cached ownership was accepted")
	}
	if err := cached.Get(context.Background(), client.ObjectKeyFromObject(sts), sts); err != nil || *sts.Spec.Replicas != 1 {
		t.Fatal("workload changed using stale ownership")
	}
}

func TestSandboxCustomDeletionRemovesHoldFinalizer(t *testing.T) {
	t.Skip("Custom harnesses cannot receive a Policy policy")
	scenario := newCompletionScenario(t)
	if err := scenario.step(); err != nil {
		t.Fatal(err)
	}
	scenario.observeApplied()
	agent := scenario.current()
	agent.Spec.SandboxRef = "standard"
	if err := scenario.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := scenario.step(); err != nil {
		t.Fatal(err)
	}
	agent = scenario.current()
	if !slices.Contains(agent.Finalizers, holdFinalizer) {
		t.Fatal("Custom hold is not persisted")
	}
	if err := scenario.client.Delete(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := scenario.step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := scenario.client.Get(context.Background(), client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Custom deletion left the hold finalizer behind: %v", err)
	}
}

func TestSandboxDeletionWaitsForNamespaceWithoutStatusChurn(t *testing.T) {
	ctx := context.Background()
	agent := newCR("deleting", api.PhaseFailed, nil, 1, 1, "")
	agent.Finalizers = []string{api.Finalizer, holdFinalizer}
	namespace := instanceNamespace(agent.Name)
	ownSandboxResources(agent, namespace)
	namespace.Finalizers = []string{"example.com/cleanup"}
	statusWrites := 0
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(agent).
		WithObjects(agent, namespace).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if subResource == "status" {
				statusWrites++
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	if err := c.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	r := &controller.AgentReconciler{Client: c}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
	for range 3 {
		result, err := r.Reconcile(ctx, req)
		if err != nil || result.RequeueAfter == 0 {
			t.Fatalf("deletion did not wait for the namespace: result=%+v, err=%v", result, err)
		}
		if err := c.Get(ctx, req.NamespacedName, agent); err != nil || !slices.Contains(agent.Finalizers, holdFinalizer) {
			t.Fatal("Agent was finalized while its namespace was still terminating")
		}
	}
	if statusWrites != 1 {
		t.Fatalf("unchanged deletion status was rewritten: got %d writes, want 1", statusWrites)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {
		t.Fatal(err)
	}
	namespace.Finalizers = nil
	if err := c.Update(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, agent); !apierrors.IsNotFound(err) {
		t.Fatalf("Agent was not finalized after namespace removal: %v", err)
	}
}
