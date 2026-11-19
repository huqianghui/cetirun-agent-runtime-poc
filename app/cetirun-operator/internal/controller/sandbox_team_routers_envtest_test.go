// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxTeamRoutersAPIServer(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
			t.Run(kind+"/"+string(backend), func(t *testing.T) {
				h := newNativeStartAPIHarnessWithRuntime(t, true, true, corev1.ReadWriteOncePod, kind, backend, nil)
				h = startReconciledAPIHarness(t, h)
				workload := h.workload(t)
				workload.Status.ObservedGeneration = workload.Generation
				workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
				if err := h.admin.Status().Update(h.ctx, workload); err != nil {
					t.Fatal(err)
				}
				h.reconcileOK(t)
				agent, workload := h.current(t), h.workload(t)
				if agent.Status.Phase != api.PhaseReady {
					t.Fatal("served fixture did not become Ready")
				}
				token := aigateway.Token(h.gateway.generated[0].Key)
				h.gateway.keys[token].Spend = ptr.To(2.5)
				original := testRouter("ungranted-router", testSharedModelOne, testSharedModelOne, testSharedModelOne)
				changed := original
				changed.Tiers.Simple = testSmallModel
				for _, routers := range [][]api.TeamModelRouter{{original}, {changed}, nil} {
					before := &api.Team{}
					teamKey := client.ObjectKey{Name: agent.Spec.Team}
					if err := h.admin.Get(h.ctx, teamKey, before); err != nil {
						t.Fatal(err)
					}
					after := before.DeepCopy()
					after.Spec.ModelRouters = routers
					store := h.r.SandboxNativeCoordination
					change, err := store.AcquireConfigurationChange(h.ctx, before, after)
					if err != nil {
						t.Fatal("router-only Team update conflicted with running execution:", err)
					}
					adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(h.admin, h.admin, before, after)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.ReconcileChange(h.ctx, change, adapter); err != nil {
						t.Fatal("served router-only Team update was rejected:", err)
					}
					for _, observed := range []bool{false, true} {
						if observed {
							teams := &TeamReconciler{Client: h.kube, APIReader: h.admin}
							if _, err := teams.Reconcile(h.ctx, ctrl.Request{NamespacedName: teamKey}); err != nil {
								t.Fatal(err)
							}
						}
						h.reconcileOK(t)
						current, updated := h.current(t), h.workload(t)
						if current.Status.Phase != api.PhaseReady || updated.UID != workload.UID ||
							updated.Generation != workload.Generation || !reflect.DeepEqual(updated.Spec, workload.Spec) ||
							current.Status.Sandbox.AppliedRevision != agent.Status.Sandbox.AppliedRevision {
							t.Fatalf("router-only change rolled the served workload: observed=%t", observed)
						}
						key := h.gateway.keys[token]
						if key == nil || key.Blocked || ptr.Deref(key.Spend, 0) != 2.5 ||
							current.Status.AIGateway.KeyID != agent.Status.AIGateway.KeyID || len(h.gateway.generated) != 1 {
							t.Fatal("served router update replaced or blocked the Gateway key")
						}
					}
				}
			})
		}
	}
}
