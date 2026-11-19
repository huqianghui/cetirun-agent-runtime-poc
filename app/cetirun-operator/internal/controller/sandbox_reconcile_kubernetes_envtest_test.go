// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newKubernetesAccessAPIHarness(t *testing.T, kind string) *nativeStartAPIHarness {
	t.Helper()
	h := newUnreconciledKubernetesAccessAPIHarness(t, kind)
	for range 6 {
		h.reconcileOK(t)
	}
	return h
}

func newUnreconciledKubernetesAccessAPIHarness(t *testing.T, kind string) *nativeStartAPIHarness {
	t.Helper()
	return newUnreconciledKubernetesAccessAPIHarnessWithRuntime(t, kind, api.SandboxKata)
}

func newUnreconciledKubernetesAccessAPIHarnessWithRuntime(t *testing.T, kind string, backend api.SandboxType) *nativeStartAPIHarness {
	t.Helper()
	h := newNativeStartAPIHarnessWithRuntime(t, true, true, corev1.ReadWriteOnce, kind, backend, nil, api.AgentPolicyNetwork{})
	configureReconciledKubernetesAccess(t, h.r, h.admin, h.agent.Name, "policy-example")
	configuration := &corev1.ConfigMap{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: h.r.OperatorNamespace, Name: "sandbox-platform"}, configuration); err != nil {
		t.Fatal(err)
	}
	approved := configuration.Data[SandboxPlatformConfigurationKey]
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(approved), &document); err != nil {
		t.Fatal(err)
	}
	platform := document.Platforms[backend]
	platform.Identities = nil
	document.Platforms[backend] = platform
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := h.admin.Update(h.ctx, configuration); err != nil {
		t.Fatal(err)
	}
	_, _ = h.reconcile()
	if !slices.Contains(h.current(t).Finalizers, sandbox.HoldFinalizer) {
		t.Fatal("missing per-Agent identity approval did not hold startup")
	}
	configuration.Data[SandboxPlatformConfigurationKey] = approved
	if err := h.admin.Update(h.ctx, configuration); err != nil {
		t.Fatal(err)
	}
	h.kube = interceptor.NewClient(h.kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, object, opts...); err != nil {
				return err
			}
			if namespace, ok := object.(*corev1.Namespace); ok {
				return h.admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace.Name, Name: "default",
				}})
			}
			return nil
		},
	})
	h.r.Client = h.kube
	installKubernetesFixtureCA(t, h.r, h.admin, h.agent.Name)
	installer := &nativeSandboxInstaller{kube: h.kube, chart: h.localHelm.chart}
	h.installer = installer
	h.r.HelmClient = &sharedSandboxInstaller{nativeSandboxInstaller: installer}
	return h
}

func TestReconciledSandboxKubernetesAccessAPIServer(t *testing.T) {
	testReconciledSandboxKubernetesAccessAPIServer(t, api.SandboxKata)
}

func TestContainerSandboxKubernetesAccessAPIServer(t *testing.T) {
	testReconciledSandboxKubernetesAccessAPIServer(t, api.SandboxContainer)
}

