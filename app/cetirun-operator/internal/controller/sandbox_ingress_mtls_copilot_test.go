// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxIngressMTLSServiceBoundaries(t *testing.T) {
	for _, scenario := range []struct {
		name, template, old, replacement, want string
	}{
		{
			name: "public companion", template: "templates/copilot-proxy.yaml",
			old: "  type: ClusterIP", replacement: "  type: LoadBalancer",
			want: "Sandbox services must remain private",
		},
		{
			name: "external companion", template: "templates/copilot-proxy.yaml",
			old: "  type: ClusterIP", replacement: "  type: ClusterIP\n  externalIPs: [\"192.0.2.10\"]",
			want: "Sandbox services must remain private",
		},
		{
			name: "foreign namespace", template: "templates/copilot-proxy.yaml",
			old:         "kind: Service\nmetadata:\n  name: {{ $name }}",
			replacement: "kind: Service\nmetadata:\n  name: {{ $name }}\n  namespace: another-namespace",
			want:        "foreign native Sandbox object",
		},
		{
			name: "foreign owner", template: "templates/copilot-proxy.yaml",
			old: "kind: Service\nmetadata:\n  name: {{ $name }}",
			replacement: "kind: Service\nmetadata:\n  name: {{ $name }}\n  ownerReferences:\n" +
				"    - apiVersion: cetirun.microsoft.com/v1alpha1\n      kind: Agent\n" +
				"      name: another-agent\n      uid: another-agent-uid\n      controller: true",
			want: "native Sandbox source has foreign ownership",
		},
		{
			name: "extra Agent ingress", template: "templates/service.yaml",
			old: "  ports:", replacement: "  ports:\n    - name: unexpected\n      port: 8080\n      targetPort: 8080\n      protocol: TCP",
			want: "mTLS Service inventory differs",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, true, withIngressMTLS(t, "openclaw"))
			changed := false
			for _, template := range f.installer.chart.Templates {
				if template.Name != scenario.template {
					continue
				}
				if strings.Count(string(template.Data), scenario.old) != 1 {
					t.Fatal("fixture requires exactly one targeted Service field")
				}
				template.Data = []byte(strings.Replace(string(template.Data), scenario.old, scenario.replacement, 1))
				changed = true
			}
			if !changed {
				t.Fatal("fixture did not locate its Service template")
			}
			var rejected error
			for range 4 {
				_, rejected = f.reconcile()
				if rejected != nil {
					break
				}
			}
			if rejected == nil || !strings.Contains(rejected.Error(), scenario.want) || f.installer.stages != 0 {
				t.Fatalf("Service boundary was not rejected before apply: %v", rejected)
			}
		})
	}
}

