// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestWorkloadReadinessUsesCurrentRollout(t *testing.T) {
	for _, scenario := range []string{"rollout-pending", "rollout-complete", "read-failed", "missing", "client-only"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example-agent"}}
			current := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name),
					UID: "current-workload", Generation: 2,
				},
				Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1,
				},
			}
			cached := current.DeepCopy()
			cached.Generation = 1
			if scenario == "rollout-complete" {
				current.Status.ObservedGeneration = current.Generation
				cached.Status.ReadyReplicas = 0
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
			if scenario == "missing" {
				if err := kube.Delete(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
			cachedReads, currentReads := 0, 0
			cachedClient := interceptor.NewClient(kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if workload, ok := object.(*appsv1.StatefulSet); ok {
						cachedReads++
						cached.DeepCopyInto(workload)
						return nil
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			injected := errors.New("synthetic current workload read failure")
			reader := interceptor.NewClient(kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					currentReads++
					if scenario == "read-failed" {
						return injected
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			r := &AgentReconciler{Client: cachedClient, APIReader: reader}
			if scenario == "client-only" {
				r.APIReader = nil
			}
			ready, result, err := r.observeWorkload(ctx, agent)
			wantReady := scenario == "rollout-complete" || scenario == "client-only"
			if ready != wantReady || (err != nil) != (scenario == "read-failed") {
				t.Fatalf("readiness used stale cache state: ready=%t want=%t result=%+v err=%v", ready, wantReady, result, err)
			}
			if scenario == "read-failed" && !errors.Is(err, injected) {
				t.Fatal("current read failure was hidden:", err)
			}
			if scenario != "client-only" && (cachedReads != 0 || currentReads != 1) {
				t.Fatalf("readiness did not use exactly one current read: cached=%d current=%d", cachedReads, currentReads)
			}
		})
	}
}
