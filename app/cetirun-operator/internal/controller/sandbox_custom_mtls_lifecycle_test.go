// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCommonCustomMTLSSuspensionAndResume(t *testing.T) {
	for _, mode := range []string{"chat", "web-ui"} {
		for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
			for _, terminal := range []bool{false, true} {
				name := mode + "/" + string(backend)
				if terminal {
					name += "/terminal"
				}
				t.Run(name, func(t *testing.T) {
					setup := []func(*AgentReconciler, client.Client){withCommonCustom(t, mode), withReconciledCoordination(t)}
					if backend == api.SandboxContainer {
						setup = append(setup, withContainerRuntime(t))
					}
					setup = append(setup, withCustomMTLS(t, terminal))
					f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "custom", true, false, setup...)
					ctx := context.Background()
					if terminal {
						if err := f.kube.Create(ctx, &corev1.ConfigMap{
							ObjectMeta: metav1.ObjectMeta{Namespace: instanceNamespace(f.agent.Name), Name: "kube-root-ca.crt"},
							Data:       map[string]string{"ca.crt": "synthetic-ca"},
						}); err != nil {
							t.Fatal(err)
						}
					}
					for range 5 {
						f.reconcileOK(t)
					}
					ready := func() {
						readyCustomWorkload(t, f)
						if terminal {
							deployment := customTerminalDeployment(t, f)
							deployment.Status = appsv1.DeploymentStatus{
								ObservedGeneration: deployment.Generation, Replicas: 1,
								ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1,
							}
							if err := f.kube.Status().Update(ctx, deployment); err != nil {
								t.Fatal(err)
							}
						}
						for range 2 {
							f.reconcileOK(t)
						}
						if f.current(t).Status.Phase != api.PhaseReady {
							t.Fatal("current mTLS endpoints did not become Ready")
						}
					}
					ready()
					before := f.current(t)
					key, workloadUID := before.Status.AIGateway.KeyID, f.workload(t).UID
					current := before.DeepCopy()
					current.Spec.Suspended, current.Generation = true, current.Generation+1
					if err := f.kube.Update(ctx, current); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "waits for acknowledged controller shutdown") {
						t.Fatal("suspension did not wait for current shutdown:", err)
					}
					main := f.workload(t)
					if ptr.Deref(main.Spec.Replicas, -1) != 0 ||
						!f.current(t).Status.AIGateway.Blocked {
						t.Fatal("pending suspension did not stop compute and block the key")
					}
					main.Status = appsv1.StatefulSetStatus{ObservedGeneration: main.Generation}
					if err := f.kube.Status().Update(ctx, main); err != nil {
						t.Fatal(err)
					}
					if terminal {
						deployment := customTerminalDeployment(t, f)
						if ptr.Deref(deployment.Spec.Replicas, -1) != 0 {
							t.Fatal("pending suspension left the terminal running")
						}
						deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation}
						if err := f.kube.Status().Update(ctx, deployment); err != nil {
							t.Fatal(err)
						}
					}
					for range 3 {
						f.reconcileOK(t)
						current = f.current(t)
						if current.Status.Phase != api.PhaseSuspended || current.Status.AccessURL != "" ||
							current.Status.AIGateway.KeyID != key || !current.Status.AIGateway.Blocked ||
							ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
							t.Fatal("mTLS suspension did not retain a stopped, blocked lifetime")
						}
						if terminal && ptr.Deref(customTerminalDeployment(t, f).Spec.Replicas, -1) != 0 {
							t.Fatal("steady suspended reconciliation restarted the terminal")
						}
					}
					current.Spec.Suspended, current.Generation = false, current.Generation+1
					if err := f.kube.Update(ctx, current); err != nil {
						t.Fatal(err)
					}
					f.reconcileOK(t)
					ready()
					current = f.current(t)
					if current.UID != before.UID || current.Status.Namespace != before.Status.Namespace ||
						current.Status.AIGateway.KeyID != key || current.Status.AIGateway.Blocked ||
						f.workload(t).UID != workloadUID {
						t.Fatal("mTLS resume replaced the retained lifetime or left its key blocked")
					}
					if terminal {
						deployment := customTerminalDeployment(t, f)
						if ptr.Deref(deployment.Spec.Replicas, -1) != 1 ||
							len(deployment.Spec.Template.Spec.Containers) != 2 ||
							deployment.Spec.Template.Spec.Containers[1].Name != ingressmtls.ContainerName {
							t.Fatal("resume lost the approved isolated TLS terminal")
						}
					}
				})
			}
		}
	}
}
