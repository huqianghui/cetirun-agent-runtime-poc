// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIngressMTLSApprovalDoesNotAuthorizeApplicationKeyAccess(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false,
		withIngressMTLS(t, "hermes"),
		func(_ *AgentReconciler, kube client.Client) {
			agent := &api.Agent{}
			if err := kube.Get(ctx, client.ObjectKey{Name: "example"}, agent); err != nil {
				t.Fatal(err)
			}
			agent.Spec.Helm.Values["extraEnv"] = runtime.RawExtension{Raw: []byte(
				`[{"name":"UNAPPROVED_KEY","valueFrom":{"secretKeyRef":{"name":"agent-ingress-tls","key":"tls.key"}}}]`,
			)}
			if err := kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
		})
	for range 3 {
		_, _ = f.reconcile()
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: instanceServiceName(f.agent.Name)}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("endpoint approval exposed the TLS private key to the application:", err)
	}
	f.assertNotReady(t)
}
