// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeSandboxNetworkControlsAPIServer(t *testing.T) {
	network := api.AgentPolicyNetwork{
		BlockedCIDRs: []string{"198.51.100.0/25"},
		BlockedPorts: []api.AgentPolicyPort{
			{Port: 18080, Protocol: corev1.ProtocolTCP},
			{Port: 18081, Protocol: corev1.ProtocolUDP},
		},
	}
	h := newNativeStartAPIHarness(t, true, true, network)
	h.seedRetainedBootstrap(t)
	h.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: instanceNamespace(h.agent.Name), Name: "default",
	}})
	h.prepare(t)
	_, expected := h.expectation(t)
	if !expected.ExecutionHeld || ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("network policy staging started a workload")
	}
	var source w.SourceReference
	for _, candidate := range expected.Sources {
		if candidate.Kind == "CiliumNetworkPolicy" && candidate.APIVersion == "cilium.io/v2" {
			if source.Name != "" {
				t.Fatal("native workload has more than one Cilium policy")
			}
			source = candidate
		}
	}
	if source.Name == "" {
		t.Fatal("native preparation did not pin a Cilium policy")
	}
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(source.APIVersion)
	object.SetKind(source.Kind)
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
		t.Fatal(err)
	}
	var policy sandboxpolicy.CiliumNetworkPolicy
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &policy); err != nil {
		t.Fatal(err)
	}
	if object.GetUID() != source.UID || !sandboxBuiltinOwned(object, h.current(t)) ||
		policy.Namespace != expected.Namespace.Name ||
		policy.Spec.EndpointSelector.MatchLabels["k8s:"+sandboxpolicy.AgentUIDLabel] != string(h.agent.UID) {
		t.Fatal("staged Cilium policy lost its Agent-scoped ownership or selector")
	}
	cidr, tcp, udp := false, false, false
	for _, rule := range policy.Spec.EgressDeny {
		cidr = cidr || slices.ContainsFunc(rule.ToCIDRSet, func(item sandboxpolicy.CIDRRule) bool {
			return item.CIDR == network.BlockedCIDRs[0] && len(item.Except) == 0
		})
		if !slices.Contains(rule.ToEntities, "all") {
			continue
		}
		for _, group := range rule.ToPorts {
			for _, port := range group.Ports {
				tcp = tcp || (port.Port == "18080" && port.Protocol == corev1.ProtocolTCP && port.EndPort == 0)
				udp = udp || (port.Port == "18081" && port.Protocol == corev1.ProtocolUDP && port.EndPort == 0)
			}
		}
	}
	if !cidr || !tcp || !udp {
		t.Fatalf("served policy omitted requested denies before startup: CIDR=%t TCP=%t UDP=%t", cidr, tcp, udp)
	}
	for _, tc := range []struct {
		name, message string
		mutate        func(*api.AgentPolicyNetwork)
	}{
		{"dns-cidr", "required", func(n *api.AgentPolicyNetwork) { n.BlockedCIDRs = []string{"10.96.0.0/24"} }},
		{"gateway-cidr", "required", func(n *api.AgentPolicyNetwork) { n.BlockedCIDRs = []string{"10.96.1.0/24"} }},
		{"dns-port", "required", func(n *api.AgentPolicyNetwork) {
			n.BlockedPorts = []api.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolUDP}}
		}},
		{"sctp", "protocol", func(n *api.AgentPolicyNetwork) {
			n.BlockedPorts = []api.AgentPolicyPort{{Port: 18080, Protocol: corev1.ProtocolSCTP}}
		}},
		{"fqdn", "blockedFQDNs", func(n *api.AgentPolicyNetwork) { n.BlockedFQDNs = []string{"blocked.example.com"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "denied-" + tc.name},
				Spec: api.AgentPolicySpec{
					Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
					Network:   *network.DeepCopy(),
				},
			}
			tc.mutate(&candidate.Spec.Network)
			h.create(t, candidate)
			team := &api.Team{
				ObjectMeta: metav1.ObjectMeta{Name: "denied-" + tc.name},
				Spec: api.TeamSpec{
					AgentPolicyRef: candidate.Name, SandboxRefs: []string{h.agent.Spec.SandboxRef},
					DefaultSandboxRef: h.agent.Spec.SandboxRef,
				},
			}
			err := h.admin.Create(h.ctx, team)
			if err == nil || !strings.Contains(err.Error(), "denied the request") || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("invalid native policy/Sandbox binding did not receive a semantic admission denial: %v", err)
			}
		})
	}
	h.reconcileOK(t)
	state, started := h.expectation(t)
	h.assertHoldAdmission(t, started.Revision, true)
	receipt, _, err := h.r.readSandboxPreparationReceipt(h.ctx, h.current(t))
	if err != nil {
		t.Fatal(err)
	}
	pod := testfixture.Pod(*started)
	pod.Spec.Overhead = nil
	h.create(t, pod)
	h.observeReadyPod(t, pod)
	h.reconcileOK(t)
	h.assertReady(t)

	// This exercises served API enforcement and source-drift handling, not a
	// live Cilium dataplane. An actual network witness is a separate live gate.
	if err := unstructured.SetNestedSlice(object.Object, []any{}, "spec", "egressDeny"); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Update(h.ctx, object); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcile(); err == nil {
		t.Fatal("running native Agent accepted removed network restrictions")
	}
	h.assertNotReady(t)
	h.assertNoRoute(t)
	if !slices.Contains(h.current(t).Finalizers, sandbox.HoldFinalizer) ||
		ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("network policy drift did not hold the existing workload")
	}
	h.cleanupAgent(t, pod, state, receipt)
}

