// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionhandler "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	istioapi "istio.io/api/networking/v1beta1"
	istio "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func withIngressMTLS(t *testing.T, kind string, desktop ...bool) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		ctx := context.Background()
		input := testfixture.New(kind)
		agent := &api.Agent{}
		if err := kube.Get(ctx, client.ObjectKey{Name: input.Agent.Name}, agent); err != nil {
			t.Fatal(err)
		}
		box := &api.Sandbox{}
		if err := kube.Get(ctx, client.ObjectKey{Name: agent.Spec.SandboxRef}, box); err != nil {
			t.Fatal(err)
		}
		agent.Spec.Helm.Values = map[string]runtime.RawExtension{
			"desktop": {Raw: []byte(`{"enabled":false}`)},
		}
		desktopEnabled := len(desktop) != 0 && desktop[0]
		if desktopEnabled {
			delete(agent.Spec.Helm.Values, "desktop")
		}
		if err := kube.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		ref := func(object client.Object) ingressmtls.Reference {
			return ingressmtls.Reference{Namespace: object.GetNamespace(), Name: object.GetName(),
				UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()}
		}
		server := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-ingress-tls", Namespace: instanceNamespace(agent.Name)}}
		clientSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-agent-example", Namespace: "platform"}}
		gateway := &istio.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "ingress", Namespace: "platform"}, Spec: istioapi.Gateway{
			Selector: map[string]string{"app": "managed-ingress"},
			Servers: []*istioapi.Server{{Port: &istioapi.Port{Number: 443, Protocol: "HTTPS", Name: "https"},
				Hosts: []string{r.DomainSuffix}, Tls: &istioapi.ServerTLSSettings{Mode: istioapi.ServerTLSSettings_SIMPLE, CredentialName: "browser-tls"}}},
		}}
		for _, object := range []client.Object{server, clientSecret, gateway} {
			if err := kube.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
		cm := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, cm); err != nil {
			t.Fatal(err)
		}
		var config SandboxPlatformConfiguration
		if err := json.Unmarshal([]byte(cm.Data[SandboxPlatformConfigurationKey]), &config); err != nil {
			t.Fatal(err)
		}
		registration := config.Registrations[box.Spec.Type]
		registration.Capabilities.MutualTLS = true
		config.Registrations[box.Spec.Type] = registration
		platform := config.Platforms[box.Spec.Type]
		platform.IngressMTLS = &ingressmtls.Registration{
			Image: "registry.example.com/envoy@sha256:" + strings.Repeat("a", 64),
			Bindings: []ingressmtls.Binding{{AgentUID: agent.UID, Namespace: server.Namespace, Harness: kind,
				Port: *agent.Spec.Agent.Port, Terminal: true, Desktop: desktopEnabled, ServerSecret: ref(server), ClientSecret: ref(clientSecret),
				Gateway: ref(gateway), ClientURI: "spiffe://example.com/gateway/" + string(agent.UID)}},
		}
		destination := &platform.Topology.Ingress[0]
		destination.Peer.IPs = nil
		destination.Peer.PodSelector = gateway.Spec.Selector
		destination.TrustedProxyCIDRs = []string{"10.244.0.0/16"}
		destination.Ports = nil
		for _, port := range (ingressmtls.Plan{Binding: platform.IngressMTLS.Bindings[0]}).Ports() {
			destination.Ports = append(destination.Ports, api.AgentPolicyPort{Port: port.Backend, Protocol: corev1.ProtocolTCP})
		}
		config.Platforms[box.Spec.Type] = platform
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		cm.Data[SandboxPlatformConfigurationKey] = string(raw)
		if err := kube.Update(ctx, cm); err != nil {
			t.Fatal(err)
		}
		policy := &api.AgentPolicy{}
		if err := kube.Get(ctx, client.ObjectKey{Name: input.Policy.Name}, policy); err != nil {
			t.Fatal(err)
		}
		policy.Spec.Network.Ingress.RequireMTLS = true
		if r.SandboxNativeCoordination == nil {
			if err := kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
		} else {
			before := &api.AgentPolicy{}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), before); err != nil {
				t.Fatal(err)
			}
			ticket, err := r.SandboxNativeCoordination.AcquireConfigurationChange(ctx, before, policy)
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(kube, kube, before, policy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.SandboxNativeCoordination.ReconcileChange(ctx, ticket, adapter); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestReconciledSandboxIngressMTLS(t *testing.T) {
	testReconciledSandboxIngressMTLS(t, api.SandboxKata)
}

func TestReconciledSandboxIngressMTLSPortalSignIn(t *testing.T) {
	for _, runtimeType := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		t.Run(string(runtimeType), func(t *testing.T) {
			ctx := context.Background()
			setup := []func(*AgentReconciler, client.Client){}
			if runtimeType == api.SandboxContainer {
				setup = append(setup, withContainerRuntime(t))
			}
			setup = append(setup, withIngressMTLS(t, "openclaw"), func(_ *AgentReconciler, kube client.Client) {
				agent := &api.Agent{}
				if err := kube.Get(ctx, client.ObjectKey{Name: testfixture.New("openclaw").Agent.Name}, agent); err != nil {
					t.Fatal(err)
				}
				agent.Spec.Helm.Values["openclaw"] = runtime.RawExtension{Raw: []byte(`{"portalSignIn":{"enabled":true}}`)}
				if err := kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
			})
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, false, setup...)
			f.reconcileOK(t)
			expected := reconciledExpectationForTest(t, f)
			workload := f.workload(t)
			config := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name + "-config"}, config); err != nil {
				t.Fatal(err)
			}
			var native struct {
				Gateway struct {
					Bind           string
					TrustedProxies []string
					Auth           struct{ Mode string }
				}
			}
			if err := json.Unmarshal([]byte(config.Data["openclaw.json"]), &native); err != nil ||
				native.Gateway.Bind != "loopback" || !slices.Equal(native.Gateway.TrustedProxies, []string{"127.0.0.1"}) ||
				native.Gateway.Auth.Mode != "trusted-proxy" {
				t.Fatal("reconciled mTLS Portal sign-in lost its loopback authentication contract:", err)
			}
			main := workload.Spec.Template.Spec.Containers[0]
			if !slices.ContainsFunc(main.Env, func(env corev1.EnvVar) bool {
				return env.Name == "OPENCLAW_GATEWAY_PASSWORD" && env.ValueFrom != nil &&
					env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Key == "OPENCLAW_GATEWAY_TOKEN"
			}) {
				t.Fatal("reconciled sign-in omitted the CLI loopback password")
			}
			handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
			if err != nil {
				t.Fatal(err)
			}
			pod := reconciledPodForTest(t, f, expected.Workloads[0])
			if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); !response.Allowed {
				t.Fatal("mTLS Portal sign-in Pod was not admitted:", response.Result)
			}
			modified := pod.DeepCopy()
			for index := range modified.Spec.Containers[0].Env {
				if modified.Spec.Containers[0].Env[index].Name == "OPENCLAW_GATEWAY_PASSWORD" {
					modified.Spec.Containers[0].Env[index].Name = "OPENCLAW_GATEWAY_TOKEN"
				}
			}
			if response := handler.Validate(ctx, reconciledPodRequest(t, modified)); response.Allowed {
				t.Fatal("admission accepted an authentication environment change")
			}
			config.Data["openclaw.json"] = strings.Replace(config.Data["openclaw.json"], "x-auth-request-email", "x-example-user", 1)
			if err := f.kube.Update(ctx, config); err != nil {
				t.Fatal(err)
			}
			if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); response.Allowed {
				t.Fatal("admission accepted drift in the managed authentication configuration")
			}
		})
	}
}

