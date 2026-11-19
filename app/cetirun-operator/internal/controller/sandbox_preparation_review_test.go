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
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSandboxPreparationUnsupportedIntegrationsRemainCleanlyHeld(t *testing.T) {
	for name, mutate := range map[string]func(*api.Agent){
		"tailscale": func(a *api.Agent) { a.Spec.Tailscale = &api.TailscaleSpec{AuthKeySecretRef: "example-vpn"} },
		"identity":  func(a *api.Agent) { a.Spec.AgentIdentity = &api.AgentIdentitySpec{Enabled: true} },
	} {
		for _, previouslyPrepared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prepared=%v", name, previouslyPrepared), func(t *testing.T) {
				f := newPreparationFixture(t, "openclaw")
				if previouslyPrepared {
					if err := f.reconcile(); err != nil {
						t.Fatal(err)
					}
				}
				agent := f.current(t)
				mutate(agent)
				agent.Generation++
				if err := f.client.Update(context.Background(), agent); err != nil {
					t.Fatal(err)
				}
				stages, resolves := f.installer.stages, f.source.resolves
				result, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: agent.Name}})
				if err != nil || result.RequeueAfter != time.Minute {
					t.Fatalf("unsupported configuration entered an error/backoff loop: %+v %v", result, err)
				}
				agent = f.current(t)
				condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "UnsupportedControl" ||
					agent.Status.AccessURL != "" || !slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) ||
					f.installer.stages != stages || f.source.resolves != resolves {
					t.Fatal("unsupported configuration claimed progress or entered preparation")
				}
				if previouslyPrepared {
					bindings, err := (&sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}).
						Read(context.Background(), agent.Name, agent.UID)
					if err != nil || len(bindings) != 0 {
						t.Fatalf("unsupported transition retained prior grants: %+v %v", bindings, err)
					}
				}
			})
		}
	}
}

func TestSandboxPreparationUnsupportedProviderControlIsNotRetryableFailure(t *testing.T) {
	unsupported := fmt.Errorf("%w: synthetic unimplemented control", sandboxpolicy.ErrUnsupportedControl)
	for _, tc := range []struct {
		name   string
		cause  error
		clean  bool
		reason string
	}{
		{"single", unsupported, true, "UnsupportedControl"},
		{"joined-unsupported", fmt.Errorf("synthetic source: %w", errors.Join(unsupported, sandboxpolicy.ErrUnsupportedControl)), true, "UnsupportedControl"},
		{"joined-unavailable", errors.Join(unsupported, sandboxpolicy.ErrUnavailable), false, "ObservationUnavailable"},
		{"joined-deadline", errors.Join(unsupported, context.DeadlineExceeded), false, "PolicyApplyFailed"},
		{"joined-cancellation", errors.Join(unsupported, context.Canceled), false, "PolicyApplyFailed"},
		{"joined-other-failure", errors.Join(unsupported, errors.New("synthetic source failure")), false, "PolicyApplyFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreparationFixture(t, "hermes")
			f.source.resolveErr = tc.cause
			result, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: f.fixture.Agent.Name}})
			condition := apimeta.FindStatusCondition(f.current(t).Status.Conditions, api.ConditionAgentConfigReady)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != tc.reason || f.installer.stages != 0 {
				t.Fatalf("unsupported control did not remain held: %+v %v", condition, err)
			}
			if tc.clean {
				if err != nil || result.RequeueAfter != time.Minute || condition.Reason != "UnsupportedControl" {
					t.Fatalf("unsupported control was treated as missing prerequisites: %+v %v", result, err)
				}
			} else if !errors.Is(err, tc.cause) || result.RequeueAfter != 0 {
				t.Fatalf("unsupported hold hid an operational failure: %+v %+v %v", result, condition, err)
			}
		})
	}
}

