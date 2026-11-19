// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxObservedSNIAPIServer(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mtls := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "fqdn-only", true: "both"}[mtls], func(t *testing.T) {
				h := newNativeStartAPIHarnessConfigured(t, true, true, corev1.ReadWriteOnce, kind, nil)
				installer := &nativeSandboxInstaller{kube: h.kube, chart: h.localHelm.chart}
				h.installer = installer
				h.r.HelmClient = &sharedSandboxInstaller{nativeSandboxInstaller: installer}
				if _, err := h.r.reconcilePolicy(h.ctx, h.current(t)); err != nil {
					t.Fatal(err)
				}
				if err := h.r.bootstrapReconciledSandbox(h.ctx, h.current(t)); err != nil {
					t.Fatal(err)
				}
				h.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: instanceNamespace(h.agent.Name)}})
				if err := h.admin.Create(h.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "platform"}}); client.IgnoreAlreadyExists(err) != nil {
					t.Fatal(err)
				}
				if mtls {
					withIngressMTLS(t, kind)(h.r, h.admin)
				}
				withObservedSNI(t, kind)(h.r, h.admin)
				team := &api.Team{}
				if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.agent.Spec.Team}, team); err != nil {
					t.Fatal(err)
				}
				policyReconciler := &AgentPolicyReconciler{Client: h.kube, APIReader: h.kube,
					Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
				if _, err := policyReconciler.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: team.Spec.AgentPolicyRef}}); err != nil {
					t.Fatal(err)
				}
				for range 4 {
					h.reconcileOK(t)
				}
				expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
				observed := *h.nativeSandboxFixture
				observed.kube = h.admin
				pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
				pod.Spec.Overhead = nil
				if err := h.admin.Create(h.ctx, pod, client.DryRunAll); err != nil {
					t.Fatal("real Pod admission rejected scoped capture:", err)
				}
				for name, mutate := range map[string]func(*corev1.Pod){
					"agent privilege": func(p *corev1.Pod) {
						p.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
					},
					"init extra privilege": func(p *corev1.Pod) {
						p.Spec.InitContainers[1].SecurityContext.Capabilities.Add = append(p.Spec.InitContainers[1].SecurityContext.Capabilities.Add, "SYS_ADMIN")
					},
					"proxy UID":       func(p *corev1.Pod) { p.Spec.InitContainers[0].SecurityContext.RunAsUser = ptr.To[int64](1024) },
					"missing capture": func(p *corev1.Pod) { p.Spec.InitContainers = slices.Delete(p.Spec.InitContainers, 1, 2) },
					"capture args":    func(p *corev1.Pod) { p.Spec.InitContainers[1].Command = []string{"/bin/sh", "-c", "true"} },
				} {
					t.Run(name, func(t *testing.T) {
						changed := pod.DeepCopy()
						mutate(changed)
						if err := h.admin.Create(h.ctx, changed, client.DryRunAll); !apierrors.IsForbidden(err) {
							t.Fatal("served admission accepted capture drift:", err)
						}
					})
				}
				config := &corev1.ConfigMap{}
				if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ingressmtls.ConfigName}, config); err != nil {
					t.Fatal(err)
				}
				config.Data["capture.sh"] = "exit 0\n"
				if err := h.admin.Update(h.ctx, config); err != nil {
					t.Fatal(err)
				}
				if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
					t.Fatal("served admission accepted removed capture commands:", err)
				}
				if pod.Spec.InitContainers[1].Name != egresscapture.InitContainerName {
					t.Fatal("capture did not traverse the served Kubernetes init-container path")
				}
			})
		}
	}
}