func testReconciledSandboxKubernetesAccessAPIServer(t *testing.T, backend api.SandboxType) {
	t.Helper()
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			h := newUnreconciledKubernetesAccessAPIHarnessWithRuntime(t, kind, backend)
			for range 6 {
				h.reconcileOK(t)
			}
			before := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			bindings := assertKubernetesFixtureGrants(t, h.nativeSandboxFixture, true)
			identity := &corev1.ServiceAccount{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: before.Namespace.Name, Name: kubernetesFixtureAccount}, identity); err != nil ||
				ptr.Deref(identity.AutomountServiceAccountToken, true) || !sandboxBuiltinOwned(identity, h.current(t)) {
				t.Fatal("served identity lost its ownership or disabled automount:", err)
			}
			scoped := rest.CopyConfig(h.localHelm.config)
			scoped.Impersonate = rest.ImpersonationConfig{
				UserName: "system:serviceaccount:" + before.Namespace.Name + ":" + kubernetesFixtureAccount,
				Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + before.Namespace.Name, "system:authenticated"},
			}
			workloadClient, err := client.New(scoped, client.Options{Scheme: h.admin.Scheme()})
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct {
				resource, verb, namespace string
				allowed                   bool
			}{
				{"pods", "list", "operations", true},
				{"pods", "delete", "operations", false},
				{"pods", "get", before.Namespace.Name, false},
				{"secrets", "get", "operations", false},
				{"nodes", "get", "", true},
				{"nodes", "list", "", false},
			} {
				h.assertAccess(t, workloadClient, authorizationv1.ResourceAttributes{
					Resource: check.resource, Verb: check.verb, Namespace: check.namespace,
				}, check.allowed)
			}
			h.assertAccess(t, h.member, authorizationv1.ResourceAttributes{
				Group: api.GroupVersion.Group, Resource: "agentpolicies", Verb: "update",
			}, false)
			policy := &api.AgentPolicy{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Name: "policy-example"}, policy); err != nil {
				t.Fatal(err)
			}
			changed := policy.DeepCopy()
			changed.Spec.KubernetesAccess.RoleBindings[0].Namespace = "kube-system"
			if err := h.admin.Update(h.ctx, changed, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatal("served configuration admission allowed a different or unapproved active grant:", err)
			}
			unapproved := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "unapproved-kubernetes-access"},
				Spec:       *policy.Spec.DeepCopy(),
			}
			h.create(t, unapproved)
			unapprovedTeam := &api.Team{
				ObjectMeta: metav1.ObjectMeta{Name: "unapproved-kubernetes-team"},
				Spec: api.TeamSpec{AgentPolicyRef: unapproved.Name, SandboxRefs: []string{h.agent.Spec.SandboxRef},
					DefaultSandboxRef: h.agent.Spec.SandboxRef},
			}
			if err := h.admin.Create(h.ctx, unapprovedTeam); !apierrors.IsForbidden(err) ||
				!strings.Contains(err.Error(), "approval") {
				t.Fatal("served binding admission accepted grants approved for a different policy UID:", err)
			}

			observed := *h.nativeSandboxFixture
			observed.kube = h.admin
			pod := reconciledPodForTest(t, &observed, before.Workloads[0])
			pod.Spec.Overhead = nil
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); err != nil {
				t.Fatal("served admission rejected the exact approved Kubernetes Pod:", err)
			}
			bad := pod.DeepCopy()
			for index := range bad.Spec.Volumes {
				if bad.Spec.Volumes[index].Name == "sandbox-kubernetes-api" {
					bad.Spec.Volumes[index].Projected.Sources[0].ServiceAccountToken.Audience = "unapproved.example.com"
				}
			}
			if err := h.admin.Create(h.ctx, bad, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatal("served Pod admission accepted another token audience:", err)
			}
			grant := &rbacv1.RoleBinding{}
			ref := bindings[slices.IndexFunc(bindings, func(ref sandboxpolicy.BindingReference) bool { return ref.Kind == "RoleBinding" })]
			if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, grant); err != nil {
				t.Fatal(err)
			}
			originalSubjects := slices.Clone(grant.Subjects)
			grant.Subjects = append(grant.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "default", Namespace: "operations"})
			if err := h.admin.Update(h.ctx, grant); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatal("served Pod admission accepted a shared binding:", err)
			}
			grant.Subjects = originalSubjects
			if err := h.admin.Update(h.ctx, grant); err != nil {
				t.Fatal(err)
			}
			h.create(t, pod)
			h.observeReadyPod(t, pod)
			workload := h.workload(t)
			workload.Status.ObservedGeneration = workload.Generation
			workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
			if err := h.admin.Status().Update(h.ctx, workload); err != nil {
				t.Fatal("supply the absent StatefulSet controller's rollout observation:", err)
			}
			h.reconcileOK(t)
			h.reconcileOK(t)
			current := h.current(t)
			if current.Status.Phase != api.PhaseReady || current.Status.AccessURL == "" ||
				!apimeta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionSandboxReady) ||
				!apimeta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionPolicyApplied) {
				t.Fatal("current Kubernetes workload was not ready after acknowledged rollout")
			}
			if status := h.current(t).Status.Sandbox; status == nil || status.AppliedRevision != before.Revision {
				t.Fatal("ready status did not report the applied Kubernetes control revision")
			}

			// Role metadata changes also invalidate exact approval versions.
			role := &rbacv1.ClusterRole{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Name: ref.RoleRef.Name}, role); err != nil {
				t.Fatal(err)
			}
			role.Annotations = map[string]string{"example.com/review": "new-revision"}
			if err := h.admin.Update(h.ctx, role); err != nil {
				t.Fatal(err)
			}
			pod.UID, pod.ResourceVersion = "", ""
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) ||
				!strings.Contains(err.Error(), "SourceChanged") {
				t.Fatal("served Pod admission ignored approved ClusterRole version drift:", err)
			}
			_, _ = h.reconcile()
			h.assertNotReady(t)
			h.assertNoRoute(t)
			if !slices.Contains(h.current(t).Finalizers, sandbox.HoldFinalizer) || ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("role drift did not persist a hold and stop the current workload")
			}
			for _, check := range []authorizationv1.ResourceAttributes{
				{Resource: "pods", Verb: "list", Namespace: "operations"},
				{Resource: "nodes", Verb: "get"},
			} {
				h.assertAccess(t, workloadClient, check, false)
			}
			cleanupKubernetesAccessAPIHarness(t, h, bindings)
		})
	}
}