func TestSandboxPreparationPlatformPrerequisiteClassification(t *testing.T) {
	readFailure := errors.New("synthetic RuntimeClass read failure")
	for _, tc := range []struct {
		name   string
		mutate func(*preparationFixture)
		clean  bool
		cause  error
		reason string
		status metav1.ConditionStatus
	}{
		{"unqualified adapter", func(f *preparationFixture) {
			platform := f.config.Platforms[api.SandboxKata]
			platform.Adapter.Qualified = false
			f.config.Platforms[api.SandboxKata] = platform
		}, true, nil, "UnsupportedControl", metav1.ConditionFalse},
		{"unsupported adapter", func(f *preparationFixture) {
			platform := f.config.Platforms[api.SandboxKata]
			platform.Adapter.Name = "synthetic-unsupported-adapter"
			f.config.Platforms[api.SandboxKata] = platform
		}, true, nil, "UnsupportedControl", metav1.ConditionFalse},
		{"unavailable adapter", func(f *preparationFixture) {
			platform := f.config.Platforms[api.SandboxKata]
			platform.Adapter.Available = false
			f.config.Platforms[api.SandboxKata] = platform
		}, false, sandboxpolicy.ErrUnavailable, "ObservationUnavailable", metav1.ConditionUnknown},
		{"missing platform inventory", func(f *preparationFixture) {
			delete(f.config.Platforms, api.SandboxKata)
		}, false, sandboxpolicy.ErrUnavailable, "ObservationUnavailable", metav1.ConditionUnknown},
		{"RuntimeClass read failure", func(f *preparationFixture) {
			f.reconciler.APIReader = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if _, ok := object.(*nodev1.RuntimeClass); ok {
						return readFailure
					}
					return c.Get(ctx, key, object, options...)
				},
			})
		}, false, readFailure, "PolicyApplyFailed", metav1.ConditionFalse},
	} {
		for _, previouslyPrepared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prepared=%v", tc.name, previouslyPrepared), func(t *testing.T) {
				f := newPreparationFixture(t, "hermes")
				if previouslyPrepared {
					if err := f.reconcile(); err != nil {
						t.Fatal(err)
					}
				}
				stages := f.installer.stages
				tc.mutate(f)
				result, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{
					NamespacedName: client.ObjectKey{Name: f.fixture.Agent.Name},
				})
				agent := f.current(t)
				condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionAgentConfigReady)
				sandboxCondition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
				if condition == nil || condition.Status != metav1.ConditionFalse ||
					condition.Reason != tc.reason || sandboxCondition == nil || sandboxCondition.Status != tc.status ||
					condition.ObservedGeneration != agent.Generation || agent.Status.AccessURL != "" ||
					!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) ||
					!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionReady) ||
					f.installer.stages != stages {
					t.Fatalf("failed prerequisites did not retain the execution hold: %+v %v", agent.Status, err)
				}
				if tc.clean {
					if err != nil || result.RequeueAfter != time.Minute || condition.Reason != "UnsupportedControl" ||
						agent.Status.Phase != api.PhaseFailed {
						t.Fatalf("unsupported platform entered an outage/backoff loop: %+v %+v %v", result, condition, err)
					}
				} else if !errors.Is(err, tc.cause) || result.RequeueAfter != 0 ||
					agent.Status.Phase != api.PhasePending {
					t.Fatalf("an unavailable prerequisite was hidden by an unsupported hold: %+v %+v %v", result, condition, err)
				}
				if previouslyPrepared {
					bindings, err := (&sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}).
						Read(context.Background(), agent.Name, agent.UID)
					if err != nil || len(bindings) != 0 {
						t.Fatalf("failed prerequisites retained previous grants: %+v %v", bindings, err)
					}
				}
			})
		}
	}
}

func TestSandboxPreparationDisabledUntrackedDoesNotReadState(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	f.reconciler.SandboxPreparation = nil
	stateReads := 0
	reader := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, ok := object.(*corev1.ConfigMap); ok {
				stateReads++
				return errors.New("unexpected preparation state access")
			}
			return c.Get(ctx, key, object, options...)
		},
	})
	f.reconciler.APIReader = reader
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	if stateReads != 0 {
		t.Fatal("configured operator namespace activated untracked preparation state")
	}
}

