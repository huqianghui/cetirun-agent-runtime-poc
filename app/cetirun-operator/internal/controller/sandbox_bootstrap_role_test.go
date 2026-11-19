// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func bootstrapRoleUpgradeFixture(t *testing.T, kind, backend string) (*nativeSandboxFixture, *sandboxBootstrapReceipt, *rbacv1.Role) {
	t.Helper()
	ctx := context.Background()
	setup := []func(*AgentReconciler, client.Client){configureRolloutStableIngress(t, kind)}
	if kind == "custom" {
		setup = []func(*AgentReconciler, client.Client){withCommonCustom(t, "headless")}
	}
	if backend == "container" {
		setup = append(setup, withContainerRuntime(t))
	}
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, setup...)
	readyCustomWorkload(t, f)
	f.reconcileOK(t)
	if f.current(t).Status.Phase != api.PhaseReady {
		t.Fatal("fixture did not reach Ready")
	}
	_, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.Objects[1].Name}, role); err != nil {
		t.Fatal(err)
	}
	role.Rules = readHistoricalBootstrapRBAC(t).Role.Rules
	if err := f.kube.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	return f, receipt, role
}

func TestSandboxBootstrapRoleUpgradePreservesLifetime(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes", "custom"} {
		for _, backend := range []string{"kata", "container"} {
			for _, held := range []bool{false, true} {
				state := "running"
				if held {
					state = "held"
				}
				t.Run(kind+"/"+backend+"/"+state, func(t *testing.T) {
					ctx := context.Background()
					f, receipt, role := bootstrapRoleUpgradeFixture(t, kind, backend)
					agent, workload := f.current(t), f.workload(t)
					token := aigateway.Token(f.gateway.generated[0].Key)
					f.gateway.keys[token].Spend = ptr.To(2.5)
					budget, reset := f.gateway.keys[token].MaxBudget, f.gateway.keys[token].BudgetResetAt
					if held {
						if _, err := f.r.holdPolicyDependencyUnavailable(ctx, agent); err != nil {
							t.Fatal(err)
						}
						stopped := f.workload(t)
						stopped.Status = appsv1.StatefulSetStatus{ObservedGeneration: stopped.Generation}
						if err := f.kube.Status().Update(ctx, stopped); err != nil {
							t.Fatal(err)
						}
					}
					patches := 0
					f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
						Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
							if _, ok := object.(*rbacv1.Role); ok && object.GetName() == role.Name {
								patches++
							}
							return c.Patch(ctx, object, patch, opts...)
						},
					})
					for range 3 {
						f.reconcileOK(t)
						readyCustomWorkload(t, f)
					}
					current := &rbacv1.Role{}
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(role), current); err != nil {
						t.Fatal(err)
					}
					desired, _ := f.r.operatorNamespaceRBAC(receipt.Namespace)
					if current.UID != role.UID || !reflect.DeepEqual(current.Rules, desired.Rules) || patches != 1 {
						t.Fatalf("recorded Role did not upgrade exactly once in place: patches=%d", patches)
					}
					_, after, err := f.r.readSandboxBootstrap(ctx, f.current(t))
					if err != nil || !reflect.DeepEqual(after, receipt) {
						t.Fatal("Role upgrade changed the bootstrap ownership receipt:", err)
					}
					for _, object := range []client.Object{
						&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: receipt.Namespace}},
						&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: receipt.Namespace, Name: receipt.Objects[3].Name}},
						&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: receipt.Namespace, Name: receipt.Objects[4].Name}},
					} {
						entry := slices.IndexFunc(receipt.Objects, func(entry sandboxBootstrapObject) bool { return entry.Name == object.GetName() })
						if err := f.kube.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil ||
							entry < 0 || object.GetUID() != receipt.Objects[entry].UID {
							t.Fatal("Role upgrade replaced retained bootstrap state:", err)
						}
					}
					key := f.gateway.keys[token]
					if key == nil || key.Blocked || ptr.Deref(key.Spend, 0) != 2.5 ||
						!reflect.DeepEqual(key.MaxBudget, budget) || !reflect.DeepEqual(key.BudgetResetAt, reset) ||
						len(f.gateway.generated) != 1 || len(f.gateway.deletedTokens) != 0 ||
						f.current(t).Status.AIGateway.KeyID != agent.Status.AIGateway.KeyID {
						t.Fatal("Role recovery replaced or reset the Gateway key")
					}
					if f.current(t).Status.Phase != api.PhaseReady || f.workload(t).UID != workload.UID ||
						!reflect.DeepEqual(f.workload(t).Spec.Template, workload.Spec.Template) {
						t.Fatal("Role upgrade did not recover the retained workload")
					}
				})
			}
		}
	}
}

