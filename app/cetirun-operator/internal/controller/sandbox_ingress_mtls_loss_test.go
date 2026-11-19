// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	istio "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIngressMTLSSourceLossWithdrawsAccess(t *testing.T) {
	for _, scenario := range []string{"server deletion", "client deletion", "server replacement", "Gateway deletion"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, false, withIngressMTLS(t, "openclaw"))
			f.reconcileOK(t)
			agent := f.current(t)
			if _, err := f.r.ensureVirtualService(ctx, agent); err != nil {
				t.Fatal(err)
			}
			var source client.Object = &corev1.Secret{}
			key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: "agent-ingress-tls"}
			if scenario == "client deletion" {
				key = client.ObjectKey{Namespace: "platform", Name: "gateway-agent-example"}
			} else if scenario == "Gateway deletion" {
				key, source = client.ObjectKey{Namespace: "platform", Name: "ingress"}, &istio.Gateway{}
			}
			if err := f.kube.Get(ctx, key, source); err != nil {
				t.Fatal(err)
			}
			requests := f.r.enqueueIngressMTLSSource(ctx, source)
			if len(requests) != 1 || requests[0].Name != agent.Name {
				t.Fatal("TLS source event did not target its approved Agent")
			}
			unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: "unrelated"}}
			if len(f.r.enqueueIngressMTLSSource(ctx, unrelated)) != 0 {
				t.Fatal("TLS source event selected an unrelated Secret")
			}
			if err := f.kube.Delete(ctx, source); err != nil {
				t.Fatal(err)
			}
			if scenario == "server replacement" {
				replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
				if err := f.kube.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				if replacement.UID == source.GetUID() {
					t.Fatal("replacement fixture did not change identity")
				}
			}
			if _, err := f.reconcile(); err == nil {
				t.Fatal("lost TLS identity was adopted or ignored")
			}
			f.assertNotReady(t)
			if ptr.Deref(f.workload(t).Spec.Replicas, 1) != 0 {
				t.Fatal("lost TLS identity retained running compute")
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: virtualServiceName(agent.Name)}, &istio.VirtualService{}); !apierrors.IsNotFound(err) {
				t.Fatal("lost TLS identity retained access:", err)
			}
		})
	}
}