func TestSandboxPreparationInterruptedFinalizationRetainsStateAfterProviderDisablement(t *testing.T) {
	f := newPreparationFixture(t, "openclaw")
	ctx := context.Background()
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("synthetic interrupted journal finalization")
	fired := false
	f.reconciler.APIReader = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if key.Name == sandboxpolicy.GrantJournalName(f.fixture.Agent.UID) && !fired {
				agent := &api.Agent{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(f.fixture.Agent), agent); err != nil {
					return err
				}
				if !slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) {
					fired = true
					return injected
				}
			}
			return c.Get(ctx, key, object, options...)
		},
	})
	if err := f.reconcile(); !errors.Is(err, injected) || !fired {
		t.Fatalf("fixture did not interrupt cleanup after marker release: %v", err)
	}
	if slices.Contains(f.current(t).Finalizers, sandbox.PreparationFinalizer) {
		t.Fatal("fixture must reach the released-marker cleanup phase")
	}
	preparation := f.reconciler.SandboxPreparation
	f.reconciler.SandboxPreparation = nil
	if err := f.reconcile(); err == nil || !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
		t.Fatal("provider disablement discarded interrupted state cleanup")
	}
	f.reconciler.SandboxPreparation = preparation
	if err := f.reconcile(); err != nil {
		t.Fatal("qualified cleanup retry failed:", err)
	}
}