func TestSandboxBootstrapRoleDriftStillStopsOwnedLifetimeOnSuspension(t *testing.T) {
	ctx := context.Background()
	f, _, role := bootstrapRoleUpgradeFixture(t, "openclaw", "kata")
	desired, _ := f.r.operatorNamespaceRBAC(role.Namespace)
	role.Rules = append(append([]rbacv1.PolicyRule(nil), desired.Rules...), rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}})
	if err := f.kube.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	before := f.workload(t)
	agent := f.current(t)
	keyID := agent.Status.AIGateway.KeyID
	token := aigateway.Token(f.gateway.generated[0].Key)
	if key := f.gateway.keys[token]; key == nil || key.Blocked {
		t.Fatal("fixture did not retain an unblocked Gateway key")
	}
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	stages, rolePatches := f.installer.stages, 0
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if candidate, ok := object.(*rbacv1.Role); ok && candidate.Name == role.Name && candidate.Namespace == role.Namespace {
				rolePatches++
			}
			return c.Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.reconcile(); err == nil {
		t.Fatal("full bootstrap inventory drift unexpectedly verified suspension")
	}
	stopped := f.workload(t)
	if stopped.UID != before.UID || ptr.Deref(stopped.Spec.Replicas, -1) != 0 || f.installer.stages != stages {
		t.Fatalf("Role drift left the recorded workload running: %#v", stopped.Spec.Replicas)
	}
	if key := f.gateway.keys[token]; key == nil || !key.Blocked || f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
		t.Fatal("Role drift prevented suspension from retaining and blocking the existing Gateway key")
	}
	current := f.current(t)
	if current.Status.Phase != api.PhasePending || current.Status.AccessURL != "" {
		t.Fatalf("unverified bootstrap inventory incorrectly completed suspension: %#v", current.Status)
	}
	observed := &rbacv1.Role{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(role), observed); err != nil || !reflect.DeepEqual(observed.Rules, role.Rules) || rolePatches != 0 {
		t.Fatal("suspension repaired unrelated Role drift:", err)
	}
}

func TestSandboxBootstrapRoleUpgradeRejectsUnconfirmedState(t *testing.T) {
	for _, change := range []string{
		"extra-rule", "missing-rule", "role-uid", "role-owner", "role-marker", "role-finalizer",
		"namespace-uid", "namespace-protection", "binding-uid", "binding-subject", "binding-role",
		"unrecorded-role", "unrecorded-binding", "stale-agent",
	} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f, receipt, role := bootstrapRoleUpgradeFixture(t, "openclaw", "kata")
			attempt, err := f.r.resolveSandboxInputs(ctx, f.current(t), false, true)
			if err != nil {
				t.Fatal(err)
			}
			var object client.Object = role
			switch change {
			case "extra-rule":
				role.Rules = append(role.Rules, rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}})
			case "missing-rule":
				role.Rules = role.Rules[1:]
			case "role-uid":
				role.UID = "replacement-role-uid"
			case "role-owner":
				role.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("foreign-agent", "foreign-agent-uid")}
			case "role-marker":
				role.Annotations[sandboxBootstrapAnnotation] = "foreign-receipt"
			case "role-finalizer":
				role.Finalizers = slices.DeleteFunc(role.Finalizers, func(value string) bool { return value == sandbox.BootstrapFinalizer })
			case "namespace-uid", "namespace-protection":
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: receipt.Namespace}, namespace); err != nil {
					t.Fatal(err)
				}
				object = namespace
				if change == "namespace-uid" {
					namespace.UID = "replacement-namespace-uid"
				} else {
					delete(namespace.Labels, w.ProtectedNamespaceLabel)
				}
			case "binding-uid", "binding-subject", "binding-role":
				binding := &rbacv1.RoleBinding{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.Objects[2].Name}, binding); err != nil {
					t.Fatal(err)
				}
				object = binding
				switch change {
				case "binding-uid":
					binding.UID = "replacement-binding-uid"
				case "binding-subject":
					binding.Subjects[0].Name = "foreign-service-account"
				case "binding-role":
					binding.RoleRef.Name = "foreign-role"
				}
			case "unrecorded-role", "unrecorded-binding":
				state, _, err := f.r.readSandboxBootstrap(ctx, f.current(t))
				if err != nil {
					t.Fatal(err)
				}
				index := 1
				if change == "unrecorded-binding" {
					index = 2
				}
				receipt.Objects[index].UID = ""
				if err := f.r.saveSandboxBootstrap(ctx, state, receipt); err != nil {
					t.Fatal(err)
				}
			case "stale-agent":
				agent := f.current(t)
				agent.Generation++
				object = agent
			}
			if err := f.kube.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			patches := 0
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := object.(*rbacv1.Role); ok {
						patches++
					}
					return c.Patch(ctx, object, patch, opts...)
				},
			})
			if err := f.r.bootstrapSandboxOwned(ctx, attempt); err == nil || patches != 0 {
				t.Fatalf("unconfirmed Role upgrade: change=%s patches=%d err=%v", change, patches, err)
			}
		})
	}
}

