// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func withCommonTerminalTopology(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		updateContainerPlatformForTest(t, r, kube, func(document *SandboxPlatformConfiguration) {
			platform := document.Platforms[api.SandboxKata]
			platform.Topology.KubernetesAPI = []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "api", UID: "example-api-uid", IPs: []string{"203.0.113.10"}},
				Ports: []api.AgentPolicyPort{{Port: 6443, Protocol: corev1.ProtocolTCP}},
			}}
			document.Platforms[api.SandboxKata] = platform
		})
	}
}

func customTerminalFixture(t *testing.T, backend string) *nativeSandboxFixture {
	t.Helper()
	setup := []func(*AgentReconciler, client.Client){
		withCommonCustom(t, "chat"), withReconciledCoordination(t), withCommonTerminalTopology(t),
	}
	if backend == "container" {
		setup = append(setup, withContainerRuntime(t))
	}
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "custom", true, false, setup...)
	f.r.CetiRunHelmValues["ttyd"].(map[string]any)["enabled"] = true
	if err := f.kube.Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: instanceNamespace(f.agent.Name), Name: "kube-root-ca.crt"},
		Data:       map[string]string{"ca.crt": "synthetic-ca"},
	}); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		_, err := f.reconcile()
		if err != nil && !strings.Contains(err.Error(), "common Custom terminal is not ready") {
			t.Fatal("prepare terminal:", err)
		}
		terminal := &appsv1.Deployment{}
		if err := f.kube.Get(context.Background(), client.ObjectKey{
			Namespace: instanceNamespace(f.agent.Name), Name: terminalpolicy.WorkloadName(instanceServiceName(f.agent.Name)),
		}, terminal); err == nil {
			return f
		} else if client.IgnoreNotFound(err) != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("Custom terminal was not created")
	return nil
}

