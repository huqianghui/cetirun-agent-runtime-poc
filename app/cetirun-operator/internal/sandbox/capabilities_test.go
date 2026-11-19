// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestCompatibilityFailsClosed(t *testing.T) {
	input := resolverFixture()
	sandbox := input.Resources.Sandboxes["standard"].Spec
	policy := input.Resources.AgentPolicies["normal"].Spec
	for name, remove := range map[string]func(*Capabilities){
		"runtime class": func(c *Capabilities) { c.RuntimeClassName = "" },
		"revision":      func(c *Capabilities) { c.Revision = "" },
		"host limits":   func(c *Capabilities) { c.HostResourceLimits = false },
		"read-only":     func(c *Capabilities) { c.ReadOnlyRootFilesystem = false },
		"home":          func(c *Capabilities) { c.HomeAccessModes = nil },
		"families":      func(c *Capabilities) { c.IPFamilies = nil },
		"protocols":     func(c *Capabilities) { c.Protocols = nil },
		"external":      func(c *Capabilities) { c.GuestExternalNetwork = false },
		"precedence":    func(c *Capabilities) { c.DenyPrecedence = false },
		"node paths":    func(c *Capabilities) { c.NodeTraffic = false },
		"DNS IPs":       func(c *Capabilities) { c.DNSIPRestrictions = false },
		"ingress":       func(c *Capabilities) { c.AuthenticatedIngress = false },
		"observation":   func(c *Capabilities) { c.EnforcementObservation = false },
		"containment":   func(c *Capabilities) { c.RestrictiveFailure = false },
		"bad family":    func(c *Capabilities) { c.IPFamilies = []corev1.IPFamily{"other"} },
		"bad protocol":  func(c *Capabilities) { c.Protocols = []corev1.Protocol{corev1.ProtocolTCP, "other"} },
	} {
		t.Run(name, func(t *testing.T) {
			capabilities := fixtureCapabilities()
			remove(&capabilities)
			if errs := CheckCompatibility(sandbox, policy, capabilities); len(errs) == 0 {
				t.Fatal("missing mandatory capability accepted")
			}
		})
	}
	if len(CheckCompatibility(sandbox, policy, Capabilities{})) == 0 {
		t.Fatal("zero-value platform registration must not be usable")
	}
}

func TestCompatibilityOptionalControls(t *testing.T) {
	input := resolverFixture()
	sandbox := input.Resources.Sandboxes["standard"].Spec
	for _, tc := range []struct {
		name   string
		policy cetirunv1alpha1.AgentPolicySpec
		enable func(*Capabilities)
	}{
		{
			"FQDN", compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{Network: cetirunv1alpha1.AgentPolicyNetwork{BlockedFQDNs: []string{"blocked.example.com"}}}),
			func(c *Capabilities) { c.BlockedFQDNs = true },
		},
		{
			"mTLS", compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{Network: cetirunv1alpha1.AgentPolicyNetwork{Ingress: cetirunv1alpha1.AgentPolicyIngress{RequireMTLS: true}}}),
			func(c *Capabilities) { c.MutualTLS = true },
		},
		{
			"UDP port", compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{Network: cetirunv1alpha1.AgentPolicyNetwork{BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 1234, Protocol: corev1.ProtocolUDP}}}}),
			func(c *Capabilities) { c.Protocols = append(c.Protocols, corev1.ProtocolUDP) },
		},
		{
			"Entra", compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{EntraID: cetirunv1alpha1.AgentPolicyEntraID{Enabled: true}}),
			func(c *Capabilities) { c.EntraID = true },
		},
		{
			"Kubernetes", compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{KubernetesAccess: &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "operations",
				RoleBindings:       []cetirunv1alpha1.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "reader"}},
			}}),
			func(c *Capabilities) { c.KubernetesAccess = true },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capabilities := fixtureCapabilities()
			if len(CheckCompatibility(sandbox, tc.policy, capabilities)) == 0 {
				t.Fatal("unqualified optional control accepted")
			}
			tc.enable(&capabilities)
			if errs := CheckCompatibility(sandbox, tc.policy, capabilities); len(errs) != 0 {
				t.Fatalf("qualified control rejected: %v", errs)
			}
		})
	}
}