func TestSandboxBootstrapRoleUpgradeRetriesSafely(t *testing.T) {
	for _, failure := range []string{"conflict", "lost-reply"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f, receipt, role := bootstrapRoleUpgradeFixture(t, "hermes", "container")
			patches := 0
			lost := errors.New("synthetic lost Role patch reply")
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := object.(*rbacv1.Role); !ok {
						return c.Patch(ctx, object, patch, opts...)
					}
					patches++
					if failure == "conflict" {
						current := &rbacv1.Role{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(role), current); err != nil {
							return err
						}
						metav1.SetMetaDataAnnotation(&current.ObjectMeta, "example.com/observation", "concurrent-update")
						if err := c.Update(ctx, current); err != nil {
							return err
						}
						return c.Patch(ctx, object, patch, opts...)
					}
					if err := c.Patch(ctx, object, patch, opts...); err != nil {
						return err
					}
					return lost
				},
			})
			err := f.r.bootstrapReconciledSandbox(ctx, f.current(t))
			if failure == "conflict" && !apierrors.IsConflict(err) || failure == "lost-reply" && !errors.Is(err, lost) || patches != 1 {
				t.Fatalf("Role patch failure was hidden: patches=%d err=%v", patches, err)
			}
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := object.(*rbacv1.Role); ok {
						patches++
					}
					return c.Patch(ctx, object, patch, opts...)
				},
			})
			f.r = restartEntraReconciler(f.r)
			if err := f.r.bootstrapReconciledSandbox(ctx, f.current(t)); err != nil {
				t.Fatal("ordinary retry could not recover:", err)
			}
			current := &rbacv1.Role{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(role), current); err != nil {
				t.Fatal(err)
			}
			desired, _ := f.r.operatorNamespaceRBAC(receipt.Namespace)
			if current.UID != role.UID || !reflect.DeepEqual(current.Rules, desired.Rules) {
				t.Fatal("retry replaced the Role or failed to migrate its rules")
			}
			if failure == "conflict" && current.Annotations["example.com/observation"] != "concurrent-update" {
				t.Fatal("retry overwrote a concurrent metadata update")
			}
			wantPatches := 1
			if failure == "conflict" {
				wantPatches = 2
			}
			if patches != wantPatches {
				t.Fatalf("retry repeated an already applied migration: patches=%d want=%d", patches, wantPatches)
			}
		})
	}
}

func TestSandboxBootstrapRoleUpgradeRejectsUnknownTarget(t *testing.T) {
	ctx := context.Background()
	f, receipt, role := bootstrapRoleUpgradeFixture(t, "openclaw", "kata")
	attempt, err := f.r.resolveSandboxInputs(ctx, f.current(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := f.r.readSandboxBootstrap(ctx, f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	desired := f.r.sandboxBootstrapObjects(f.current(t), state, receipt)
	target := desired[1].(*rbacv1.Role)
	target.Rules = append(target.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}})
	observed, err := f.r.upgradeSandboxBootstrapRole(ctx, attempt, receipt, desired, role)
	if err != nil || !reflect.DeepEqual(observed, role) ||
		validateSandboxBootstrapObject(observed, target, receipt.Objects[1].UID, false) == nil {
		t.Fatal("unknown target rules bypassed strict bootstrap validation:", err)
	}
	current := &rbacv1.Role{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(role), current); err != nil || !reflect.DeepEqual(current, role) {
		t.Fatal("unknown target rules changed the recorded Role:", err)
	}
}

func TestSandboxBootstrapRoleUpgradeSupportsHeldSingleStart(t *testing.T) {
	ctx := context.Background()
	f := newNativeSandboxFixture(t, true)
	state, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Reconciled || !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
		t.Fatal("fixture is not a recorded held single-start lifetime")
	}
	role := &rbacv1.Role{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.Objects[1].Name}, role); err != nil {
		t.Fatal(err)
	}
	role.Rules = readHistoricalBootstrapRBAC(t).Role.Rules
	if err := f.kube.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	attempt, err := f.r.resolveSandboxInputs(ctx, f.current(t), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.bootstrapSandboxOwned(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(role), role); err != nil {
		t.Fatal(err)
	}
	desired := f.r.sandboxBootstrapObjects(f.current(t), state, receipt)[1]
	if err := validateSandboxBootstrapObject(role, desired, receipt.Objects[1].UID, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) || f.installer.stages != 0 {
		t.Fatal("Role migration authorized held single-start execution")
	}
}
