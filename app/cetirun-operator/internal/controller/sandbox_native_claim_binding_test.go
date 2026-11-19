// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNativeSandboxConcurrentClaimBinding(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		skills  bool
		running bool
	}{
		{name: "starting HOME"},
		{name: "starting skills", skills: true},
		{name: "running HOME", running: true},
		{name: "running skills", skills: true, running: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newNativeSandboxFixture(t, true)
			f.prepare(t)
			f.reconcileOK(t)
			_, expected := f.expectation(t)
			pod := f.createPod(t, true)
			if scenario.running {
				f.reconcileOK(t)
				f.assertReady(t)
			}
			claim := expected.Home
			if scenario.skills {
				claim = expected.Skills
			}
			stages := f.installer.stages
			changed := changeNativeClaimDuringRecheck(f, claim, func(ctx context.Context, key client.ObjectKey) error {
				return bindNativeTestClaim(ctx, f.kube, key)
			})
			f.reconcileOK(t)
			if !changed() {
				t.Fatal("fixture did not bind a claim between prerequisite snapshot and recheck")
			}
			f.assertReady(t)
			_, after := f.expectation(t)
			if slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) ||
				ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 || after.PodUID != pod.UID ||
				!maps.Equal(after.SourceDigests, expected.SourceDigests) || f.installer.stages != stages {
				t.Fatal("ordinary claim binding changed the native lifetime or replayed preparation")
			}
			receipt, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
			if err != nil || receipt.Annotations[w.NativeStartAnnotation] != expected.Revision {
				t.Fatal("ordinary claim binding changed the consumed single-start receipt:", err)
			}
		})
	}
}

func TestNativeSandboxConcurrentClaimChangesFailClosed(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(context.Context, client.Client, client.ObjectKey) error
	}{
		{name: "replacement", change: func(ctx context.Context, c client.Client, key client.ObjectKey) error {
			claim := &corev1.PersistentVolumeClaim{}
			if err := c.Get(ctx, key, claim); err != nil {
				return err
			}
			if err := c.Delete(ctx, claim); err != nil {
				return err
			}
			claim.UID, claim.ResourceVersion = "", ""
			return c.Create(ctx, claim)
		}},
		{name: "storage request", change: mutateNativeTestClaim(func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("30Gi")}
		})},
		{name: "storage class", change: mutateNativeTestClaim(func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.StorageClassName = ptr.To("different-storage")
		})},
		{name: "access mode", change: mutateNativeTestClaim(func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		})},
		{name: "owner", change: mutateNativeTestClaim(func(claim *corev1.PersistentVolumeClaim) {
			claim.OwnerReferences[0].UID = "different-owner"
		})},
		{name: "protected metadata", change: mutateNativeTestClaim(func(claim *corev1.PersistentVolumeClaim) {
			if claim.Annotations == nil {
				claim.Annotations = map[string]string{}
			}
			claim.Annotations[w.AgentUIDAnnotation] = "different-agent"
		})},
		{name: "other finalizer", change: mutateNativeTestClaim(func(claim *corev1.PersistentVolumeClaim) {
			claim.Finalizers = append(claim.Finalizers, "example.com/storage-hold")
		})},
		{name: "lost", change: func(ctx context.Context, c client.Client, key client.ObjectKey) error {
			claim := &corev1.PersistentVolumeClaim{}
			if err := c.Get(ctx, key, claim); err != nil {
				return err
			}
			claim.Status.Phase = corev1.ClaimLost
			return c.Status().Update(ctx, claim)
		}},
		{name: "deleted", change: func(ctx context.Context, c client.Client, key client.ObjectKey) error {
			return c.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}})
		}},
		{name: "terminating", change: func(ctx context.Context, c client.Client, key client.ObjectKey) error {
			if err := mutateNativeTestClaim(func(claim *corev1.PersistentVolumeClaim) {
				claim.Finalizers = append(claim.Finalizers, "example.com/storage-hold")
			})(ctx, c, key); err != nil {
				return err
			}
			return c.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}})
		}},
		{name: "read unavailable", change: func(context.Context, client.Client, client.ObjectKey) error {
			return errors.New("synthetic claim read outage")
		}},
	} {
		for _, skills := range []bool{false, true} {
			name := scenario.name + "/HOME"
			if skills {
				name = scenario.name + "/skills"
			}
			t.Run(name, func(t *testing.T) {
				f := newNativeSandboxFixture(t, true)
				f.prepare(t)
				f.reconcileOK(t)
				_, expected := f.expectation(t)
				f.createPod(t, true)
				f.reconcileOK(t)
				f.assertReady(t)
				claim := expected.Home
				if skills {
					claim = expected.Skills
				}
				stages := f.installer.stages
				changed := changeNativeClaimDuringRecheck(f, claim, func(ctx context.Context, key client.ObjectKey) error {
					return scenario.change(ctx, f.kube, key)
				})
				if _, err := f.reconcile(); err == nil || !changed() {
					t.Fatalf("concurrent claim drift did not fail closed: changed=%t, error=%v", changed(), err)
				}
				f.assertNotReady(t)
				if !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) ||
					ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || f.installer.stages != stages {
					t.Fatal("claim drift did not hold the existing lifetime without replay")
				}
				receipt, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
				if err != nil || receipt.Annotations[w.NativeStartAnnotation] != expected.Revision {
					t.Fatal("claim drift lost the consumed single-start receipt:", err)
				}
			})
		}
	}
}