func TestReconciledSandboxIngressMTLSCopilot(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, shared := range []bool{false, true} {
			for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
				t.Run(fmt.Sprintf("%s/shared=%t/%s", kind, shared, mode), func(t *testing.T) {
					ctx := context.Background()
					f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, shared, true,
						withNativeHomeAccessMode(t, mode), withIngressMTLS(t, kind))
					f.reconcileOK(t)
					initial := reconciledExpectationForTest(t, f)
					agent := f.current(t)
					keyID := agent.Status.AIGateway.KeyID
					proxyKey := client.ObjectKey{Namespace: initial.Namespace.Name, Name: copilotProxyServiceName(agent)}
					proxyService := &corev1.Service{}
					if err := f.kube.Get(ctx, proxyKey, proxyService); err != nil {
						t.Fatal(err)
					}
					proxyDeployment := &appsv1.Deployment{}
					if err := f.kube.Get(ctx, proxyKey, proxyDeployment); err != nil {
						t.Fatal(err)
					}
					if !sandboxBuiltinOwned(proxyService, agent) || proxyService.Spec.Type != corev1.ServiceTypeClusterIP ||
						proxyService.Spec.ExternalName != "" || len(proxyService.Spec.ExternalIPs) != 0 ||
						!equality.Semantic.DeepEqual(proxyService.Spec.Selector, proxyDeployment.Spec.Selector.MatchLabels) ||
						len(proxyService.Spec.Ports) != 1 || proxyService.Spec.Ports[0].Name != "http" ||
						proxyService.Spec.Ports[0].Port != 4000 || proxyService.Spec.Ports[0].Protocol != corev1.ProtocolTCP ||
						proxyService.Spec.Ports[0].TargetPort != intstr.FromString("http") {
						t.Fatal("mTLS changed the private, separately selected Copilot Service")
					}
					for _, key := range f.gateway.keys {
						key.Spend = ptr.To(3.25)
					}
					for _, suspended := range []bool{false, true, false} {
						if suspended != f.current(t).Spec.Suspended {
							current := f.current(t)
							current.Spec.Suspended = suspended
							current.Generation++
							if err := f.kube.Update(ctx, current); err != nil {
								t.Fatal(err)
							}
							f.reconcileOK(t)
						}
						current := f.current(t)
						expected := reconciledExpectationForTest(t, f)
						wantReplicas, wantWorkloads := int32(1), 2
						if suspended {
							wantReplicas, wantWorkloads = 0, 1
						}
						if current.UID != agent.UID || expected.Namespace != initial.Namespace || expected.Home != initial.Home ||
							current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 || len(expected.Workloads) != wantWorkloads {
							t.Fatalf("mTLS/Copilot retained lifetime: suspended=%t agent=%t namespace=%t home=%t key=%t generated=%d workloads=%d",
								suspended, current.UID == agent.UID, expected.Namespace == initial.Namespace, expected.Home == initial.Home,
								current.Status.AIGateway.KeyID == keyID, len(f.gateway.generated), len(expected.Workloads))
						}
						for _, workload := range expected.Workloads {
							if workload.Replicas != wantReplicas {
								t.Fatal("Agent and Copilot companion did not follow suspension/resume")
							}
							if workload.Kind == "StatefulSet" && !slices.ContainsFunc(workload.Template.Spec.Containers, func(container corev1.Container) bool {
								return container.Name == ingressmtls.ContainerName
							}) {
								t.Fatal("Agent workload lost its mTLS endpoint")
							}
						}
						if suspended && (current.Status.Phase != api.PhaseSuspended || !current.Status.AIGateway.Blocked) {
							t.Fatal("suspension was not acknowledged with the Gateway key blocked")
						}
						if !suspended && current.Status.AIGateway.Blocked {
							t.Fatal("mTLS/Copilot resume retained a stale key block")
						}
						service := &corev1.Service{}
						if err := f.kube.Get(ctx, client.ObjectKey{Namespace: initial.Namespace.Name, Name: instanceServiceName(agent.Name)}, service); err != nil {
							t.Fatal(err)
						}
						for _, port := range service.Spec.Ports {
							if port.TargetPort.Type != intstr.Int || port.TargetPort.IntVal < ingressmtls.APIPort ||
								port.TargetPort.IntVal > ingressmtls.TerminalPort {
								t.Fatal("Agent Service retained a plaintext ingress target")
							}
						}
						currentProxyService := &corev1.Service{}
						if err := f.kube.Get(ctx, proxyKey, currentProxyService); err != nil {
							t.Fatal(err)
						}
						if currentProxyService.UID != proxyService.UID || !equality.Semantic.DeepEqual(currentProxyService.Spec, proxyService.Spec) {
							t.Fatal("suspension/resume rewrote the Copilot companion Service")
						}
						if err := f.kube.Get(ctx, proxyKey, proxyDeployment); err != nil || ptr.Deref(proxyDeployment.Spec.Replicas, 0) != wantReplicas {
							t.Fatal("retained Copilot companion did not follow suspension/resume:", err)
						}
						for _, key := range f.gateway.keys {
							if ptr.Deref(key.Spend, 0) != 3.25 {
								t.Fatal("mTLS/Copilot reconciliation reset retained spend")
							}
						}
					}
				})
			}
		}
	}
}
