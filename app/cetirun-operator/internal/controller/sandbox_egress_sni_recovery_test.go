// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxObservedSNIFinalSourceGuard(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
				withReconciledCoordination(t), withObservedSNI(t, kind))
			key := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}
			original := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, key, original); err != nil {
				t.Fatal(err)
			}
			installer := f.r.HelmClient.(*sharedSandboxInstaller)
			called := false
			installer.beforeApply = func(prepared *helm.PreparedRelease) error {
				if err := prepared.CheckSandboxApply(ctx); err != nil {
					return err
				}
				current := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, key, current); err != nil {
					return err
				}
				current.Data[SandboxPlatformConfigurationKey] = "{}"
				called = true
				return f.kube.Update(ctx, current)
			}
			var failed error
			for range 5 {
				_, failed = f.reconcile()
				if failed != nil {
					break
				}
			}
			if !called || failed == nil || f.installer.stages != 0 {
				t.Fatal("changed capture authority crossed the final apply guard:", failed)
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: instanceServiceName(f.agent.Name)}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatal("source drift delivered a workload:", err)
			}
			current := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			current.Data = original.Data
			if err := f.kube.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			installer.beforeApply = nil
			for range 4 {
				f.reconcileOK(t)
			}
			if len(f.workload(t).Spec.Template.Spec.InitContainers) < 3 {
				t.Fatal("restored source did not recover actual capture workload delivery")
			}
		})
	}
}

func TestReconciledSandboxObservedSNIDeletion(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mtls := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "fqdn-only", true: "both"}[mtls], func(t *testing.T) {
				options := []func(*AgentReconciler, client.Client){}
				if mtls {
					options = append(options, withIngressMTLS(t, kind))
				}
				options = append(options, withObservedSNI(t, kind))
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true, options...)
				ctx := context.Background()
				agent := f.current(t)
				bindings, err := f.r.sandboxGrantJournal().Read(ctx, agent.Name, agent.UID)
				if err != nil || len(bindings) == 0 {
					t.Fatal("capture must retain its source-access grant inventory:", err)
				}
				f.r.SandboxPreparation = nil
				if err := f.kube.Delete(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				for _, binding := range bindings {
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Name}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
						t.Fatal("capture deletion retained a source grant:", err)
					}
				}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: sandboxpolicy.GrantJournalName(agent.UID)}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
					t.Fatal("capture deletion retained its journal:", err)
				}
				if err := f.kube.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatal("capture deletion did not finish:", err)
				}
			})
		}
	}
}
