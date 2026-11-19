// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"fmt"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCustomUnboundDeletionPreservesForeignNamespace(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, forgedMetadata := range []bool{false, true} {
		cr, b, _ := customFixture("example", nil)
		cr.Finalizers = nil
		if forgedMetadata {
			cr.Status.Namespace = b.Namespace.Name
			cr.Annotations = map[string]string{"cetirun.microsoft.com/namespace-uid": "foreign-uid"}
		}
		foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: b.Namespace.Name, UID: "foreign-uid",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: cr.Name, UID: "old-agent"}}}}
		scheme := testScheme()
		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(cr, foreign).Build()
		r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: &fakeHelmClient{useHarness: true}, EventRecorder: record.NewFakeRecorder(100)}
		ctx, key := context.Background(), types.NamespacedName{Name: cr.Name}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, key, cr); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, cr); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil || foreign.UID != "foreign-uid" {
			t.Fatal("unbound deletion modified the foreign namespace")
		}
		if err := c.Get(ctx, key, &api.Agent{}); !apierrors.IsNotFound(err) {
			t.Fatalf("unbound Agent was not finalized: %v", err)
		}
	}
}

func TestCustomRecordedDeletionAndNamespaceReplacement(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, replaceNamespace := range []bool{false, true} {
		cr, b, objects := customFixture("example", nil)
		scheme := testScheme()
		c := provisioningClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
		r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: &fakeHelmClient{useHarness: true},
			CustomBindings: &customProvider{bindings: b}, EventRecorder: record.NewFakeRecorder(100)}
		controller.ConfigureTestGateway(t, r)
		ctx, key := context.Background(), types.NamespacedName{Name: cr.Name}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		ns := &corev1.Namespace{}
		if err := c.Get(ctx, types.NamespacedName{Name: b.Namespace.Name}, ns); err != nil {
			t.Fatal(err)
		}
		if replaceNamespace {
			if err := c.Delete(ctx, ns); err != nil {
				t.Fatal(err)
			}
			ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: b.Namespace.Name, UID: "replacement-uid"}}
			if err := c.Create(ctx, ns); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Get(ctx, key, cr); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, cr); err != nil {
			t.Fatal(err)
		}
		r.CustomBindings = nil // Cleanup uses the durable record, not runtime dependencies.
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if replaceNamespace {
			if err == nil {
				t.Fatal("replaced namespace did not block cleanup")
			}
			if err := c.Get(ctx, types.NamespacedName{Name: b.Namespace.Name}, ns); err != nil || ns.UID != "replacement-uid" {
				t.Fatal("cleanup deleted the replacement namespace")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, key, &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("owned cleanup failed to finalize")
			}
		}
	}
}

func TestCustomAgentChangesDuringApprovalPreventApply(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, change := range []string{"suspend", "delete", "generation", "revoked"} {
		t.Run(change, func(t *testing.T) {
			cr, b, objects := customFixture("example", nil)
			scheme := testScheme()
			c := provisioningClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
			p := &customProvider{bindings: b}
			p.approve = func(_ *appsv1.StatefulSet) error {
				current := &api.Agent{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: cr.Name}, current); err != nil {
					return err
				}
				if change == "delete" {
					return c.Delete(context.Background(), current)
				}
				if change == "revoked" {
					p.leaseErr = fmt.Errorf("revoked")
					return nil
				}
				if change == "suspend" {
					current.Spec.Suspended = true
				} else {
					*current.Spec.Agent.Image = "registry.example.com/new:1"
				}
				current.Generation++
				return c.Update(context.Background(), current)
			}
			h := &fakeHelmClient{useHarness: true}
			r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: h, CustomBindings: p, EventRecorder: record.NewFakeRecorder(100)}
			controller.ConfigureTestGateway(t, r)
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
			if h.installCalled || len(p.completed) != 1 || p.completed[0] != controller.CustomApplyNotStarted {
				t.Fatal("stale/revoked approval was applied or not completed")
			}
		})
	}
}

type failStatusOnceClient struct {
	client.Client
	fail bool
}

func (c *failStatusOnceClient) Create(ctx context.Context, obj client.Object, options ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("synthetic-" + obj.GetName()))
	}
	return c.Client.Create(ctx, obj, options...)
}

type failStatusWriter struct {
	client.SubResourceWriter
	parent *failStatusOnceClient
}

func (c *failStatusOnceClient) Status() client.SubResourceWriter {
	return failStatusWriter{c.Client.Status(), c}
}

func (w failStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if w.parent.fail {
		w.parent.fail = false
		return fmt.Errorf("synthetic status failure")
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func TestCustomSuccessfulApplyRecoversStatusWithoutHandoff(t *testing.T) {
	requireLegacyCustomRuntime(t)
	cr, b, objects := customFixture("example", nil)
	scheme := testScheme()
	c := &failStatusOnceClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
	p, h := &customProvider{bindings: b}, &fakeHelmClient{useHarness: true}
	r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: h, CustomBindings: p, EventRecorder: record.NewFakeRecorder(100)}
	controller.ConfigureTestGateway(t, r)
	p.approve = func(*appsv1.StatefulSet) error {
		c.fail = true
		return nil
	}
	ctx, key := context.Background(), types.NamespacedName{Name: cr.Name}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err == nil || !h.installCalled {
		t.Fatal("expected application success followed by status failure")
	}
	sts := statefulSet(cr.Name, 1)
	sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: cr.Name, UID: cr.UID}}
	sts.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": string(cr.UID),
		"cetirun.microsoft.com/custom-plan-hash": h.lastValues["runtime"].(map[string]any)["workloadHash"].(string)}
	markGatewayCustomWorkload(sts, r.AIGateway.URL)
	sts.Status.CurrentRevision, sts.Status.UpdateRevision = "same-plan", "same-plan"
	if err := c.Create(ctx, sts); err != nil {
		t.Fatal(err)
	}
	p.applied, h.installCalled = false, false
	p.applyErr = fmt.Errorf("no new handoff authorized")
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if p.applied || h.installCalled || len(p.completed) != 1 || p.completed[0] != controller.CustomApplySucceeded {
		t.Fatal("status repair caused another handoff/application")
	}
	if err := c.Get(ctx, key, cr); err != nil || cr.Status.Phase != api.PhaseReady || cr.Status.ObservedOperatorHash != h.appliedKey {
		t.Fatal("successful application status was not recovered")
	}
}

func TestCustomInvalidResourcesDoNotRequestHandoff(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, body := range []string{
		`{"requests":{"cpu":"8"},"limits":{"cpu":"1"}}`,
		`{"requests":{"example.com/device":"1"}}`,
		`{"requests":{"hugepages-2Mi":"3Mi"},"limits":{"hugepages-2Mi":"3Mi"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			cr, b, objects := customFixture("example", nil)
			cr.Spec.Helm.Values["resources"] = runtime.RawExtension{Raw: []byte(body)}
			scheme := testScheme()
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()
			p, h := &customProvider{bindings: b}, &fakeHelmClient{useHarness: true}
			r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: h, CustomBindings: p, EventRecorder: record.NewFakeRecorder(100)}
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
			if p.applied || h.installCalled {
				t.Fatal("known-invalid resource request reached handoff")
			}
		})
	}
}
