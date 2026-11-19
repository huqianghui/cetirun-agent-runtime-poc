// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func fixtureResources() cetirunv1alpha1.SandboxContainerResources {
	return cetirunv1alpha1.SandboxContainerResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
	}
}

func fixtureCapabilities() Capabilities {
	return Capabilities{
		RuntimeClassName: "kata-test", Revision: "synthetic-qualified-registration",
		HostResourceLimits: true, ReadOnlyRootFilesystem: true,
		HomeAccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
		IPFamilies:           []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
		Protocols:            []corev1.Protocol{corev1.ProtocolTCP},
		GuestExternalNetwork: true, DenyPrecedence: true, NodeTraffic: true,
		DNSIPRestrictions: true, BlockedPorts: true,
		AuthenticatedIngress: true, DisabledIngress: true,
		EnforcementObservation: true, RestrictiveFailure: true,
	}
}

func resolverFixture() ResolveInput {
	sandbox := &cetirunv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "sandbox-uid"},
		Spec: cetirunv1alpha1.SandboxSpec{
			Resources: cetirunv1alpha1.SandboxResources{Agent: fixtureResources(), Helpers: fixtureResources()},
			Storage: cetirunv1alpha1.SandboxStorage{
				Home: cetirunv1alpha1.SandboxHomeStorage{
					StorageClassName: "home-test", Size: resource.MustParse("10Gi"), AccessMode: corev1.ReadWriteOncePod,
				},
				Scratch: cetirunv1alpha1.SandboxScratchStorage{SizeLimit: resource.MustParse("1Gi")},
			},
		},
	}
	return ResolveInput{
		Agent: &cetirunv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "alice-work", UID: "agent-uid"},
			Spec: cetirunv1alpha1.AgentSpec{
				Agent: cetirunv1alpha1.HarnessSpec{Type: cetirunv1alpha1.AgentTypeHermes},
				Team:  "engineering", SandboxRef: "standard",
				User: cetirunv1alpha1.UserSpec{Alias: "alice"},
			},
		},
		Team: &cetirunv1alpha1.Team{
			ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "team-uid"},
			Spec: cetirunv1alpha1.TeamSpec{
				AgentPolicyRef: "normal",
				SandboxRefs:    []string{"standard"}, DefaultSandboxRef: "standard",
			},
		},
		Resources: Resources{
			Sandboxes: map[string]*cetirunv1alpha1.Sandbox{"standard": sandbox},
			AgentPolicies: map[string]*cetirunv1alpha1.AgentPolicy{
				"normal": {
					ObjectMeta: metav1.ObjectMeta{Name: "normal", UID: "normal-uid"},
					Spec: cetirunv1alpha1.AgentPolicySpec{Harnesses: cetirunv1alpha1.AgentPolicyHarnesses{Allowed: []cetirunv1alpha1.AgentType{
						cetirunv1alpha1.AgentTypeHermes,
						cetirunv1alpha1.AgentTypeOpenClaw,
					}}},
				},
				"operations": {
					ObjectMeta: metav1.ObjectMeta{Name: "operations", UID: "operations-uid"},
					Spec: cetirunv1alpha1.AgentPolicySpec{Harnesses: cetirunv1alpha1.AgentPolicyHarnesses{Allowed: []cetirunv1alpha1.AgentType{
						cetirunv1alpha1.AgentTypeHermes,
						cetirunv1alpha1.AgentTypeOpenClaw,
					}}},
				},
			},
		},
		Registrations: map[cetirunv1alpha1.SandboxType]Registration{
			cetirunv1alpha1.SandboxKata: {Capabilities: fixtureCapabilities(), Available: true},
		},
		AuthorizedBinding: Binding{
			AgentUID: "agent-uid", TeamUID: "team-uid", SandboxUID: "sandbox-uid",
			AgentPolicyUID: "normal-uid"},
	}
}