func TestReconciledSandboxInternalNetworkAPIServer(t *testing.T) {
	network := api.AgentPolicyNetwork{AllowedInternalCIDRs: []api.AgentPolicyCIDRRule{
		{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}},
	}}
	h := newReconciledStartAPIHarness(t, corev1.ReadWriteOnce, network)
	expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
	compiled := reconciledNetworkPolicyForTest(t, h.nativeSandboxFixture)
	assertReconciledInternalGrant(t, compiled, network.AllowedInternalCIDRs[0].CIDR, true)
	for _, tc := range []struct {
		name, cidr, message string
		protocol            corev1.Protocol
	}{
		{"node", "192.168.0.0/16", "internal exceptions", corev1.ProtocolTCP},
		{"management", "10.1.0.0/16", "internal exceptions", corev1.ProtocolTCP},
		{"metadata", "169.254.169.254/32", "internal exceptions", corev1.ProtocolTCP},
		{"public", "203.0.113.0/24", "internal exceptions", corev1.ProtocolTCP},
		{"sctp", "10.20.0.0/16", "protocol", corev1.ProtocolSCTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "internal-denied-" + tc.name},
				Spec: api.AgentPolicySpec{
					Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
					Network: api.AgentPolicyNetwork{AllowedInternalCIDRs: []api.AgentPolicyCIDRRule{
						{CIDR: tc.cidr, Ports: []api.AgentPolicyPort{{Port: 443, Protocol: tc.protocol}}},
					}},
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
				t.Fatal("served admission did not reject the conflicting internal grant:", err)
			}
		})
	}
	team := &api.Team{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	policy := &api.AgentPolicy{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Network.AllowedInternalCIDRs) != 1 ||
		policy.Spec.Network.AllowedInternalCIDRs[0].Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatal("served API lost the grant or its default TCP protocol")
	}
	policy.Spec.Network.AllowedInternalCIDRs = nil
	if err := h.admin.Update(h.ctx, policy); !apierrors.IsForbidden(err) {
		t.Fatal("active configuration reservation did not protect internal grants:", err)
	}
	observed := *h.nativeSandboxFixture
	observed.kube = h.admin
	pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
	pod.Spec.Overhead = nil
	if err := h.admin.Create(h.ctx, pod, client.DryRunAll); err != nil {
		t.Fatal("served Pod admission rejected the current internal-network policy:", err)
	}
	object := &unstructured.Unstructured{}
	object.SetAPIVersion("cilium.io/v2")
	object.SetKind("CiliumNetworkPolicy")
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: compiled.Namespace, Name: compiled.Name}, object); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(object.Object, []any{}, "spec", "egressDeny"); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Update(h.ctx, object); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Fatal("served Pod admission accepted drifted internal-network controls:", err)
	}
}
