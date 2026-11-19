// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestNativeProxyTrustRequiresBoundedOpenClawIngress(t *testing.T) {
	fixture := func() w.Input {
		input := testfixture.New("openclaw").Input
		input.Resolution.Capabilities.AuthenticatedIngress = true
		input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
		input.AuthorizedSecrets = append(input.AuthorizedSecrets, input.GatewayKey.Name)
		input.Controls.IngressMode, input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated, api.AgentPolicyIngressAuthenticated
		input.Controls.TrustedProxyIPs = []string{"192.0.2.10", "2001:db8::10"}
		return input
	}
	input := fixture()
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"secrets":   map[string]any{"existingSecret": "runtime-secret"},
		"aiGateway": map[string]any{"enabled": true, "keySecretName": input.GatewayKey.Name, "keySecretKey": "api-key"},
		w.ValuesKey: map[string]any{"trustedProxyIPs": []any{"0.0.0.0/0"}},
	}
	projected, err := bundle.Apply(values)
	if err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(projected[w.ValuesKey].(map[string]any)["trustedProxyIPs"], []any{"192.0.2.10", "2001:db8::10"}) {
		t.Fatal("protected proxy trust was lost or merged with a broader value")
	}
	input.Controls.TrustedProxyIPs[0] = "192.0.2.11"
	if !slices.Equal(bundle.Identity().Controls.TrustedProxyIPs, []string{"192.0.2.10", "2001:db8::10"}) {
		t.Fatal("bundle aliases caller proxy trust")
	}
	input = fixture()
	input.Controls.TrustedProxyIPs = []string{"10.244.0.0/16", "fd00:244::/64"}
	cidrBundle, err := w.Build(input)
	if err != nil {
		t.Fatal("compiler-selected bounded proxy CIDRs were rejected:", err)
	}
	cidrValues, err := cidrBundle.Apply(values)
	if err != nil || !equality.Semantic.DeepEqual(cidrValues[w.ValuesKey].(map[string]any)["trustedProxyIPs"], []any{"10.244.0.0/16", "fd00:244::/64"}) {
		t.Fatal("protected proxy CIDRs were lost or broadened:", err)
	}
	input.Controls.TrustedProxyIPs[0] = "10.244.1.0/24"
	if !slices.Equal(cidrBundle.Identity().Controls.TrustedProxyIPs, []string{"10.244.0.0/16", "fd00:244::/64"}) {
		t.Fatal("bundle aliases caller proxy CIDRs")
	}
	for name, mutate := range map[string]func(*w.Input){
		"missing proxies":   func(i *w.Input) { i.Controls.TrustedProxyIPs = nil },
		"CIDR":              func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"0.0.0.0/0"} },
		"IPv6 root CIDR":    func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"::/0"} },
		"loopback CIDR":     func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"127.0.0.0/8"} },
		"link-local CIDR":   func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"fe80::/64"} },
		"multicast CIDR":    func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"224.0.0.0/4"} },
		"host bits in CIDR": func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"10.244.0.1/16"} },
		"unspecified":       func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"::"} },
		"loopback":          func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"127.0.0.6"} },
		"noncanonical":      func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"2001:0db8::10"} },
		"duplicates":        func(i *w.Input) { i.Controls.TrustedProxyIPs = []string{"192.0.2.10", "192.0.2.10"} },
		"unsorted":          func(i *w.Input) { slices.Reverse(i.Controls.TrustedProxyIPs) },
		"no native Gateway": func(i *w.Input) { i.GatewayKey = nil },
		"disabled ingress": func(i *w.Input) {
			i.Controls.IngressMode, i.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressDisabled, api.AgentPolicyIngressDisabled
		},
		"Hermes": func(i *w.Input) { i.AgentType, i.Helpers.Doctor = "hermes", false },
	} {
		t.Run(name, func(t *testing.T) {
			input := fixture()
			mutate(&input)
			if _, err := w.Build(input); err == nil {
				t.Fatal("invalid or non-native proxy trust accepted")
			}
		})
	}
	for _, user := range []map[string]any{
		{w.ValuesKey: map[string]any{"trustedProxyIPs": []any{"192.0.2.10"}}},
		{"openclaw": map[string]any{"config": map[string]any{"gateway": map[string]any{"trustedProxies": []any{"192.0.2.10"}}}}},
	} {
		if err := w.ValidateUserValues(user, input.AuthorizedSecrets); err == nil {
			t.Fatal("an Agent supplied its own proxy trust")
		}
	}
	raw, err := json.Marshal(testfixture.New("openclaw").Input.Controls)
	if err != nil || strings.Contains(string(raw), "TrustedProxyIPs") {
		t.Fatal("default non-native controls gained serialized proxy trust:", err)
	}
}

