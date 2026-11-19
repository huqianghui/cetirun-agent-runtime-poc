// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestContainerSandboxCommonAPIServer(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
			t.Run(kind+"/"+string(mode), func(t *testing.T) {
				network := api.AgentPolicyNetwork{AllowedInternalCIDRs: []api.AgentPolicyCIDRRule{
					{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}},
				}, BlockedPorts: []api.AgentPolicyPort{{Port: 25}}}
				h := startReconciledAPIHarness(t, newNativeStartAPIHarnessWithRuntime(t, true, true, mode, kind, api.SandboxContainer, nil, network))
				assertContainerWorkload(t, h.nativeSandboxFixture, mode)
				initial := reconciledExpectationForTest(t, h.nativeSandboxFixture)
				assertReconciledInternalGrant(t, reconciledNetworkPolicyForTest(t, h.nativeSandboxFixture), "10.20.0.0/16", true)
				box := &api.Sandbox{}
				if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.current(t).Spec.SandboxRef}, box); err != nil {
					t.Fatal(err)
				}
				changedRuntime := box.DeepCopy()
				changedRuntime.Spec.Type = api.SandboxKata
				if err := h.admin.Update(h.ctx, changedRuntime); !apierrors.IsForbidden(err) {
					t.Fatal("active container execution allowed an in-place runtime conversion:", err)
				}
				assertContainerUnsupportedOrUnregisteredPolicies(t, h, kind)

				observed := *h.nativeSandboxFixture
				observed.kube = h.admin
				pod := reconciledPodForTest(t, &observed, initial.Workloads[0])
				for _, runtimeClass := range []*string{nil, ptr.To("unregistered-runtime")} {
					changed := pod.DeepCopy()
					changed.Spec.RuntimeClassName = runtimeClass
					if err := h.admin.Create(h.ctx, changed, client.DryRunAll); err == nil {
						t.Fatal("served Pod admission accepted a default or unregistered runtime")
					}
				}
				h.create(t, pod)
				if len(pod.Spec.Overhead) != 0 || ptr.Deref(pod.Spec.RuntimeClassName, "") != "example-container" {
					t.Fatal("served RuntimeClass admission invented Kata overhead or used a default runtime")
				}
				h.observeReadyPod(t, pod)
				updateContainerWorkloadStatus(t, h, 1)
				h.reconcileOK(t)
				token := aigateway.Token(h.gateway.generated[0].Key)
				keyID := h.current(t).Status.AIGateway.KeyID
				h.gateway.keys[token].Spend = ptr.To(3.25)
				budget := ptr.Deref(h.gateway.keys[token].MaxBudget, 0)
				if budget <= 0 || h.gateway.keys[token].BudgetDuration != "1mo" {
					t.Fatal("container start did not install the governed monthly Gateway budget")
				}

				agent := h.current(t)
				before := agent.DeepCopy()
				agent.Spec.RestartRequestID = "container-restart-0001"
				if err := h.member.Patch(h.ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					t.Fatal("served retained restart:", err)
				}
				stages := h.installer.stages
				if _, err := h.reconcile(); err == nil || !strings.Contains(err.Error(), "acknowledged controller shutdown") {
					t.Fatal("coordinated restart did not await controller shutdown:", err)
				}
				if ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 || h.installer.stages != stages {
					t.Fatal("coordinated restart replaced compute before ordinary shutdown")
				}
				if h.current(t).Status.ObservedRestartRequestID == agent.Spec.RestartRequestID {
					t.Fatal("old Pod completed a new retained restart")
				}
				if err := h.admin.Delete(h.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
					t.Fatal(err)
				}
				updateContainerWorkloadStatus(t, h, 0)
				for range 4 {
					h.reconcileOK(t)
				}
				next := reconciledExpectationForTest(t, h.nativeSandboxFixture)
				pod = reconciledPodForTest(t, &observed, next.Workloads[0])
				h.create(t, pod)
				binding := &corev1.Binding{
					ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
					Target:     corev1.ObjectReference{Kind: "Node", Name: "synthetic-" + h.agent.Name + "-node"},
				}
				if err := h.admin.SubResource("binding").Create(h.ctx, pod, binding); err != nil {
					t.Fatal(err)
				}
				pod = h.pod(t)
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				if err := h.admin.Status().Update(h.ctx, pod); err != nil {
					t.Fatal(err)
				}
				updateContainerWorkloadStatus(t, h, 1)
				h.reconcileOK(t)
				if h.current(t).Status.ObservedRestartRequestID != agent.Spec.RestartRequestID {
					t.Fatal("current Ready Pod did not complete the retained container restart")
				}
				stages = h.installer.stages
				h.reconcileOK(t)
				if h.installer.stages != stages {
					t.Fatal("unchanged container restart repeated the workload apply")
				}

				agent = h.current(t)
				before = agent.DeepCopy()
				agent.Spec.Suspended = true
				if err := h.member.Patch(h.ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					t.Fatal("served suspend:", err)
				}
				_, _ = h.reconcile()
				if ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 || !h.current(t).Status.AIGateway.Blocked {
					t.Fatal("suspension did not stop compute and access before ordinary Pod termination")
				}
				if err := h.admin.Delete(h.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
					t.Fatal(err)
				}
				updateContainerWorkloadStatus(t, h, 0)
				for range 3 {
					h.reconcileOK(t)
				}
				if h.current(t).Status.Phase != api.PhaseSuspended {
					t.Fatal("container suspension was not acknowledged after ordinary termination")
				}
				agent = h.current(t)
				before = agent.DeepCopy()
				agent.Spec.Suspended = false
				if err := h.member.Patch(h.ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					t.Fatal("served resume:", err)
				}
				for range 3 {
					h.reconcileOK(t)
				}
				current := reconciledExpectationForTest(t, h.nativeSandboxFixture)
				key := h.gateway.keys[token]
				if current.Home != initial.Home || current.Agent != initial.Agent || current.Namespace != initial.Namespace ||
					h.current(t).Status.AIGateway.KeyID != keyID || len(h.gateway.generated) != 1 ||
					key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 ||
					ptr.Deref(key.MaxBudget, 0) != budget || key.BudgetDuration != "1mo" {
					t.Fatal("retained container lifecycle changed HOME, key, spend or ownership")
				}
				assertContainerWorkload(t, h.nativeSandboxFixture, mode)

				home := &corev1.PersistentVolumeClaim{}
				if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: current.Namespace.Name, Name: current.Home.Name}, home); err != nil {
					t.Fatal(err)
				}
				home.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
				if err := h.admin.Update(h.ctx, home); !apierrors.IsInvalid(err) {
					t.Fatal("served HOME claim permitted an access-mode conversion:", err)
				}
				pod = reconciledPodForTest(t, &observed, current.Workloads[0])
				updateContainerPlatformForTest(t, h.r, h.admin, func(document *SandboxPlatformConfiguration) {
					pin := document.RuntimeClasses["example-container"]
					pin.Handler = "unverified-handler"
					document.RuntimeClasses["example-container"] = pin
				})
				if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
					t.Fatal("served Pod admission ignored a changed handler approval:", err)
				}
				_, _ = h.reconcile()
				if ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 || !h.current(t).Status.AIGateway.Blocked {
					t.Fatal("changed runtime approval did not hold the ordinary-container executor")
				}
			})
		}
	}
}

