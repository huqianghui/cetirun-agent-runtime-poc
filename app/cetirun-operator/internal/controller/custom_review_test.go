// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func runningCustomReview(t *testing.T) *completionScenario {
	t.Helper()
	s := newCompletionScenario(t)
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	s.observeApplied()
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *completionScenario) workload() *appsv1.StatefulSet {
	s.t.Helper()
	workload := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: s.provider.bindings.Namespace.Name, Name: "openclaw-" + s.agent.Name + "-cetirun"}
	if err := s.client.Get(context.Background(), key, workload); err != nil {
		s.t.Fatal(err)
	}
	return workload
}

func TestCustomOnDeleteReadinessAndSteadyCache(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := runningCustomReview(t)
	current := s.workload()
	current.Status.CurrentRevision = "old"
	current.Status.UpdateRevision = "new"
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	prepares := s.helm.prepareCalls
	for i := 0; i < 3; i++ {
		if err := s.step(); err != nil {
			t.Fatal(err)
		}
	}
	if s.current().Status.Phase != api.PhaseReady || s.helm.prepareCalls != prepares {
		t.Fatal("OnDelete revision bookkeeping or steady-state cache is incorrect")
	}
	if s.current().Status.LastReconcileTime == nil {
		t.Fatal("Custom status has no reconciliation timestamp")
	}
}

func TestCustomPlatformAndReceiptUpdatesDoNotStopWriter(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, change := range []string{"network", "pdb", "namespace-revision", "generation", "ttyd"} {
		t.Run(change, func(t *testing.T) {
			s := runningCustomReview(t)
			before := s.workload().DeepCopy()
			oldHash := s.helm.lastValues["runtime"].(map[string]any)["workloadHash"]
			approvals := s.provider.store.approvals
			s.helm.installCalled = false
			switch change {
			case "network":
				s.reconciler.CetiRunHelmValues = map[string]any{"networkPolicy": map[string]any{"blockedCIDRs": []any{"10.0.0.0/8", "169.254.169.254/32"}}}
			case "pdb":
				s.reconciler.CetiRunHelmValues = map[string]any{"podDisruptionBudget": map[string]any{"enabled": true}}
			case "ttyd":
				s.reconciler.CetiRunHelmValues = map[string]any{"ttyd": map[string]any{"port": 9000, "shell": "/bin/sh"}}
			case "generation":
				current := s.current()
				current.Generation++
				current.Spec.User.DisplayName = "Updated label"
				if err := s.client.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
				s.provider.bindings.Generation = current.Generation
			case "namespace-revision":
				ns := &corev1.Namespace{}
				if err := s.client.Get(context.Background(), types.NamespacedName{Name: s.provider.bindings.Namespace.Name}, ns); err != nil {
					t.Fatal(err)
				}
				ns.Labels = map[string]string{"example.com/metadata": "updated"}
				if err := s.client.Update(context.Background(), ns); err != nil {
					t.Fatal(err)
				}
				s.provider.bindings.Namespace.Revision = ns.ResourceVersion
			}
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			if s.provider.store.approvals != approvals || !reflect.DeepEqual(before.Spec, s.workload().Spec) ||
				s.helm.lastValues["runtime"].(map[string]any)["workloadHash"] != oldHash || s.current().Status.Phase != api.PhaseReady {
				t.Fatal("platform-only change interrupted the writer")
			}
			if change != "ttyd" && !s.helm.installCalled {
				t.Fatal("platform/audit update was not applied")
			}
		})
	}
}

type reviewClient struct {
	client.Client
	podsForbidden bool
	noRoutes      bool
}

func (c reviewClient) List(ctx context.Context, obj client.ObjectList, options ...client.ListOption) error {
	if _, ok := obj.(*corev1.PodList); ok && c.podsForbidden {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", fmt.Errorf("synthetic denial"))
	}
	return c.Client.List(ctx, obj, options...)
}

func (c reviewClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, options ...client.GetOption) error {
	if _, ok := obj.(*istiov1beta1.VirtualService); ok && c.noRoutes {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "networking.istio.io", Kind: "VirtualService"}}
	}
	return c.Client.Get(ctx, key, obj, options...)
}