func mustResolve(t *testing.T, input ResolveInput) *Resolution {
	t.Helper()
	result, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNativeResolveRequiresExplicitModeAndSupportedHarness(t *testing.T) {
	fixture := func() ResolveInput {
		input := resolverFixture()
		input.NativeExecution = true
		input.Agent.Spec.Agent.Type = cetirunv1alpha1.AgentTypeOpenClaw
		input.Resources.AgentPolicies["normal"].Spec.Harnesses.Allowed = []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw}
		input.Registrations[cetirunv1alpha1.SandboxKata] = Registration{Capabilities: nativeCapabilities(), Available: true}
		return input
	}
	resolved := mustResolve(t, fixture())
	if resolved.Capabilities.Profile != ProfileNativeSingleStart ||
		resolved.Capabilities.EnforcementObservation || resolved.Capabilities.RestrictiveFailure {
		t.Fatal("resolution invented stronger native guarantees")
	}
	for name, mutate := range map[string]func(*ResolveInput){
		"default mode": func(i *ResolveInput) { i.NativeExecution = false },
		"unknown":      func(i *ResolveInput) { i.Agent.Spec.Agent.Type = cetirunv1alpha1.AgentType("unknown") },
		"Kubernetes access": func(i *ResolveInput) {
			i.Resources.AgentPolicies["normal"].Spec.KubernetesAccess = &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "operations",
				RoleBindings:       []cetirunv1alpha1.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: "view"}},
			}
		},
		"RWO": func(i *ResolveInput) {
			i.Resources.Sandboxes["standard"].Spec.Storage.Home.AccessMode = corev1.ReadWriteOnce
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := fixture()
			mutate(&input)
			if _, err := Resolve(input); err == nil {
				t.Fatal("native-only registration authorized an unsupported execution")
			}
		})
	}
	unselected := fixture()
	unselected.Resources.AgentPolicies["operations"].Spec.KubernetesAccess = &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName: "operations",
		RoleBindings:       []cetirunv1alpha1.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: "view"}},
	}
	mustResolve(t, unselected)
	qualified := fixture()
	registration := qualified.Registrations[cetirunv1alpha1.SandboxKata]
	registration.Capabilities.Profile = ""
	registration.Capabilities.EnforcementObservation, registration.Capabilities.RestrictiveFailure = true, true
	qualified.Registrations[cetirunv1alpha1.SandboxKata] = registration
	qualified.NativeExecution = false
	if strong := mustResolve(t, qualified); strong.Revision == resolved.Revision {
		t.Fatal("profile change did not invalidate the resolution with an unchanged platform revision")
	}
}

func TestValidateTeamStopsAtInvalidPersistedIdentity(t *testing.T) {
	input := resolverFixture()
	input.Team.UID = ""
	input.Team.Spec.AgentPolicyRef = "missing-policy"
	errs := ValidateTeam(input.Team, Resources{}, nil)
	if len(errs) != 1 || errs[0].Field != "team.metadata.uid" {
		t.Fatalf("invalid identity should not produce unrelated reference errors: %v", errs)
	}
}

func TestFoundationRevisionUsesV1Domain(t *testing.T) {
	const expectedRevision = "39efbdbf83b160e3e032255dbc0df12bc80d9787e0a7866f2b0faff9a622e644"
	resolved := mustResolve(t, resolverFixture())
	if resolved.Revision != expectedRevision {
		t.Fatalf("v1 foundation revision = %q, want %q", resolved.Revision, expectedRevision)
	}
}

