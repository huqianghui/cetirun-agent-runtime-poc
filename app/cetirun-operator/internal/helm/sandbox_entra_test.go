// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// This compiles synthetic protected contracts, not a native registration or an
// execution qualification. OpenClaw starts from the retained renderer; Hermes
// starts from the current common executor.
func entraRenderBundle(t *testing.T, kind string, enabled bool) *w.Bundle {
	t.Helper()
	return entraRenderBundleAtRevision(t, kind, enabled, "synthetic-approval")
}

func entraRenderBundleAtRevision(t *testing.T, kind string, enabled bool, revision string) *w.Bundle {
	t.Helper()
	f := testfixture.New(kind)
	// Gateway-only charts require the separately authorized virtual-key
	// projection in addition to the runtime Secret used by the fixture.
	f.Input.GatewayKey = &w.ObjectIdentity{Name: "ai-gateway-virtual-key", UID: "model-key-uid"}
	f.Input.AuthorizedSecrets = append(f.Input.AuthorizedSecrets, f.Input.GatewayKey.Name)
	// Hermes has no retained Gateway-only renderer. Its protected identity
	// projection is exercised through the current common executor instead.
	f.Input.Reconciled = kind == "hermes"
	f.Policy.Spec.EntraID.Enabled = enabled
	objects := sandbox.Resources{Sandboxes: map[string]*api.Sandbox{f.Sandbox.Name: f.Sandbox},
		AgentPolicies: map[string]*api.AgentPolicy{f.Policy.Name: f.Policy}}
	record, err := sandboxbinding.Snapshot(f.Agent, f.Team, objects)
	if err != nil {
		t.Fatal(err)
	}
	f.Agent.Annotations[sandboxbinding.AuthorizationAnnotation] = record
	binding, err := sandboxbinding.AuthorizedBinding(f.Agent)
	if err != nil {
		t.Fatal(err)
	}
	caps := f.Input.Resolution.Capabilities
	caps.EntraID = true
	f.Input.Resolution, err = sandbox.Resolve(sandbox.ResolveInput{
		Agent: f.Agent, Team: f.Team, Resources: objects, AuthorizedBinding: binding,
		Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {Capabilities: caps, Available: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := sandboxpolicy.Compile(sandboxpolicy.CompileInput{
		AgentName: f.Agent.Name, Namespace: f.Namespace.Name, Resolution: *f.Input.Resolution,
		Platform: sandboxpolicy.Platform{
			Adapter: sandboxpolicy.AdapterRegistration{Name: sandboxpolicy.AzureCNIOverlayCilium,
				Revision: "synthetic-only", Available: true, Qualified: true},
			Topology: sandboxpolicy.Topology{
				Revision: "synthetic-only", ManagedPeersComplete: true, DNSResolverIPs: []string{"10.96.0.10"},
				DNS: []sandboxpolicy.Peer{{Name: "dns", Namespace: "platform-dns", UID: "dns-uid", IPs: []string{"10.96.0.10"}}},
				Gateway: []sandboxpolicy.Destination{{Peer: sandboxpolicy.Peer{Name: "gateway", Namespace: "platform",
					UID: "gateway-uid", IPs: []string{"10.96.1.50"}}, Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}}}},
				PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
				NodeCIDRs: []string{"192.168.0.0/16"}, ManagementCIDRs: []string{"10.1.0.0/16"}, MetadataCIDRs: []string{"169.254.169.254/32"},
			},
			KubernetesAudience: "https://kubernetes.default.svc",
			Identities: []sandboxpolicy.ApprovedIdentity{{
				AgentUID: f.Agent.UID, Namespace: f.Namespace.Name, ServiceAccountName: "example-entra",
				Revision: revision, Entra: &sandboxpolicy.EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Input.Controls, err = w.ControlsFromPlan(*plan)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := w.Build(f.Input)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func entraChartInput(kind string) map[string]any {
	values := sampleInput(kind, "openai", false)
	values["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	return values
}

func TestEntraRuntimeProjectionContainerScope(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			bundle := entraRenderBundle(t, kind, true)
			disabled := entraRenderBundle(t, kind, false)
			if !reflect.DeepEqual(bundle.Accounting(), disabled.Accounting()) ||
				bundle.Identity().Home.UID != disabled.Identity().Home.UID {
				t.Fatal("Entra projection changed the resource budget or retained HOME")
			}
			prepared, err := PrepareSandboxChart(testChart(t), entraChartInput(kind), nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			input := bundle.Identity()
			var expected *w.Expectation
			if input.Reconciled {
				if err := bundle.ValidateReconciledTemplate(template, prepared.Agent.Image, false, prepared.Agent.Port); err != nil {
					t.Fatal("common renderer lost the Entra projection:", err)
				}
			} else {
				expectation, err := bundle.Expect(*template, testfixture.Workload(input, *template), nil)
				if err != nil || !expectation.ExecutionHeld {
					t.Fatal("projection rendering must remain held:", err)
				}
				expected = &expectation
			}
			if template.Labels["azure.workload.identity/use"] != "" {
				t.Fatal("projection opted into arbitrary workload identity webhook injection")
			}
			volume := input.Controls.Volumes[0]
			mount := input.Controls.VolumeMounts[0]
			for name, mutate := range map[string]func(*corev1.PodSpec){
				"wrong account": func(s *corev1.PodSpec) { s.ServiceAccountName = "other-agent" },
				"automount":     func(s *corev1.PodSpec) { s.AutomountServiceAccountToken = ptr.To(true) },
				"missing token": func(s *corev1.PodSpec) {
					s.Volumes = slices.DeleteFunc(s.Volumes, func(v corev1.Volume) bool { return v.Name == volume.Name })
				},
				"missing mount": func(s *corev1.PodSpec) {
					s.Containers[0].VolumeMounts = slices.DeleteFunc(s.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == mount.Name })
				},
				"missing environment": func(s *corev1.PodSpec) {
					s.Containers[0].Env = slices.DeleteFunc(s.Containers[0].Env, func(e corev1.EnvVar) bool { return e.Name == "AZURE_TENANT_ID" })
				},
				"writable mount": func(s *corev1.PodSpec) {
					for i := range s.Containers[0].VolumeMounts {
						if s.Containers[0].VolumeMounts[i].Name == mount.Name {
							s.Containers[0].VolumeMounts[i].ReadOnly = false
						}
					}
				},
				"helper environment": func(s *corev1.PodSpec) {
					s.Containers[1].Env = append(s.Containers[1].Env, input.Controls.Environment[0])
				},
				"helper mount": func(s *corev1.PodSpec) { s.Containers[1].VolumeMounts = append(s.Containers[1].VolumeMounts, mount) },
				"init mount": func(s *corev1.PodSpec) {
					s.InitContainers[0].VolumeMounts = append(s.InitContainers[0].VolumeMounts, mount)
				},
				"init environment": func(s *corev1.PodSpec) {
					s.InitContainers[0].Env = append(s.InitContainers[0].Env, input.Controls.Environment[0])
				},
				"secret environment": func(s *corev1.PodSpec) {
					s.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "runtime-secret"}}}}
				},
				"borrowed token": func(s *corev1.PodSpec) {
					other := *volume.DeepCopy()
					other.Name = "borrowed"
					s.Volumes = append(s.Volumes, other)
				},
			} {
				t.Run(name, func(t *testing.T) {
					changed := template.DeepCopy()
					mutate(&changed.Spec)
					if err := bundle.ValidateIdentityProjection(changed.Spec); err == nil {
						t.Fatal("runtime identity drift passed the shared validation core")
					}
					if input.Reconciled {
						if err := bundle.ValidateReconciledTemplate(changed, prepared.Agent.Image, false, prepared.Agent.Port); err == nil {
							t.Fatal("common rendering accepted identity drift")
						}
					} else if err := bundle.ValidateTemplate(changed, prepared.Agent.Image); err == nil {
						t.Fatal("held rendering accepted identity drift")
					}
					common := bundle.Identity()
					common.Reconciled = true
					commonBundle, err := w.Build(common)
					if err != nil {
						t.Fatal(err)
					}
					if err := commonBundle.ValidateReconciledTemplate(changed, prepared.Agent.Image, false, prepared.Agent.Port); err == nil {
						t.Fatal("common rendering accepted identity drift")
					}
					if expected != nil {
						pod := testfixture.Pod(*expected)
						pod.Spec = changed.Spec
						if err := w.ValidatePod(pod, *expected); err == nil {
							t.Fatal("final Pod accepted identity drift")
						}
					}
				})
			}
			for _, user := range []map[string]any{
				{"azureWorkloadIdentity": map[string]any{"enabled": true}},
				{"extraEnv": []any{map[string]any{"name": "AZURE_CLIENT_ID", "value": "other-client"}}},
				{"serviceAccount": map[string]any{"name": "borrowed"}},
			} {
				if _, err := PrepareSandboxChart(testChart(t), entraChartInput(kind), user, bundle); err == nil {
					t.Fatal("tenant values changed the approved runtime identity")
				}
			}
		})
	}
}

func TestEntraCommonRenderingIsNotExecutionAuthority(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			input := entraRenderBundle(t, kind, true).Identity()
			input.Reconciled = true
			bundle, err := w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareSandboxChart(testChart(t), entraChartInput(kind), nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			if err := bundle.ValidateReconciledTemplate(template, prepared.Agent.Image, false, prepared.Agent.Port); err != nil {
				t.Fatal("common renderer lost the compiler's Entra projection:", err)
			}
			manifest, err := prepared.SandboxManifest()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(manifest), `"kind":"RoleBinding"`) || strings.Contains(string(manifest), `"kind":"ClusterRoleBinding"`) {
				t.Fatal("Entra-only rendering granted Kubernetes RBAC")
			}
			if err := prepared.AuthorizeSandboxApply(context.Background()); err == nil {
				t.Fatal("render-only Entra inputs authorized common execution")
			}
		})
	}
}
