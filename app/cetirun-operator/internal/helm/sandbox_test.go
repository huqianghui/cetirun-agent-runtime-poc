// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"errors"
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
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

// The historical held renderer supports the OpenClaw Gateway key contract.
// Common-executor Hermes rendering is covered in sandbox_reconcile_test.go.
func gatewayHeldFixture(typ string) testfixture.Fixture {
	fixture := testfixture.New(typ)
	fixture.Input.GatewayKey = &w.ObjectIdentity{Name: "ai-gateway-virtual-key", UID: "model-key-uid"}
	fixture.Input.AuthorizedSecrets = append(fixture.Input.AuthorizedSecrets, fixture.Input.GatewayKey.Name)
	return fixture
}

func sandboxRender(t *testing.T, typ string, readOnly, desktop bool) (*PreparedRelease, *w.Bundle, w.Expectation) {
	t.Helper()
	fixture := gatewayHeldFixture(typ)
	fixture.Input.Helpers.Desktop = desktop
	fixture.Input.Resolution.AgentPolicy.Security.ReadOnlyRootFilesystem = ptr.To(readOnly)
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput(typ, "openai", false)
	input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	prepared, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	workload := testfixture.Workload(fixture.Input, *template)
	expected, err := bundle.Expect(*template, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected.SinglePod = true
	expected.ControllerRevision = workload.Status.UpdateRevision
	return prepared, bundle, expected
}

func TestSandboxNativeProtectedRendering(t *testing.T) {
	for _, typ := range []string{"openclaw"} {
		for _, readOnly := range []bool{true, false} {
			t.Run(typ+map[bool]string{true: "/readonly", false: "/writable-root"}[readOnly], func(t *testing.T) {
				prepared, bundle, expected := sandboxRender(t, typ, readOnly, true)
				template, err := prepared.SandboxTemplate()
				if err != nil {
					t.Fatal(err)
				}
				if err := bundle.ValidateTemplate(template, prepared.Agent.Image); err != nil {
					t.Fatal(err)
				}
				if template.Spec.DNSPolicy != corev1.DNSNone || template.Spec.DNSConfig == nil ||
					len(template.Spec.DNSConfig.Nameservers) != 1 || template.Spec.DNSConfig.Nameservers[0] != "10.96.0.10" ||
					len(template.Spec.DNSConfig.Searches) != 0 || len(template.Spec.DNSConfig.Options) != 0 {
					t.Fatalf("protected DNS was not rendered: policy=%q config=%+v", template.Spec.DNSPolicy, template.Spec.DNSConfig)
				}
				if !expected.ExecutionHeld || prepared.Values["replicaCount"] != 0 {
					t.Fatal("rendering lifted the execution hold")
				}
				manifest, err := prepared.SandboxManifest()
				if err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{`"kind":"PersistentVolume"`, `"kind":"PersistentVolumeClaim"`, `"kind":"Job"`, "volumeClaimTemplates", "helm.sh/hook"} {
					if strings.Contains(string(manifest), forbidden) {
						t.Fatalf("Sandbox rendered forbidden lifecycle resource %s", forbidden)
					}
				}
				if !strings.Contains(string(manifest), `"kind":"CiliumNetworkPolicy"`) {
					t.Fatal("compiler network output was not rendered")
				}
				if strings.Contains(string(manifest), `"kind":"ServiceAccount"`) || strings.Contains(string(manifest), `"kind":"NetworkPolicy"`) {
					t.Fatal("ordinary compiler mode emitted a default identity or legacy network resource")
				}
				if err := w.ValidatePod(testfixture.Pod(expected), expected); err != nil {
					t.Fatalf("valid final Pod rejected: %v", err)
				}
				if err := (&Client{}).InstallOrUpgrade(expected.Namespace.Name, "example", prepared); !errors.Is(err, w.ErrExecutionHeld) {
					t.Fatalf("a render-only Sandbox plan reached an apply path: %v", err)
				}
			})
		}
	}
}

func TestSandboxDesktopScratchIsolation(t *testing.T) {
	for _, typ := range []string{"openclaw"} {
		for _, readOnly := range []bool{true, false} {
			for _, desktop := range []bool{true, false} {
				name := typ + map[bool]string{true: "/readonly", false: "/writable-root"}[readOnly] +
					map[bool]string{true: "/desktop", false: "/no-desktop"}[desktop]
				t.Run(name, func(t *testing.T) {
					prepared, bundle, expected := sandboxRender(t, typ, readOnly, desktop)
					template, err := prepared.SandboxTemplate()
					if err != nil {
						t.Fatal(err)
					}
					if err := bundle.ValidateTemplate(template, prepared.Agent.Image); err != nil {
						t.Fatal(err)
					}
					mounts := map[string]string{}
					for _, container := range template.Spec.Containers {
						if container.Name != "cetirun" && container.Name != "desktop" {
							continue
						}
						for _, mount := range container.VolumeMounts {
							if mount.MountPath == "/tmp" {
								mounts[container.Name] = mount.Name
							}
						}
					}
					if mounts["cetirun"] != "tmp" ||
						(desktop && mounts["desktop"] != "tmp-desktop") ||
						(!desktop && mounts["desktop"] != "") {
						t.Fatalf("desktop scratch is not isolated or unexpectedly present: %v", mounts)
					}
					found := map[string]bool{}
					for _, volume := range template.Spec.Volumes {
						if volume.Name != "tmp" && volume.Name != "tmp-desktop" {
							continue
						}
						found[volume.Name] = true
						if volume.EmptyDir == nil || volume.EmptyDir.Medium != "" || volume.EmptyDir.SizeLimit == nil ||
							volume.EmptyDir.SizeLimit.Cmp(resource.MustParse("1Gi")) != 0 {
							t.Fatalf("scratch volume %s is not bounded by the Sandbox limit", volume.Name)
						}
					}
					if !found["tmp"] || found["tmp-desktop"] != desktop {
						t.Fatalf("unexpected scratch volume inventory: %v", found)
					}
					if err := w.ValidatePod(testfixture.Pod(expected), expected); err != nil {
						t.Fatalf("isolated desktop template failed final Pod validation: %v", err)
					}
				})
			}
		}
	}
}

func TestSandboxAllowsModelAndAuthorizedSecretWithoutOverrides(t *testing.T) {
	fixture := gatewayHeldFixture("openclaw")
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput("openclaw", "openai", false)
	input["aiGateway"].(map[string]any)["models"] = []any{map[string]any{"id": "chosen-model", "name": "Chosen model"}}
	input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	user := map[string]any{
		"secrets":  map[string]any{"existingSecret": "runtime-secret", "model": "chosen-model"},
		"extraEnv": []any{map[string]any{"name": "EXAMPLE_SETTING", "value": "allowed"}},
	}
	prepared, err := PrepareSandboxChart(testChart(t), input, user, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if object(prepared.Values["secrets"])["model"] != "chosen-model" ||
		object(prepared.Values["secrets"])["provider"] != "openai" {
		t.Fatal("allowed model inputs were overwritten")
	}
	for _, malicious := range []map[string]any{
		{"secrets": map[string]any{"provider": "anthropic"}},
		{"podAnnotations": map[string]any{"sidecar.istio.io/inject": "true"}},
		{"resources": map[string]any{}}, {"secrets": map[string]any{"existingSecret": "foreign"}},
		{"dnsPolicy": "Default"},
		{"dnsConfig": map[string]any{"nameservers": []any{"192.0.2.53"}}},
	} {
		if _, err := PrepareSandboxChart(testChart(t), input, malicious, bundle); err == nil {
			t.Fatal("protected original override was silently overwritten instead of rejected")
		}
	}
	prepared.Values["replicaCount"] = 1
	if _, err := prepared.SandboxManifest(); err == nil {
		t.Fatal("mutated sealed values were accepted")
	}
	input[w.ValuesKey] = map[string]any{"contractVersion": "v1"}
	if _, err := PrepareChart(testChart(t), input); err == nil {
		t.Fatal("caller supplied protected chart values through the legacy renderer")
	}
}

func TestSandboxFinalPodRejectsInjectedPaths(t *testing.T) {
	_, _, expected := sandboxRender(t, "openclaw", true, true)
	cases := map[string]func(*corev1.Pod){
		"main privilege":   func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) },
		"init privilege":   func(p *corev1.Pod) { p.Spec.InitContainers[0].SecurityContext.AllowPrivilegeEscalation = ptr.To(true) },
		"helper privilege": func(p *corev1.Pod) { p.Spec.Containers[1].SecurityContext.RunAsUser = ptr.To(int64(0)) },
		"root filesystem":  func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = ptr.To(false) },
		"capabilities": func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"SYS_ADMIN"}
		},
		"container resources": func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("100")
		},
		"init resources": func(p *corev1.Pod) {
			delete(p.Spec.InitContainers[0].Resources.Limits, corev1.ResourceEphemeralStorage)
		},
		"helper resources": func(p *corev1.Pod) { p.Spec.Containers[1].Resources.Requests = nil },
		"Pod resources":    func(p *corev1.Pod) { p.Spec.Resources = nil },
		"overhead":         func(p *corev1.Pod) { p.Spec.Overhead = nil },
		"host network":     func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"host PID":         func(p *corev1.Pod) { p.Spec.HostPID = true },
		"host IPC":         func(p *corev1.Pod) { p.Spec.HostIPC = true },
		"cluster DNS":      func(p *corev1.Pod) { p.Spec.DNSPolicy = corev1.DNSClusterFirst },
		"node DNS":         func(p *corev1.Pod) { p.Spec.DNSPolicy = corev1.DNSDefault },
		"missing DNS":      func(p *corev1.Pod) { p.Spec.DNSConfig = nil },
		"DNS server":       func(p *corev1.Pod) { p.Spec.DNSConfig.Nameservers[0] = "192.0.2.53" },
		"fallback DNS": func(p *corev1.Pod) {
			p.Spec.DNSConfig.Nameservers = append(p.Spec.DNSConfig.Nameservers, "192.0.2.53")
		},
		"DNS search": func(p *corev1.Pod) { p.Spec.DNSConfig.Searches = []string{"example.com"} },
		"DNS options": func(p *corev1.Pod) {
			p.Spec.DNSConfig.Options = []corev1.PodDNSConfigOption{{Name: "ndots", Value: ptr.To("2")}}
		},
		"host alias": func(p *corev1.Pod) {
			p.Spec.HostAliases = []corev1.HostAlias{{IP: "127.0.0.1", Hostnames: []string{"example.com"}}}
		},
		"host port":         func(p *corev1.Pod) { p.Spec.Containers[0].Ports[0].HostPort = 1234 },
		"shared process":    func(p *corev1.Pod) { p.Spec.ShareProcessNamespace = ptr.To(true) },
		"runtime":           func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil },
		"direct scheduling": func(p *corev1.Pod) { p.Spec.NodeName = "unapproved" },
		"sidecar":           func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, *p.Spec.Containers[1].DeepCopy()) },
		"init sidecar": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		},
		"ephemeral container": func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "example.com/debug:v1"}}}
		},
		"mount injection": func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/etc"})
		},
		"skills writable": func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[3].ReadOnly = false },
		"home replacement": func(p *corev1.Pod) {
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "data" {
					p.Spec.Volumes[i].PersistentVolumeClaim.ClaimName = "foreign"
				}
			}
		},
		"shared desktop scratch": func(p *corev1.Pod) {
			for i := range p.Spec.Containers[1].VolumeMounts {
				if p.Spec.Containers[1].VolumeMounts[i].MountPath == "/tmp" {
					p.Spec.Containers[1].VolumeMounts[i].Name = "tmp"
				}
			}
		},
		"unbounded desktop scratch": func(p *corev1.Pod) {
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "tmp-desktop" {
					p.Spec.Volumes[i].EmptyDir.SizeLimit = nil
				}
			}
		},
		"oversized desktop scratch": func(p *corev1.Pod) {
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "tmp-desktop" {
					p.Spec.Volumes[i].EmptyDir.SizeLimit = ptr.To(resource.MustParse("2Gi"))
				}
			}
		},
		"host volume": func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		},
		"identity automount": func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) },
		"identity account":   func(p *corev1.Pod) { p.Spec.ServiceAccountName = "privileged" },
		"environment": func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "HOME", Value: "/root"})
		},
		"secret reference": func(p *corev1.Pod) {
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "secrets" {
					p.Spec.Volumes[i].Secret.SecretName = "foreign"
				}
			}
		},
		"helper classification": func(p *corev1.Pod) { p.Annotations[w.KindAnnotation] = "copilot-helper" },
		"owner UID":             func(p *corev1.Pod) { p.OwnerReferences[0].UID = "foreign" },
		"revision":              func(p *corev1.Pod) { p.Annotations[w.RevisionAnnotation] = "stale" },
		"selector":              func(p *corev1.Pod) { delete(p.Labels, "sandbox.cetirun.microsoft.com/agent-uid") },
		"injection annotation":  func(p *corev1.Pod) { p.Annotations["sidecar.istio.io/inject"] = "true" },
		"toleration": func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			pod := testfixture.Pod(expected)
			change(pod)
			if err := w.ValidatePod(pod, expected); err == nil {
				t.Fatal("final Pod bypass accepted")
			}
		})
	}
}