func TestSandboxPreparationDeletionResumesReleasedEmptyJournal(t *testing.T) {
	const foreignFinalizer = "cleanup.example.com/retained"
	for _, tc := range []struct {
		name        string
		empty       bool
		terminating bool
		mutate      func(*corev1.ConfigMap)
		wantError   bool
	}{
		{name: "released live journal", empty: true},
		{name: "released terminating journal", empty: true, terminating: true},
		{name: "unprotected grant inventory", wantError: true},
		{name: "foreign empty journal", empty: true, wantError: true, mutate: func(object *corev1.ConfigMap) {
			object.OwnerReferences[0].UID = "different-agent-uid"
		}},
		{name: "corrupt empty journal", empty: true, wantError: true, mutate: func(object *corev1.ConfigMap) {
			for key := range object.Data {
				object.Data[key] = "not-json"
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreparationFixture(t, "openclaw")
			ctx := context.Background()
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			agent := f.current(t)
			if err := f.client.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			journal := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
			if tc.empty {
				bindings, err := journal.Read(ctx, agent.Name, agent.UID)
				if err != nil {
					t.Fatal(err)
				}
				if err := journal.Revoke(ctx, agent.Name, agent.UID, bindings); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.wantError {
				namespace := &corev1.Namespace{}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Delete(ctx, namespace); err != nil {
					t.Fatal(err)
				}
			}
			agent = f.current(t)
			agent.Finalizers = slices.DeleteFunc(agent.Finalizers, func(value string) bool {
				return value == sandbox.PreparationFinalizer
			})
			if err := f.client.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKey{Namespace: "cetirun-system", Name: sandboxpolicy.GrantJournalName(agent.UID)}
			object := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, key, object); err != nil {
				t.Fatal(err)
			}
			object.Finalizers = nil
			if tc.terminating {
				object.Finalizers = []string{foreignFinalizer}
			}
			if tc.mutate != nil {
				tc.mutate(object)
			}
			if err := f.client.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			if tc.terminating {
				if err := f.client.Delete(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			err := f.reconcile()
			if tc.wantError {
				if err == nil || !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
					t.Fatal("unsafe journal state allowed cleanup to complete:", err)
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), &corev1.Namespace{}); err != nil {
					t.Fatal("unsafe journal state allowed namespace removal:", err)
				}
				return
			}
			if err != nil {
				t.Fatal("interrupted journal release prevented deletion retry:", err)
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("completed journal cleanup retained the Agent:", err)
			}
			err = f.client.Get(ctx, key, object)
			if tc.terminating {
				if err != nil || object.DeletionTimestamp.IsZero() || !slices.Equal(object.Finalizers, []string{foreignFinalizer}) {
					t.Fatalf("cleanup did not preserve the foreign finalizer: %+v %v", object, err)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Fatal("released journal was not deleted:", err)
			}
			if err := f.reconcile(); err != nil {
				t.Fatal("completed deletion did not remain idempotent:", err)
			}
		})
	}
}

func TestSandboxBuiltinDeletionAttemptsShutdownWhenRevocationFails(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		for _, revocation := range []string{"journal read", "binding deletion"} {
			for _, failure := range []string{"none", "workload", "route", "status", "all"} {
				t.Run(typ+"/"+revocation+"/"+failure, func(t *testing.T) {
					ctx := context.Background()
					f := newPreparationFixture(t, typ)
					if err := f.reconcile(); err != nil {
						t.Fatal(err)
					}
					agent := f.current(t)
					expected := f.expectation(t)
					workload := &appsv1.StatefulSet{}
					workloadKey := client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}
					if err := f.client.Get(ctx, workloadKey, workload); err != nil {
						t.Fatal(err)
					}
					workload.Spec.Replicas = ptr.To[int32](1)
					workload.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
						WhenScaled: appsv1.DeletePersistentVolumeClaimRetentionPolicyType, WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
					}
					if err := f.client.Update(ctx, workload); err != nil {
						t.Fatal(err)
					}
					route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{
						Name: "cetirun-" + agent.Name, Namespace: workload.Namespace,
						OwnerReferences: []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)},
					}}
					registry := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
						Name: "registry-source-retained", Namespace: registryauth.DefaultNamespace, UID: "retained-registry-uid",
						OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)},
					}}
					for _, object := range []client.Object{route, registry} {
						if err := f.client.Create(ctx, object); err != nil {
							t.Fatal(err)
						}
					}
					agent.Status.Phase, agent.Status.AccessURL = api.PhaseReady, "https://platform.example.com/agent/"
					agent.Status.RegistryCredentials = &api.RegistryCredentialsStatus{SourceUID: string(registry.UID)}
					for _, condition := range []string{api.ConditionReady, api.ConditionSandboxReady, api.ConditionAgentConfigReady} {
						apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
							Type: condition, Status: metav1.ConditionTrue, Reason: "SyntheticRunning", ObservedGeneration: agent.Generation,
						})
					}
					if err := f.client.Status().Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if err := f.client.Delete(ctx, agent); err != nil {
						t.Fatal(err)
					}
					finalizers := slices.Clone(f.current(t).Finalizers)
					journalKey := client.ObjectKey{Namespace: "cetirun-system", Name: sandboxpolicy.GrantJournalName(agent.UID)}
					retained := []client.Object{
						f.fixture.Namespace.DeepCopy(), f.fixture.Input.Home.DeepCopy(), registry,
						&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: journalKey.Name, Namespace: journalKey.Namespace}},
					}
					versions := make([]string, len(retained))
					for i, object := range retained {
						if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
							t.Fatal(err)
						}
						versions[i] = object.GetResourceVersion()
					}
					fences, stages := f.source.fences, f.installer.stages
					injected := map[string]error{
						"revocation": fmt.Errorf("synthetic %s failure", revocation),
						"workload":   errors.New("synthetic workload stop failure"),
						"route":      errors.New("synthetic route removal failure"),
						"status":     errors.New("synthetic status patch failure"),
					}
					fails := func(operation string) bool { return failure == operation || failure == "all" }
					calls := map[string]int{}
					wrapped := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
							if key == journalKey && revocation == "journal read" {
								calls["revocation"]++
								return injected["revocation"]
							}
							return c.Get(ctx, key, object, options...)
						},
						Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
							if _, ok := object.(*appsv1.StatefulSet); ok {
								calls["workload"]++
								if fails("workload") {
									return injected["workload"]
								}
							}
							return c.Patch(ctx, object, patch, options...)
						},
						Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
							switch object.(type) {
							case *rbacv1.RoleBinding:
								calls["revocation"]++
								return injected["revocation"]
							case *istiov1beta1.VirtualService:
								calls["route"]++
								if fails("route") {
									return injected["route"]
								}
							default:
								calls["destructive cleanup"]++
							}
							return c.Delete(ctx, object, options...)
						},
						List: func(ctx context.Context, c client.WithWatch, objects client.ObjectList, options ...client.ListOption) error {
							if _, ok := objects.(*corev1.SecretList); ok {
								calls["registry cleanup"]++
							}
							return c.List(ctx, objects, options...)
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
					f.reconciler.Client, f.reconciler.APIReader = wrapped, wrapped
					err := f.reconcile()
					if !errors.Is(err, injected["revocation"]) {
						t.Fatal("revocation failure was lost:", err)
					}
					for _, operation := range []string{"workload", "route", "status"} {
						if calls[operation] != 1 || fails(operation) && !errors.Is(err, injected[operation]) {
							t.Fatalf("%s was skipped or its error lost: calls=%v err=%v", operation, calls, err)
						}
					}
					if calls["revocation"] != 1 || calls["destructive cleanup"] != 0 || calls["registry cleanup"] != 0 ||
						f.source.fences != fences || f.installer.stages != stages {
						t.Fatalf("failed shutdown advanced cleanup or preparation: %v", calls)
					}
					current := f.current(t)
					if current.DeletionTimestamp.IsZero() || !slices.Equal(current.Finalizers, finalizers) {
						t.Fatal("failed shutdown released lifecycle finalizers")
					}
					if !fails("status") {
						if current.Status.Phase != api.PhaseDeleting || current.Status.AccessURL != "" {
							t.Fatal("deletion retained running status or access")
						}
						for _, condition := range []string{api.ConditionReady, api.ConditionSandboxReady, api.ConditionAgentConfigReady} {
							if !apimeta.IsStatusConditionFalse(current.Status.Conditions, condition) {
								t.Fatalf("deletion retained %s readiness", condition)
							}
						}
					}
					wantReplicas := int32(0)
					if fails("workload") {
						wantReplicas = 1
					}
					if err := f.client.Get(ctx, workloadKey, workload); err != nil || ptr.Deref(workload.Spec.Replicas, -1) != wantReplicas {
						t.Fatal("workload shutdown was not independently attempted:", err)
					}
					if !fails("workload") && workload.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
						t.Fatal("shutdown did not preserve HOME retention")
					}
					routeErr := f.client.Get(ctx, client.ObjectKeyFromObject(route), route)
					if fails("route") && routeErr != nil || !fails("route") && !apierrors.IsNotFound(routeErr) {
						t.Fatal("route removal was not independently attempted:", routeErr)
					}
					for i, object := range retained {
						if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil ||
							!object.GetDeletionTimestamp().IsZero() || object.GetResourceVersion() != versions[i] {
							t.Fatalf("%T changed before successful shutdown: %v", object, err)
						}
					}

					f.reconciler.Client, f.reconciler.APIReader = f.client, f.client
					f.source.fenceErr = errors.New("synthetic unavailable deletion fence")
					if err := f.reconcile(); !errors.Is(err, f.source.fenceErr) {
						t.Fatal("recovery did not retain the qualified fence gate:", err)
					}
					if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), &corev1.Namespace{}); err != nil {
						t.Fatal("recovery bypassed namespace fencing:", err)
					}
					f.source.fenceErr = nil
					for range 2 {
						if err := f.reconcile(); err != nil {
							t.Fatal("deletion failed to resume after recovery:", err)
						}
					}
					if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("completed deletion retained the Agent:", err)
					}
				})
			}
		}
	}
}