func TestCustomPodPermissionFailurePrecedesHandoff(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.reconciler.APIReader = reviewClient{Client: s.client, podsForbidden: true}
	_ = s.step()
	if s.provider.store.approvals != 0 || s.helm.installCalled {
		t.Fatal("missing Pod permissions reached lifecycle handoff")
	}
	c := meta.FindStatusCondition(s.current().Status.Conditions, api.ConditionAgentConfigReady)
	if c == nil || c.Reason != "CustomPermissionDenied" {
		t.Fatal("permission failure was not diagnosed")
	}
}

func TestCustomRouteErrorsAndPendingReleaseKeepObservedState(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, unavailable := range []string{"route-api", "pending-release", "registry"} {
		t.Run(unavailable, func(t *testing.T) {
			s := runningCustomReview(t)
			s.restart()
			if unavailable == "route-api" {
				s.reconciler.Client = reviewClient{Client: s.client, noRoutes: true}
			} else if unavailable == "pending-release" {
				s.helm.prepareErr = helm.ErrReleaseOperationPending
			} else {
				s.helm.prepareErr = fmt.Errorf("synthetic registry timeout")
			}
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			if s.current().Status.Phase != api.PhaseReady {
				t.Fatal("dependency failure discarded observed Ready workload")
			}
			if unavailable == "route-api" {
				c := meta.FindStatusCondition(s.current().Status.Conditions, api.ConditionIngressReady)
				if c == nil || c.Status != metav1.ConditionFalse {
					t.Fatal("unavailable route API was not reported")
				}
			}
		})
	}
}

type observedProvider struct {
	*recoveryProvider
	deadline bool
	fail     bool
}

func (p *observedProvider) Resolve(ctx context.Context, agent *api.Agent, team *api.Team) (runtimebindings.Custom, error) {
	_, p.deadline = ctx.Deadline()
	if p.fail {
		return runtimebindings.Custom{}, fmt.Errorf("synthetic provider outage")
	}
	return p.recoveryProvider.Resolve(ctx, agent, team)
}

func TestCustomProviderDeadlineAndOutage(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := runningCustomReview(t)
	provider := &observedProvider{recoveryProvider: s.provider, fail: true}
	s.reconciler.CustomBindings = provider
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if !provider.deadline || s.current().Status.Phase != api.PhaseReady {
		t.Fatal("provider deadline or independent running observation was lost")
	}
	condition := meta.FindStatusCondition(s.current().Status.Conditions, api.ConditionAgentConfigReady)
	if condition == nil || condition.Reason != "CustomBindingsPending" {
		t.Fatal("provider outage not reported separately")
	}
}

