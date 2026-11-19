// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func withApprovedCustomUser(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		agent := &api.Agent{}
		if err := kube.Get(context.Background(), client.ObjectKey{Name: "example"}, agent); err != nil {
			t.Fatal(err)
		}
		provider := r.CustomBindings.(*KubernetesCustomBindings)
		provider.config.ImageUsers = map[string]CustomImageUser{*agent.Spec.Agent.Image: {UID: 1024, GID: 1024}}
	}
}

func TestCommonCustomObservedSNIWithIdentityTLSAndTerminal(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		t.Run(string(backend), func(t *testing.T) {
			setup := []func(*AgentReconciler, client.Client){
				withCommonCustom(t, "chat"), withReconciledCoordination(t), withNativeEntra(t),
				withCustomConnections(t), withCustomMTLS(t, true), withObservedSNI(t, "custom"), withApprovedCustomUser(t),
			}
			if backend == api.SandboxContainer {
				setup = append(setup, withContainerRuntime(t))
			}
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "custom", true, false, setup...)
			if err := f.kube.Create(context.Background(), &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: instanceNamespace(f.agent.Name), Name: "kube-root-ca.crt"},
				Data:       map[string]string{"ca.crt": "synthetic-ca"},
			}); err != nil {
				t.Fatal(err)
			}
			for range 6 {
				f.reconcileOK(t)
			}
			config := &corev1.ConfigMap{}
			if err := f.kube.Get(context.Background(), client.ObjectKey{
				Namespace: instanceNamespace(f.agent.Name), Name: "cetirun-ingress-mtls",
			}, config); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(config.Data["envoy.json"], "cetirun_egress") ||
				config.Data["terminal-envoy.json"] == "" || config.Data["capture.sh"] == "" {
				t.Fatal("composed proxy lost an independently approved endpoint or capture initializer")
			}
			terminal := customTerminalDeployment(t, f)
			if len(terminal.Spec.Template.Spec.Containers) != 2 {
				t.Fatal("FQDN capture removed the independent terminal TLS helper")
			}
			terminalDigest := terminal.Spec.Template.Annotations[ingressmtls.ConfigDigestAnnotation]
			if terminalDigest == "" || terminalDigest == f.workload(t).Spec.Template.Annotations[ingressmtls.ConfigDigestAnnotation] {
				t.Fatal("terminal reused the main endpoint's combined TLS and capture digest")
			}
		})
	}
}

func TestCommonCustomObservedSNIComposition(t *testing.T) {
	for _, mode := range []string{"headless", "chat", "web-ui"} {
		for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
			t.Run(mode+"/"+string(backend), func(t *testing.T) {
				setup := []func(*AgentReconciler, client.Client){
					withCommonCustom(t, mode), withReconciledCoordination(t), withObservedSNI(t, "custom"),
					withApprovedCustomUser(t),
				}
				if backend == api.SandboxContainer {
					setup = append(setup, withContainerRuntime(t))
				}
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "custom", true, false, setup...)
				for range 5 {
					f.reconcileOK(t)
				}
				if len(f.workload(t).Spec.Template.Spec.InitContainers) < 3 {
					t.Fatal("Custom omitted managed proxy initialization")
				}
			})
		}
	}
}
