// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func reconciledNetworkPolicyForTest(t *testing.T, f *nativeSandboxFixture) sandboxpolicy.CiliumNetworkPolicy {
	t.Helper()
	expected := reconciledExpectationForTest(t, f)
	for _, control := range expected.Controls {
		if control.GetAPIVersion() != "cilium.io/v2" || control.GetKind() != "CiliumNetworkPolicy" {
			continue
		}
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(control.GroupVersionKind())
		if err := f.kube.Get(context.Background(), client.ObjectKeyFromObject(&control), object); err != nil {
			t.Fatal(err)
		}
		var policy sandboxpolicy.CiliumNetworkPolicy
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &policy); err != nil {
			t.Fatal(err)
		}
		if policy.UID == "" || !sandboxBuiltinOwned(object, f.current(t)) ||
			!reflect.DeepEqual(object.Object["spec"], control.Object["spec"]) ||
			policy.Namespace != expected.Namespace.Name ||
			policy.Spec.EndpointSelector.MatchLabels["k8s:"+sandboxpolicy.AgentUIDLabel] != string(f.agent.UID) ||
			policy.Spec.EndpointSelector.MatchLabels["k8s:io.kubernetes.pod.namespace"] != expected.Namespace.Name {
			t.Fatal("common network policy lost its expected controls, ownership or Agent scope")
		}
		return policy
	}
	t.Fatal("common workload did not pin its Cilium policy")
	return sandboxpolicy.CiliumNetworkPolicy{}
}

func assertReconciledInternalGrant(t *testing.T, policy sandboxpolicy.CiliumNetworkPolicy, cidr string, present bool) {
	t.Helper()
	found := false
	for _, rule := range policy.Spec.Egress {
		if !slices.ContainsFunc(rule.ToCIDRSet, func(item sandboxpolicy.CIDRRule) bool {
			return item.CIDR == cidr && len(item.Except) == 0
		}) {
			continue
		}
		for _, group := range rule.ToPorts {
			found = found || slices.ContainsFunc(group.Ports, func(port sandboxpolicy.PortRange) bool {
				return port.Port == "443" && port.Protocol == corev1.ProtocolTCP && port.EndPort == 0
			})
		}
	}
	if found != present {
		t.Fatalf("internal TCP/443 grant present=%t, want %t", found, present)
	}
}

func TestReconciledSandboxInternalNetwork(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(kind+"/"+string(mode), func(t *testing.T) {
				ctx := context.Background()
				policyName := testfixture.New(kind).Policy.Name
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
					withNativeHomeAccessMode(t, mode),
					func(_ *AgentReconciler, kube client.Client) {
						policy := &api.AgentPolicy{}
						if err := kube.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
							t.Fatal(err)
						}
						policy.Spec.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
							{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}},
						}
						if err := kube.Update(ctx, policy); err != nil {
							t.Fatal(err)
						}
					})
				f.reconcileOK(t)
				before := reconciledExpectationForTest(t, f)
				network := reconciledNetworkPolicyForTest(t, f)
				assertReconciledInternalGrant(t, network, "10.20.0.0/16", true)
				assertNativeHomeAccessMode(t, f, mode)

				policy := &api.AgentPolicy{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Spec.Network.AllowedInternalCIDRs = nil
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
				revoked := reconciledNetworkPolicyForTest(t, f)
				assertReconciledInternalGrant(t, revoked, "10.20.0.0/16", false)
				if after.Revision == before.Revision || after.PlanHash == before.PlanHash ||
					after.Home != before.Home || after.Agent != before.Agent || after.Namespace != before.Namespace ||
					revoked.UID != network.UID {
					t.Fatal("grant revocation retained stale controls or replaced the owned lifetime")
				}
			})
		}
	}
}