func TestSandboxFinalPodRequiresProtectedExpectedDNS(t *testing.T) {
	_, _, expected := sandboxRender(t, "openclaw", true, true)
	for name, mutate := range map[string]func(*corev1.PodSpec){
		"default policy": func(spec *corev1.PodSpec) { spec.DNSPolicy = corev1.DNSClusterFirst },
		"missing config": func(spec *corev1.PodSpec) { spec.DNSConfig = nil },
		"empty servers":  func(spec *corev1.PodSpec) { spec.DNSConfig.Nameservers = nil },
		"invalid server": func(spec *corev1.PodSpec) { spec.DNSConfig.Nameservers = []string{"dns.example.com"} },
		"search domains": func(spec *corev1.PodSpec) { spec.DNSConfig.Searches = []string{"example.com"} },
		"options": func(spec *corev1.PodSpec) {
			spec.DNSConfig.Options = []corev1.PodDNSConfigOption{{Name: "ndots", Value: ptr.To("2")}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := expected
			changed.Template = *expected.Template.DeepCopy()
			mutate(&changed.Template.Spec)
			if err := w.ValidatePod(testfixture.Pod(changed), changed); err == nil {
				t.Fatal("matching an unprotected DNS expectation allowed a Pod")
			}
		})
	}
}