func TestSandboxPreparationDeletionKeepsRegistryCleanupBehindShutdown(t *testing.T) {
	ctx := context.Background()
	f := newPreparationFixture(t, "hermes")
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	expected := f.expectation(t)
	workload := &appsv1.StatefulSet{}
	workloadKey := client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}
	if err := f.client.Get(ctx, workloadKey, workload); err != nil {
		t.Fatal(err)
	}
	workload.Spec.Replicas = ptr.To[int32](1)
	if err := f.client.Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "registry-source-retained", Namespace: registryauth.DefaultNamespace, UID: "retained-registry-uid",
		OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)},
	}}
	if err := f.client.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	agent.Status.RegistryCredentials = &api.RegistryCredentialsStatus{SourceUID: string(source.UID)}
	if err := f.client.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.reconciler.APIReader = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if _, ok := list.(*corev1.SecretList); ok {
				return errors.New("synthetic registry cleanup outage")
			}
			return c.List(ctx, list, options...)
		},
	})
	result, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("registry cleanup outage did not retain a bounded retry: %+v %v", result, err)
	}
	bindings, err := (&sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}).
		Read(ctx, agent.Name, agent.UID)
	if err != nil || len(bindings) != 0 {
		t.Fatal("registry cleanup failure skipped Sandbox grant revocation:", err)
	}
	if err := f.client.Get(ctx, workloadKey, workload); err != nil || ptr.Deref(workload.Spec.Replicas, -1) != 0 {
		t.Fatal("registry cleanup failure skipped Sandbox workload shutdown:", err)
	}
	current := f.current(t)
	if !slices.Equal(current.Finalizers, agent.Finalizers) || current.Status.Phase != api.PhaseDeleting ||
		current.Status.AccessURL != "" ||
		!apimeta.IsStatusConditionFalse(current.Status.Conditions, api.ConditionRegistryCredentialsReady) ||
		!apimeta.IsStatusConditionFalse(current.Status.Conditions, api.ConditionReady) {
		t.Fatal("registry cleanup failure lost the prepared Agent's deletion hold")
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: workload.Namespace}, &corev1.Namespace{}); err != nil {
		t.Fatal("namespace was removed before registry cleanup:", err)
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal("registry cleanup outage lost its retry state:", err)
	}
	f.reconciler.APIReader = f.client
	f.source.fenceErr = errors.New("synthetic unavailable deletion fence")
	if err := f.reconcile(); err == nil {
		t.Fatal("registry recovery bypassed the Sandbox writer fence")
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(source), source); !apierrors.IsNotFound(err) {
		t.Fatal("Sandbox deletion skipped retained registry credential cleanup:", err)
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: workload.Namespace}, &corev1.Namespace{}); err != nil {
		t.Fatal("registry recovery bypassed namespace fencing:", err)
	}
	f.source.fenceErr = nil
	for range 2 {
		if err := f.reconcile(); err != nil {
			t.Fatal("Sandbox deletion could not resume after registry recovery:", err)
		}
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatal("Sandbox deletion did not finish after registry recovery:", err)
	}
}