func customTerminalDeployment(t *testing.T, f *nativeSandboxFixture) *appsv1.Deployment {
	t.Helper()
	deployment := &appsv1.Deployment{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{
		Namespace: instanceNamespace(f.agent.Name), Name: terminalpolicy.WorkloadName(instanceServiceName(f.agent.Name)),
	}, deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

func readyCustomWorkload(t *testing.T, f *nativeSandboxFixture) {
	t.Helper()
	main := f.workload(t)
	main.Status.ObservedGeneration = main.Generation
	main.Status.Replicas, main.Status.ReadyReplicas, main.Status.UpdatedReplicas = 1, 1, 1
	if err := f.kube.Status().Update(context.Background(), main); err != nil {
		t.Fatal(err)
	}
}

func TestCommonCustomTerminalWaitsForCurrentDeployment(t *testing.T) {
	f := customTerminalFixture(t, "container")
	for _, scenario := range []struct {
		name                     string
		replicas, ready, updated int32
		available                int32
		wantReady                bool
	}{
		{name: "old available pod", replicas: 1, ready: 1, available: 1},
		{name: "surge replacement pending", replicas: 2, ready: 1, updated: 1, available: 1},
		{name: "old pod still draining", replicas: 2, ready: 2, updated: 1, available: 2},
		{name: "current rollout complete", replicas: 1, ready: 1, updated: 1, available: 1, wantReady: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			deployment := customTerminalDeployment(t, f)
			deployment.Status.ObservedGeneration = deployment.Generation
			deployment.Status.Replicas, deployment.Status.ReadyReplicas = scenario.replicas, scenario.ready
			deployment.Status.UpdatedReplicas, deployment.Status.AvailableReplicas = scenario.updated, scenario.available
			if err := f.kube.Status().Update(context.Background(), deployment); err != nil {
				t.Fatal(err)
			}
			ready, err := f.r.customTerminalReady(context.Background(), f.current(t))
			if err != nil || ready != scenario.wantReady {
				t.Fatalf("terminal accepted an earlier or incomplete rollout: ready=%t want=%t err=%v", ready, scenario.wantReady, err)
			}
		})
	}
}

func TestCommonCustomTerminalReadinessAndSuspension(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		t.Run(backend, func(t *testing.T) {
			f := customTerminalFixture(t, backend)
			readyCustomWorkload(t, f)
			if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "common Custom terminal is not ready") {
				t.Fatal("terminal rollout was not reported:", err)
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 || ptr.Deref(customTerminalDeployment(t, f).Spec.Replicas, 0) != 1 {
				t.Fatal("waiting for the terminal stopped the rollout")
			}
			terminal := customTerminalDeployment(t, f)
			terminal.Status.ObservedGeneration = terminal.Generation
			terminal.Status.Replicas, terminal.Status.ReadyReplicas, terminal.Status.AvailableReplicas = 1, 1, 1
			terminal.Status.UpdatedReplicas = 1
			if err := f.kube.Status().Update(context.Background(), terminal); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				f.reconcileOK(t)
			}
			if f.current(t).Status.Phase != api.PhaseReady || !isConditionTrue(f.current(t).Status.Conditions, api.ConditionTerminalConfigured) {
				t.Fatal("current terminal did not become available")
			}
			expected := reconciledExpectationForTest(t, f)
			if expected.Terminal == nil || len(expected.Workloads) != 2 {
				t.Fatal("terminal was not included in common execution authority")
			}
			for _, workload := range expected.Workloads {
				if workload.Kind == "Deployment" && (backend == "kata") != (len(workload.Overhead) != 0) {
					t.Fatal("terminal did not retain the selected RuntimeClass overhead")
				}
			}
			key, stages := f.current(t).Status.AIGateway.KeyID, f.installer.stages
			f.reconcileOK(t)
			if f.installer.stages != stages {
				t.Fatal("steady terminal reconciliation reapplied resources")
			}
			agent := f.current(t)
			agent.Spec.Suspended, agent.Generation = true, agent.Generation+1
			if err := f.kube.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "waits for acknowledged controller shutdown") {
				t.Fatal("suspension did not wait for controller acknowledgement:", err)
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 ||
				ptr.Deref(customTerminalDeployment(t, f).Spec.Replicas, -1) != 0 ||
				!f.current(t).Status.AIGateway.Blocked || f.current(t).Status.AIGateway.KeyID != key {
				t.Fatal("suspension failed to stop both workloads and retain the blocked accounting identity")
			}
			main := f.workload(t)
			main.Status = appsv1.StatefulSetStatus{ObservedGeneration: main.Generation}
			if err := f.kube.Status().Update(context.Background(), main); err != nil {
				t.Fatal(err)
			}
			terminal = customTerminalDeployment(t, f)
			terminal.Status = appsv1.DeploymentStatus{ObservedGeneration: terminal.Generation}
			if err := f.kube.Status().Update(context.Background(), terminal); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			if f.current(t).Status.Phase != api.PhaseSuspended {
				t.Fatal("acknowledged shutdown did not finish retained suspension")
			}
		})
	}
}