func TestCustomForeignRouteIsNotAdoptedOrDeleted(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := runningCustomReview(t)
	key := types.NamespacedName{Namespace: s.provider.bindings.Namespace.Name, Name: "cetirun-" + s.agent.Name}
	route := &istiov1beta1.VirtualService{}
	if err := s.client.Get(context.Background(), key, route); err != nil {
		t.Fatal(err)
	}
	route.OwnerReferences[0].UID = "foreign"
	if err := s.client.Update(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	version := route.ResourceVersion
	_ = s.step()
	if err := s.client.Get(context.Background(), key, route); err != nil || route.ResourceVersion != version {
		t.Fatal("foreign route was updated")
	}
	current := s.current()
	current.Spec.Suspended = true
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	_ = s.step()
	if err := s.client.Get(context.Background(), key, route); err != nil || route.ResourceVersion != version {
		t.Fatal("foreign route was deleted or changed on suspension")
	}
	if s.current().Status.Phase != api.PhaseSuspended || *s.workload().Spec.Replicas != 0 {
		t.Fatal("route conflict hid the completed suspension")
	}
}

func TestCustomBlockedDeletionRevokesAccessAndStopsWorkload(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.provider.store.failures = 5
	_ = s.step()
	s.observeApplied()
	if err := s.client.Delete(context.Background(), s.current()); err != nil {
		t.Fatal(err)
	}
	_ = s.step()
	if s.current().Status.Phase != api.PhaseDeleting || *s.workload().Spec.Replicas != 0 || s.pending() == nil {
		t.Fatal("blocked deletion did not quiesce the instance")
	}
	route := &istiov1beta1.VirtualService{}
	err := s.client.Get(context.Background(), types.NamespacedName{Namespace: s.provider.bindings.Namespace.Name, Name: "cetirun-" + s.agent.Name}, route)
	if !apierrors.IsNotFound(err) {
		t.Fatal("blocked deletion retained an owned route")
	}
}

func TestCustomDeletionAttemptsShutdownWhenSandboxRevocationFails(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, trigger := range []string{"retained hold", "enabled preparation"} {
		for _, failure := range []string{"journal only", "workload", "terminal", "route", "status", "all"} {
			t.Run(trigger+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				s := newCompletionScenario(t)
				enableTerminal(t, s, true)
				if err := s.step(); err != nil {
					t.Fatal(err)
				}
				s.observeApplied()
				if err := s.step(); err != nil {
					t.Fatal(err)
				}
				agent := s.current()
				if agent.Status.Phase != api.PhaseReady {
					t.Fatal("fixture did not reach a running Custom Agent")
				}
				agent.Status.AccessURL = "https://platform.example.com/completion/"
				if err := s.client.Status().Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				s.reconciler.OperatorNamespace = "cetirun-system"
				if trigger == "retained hold" {
					agent.Finalizers = append(agent.Finalizers, sandbox.HoldFinalizer)
					if err := s.client.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
				} else {
					s.reconciler.SandboxPreparation = &controller.SandboxPreparation{}
				}
				if err := s.client.Delete(ctx, agent); err != nil {
					t.Fatal(err)
				}
				finalizers := slices.Clone(s.current().Finalizers)
				namespace := &corev1.Namespace{}
				if err := s.client.Get(ctx, client.ObjectKey{Name: s.provider.bindings.Namespace.Name}, namespace); err != nil {
					t.Fatal(err)
				}
				namespaceVersion := namespace.ResourceVersion
				home := &corev1.PersistentVolumeClaim{}
				homeKey := client.ObjectKey{Namespace: namespace.Name, Name: s.provider.bindings.Home.Name}
				if err := s.client.Get(ctx, homeKey, home); err != nil {
					t.Fatal(err)
				}
				homeVersion := home.ResourceVersion
				injected := map[string]error{
					"journal":  errors.New("synthetic journal read failure"),
					"workload": errors.New("synthetic workload stop failure"),
					"terminal": errors.New("synthetic terminal stop failure"),
					"route":    errors.New("synthetic route removal failure"),
					"status":   errors.New("synthetic status update failure"),
				}
				fails := func(operation string) bool { return failure == operation || failure == "all" }
				calls := map[string]int{}
				wrapped := interceptor.NewClient(s.client.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if key.Namespace == "cetirun-system" && key.Name == sandboxpolicy.GrantJournalName(agent.UID) {
							calls["journal"]++
							return injected["journal"]
						}
						return c.Get(ctx, key, object, options...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						var operation string
						switch object.(type) {
						case *appsv1.StatefulSet:
							operation = "workload"
						case *appsv1.Deployment:
							operation = "terminal"
						}
						if operation != "" {
							calls[operation]++
							if fails(operation) {
								return injected[operation]
							}
						}
						return c.Patch(ctx, object, patch, options...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
						if _, ok := object.(*istiov1beta1.VirtualService); ok {
							calls["route"]++
							if fails("route") {
								return injected["route"]
							}
						} else {
							calls["destructive cleanup"]++
						}
						return c.Delete(ctx, object, options...)
					},
					SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
						if subresource == "status" {
							calls["status"]++
							if fails("status") {
								return injected["status"]
							}
						}
						return c.SubResource(subresource).Patch(ctx, object, patch, options...)
					},
				})
				s.reconciler.Client, s.reconciler.APIReader = wrapped, wrapped
				err := s.step()
				if !errors.Is(err, injected["journal"]) {
					t.Fatal("journal revocation failure was lost:", err)
				}
				for _, operation := range []string{"workload", "terminal", "route", "status"} {
					if calls[operation] != 1 {
						t.Fatalf("journal failure skipped %s: calls=%v error=%v", operation, calls, err)
					}
					if fails(operation) && !errors.Is(err, injected[operation]) {
						t.Fatalf("%s failure was hidden: %v", operation, err)
					}
				}
				if calls["journal"] != 1 || calls["destructive cleanup"] != 0 {
					t.Fatalf("failed revocation did not block destructive cleanup: %v", calls)
				}
				agent = s.current()
				if agent.DeletionTimestamp.IsZero() || !slices.Equal(agent.Finalizers, finalizers) {
					t.Fatal("failed revocation released lifecycle finalizers")
				}
				if !fails("status") {
					if agent.Status.Phase != api.PhaseDeleting || agent.Status.AccessURL != "" {
						t.Fatal("failed revocation retained running status or access URL")
					}
					for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionIngressReady,
						api.ConditionDesktopConfigured, api.ConditionTerminalConfigured} {
						if !meta.IsStatusConditionFalse(agent.Status.Conditions, condition) {
							t.Fatalf("failed revocation retained %s readiness", condition)
						}
					}
				}
				workload := s.workload()
				deployment := &appsv1.Deployment{}
				if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: terminalpolicy.WorkloadName(workload.Name)}, deployment); err != nil {
					t.Fatal(err)
				}
				for operation, replicas := range map[string]*int32{"workload": workload.Spec.Replicas, "terminal": deployment.Spec.Replicas} {
					want := int32(0)
					if fails(operation) {
						want = 1
					}
					if ptr.Deref(replicas, -1) != want {
						t.Fatalf("%s replicas=%v, want %d", operation, replicas, want)
					}
				}
				routeErr := s.client.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: "cetirun-" + agent.Name}, &istiov1beta1.VirtualService{})
				if fails("route") && routeErr != nil || !fails("route") && !apierrors.IsNotFound(routeErr) {
					t.Fatal("route removal did not run independently:", routeErr)
				}
				if err := s.client.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil ||
					!namespace.DeletionTimestamp.IsZero() || namespace.ResourceVersion != namespaceVersion {
					t.Fatal("namespace changed before successful revocation:", err)
				}
				if err := s.client.Get(ctx, homeKey, home); err != nil || home.ResourceVersion != homeVersion {
					t.Fatal("HOME changed before successful revocation:", err)
				}
				if failure == "journal only" {
					s.reconciler.Client, s.reconciler.APIReader = s.client, s.client
					for range 2 {
						if err := s.step(); err != nil {
							t.Fatal("Custom deletion did not recover after the journal outage:", err)
						}
					}
					if err := s.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("recovered cleanup retained the Agent:", err)
					}
				}
			})
		}
	}
}