func TestBundleRejectsUnboundOrIncompatibleInputs(t *testing.T) {
	cases := map[string]func(*w.Input){
		"missing resolution":      func(i *w.Input) { i.Resolution = nil },
		"missing binding":         func(i *w.Input) { i.Resolution.Binding.AgentUID = "" },
		"Custom cannot fall back": func(i *w.Input) { i.AgentType = "custom" },
		"foreign controls":        func(i *w.Input) { i.Controls.Binding.AgentUID = "foreign" },
		"stale controls":          func(i *w.Input) { i.Controls.ResolutionRevision = "old" },
		"foreign home":            func(i *w.Input) { i.Home.Annotations[w.AgentUIDAnnotation] = "foreign" },
		"missing existing home":   func(i *w.Input) { i.Home = nil },
		"replaced home":           func(i *w.Input) { i.Home.UID = "" },
		"shrinking home":          func(i *w.Input) { i.Resolution.Sandbox.Storage.Home.Size = resource.MustParse("1Gi") },
		"home access mode":        func(i *w.Input) { i.Home.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany} },
		"no runtime":              func(i *w.Input) { i.RuntimeClass = nil },
		"wrong runtime":           func(i *w.Input) { i.RuntimeClass.Name = "not-approved" },
		"shared home and skills":  func(i *w.Input) { i.Skills = i.Home.DeepCopy() },
		"no secret authority":     func(i *w.Input) { i.AuthorizedSecrets = nil },
		"no network outputs":      func(i *w.Input) { i.Controls.Objects = nil },
		"wrong selector":          func(i *w.Input) { i.Controls.PodLabels = nil },
		"forged helper":           func(i *w.Input) { i.AgentType = "hermes" },
		"missing DNS":             func(i *w.Input) { i.Controls.DNSIPs = nil },
		"DNS hostname":            func(i *w.Input) { i.Controls.DNSIPs = []string{"dns.example.com"} },
		"DNS port":                func(i *w.Input) { i.Controls.DNSIPs = []string{"192.0.2.53:53"} },
		"DNS subnet":              func(i *w.Input) { i.Controls.DNSIPs = []string{"192.0.2.0/24"} },
		"scoped DNS":              func(i *w.Input) { i.Controls.DNSIPs = []string{"fe80::53%eth0"} },
		"noncanonical DNS":        func(i *w.Input) { i.Controls.DNSIPs = []string{"2001:0db8::53"} },
		"duplicate DNS":           func(i *w.Input) { i.Controls.DNSIPs = []string{"10.96.0.10", "10.96.0.10"} },
		"too many DNS servers": func(i *w.Input) {
			i.Controls.DNSIPs = []string{"10.96.0.10", "10.96.0.11", "10.96.0.12", "10.96.0.13"}
		},
		"missing ingress mode":        func(i *w.Input) { i.Controls.IngressMode = "" },
		"foreign ingress mode":        func(i *w.Input) { i.Controls.IngressMode = api.AgentPolicyIngressAuthenticated },
		"ordinary dedicated identity": func(i *w.Input) { i.Controls.ServiceAccountName = "unexpected" },
		"ordinary identity environment": func(i *w.Input) {
			i.Controls.Environment = []corev1.EnvVar{{Name: "AZURE_CLIENT_ID", Value: "unexpected"}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := testfixture.New("openclaw")
			change(&fixture.Input)
			if _, err := w.Build(fixture.Input); err == nil {
				t.Fatal("unsafe or incomplete bundle accepted")
			}
		})
	}
}

