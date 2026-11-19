// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIngressMTLSPendingApprovalBootstrapsOnlyOwnedPrerequisites(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false,
		withIngressMTLS(t, "openclaw"),
		func(r *AgentReconciler, kube client.Client) {
			cm := &corev1.ConfigMap{}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, cm); err != nil {
				t.Fatal(err)
			}
			var config SandboxPlatformConfiguration
			if err := json.Unmarshal([]byte(cm.Data[SandboxPlatformConfigurationKey]), &config); err != nil {
				t.Fatal(err)
			}
			registration := config.Platforms[api.SandboxKata].IngressMTLS
			registration.Bindings = registration.Bindings[:0]
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			cm.Data[SandboxPlatformConfigurationKey] = string(raw)
			if err := kube.Update(ctx, cm); err != nil {
				t.Fatal(err)
			}
		})
	namespace := &corev1.Namespace{}
	var lastErr error
	for range 3 {
		_, lastErr = f.reconcile()
		if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(f.agent.Name)}, namespace); err == nil {
			break
		}
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(f.agent.Name)}, namespace); err != nil {
		t.Fatal("pending enrollment deadlocked owned namespace bootstrap:", err, lastErr)
	}
	if err := f.r.validateSandboxBootstrapNamespace(ctx, f.current(t), namespace); err != nil {
		t.Fatal("pending enrollment weakened namespace identity:", err)
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: instanceServiceName(f.agent.Name)}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("pending certificate enrollment created compute:", err)
	}
	f.assertNotReady(t)
}
