// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestConnectionsMCPCAOutageDoesNotBlockSuspension(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		for _, failure := range []string{"missing", "invalid", "unreadable"} {
			t.Run(string(kind)+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				r, agent, _ := legacyGatewayFixture(t)
				agent.Spec.Agent.Type = kind
				if err := r.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				r.ConnectionsMCPURL = "https://tools.tools-example.svc.cluster.local/mcp"
				r.ConnectionsMCPCASecretName = connectionsMCPTestSecretName
				r.ConnectionsMCPAudience = "cetirun-connections-mcp"
				ca := connectionsMCPTestSource(connectionsMCPTestCA(t, 1))
				if err := r.Create(ctx, ca); err != nil {
					t.Fatal(err)
				}
				chart := &chartHelm{}
				r.HelmClient = chart
				sts := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name), Generation: 1},
					Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
					Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, ObservedGeneration: 1},
				}
				if err := r.Create(ctx, sts); err != nil {
					t.Fatal(err)
				}
				req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
				current := func() *api.Agent {
					t.Helper()
					value := &api.Agent{}
					if err := r.Get(ctx, req.NamespacedName, value); err != nil {
						t.Fatal(err)
					}
					return value
				}
				for range 4 {
					if _, err := r.Reconcile(ctx, req); err != nil {
						t.Fatal(err)
					}
					if current().Status.Phase == api.PhaseReady {
						break
					}
				}
				if current().Status.Phase != api.PhaseReady || chart.installs != 1 {
					t.Fatal("fixture did not reach Ready with MCP enabled")
				}
				switch failure {
				case "missing":
					if err := r.Delete(ctx, ca); err != nil {
						t.Fatal(err)
					}
				case "invalid":
					ca.Data["ca.crt"] = []byte("invalid certificate")
					if err := r.Update(ctx, ca); err != nil {
						t.Fatal(err)
					}
				}
				caReads := 0
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, kube client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, secret := obj.(*corev1.Secret); secret && key.Name == connectionsMCPTestSecretName {
							caReads++
							if failure == "unreadable" {
								return apierrors.NewServiceUnavailable("synthetic CA store outage")
							}
						}
						return kube.Get(ctx, key, obj, opts...)
					},
				})
				agent = current()
				agent.Spec.Suspended = true
				agent.Generation++
				if err := r.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
				if current().Status.Phase != api.PhaseSuspended || chart.installs != 1 {
					t.Fatalf("CA outage prevented suspension: phase=%s installs=%d", current().Status.Phase, chart.installs)
				}
				stopped := &appsv1.StatefulSet{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(sts), stopped); err != nil {
					t.Fatal(err)
				}
				if stopped.Spec.Replicas == nil || *stopped.Spec.Replicas != 0 {
					t.Fatalf("suspension did not stop the Gateway workload: replicas=%v", stopped.Spec.Replicas)
				}
				if caReads != 0 {
					t.Fatalf("suspension read MCP CA material %d times", caReads)
				}
				routeKey := client.ObjectKey{Name: virtualServiceName(agent.Name), Namespace: instanceNamespace(agent.Name)}
				if err := r.Get(ctx, routeKey, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
					t.Fatalf("suspension retained ingress: %v", err)
				}
				// The suspended steady state must also remain independent of CA reads.
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
				if caReads != 0 || chart.installs != 1 || current().Status.Phase != api.PhaseSuspended {
					t.Fatal("suspended reconcile depended on CA trust or reinstalled the chart")
				}
				// Resuming still requires valid trust before any workload is started.
				agent = current()
				agent.Spec.Suspended = false
				agent.Generation++
				if err := r.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				_, _ = r.Reconcile(ctx, req)
				if current().Status.Phase != api.PhaseFailed || chart.installs != 1 || caReads == 0 {
					t.Fatal("resume bypassed the MCP CA requirement")
				}
				if err := r.Get(ctx, routeKey, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
					t.Fatalf("failed resume exposed ingress: %v", err)
				}
			})
		}
	}
}
