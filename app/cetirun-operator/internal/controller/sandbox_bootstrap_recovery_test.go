// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func bootstrappedFixture(t *testing.T, typ string) *preparationFixture {
	t.Helper()
	f := newSandboxBootstrapFixture(t, typ, true)
	if err := f.reconcile(); !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return f
}

func TestSandboxBootstrapRefusesRecordedReplacement(t *testing.T) {
	for _, kind := range []string{"namespace", "home", "secret", "role", "binding"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := bootstrappedFixture(t, "hermes")
			var object client.Object
			ns := f.fixture.Namespace.Name
			switch kind {
			case "namespace":
				object = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
			case "home":
				object = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: f.fixture.Input.Home.Name, Namespace: ns}}
			case "secret":
				object = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: f.fixture.Agent.Spec.GatewayTokenSecretRef, Namespace: ns}}
			case "role":
				object = &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator-ns-role", Namespace: ns}}
			case "binding":
				object = &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator-ns-rolebinding", Namespace: ns}}
			}
			key := client.ObjectKeyFromObject(object)
			if err := f.client.Get(ctx, key, object); err != nil {
				t.Fatal(err)
			}
			// Simulate an out-of-band forced replacement, not normal bootstrap.
			object.SetFinalizers(nil)
			if err := f.client.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Delete(ctx, object); err != nil {
				t.Fatal(err)
			}
			replacement := object.DeepCopyObject().(client.Object)
			replacement.SetUID("")
			replacement.SetResourceVersion("")
			if err := f.client.Create(ctx, replacement); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); err == nil {
				t.Fatal("same-name replacement was silently accepted")
			}
			after := replacement.DeepCopyObject().(client.Object)
			if err := f.client.Get(ctx, key, after); err != nil || !equality.Semantic.DeepEqual(after, replacement) {
				t.Fatal("bootstrap mutated the replacement:", err)
			}
			if err := f.client.Delete(ctx, f.current(t)); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); err == nil {
				t.Fatal("cleanup accepted a replacement outside the ownership receipt")
			}
			if !slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("cleanup discarded ownership recovery state")
			}
			if err := f.client.Get(ctx, key, after); err != nil || !equality.Semantic.DeepEqual(after, replacement) {
				t.Fatal("cleanup mutated the replacement:", err)
			}
		})
	}
}

func TestSandboxBootstrapDoesNotRecreateMissingHome(t *testing.T) {
	f := bootstrappedFixture(t, "openclaw")
	home := &corev1.PersistentVolumeClaim{}
	key := client.ObjectKeyFromObject(f.fixture.Input.Home)
	if err := f.client.Get(context.Background(), key, home); err != nil {
		t.Fatal(err)
	}
	// Simulate forced removal, not a normal DELETE of protected HOME.
	home.Finalizers = nil
	if err := f.client.Update(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("missing recorded HOME was replaced with empty storage")
	}
	if err := f.client.Get(context.Background(), key, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatal("bootstrap recreated HOME:", err)
	}
}

func TestSandboxBootstrapNativeTypeChangeRequiresHandoff(t *testing.T) {
	f := bootstrappedFixture(t, "openclaw")
	agent := f.current(t)
	agent.Spec.Agent.Type = api.AgentTypeHermes
	if err := f.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); !errors.Is(err, sandboxworkload.ErrStorageTransitionRequired) {
		t.Fatal("bootstrap reused credentials initialized for a different native type:", err)
	}
}

func TestSandboxBootstrapDisablingWritesRetainsOwnershipGate(t *testing.T) {
	f := bootstrappedFixture(t, "openclaw")
	config, err := controller.ParseSandboxSourcesConfig(sandboxSourcesConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := controller.NewKubernetesSandboxPreparation(config, f.client, f.installer, "cetirun-system", f.installer.chart.Metadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = preparation
	if err := f.reconcile(); !errors.Is(err, sandboxpolicy.ErrUnavailable) {
		t.Fatal("disabling bootstrap bypassed its retained ownership checks:", err)
	}
	if !slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) || f.installer.stages != 0 {
		t.Fatal("disabling bootstrap lost ownership protection or staged a workload")
	}
}

func TestSandboxBootstrapUnexpectedPodBlocksCleanup(t *testing.T) {
	ctx := context.Background()
	f := bootstrappedFixture(t, "hermes")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unexpected-pod", Namespace: f.fixture.Namespace.Name}}
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("unexpected Pod did not block bootstrap-only cleanup")
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil ||
		!namespace.DeletionTimestamp.IsZero() || !slices.Contains(namespace.Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("Pod check failed to preserve the namespace:", err)
	}
}

func TestSandboxBootstrapUnmarkedReceiptCleanup(t *testing.T) {
	for _, typ := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeCustom} {
		t.Run(string(typ), func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxBootstrapFixture(t, "openclaw", true)
			failure := errors.New("synthetic lost receipt response")
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
					if err := c.Create(ctx, o, opts...); err != nil {
						return err
					}
					if bootstrapKind(o) == "receipt" {
						return failure
					}
					return nil
				},
			})
			if err := f.reconcile(); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			receipt := bootstrapReceipt(t, f)
			f.reconciler.Client, f.reconciler.SandboxPreparation = f.client, nil
			agent := f.current(t)
			if slices.Contains(agent.Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("test did not stop before the Agent marker")
			}
			agent.Spec.Agent.Type = typ
			if err := f.client.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); err != nil {
				t.Fatal("unmarked bootstrap receipt prevented deletion:", err)
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("interrupted bootstrap left Agent cleanup obligations:", err)
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(receipt), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatal("interrupted bootstrap leaked a retained receipt:", err)
			}
		})
	}
}

