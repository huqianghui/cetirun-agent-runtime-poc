// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func withCustomMTLS(t *testing.T, terminal bool) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		r.ChatAccess = chataccess.Config{Enabled: true, Callers: []chataccess.Caller{{
			Namespace: "portal-system", PodLabels: map[string]string{"app": "portal"},
		}}}
		withIngressMTLS(t, "custom")(r, kube)
		r.CetiRunHelmValues["ttyd"].(map[string]any)["enabled"] = terminal
		agent := &api.Agent{}
		if err := kube.Get(context.Background(), client.ObjectKey{Name: "example"}, agent); err != nil {
			t.Fatal(err)
		}
		var clients []ingressmtls.ChatClient
		if !agent.Spec.Agent.WebUI {
			for _, caller := range r.ChatAccess.Callers {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: caller.Namespace, Name: "custom-chat-client"}}
				if err := kube.Create(context.Background(), secret); err != nil {
					t.Fatal(err)
				}
				clients = append(clients, ingressmtls.ChatClient{
					Secret: ingressmtls.Reference{Namespace: secret.Namespace, Name: secret.Name, UID: secret.UID, ResourceVersion: secret.ResourceVersion},
					URI:    "spiffe://example.com/portal/" + string(agent.UID), PodLabels: caller.PodLabels,
				})
			}
		}
		updateContainerPlatformForTest(t, r, kube, func(document *SandboxPlatformConfiguration) {
			for kind, platform := range document.Platforms {
				if platform.IngressMTLS != nil {
					platform.IngressMTLS.Bindings[0].Terminal = terminal
					platform.IngressMTLS.Bindings[0].ChatClients = clients
					if terminal {
						platform.Topology.KubernetesAPI = []sandboxpolicy.Destination{{
							Peer:  sandboxpolicy.Peer{Name: "api", UID: "example-api-uid", IPs: []string{"203.0.113.10"}},
							Ports: []api.AgentPolicyPort{{Port: 6443, Protocol: corev1.ProtocolTCP}},
						}}
					}
					document.Platforms[kind] = platform
				}
			}
		})
	}
}

func TestCommonCustomAllCapabilitiesCompose(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		t.Run(string(backend), func(t *testing.T) {
			setup := []func(*AgentReconciler, client.Client){
				withCommonCustom(t, "chat"), withReconciledCoordination(t), withNativeEntra(t), withCustomConnections(t),
			}
			if backend == api.SandboxContainer {
				setup = append(setup, withContainerRuntime(t))
			}
			setup = append(setup, withCustomMTLS(t, false))
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "custom", true, false, setup...)
			for range 6 {
				f.reconcileOK(t)
			}
			main := f.workload(t).Spec.Template.Spec.Containers[0]
			for _, name := range []string{"AZURE_CLIENT_ID", "CETIRUN_CONNECTIONS_MCP_URL", "CETIRUN_INGRESS_MTLS"} {
				if !slices.ContainsFunc(main.Env, func(env corev1.EnvVar) bool { return env.Name == name && env.Value != "" }) {
					t.Fatal("combined Custom workload omitted", name)
				}
			}
			expected := reconciledExpectationForTest(t, f)
			for index, control := range expected.Controls {
				if control.GetKind() == "DestinationRule" && control.GetNamespace() == "portal-system" {
					tampered := control.DeepCopy()
					if err := unstructured.SetNestedField(tampered.Object, "foreign-client", "spec", "trafficPolicy", "tls", "credentialName"); err != nil {
						t.Fatal(err)
					}
					expected.Controls[index] = *tampered
					if err := expected.Validate(); err == nil {
						t.Fatal("foreign Chat credential passed protected control validation")
					}
					return
				}
			}
			t.Fatal("Custom private Chat lacks its approved client policy")
		})
	}
}

func TestCommonCustomMTLSEndpoints(t *testing.T) {
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
					if terminal {
						if err := f.kube.Create(context.Background(), &corev1.ConfigMap{
							ObjectMeta: metav1.ObjectMeta{Namespace: instanceNamespace(f.agent.Name), Name: "kube-root-ca.crt"},
							Data:       map[string]string{"ca.crt": "synthetic-ca"},
						}); err != nil {
							t.Fatal(err)
						}
					}
					for range 5 {
						f.reconcileOK(t)
					}
					expected := reconciledExpectationForTest(t, f)
					spec := f.workload(t).Spec.Template.Spec
					if len(spec.Containers) != 2 || spec.Containers[1].Name != ingressmtls.ContainerName ||
						ptr.Deref(spec.Containers[1].SecurityContext.RunAsUser, 0) != 65532 ||
						len(spec.Containers[0].Command) != 0 ||
						!slices.Contains(spec.Containers[0].Env, corev1.EnvVar{Name: "CETIRUN_BIND_HOST", Value: "127.0.0.1"}) {
						t.Fatal("Custom TLS lost its native entrypoint or protected loopback endpoint")
					}
					destinations := 0
					for _, control := range expected.Controls {
						if control.GetKind() == "DestinationRule" {
							destinations++
						}
						if control.GetKind() == "Service" {
							if control.GetName() != instanceServiceName(f.agent.Name) &&
								(!terminal || control.GetName() != customTerminalDeployment(t, f).Name) {
								continue
							}
							ports, _, err := unstructured.NestedSlice(control.Object, "spec", "ports")
							if err != nil || len(ports) != 1 {
								t.Fatal("Custom TLS Service has unexpected ports")
							}
							port := ports[0].(map[string]any)["targetPort"]
							if port != int64(ingressmtls.APIPort) && port != int64(ingressmtls.TerminalPort) {
								t.Fatal("Custom TLS Service retained a plaintext route")
							}
						}
					}
					want := 1
					if mode == "chat" {
						want++
					}
					if terminal {
						want++
						terminalSpec := customTerminalDeployment(t, f).Spec.Template.Spec
						if len(terminalSpec.Containers) != 2 || terminalSpec.Containers[1].Name != ingressmtls.ContainerName ||
							terminalSpec.Containers[0].ReadinessProbe.Exec == nil {
							t.Fatal("isolated Custom terminal lacks its own loopback TLS endpoint")
						}
					}
					if destinations != want {
						t.Fatalf("got %d destination policies, want %d", destinations, want)
					}
				})
			}
		}
	}
}