func TestCustomDeletionStopsBeforeRetainedRegistryCleanup(t *testing.T) {
	requireLegacyCustomRuntime(t)
	ctx := context.Background()
	s := newCompletionScenario(t)
	enableTerminal(t, s, true)
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	s.observeApplied()
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	agent := s.current()
	agent.Finalizers = append(agent.Finalizers, sandbox.HoldFinalizer)
	if err := s.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	s.reconciler.OperatorNamespace = "cetirun-system"
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "registry-source-retained", Namespace: registryauth.DefaultNamespace, UID: "retained-registry-uid",
		OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)},
	}}
	if err := s.client.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	agent.Status.RegistryCredentials = &api.RegistryCredentialsStatus{SourceUID: string(source.UID)}
	agent.Status.AccessURL = "https://platform.example.com/completion/"
	if err := s.client.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	s.reconciler.APIReader = interceptor.NewClient(s.client.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if _, ok := list.(*corev1.SecretList); ok {
				return errors.New("synthetic registry cleanup outage")
			}
			return c.List(ctx, list, options...)
		},
	})
	result, err := s.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("registry cleanup outage did not retain a bounded retry: %+v %v", result, err)
	}
	current := s.current()
	if current.Status.Phase != api.PhaseDeleting || current.Status.AccessURL != "" ||
		!slices.Equal(current.Finalizers, agent.Finalizers) ||
		!meta.IsStatusConditionFalse(current.Status.Conditions, api.ConditionRegistryCredentialsReady) ||
		!meta.IsStatusConditionFalse(current.Status.Conditions, api.ConditionReady) {
		t.Fatal("registry cleanup failure lost the deletion hold or status")
	}
	workload := s.workload()
	deployment := &appsv1.Deployment{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: terminalpolicy.WorkloadName(workload.Name)}, deployment); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(workload.Spec.Replicas, -1) != 0 || ptr.Deref(deployment.Spec.Replicas, -1) != 0 {
		t.Fatal("registry cleanup failure skipped Custom workload or terminal shutdown")
	}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: "cetirun-" + agent.Name}, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatal("registry cleanup failure retained the owned Custom route:", err)
	}
	if err := s.client.Get(ctx, client.ObjectKey{Name: workload.Namespace}, &corev1.Namespace{}); err != nil {
		t.Fatal("namespace was removed before registry cleanup:", err)
	}
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal("registry cleanup outage did not retain its retry state:", err)
	}
	s.reconciler.APIReader = s.client
	for range 2 {
		if err := s.step(); err != nil {
			t.Fatal("Custom deletion could not resume after registry recovery:", err)
		}
	}
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(source), source); !apierrors.IsNotFound(err) {
		t.Fatal("Custom deletion skipped retained registry credential cleanup:", err)
	}
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatal("Custom deletion did not finish after registry recovery:", err)
	}
}