func TestBundleRejectsUnexpectedCompilerResources(t *testing.T) {
	for _, kind := range []struct{ apiVersion, kind string }{
		{"rbac.authorization.k8s.io/v1", "Role"},
		{"rbac.authorization.k8s.io/v1", "ClusterRole"},
		{"v1", "ConfigMap"},
		{"networking.k8s.io/v1", "NetworkPolicy"},
	} {
		t.Run(kind.kind, func(t *testing.T) {
			fixture := testfixture.New("hermes")
			extra := fixture.Input.Controls.Objects[0].DeepCopy()
			extra.SetAPIVersion(kind.apiVersion)
			extra.SetKind(kind.kind)
			extra.SetName("unexpected-shared-resource")
			fixture.Input.Controls.Objects = append(fixture.Input.Controls.Objects, *extra)
			if _, err := w.Build(fixture.Input); err == nil {
				t.Fatal("unsupported compiler resource was accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*metav1.OwnerReference){
		"controller flag": func(owner *metav1.OwnerReference) { owner.Controller = ptr.To(false) },
		"deletion flag":   func(owner *metav1.OwnerReference) { owner.BlockOwnerDeletion = ptr.To(false) },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := testfixture.New("hermes")
			owners := fixture.Input.Controls.Objects[0].GetOwnerReferences()
			mutate(&owners[0])
			fixture.Input.Controls.Objects[0].SetOwnerReferences(owners)
			if _, err := w.Build(fixture.Input); err == nil {
				t.Fatal("noncanonical compiler ownership was accepted")
			}
		})
	}
	for _, annotation := range []string{sandboxpolicy.BindingAnnotation, sandboxpolicy.RevisionAnnotation} {
		t.Run(annotation, func(t *testing.T) {
			fixture := testfixture.New("hermes")
			annotations := fixture.Input.Controls.Objects[0].GetAnnotations()
			annotations[annotation] = "foreign"
			fixture.Input.Controls.Objects[0].SetAnnotations(annotations)
			if _, err := w.Build(fixture.Input); err == nil {
				t.Fatal("foreign compiler binding metadata was accepted")
			}
		})
	}
}

func TestProtectedBundleIsLastAndImmutable(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		fixture := testfixture.New("hermes")
		fixture.Input.Resolution.AgentPolicy.Security.ReadOnlyRootFilesystem = ptr.To(readOnly)
		bundle, err := w.Build(fixture.Input)
		if err != nil {
			t.Fatal(err)
		}
		values := map[string]any{
			"secrets":          map[string]any{"existingSecret": "runtime-secret", "provider": "openai", "model": "chosen-model"},
			"runtimeClassName": "unsafe", "replicaCount": 9,
			"resources": map[string]any{}, "securityContext": map[string]any{"privileged": true},
			"sidecarScope":  map[string]any{"enabled": true, "additionalEgressHosts": []any{"*/*"}},
			"networkPolicy": map[string]any{"enabled": false},
			"persistence":   map[string]any{"existingClaim": "foreign"},
			"extraVolumes":  []any{map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}}},
			w.ValuesKey:     map[string]any{"dnsConfig": map[string]any{"nameservers": []any{"192.0.2.53"}}},
		}
		protected, err := bundle.Apply(values)
		if err != nil {
			t.Fatal(err)
		}
		if values["runtimeClassName"] != "unsafe" || protected["runtimeClassName"] != "example-kata" ||
			protected["replicaCount"] != 0 || protected["networkPolicy"].(map[string]any)["enabled"] != true ||
			protected["sidecarScope"].(map[string]any)["enabled"] != false ||
			protected["podAnnotations"].(map[string]any)["sidecar.istio.io/inject"] != "false" {
			t.Fatal("protected bundle did not replace inputs without mutating them")
		}
		security := protected["securityContext"].(map[string]any)
		if security["readOnlyRootFilesystem"] != readOnly || security["privileged"] != false ||
			security["allowPrivilegeEscalation"] != false || security["runAsNonRoot"] != true {
			t.Fatal("explicit filesystem false relaxed other security controls")
		}
		if protected["secrets"].(map[string]any)["model"] != "chosen-model" ||
			protected["secrets"].(map[string]any)["existingSecret"] != "runtime-secret" {
			t.Fatal("allowed model or Secret input was lost")
		}
		dns := protected[w.ValuesKey].(map[string]any)["dnsConfig"].(map[string]any)
		if !equality.Semantic.DeepEqual(dns, map[string]any{"nameservers": []any{"10.96.0.10"}}) {
			t.Fatal("trusted DNS did not replace the earlier merged values")
		}
		fixture.Input.Home.Name = "mutated"
		fixture.Input.Controls.PodLabels["unexpected"] = "value"
		fixture.Input.Controls.DNSIPs[0] = "192.0.2.53"
		bundle.DNSConfig().Nameservers[0] = "192.0.2.54"
		if bundle.Identity().Home.Name != "data-example-0" || len(bundle.Identity().Controls.PodLabels) != 1 {
			t.Fatal("bundle aliases caller input")
		}
		if !equality.Semantic.DeepEqual(bundle.DNSConfig(), &corev1.PodDNSConfig{Nameservers: []string{"10.96.0.10"}}) {
			t.Fatal("bundle aliases DNS input or returned configuration")
		}
	}
}

