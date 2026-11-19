// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestContainerSandboxFirstWorkloadRejectionAPIServer(t *testing.T) {
	testContainerSandboxFirstWorkloadRejectionAPIServer(t, false)
}

func TestContainerSandboxCopilotFirstWorkloadRejectionAPIServer(t *testing.T) {
	testContainerSandboxFirstWorkloadRejectionAPIServer(t, true)
}

func testContainerSandboxFirstWorkloadRejectionAPIServer(t *testing.T, copilot bool) {
	t.Helper()
	h := newUnreconciledKubernetesAccessAPIHarnessWithRuntime(t, "openclaw", api.SandboxContainer)
	if copilot {
		policy := &api.AgentPolicy{}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: "policy-example"}, policy); err != nil {
			t.Fatal(err)
		}
		desired := policy.DeepCopy()
		desired.Spec.ModelAccess.Copilot.Allowed = true
		ticket, err := h.r.SandboxNativeCoordination.AcquireConfigurationChange(h.ctx, policy, desired)
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(h.admin, h.admin, policy, desired)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.r.SandboxNativeCoordination.ReconcileChange(h.ctx, ticket, adapter); err != nil {
			t.Fatal(err)
		}
		reconciler := &AgentPolicyReconciler{Client: h.kube, APIReader: h.kube,
			Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
		if _, err := reconciler.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
			t.Fatal(err)
		}
		agent := h.current(t)
		agent.Spec.ModelAccess = modelAccess(true, true)
		if err := h.admin.Update(h.ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	config := rest.CopyConfig(h.localHelm.config)
	config.Impersonate = rest.ImpersonationConfig{UserName: "unprivileged-workload-writer", Groups: []string{"system:authenticated"}}
	writer, err := client.NewWithWatch(config, client.Options{Scheme: h.admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	installer := h.r.HelmClient.(*sharedSandboxInstaller)
	original := installer.kube
	grants, auxiliaries, rejected := 0, 0, false
	installer.kube = interceptor.NewClient(h.kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			kind := object.GetObjectKind().GroupVersionKind().Kind
			if kind == "StatefulSet" {
				if copilot && auxiliaries == 0 {
					t.Fatal("served Copilot fixture did not create its auxiliary first")
				}
				err := writer.Create(ctx, object, opts...)
				rejected = apierrors.IsForbidden(err)
				return err
			}
			err := c.Create(ctx, object, opts...)
			if err == nil && (kind == "RoleBinding" || kind == "ClusterRoleBinding") {
				grants++
			}
			if err == nil && kind == "Deployment" {
				auxiliaries++
			}
			return err
		},
	})
	var failed error
	for range 5 {
		_, failed = h.reconcile()
		if failed != nil {
			break
		}
	}
	if !apierrors.IsForbidden(failed) || !rejected || grants < 2 {
		t.Fatal("API server did not reject the first StatefulSet after binding success:", failed)
	}
	expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
	state := &corev1.ConfigMap{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: h.r.OperatorNamespace,
		Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}, state); err != nil ||
		state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled {
		t.Fatal("served rejection did not record a fenced cancellation:", err)
	}
	if _, found, err := h.r.SandboxNativeCoordination.ReadNative(h.ctx, h.agent.UID); err != nil || found != copilot {
		t.Fatal("served rejection did not respect auxiliary shutdown acknowledgement:", err)
	}
	if copilot {
		deployments := &appsv1.DeploymentList{}
		if err := h.admin.List(h.ctx, deployments, client.InNamespace(expected.Namespace.Name)); err != nil {
			t.Fatal(err)
		}
		for index := range deployments.Items {
			deployment := &deployments.Items[index]
			if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
				t.Fatal("primary rejection did not stop the auxiliary")
			}
			deployment.Status.ObservedGeneration = deployment.Generation
			if err := h.admin.Status().Update(h.ctx, deployment); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: expected.Namespace.Name,
		Name: instanceServiceName(h.agent.Name)}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("rejected CREATE nevertheless produced a workload:", err)
	}
	installer.kube = original
	for range 5 {
		h.reconcileOK(t)
	}
	assertKubernetesFixtureGrants(t, h.nativeSandboxFixture, true)
	current := reconciledExpectationForTest(t, h.nativeSandboxFixture)
	if current.Agent != expected.Agent || current.Namespace != expected.Namespace || current.Home != expected.Home {
		t.Fatal("restored workload authority replaced the retained lifetime")
	}
}
