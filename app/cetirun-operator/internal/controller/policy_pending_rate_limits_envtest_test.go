// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxPendingRateLimitsAPIServer(t *testing.T) {
	for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
		t.Run(string(mode), func(t *testing.T) {
			h := newReconciledStartAPIHarness(t, mode)
			expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			observed := *h.nativeSandboxFixture
			observed.kube = h.admin
			pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
			pod.Spec.Overhead = nil
			h.create(t, pod)
			agent, workload := h.current(t), h.workload(t)
			if agent.Status.Phase == api.PhaseReady {
				t.Fatal("served fixture must still be waiting for its first Ready observation")
			}
			store := h.r.SandboxNativeCoordination
			reservation, found, err := store.ReadNative(h.ctx, agent.UID)
			if err != nil || !found {
				t.Fatal("missing initial common reservation:", err)
			}
			token := aigateway.Token(h.gateway.generated[0].Key)
			h.gateway.keys[token].Spend = ptr.To(2.5)
			gateway := &policyLimitBlockRecorder{fakeAIGateway: h.gateway}
			h.r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
			policyKey := client.ObjectKey{Name: agent.Status.Policy.EffectivePolicyRef.Name}
			policies := &AgentPolicyReconciler{
				Client: h.kube, APIReader: h.kube, NativeExecution: true, Source: h.r.SandboxPreparation.Configuration,
			}
			catchUp, policyRead, stopWrites := false, false, 0
			checkStop := func(object client.Object) {
				if current, ok := object.(*appsv1.StatefulSet); ok && current.UID == workload.UID &&
					ptr.Deref(current.Spec.Replicas, 1) == 0 {
					stopWrites++
				}
			}
			writer := interceptor.NewClient(h.kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*api.Team); catchUp && policyRead && ok && key.Name == agent.Spec.Team {
						catchUp = false
						if _, err := policies.Reconcile(ctx, ctrl.Request{NamespacedName: policyKey}); err != nil {
							return err
						}
					}
					err := c.Get(ctx, key, object, opts...)
					if policy, ok := object.(*unstructured.Unstructured); err == nil && ok &&
						policy.GetKind() == "AgentPolicy" && key == policyKey {
						policyRead = true
					}
					return err
				},
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					checkStop(object)
					return c.Update(ctx, object, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					checkStop(object)
					return c.Patch(ctx, object, patch, opts...)
				},
			})
			h.r.Client, h.r.APIReader = writer, writer
			stages := h.installer.stages
			for _, step := range []struct {
				name   string
				limits *api.AgentPolicyRateLimits
				want   aigateway.KeyLimits
			}{
				{"add", exampleRateLimits(), exampleKeyLimits()},
				{"remove", nil, aigateway.KeyLimits{}},
			} {
				before := &api.AgentPolicy{}
				if err := h.admin.Get(h.ctx, policyKey, before); err != nil {
					t.Fatal(err)
				}
				after := before.DeepCopy()
				after.Spec.ModelAccess.RateLimits = step.limits
				change, err := store.AcquireConfigurationChange(h.ctx, before, after)
				if err != nil {
					t.Fatal("reserve limits-only policy update:", err)
				}
				adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(h.admin, h.admin, before, after)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.ReconcileChange(h.ctx, change, adapter); err != nil {
					t.Fatal("commit limits-only policy update:", err)
				}
				catchUp, policyRead = true, false
				h.reconcileOK(t)
				if err := h.admin.Get(h.ctx, policyKey, after); err != nil {
					t.Fatal(err)
				}
				if after.UID != before.UID || after.ResourceVersion == before.ResourceVersion ||
					after.Generation != before.Generation+1 || after.Status.ObservedGeneration != after.Generation || catchUp {
					t.Fatal("fixture did not catch policy acceptance up during bootstrap")
				}
				current, found, err := store.ReadNative(h.ctx, agent.UID)
				updated, actual := h.current(t), h.workload(t)
				key := h.gateway.keys[token]
				if err != nil || !found || current.Reservation.ID != reservation.Reservation.ID ||
					current.Reservation.Phase != sandboxcoordination.Reserved ||
					!reflect.DeepEqual(current.Reservation.Intent.SourceDigests, reservation.Reservation.Intent.SourceDigests) ||
					h.installer.stages != stages || stopWrites != 0 || gateway.blocks != 0 ||
					updated.UID != agent.UID || !reflect.DeepEqual(updated.Spec, agent.Spec) ||
					updated.Status.Phase != agent.Status.Phase || updated.Status.AIGateway.KeyID != agent.Status.AIGateway.KeyID ||
					h.pod(t).UID != pod.UID || actual.UID != workload.UID || !reflect.DeepEqual(actual.Spec, workload.Spec) ||
					reconciledExpectationForTest(t, h.nativeSandboxFixture).Home != expected.Home ||
					key == nil || key.Blocked || !key.Limits.Equal(step.want) || ptr.Deref(key.Spend, 0) != 2.5 ||
					len(h.gateway.generated) != 1 || len(h.gateway.deletedTokens) != 0 || len(h.gateway.deletedAliases) != 0 {
					t.Fatalf("%s interrupted the pending served lifetime: err=%v phase=%s applies=%d/%d stops=%d blocks=%d",
						step.name, err, updated.Status.Phase, h.installer.stages, stages, stopWrites, gateway.blocks)
				}
				mixed := after.DeepCopy()
				mixed.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
				if _, err := store.AcquireConfigurationChange(h.ctx, after, mixed); !errors.Is(err, sandboxcoordination.ErrConflict) {
					t.Fatal("mixed policy edit bypassed the active reservation:", err)
				}
				if err := h.admin.Update(h.ctx, mixed); !apierrors.IsForbidden(err) {
					t.Fatal("served policy admission accepted a mixed edit without stopping:", err)
				}
			}
		})
	}
}