func TestCommonCustomTerminalConnectUsesRenderedAuthority(t *testing.T) {
	ctx := context.Background()
	f := customTerminalFixture(t, "kata")
	readyCustomWorkload(t, f)
	expected := reconciledExpectationForTest(t, f)
	var pod *corev1.Pod
	for _, workload := range expected.Workloads {
		candidate := reconciledPodForTest(t, f, workload)
		handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{
			StateNamespace: f.r.OperatorNamespace, CoordinationConfigMapName: f.r.SandboxNativeCoordination.Identity.Name,
			CoordinationConfigMapUID: string(f.r.SandboxNativeCoordination.Identity.UID),
		})
		if err != nil {
			t.Fatal(err)
		}
		if response := handler.Validate(ctx, reconciledPodRequest(t, candidate)); !response.Allowed {
			t.Fatal("rendered workload failed Pod admission:", response.Result)
		}
		if workload.Kind == "StatefulSet" {
			pod = candidate
		}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a", UID: "example-node-uid",
		Labels: map[string]string{corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone"},
	}}
	if err := f.kube.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = node.Name
	pod.Labels[corev1.LabelTopologyRegion] = node.Labels[corev1.LabelTopologyRegion]
	pod.Labels[corev1.LabelTopologyZone] = node.Labels[corev1.LabelTopologyZone]
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "cetirun", Ready: true, ContainerID: "containerd://synthetic-main",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}
	if err := f.kube.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{
		StateNamespace: f.r.OperatorNamespace, CoordinationConfigMapName: f.r.SandboxNativeCoordination.Identity.Name,
		CoordinationConfigMapUID: string(f.r.SandboxNativeCoordination.Identity.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	update := reconciledPodRequest(t, pod)
	update.Operation = admissionv1.Update
	update.OldObject = update.Object
	if response := handler.Validate(ctx, update); !response.Allowed {
		t.Fatal("current bound Pod update was denied:", response.Result)
	}
	otherNode := node.DeepCopy()
	otherNode.Name, otherNode.UID, otherNode.ResourceVersion = "other-node", "other-node-uid", ""
	if err := f.kube.Create(ctx, otherNode); err != nil {
		t.Fatal(err)
	}
	moved := pod.DeepCopy()
	moved.Spec.NodeName = otherNode.Name
	update.Object = reconciledPodRequest(t, moved).Object
	if response := handler.Validate(ctx, update); response.Allowed {
		t.Fatal("Pod update reassigned a bound Pod to another observed Node")
	}
	options, err := json.Marshal(corev1.PodExecOptions{
		Container: "cetirun", Command: []string{"sh", "-i"}, Stdout: true, Stdin: true, TTY: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Connect, Namespace: pod.Namespace, Name: pod.Name, SubResource: "exec", RequestSubResource: "exec",
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"}, Object: runtime.RawExtension{Raw: options},
		UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:" + pod.Namespace + ":" + expected.Terminal.ServiceAccountName},
	}}
	if response := handler.Validate(ctx, request); !response.Allowed {
		t.Fatal("current terminal was denied:", response.Result)
	}
	for _, mutate := range []func(*admission.Request){
		func(r *admission.Request) {
			r.UserInfo.Username = "system:serviceaccount:" + pod.Namespace + ":foreign"
		},
		func(r *admission.Request) { r.Name = "other-0" },
		func(r *admission.Request) { r.SubResource = "attach" },
		func(r *admission.Request) { r.SubResource = "portforward" },
		func(r *admission.Request) {
			r.Object.Raw = []byte(`{"container":"other","command":["sh","-i"],"stdin":true,"stdout":true,"tty":true}`)
		},
		func(r *admission.Request) {
			r.Object.Raw = []byte(`{"container":"cetirun","command":["sh","-c","id"],"stdout":true}`)
		},
	} {
		changed := request
		mutate(&changed)
		if response := handler.Validate(ctx, changed); response.Allowed {
			t.Fatal("terminal authority admitted a different caller, target or protocol")
		}
	}
	node.Labels[corev1.LabelTopologyZone] = "changed-zone"
	if err := f.kube.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if response := handler.Validate(ctx, request); response.Allowed {
		t.Fatal("terminal accepted topology from a stale Node observation")
	}
	node.Labels[corev1.LabelTopologyZone] = "example-zone"
	if err := f.kube.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, node); err != nil {
		t.Fatal(err)
	}
	if response := handler.Validate(ctx, request); response.Allowed || !strings.Contains(response.Result.Message, "Node") {
		t.Fatal("terminal did not fail closed when its assigned Node disappeared:", response.Result)
	}
	node.ResourceVersion = ""
	if err := f.kube.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	agent.Spec.Suspended, agent.Generation = true, agent.Generation+1
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if response := handler.Validate(ctx, request); response.Allowed {
		t.Fatal("terminal CONNECT survived a lifecycle change")
	}
}
