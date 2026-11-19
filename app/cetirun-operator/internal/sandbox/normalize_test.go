// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func normalizationSandboxFixture() v1alpha1.SandboxSpec {
	resources := v1alpha1.SandboxContainerResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
	}
	return v1alpha1.SandboxSpec{
		Resources: v1alpha1.SandboxResources{Agent: resources, Helpers: *resources.DeepCopy()},
		Storage: v1alpha1.SandboxStorage{
			Home: v1alpha1.SandboxHomeStorage{
				StorageClassName: "sandbox-home", Size: resource.MustParse("20Gi"), AccessMode: corev1.ReadWriteOncePod,
			},
			Scratch: v1alpha1.SandboxScratchStorage{SizeLimit: resource.MustParse("1Gi")},
		},
	}
}

func TestSchemaDefaultsPreserveExplicitValuesAndInputOwnership(t *testing.T) {
	box := normalizationSandboxFixture()
	originalBox := box.DeepCopy()
	defaultedBox := DefaultSandbox(box)
	if defaultedBox.Type != v1alpha1.SandboxKata ||
		defaultedBox.Storage.Home.Size.String() != box.Storage.Home.Size.String() {
		t.Fatal("Sandbox defaulting changed explicit quantity representation")
	}
	defaultedBox.Resources.Agent.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
	if !reflect.DeepEqual(box, *originalBox) {
		t.Fatal("Sandbox defaulting retained caller-owned maps")
	}
	box.Type = v1alpha1.SandboxGVisor
	if DefaultSandbox(box).Type != v1alpha1.SandboxGVisor {
		t.Fatal("Sandbox defaulting replaced an explicit runtime")
	}

	policy := v1alpha1.AgentPolicySpec{
		Security: v1alpha1.AgentPolicySecurity{ReadOnlyRootFilesystem: normalizationBool(false)},
		Network: v1alpha1.AgentPolicyNetwork{
			Ingress:      v1alpha1.AgentPolicyIngress{Mode: v1alpha1.AgentPolicyIngressDisabled},
			BlockedCIDRs: []string{"203.0.113.7/24", "198.51.100.0/24", "203.0.113.7/24"},
			BlockedPorts: []v1alpha1.AgentPolicyPort{
				{Port: 8080, Protocol: corev1.ProtocolUDP}, {Port: 53}, {Port: 8080, Protocol: corev1.ProtocolUDP},
			},
			AllowedInternalCIDRs: []v1alpha1.AgentPolicyCIDRRule{
				{CIDR: "203.0.113.7/24", Ports: []v1alpha1.AgentPolicyPort{{Port: 8443}, {Port: 443, Protocol: corev1.ProtocolSCTP}}},
			},
		},
	}
	original := policy.DeepCopy()
	want := policy.DeepCopy()
	want.Network.BlockedPorts[1].Protocol = corev1.ProtocolTCP
	want.Network.AllowedInternalCIDRs[0].Ports[0].Protocol = corev1.ProtocolTCP
	got := DefaultAgentPolicy(policy)
	if !reflect.DeepEqual(got, *want) || !reflect.DeepEqual(got, DefaultAgentPolicy(got)) {
		t.Fatal("schema defaults canonicalized explicit values or were not idempotent")
	}
	*got.Security.ReadOnlyRootFilesystem = true
	got.Network.BlockedPorts[0].Protocol = corev1.ProtocolTCP
	got.Network.AllowedInternalCIDRs[0].Ports[0].Port = 8080
	if !reflect.DeepEqual(policy, *original) {
		t.Fatal("policy defaulting retained caller-owned pointers or slices")
	}
}