func TestResolveDefaultsAndIsolation(t *testing.T) {
	input := resolverFixture()
	beforeAgent := input.Agent.DeepCopy()
	beforeTeam := input.Team.DeepCopy()
	beforeSandbox := input.Resources.Sandboxes["standard"].DeepCopy()
	beforePolicy := input.Resources.AgentPolicies["normal"].DeepCopy()
	resolved := mustResolve(t, input)
	if resolved.Sandbox.Type != cetirunv1alpha1.SandboxKata ||
		!(*resolved.AgentPolicy.Security.ReadOnlyRootFilesystem) ||
		resolved.AgentPolicy.Network.Ingress.Mode != cetirunv1alpha1.AgentPolicyIngressAuthenticated ||
		len(resolved.Revision) != 64 {
		t.Fatalf("unexpected normalized resolution: %+v", resolved)
	}
	resolved.Sandbox.Resources.Agent.Limits[corev1.ResourceCPU] = resource.MustParse("99")
	*resolved.AgentPolicy.Security.ReadOnlyRootFilesystem = false
	resolved.Capabilities.IPFamilies[0] = corev1.IPv6Protocol
	if !reflect.DeepEqual(input.Agent, beforeAgent) || !reflect.DeepEqual(input.Team, beforeTeam) ||
		!reflect.DeepEqual(input.Resources.Sandboxes["standard"], beforeSandbox) ||
		!reflect.DeepEqual(input.Resources.AgentPolicies["normal"], beforePolicy) ||
		input.Registrations[cetirunv1alpha1.SandboxKata].Capabilities.IPFamilies[0] != corev1.IPv4Protocol {
		t.Fatal("resolution mutated the source snapshot")
	}
	if input.Agent.Status.Sandbox != nil || len(input.Agent.Status.Conditions) != 0 {
		t.Fatal("resolution must not claim application or enforcement")
	}
}

func TestResolveIndependentTeamPolicies(t *testing.T) {
	input := resolverFixture()
	input.Resources.AgentPolicies["operations"].Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 25}}
	normal := mustResolve(t, input)
	input.Team.Name, input.Team.UID = "administration", "admin-team-uid"
	input.Team.Spec.AgentPolicyRef = "operations"
	input.Agent.Spec.Team = input.Team.Name
	input.AuthorizedBinding.TeamUID = input.Team.UID
	input.AuthorizedBinding.AgentPolicyUID = "operations-uid"
	admin := mustResolve(t, input)
	if len(normal.AgentPolicy.Network.BlockedPorts) != 0 || len(admin.AgentPolicy.Network.BlockedPorts) != 1 {
		t.Fatal("independent Team policies must not be merged")
	}
	if admin.Binding.SandboxUID != normal.Binding.SandboxUID || admin.Revision == normal.Revision {
		t.Fatal("shared Sandbox must not collapse distinct policy bindings")
	}
}

func TestResolveRevisionUsesOnlySandboxPolicyProjection(t *testing.T) {
	input := resolverFixture()
	baseline := mustResolve(t, input).Revision
	policy := input.Resources.AgentPolicies["normal"]
	policy.Spec.ModelAccess = cetirunv1alpha1.AgentPolicyModelAccess{
		Copilot:       cetirunv1alpha1.AgentPolicyCopilotAccess{Allowed: true},
		GatewayBudget: &cetirunv1alpha1.AgentPolicyGatewayBudget{DefaultMonthlyUSD: "10", MaxMonthlyUSD: "20"},
	}
	policy.Spec.AgentIdentity = cetirunv1alpha1.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}
	policy.Spec.Skills.Allowed = []string{"unrelated-skill"}
	if revision := mustResolve(t, input).Revision; revision != baseline {
		t.Fatal("Gateway, credential, or skill policy changed the Sandbox revision")
	}
	policy.Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 443}}
	if revision := mustResolve(t, input).Revision; revision == baseline {
		t.Fatal("Sandbox network policy did not change the Sandbox revision")
	}
}