type preparationRequestCounts struct {
	client.Client
	gets, lists, writes int
}

func (c *preparationRequestCounts) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	c.gets++
	return c.Client.Get(ctx, key, object, opts...)
}
func (c *preparationRequestCounts) List(ctx context.Context, objects client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Client.List(ctx, objects, opts...)
}
func (c *preparationRequestCounts) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	c.writes++
	return c.Client.Create(ctx, object, opts...)
}
func (c *preparationRequestCounts) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	c.writes++
	return c.Client.Update(ctx, object, opts...)
}
func (c *preparationRequestCounts) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.writes++
	return c.Client.Patch(ctx, object, patch, opts...)
}
func (c *preparationRequestCounts) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	c.writes++
	return c.Client.Delete(ctx, object, opts...)
}
func (c *preparationRequestCounts) Status() client.SubResourceWriter {
	return &preparationStatusCounts{SubResourceWriter: c.Client.Status(), counts: c}
}

type preparationStatusCounts struct {
	client.SubResourceWriter
	counts *preparationRequestCounts
}

func (s *preparationStatusCounts) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	s.counts.writes++
	return s.SubResourceWriter.Patch(ctx, object, patch, opts...)
}
func (s *preparationStatusCounts) Update(ctx context.Context, object client.Object, opts ...client.SubResourceUpdateOption) error {
	s.counts.writes++
	return s.SubResourceWriter.Update(ctx, object, opts...)
}

func TestSandboxPreparationUnchangedRequestBudget(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%v", admin), func(t *testing.T) {
			f := newPreparationFixture(t, "hermes")
			if admin {
				f.makeAdmin(t)
			}
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			state := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: "cetirun-system", Name: w.PodExpectationName(f.fixture.Namespace.Name, f.fixture.Namespace.UID)}
			if err := f.client.Get(context.Background(), key, state); err != nil {
				t.Fatal(err)
			}
			// A missing reuse proof takes the unchanged but fully guarded path.
			delete(state.Annotations, "cetirun.microsoft.com/sandbox-preparation-digest")
			if err := f.client.Update(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			counts := &preparationRequestCounts{Client: f.client}
			f.reconciler.Client, f.reconciler.APIReader, f.installer.client = counts, counts, counts
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			fullReads := counts.gets + counts.lists
			stages, renders, fences, digestReads := f.installer.stages, f.installer.renders, f.source.fences, f.installer.digestReads
			counts.gets, counts.lists, counts.writes = 0, 0, 0
			f.now = f.now.Add(time.Second)
			f.config.ObservedAt, f.source.inputs.ObservedAt = f.now, f.now
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			sources := len(f.expectation(t).Sources)
			reads := counts.gets + counts.lists
			// Admin-class resolution reads the selected Team before the held
			// preparation reuse path; Team selection has no separate authorization
			// record that can short-circuit that read.
			if counts.writes != 0 || reads > sources+26 || reads*2 >= fullReads || counts.lists != 1 ||
				f.installer.stages != stages || f.installer.renders != renders ||
				f.source.fences != fences+1 || f.installer.digestReads != digestReads+1 {
				t.Fatalf("unchanged preparation exceeded its read-only budget: reads=%d full=%d sources=%d writes=%d renders=%d stages=%d fences=%d",
					reads, fullReads, sources, counts.writes, f.installer.renders-renders, f.installer.stages-stages, f.source.fences-fences)
			}
			t.Logf("unchanged held cycle: %d controller API reads vs %d without reuse; %d writes, 1 release-store read, 1 fence", reads, fullReads, counts.writes)
		})
	}
}