func cleanupKubernetesAccessAPIHarness(t *testing.T, h *nativeStartAPIHarness, refs []sandboxpolicy.BindingReference) {
	t.Helper()
	agent := h.current(t)
	if err := h.member.Delete(h.ctx, agent); err != nil {
		t.Fatal(err)
	}
	pod := h.pod(t)
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := h.admin.Status().Update(h.ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Delete(h.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	workload := h.workload(t)
	workload.Status.ObservedGeneration = workload.Generation
	workload.Status.Replicas, workload.Status.CurrentReplicas, workload.Status.ReadyReplicas = 0, 0, 0
	workload.Status.UpdatedReplicas = 0
	if err := h.admin.Status().Update(h.ctx, workload); err != nil {
		t.Fatal(err)
	}
	// Simulate restart with new starts disabled, not another execution path.
	h.r.SandboxPreparation = nil
	var cleanupErr error
	for range 3 {
		_, cleanupErr = h.reconcile()
	}
	if cleanupErr != nil {
		t.Fatal("served cleanup did not reach ordinary namespace deletion:", cleanupErr)
	}
	for _, ref := range refs {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(rbacv1.SchemeGroupVersion.String())
		object.SetKind(ref.Kind)
		if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); !apierrors.IsNotFound(err) {
			t.Fatal("served cleanup retained an external Kubernetes binding:", err)
		}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: ref.RoleRef.Name}, &rbacv1.ClusterRole{}); err != nil {
			t.Fatal("served cleanup changed a shared ClusterRole:", err)
		}
	}
	namespace := &corev1.Namespace{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: workload.Namespace}, namespace); err != nil ||
		namespace.DeletionTimestamp.IsZero() || len(namespace.Finalizers) != 0 {
		t.Fatal("controller did not release its namespace ownership before deletion:", err)
	}
	// Envtest has no namespace/volume garbage collector. CetiRun must release
	// its own protections before these ordinary Kubernetes deletions proceed.
	for _, list := range []client.ObjectList{
		&appsv1.StatefulSetList{}, &appsv1.ControllerRevisionList{}, &corev1.ServiceList{},
		&corev1.SecretList{}, &corev1.ConfigMapList{}, &corev1.PersistentVolumeClaimList{},
		&corev1.ServiceAccountList{}, &rbacv1.RoleList{}, &rbacv1.RoleBindingList{},
		&networkingv1.NetworkPolicyList{},
		&unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicyList"}},
	} {
		if err := h.admin.List(h.ctx, list, client.InNamespace(namespace.Name)); err != nil {
			t.Fatal(err)
		}
		if err := apimeta.EachListItem(list, func(item runtime.Object) error {
			object := item.(client.Object)
			for _, finalizer := range object.GetFinalizers() {
				if finalizer != "kubernetes.io/pvc-protection" {
					t.Fatalf("CetiRun left a protected dependent: %s %s", object.GetName(), finalizer)
				}
			}
			if len(object.GetFinalizers()) != 0 {
				before := object.DeepCopyObject().(client.Object)
				object.SetFinalizers(nil)
				if err := h.admin.Patch(h.ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return err
				}
			}
			return client.IgnoreNotFound(h.admin.Delete(h.ctx, object, client.Preconditions{
				UID: ptr.To(object.GetUID()), ResourceVersion: ptr.To(object.GetResourceVersion()),
			}))
		}); err != nil {
			t.Fatal(err)
		}
	}
	namespace.Spec.Finalizers = nil
	if err := h.admin.SubResource("finalize").Update(h.ctx, namespace); err != nil {
		t.Fatal(err)
	}
	h.reconcileOK(t)
	journal := &corev1.ConfigMap{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{
		Namespace: h.r.OperatorNamespace, Name: sandboxpolicy.GrantJournalName(h.agent.UID),
	}, journal); !apierrors.IsNotFound(err) {
		t.Fatal("served cleanup retained the protected grant journal:", err, "reconcile:", cleanupErr)
	}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("served deletion retained compute after acknowledged shutdown:", err)
	}
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatal("served deletion retained the Agent after dependent cleanup:", err)
	}
}