func TestNormalizeSandboxDefaultsAndCopies(t *testing.T) {
	for _, runtime := range []v1alpha1.SandboxType{"", v1alpha1.SandboxKata, v1alpha1.SandboxGVisor, v1alpha1.SandboxContainer} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(string(runtime)+"/"+string(mode), func(t *testing.T) {
				input := normalizationSandboxFixture()
				input.Type = runtime
				input.Storage.Home.AccessMode = mode
				quantity := input.Resources.Agent.Requests[corev1.ResourceCPU]
				quantity.AsDec()
				input.Resources.Agent.Requests[corev1.ResourceCPU] = quantity
				input.Storage.Home.Size.AsDec()
				before := input.DeepCopy()
				got, errs := NormalizeSandbox(input)
				if len(errs) != 0 {
					t.Fatal(errs.ToAggregate())
				}
				if got.Type == "" || got.Storage.Home.AccessMode != mode || !reflect.DeepEqual(input, *before) {
					t.Fatal("defaulting lost explicit values or mutated the input")
				}
				if runtime == "" && got.Type != v1alpha1.SandboxKata {
					t.Fatalf("default runtime = %q, want kata", got.Type)
				}
				if runtime != "" && got.Type != runtime {
					t.Fatalf("explicit runtime = %q, want %q", got.Type, runtime)
				}
				again, errs := NormalizeSandbox(got)
				if len(errs) != 0 || !reflect.DeepEqual(got, again) {
					t.Fatalf("normalization is not idempotent: %v", errs)
				}
				changed := got.Resources.Agent.Requests[corev1.ResourceCPU]
				changed.AsDec().SetUnscaled(5)
				got.Resources.Helpers.Limits[corev1.ResourceMemory] = resource.MustParse("5Gi")
				got.Storage.Home.Size.AsDec().SetUnscaled(5)
				got.Storage.Scratch.SizeLimit.AsDec().SetUnscaled(5)
				if !reflect.DeepEqual(input, *before) {
					t.Fatal("normalized Sandbox shares mutable input state")
				}
			})
		}
	}
}

func TestNormalizeSandboxEquivalentQuantities(t *testing.T) {
	for _, tc := range []struct{ first, second string }{
		{"1000m", "1"}, {"1024Mi", "1Gi"}, {"1Gi", "1073741824"}, {"1e3", "1k"}, {"0.000000001", "1n"},
	} {
		t.Run(tc.first+"/"+tc.second, func(t *testing.T) {
			first, second := normalizationSandboxFixture(), normalizationSandboxFixture()
			for _, input := range []*v1alpha1.SandboxSpec{&first, &second} {
				quantity := resource.MustParse(tc.first)
				if input == &second {
					quantity = resource.MustParse(tc.second)
				}
				for _, group := range []*v1alpha1.SandboxContainerResources{&input.Resources.Agent, &input.Resources.Helpers} {
					for _, values := range []corev1.ResourceList{group.Requests, group.Limits} {
						for name := range values {
							values[name] = quantity.DeepCopy()
						}
					}
				}
				input.Storage.Home.Size = quantity.DeepCopy()
				input.Storage.Scratch.SizeLimit = quantity.DeepCopy()
			}
			left, leftErrors := NormalizeSandbox(first)
			right, rightErrors := NormalizeSandbox(second)
			if len(leftErrors)+len(rightErrors) != 0 {
				t.Fatalf("equivalent positive quantities rejected: %v; %v", leftErrors, rightErrors)
			}
			if normalizationJSON(t, left) != normalizationJSON(t, right) {
				t.Fatalf("equivalent quantities have different canonical JSON:\n%s\n%s", normalizationJSON(t, left), normalizationJSON(t, right))
			}
		})
	}
}

func TestNormalizeSandboxRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		change func(*v1alpha1.SandboxSpec)
		path   string
	}{
		{"runtime", func(s *v1alpha1.SandboxSpec) { s.Type = "native" }, "spec.type"},
		{"storage class omitted", func(s *v1alpha1.SandboxSpec) { s.Storage.Home.StorageClassName = "" }, "spec.storage.home.storageClassName"},
		{"storage class malformed", func(s *v1alpha1.SandboxSpec) { s.Storage.Home.StorageClassName = "Not/AClass" }, "spec.storage.home.storageClassName"},
		{"home zero", func(s *v1alpha1.SandboxSpec) { s.Storage.Home.Size = resource.Quantity{} }, "spec.storage.home.size"},
		{"home negative", func(s *v1alpha1.SandboxSpec) { s.Storage.Home.Size = resource.MustParse("-1Gi") }, "spec.storage.home.size"},
		{"scratch zero", func(s *v1alpha1.SandboxSpec) { s.Storage.Scratch.SizeLimit = resource.Quantity{} }, "spec.storage.scratch.sizeLimit"},
		{"scratch negative", func(s *v1alpha1.SandboxSpec) { s.Storage.Scratch.SizeLimit = resource.MustParse("-1Gi") }, "spec.storage.scratch.sizeLimit"},
		{"access mode omitted", func(s *v1alpha1.SandboxSpec) { s.Storage.Home.AccessMode = "" }, "spec.storage.home.accessMode"},
		{"multiple writers", func(s *v1alpha1.SandboxSpec) { s.Storage.Home.AccessMode = corev1.ReadWriteMany }, "spec.storage.home.accessMode"},
		{"read only", func(s *v1alpha1.SandboxSpec) { s.Storage.Home.AccessMode = corev1.ReadOnlyMany }, "spec.storage.home.accessMode"},
	}
	for _, group := range []string{"agent", "helpers"} {
		for _, kind := range []string{"requests", "limits"} {
			for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
				for _, value := range []string{"missing", "0", "-1"} {
					tests = append(tests, struct {
						name   string
						change func(*v1alpha1.SandboxSpec)
						path   string
					}{
						group + "/" + kind + "/" + string(resourceName) + "/" + value,
						func(s *v1alpha1.SandboxSpec) {
							resources := &s.Resources.Agent
							if group == "helpers" {
								resources = &s.Resources.Helpers
							}
							list := resources.Requests
							if kind == "limits" {
								list = resources.Limits
							}
							if value == "missing" {
								delete(list, resourceName)
							} else {
								list[resourceName] = resource.MustParse(value)
							}
						},
						"spec.resources." + group + "." + kind + "[" + string(resourceName) + "]",
					})
				}
			}
			tests = append(tests, struct {
				name   string
				change func(*v1alpha1.SandboxSpec)
				path   string
			}{
				group + "/" + kind + "/unsupported",
				func(s *v1alpha1.SandboxSpec) {
					resources := &s.Resources.Agent
					if group == "helpers" {
						resources = &s.Resources.Helpers
					}
					list := resources.Requests
					if kind == "limits" {
						list = resources.Limits
					}
					list[corev1.ResourceStorage] = resource.MustParse("1")
				},
				"spec.resources." + group + "." + kind + "[storage]",
			})
		}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			tests = append(tests, struct {
				name   string
				change func(*v1alpha1.SandboxSpec)
				path   string
			}{
				group + "/" + string(name) + "/request exceeds limit",
				func(s *v1alpha1.SandboxSpec) {
					resources := &s.Resources.Agent
					if group == "helpers" {
						resources = &s.Resources.Helpers
					}
					resources.Requests[name] = resource.MustParse("2")
					resources.Limits[name] = resource.MustParse("1")
				},
				"spec.resources." + group + ".requests[" + string(name) + "]",
			})
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := normalizationSandboxFixture()
			tc.change(&input)
			before := input.DeepCopy()
			_, errs := NormalizeSandbox(input)
			requireNormalizationError(t, errs, tc.path)
			if !reflect.DeepEqual(input, *before) {
				t.Fatal("invalid input was mutated")
			}
		})
	}
	_, errs := NormalizeSandbox(v1alpha1.SandboxSpec{})
	requireNormalizationError(t, errs, "spec.resources.agent.requests[cpu]")
	requireNormalizationError(t, errs, "spec.resources.helpers.limits[memory]")
}