func TestSandboxPreparationReuseRefreshesChangedIntentAndRelease(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *preparationFixture){
		"reauthorization": func(t *testing.T, f *preparationFixture) {
			agent := f.current(t)
			reauthorize, err := sandboxbinding.FormatReauthorization("new-request", f.expectation(t).Binding)
			if err != nil {
				t.Fatal(err)
			}
			agent.Annotations[sandboxbinding.ReauthorizeAnnotation] = reauthorize
			if err := f.client.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
		},
		"render defaults": func(_ *testing.T, f *preparationFixture) {
			f.reconciler.CetiRunHelmValues["logger"].(map[string]any)["image"] = map[string]any{"repository": "example.org/logger"}
		},
		"Helm store": func(_ *testing.T, f *preparationFixture) { f.installer.stageDigest = "changed-synthetic-release" },
		"Service": func(t *testing.T, f *preparationFixture) {
			service := &corev1.Service{}
			expected := f.expectation(t)
			if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, service); err != nil {
				t.Fatal(err)
			}
			service.Labels["example.com/changed"] = "true"
			if err := f.client.Update(context.Background(), service); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPreparationFixture(t, "openclaw")
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			stages := f.installer.stages
			change(t, f)
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			if f.installer.stages != stages+1 {
				t.Fatal("changed preparation authority or release was skipped")
			}
			assertSandboxPrepared(t, f)
		})
	}
}

func TestSandboxPreparationReuseKeepsFreshFailureChecks(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *preparationFixture){
		"fence unavailable": func(_ *testing.T, f *preparationFixture) { f.source.fenceErr = errors.New("synthetic missing fence") },
		"fence expires sources": func(_ *testing.T, f *preparationFixture) {
			f.source.onFence = func() { f.now = f.now.Add(time.Minute) }
		},
		"old Pod": func(t *testing.T, f *preparationFixture) {
			if err := f.client.Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "old-writer", Namespace: f.fixture.Namespace.Name,
			}}); err != nil {
				t.Fatal(err)
			}
		},
		"release store unavailable": func(_ *testing.T, f *preparationFixture) {
			f.installer.digestErr = errors.New("synthetic unavailable release store")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPreparationFixture(t, "hermes")
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			stages := f.installer.stages
			change(t, f)
			if err := f.reconcile(); err == nil {
				t.Fatal("a previous successful preparation concealed a current failure")
			}
			if f.installer.stages != stages || apimeta.IsStatusConditionTrue(f.current(t).Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatal("unsafe reuse restaged or reported successful preparation")
			}
		})
	}
}

func TestSandboxPreparationStatusChurnDoesNotRestageOrRewriteExpectation(t *testing.T) {
	f := newPreparationFixture(t, "openclaw")
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	stages := f.installer.stages
	before, err := json.Marshal(f.expectation(t))
	if err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: "ExampleObservation", Status: metav1.ConditionTrue, Reason: "Observed", Message: "synthetic observation",
	})
	if err := f.client.Status().Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(f.expectation(t))
	if err != nil || string(before) != string(after) || f.installer.stages != stages {
		t.Fatal("Agent status-only write forced a new expectation or stage")
	}
}
