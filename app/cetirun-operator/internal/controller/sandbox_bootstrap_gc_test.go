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
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func bootstrapDependents(f *preparationFixture) []client.Object {
	namespace := f.fixture.Namespace.Name
	return []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator-ns-role", Namespace: namespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator-ns-rolebinding", Namespace: namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: f.fixture.Input.Home.Name, Namespace: namespace}},
		&metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: f.fixture.Agent.Spec.GatewayTokenSecretRef, Namespace: namespace}},
	}
}

func preparedBootstrapFixture(t *testing.T, typ string) *preparationFixture {
	t.Helper()
	ctx := context.Background()
	f := bootstrappedFixture(t, typ)
	skills := f.fixture.Input.Skills.DeepCopy()
	skills.UID, skills.ResourceVersion = "", ""
	skills.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: api.GroupVersion.String(), Kind: "Team", Name: f.fixture.Team.Name, UID: f.fixture.Team.UID,
	}}
	if err := f.client.Create(ctx, skills); err != nil {
		t.Fatal(err)
	}
	account := f.fixture.ServiceAccount.DeepCopy()
	account.UID, account.ResourceVersion = "", ""
	if err := f.client.Create(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatal("absence of a real fence was ignored:", err)
	}
	f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites).WriterExclusion = &testSandboxWriterExclusion{}
	if err := f.reconcile(); err != nil {
		t.Fatal("test-only evidence could not drive the bootstrapped held path:", err)
	}
	return f
}

func TestSandboxBootstrapForegroundGCWaitsForCleanup(t *testing.T) {
	for _, failure := range []string{"shutdown", "receipt", "fence"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := preparedBootstrapFixture(t, "hermes")
			workload := &appsv1.StatefulSet{}
			key := client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: "openclaw-example-cetirun"}
			if err := f.client.Get(ctx, key, workload); err != nil {
				t.Fatal(err)
			}
			workload.Spec.Replicas = ptr.To(int32(1))
			if err := f.client.Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			receipt := bootstrapReceipt(t, f)
			originalData := receipt.DeepCopy().Data
			blocked := errors.New("synthetic shutdown failure")
			switch failure {
			case "shutdown":
				f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						if _, ok := object.(*appsv1.StatefulSet); ok {
							return blocked
						}
						return c.Patch(ctx, object, patch, options...)
					},
				})
			case "receipt":
				receipt.Data["bootstrap.json"] = `{"invalid":"synthetic-receipt"}`
				if err := f.client.Update(ctx, receipt); err != nil {
					t.Fatal(err)
				}
			case "fence":
				f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites).WriterExclusion = nil
			}
			if err := f.client.Delete(ctx, f.current(t), client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
				t.Fatal(err)
			}
			// The fake client has no GC. Issue the dependent DELETEs that
			// foreground GC can send before the Agent controller reconciles.
			dependents := bootstrapDependents(f)
			for _, object := range dependents {
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Delete(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.reconcile(); err == nil {
				t.Fatal("the cleanup failure was ignored")
			}
			for _, object := range dependents {
				uid := object.GetUID()
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatalf("GC removed %T before cleanup was authorized: %v", object, err)
				}
				if object.GetUID() != uid || object.GetDeletionTimestamp().IsZero() ||
					!slices.Contains(object.GetFinalizers(), sandbox.BootstrapFinalizer) {
					t.Fatalf("%T lost its identity or lifecycle protection", object)
				}
			}
			if failure != "shutdown" {
				if err := f.client.Get(ctx, key, workload); err != nil || ptr.Deref(workload.Spec.Replicas, -1) != 0 {
					t.Fatal("blocked cleanup suppressed independent shutdown:", err)
				}
			}
			f.reconciler.Client = f.client
			f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites).WriterExclusion = &testSandboxWriterExclusion{}
			if failure == "receipt" {
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(receipt), receipt); err != nil {
					t.Fatal(err)
				}
				receipt.Data = originalData
				if err := f.client.Update(ctx, receipt); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := f.reconcile(); err != nil {
					t.Fatal("cleanup did not recover after its gate succeeded:", err)
				}
			}
			for _, object := range dependents {
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
					t.Fatalf("authorized cleanup retained %T: %v", object, err)
				}
			}
		})
	}
}

func TestSandboxBootstrapDependentReleasePreservesOtherFinalizers(t *testing.T) {
	ctx := context.Background()
	f := bootstrappedFixture(t, "openclaw")
	const retained = "example.com/retain"
	dependents := bootstrapDependents(f)
	for _, object := range dependents {
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
		base := object.DeepCopyObject().(client.Object)
		object.SetFinalizers(append(object.GetFinalizers(), retained))
		if err := f.client.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			t.Fatal(err)
		}
		if err := f.client.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, object := range dependents {
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal("cleanup discarded another controller's retained object:", err)
		}
		if !slices.Equal(object.GetFinalizers(), []string{retained}) {
			t.Fatal("cleanup did not release only the bootstrap finalizer")
		}
	}
}

func TestSandboxBootstrapDependentReleaseUsesOptimisticLock(t *testing.T) {
	for index := range 4 {
		t.Run([]string{"Role", "RoleBinding", "HOME", "Secret"}[index], func(t *testing.T) {
			ctx := context.Background()
			f := bootstrappedFixture(t, "hermes")
			target := bootstrapDependents(f)[index]
			key := client.ObjectKeyFromObject(target)
			var attempted bool
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if client.ObjectKeyFromObject(object) == key {
						attempted = true
						current := target.DeepCopyObject().(client.Object)
						if err := c.Get(ctx, key, current); err != nil {
							return err
						}
						concurrent := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"example.com/concurrent":"changed"}}}`))
						if err := c.Patch(ctx, current, concurrent); err != nil {
							return err
						}
					}
					return c.Patch(ctx, object, patch, options...)
				},
			})
			if err := f.client.Delete(ctx, f.current(t)); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); !apierrors.IsConflict(err) || !attempted {
				t.Fatal("dependent release overwrote a concurrent change:", err)
			}
			if err := f.client.Get(ctx, key, target); err != nil ||
				!slices.Contains(target.GetFinalizers(), sandbox.BootstrapFinalizer) {
				t.Fatal("conflicting patch discarded dependent protection:", err)
			}
			namespace := &corev1.Namespace{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil ||
				!slices.Contains(namespace.Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("dependent release failure discarded namespace protection:", err)
			}
		})
	}
}

func TestSandboxBootstrapRejectsMissingDependentProtection(t *testing.T) {
	for index := range 4 {
		t.Run([]string{"Role", "RoleBinding", "HOME", "Secret"}[index], func(t *testing.T) {
			ctx := context.Background()
			f := bootstrappedFixture(t, "hermes")
			object := bootstrapDependents(f)[index]
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				t.Fatal(err)
			}
			base := object.DeepCopyObject().(client.Object)
			object.SetFinalizers(nil)
			if err := f.client.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); err == nil || apierrors.IsNotFound(err) {
				t.Fatal("missing lifecycle protection was accepted as a normal bootstrap retry:", err)
			}
		})
	}
}