func TestResolveRejectsInvalidReferences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ResolveInput)
	}{
		{"missing Agent", func(i *ResolveInput) { i.Agent = nil }},
		{"missing Team", func(i *ResolveInput) { i.Team = nil }},
		{"wrong Team", func(i *ResolveInput) { i.Agent.Spec.Team = "another-team" }},
		{"missing persisted selection", func(i *ResolveInput) { i.Agent.Spec.SandboxRef = "" }},
		{"unlisted selection", func(i *ResolveInput) { i.Agent.Spec.SandboxRef = "unlisted" }},
		{"empty list", func(i *ResolveInput) { i.Team.Spec.SandboxRefs = nil }},
		{"duplicate list", func(i *ResolveInput) { i.Team.Spec.SandboxRefs = []string{"standard", "standard"} }},
		{"unlisted default", func(i *ResolveInput) { i.Team.Spec.DefaultSandboxRef = "absent" }},
		{"missing Team policy", func(i *ResolveInput) { i.Team.Spec.AgentPolicyRef = "" }},
		{"missing Sandbox", func(i *ResolveInput) { delete(i.Resources.Sandboxes, "standard") }},
		{"missing selected policy", func(i *ResolveInput) { delete(i.Resources.AgentPolicies, "normal") }},
		{"mismatched Sandbox name", func(i *ResolveInput) { i.Resources.Sandboxes["standard"].Name = "different" }},
		{"mismatched policy name", func(i *ResolveInput) { i.Resources.AgentPolicies["normal"].Name = "different" }},
		{"missing UID", func(i *ResolveInput) { i.Resources.Sandboxes["standard"].UID = "" }},
		{"namespaced CR", func(i *ResolveInput) { i.Team.Namespace = "unexpected" }},
		{"terminating Agent", func(i *ResolveInput) { now := metav1.Now(); i.Agent.DeletionTimestamp = &now }},
		{"terminating Team", func(i *ResolveInput) { now := metav1.Now(); i.Team.DeletionTimestamp = &now }},
		{"terminating Sandbox", func(i *ResolveInput) { now := metav1.Now(); i.Resources.Sandboxes["standard"].DeletionTimestamp = &now }},
		{"terminating policy", func(i *ResolveInput) {
			now := metav1.Now()
			i.Resources.AgentPolicies["normal"].DeletionTimestamp = &now
		}},
		{"unregistered runtime", func(i *ResolveInput) { i.Registrations = nil }},
		{"zero capability registration", func(i *ResolveInput) {
			i.Registrations[cetirunv1alpha1.SandboxKata] = Registration{Available: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := resolverFixture()
			tc.mutate(&input)
			if result, err := Resolve(input); err == nil || result != nil {
				t.Fatalf("invalid references accepted: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestResolveRequiresExplicitUIDAuthorization(t *testing.T) {
	for _, change := range []func(*ResolveInput){
		func(i *ResolveInput) { i.AuthorizedBinding = Binding{} },
		func(i *ResolveInput) { i.Agent.UID = "replacement-agent" },
		func(i *ResolveInput) { i.Team.UID = "replacement-team" },
		func(i *ResolveInput) { i.Resources.Sandboxes["standard"].UID = "replacement-sandbox" },
		func(i *ResolveInput) { i.Resources.AgentPolicies["normal"].UID = "replacement-policy" },
		func(i *ResolveInput) { i.Team.Spec.AgentPolicyRef = "operations" },
	} {
		input := resolverFixture()
		change(&input)
		if _, err := Resolve(input); !errors.Is(err, ErrUnauthorizedBinding) {
			t.Fatalf("expected explicit binding authorization failure, got %v", err)
		}
	}
	input := resolverFixture()
	input.Resources.AgentPolicies["normal"].UID = "replacement-policy"
	input.Agent.Status.Sandbox = &cetirunv1alpha1.SandboxStatus{
		TeamUID: "team-uid", SandboxUID: "sandbox-uid", AgentPolicyUID: "replacement-policy",
		AppliedRevision: "old",
	}
	if _, err := Resolve(input); !errors.Is(err, ErrUnauthorizedBinding) {
		t.Fatalf("status must not substitute for authorization: %v", err)
	}
	input.AuthorizedBinding.AgentPolicyUID = "replacement-policy"
	mustResolve(t, input)
}

func TestEveryConfiguredPairAndSelectedRuntimeHealth(t *testing.T) {
	input := resolverFixture()
	other := input.Resources.Sandboxes["standard"].DeepCopy()
	other.Name, other.UID, other.Spec.Type = "gvisor", "gvisor-uid", cetirunv1alpha1.SandboxGVisor
	input.Resources.Sandboxes["gvisor"] = other
	input.Team.Spec.SandboxRefs = append(input.Team.Spec.SandboxRefs, "gvisor")
	input.Registrations[cetirunv1alpha1.SandboxGVisor] = Registration{Capabilities: fixtureCapabilities()}
	if _, err := Resolve(input); err == nil {
		t.Fatal("Team policy must permit multiple Sandboxes")
	}
	input.Resources.AgentPolicies["normal"].Spec.SandboxSelection.AllowMultiple = true
	mustResolve(t, input) // Unselected gVisor health does not stop a Kata consumer.
	registration := input.Registrations[cetirunv1alpha1.SandboxGVisor]
	registration.Capabilities.GuestExternalNetwork = false
	input.Registrations[cetirunv1alpha1.SandboxGVisor] = registration
	if _, err := Resolve(input); err == nil {
		t.Fatal("every permitted Sandbox must support both policy slots")
	}
	input = resolverFixture()
	registration = input.Registrations[cetirunv1alpha1.SandboxKata]
	registration.Available = false
	input.Registrations[cetirunv1alpha1.SandboxKata] = registration
	if _, err := Resolve(input); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("selected runtime health must gate resolution: %v", err)
	}
}

func TestRevisionIncludesOnlyEffectiveConfiguration(t *testing.T) {
	base := mustResolve(t, resolverFixture()).Revision
	for _, tc := range []struct {
		name   string
		mutate func(*ResolveInput)
	}{
		{"metadata", func(i *ResolveInput) {
			i.Agent.Generation = 42
			i.Team.ResourceVersion = "42"
			i.Team.Labels = map[string]string{"example": "changed"}
			i.Resources.Sandboxes["standard"].Annotations = map[string]string{"example": "changed"}
		}},
		{"model settings", func(i *ResolveInput) {
			i.Agent.Spec.Model = "example-model"
		}},
		{"unselected policy", func(i *ResolveInput) {
			i.Resources.AgentPolicies["operations"].Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 25}}
		}},
		{"selection allowance", func(i *ResolveInput) {
			i.Resources.AgentPolicies["normal"].Spec.SandboxSelection.AllowMultiple = true
		}},
		{"equivalent CPU quantity", func(i *ResolveInput) {
			i.Resources.Sandboxes["standard"].Spec.Resources.Agent.Requests[corev1.ResourceCPU] = resource.MustParse("1000m")
		}},
		{"explicit defaults", func(i *ResolveInput) {
			i.Resources.Sandboxes["standard"].Spec.Type = cetirunv1alpha1.SandboxKata
			readonly := true
			i.Resources.AgentPolicies["normal"].Spec.Security.ReadOnlyRootFilesystem = &readonly
			i.Resources.AgentPolicies["normal"].Spec.Network.Ingress.Mode = cetirunv1alpha1.AgentPolicyIngressAuthenticated
		}},
		{"registration set ordering", func(i *ResolveInput) {
			reg := i.Registrations[cetirunv1alpha1.SandboxKata]
			reg.Capabilities.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol, corev1.IPv4Protocol}
			i.Registrations[cetirunv1alpha1.SandboxKata] = reg
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := resolverFixture()
			tc.mutate(&input)
			if got := mustResolve(t, input).Revision; got != base {
				t.Fatalf("ineffective change altered revision: %s != %s", got, base)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ResolveInput)
	}{
		{"resources", func(i *ResolveInput) {
			i.Resources.Sandboxes["standard"].Spec.Resources.Agent.Limits[corev1.ResourceCPU] = resource.MustParse("3")
		}},
		{"port controls", func(i *ResolveInput) {
			i.Resources.AgentPolicies["normal"].Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 25}}
		}},
		{"ingress", func(i *ResolveInput) {
			i.Resources.AgentPolicies["normal"].Spec.Network.Ingress.Mode = cetirunv1alpha1.AgentPolicyIngressDisabled
		}},
		{"platform topology", func(i *ResolveInput) {
			reg := i.Registrations[cetirunv1alpha1.SandboxKata]
			reg.Capabilities.Revision = "new-qualified-topology"
			i.Registrations[cetirunv1alpha1.SandboxKata] = reg
		}},
		{"authorized policy replacement", func(i *ResolveInput) {
			i.Resources.AgentPolicies["normal"].UID = "replacement"
			i.AuthorizedBinding.AgentPolicyUID = "replacement"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := resolverFixture()
			tc.mutate(&input)
			if mustResolve(t, input).Revision == base {
				t.Fatal("effective change did not alter revision")
			}
		})
	}
}

func TestRevisionNormalizesRuleOrdering(t *testing.T) {
	input := resolverFixture()
	policy := input.Resources.AgentPolicies["normal"]
	policy.Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 25}, {Port: 443}}
	policy.Spec.Network.BlockedCIDRs = []string{"192.0.2.0/24", "198.51.100.0/24"}
	first := mustResolve(t, input).Revision
	policy.Spec.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{
		{Port: 443, Protocol: corev1.ProtocolTCP}, {Port: 25}, {Port: 25, Protocol: corev1.ProtocolTCP},
	}
	policy.Spec.Network.BlockedCIDRs = []string{"198.51.100.42/24", "192.0.2.0/24", "192.0.2.0/24"}
	if mustResolve(t, input).Revision != first {
		t.Fatal("equivalent rules must have an identical revision")
	}
}

