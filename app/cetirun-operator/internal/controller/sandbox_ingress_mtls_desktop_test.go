// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"slices"
	"testing"

	admissionhandler "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	istio "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func withIngressMTLSDesktop(t *testing.T, kind string) func(*AgentReconciler, client.Client) {
	return withIngressMTLS(t, kind, true)
}

func TestReconciledSandboxIngressMTLSDefaultDesktop(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
			t.Run(kind+"/"+string(mode), func(t *testing.T) {
				ctx := context.Background()
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
					withNativeHomeAccessMode(t, mode), withIngressMTLSDesktop(t, kind))
				f.reconcileOK(t)
				workload := f.workload(t)
				desktop := slices.IndexFunc(workload.Spec.Template.Spec.Containers, func(c corev1.Container) bool { return c.Name == "desktop" })
				if desktop < 0 {
					t.Fatal("mTLS disabled the existing desktop default")
				}
				container := workload.Spec.Template.Spec.Containers[desktop]
				if !slices.Equal(container.Command, []string{"/bin/bash", "/etc/cetirun/desktop-ingress-entrypoint.sh"}) ||
					container.ReadinessProbe == nil || container.ReadinessProbe.Exec == nil ||
					!slices.Equal(container.ReadinessProbe.Exec.Command, []string{"python3", "/etc/cetirun/desktop-ingress-probe.py"}) {
					t.Fatal("desktop did not receive managed startup and local readiness")
				}
				accounting := w.AccountResources(workload.Spec.Template.Spec, nil)
				if !equality.Semantic.DeepEqual(workload.Spec.Template.Spec.Resources, &accounting.Pod) {
					t.Fatal("desktop/Envoy resources escaped the Pod accounting")
				}
				service := &corev1.Service{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name}, service); err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(service.Spec.Ports, func(p corev1.ServicePort) bool {
					return p.Name == "novnc" && p.Port == 6080 && p.TargetPort.IntVal == ingressmtls.DesktopPort
				}) {
					t.Fatal("noVNC service port did not traverse the TLS endpoint")
				}
				if _, err := f.r.ensureVirtualService(ctx, f.current(t)); err != nil {
					t.Fatal(err)
				}
				route := &istio.VirtualService{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: virtualServiceName(f.agent.Name)}, route); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, http := range route.Spec.Http {
					for _, destination := range http.Route {
						found = found || destination.Destination.Port.Number == 6080
					}
				}
				if !found {
					t.Fatal("default desktop route was discarded")
				}
				expected := reconciledExpectationForTest(t, f)
				pod := reconciledPodForTest(t, f, expected.Workloads[0])
				handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
				if err != nil {
					t.Fatal(err)
				}
				if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); !response.Allowed {
					t.Fatal("approved default desktop Pod rejected:", response.Result)
				}
				pod.Spec.Containers[desktop].Command = nil
				if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); response.Allowed {
					t.Fatal("Pod admission allowed loss of desktop loopback authority")
				}
			})
		}
	}
}
