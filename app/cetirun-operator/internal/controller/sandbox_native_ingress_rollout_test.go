// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func configureRolloutStableIngress(t *testing.T, kind string) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		ctx := context.Background()
		cm := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, cm); err != nil {
			t.Fatal(err)
		}
		var document SandboxPlatformConfiguration
		if err := json.Unmarshal([]byte(cm.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
			t.Fatal(err)
		}
		platform := document.Platforms[api.SandboxKata]
		ingress := &platform.Topology.Ingress[0]
		ingress.Peer.Namespace, ingress.Peer.IPs = "aks-istio-ingress", nil
		ingress.Peer.PodSelector = map[string]string{"istio": "aks-istio-ingressgateway-external"}
		ingress.TrustedProxyCIDRs = []string{"10.244.0.0/16"}
		if kind == "hermes" {
			ingress.Ports = []api.AgentPolicyPort{{Port: 8642, Protocol: corev1.ProtocolTCP}, {Port: 9120, Protocol: corev1.ProtocolTCP}}
		}
		document.Platforms[api.SandboxKata] = platform
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		cm.Data[SandboxPlatformConfigurationKey] = string(raw)
		if err := kube.Update(ctx, cm); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeSandboxIngressRolloutDoesNotRestartAgent(t *testing.T) {
	ctx := context.Background()
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, false, "openclaw", true, false,
		withNativePublishedSkills(t), configureRolloutStableIngress(t, "openclaw"))
	gateway := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gateway-before-rollout", Namespace: "aks-istio-ingress", UID: "old-gateway-pod-uid",
			Labels: map[string]string{"istio": "aks-istio-ingressgateway-external"},
		},
		Status: corev1.PodStatus{PodIP: "10.244.0.10"},
	}
	if err := f.kube.Create(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	f.ready(t)
	beforeAgent, beforeWorkload, beforePod := f.current(t), f.workload(t), f.pod(t)
	_, beforeExpectation := f.expectation(t)
	beforeStages := f.installer.stages
	if err := f.kube.Delete(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	gateway = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gateway-after-rollout", Namespace: "aks-istio-ingress", UID: "new-gateway-pod-uid",
			Labels: map[string]string{"istio": "aks-istio-ingressgateway-external"},
		},
		Status: corev1.PodStatus{PodIP: "10.244.1.25"},
	}
	if err := f.kube.Create(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	f.assertReady(t)
	afterAgent, afterWorkload, afterPod := f.current(t), f.workload(t), f.pod(t)
	_, afterExpectation := f.expectation(t)
	if !reflect.DeepEqual(beforeAgent.Spec, afterAgent.Spec) ||
		beforeAgent.Status.Sandbox.AppliedRevision != afterAgent.Status.Sandbox.AppliedRevision ||
		!reflect.DeepEqual(beforeWorkload.Spec, afterWorkload.Spec) || beforeWorkload.UID != afterWorkload.UID ||
		beforePod.UID != afterPod.UID || beforeStages != f.installer.stages ||
		!reflect.DeepEqual(beforeExpectation, afterExpectation) {
		t.Fatal("ingress Pod replacement changed Agent authorization, rendering or the running lifetime")
	}
}

func TestReconciledSandboxIngressRolloutDoesNotRestartAgent(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
				configureRolloutStableIngress(t, kind))
			workload := f.workload(t)
			workload.Status.ObservedGeneration = workload.Generation
			workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1
			if err := f.kube.Status().Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			beforeAgent, beforeWorkload := f.current(t), f.workload(t)
			beforeExpectation := reconciledExpectationForTest(t, f)
			if beforeAgent.Status.Phase != api.PhaseReady {
				t.Fatal("fixture must be ready before the gateway rollout")
			}
			beforeStages := f.installer.stages
			gateway := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "gateway-before-rollout", Namespace: "aks-istio-ingress", UID: "old-gateway-pod-uid",
					Labels: map[string]string{"istio": "aks-istio-ingressgateway-external"},
				},
				Status: corev1.PodStatus{PodIP: "10.244.0.10"},
			}
			if err := f.kube.Create(ctx, gateway); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Delete(ctx, gateway); err != nil {
				t.Fatal(err)
			}
			gateway = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "gateway-after-rollout", Namespace: "aks-istio-ingress", UID: "new-gateway-pod-uid",
					Labels: map[string]string{"istio": "aks-istio-ingressgateway-external"},
				},
				Status: corev1.PodStatus{PodIP: "10.244.1.25"},
			}
			if err := f.kube.Create(ctx, gateway); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			afterAgent, afterWorkload := f.current(t), f.workload(t)
			afterExpectation := reconciledExpectationForTest(t, f)
			if afterAgent.Status.Phase != api.PhaseReady || !reflect.DeepEqual(beforeAgent.Spec, afterAgent.Spec) ||
				beforeAgent.Status.AIGateway.KeyID != afterAgent.Status.AIGateway.KeyID ||
				!reflect.DeepEqual(beforeWorkload.Spec, afterWorkload.Spec) || beforeWorkload.UID != afterWorkload.UID ||
				beforeStages != f.installer.stages || !reflect.DeepEqual(beforeExpectation, afterExpectation) {
				t.Fatal("ingress Pod replacement reauthorized, rerendered or disrupted the Agent")
			}
		})
	}
}