func TestCompatibilityResourceGuarantees(t *testing.T) {
	input := resolverFixture()
	sandbox := input.Resources.Sandboxes["standard"].Spec
	sandbox.Type = cetirunv1alpha1.SandboxGVisor
	capabilities := fixtureCapabilities()
	capabilities.RuntimeClassName = "gvisor-test"
	policy := compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{})
	if errs := CheckCompatibility(sandbox, policy, capabilities); len(errs) != 0 {
		t.Fatalf("aggregate gVisor limits must not imply independent quotas: %v", errs)
	}
	capabilities.RequirePerContainerResourceLimits = true
	if len(CheckCompatibility(sandbox, policy, capabilities)) == 0 {
		t.Fatal("explicit stronger guarantee must fail without qualified support")
	}
	capabilities.PerContainerResourceLimits = true
	if errs := CheckCompatibility(sandbox, policy, capabilities); len(errs) != 0 {
		t.Fatalf("qualified stronger guarantee rejected: %v", errs)
	}
}

func TestCompatibilityBlockedPortsRequireQualifiedSupport(t *testing.T) {
	input := resolverFixture()
	sandbox := input.Resources.Sandboxes["standard"].Spec
	policy := compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{Network: cetirunv1alpha1.AgentPolicyNetwork{
		BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 25}},
	}})
	capabilities := fixtureCapabilities()
	capabilities.BlockedPorts = false
	if len(CheckCompatibility(sandbox, policy, capabilities)) == 0 {
		t.Fatal("TCP support alone must not imply destination-port blocking")
	}
	capabilities.BlockedPorts = true
	if errs := CheckCompatibility(sandbox, policy, capabilities); len(errs) != 0 {
		t.Fatalf("qualified destination-port blocking rejected: %v", errs)
	}
}

func TestCompatibilityAddressFamiliesAndDisabledIngress(t *testing.T) {
	input := resolverFixture()
	sandbox := input.Resources.Sandboxes["standard"].Spec
	policy := compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{Network: cetirunv1alpha1.AgentPolicyNetwork{
		BlockedCIDRs: []string{"2001:db8::/32"},
		Ingress:      cetirunv1alpha1.AgentPolicyIngress{Mode: cetirunv1alpha1.AgentPolicyIngressDisabled},
	}})
	capabilities := fixtureCapabilities()
	capabilities.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
	if len(CheckCompatibility(sandbox, policy, capabilities)) == 0 {
		t.Fatal("IPv6 policy accepted by IPv4-only registration")
	}
	capabilities.IPFamilies = append(capabilities.IPFamilies, corev1.IPv6Protocol)
	capabilities.DisabledIngress = false
	if len(CheckCompatibility(sandbox, policy, capabilities)) == 0 {
		t.Fatal("disabled ingress accepted without qualified removal")
	}
	capabilities.DisabledIngress = true
	if errs := CheckCompatibility(sandbox, policy, capabilities); len(errs) != 0 {
		t.Fatalf("qualified disabled ingress rejected: %v", errs)
	}
}

func compatibilityPolicy(policy cetirunv1alpha1.AgentPolicySpec) cetirunv1alpha1.AgentPolicySpec {
	policy.Harnesses.Allowed = []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeHermes}
	return policy
}

func nativeCapabilities() Capabilities {
	capabilities := fixtureCapabilities()
	capabilities.Profile = ProfileNativeSingleStart
	capabilities.EnforcementObservation, capabilities.RestrictiveFailure, capabilities.BlockedPorts = false, false, false
	capabilities.Protocols = []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}
	return capabilities
}

func TestNativeHomeAccessModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requested  corev1.PersistentVolumeAccessMode
		registered []corev1.PersistentVolumeAccessMode
		allowed    bool
	}{
		{"RWOP", corev1.ReadWriteOncePod, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, true},
		{"RWO", corev1.ReadWriteOnce, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, true},
		{"both with RWOP", corev1.ReadWriteOncePod, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod}, true},
		{"both with RWO", corev1.ReadWriteOnce, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce}, true},
		{"RWO not registered", corev1.ReadWriteOnce, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, false},
		{"RWOP not registered", corev1.ReadWriteOncePod, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, false},
		{"no registered modes", corev1.ReadWriteOnce, nil, false},
		{"shared mode", corev1.ReadWriteOnce, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteMany}, false},
		{"duplicate mode", corev1.ReadWriteOnce, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOnce}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := resolverFixture()
			box := input.Resources.Sandboxes["standard"].Spec
			box.Storage.Home.AccessMode = tc.requested
			capabilities := nativeCapabilities()
			capabilities.HomeAccessModes = tc.registered
			policy := compatibilityPolicy(cetirunv1alpha1.AgentPolicySpec{})
			errs := CheckCompatibility(box, policy, capabilities)
			if (len(errs) == 0) != tc.allowed {
				t.Fatalf("HOME mode %s with registration %v: %v", tc.requested, tc.registered, errs)
			}
			if tc.allowed {
				if err := CheckExecutionProfile(capabilities, false); err == nil {
					t.Fatal("native HOME support authorized the held execution path")
				}
				box.Type = cetirunv1alpha1.SandboxGVisor
				if len(CheckCompatibility(box, policy, capabilities)) == 0 {
					t.Fatal("native HOME support authorized another runtime")
				}
			}
		})
	}
}

func TestNativeNetworkCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		network   cetirunv1alpha1.AgentPolicyNetwork
		protocols []corev1.Protocol
	}{
		{name: "CIDR", network: cetirunv1alpha1.AgentPolicyNetwork{BlockedCIDRs: []string{"198.51.100.0/24"}}},
		{name: "TCP", network: cetirunv1alpha1.AgentPolicyNetwork{
			BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
		}},
		{name: "UDP", network: cetirunv1alpha1.AgentPolicyNetwork{
			BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolUDP}},
		}},
		{name: "CIDR and default TCP", network: cetirunv1alpha1.AgentPolicyNetwork{
			BlockedCIDRs: []string{"198.51.100.0/24"},
			BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 8080}},
		}},
		{name: "scoped internal TCP and UDP", network: cetirunv1alpha1.AgentPolicyNetwork{
			AllowedInternalCIDRs: []cetirunv1alpha1.AgentPolicyCIDRRule{
				{CIDR: "10.20.0.0/16", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 443}}},
				{CIDR: "fd00:20::/64", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolUDP}}},
			},
		}},
		{name: "registered SCTP", network: cetirunv1alpha1.AgentPolicyNetwork{
			BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 3869, Protocol: corev1.ProtocolSCTP}},
			AllowedInternalCIDRs: []cetirunv1alpha1.AgentPolicyCIDRRule{
				{CIDR: "10.20.0.0/16", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 3868, Protocol: corev1.ProtocolSCTP}}},
				{CIDR: "fd00:20::/64", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 65535, Protocol: corev1.ProtocolSCTP}}},
			},
		}, protocols: []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP}},
		{name: "registered SCTP grant without port blocking", network: cetirunv1alpha1.AgentPolicyNetwork{
			AllowedInternalCIDRs: []cetirunv1alpha1.AgentPolicyCIDRRule{{
				CIDR: "10.20.0.0/16", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 3868, Protocol: corev1.ProtocolSCTP}},
			}},
		}, protocols: []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := resolverFixture()
			policy := cetirunv1alpha1.AgentPolicySpec{
				Harnesses: cetirunv1alpha1.AgentPolicyHarnesses{Allowed: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw}},
				Network:   tc.network,
			}
			capabilities := nativeCapabilities()
			capabilities.BlockedPorts = len(tc.network.BlockedPorts) != 0
			if tc.protocols != nil {
				capabilities.Protocols = tc.protocols
			}
			if errs := CheckCompatibility(input.Resources.Sandboxes["standard"].Spec, policy, capabilities); len(errs) != 0 {
				t.Fatalf("supported native network restrictions were rejected: %v", errs.ToAggregate())
			}
		})
	}
}

func TestNativeNetworkCompatibilityRetainsCapabilityChecks(t *testing.T) {
	input := resolverFixture()
	box := input.Resources.Sandboxes["standard"].Spec
	policy := cetirunv1alpha1.AgentPolicySpec{
		Harnesses: cetirunv1alpha1.AgentPolicyHarnesses{Allowed: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw}},
	}
	for _, tc := range []struct {
		name      string
		network   cetirunv1alpha1.AgentPolicyNetwork
		configure func(*Capabilities)
	}{
		{name: "ports not declared", network: cetirunv1alpha1.AgentPolicyNetwork{
			BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 18080}},
		}, configure: func(c *Capabilities) { c.BlockedPorts = false }},
		{name: "IPv6 not declared", network: cetirunv1alpha1.AgentPolicyNetwork{
			BlockedCIDRs: []string{"2001:db8::/32"},
		}, configure: func(c *Capabilities) { c.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol} }},
		{name: "SCTP not declared", network: cetirunv1alpha1.AgentPolicyNetwork{
			BlockedPorts: []cetirunv1alpha1.AgentPolicyPort{{Port: 18080, Protocol: corev1.ProtocolSCTP}},
		}},
		{name: "internal SCTP not declared", network: cetirunv1alpha1.AgentPolicyNetwork{
			AllowedInternalCIDRs: []cetirunv1alpha1.AgentPolicyCIDRRule{{
				CIDR: "10.20.0.0/16", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolSCTP}},
			}},
		}},
		{name: "internal IPv6 not declared", network: cetirunv1alpha1.AgentPolicyNetwork{
			AllowedInternalCIDRs: []cetirunv1alpha1.AgentPolicyCIDRRule{{
				CIDR: "fd00:20::/64", Ports: []cetirunv1alpha1.AgentPolicyPort{{Port: 443}},
			}},
		}, configure: func(c *Capabilities) { c.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capabilities := nativeCapabilities()
			capabilities.BlockedPorts = true
			if tc.configure != nil {
				tc.configure(&capabilities)
			}
			policy.Network = tc.network
			if errs := CheckCompatibility(box, policy, capabilities); len(errs) == 0 {
				t.Fatal("native network control exceeded its declared or supported capabilities")
			}
		})
	}
}