func TestNativeSandboxHeldClaimSnapshotRemainsStrict(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	_, expected := f.expectation(t)
	changed := changeNativeClaimDuringRecheck(f, expected.Home, func(ctx context.Context, key client.ObjectKey) error {
		return bindNativeTestClaim(ctx, f.kube, key)
	})
	team := &api.Team{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.current(t).Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.SandboxPreparation.Prerequisites.Resolve(context.Background(), f.current(t), team); err == nil || !changed() {
		t.Fatalf("held preparation accepted a stale source snapshot: changed=%t, error=%v", changed(), err)
	}
	receipt, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
	if err != nil || receipt.Annotations[w.NativeStartAnnotation] != "" ||
		ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("held snapshot failure authorized a start:", err)
	}
}

func changeNativeClaimDuringRecheck(f *nativeSandboxFixture, claim w.ObjectIdentity,
	change func(context.Context, client.ObjectKey) error,
) func() bool {
	reads := 0
	changed := false
	source := f.r.SandboxPreparation.Prerequisites.(*KubernetesSandboxPrerequisites)
	source.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if key.Namespace == instanceNamespace(f.agent.Name) && key.Name == claim.Name {
				switch object.(type) {
				case *corev1.PersistentVolumeClaim, *metav1.PartialObjectMetadata:
					reads++
					if reads == 2 {
						changed = true
						if err := change(ctx, key); err != nil {
							return err
						}
					}
				}
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	return func() bool { return changed }
}

func mutateNativeTestClaim(mutate func(*corev1.PersistentVolumeClaim)) func(context.Context, client.Client, client.ObjectKey) error {
	return func(ctx context.Context, c client.Client, key client.ObjectKey) error {
		claim := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, key, claim); err != nil {
			return err
		}
		mutate(claim)
		return c.Update(ctx, claim)
	}
}

func bindNativeTestClaim(ctx context.Context, c client.Client, key client.ObjectKey) error {
	claim := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, key, claim); err != nil {
		return err
	}
	if claim.Spec.VolumeName == "" {
		claim.Spec.VolumeName = "bound-example-volume"
	}
	if !slices.Contains(claim.Finalizers, "kubernetes.io/pvc-protection") {
		claim.Finalizers = append(claim.Finalizers, "kubernetes.io/pvc-protection")
	}
	if err := c.Update(ctx, claim); err != nil {
		return err
	}
	claim.Status.Phase = corev1.ClaimBound
	return c.Status().Update(ctx, claim)
}