func TestSandboxBootstrapCleanupWithoutSources(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes", "custom-after-bootstrap"} {
		t.Run(typ, func(t *testing.T) {
			ctx := context.Background()
			nativeType := typ
			if typ == "custom-after-bootstrap" {
				nativeType = "openclaw"
			}
			f := bootstrappedFixture(t, nativeType)
			receipt := bootstrapReceipt(t, f)
			agent := f.current(t)
			if typ == "custom-after-bootstrap" {
				agent.Spec.Agent.Type = api.AgentTypeCustom
				if err := f.client.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.client.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			f.reconciler.SandboxPreparation = nil
			if err := f.reconcile(); err != nil {
				t.Fatal("bootstrap-only cleanup incorrectly required unavailable configuration or fencing:", err)
			}
			if err := f.reconcile(); err != nil {
				t.Fatal("bootstrap cleanup did not finish:", err)
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("bootstrap finalizer was not released:", err)
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(receipt), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatal("bootstrap receipt was not removed:", err)
			}
		})
	}
}

func TestSandboxBootstrapCorruptReceiptStillStopsWorkload(t *testing.T) {
	ctx := context.Background()
	f := bootstrappedFixture(t, "hermes")
	receipt := bootstrapReceipt(t, f)
	receipt.Data["bootstrap.json"] = `{"unexpected":"synthetic-content-not-an-ownership-receipt"}`
	if err := f.client.Update(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	workload := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "openclaw-example-cetirun", Namespace: f.fixture.Namespace.Name,
			OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	if err := f.client.Create(ctx, workload); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("corrupt ownership was accepted")
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(workload), workload); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(workload.Spec.Replicas, -1) != 0 {
		t.Fatal("receipt failure skipped independent workload shutdown")
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil || !namespace.DeletionTimestamp.IsZero() {
		t.Fatal("receipt failure allowed destructive namespace cleanup:", err)
	}
}

func TestSandboxBootstrapGuardRejectsConcurrentAuthorityChange(t *testing.T) {
	f := newSandboxBootstrapFixture(t, "hermes", true)
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, o, opts...); err != nil {
				return err
			}
			if _, ok := o.(*corev1.Namespace); ok {
				team := &api.Team{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), team); err != nil {
					return err
				}
				team.Labels = map[string]string{"example.com/revision": "changed"}
				return c.Update(ctx, team)
			}
			return nil
		},
	})
	if err := f.reconcile(); err == nil || apierrors.IsNotFound(err) {
		t.Fatal("stale authority was used to finish bootstrap:", err)
	}
	home := &corev1.PersistentVolumeClaim{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Input.Home), home); !apierrors.IsNotFound(err) {
		t.Fatal("bootstrap continued writes after an authority change:", err)
	}
	f.reconciler.Client = f.client
	if err := f.reconcile(); !apierrors.IsNotFound(err) {
		t.Fatal("fresh authority did not recover interrupted ownership:", err)
	}
}

func TestSandboxBootstrapNamespaceReleaseUsesOptimisticLock(t *testing.T) {
	ctx := context.Background()
	f := bootstrappedFixture(t, "hermes")
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	var conflicted bool
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, o client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if ns, ok := o.(*corev1.Namespace); ok {
				current := &corev1.Namespace{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(ns), current); err != nil {
					return err
				}
				current.Labels["example.com/concurrent"] = "changed"
				if err := c.Update(ctx, current); err != nil {
					return err
				}
				err := c.Patch(ctx, o, patch, opts...)
				conflicted = apierrors.IsConflict(err)
				return err
			}
			return c.Patch(ctx, o, patch, opts...)
		},
	})
	if err := f.reconcile(); !apierrors.IsConflict(err) || !conflicted {
		t.Fatal("namespace release overwrote a concurrent mutation:", err)
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil ||
		!slices.Contains(namespace.Finalizers, sandbox.BootstrapFinalizer) || !namespace.DeletionTimestamp.IsZero() {
		t.Fatal("conflicting release discarded namespace protection:", err)
	}
}

