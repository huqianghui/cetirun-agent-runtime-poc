// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionhandler "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nativeSCTPNetworkForTest() api.AgentPolicyNetwork {
	return api.AgentPolicyNetwork{
		AllowedInternalCIDRs: []api.AgentPolicyCIDRRule{{
			CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{
				{Port: 3868, Protocol: corev1.ProtocolSCTP}, {Port: 8080},
			},
		}},
		BlockedPorts: []api.AgentPolicyPort{{Port: 3869, Protocol: corev1.ProtocolSCTP}},
	}
}

func updateSCTPRegistrationForTest(t *testing.T, r *AgentReconciler, kube client.Client, registered bool) {
	t.Helper()
	ctx := context.Background()
	object := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, object); err != nil {
		t.Fatal(err)
	}
	var platform SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(object.Data[SandboxPlatformConfigurationKey]), &platform); err != nil {
		t.Fatal(err)
	}
	registration := platform.Registrations[api.SandboxKata]
	registration.Capabilities.Protocols = []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}
	if registered {
		registration.Capabilities.Protocols = append(registration.Capabilities.Protocols, corev1.ProtocolSCTP)
	}
	registration.Capabilities.BlockedPorts = true
	platform.Registrations[api.SandboxKata] = registration
	raw, err := json.Marshal(platform)
	if err != nil {
		t.Fatal(err)
	}
	object.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := kube.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
}

func withNativeSCTPForTest(t *testing.T, kind string) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		updateSCTPRegistrationForTest(t, r, kube, true)
		policy := &api.AgentPolicy{}
		ctx := context.Background()
		if err := kube.Get(ctx, client.ObjectKey{Name: testfixture.New(kind).Policy.Name}, policy); err != nil {
			t.Fatal(err)
		}
		policy.Spec.Network.AllowedInternalCIDRs = nativeSCTPNetworkForTest().AllowedInternalCIDRs
		policy.Spec.Network.BlockedPorts = nativeSCTPNetworkForTest().BlockedPorts
		if err := kube.Update(ctx, policy); err != nil {
			t.Fatal(err)
		}
	}
}

func assertReconciledSCTPControls(t *testing.T, policy sandboxpolicy.CiliumNetworkPolicy, present bool) {
	t.Helper()
	grant, deny := false, false
	for _, rule := range policy.Spec.Egress {
		if !slices.ContainsFunc(rule.ToCIDRSet, func(cidr sandboxpolicy.CIDRRule) bool {
			return cidr.CIDR == "10.20.0.0/16" && len(cidr.Except) == 0
		}) {
			continue
		}
		for _, group := range rule.ToPorts {
			grant = grant || slices.Contains(group.Ports, sandboxpolicy.PortRange{Port: "3868", Protocol: corev1.ProtocolSCTP})
		}
	}
	for _, rule := range policy.Spec.EgressDeny {
		if !slices.Contains(rule.ToEntities, "all") {
			continue
		}
		for _, group := range rule.ToPorts {
			deny = deny || slices.Contains(group.Ports, sandboxpolicy.PortRange{Port: "3869", Protocol: corev1.ProtocolSCTP})
		}
	}
	if grant != present || deny != present {
		t.Fatalf("owned Cilium SCTP controls: grant=%t deny=%t, want %t", grant, deny, present)
	}
}

func TestReconciledSandboxSCTPPolicyChanges(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(kind+"/"+string(mode), func(t *testing.T) {
				ctx := context.Background()
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
					withNativeHomeAccessMode(t, mode), withNativeSCTPForTest(t, kind))
				before := reconciledExpectationForTest(t, f)
				network := reconciledNetworkPolicyForTest(t, f)
				assertReconciledSCTPControls(t, network, true)
				assertNativeHomeAccessMode(t, f, mode)
				policy := &api.AgentPolicy{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: testfixture.New(kind).Policy.Name}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Spec.Network.AllowedInternalCIDRs, policy.Spec.Network.BlockedPorts = nil, nil
				policy.Generation++
				if err := f.kube.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
				reconciler := &AgentPolicyReconciler{
					Client: f.kube, APIReader: f.kube, Source: f.r.SandboxPreparation.Configuration, NativeExecution: true,
				}
				if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				after := reconciledExpectationForTest(t, f)
				removed := reconciledNetworkPolicyForTest(t, f)
				assertReconciledSCTPControls(t, removed, false)
				if after.Revision == before.Revision || after.PlanHash == before.PlanHash ||
					after.Home != before.Home || after.Agent != before.Agent || after.Namespace != before.Namespace ||
					removed.UID != network.UID {
					t.Fatal("SCTP policy removal retained stale controls or replaced the owned lifetime")
				}
			})
		}
	}
}

func TestReconciledSandboxSCTPRegistrationRevocation(t *testing.T) {
	ctx := context.Background()
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, false, withNativeSCTPForTest(t, "openclaw"))
	expected := reconciledExpectationForTest(t, f)
	pod := reconciledPodForTest(t, f, expected.Workloads[0])
	handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
	if err != nil {
		t.Fatal(err)
	}
	if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); !response.Allowed {
		t.Fatal("registered current SCTP Pod was denied:", response.Result)
	}
	if err := f.kube.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	if err := f.kube.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	workload := f.workload(t)
	workload.Status.ObservedGeneration = workload.Generation
	workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
	if err := f.kube.Status().Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	if f.current(t).Status.Phase != api.PhaseReady {
		t.Fatal("fixture must be running and ready before protocol revocation")
	}
	before := f.installer.stages
	updateSCTPRegistrationForTest(t, f.r, f.kube, false)
	if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); response.Allowed {
		t.Fatal("changed protocol registration still authorized the old Pod")
	}
	f.reconcileOK(t)
	f.assertNotReady(t)
	if err := f.kube.Get(ctx, client.ObjectKey{
		Namespace: expected.Namespace.Name, Name: virtualServiceName(f.agent.Name),
	}, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatal("protocol revocation retained external routing:", err)
	}
	if f.installer.stages != before || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 ||
		!slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
		t.Fatal("protocol revocation applied new resources or failed to hold the running workload")
	}
}