func TestSandboxRejectsUnsupportedEnvironmentSourcesAtBothBoundaries(t *testing.T) {
	prepared, bundle, _ := sandboxRender(t, "openclaw", true, true)
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []*corev1.EnvVarSource{
		{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "unapproved"}, Key: "example"}},
		{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}},
		{},
	} {
		changed := template.DeepCopy()
		changed.Spec.Containers[0].Env = append(changed.Spec.Containers[0].Env, corev1.EnvVar{Name: "UNSUPPORTED", ValueFrom: source})
		if err := bundle.ValidateTemplate(changed, prepared.Agent.Image); err == nil {
			t.Fatal("rendered template accepted an unsupported environment source")
		}
	}
	input := sampleInput("openclaw", "openai", false)
	input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	for _, reference := range []map[string]any{
		{"configMapKeyRef": map[string]any{"name": "unapproved", "key": "example"}},
		{"fieldRef": map[string]any{"fieldPath": "metadata.name"}},
		{"resourceFieldRef": map[string]any{"resource": "limits.cpu"}},
	} {
		user := map[string]any{"extraEnv": []any{map[string]any{"name": "UNSUPPORTED", "valueFrom": reference}}}
		if _, err := PrepareSandboxChart(testChart(t), input, user, bundle); err == nil {
			t.Fatal("tenant values accepted an unsupported environment source")
		}
	}
	for _, user := range []map[string]any{
		{"tailscale": map[string]any{"enabled": true}},
		{"sidecars": []any{map[string]any{"name": "unapproved", "image": "example.com/helper:v1"}}},
		{"initContainers": []any{map[string]any{"name": "unapproved", "image": "example.com/helper:v1"}}},
		{"extraVolumes": []any{map[string]any{"name": "unapproved", "hostPath": map[string]any{"path": "/example"}}}},
	} {
		if _, err := PrepareSandboxChart(testChart(t), input, user, bundle); err == nil {
			t.Fatal("tenant chart extension bypassed the protected values allow-list")
		}
	}
}

