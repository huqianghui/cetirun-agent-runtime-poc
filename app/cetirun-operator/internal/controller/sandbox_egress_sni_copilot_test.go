// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxObservedSNIPreservesPrivateCopilot(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true,
				withIngressMTLS(t, kind), withObservedSNI(t, kind))
			f.reconcileOK(t)
			agent := f.current(t)
			key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: copilotProxyServiceName(agent)}
			service := &corev1.Service{}
			deployment := &appsv1.Deployment{}
			if err := f.kube.Get(context.Background(), key, service); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Get(context.Background(), key, deployment); err != nil {
				t.Fatal(err)
			}
			if service.Spec.Type != corev1.ServiceTypeClusterIP || len(service.Spec.ExternalIPs) != 0 ||
				service.Spec.ExternalName != "" || len(service.Spec.Ports) != 1 ||
				service.Spec.Ports[0].Port != 4000 || service.Spec.Ports[0].TargetPort != intstr.FromString("http") {
				t.Fatal("FQDN/mTLS composition changed the private Copilot Service")
			}
			for _, container := range append(slices.Clone(deployment.Spec.Template.Spec.InitContainers), deployment.Spec.Template.Spec.Containers...) {
				if container.Name == ingressmtls.ContainerName || container.Name == egresscapture.InitContainerName ||
					container.SecurityContext == nil || len(container.SecurityContext.Capabilities.Add) != 0 {
					t.Fatal("Agent capture or privileges leaked into the separate Copilot workload")
				}
			}
			for _, suspended := range []bool{true, false} {
				current := f.current(t)
				current.Spec.Suspended = suspended
				current.Generation++
				if err := f.kube.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if suspended && f.current(t).Status.Phase != api.PhaseSuspended {
					t.Fatal("private Copilot/capture lifetime did not suspend")
				}
				if !suspended && f.workload(t).Spec.Template.Spec.InitContainers[1].Name != egresscapture.InitContainerName {
					t.Fatal("resume lost the shared capture path")
				}
			}
		})
	}
}
