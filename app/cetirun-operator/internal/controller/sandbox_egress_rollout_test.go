// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func configureRolloutStableEgress(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		updateContainerPlatformForTest(t, r, kube, func(document *SandboxPlatformConfiguration) {
			for backend, platform := range document.Platforms {
				platform.Topology.DNS[0].PodSelector = map[string]string{"k8s-app": "kube-dns"}
				platform.Topology.Gateway[0].Peer.PodSelector = map[string]string{
					"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway",
					"app.kubernetes.io/instance": "example",
				}
				document.Platforms[backend] = platform
			}
		})
	}
}

func TestReconciledSandboxEgressRolloutDoesNotRestartAgent(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		for _, kind := range []string{"openclaw", "hermes"} {
			t.Run(string(backend)+"/"+kind, func(t *testing.T) {
				ctx := context.Background()
				setup := []func(*AgentReconciler, client.Client){configureRolloutStableEgress(t)}
				if backend == api.SandboxContainer {
					setup = append(setup, withContainerRuntime(t))
				}
				setup = append(setup, withReconciledCoordination(t))
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, setup...)
				workload := f.workload(t)
				workload.Status.ObservedGeneration = workload.Generation
				workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				beforeAgent, beforeWorkload := f.current(t), f.workload(t)
				if beforeAgent.Status.Phase != api.PhaseReady {
					t.Fatal("fixture must be ready before DNS or Gateway replacement")
				}
				beforeExpectation := reconciledExpectationForTest(t, f)
				beforeNetwork := reconciledNetworkPolicyForTest(t, f)
				beforeStages := f.installer.stages
				for _, peer := range []struct {
					name, namespace string
					labels          map[string]string
				}{
					{"dns", "kube-system", map[string]string{"k8s-app": "kube-dns"}},
					{"gateway", f.r.OperatorNamespace, map[string]string{
						"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway",
						"app.kubernetes.io/instance": "example",
					}},
				} {
					for generation := range 3 {
						pod := &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name: fmt.Sprintf("%s-rollout-%d", peer.name, generation), Namespace: peer.namespace,
								UID: types.UID(fmt.Sprintf("%s-pod-uid-%d", peer.name, generation)), Labels: peer.labels,
							},
							Status: corev1.PodStatus{PodIP: fmt.Sprintf("10.244.%d.25", generation)},
						}
						if err := f.kube.Create(ctx, pod); err != nil {
							t.Fatal(err)
						}
						f.reconcileOK(t)
						if err := f.kube.Delete(ctx, pod); err != nil {
							t.Fatal(err)
						}
					}
				}
				f.reconcileOK(t)
				afterAgent, afterWorkload := f.current(t), f.workload(t)
				if afterAgent.Status.Phase != api.PhaseReady ||
					!reflect.DeepEqual(beforeAgent.Spec, afterAgent.Spec) ||
					beforeAgent.Status.AIGateway.KeyID != afterAgent.Status.AIGateway.KeyID ||
					!reflect.DeepEqual(beforeWorkload.Spec, afterWorkload.Spec) ||
					beforeWorkload.UID != afterWorkload.UID || beforeStages != f.installer.stages ||
					!reflect.DeepEqual(beforeExpectation, reconciledExpectationForTest(t, f)) ||
					!reflect.DeepEqual(beforeNetwork, reconciledNetworkPolicyForTest(t, f)) {
					t.Fatal("DNS or Gateway Pod replacement reauthorized, rerendered or disrupted the Agent")
				}
			})
		}
	}
}