func TestNormalizeAgentPolicyDefaults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    v1alpha1.AgentPolicySpec
		readOnly bool
	}{
		{"omitted", v1alpha1.AgentPolicySpec{Harnesses: v1alpha1.AgentPolicyHarnesses{Allowed: []v1alpha1.AgentType{v1alpha1.AgentTypeHermes}}}, true},
		{"explicit true", v1alpha1.AgentPolicySpec{Harnesses: v1alpha1.AgentPolicyHarnesses{Allowed: []v1alpha1.AgentType{v1alpha1.AgentTypeHermes}}, Security: v1alpha1.AgentPolicySecurity{ReadOnlyRootFilesystem: normalizationBool(true)}}, true},
		{"explicit false", v1alpha1.AgentPolicySpec{Harnesses: v1alpha1.AgentPolicyHarnesses{Allowed: []v1alpha1.AgentType{v1alpha1.AgentTypeHermes}}, Security: v1alpha1.AgentPolicySecurity{ReadOnlyRootFilesystem: normalizationBool(false)}}, false},
		{"empty lists", v1alpha1.AgentPolicySpec{Harnesses: v1alpha1.AgentPolicyHarnesses{Allowed: []v1alpha1.AgentType{v1alpha1.AgentTypeHermes}}, Network: v1alpha1.AgentPolicyNetwork{
			BlockedCIDRs: []string{}, BlockedFQDNs: []string{}, BlockedPorts: []v1alpha1.AgentPolicyPort{},
			AllowedInternalCIDRs: []v1alpha1.AgentPolicyCIDRRule{},
		}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.input.DeepCopy()
			got, errs := NormalizeAgentPolicy(tc.input)
			if len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
			if got.Security.ReadOnlyRootFilesystem == nil || *got.Security.ReadOnlyRootFilesystem != tc.readOnly ||
				got.Network.Ingress.Mode != v1alpha1.AgentPolicyIngressAuthenticated || got.Network.Ingress.RequireMTLS ||
				got.SandboxSelection.AllowMultiple || got.EntraID.Enabled || got.KubernetesAccess != nil {
				t.Fatalf("incorrect defaults: %#v", got)
			}
			if got.Network.BlockedCIDRs == nil || got.Network.BlockedFQDNs == nil || got.Network.BlockedPorts == nil || got.Network.AllowedInternalCIDRs == nil {
				t.Fatal("empty network lists were not materialized")
			}
			again, errs := NormalizeAgentPolicy(got)
			if len(errs) != 0 || !reflect.DeepEqual(got, again) {
				t.Fatalf("normalization is not idempotent: %v", errs)
			}
			*got.Security.ReadOnlyRootFilesystem = !tc.readOnly
			if !reflect.DeepEqual(tc.input, *before) {
				t.Fatal("defaulting or returned pointer mutated the input")
			}
		})
	}
}

func TestNormalizeAgentPolicyPolicyEntitlements(t *testing.T) {
	valid := v1alpha1.AgentPolicySpec{
		Harnesses: v1alpha1.AgentPolicyHarnesses{Allowed: []v1alpha1.AgentType{v1alpha1.AgentTypeHermes}},
		ModelAccess: v1alpha1.AgentPolicyModelAccess{
			SharedModels:  v1alpha1.AgentPolicySharedModelsAccess{Allowed: []string{"gpt-4", "gpt-4"}},
			GatewayBudget: &v1alpha1.AgentPolicyGatewayBudget{DefaultMonthlyUSD: "5", MaxMonthlyUSD: "10"},
		},
		AgentIdentity: v1alpha1.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{"azure-resource-manager"}},
	}
	normalized, errs := NormalizeAgentPolicy(valid)
	if len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
	if !reflect.DeepEqual(normalized.ModelAccess.SharedModels.Allowed, []string{"gpt-4"}) {
		t.Fatalf("shared models were not canonicalized: %#v", normalized.ModelAccess.SharedModels.Allowed)
	}
	for _, invalid := range []v1alpha1.AgentPolicySpec{
		{},
		{Harnesses: valid.Harnesses, ModelAccess: v1alpha1.AgentPolicyModelAccess{Copilot: v1alpha1.AgentPolicyCopilotAccess{Allowed: true}}},
		{Harnesses: valid.Harnesses, ModelAccess: v1alpha1.AgentPolicyModelAccess{GatewayBudget: valid.ModelAccess.GatewayBudget}},
		{Harnesses: valid.Harnesses, AgentIdentity: v1alpha1.AgentPolicyAgentIdentity{Required: true}},
	} {
		if _, errs := NormalizeAgentPolicy(invalid); len(errs) == 0 {
			t.Fatalf("expected invalid policy policy: %#v", invalid)
		}
	}
}