func TestSandboxCompilerIdentityProjectionRendering(t *testing.T) {
	for _, typ := range []string{"openclaw"} {
		t.Run(typ, func(t *testing.T) {
			fixture := gatewayHeldFixture(typ)
			fixture.Policy.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName:  "sandbox-example",
				RoleBindings:        []api.AgentPolicyRoleBinding{{Namespace: "approved-tools", ClusterRoleName: "example-reader"}},
				ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: "example-cluster-reader"}},
			}
			fixture.Policy.Spec.EntraID.Enabled = true
			resources := sandbox.Resources{
				Sandboxes:     map[string]*api.Sandbox{fixture.Sandbox.Name: fixture.Sandbox},
				AgentPolicies: map[string]*api.AgentPolicy{fixture.Policy.Name: fixture.Policy},
			}
			record, err := sandboxbinding.Snapshot(fixture.Agent, fixture.Team, resources)
			if err != nil {
				t.Fatal(err)
			}
			fixture.Agent.Annotations[sandboxbinding.AuthorizationAnnotation] = record
			binding, err := sandboxbinding.AuthorizedBinding(fixture.Agent)
			if err != nil {
				t.Fatal(err)
			}
			caps := fixture.Input.Resolution.Capabilities
			caps.EntraID, caps.KubernetesAccess = true, true
			resolution, err := sandbox.Resolve(sandbox.ResolveInput{
				Agent: fixture.Agent, Team: fixture.Team, Resources: resources, AuthorizedBinding: binding,
				Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {Capabilities: caps, Available: true}},
			})
			if err != nil {
				t.Fatal(err)
			}
			platform := sandboxpolicy.Platform{
				Adapter: sandboxpolicy.AdapterRegistration{
					Name: sandboxpolicy.AzureCNIOverlayCilium, Revision: "synthetic-only", Qualified: true, Available: true,
				},
				Topology: sandboxpolicy.Topology{
					Revision: "synthetic-only", ManagedPeersComplete: true,
					DNSResolverIPs: []string{"10.96.0.54", "10.96.0.53"},
					DNS: []sandboxpolicy.Peer{{Name: "dns", Namespace: "platform-dns", UID: "dns-uid",
						IPs: []string{"10.96.0.54", "10.96.0.53", "10.244.0.11", "10.244.0.12", "10.244.0.13"}}},
					Gateway: []sandboxpolicy.Destination{{
						Peer:  sandboxpolicy.Peer{Name: "gateway", Namespace: "platform-gateway", UID: "gateway-uid", IPs: []string{"10.96.0.20"}},
						Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}},
					}},
					KubernetesAPI: []sandboxpolicy.Destination{{
						Peer:  sandboxpolicy.Peer{Name: "api", UID: "api-uid", IPs: []string{"10.96.0.1"}},
						Ports: []api.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}},
					}},
					PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"}, NodeCIDRs: []string{"10.0.0.0/24"},
					ManagementCIDRs: []string{"192.168.0.0/24"}, MetadataCIDRs: []string{"169.254.169.254/32"},
				},
				KubernetesAudience: "https://kubernetes.default.svc", TokenExpirationSeconds: 600,
				Identities: []sandboxpolicy.ApprovedIdentity{{
					AgentUID: binding.AgentUID, Namespace: fixture.Input.Namespace.Name, ServiceAccountName: "sandbox-example",
					Revision: "synthetic-only", Kubernetes: true,
					Entra: &sandboxpolicy.EntraIdentity{
						ClientID: "00000000-0000-0000-0000-000000000001", TenantID: "00000000-0000-0000-0000-000000000002",
					},
				}},
			}
			for _, grant := range []struct{ name, namespace string }{
				{"example-reader", "approved-tools"}, {"example-cluster-reader", ""},
			} {
				platform.Grants = append(platform.Grants, sandboxpolicy.ApprovedGrant{
					PolicyUID: fixture.Policy.UID, Namespace: grant.namespace, RoleUID: "role-uid", RoleResourceVersion: "1", Revision: "synthetic-only",
					RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: grant.name},
					Rules:   []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
				})
			}
			plan, err := sandboxpolicy.Compile(sandboxpolicy.CompileInput{
				Resolution: *resolution, AgentName: fixture.Input.AgentName, Namespace: fixture.Input.Namespace.Name, Platform: platform,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.Input.Resolution = resolution
			fixture.Input.Controls, err = w.ControlsFromPlan(*plan)
			if err != nil {
				t.Fatal(err)
			}
			testCompilerResourceMutations(t, fixture.Input, plan)
			bundle, err := w.Build(fixture.Input)
			if err != nil {
				t.Fatal(err)
			}
			input := sampleInput(typ, "openai", false)
			input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
			prepared, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			if template.Spec.DNSPolicy != corev1.DNSNone || template.Spec.DNSConfig == nil ||
				!slices.Equal(template.Spec.DNSConfig.Nameservers, plan.Network.DNSIPs) ||
				!slices.Equal(template.Spec.DNSConfig.Nameservers, []string{"10.96.0.53", "10.96.0.54"}) ||
				len(template.Spec.DNSConfig.Searches) != 0 || len(template.Spec.DNSConfig.Options) != 0 {
				t.Fatal("Pod DNS differs from the compiler's effective trusted topology")
			}
			if template.Annotations[w.RevisionAnnotation] != plan.Revision || plan.Revision == resolution.Revision ||
				template.Labels[sandboxpolicy.AgentUIDLabel] != string(plan.Binding.AgentUID) {
				t.Fatal("effective revision or Agent UID selector was lost")
			}
			audiences := map[string]string{}
			for _, volume := range template.Spec.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, projection := range volume.Projected.Sources {
					if projection.ServiceAccountToken != nil {
						audiences[volume.Name] = projection.ServiceAccountToken.Audience
					}
				}
			}
			if len(audiences) != 2 || audiences["sandbox-kubernetes-api"] != platform.KubernetesAudience ||
				audiences["sandbox-entra-exchange"] != sandboxpolicy.EntraAudience || *template.Spec.AutomountServiceAccountToken {
				t.Fatal("independent compiler identity projections were lost or merged")
			}
			manifest, err := prepared.SandboxManifest()
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{`"kind":"CiliumNetworkPolicy"`, `"kind":"ClusterRoleBinding"`, `"namespace":"approved-tools"`, `"port":"8443"`} {
				if !strings.Contains(string(manifest), expected) {
					t.Fatalf("compiler resource was not rendered unchanged: %s", expected)
				}
			}
			workload := testfixture.Workload(fixture.Input, *template)
			expectation, err := bundle.Expect(*template, workload, nil)
			if err != nil {
				t.Fatal(err)
			}
			expectation.SinglePod = true
			pod := testfixture.Pod(expectation)
			if err := w.ValidatePod(pod, expectation); err != nil {
				t.Fatal(err)
			}
			for i := range pod.Spec.Volumes {
				if pod.Spec.Volumes[i].Name == "sandbox-entra-exchange" {
					pod.Spec.Volumes[i].Projected.Sources[0].ServiceAccountToken.Audience = platform.KubernetesAudience
				}
			}
			if err := w.ValidatePod(pod, expectation); err == nil {
				t.Fatal("identity audience substitution was accepted")
			}
		})
	}
}