func testReconciledSandboxIngressMTLS(t *testing.T, runtimeType api.SandboxType) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, home := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(kind+"/"+string(home), func(t *testing.T) {
				ctx := context.Background()
				setup := []func(*AgentReconciler, client.Client){withNativeHomeAccessMode(t, home)}
				if runtimeType == api.SandboxContainer {
					setup = append(setup, withContainerRuntime(t))
				}
				setup = append(setup, withIngressMTLS(t, kind))
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, setup...)
				f.reconcileOK(t)
				if runtimeType == api.SandboxContainer {
					assertContainerWorkload(t, f, home)
				}
				expected := reconciledExpectationForTest(t, f)
				workload := f.workload(t)
				helper := slices.IndexFunc(workload.Spec.Template.Spec.Containers, func(container corev1.Container) bool {
					return container.Name == ingressmtls.ContainerName
				})
				if helper < 0 || !slices.Equal(workload.Spec.Template.Spec.Containers[0].Command, []string{"/bin/bash", "/etc/cetirun/ingress-entrypoint.sh"}) {
					t.Fatal("managed TLS endpoint or controlled native startup is missing")
				}
				main := workload.Spec.Template.Spec.Containers[0]
				for _, mount := range main.VolumeMounts {
					if mount.Name == "ingress-tls" {
						t.Fatal("application acquired the endpoint private key")
					}
				}
				if kind == "hermes" {
					for _, name := range []string{"API_SERVER_HOST", "HERMES_WEBAPP_HOST"} {
						if !slices.ContainsFunc(main.Env, func(env corev1.EnvVar) bool {
							return env.Name == name && env.Value == "127.0.0.1"
						}) {
							t.Fatal("Hermes backend is not loopback-only:", name)
						}
					}
					if !slices.ContainsFunc(workload.Spec.Template.Spec.InitContainers, func(container corev1.Container) bool {
						return container.Name == "init-home-data" && slices.Contains(container.Env, corev1.EnvVar{Name: "CETIRUN_INGRESS_MTLS", Value: "true"})
					}) {
						t.Fatal("Hermes HOME initialization did not reserve listener authority")
					}
					config := &corev1.ConfigMap{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name + "-config"}, config); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(config.Data["hermes-gateway.sh"], "hermes-runtime.py check") || !strings.Contains(config.Data["hermes-managed.yaml"], "port: 9120") {
						t.Fatal("Hermes supervised restarts omitted the current HOME authority helper")
					}
				} else {
					if !slices.Equal(main.Args, []string{"--bind", "loopback", "--port", "18789"}) {
						t.Fatal("OpenClaw command-line arguments override the loopback binding")
					}
					config := &corev1.ConfigMap{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name + "-config"}, config); err != nil {
						t.Fatal(err)
					}
					var native struct {
						Gateway struct {
							Bind           string
							TrustedProxies []string
						}
					}
					if err := json.Unmarshal([]byte(config.Data["openclaw.json"]), &native); err != nil ||
						native.Gateway.Bind != "loopback" || !slices.Equal(native.Gateway.TrustedProxies, []string{"127.0.0.1"}) {
						t.Fatal("OpenClaw did not bind only to its authenticated local endpoint:", err)
					}
				}
				handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
				if err != nil {
					t.Fatal(err)
				}
				pod := reconciledPodForTest(t, f, expected.Workloads[0])
				if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); !response.Allowed {
					t.Fatal("complete TLS Pod was not admitted:", response.Result)
				}
				modified := pod.DeepCopy()
				modified.Spec.Containers = slices.DeleteFunc(modified.Spec.Containers, func(c corev1.Container) bool { return c.Name == ingressmtls.ContainerName })
				if response := handler.Validate(ctx, reconciledPodRequest(t, modified)); response.Allowed {
					t.Fatal("admission accepted removal of the TLS endpoint")
				}
				if kind == "hermes" {
					for _, name := range []string{"API_SERVER_HOST", "API_SERVER_PORT", "API_SERVER_CORS_ORIGINS", "HERMES_WEBAPP_HOST", "HERMES_WEBAPP_PORT", "HERMES_WEBAPP_PUBLIC_URL", "HERMES_MANAGED_DIR", "TTYD_PORT"} {
						modified := pod.DeepCopy()
						for index := range modified.Spec.Containers[0].Env {
							if modified.Spec.Containers[0].Env[index].Name == name {
								modified.Spec.Containers[0].Env[index].Value = "unapproved"
							}
						}
						if response := handler.Validate(ctx, reconciledPodRequest(t, modified)); response.Allowed {
							t.Fatal("admission accepted an overridden managed listener:", name)
						}
					}
				}
				for _, source := range []struct{ namespace, name string }{
					{workload.Namespace, "agent-ingress-tls"}, {"platform", "gateway-agent-example"},
				} {
					if !slices.ContainsFunc(expected.Sources, func(ref w.SourceReference) bool {
						return ref.Kind == "Secret" && ref.Namespace == source.namespace && ref.Name == source.name && ref.UID != "" && ref.ResourceVersion != ""
					}) {
						t.Fatal("TLS Secret is not UID/version pinned")
					}
				}
				service := &corev1.Service{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name}, service); err != nil {
					t.Fatal(err)
				}
				for _, port := range service.Spec.Ports {
					if port.TargetPort.IntVal < ingressmtls.APIPort || port.TargetPort.IntVal > ingressmtls.TerminalPort {
						t.Fatal("Service retained plaintext bypass")
					}
				}
				if _, err := f.r.ensureVirtualService(ctx, f.current(t)); err != nil {
					t.Fatal("TLS ingress publication failed:", err)
				}
				homeUID := expected.Home.UID
				secret := &corev1.Secret{}
				key := client.ObjectKey{Namespace: workload.Namespace, Name: "agent-ingress-tls"}
				if err := f.kube.Get(ctx, key, secret); err != nil {
					t.Fatal(err)
				}
				secret.Data = map[string][]byte{"changed": []byte("synthetic")}
				if err := f.kube.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
				if _, err := f.reconcile(); err == nil {
					t.Fatal("unapproved certificate rotation was accepted")
				}
				if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); response.Allowed {
					t.Fatal("old certificate approval admitted another Pod after source drift")
				}
				f.assertNotReady(t)
				if f.workload(t).Spec.Replicas == nil || *f.workload(t).Spec.Replicas != 0 ||
					reconciledExpectationForTest(t, f).Home.UID != homeUID {
					t.Fatal("certificate drift did not stop the workload while retaining HOME")
				}
				platform := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, platform); err != nil {
					t.Fatal(err)
				}
				var configuration SandboxPlatformConfiguration
				if err := json.Unmarshal([]byte(platform.Data[SandboxPlatformConfigurationKey]), &configuration); err != nil {
					t.Fatal(err)
				}
				configuration.Platforms[runtimeType].IngressMTLS.Bindings[0].ServerSecret.ResourceVersion = secret.ResourceVersion
				raw, err := json.Marshal(configuration)
				if err != nil {
					t.Fatal(err)
				}
				platform.Data[SandboxPlatformConfigurationKey] = string(raw)
				if err := f.kube.Update(ctx, platform); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				rotated := reconciledExpectationForTest(t, f)
				if rotated.Home.UID != homeUID || rotated.Revision == expected.Revision {
					t.Fatal("approved rotation lost HOME or reused the old TLS execution revision")
				}
				if err := f.r.revokeSandboxPreparationGrants(ctx, f.current(t)); err != nil {
					t.Fatal("TLS source-read cleanup failed:", err)
				}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: "platform", Name: "gateway-agent-example"}, &corev1.Secret{}); err != nil {
					t.Fatal("cleanup removed externally provisioned Gateway credentials:", err)
				}
			})
		}
	}
}