func TestCustomDesktopDisableAndEnableCleansRoute(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := runningCustomReview(t)
	s.provider.approve = func(sts *appsv1.StatefulSet) error {
		sts.Spec.Replicas = ptr.To[int32](0)
		return s.client.Update(context.Background(), sts)
	}
	for _, enabled := range []bool{false, true} {
		current := s.current()
		current.Generation++
		current.Spec.Helm.Values["desktop"] = runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"enabled":%v}`, enabled))}
		if err := s.client.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		s.provider.bindings.Generation = current.Generation
		if err := s.step(); err != nil {
			t.Fatal(err)
		}
		route := &istiov1beta1.VirtualService{}
		err := s.client.Get(context.Background(), types.NamespacedName{Namespace: s.provider.bindings.Namespace.Name, Name: "cetirun-" + s.agent.Name}, route)
		if (err == nil) != enabled {
			t.Fatalf("desktop=%v route=%v", enabled, err)
		}
		if err := s.client.Delete(context.Background(), s.workload()); err != nil {
			t.Fatal(err)
		}
		s.observeApplied()
	}
}

func TestCustomDeletingOrphanedRecordsCanFinish(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	s.provider.store.failures = 1
	_ = s.step()
	s.observeApplied()
	if err := s.client.Delete(context.Background(), s.current()); err != nil {
		t.Fatal(err)
	}
	records := &corev1.ConfigMapList{}
	if err := s.client.List(context.Background(), records, client.InNamespace("cetirun-system")); err != nil {
		t.Fatal(err)
	}
	for i := range records.Items {
		records.Items[i].OwnerReferences = nil
		if err := s.client.Update(context.Background(), &records.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	workload := s.workload()
	workload.OwnerReferences = nil
	if err := s.client.Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	route := &istiov1beta1.VirtualService{}
	key := types.NamespacedName{Namespace: s.provider.bindings.Namespace.Name, Name: "cetirun-" + s.agent.Name}
	if err := s.client.Get(context.Background(), key, route); err != nil {
		t.Fatal(err)
	}
	route.OwnerReferences = nil
	if err := s.client.Update(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.client.Get(context.Background(), types.NamespacedName{Name: s.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphaned records blocked Agent finalization: %v", err)
	}
}

func TestControllerPlatformConfigurationParity(t *testing.T) {
	requireLegacyCustomRuntime(t)
	configuration := map[string]any{
		"tolerations": []any{map[string]any{"key": "agents", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
		"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
			"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{"key": "pool", "operator": "In", "values": []any{"agents"}}}}},
		}}},
		"topologySpreadConstraints": []any{map[string]any{"maxSkew": 1, "topologyKey": "kubernetes.io/hostname", "whenUnsatisfiable": "ScheduleAnyway",
			"labelSelector": map[string]any{"matchLabels": map[string]any{"app": "agents"}}}},
	}
	run := func(typ api.AgentType) map[string]map[string]any {
		cr, bindings, objects := customFixture("example", nil)
		if typ != api.AgentTypeCustom {
			cr = newCR("example", api.PhasePending, nil, 1, 0, "")
			// Let the selected chart registry pin its qualified image and port;
			// carrying newCR's Azure default would be an unqualified override.
			cr.Spec.Agent = api.HarnessSpec{Type: typ}
			objects = []client.Object{cr}
		}
		scheme := testScheme()
		c := provisioningClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
		h := &fakeHelmClient{useHarness: true}
		r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: h,
			CustomBindings: &customProvider{bindings: bindings}, CetiRunHelmValues: configuration,
			ChartRegistry: "registry.example.com", DefaultModel: "test-model", DomainSuffix: "platform.example.com",
			IstioRevision: "test", EventRecorder: record.NewFakeRecorder(100)}
		controller.ConfigureTestGateway(t, r)
		for i := 0; i < 4 && !h.installCalled; i++ {
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
				t.Fatal(err)
			}
		}
		if !h.installCalled {
			current := &api.Agent{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(cr), current); err != nil {
				t.Fatal(err)
			}
			t.Fatalf("%s did not reach the actual preparation path: status=%+v", typ, current.Status)
		}
		network := h.lastValues["networkPolicy"].(map[string]any)
		if network["istioNamespace"] != "aks-istio-system" || network["istioIngressNamespace"] != "aks-istio-ingress" {
			t.Fatalf("%s lost platform defaults: %v", typ, network)
		}
		desktop := h.lastValues["desktop"].(map[string]any)
		if desktop["novncPathPrefix"] != "example/desktop/" || desktop["image"].(map[string]any)["repository"] != "registry.example.com/cetirun-desktop" {
			t.Fatal("controller did not preserve desktop routing/image defaults")
		}
		ch, err := loader.Load("../../../cetirun-chart")
		if err != nil {
			t.Fatal(err)
		}
		caps := chartutil.DefaultCapabilities.Copy()
		caps.APIVersions = append(caps.APIVersions, "networking.istio.io/v1/Sidecar")
		values, err := chartutil.ToRenderValues(ch, h.lastValues, chartutil.ReleaseOptions{Name: "openclaw-example", Namespace: "cetirun-example", IsInstall: true}, caps)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := engine.Render(ch, values)
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]map[string]any{}
		for filename, contents := range rendered {
			if strings.HasSuffix(filename, "NOTES.txt") {
				continue
			}
			for _, document := range strings.Split(contents, "\n---") {
				var object map[string]any
				if err := yaml.Unmarshal([]byte(document), &object); err != nil {
					t.Fatal(err)
				}
				if len(object) != 0 {
					result[object["kind"].(string)] = object
				}
			}
		}
		return result
	}
	custom := run(api.AgentTypeCustom)
	for _, typ := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		native := run(typ)
		withoutGatewayEgress := func(egress []any) []any {
			shared := make([]any, 0, len(egress))
			for _, item := range egress {
				rule, _ := item.(map[string]any)
				gateway := false
				for _, target := range rule["to"].([]any) {
					peer, _ := target.(map[string]any)
					selector, _ := peer["podSelector"].(map[string]any)
					labels, _ := selector["matchLabels"].(map[string]any)
					gateway = gateway || labels["app.kubernetes.io/name"] == "cetirun-ai-gateway"
				}
				if !gateway {
					shared = append(shared, item)
				}
			}
			return shared
		}
		customEgress := custom["NetworkPolicy"]["spec"].(map[string]any)["egress"].([]any)
		nativeEgress := native["NetworkPolicy"]["spec"].(map[string]any)["egress"].([]any)
		if !reflect.DeepEqual(withoutGatewayEgress(customEgress), withoutGatewayEgress(nativeEgress)) {
			t.Fatal("shared network egress diverged across controller paths")
		}
		hosts := func(objects map[string]map[string]any) []any {
			return objects["Sidecar"]["spec"].(map[string]any)["egress"].([]any)[0].(map[string]any)["hosts"].([]any)
		}
		if !reflect.DeepEqual(hosts(custom)[:len(hosts(native))], hosts(native)) {
			t.Fatal("shared mesh discovery scope diverged")
		}
		pod := func(objects map[string]map[string]any) map[string]any {
			return objects["StatefulSet"]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		}
		for _, field := range []string{"tolerations", "affinity", "topologySpreadConstraints", "runtimeClassName", "nodeSelector"} {
			if !reflect.DeepEqual(pod(custom)[field], pod(native)[field]) {
				t.Fatalf("platform scheduling %s diverged", field)
			}
		}
	}
}