func testCompilerResourceMutations(t *testing.T, input w.Input, plan *sandboxpolicy.Plan) {
	t.Helper()
	for _, test := range []struct {
		name, kind string
		mutate     func(*unstructured.Unstructured)
	}{
		{"RoleBinding scope", "RoleBinding", func(o *unstructured.Unstructured) { o.SetNamespace("unapproved-tools") }},
		{"RoleBinding promoted to cluster", "RoleBinding", func(o *unstructured.Unstructured) {
			o.SetKind("ClusterRoleBinding")
			o.SetNamespace("")
		}},
		{"ClusterRoleBinding namespace", "ClusterRoleBinding", func(o *unstructured.Unstructured) { o.SetNamespace(input.Namespace.Name) }},
		{"ClusterRoleBinding role", "ClusterRoleBinding", func(o *unstructured.Unstructured) {
			o.Object["roleRef"].(map[string]any)["name"] = "example-reader"
		}},
		{"RoleBinding role", "RoleBinding", func(o *unstructured.Unstructured) {
			o.Object["roleRef"].(map[string]any)["name"] = "unapproved-role"
		}},
		{"RoleBinding kind", "RoleBinding", func(o *unstructured.Unstructured) {
			o.Object["roleRef"].(map[string]any)["kind"] = "Role"
		}},
		{"RoleBinding group", "RoleBinding", func(o *unstructured.Unstructured) {
			o.Object["roleRef"].(map[string]any)["apiGroup"] = "example.com"
		}},
		{"subject account", "RoleBinding", func(o *unstructured.Unstructured) {
			o.Object["subjects"].([]any)[0].(map[string]any)["name"] = "foreign"
		}},
		{"subject namespace", "ClusterRoleBinding", func(o *unstructured.Unstructured) {
			o.Object["subjects"].([]any)[0].(map[string]any)["namespace"] = "foreign"
		}},
		{"subject group", "RoleBinding", func(o *unstructured.Unstructured) {
			o.Object["subjects"].([]any)[0].(map[string]any)["apiGroup"] = "rbac.authorization.k8s.io"
		}},
		{"additional subject", "RoleBinding", func(o *unstructured.Unstructured) {
			o.Object["subjects"] = append(o.Object["subjects"].([]any), map[string]any{"kind": "User", "name": "example-user"})
		}},
		{"missing grant revision", "RoleBinding", func(o *unstructured.Unstructured) {
			annotations := o.GetAnnotations()
			delete(annotations, sandboxpolicy.GrantRevisionAnnotation)
			o.SetAnnotations(annotations)
		}},
		{"ServiceAccount name", "ServiceAccount", func(o *unstructured.Unstructured) { o.SetName("foreign") }},
		{"ServiceAccount token", "ServiceAccount", func(o *unstructured.Unstructured) { o.Object["automountServiceAccountToken"] = true }},
		{"ServiceAccount credentials", "ServiceAccount", func(o *unstructured.Unstructured) {
			o.Object["secrets"] = []any{map[string]any{"name": "unexpected-token"}}
		}},
		{"network scope", "CiliumNetworkPolicy", func(o *unstructured.Unstructured) { o.SetNamespace("foreign") }},
		{"network selector", "CiliumNetworkPolicy", func(o *unstructured.Unstructured) {
			o.Object["spec"].(map[string]any)["endpointSelector"] = map[string]any{}
		}},
	} {
		t.Run("reject "+test.name, func(t *testing.T) {
			changed := input
			var err error
			changed.Controls, err = w.ControlsFromPlan(*plan)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for i := range changed.Controls.Objects {
				object := &changed.Controls.Objects[i]
				if object.GetKind() == test.kind {
					test.mutate(object)
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("compiled fixture omitted %s", test.kind)
			}
			if _, err := w.Build(changed); err == nil {
				t.Fatal("foreign compiler resource, privilege or scope was accepted")
			}
		})
	}
	for _, kind := range []string{"CiliumNetworkPolicy", "ServiceAccount", "RoleBinding", "ClusterRoleBinding"} {
		t.Run("missing "+kind, func(t *testing.T) {
			changed := input
			var err error
			changed.Controls, err = w.ControlsFromPlan(*plan)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for i, object := range changed.Controls.Objects {
				if object.GetKind() == kind {
					changed.Controls.Objects = append(changed.Controls.Objects[:i], changed.Controls.Objects[i+1:]...)
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("compiled fixture omitted %s", kind)
			}
			if _, err := w.Build(changed); err == nil {
				t.Fatal("incomplete compiler resource inventory was accepted")
			}
		})
	}
}

func TestSandboxChartCannotInventHelperOrMount(t *testing.T) {
	fixture := gatewayHeldFixture("openclaw")
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput("openclaw", "openai", false)
	input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	for _, change := range [][2]string{
		{"- name: logger", "- name: copilot-helper"},
		{"mountPath: /home/vibe\n", "mountPath: /root\n"},
		{"automountServiceAccountToken: {{ include \"cetirun.automountServiceAccountToken\" . }}", "automountServiceAccountToken: true"},
		{"dnsPolicy: None", "dnsPolicy: ClusterFirst"},
		{"- name: tmp-desktop\n      mountPath: /tmp", "- name: tmp\n      mountPath: /tmp"},
		{"sizeLimit: {{ .Values.sandboxWorkload.scratchSizeLimit | quote }}", "sizeLimit: 2Gi"},
		{"{{- toYaml .Values.sandboxWorkload.dnsConfig | nindent 2 }}", "{{- toYaml (dict \"nameservers\" (list \"192.0.2.53\")) | nindent 2 }}"},
	} {
		chrt := testChart(t)
		changed := false
		for _, file := range chrt.Templates {
			if strings.Contains(string(file.Data), change[0]) {
				file.Data = []byte(strings.ReplaceAll(string(file.Data), change[0], change[1]))
				changed = true
			}
		}
		if !changed {
			t.Fatal("mutation did not match a template")
		}
		if _, err := PrepareSandboxChart(chrt, input, nil, bundle); err == nil {
			t.Fatal("modified protected workload was accepted")
		}
	}
}