func updateContainerWorkloadStatus(t *testing.T, h *nativeStartAPIHarness, replicas int32) {
	t.Helper()
	workload := h.workload(t)
	workload.Status = appsv1.StatefulSetStatus{
		ObservedGeneration: workload.Generation, Replicas: replicas,
		ReadyReplicas: replicas, UpdatedReplicas: replicas, UpdateRevision: workload.Status.UpdateRevision,
	}
	if err := h.admin.Status().Update(h.ctx, workload); err != nil {
		t.Fatal(err)
	}
}

func TestContainerSandboxIngressMTLSAPIServer(t *testing.T) {
	testReconciledSandboxIngressMTLSAPIServer(t, api.SandboxContainer)
}

func assertContainerUnsupportedOrUnregisteredPolicies(t *testing.T, h *nativeStartAPIHarness, kind string) {
	t.Helper()
	for name, change := range map[string]func(*api.AgentPolicySpec){
		"fqdn":                      func(p *api.AgentPolicySpec) { p.Network.BlockedFQDNs = []string{"blocked.example.com"} },
		"mtls-without-registration": func(p *api.AgentPolicySpec) { p.Network.Ingress.RequireMTLS = true },
		"entra":                     func(p *api.AgentPolicySpec) { p.EntraID.Enabled = true },
		"kubernetes": func(p *api.AgentPolicySpec) {
			p.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "operations",
				RoleBindings:       []api.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "reader"}},
			}
		},
	} {
		policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "unsupported-" + name},
			Spec: api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentType(kind)}}}}
		change(&policy.Spec)
		h.create(t, policy)
		team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: policy.Name}, Spec: api.TeamSpec{
			AgentPolicyRef: policy.Name, SandboxRefs: []string{h.current(t).Spec.SandboxRef},
			DefaultSandboxRef: h.current(t).Spec.SandboxRef,
		}}
		if err := h.admin.Create(h.ctx, team); !apierrors.IsForbidden(err) {
			t.Fatalf("served container binding accepted unsupported %s: %v", name, err)
		}
	}
}
