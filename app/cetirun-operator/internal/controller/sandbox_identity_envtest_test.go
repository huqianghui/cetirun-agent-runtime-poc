// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNativeEntraCommonRuntimeAPIServer(t *testing.T) {
	for _, tc := range []struct {
		kind string
		mode corev1.PersistentVolumeAccessMode
	}{{"openclaw", corev1.ReadWriteOnce}, {"hermes", corev1.ReadWriteOncePod}, {"custom", corev1.ReadWriteOnce}} {
		t.Run(tc.kind, func(t *testing.T) {
			h := newNativeStartAPIHarnessConfigured(t, true, true, tc.mode, tc.kind,
				func(caps *sandbox.Capabilities) { caps.EntraID = true })
			lost := false
			h.kube = interceptor.NewClient(h.kube, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if err := c.Create(ctx, object, opts...); err != nil {
						return err
					}
					if namespace, ok := object.(*corev1.Namespace); ok {
						return h.admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
							Name: "default", Namespace: namespace.Name,
						}})
					}
					if account, ok := object.(*corev1.ServiceAccount); ok && account.Name == "example-entra" && !lost {
						lost = true
						return errors.New("synthetic accepted ServiceAccount create response lost")
					}
					return nil
				},
			})
			h.r.Client = h.kube
			installer := &nativeSandboxInstaller{kube: h.kube, chart: h.localHelm.chart}
			h.installer = installer
			h.r.HelmClient = &sharedSandboxInstaller{nativeSandboxInstaller: installer}
			withNativeEntra(t)(h.r, h.admin)
			policies := &AgentPolicyReconciler{Client: h.kube, APIReader: h.kube,
				Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
			if _, err := policies.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "policy-example"}}); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				_, err := h.reconcile()
				if lost {
					if err == nil || installer.stages != 0 {
						t.Fatal("unconfirmed ServiceAccount allowed a workload submission:", err)
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if !lost {
				t.Fatal("real API create interruption was not exercised")
			}
			account := &corev1.ServiceAccount{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: instanceNamespace(h.agent.Name), Name: "example-entra"}, account); err != nil {
				t.Fatal(err)
			}
			pin := account.UID
			h.r = restartEntraReconciler(h.r)
			for range 4 {
				h.reconcileOK(t)
			}
			expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			if expected.IdentityServiceAccount == nil || expected.IdentityServiceAccount.UID != pin {
				t.Fatal("real API retry lost the first created ServiceAccount UID")
			}
			observed := *h.nativeSandboxFixture
			observed.kube = h.admin
			pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
			pod.Spec.Overhead = nil
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); err != nil {
				t.Fatal("current native Entra Pod was rejected by real admission:", err)
			}
			for _, permission := range []struct{ resource, subresource, verb string }{
				{"pods", "", "create"}, {"secrets", "", "get"}, {"serviceaccounts", "token", "create"},
			} {
				review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
					User:   "system:serviceaccount:" + account.Namespace + ":" + account.Name,
					Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + account.Namespace, "system:authenticated"},
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: account.Namespace, Resource: permission.resource, Subresource: permission.subresource, Verb: permission.verb,
					},
				}}
				if err := h.admin.Create(h.ctx, review); err != nil || review.Status.Allowed {
					t.Fatal("Entra runtime acquired Kubernetes RBAC:", err)
				}
			}
			if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(account), account); err != nil {
				t.Fatal(err)
			}
			original := account.DeepCopy()
			account.Annotations["azure.workload.identity/client-id"] = "unapproved-client"
			if err := h.admin.Update(h.ctx, account); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatal("current Entra metadata drift passed real Pod admission:", err)
			}
			account.Annotations = original.Annotations
			if err := h.admin.Update(h.ctx, account); err != nil {
				t.Fatal(err)
			}
			h.create(t, pod)
			if pod.UID == "" {
				t.Fatal("native Entra Pod was not persisted through served admission")
			}
			if err := h.admin.Delete(h.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			account.Finalizers = nil
			if err := h.admin.Update(h.ctx, account); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.Delete(h.ctx, account, client.Preconditions{UID: &pin}); err != nil {
				t.Fatal(err)
			}
			replacement := original.DeepCopy()
			replacement.UID, replacement.ResourceVersion = "", ""
			replacement.CreationTimestamp, replacement.ManagedFields = metav1.Time{}, nil
			h.create(t, replacement)
			if replacement.UID == pin {
				t.Fatal("API did not create a new ServiceAccount identity")
			}
			pod.UID, pod.ResourceVersion = "", ""
			pod.CreationTimestamp, pod.ManagedFields = metav1.Time{}, nil
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatal("same-name replacement passed native Entra admission:", err)
			}
			_, _ = h.reconcile()
			h.assertNotReady(t)
			if ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("same-name replacement did not stop the workload")
			}
			_, receipt, err := h.r.readSandboxBootstrap(h.ctx, h.current(t))
			if err != nil || receipt.ServiceAccount.UID != pin {
				t.Fatal("replacement silently re-authorized the original Entra binding:", err)
			}
			for _, volume := range expected.Workloads[0].Template.Spec.Volumes {
				if volume.Name == "sandbox-entra-exchange" && volume.Projected.Sources[0].ServiceAccountToken.Audience != sandboxpolicy.EntraAudience {
					t.Fatal("Entra audience changed")
				}
			}
		})
	}
}