func TestNormalizeAgentPolicyOrderingAndIsolation(t *testing.T) {
	input := v1alpha1.AgentPolicySpec{
		Harnesses:        v1alpha1.AgentPolicyHarnesses{Allowed: []v1alpha1.AgentType{v1alpha1.AgentTypeOpenClaw}},
		SandboxSelection: v1alpha1.SandboxSelectionPolicy{AllowMultiple: true},
		Security:         v1alpha1.AgentPolicySecurity{ReadOnlyRootFilesystem: normalizationBool(false)},
		EntraID:          v1alpha1.AgentPolicyEntraID{Enabled: true},
		Network: v1alpha1.AgentPolicyNetwork{
			BlockedCIDRs: []string{"2001:0DB8:1::7/64", "192.0.2.99/24", "192.0.2.0/24"},
			BlockedFQDNs: []string{"z.example.com", "a.example.com", "z.example.com"},
			BlockedPorts: []v1alpha1.AgentPolicyPort{
				{Port: 65535, Protocol: corev1.ProtocolSCTP}, {Port: 53, Protocol: corev1.ProtocolUDP},
				{Port: 1}, {Port: 53, Protocol: corev1.ProtocolTCP}, {Port: 1, Protocol: corev1.ProtocolTCP},
			},
			AllowedInternalCIDRs: []v1alpha1.AgentPolicyCIDRRule{
				{CIDR: "2001:db8:2::1/64", Ports: []v1alpha1.AgentPolicyPort{{Port: 443}}},
				{CIDR: "198.51.100.1/24", Ports: []v1alpha1.AgentPolicyPort{{Port: 443}, {Port: 53, Protocol: corev1.ProtocolUDP}}},
				{CIDR: "198.51.100.254/24", Ports: []v1alpha1.AgentPolicyPort{{Port: 80}, {Port: 443, Protocol: corev1.ProtocolTCP}}},
			},
			Ingress: v1alpha1.AgentPolicyIngress{RequireMTLS: true},
		},
		KubernetesAccess: &v1alpha1.AgentPolicyAdminKubernetesAccess{
			ServiceAccountName: "agent-access",
			RoleBindings: []v1alpha1.AgentPolicyRoleBinding{
				{Namespace: "z-services", ClusterRoleName: "reader"},
				{Namespace: "a-services", ClusterRoleName: "system:discovery"},
				{Namespace: "a-services", ClusterRoleName: "reader"},
				{Namespace: "a-services", ClusterRoleName: "reader"},
			},
			ClusterRoleBindings: []v1alpha1.AgentPolicyClusterRoleBinding{
				{ClusterRoleName: "system:discovery"}, {ClusterRoleName: "reader"}, {ClusterRoleName: "reader"},
			},
		},
	}
	before := input.DeepCopy()
	got, errs := NormalizeAgentPolicy(input)
	if len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
	if !reflect.DeepEqual(got.Network.BlockedCIDRs, []string{"192.0.2.0/24", "2001:db8:1::/64"}) ||
		!reflect.DeepEqual(got.Network.BlockedFQDNs, []string{"a.example.com", "z.example.com"}) {
		t.Fatalf("lists were not normalized: %#v", got.Network)
	}
	wantPorts := []v1alpha1.AgentPolicyPort{
		{Port: 1, Protocol: corev1.ProtocolTCP}, {Port: 53, Protocol: corev1.ProtocolTCP},
		{Port: 53, Protocol: corev1.ProtocolUDP}, {Port: 65535, Protocol: corev1.ProtocolSCTP},
	}
	if !reflect.DeepEqual(got.Network.BlockedPorts, wantPorts) {
		t.Fatalf("ports = %#v, want %#v", got.Network.BlockedPorts, wantPorts)
	}
	if len(got.Network.AllowedInternalCIDRs) != 2 || got.Network.AllowedInternalCIDRs[0].CIDR != "198.51.100.0/24" ||
		!reflect.DeepEqual(got.Network.AllowedInternalCIDRs[0].Ports, []v1alpha1.AgentPolicyPort{
			{Port: 53, Protocol: corev1.ProtocolUDP}, {Port: 80, Protocol: corev1.ProtocolTCP}, {Port: 443, Protocol: corev1.ProtocolTCP},
		}) {
		t.Fatalf("equivalent internal rules were not merged: %#v", got.Network.AllowedInternalCIDRs)
	}
	if len(got.KubernetesAccess.RoleBindings) != 3 || got.KubernetesAccess.RoleBindings[0].Namespace != "a-services" ||
		got.KubernetesAccess.RoleBindings[0].ClusterRoleName != "reader" || len(got.KubernetesAccess.ClusterRoleBindings) != 2 ||
		got.KubernetesAccess.ClusterRoleBindings[0].ClusterRoleName != "reader" {
		t.Fatalf("grants were not sorted and deduplicated: %#v", got.KubernetesAccess)
	}
	if !got.SandboxSelection.AllowMultiple || !got.EntraID.Enabled || *got.Security.ReadOnlyRootFilesystem || !got.Network.Ingress.RequireMTLS {
		t.Fatal("normalization discarded explicit intent")
	}
	permuted := input.DeepCopy()
	slices.Reverse(permuted.Network.BlockedCIDRs)
	slices.Reverse(permuted.Network.BlockedFQDNs)
	slices.Reverse(permuted.Network.BlockedPorts)
	slices.Reverse(permuted.Network.AllowedInternalCIDRs)
	slices.Reverse(permuted.KubernetesAccess.RoleBindings)
	slices.Reverse(permuted.KubernetesAccess.ClusterRoleBindings)
	other, errs := NormalizeAgentPolicy(*permuted)
	if len(errs) != 0 || normalizationJSON(t, got) != normalizationJSON(t, other) {
		t.Fatalf("input ordering changed canonical policy: %v", errs)
	}
	again, errs := NormalizeAgentPolicy(got)
	if len(errs) != 0 || !reflect.DeepEqual(got, again) {
		t.Fatalf("normalization is not idempotent: %v", errs)
	}
	*got.Security.ReadOnlyRootFilesystem = true
	got.Network.BlockedCIDRs[0] = "0.0.0.0/0"
	got.Network.BlockedFQDNs[0] = "other.example.com"
	got.Network.BlockedPorts[0].Port = 2
	got.Network.AllowedInternalCIDRs[0].Ports[0].Port = 2
	got.KubernetesAccess.RoleBindings[0].Namespace = "other"
	got.KubernetesAccess.ClusterRoleBindings[0].ClusterRoleName = "other"
	got.KubernetesAccess.ServiceAccountName = "other"
	if !reflect.DeepEqual(input, *before) {
		t.Fatal("normalized AgentPolicy shares mutable input state")
	}
}