func TestNativeCompatibilityKeepsBaselineWithoutFencingClaims(t *testing.T) {
	input := resolverFixture()
	box := input.Resources.Sandboxes["standard"].Spec
	policy := cetirunv1alpha1.AgentPolicySpec{Harnesses: cetirunv1alpha1.AgentPolicyHarnesses{
		Allowed: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw},
	}}
	if errs := CheckCompatibility(box, policy, nativeCapabilities()); len(errs) != 0 {
		t.Fatal("bounded native baseline rejected:", errs)
	}
	for name, mutate := range map[string]func(*Capabilities){
		"unknown profile": func(c *Capabilities) { c.Profile = "other" },
		"empty profile still requires qualification": func(c *Capabilities) { c.Profile = "" },
		"host limits":       func(c *Capabilities) { c.HostResourceLimits = false },
		"external network":  func(c *Capabilities) { c.GuestExternalNetwork = false },
		"deny precedence":   func(c *Capabilities) { c.DenyPrecedence = false },
		"node traffic":      func(c *Capabilities) { c.NodeTraffic = false },
		"DNS restrictions":  func(c *Capabilities) { c.DNSIPRestrictions = false },
		"read-only root":    func(c *Capabilities) { c.ReadOnlyRootFilesystem = false },
		"authenticated":     func(c *Capabilities) { c.AuthenticatedIngress = false },
		"RWOP":              func(c *Capabilities) { c.HomeAccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce} },
		"external observer": func(c *Capabilities) { c.EnforcementObservation = true },
		"physical fencing":  func(c *Capabilities) { c.RestrictiveFailure = true },
		"container quotas":  func(c *Capabilities) { c.PerContainerResourceLimits = true },
	} {
		t.Run(name, func(t *testing.T) {
			capabilities := nativeCapabilities()
			mutate(&capabilities)
			if errs := CheckCompatibility(box, policy, capabilities); len(errs) == 0 {
				t.Fatal("incomplete or overstated native registration accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*cetirunv1alpha1.AgentPolicySpec){
		"Unknown harness": func(p *cetirunv1alpha1.AgentPolicySpec) {
			p.Harnesses.Allowed = append(p.Harnesses.Allowed, cetirunv1alpha1.AgentType("unknown"))
		},
		"FQDN": func(p *cetirunv1alpha1.AgentPolicySpec) { p.Network.BlockedFQDNs = []string{"blocked.example.com"} },
		"port": func(p *cetirunv1alpha1.AgentPolicySpec) {
			p.Network.BlockedPorts = []cetirunv1alpha1.AgentPolicyPort{{Port: 25}}
		},
		"mTLS":  func(p *cetirunv1alpha1.AgentPolicySpec) { p.Network.Ingress.RequireMTLS = true },
		"Entra": func(p *cetirunv1alpha1.AgentPolicySpec) { p.EntraID.Enabled = true },
		"admin": func(p *cetirunv1alpha1.AgentPolicySpec) {
			p.KubernetesAccess = &cetirunv1alpha1.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "operations",
				RoleBindings:       []cetirunv1alpha1.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "reader"}},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			requested := policy.DeepCopy()
			mutate(requested)
			if errs := CheckCompatibility(box, *requested, nativeCapabilities()); len(errs) == 0 {
				t.Fatal("unsupported explicit native policy accepted")
			}
		})
	}
}