func TestDefaultSelectionIsCreationOnly(t *testing.T) {
	input := resolverFixture()
	input.Agent.Spec.SandboxRef = ""
	spec, err := DefaultAgentSelection(input.Agent.Spec, input.Team)
	if err != nil || spec.SandboxRef != "standard" {
		t.Fatalf("creation defaults = %+v, %v", spec, err)
	}
	if input.Agent.Spec.SandboxRef != "" {
		t.Fatal("caller must explicitly persist the returned default")
	}
	if _, err := Resolve(input); err == nil {
		t.Fatal("resolver must not silently default an existing selection")
	}
	for _, tc := range []struct {
		name string
		spec cetirunv1alpha1.AgentSpec
	}{
		{"wrong Team", cetirunv1alpha1.AgentSpec{Team: "other"}},
		{"bad choice", cetirunv1alpha1.AgentSpec{Team: "engineering", SandboxRef: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DefaultAgentSelection(tc.spec, input.Team); err == nil {
				t.Fatal("invalid creation selection accepted")
			}
		})
	}
}

func TestDefaultSandboxChangeDoesNotReselectExistingAgent(t *testing.T) {
	input := resolverFixture()
	for _, p := range input.Resources.AgentPolicies {
		p.Spec.SandboxSelection.AllowMultiple = true
	}
	before := mustResolve(t, input).Revision
	other := input.Resources.Sandboxes["standard"].DeepCopy()
	other.Name, other.UID = "larger", types.UID("larger-uid")
	other.Spec.Resources.Agent.Limits[corev1.ResourceCPU] = resource.MustParse("4")
	input.Resources.Sandboxes["larger"] = other
	input.Team.Spec.SandboxRefs = append(input.Team.Spec.SandboxRefs, "larger")
	input.Team.Spec.DefaultSandboxRef = "larger"
	result := mustResolve(t, input)
	if result.Revision != before || result.Binding.SandboxUID != "sandbox-uid" {
		t.Fatal("adding/changing an unselected default changed the existing Agent")
	}
	input.Resources.AgentPolicies["normal"].Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	if _, err := Resolve(input); err == nil || !strings.Contains(err.Error(), "observed-SNI") {
		t.Fatalf("unregistered observed-SNI blocking must fail explicitly: %v", err)
	}
}