func TestProtectedOverridesBeforeMerge(t *testing.T) {
	for _, field := range []string{
		"runtime", "sandboxWorkload", "runtimeClassName", "securityContext", "podSecurityContext", "resources",
		"podAnnotations", "podLabels", "sidecarScope", "networkPolicy", "serviceAccount", "azureWorkloadIdentity", "agentIdentity",
		"persistence", "skills", "replicaCount", "autoscaling", "nodeSelector", "tolerations", "affinity", "topologySpreadConstraints",
		"initContainers", "sidecars", "extraContainers", "extraVolumes", "extraVolumeMounts", "extraEnvFrom", "ingress",
		"imagePullSecrets", "updateStrategy", "desktop", "logger", "doctor", "openclaw", "hermes", "unknown",
		"dnsPolicy", "dnsConfig",
	} {
		t.Run(field, func(t *testing.T) {
			if err := w.ValidateUserValues(map[string]any{field: nil}, []string{"runtime-secret"}); err == nil {
				t.Fatal("protected input accepted")
			}
		})
	}
	for _, name := range []string{"HOME", "PATH", "NODE_OPTIONS", "LD_PRELOAD", "KUBECONFIG", "AZURE_CLIENT_ID", "CETIRUN_AGENT_BROKER_URL", "OPENCLAW_STATE_DIR", "HERMES_HOME"} {
		if err := w.ValidateUserValues(map[string]any{"extraEnv": []any{map[string]any{"name": name, "value": "forged"}}}, nil); err == nil {
			t.Fatalf("reserved environment %s accepted", name)
		}
	}
	for _, input := range []map[string]any{
		{"secrets": map[string]any{"existingSecret": "foreign"}},
		{"secrets": map[string]any{"apiBase": "https://example.com"}},
		{"extraEnv": []any{map[string]any{"name": "OK", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "foreign", "key": "KEY"}}}}},
		{"extraEnv": []any{map[string]any{"name": "OK", "injected": true}}},
	} {
		if err := w.ValidateUserValues(input, []string{"runtime-secret"}); err == nil {
			t.Fatal("unauthorized override accepted")
		}
	}
	allowed := map[string]any{
		"secrets": map[string]any{"existingSecret": "runtime-secret", "model": "chosen-model"},
		"extraEnv": []any{
			map[string]any{"name": "EXAMPLE_SETTING", "value": "value"},
			map[string]any{"name": "EXAMPLE_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "runtime-secret", "key": "EXAMPLE_KEY"}}},
		},
	}
	if err := w.ValidateUserValues(allowed, []string{"runtime-secret"}); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayProviderOverridesAreRejectedBeforeMerge(t *testing.T) {
	for name, validate := range map[string]func(map[string]any, []string) error{
		"native": w.ValidateUserValues, "reconciled": w.ValidateReconciledUserValues,
	} {
		for _, provider := range []string{"github_copilot", "openai", "anthropic"} {
			t.Run(name+"/"+provider, func(t *testing.T) {
				values := map[string]any{"secrets": map[string]any{"provider": provider}}
				if err := validate(values, nil); err == nil || !strings.Contains(err.Error(), "secrets.provider is protected") {
					t.Fatalf("expected a protected provider error before chart rendering, got %v", err)
				}
			})
		}
	}
}

func TestCollectorAnnotationsAreRejectedBeforeMerge(t *testing.T) {
	for name, validate := range map[string]func(map[string]any, []string) error{
		"native": w.ValidateUserValues, "reconciled": w.ValidateReconciledUserValues,
	} {
		for _, key := range []string{"adx-mon/scrape", "adx-mon/log-destination"} {
			values := map[string]any{"podAnnotations": map[string]any{key: "CetiRunLogs:Platform"}}
			if err := validate(values, nil); err == nil {
				t.Errorf("%s values set %s", name, key)
			}
		}
	}
	if err := w.ValidateReconciledUserValues(map[string]any{"podAnnotations": map[string]any{"example.com/team": "a"}}, nil); err != nil {
		t.Fatalf("unrelated annotation rejected: %v", err)
	}
}

func TestResourcesIncludeHelpersInitAndOverhead(t *testing.T) {
	req := func(cpu string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Requests: testfixture.ResourceList(cpu, "1Gi", "1Gi"), Limits: testfixture.ResourceList(cpu, "1Gi", "1Gi")}
	}
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{Resources: req("2")}, {Resources: req("1")}},
		InitContainers: []corev1.Container{
			{Resources: req("1"), RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)},
			{Resources: req("6")},
			{Resources: req("2"), RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)},
			{Resources: req("2")},
		},
	}
	overhead := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
	got := w.AccountResources(spec, overhead)
	cpu := got.Pod.Limits[corev1.ResourceCPU]
	effectiveCPU := got.Effective.Limits[corev1.ResourceCPU]
	storage := got.Effective.Limits[corev1.ResourceEphemeralStorage]
	if cpu.Cmp(resource.MustParse("7")) != 0 || effectiveCPU.Cmp(resource.MustParse("7100m")) != 0 ||
		storage.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Fatalf("wrong effective init/sidecar resource accounting: %+v", got)
	}
	if _, ok := got.Pod.Limits[corev1.ResourceEphemeralStorage]; ok {
		t.Fatal("unsupported ephemeral storage Pod-level limit was emitted")
	}
}

func TestExistingHOMEIsRetainedWithoutClaimTemplateMigration(t *testing.T) {
	fixture := testfixture.New("hermes")
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	before := fixture.Input.Home.DeepCopy()
	existing := testfixture.Workload(fixture.Input, corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
		Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: fixture.Input.Home.Name}},
	}}}})
	if err := bundle.CheckStatefulSetTransition(existing); err != nil {
		t.Fatal(err)
	}
	for _, replicas := range []int32{0, 1} {
		existing.Spec.Replicas = ptr.To(replicas)
		existing.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{*fixture.Input.Home.DeepCopy()}
		if err := bundle.CheckStatefulSetTransition(existing); !errors.Is(err, w.ErrStorageTransitionRequired) {
			t.Fatalf("implicit immutable-claim transition accepted: %v", err)
		}
	}
	if !equality.Semantic.DeepEqual(before, fixture.Input.Home) || existing.Spec.PersistentVolumeClaimRetentionPolicy != nil {
		t.Fatal("rendering changed existing storage ownership or retention")
	}
}
