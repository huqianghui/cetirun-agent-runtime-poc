// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxSCTPNetworkAPIServer(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	network := nativeSCTPNetworkForTest()
	h := newReconciledStartAPIHarnessWithRegistration(t, corev1.ReadWriteOnce,
		func(c *sandbox.Capabilities) { c.Protocols = append(c.Protocols, corev1.ProtocolSCTP) }, network)
	expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
	compiled := reconciledNetworkPolicyForTest(t, h.nativeSandboxFixture)
	assertReconciledSCTPControls(t, compiled, true)

	policy := &api.AgentPolicy{}
	team := &api.Team{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
		t.Fatal(err)
	}
	ports := policy.Spec.Network.AllowedInternalCIDRs[0].Ports
	if !slices.Contains(ports, api.AgentPolicyPort{Port: 3868, Protocol: corev1.ProtocolSCTP}) ||
		!slices.Contains(ports, api.AgentPolicyPort{Port: 8080, Protocol: corev1.ProtocolTCP}) ||
		!slices.Equal(policy.Spec.Network.BlockedPorts, network.BlockedPorts) {
		t.Fatal("served API lost SCTP or changed the default TCP port")
	}
	for _, tc := range []struct {
		name, cidr, message string
		protocol            corev1.Protocol
	}{
		{"node", "192.168.0.0/16", "internal exceptions", corev1.ProtocolSCTP},
		{"management", "10.1.0.0/16", "internal exceptions", corev1.ProtocolSCTP},
		{"metadata", "169.254.169.254/32", "internal exceptions", corev1.ProtocolSCTP},
		{"service-vip", "10.96.2.20/32", "Service VIP", corev1.ProtocolSCTP},
		{"unregistered-family", "fd00:20::/64", "family", corev1.ProtocolSCTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "sctp-denied-" + tc.name},
				Spec: api.AgentPolicySpec{
					Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
					Network: api.AgentPolicyNetwork{AllowedInternalCIDRs: []api.AgentPolicyCIDRRule{{
						CIDR: tc.cidr, Ports: []api.AgentPolicyPort{{Port: 3868, Protocol: tc.protocol}},
					}}},
				},
			}
			h.create(t, candidate)
			team := &api.Team{
				ObjectMeta: metav1.ObjectMeta{Name: candidate.Name},
				Spec: api.TeamSpec{
					AgentPolicyRef: candidate.Name, SandboxRefs: []string{h.agent.Spec.SandboxRef},
					DefaultSandboxRef: h.agent.Spec.SandboxRef,
				},
			}
			if err := h.admin.Create(h.ctx, team); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), tc.message) {
				t.Fatal("served binding admission accepted an unsupported SCTP destination:", err)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		port     int32
		protocol corev1.Protocol
	}{
		{"lowercase-protocol", 3868, "sctp"},
		{"unknown-protocol", 3868, "OTHER"},
		{"zero-port", 0, corev1.ProtocolSCTP},
		{"oversized-port", 65536, corev1.ProtocolSCTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "sctp-invalid-" + tc.name},
				Spec: api.AgentPolicySpec{
					Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
					Network: api.AgentPolicyNetwork{
						BlockedPorts: []api.AgentPolicyPort{{Port: tc.port, Protocol: tc.protocol}},
					},
				},
			}
			if err := h.admin.Create(h.ctx, candidate); err == nil || (!apierrors.IsInvalid(err) && !apierrors.IsForbidden(err)) {
				t.Fatal("served API accepted malformed SCTP port input:", err)
			}
		})
	}
	policy.Spec.Network.AllowedInternalCIDRs, policy.Spec.Network.BlockedPorts = nil, nil
	if err := h.admin.Update(h.ctx, policy); !apierrors.IsForbidden(err) {
		t.Fatal("active source reservation did not protect SCTP rules:", err)
	}
	observed := *h.nativeSandboxFixture
	observed.kube = h.admin
	pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
	pod.Spec.Overhead = nil
	if err := h.admin.Create(h.ctx, pod, client.DryRunAll); err != nil {
		t.Fatal("served Pod admission rejected current SCTP controls:", err)
	}
	h.create(t, pod)
	if pod.UID == "" {
		t.Fatal("served API did not persist the admitted SCTP Pod")
	}
	h.observeReadyPod(t, pod)
	workload := h.workload(t)
	workload.Status.ObservedGeneration = workload.Generation
	workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
	if err := h.admin.Status().Update(h.ctx, workload); err != nil {
		t.Fatal(err)
	}
	h.reconcileOK(t)
	if h.current(t).Status.Phase != api.PhaseReady {
		t.Fatal("synthetic running Pod must be ready before source drift")
	}
	object := &unstructured.Unstructured{}
	object.SetAPIVersion("cilium.io/v2")
	object.SetKind("CiliumNetworkPolicy")
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: compiled.Namespace, Name: compiled.Name}, object); err != nil {
		t.Fatal(err)
	}
	before := object.DeepCopy()
	if err := unstructured.SetNestedSlice(object.Object, []any{}, "spec", "egressDeny"); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Update(h.ctx, object); err != nil {
		t.Fatal(err)
	}
	pod = h.pod(t)
	if err := h.admin.Update(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Fatal("served admission accepted an existing Pod after SCTP control drift:", err)
	}
	object.Object["spec"] = before.Object["spec"]
	if err := h.admin.Update(h.ctx, object); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Update(h.ctx, pod, client.DryRunAll); err != nil {
		t.Fatal("restored current controls did not readmit the unchanged Pod:", err)
	}

	// Platform inventory is administrator-controlled, not an AgentPolicy
	// mutation. A changed declaration must invalidate existing Pod authority.
	platform := &corev1.ConfigMap{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: h.r.OperatorNamespace, Name: "sandbox-platform"}, platform); err != nil {
		t.Fatal(err)
	}
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(platform.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
		t.Fatal(err)
	}
	registration := document.Registrations[api.SandboxKata]
	registration.Capabilities.Protocols = []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}
	document.Registrations[api.SandboxKata] = registration
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	platform.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := h.admin.Update(h.ctx, platform); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Update(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Fatal("served admission accepted a Pod with newly unregistered SCTP:", err)
	}
	if _, err := h.reconcile(); err == nil || !strings.Contains(err.Error(), "acknowledged controller shutdown") {
		t.Fatal("changed registration did not wait for the running workload to stop:", err)
	}
	h.assertNotReady(t)
	h.assertNoRoute(t)
	if ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 ||
		!slices.Contains(h.current(t).Finalizers, sandbox.HoldFinalizer) {
		t.Fatal("protocol registration drift did not hold the existing workload")
	}
}