func TestSandboxBootstrapReceiptPatchFailureRetainsIntent(t *testing.T) {
	for _, afterWrite := range []bool{false, true} {
		t.Run(fmt.Sprint(afterWrite), func(t *testing.T) {
			f := newSandboxBootstrapFixture(t, "hermes", true)
			failure := errors.New("synthetic receipt persistence failure")
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, o client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if cm, ok := o.(*corev1.ConfigMap); ok && cm.Data["bootstrap.json"] != "" {
						if afterWrite {
							if err := c.Patch(ctx, o, patch, opts...); err != nil {
								return err
							}
						}
						return failure
					}
					return c.Patch(ctx, o, patch, opts...)
				},
			})
			if err := f.reconcile(); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			namespace := &corev1.Namespace{}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil {
				t.Fatal(err)
			}
			f.reconciler.Client = f.client
			if err := f.reconcile(); !apierrors.IsNotFound(err) {
				t.Fatal("receipt write failure was not recoverable:", err)
			}
			current := &corev1.Namespace{}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(namespace), current); err != nil || current.UID != namespace.UID {
				t.Fatal("receipt failure lost the created namespace identity:", err)
			}
		})
	}
}

func TestSandboxBootstrapHeldPreparationStillRequiresFence(t *testing.T) {
	ctx := context.Background()
	f := preparedBootstrapFixture(t, "hermes")
	prerequisites := f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites)
	workload := &appsv1.StatefulSet{}
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: "openclaw-example-cetirun"}, workload); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(workload.Spec.Replicas, -1) != 0 || f.current(t).Status.Sandbox != nil ||
		!slices.Contains(f.current(t).Finalizers, sandbox.PreparationFinalizer) {
		t.Fatal("test-only preparation permitted execution or lost the real cleanup gate")
	}
	prerequisites.WriterExclusion = nil
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatal("prepared bootstrap cleanup bypassed writer exclusion:", err)
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil ||
		!namespace.DeletionTimestamp.IsZero() || !slices.Contains(namespace.Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("lost fencing permitted namespace cleanup:", err)
	}
}

func TestSandboxBootstrapCleanupRecoversInterruptedCheckpoints(t *testing.T) {
	for _, step := range []string{
		"release HOME", "release Secret", "release Role", "release RoleBinding",
		"release namespace", "delete namespace", "release Agent", "release receipt", "delete receipt",
	} {
		for _, afterWrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/response-lost=%t", step, afterWrite), func(t *testing.T) {
				ctx := context.Background()
				f := bootstrappedFixture(t, "hermes")
				agent, receipt := f.current(t), bootstrapReceipt(t, f)
				if err := f.client.Delete(ctx, agent); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("synthetic cleanup checkpoint failure")
				var hit bool
				write := func(match bool, action func() error) error {
					if !match {
						return action()
					}
					hit = true
					if afterWrite {
						if err := action(); err != nil {
							return err
						}
					}
					return failure
				}
				f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, o client.Object, patch client.Patch, opts ...client.PatchOption) error {
						var match bool
						if !slices.Contains(o.GetFinalizers(), sandbox.BootstrapFinalizer) {
							switch o.(type) {
							case *corev1.PersistentVolumeClaim:
								match = step == "release HOME"
							case *metav1.PartialObjectMetadata:
								match = step == "release Secret"
							case *rbacv1.Role:
								match = step == "release Role"
							case *rbacv1.RoleBinding:
								match = step == "release RoleBinding"
							case *corev1.Namespace:
								match = step == "release namespace"
							case *api.Agent:
								match = step == "release Agent"
							case *corev1.ConfigMap:
								match = step == "release receipt" && o.GetName() == receipt.Name
							}
						}
						return write(match, func() error { return c.Patch(ctx, o, patch, opts...) })
					},
					Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
						match := (step == "delete namespace" && bootstrapKind(o) == "namespace") ||
							(step == "delete receipt" && o.GetName() == receipt.Name)
						return write(match, func() error { return c.Delete(ctx, o, opts...) })
					},
				})
				for range 3 {
					err := f.reconcile()
					if hit {
						if !errors.Is(err, failure) {
							t.Fatal("cleanup hid a failed checkpoint:", err)
						}
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if !hit {
					t.Fatal("cleanup checkpoint was not exercised")
				}
				f.reconciler.Client = f.client
				for range 3 {
					if err := f.reconcile(); err != nil {
						t.Fatal("cleanup did not recover from the interrupted checkpoint:", err)
					}
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatal("interrupted cleanup retained the Agent:", err)
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(receipt), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
					t.Fatal("interrupted cleanup retained the receipt:", err)
				}
			})
		}
	}
}