func TestNormalizeAgentPolicyRejectsMalformedNetwork(t *testing.T) {
	for _, value := range []string{"", "192.0.2.1", "192.0.2.1/33", "2001:db8::/129", "192.000.2.1/24", "garbage/24",
		"::ffff:192.0.2.1/120", "::ffff:c000:201/120", "fe80::1%eth0/64"} {
		t.Run("CIDR/"+value, func(t *testing.T) {
			input := normalizationPolicy(v1alpha1.AgentPolicySpec{Network: v1alpha1.AgentPolicyNetwork{
				BlockedCIDRs:         []string{value},
				AllowedInternalCIDRs: []v1alpha1.AgentPolicyCIDRRule{{CIDR: value, Ports: []v1alpha1.AgentPolicyPort{{Port: 443}}}},
			}})
			_, errs := NormalizeAgentPolicy(input)
			requireNormalizationError(t, errs, "spec.network.blockedCIDRs[0]")
			requireNormalizationError(t, errs, "spec.network.allowedInternalCIDRs[0].cidr")
		})
	}
	for _, value := range []string{"", "Example.com", "example.com.", "*.example.com", "https://example.com",
		"example.com/path", "192.0.2.1", "2001:db8::1", "[2001:db8::1]", "éxample.com", "_service.example.com",
		" example.com", "example.com ", "a..example.com", "-a.example.com", strings.Repeat("a", 64) + ".example.com",
		strings.Repeat("a.", 126) + "com"} {
		t.Run("hostname/"+value, func(t *testing.T) {
			_, errs := NormalizeAgentPolicy(normalizationPolicy(v1alpha1.AgentPolicySpec{Network: v1alpha1.AgentPolicyNetwork{BlockedFQDNs: []string{value}}}))
			requireNormalizationError(t, errs, "spec.network.blockedFQDNs[0]")
		})
	}
	for _, port := range []v1alpha1.AgentPolicyPort{
		{Port: -1}, {Port: 0}, {Port: 65536}, {Port: 443, Protocol: "tcp"}, {Port: 443, Protocol: "ICMP"},
	} {
		t.Run("port/"+normalizationJSON(t, port), func(t *testing.T) {
			input := normalizationPolicy(v1alpha1.AgentPolicySpec{Network: v1alpha1.AgentPolicyNetwork{
				BlockedPorts:         []v1alpha1.AgentPolicyPort{port},
				AllowedInternalCIDRs: []v1alpha1.AgentPolicyCIDRRule{{CIDR: "198.51.100.0/24", Ports: []v1alpha1.AgentPolicyPort{port}}},
			}})
			_, errs := NormalizeAgentPolicy(input)
			member := "port"
			if port.Protocol != "" {
				member = "protocol"
			}
			requireNormalizationError(t, errs, "spec.network.blockedPorts[0]."+member)
			requireNormalizationError(t, errs, "spec.network.allowedInternalCIDRs[0].ports[0]."+member)
		})
	}
	for _, ports := range [][]v1alpha1.AgentPolicyPort{nil, {}} {
		_, errs := NormalizeAgentPolicy(normalizationPolicy(v1alpha1.AgentPolicySpec{Network: v1alpha1.AgentPolicyNetwork{
			AllowedInternalCIDRs: []v1alpha1.AgentPolicyCIDRRule{{CIDR: "198.51.100.0/24", Ports: ports}},
		}}))
		requireNormalizationError(t, errs, "spec.network.allowedInternalCIDRs[0].ports")
	}
}

func TestNormalizeAgentPolicyIngress(t *testing.T) {
	for _, tc := range []struct {
		mode v1alpha1.AgentPolicyIngressMode
		mtls bool
		path string
	}{
		{"", false, ""}, {"", true, ""}, {v1alpha1.AgentPolicyIngressAuthenticated, true, ""},
		{v1alpha1.AgentPolicyIngressDisabled, false, ""},
		{v1alpha1.AgentPolicyIngressDisabled, true, "spec.network.ingress.requireMTLS"},
		{"anonymous", false, "spec.network.ingress.mode"},
	} {
		t.Run(string(tc.mode)+"/"+normalizationJSON(t, tc.mtls), func(t *testing.T) {
			_, errs := NormalizeAgentPolicy(normalizationPolicy(v1alpha1.AgentPolicySpec{Network: v1alpha1.AgentPolicyNetwork{
				Ingress: v1alpha1.AgentPolicyIngress{Mode: tc.mode, RequireMTLS: tc.mtls},
			}}))
			if tc.path == "" && len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
			if tc.path != "" {
				requireNormalizationError(t, errs, tc.path)
			}
		})
	}
}

func TestNormalizeAgentPolicyKubernetesAccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		access v1alpha1.AgentPolicyAdminKubernetesAccess
		path   string
	}{
		{"empty block", v1alpha1.AgentPolicyAdminKubernetesAccess{}, "spec.kubernetesAccess.serviceAccountName"},
		{"no grants", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access"}, "spec.kubernetesAccess"},
		{"invalid service account", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "Not/AName", RoleBindings: []v1alpha1.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "reader"}}}, "spec.kubernetesAccess.serviceAccountName"},
		{"empty namespace", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", RoleBindings: []v1alpha1.AgentPolicyRoleBinding{{ClusterRoleName: "reader"}}}, "spec.kubernetesAccess.roleBindings[0].namespace"},
		{"invalid namespace", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", RoleBindings: []v1alpha1.AgentPolicyRoleBinding{{Namespace: "NotANamespace", ClusterRoleName: "reader"}}}, "spec.kubernetesAccess.roleBindings[0].namespace"},
		{"empty role", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", RoleBindings: []v1alpha1.AgentPolicyRoleBinding{{Namespace: "services"}}}, "spec.kubernetesAccess.roleBindings[0].clusterRoleName"},
		{"invalid role", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", RoleBindings: []v1alpha1.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "bad/role"}}}, "spec.kubernetesAccess.roleBindings[0].clusterRoleName"},
		{"empty cluster role", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", ClusterRoleBindings: []v1alpha1.AgentPolicyClusterRoleBinding{{}}}, "spec.kubernetesAccess.clusterRoleBindings[0].clusterRoleName"},
		{"dot role", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", ClusterRoleBindings: []v1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: "."}}}, "spec.kubernetesAccess.clusterRoleBindings[0].clusterRoleName"},
		{"dotdot role", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", ClusterRoleBindings: []v1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: ".."}}}, "spec.kubernetesAccess.clusterRoleBindings[0].clusterRoleName"},
		{"escaped role", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", ClusterRoleBindings: []v1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: "%72eader"}}}, "spec.kubernetesAccess.clusterRoleBindings[0].clusterRoleName"},
		{"namespaced only", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent.access", RoleBindings: []v1alpha1.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "reader"}}}, ""},
		{"cluster only", v1alpha1.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "agent-access", ClusterRoleBindings: []v1alpha1.AgentPolicyClusterRoleBinding{{ClusterRoleName: "system:discovery"}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := normalizationPolicy(v1alpha1.AgentPolicySpec{KubernetesAccess: &tc.access})
			before := input.DeepCopy()
			_, errs := NormalizeAgentPolicy(input)
			if tc.path == "" && len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
			if tc.path != "" {
				requireNormalizationError(t, errs, tc.path)
			}
			if !reflect.DeepEqual(input, *before) {
				t.Fatal("validation mutated Kubernetes access input")
			}
		})
	}
}

func requireNormalizationError(t *testing.T, errs field.ErrorList, path string) {
	t.Helper()
	for _, err := range errs {
		if err.Field == path {
			return
		}
	}
	t.Fatalf("errors = %v, want a field error at %s", errs.ToAggregate(), path)
}

func normalizationJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func normalizationBool(value bool) *bool { return &value }

func normalizationPolicy(policy v1alpha1.AgentPolicySpec) v1alpha1.AgentPolicySpec {
	policy.Harnesses.Allowed = []v1alpha1.AgentType{v1alpha1.AgentTypeHermes}
	return policy
}
