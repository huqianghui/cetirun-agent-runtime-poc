// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

func compilerFixture() CompileInput {
	resources := api.SandboxContainerResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
	}
	return CompileInput{
		AgentName: "example-agent", Namespace: "cetirun-example-agent",
		Resolution: sandbox.Resolution{
			AgentPolicy: api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}}},
			Binding: sandbox.Binding{
				AgentUID: "example-agent-uid", TeamUID: "example-team-uid", SandboxUID: "example-sandbox-uid",
				AgentPolicyUID: "example-policy-uid",
			},
			Revision: "synthetic-foundation",
			Sandbox: api.SandboxSpec{
				Type:      api.SandboxKata,
				Resources: api.SandboxResources{Agent: resources, Helpers: resources},
				Storage: api.SandboxStorage{
					Home:    api.SandboxHomeStorage{StorageClassName: "example-home", Size: resource.MustParse("10Gi"), AccessMode: corev1.ReadWriteOncePod},
					Scratch: api.SandboxScratchStorage{SizeLimit: resource.MustParse("1Gi")},
				},
			},
			Capabilities: sandbox.Capabilities{
				RuntimeClassName: "synthetic-kata", Revision: "synthetic-qualified-only",
				HostResourceLimits: true, ReadOnlyRootFilesystem: true,
				HomeAccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
				IPFamilies:           []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
				Protocols:            []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP},
				GuestExternalNetwork: true, DenyPrecedence: true, NodeTraffic: true, DNSIPRestrictions: true,
				BlockedPorts: true, AuthenticatedIngress: true, DisabledIngress: true, KubernetesAccess: true, EntraID: true,
				EnforcementObservation: true, RestrictiveFailure: true,
			},
		},
		Platform: Platform{
			Adapter: AdapterRegistration{Name: AzureCNIOverlayCilium, Revision: "synthetic-only", Qualified: true, Available: true},
			Topology: Topology{
				Revision: "synthetic-topology", ManagedPeersComplete: true,
				PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
				NodeCIDRs: []string{"192.168.0.0/16"}, ManagementCIDRs: []string{"10.1.0.0/16"},
				MetadataCIDRs:  []string{"169.254.169.254/32"},
				DNSResolverIPs: []string{"10.96.0.10"},
				DNS:            []Peer{{Name: "dns", Namespace: "kube-system", UID: "example-dns-uid", IPs: []string{"10.96.0.10"}}},
				Gateway: []Destination{{
					Peer:  Peer{Name: "gateway", Namespace: "platform", UID: "example-gateway-uid", IPs: []string{"10.96.1.50"}},
					Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}},
				}},
				Ingress: []Destination{{
					Peer:  Peer{Name: "ingress", Namespace: "platform", UID: "example-ingress-uid", IPs: []string{"10.96.1.60"}},
					Ports: []api.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
				}},
				KubernetesAPI: []Destination{{
					Peer:  Peer{Name: "api", UID: "example-api-uid", IPs: []string{"203.0.113.10"}},
					Ports: []api.AgentPolicyPort{{Port: 6443, Protocol: corev1.ProtocolTCP}},
				}},
			},
			KubernetesAudience: "https://kubernetes.default.svc", TokenExpirationSeconds: 600,
		},
	}
}

func mustCompile(t *testing.T, input CompileInput) *Plan {
	t.Helper()
	plan, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func nativeCompilerFixture() CompileInput {
	input := compilerFixture()
	input.NativeExecution = true
	input.Resolution.AgentPolicy.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
	capabilities := &input.Resolution.Capabilities
	capabilities.Profile = sandbox.ProfileNativeSingleStart
	capabilities.EnforcementObservation, capabilities.RestrictiveFailure = false, false
	capabilities.BlockedPorts, capabilities.KubernetesAccess, capabilities.EntraID = false, false, false
	input.Platform.Adapter.Name, input.Platform.Adapter.Qualified = Cilium, false
	input.Platform.KubernetesAudience, input.Platform.TokenExpirationSeconds = "", 0
	return input
}

func TestCompileNativeProfilePreservesNetworkBaseline(t *testing.T) {
	input := nativeCompilerFixture()
	native := mustCompile(t, input)
	if native.Network.Adapter != Cilium || native.Identity.AutomountServiceAccountToken ||
		len(native.Identity.Bindings) != 0 || len(native.Identity.Volumes) != 0 {
		t.Fatal("native compilation mislabeled its adapter or granted identity access")
	}
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "normal", UID: "normal-uid"}, Spec: input.Resolution.AgentPolicy}
	if err := ValidatePolicyConfiguration(policy, input.Resolution.Capabilities, input.Platform); err != nil {
		t.Fatal("bounded standalone policy was rejected:", err)
	}
	input.NativeExecution = false
	input.Resolution.Capabilities.Profile = ""
	input.Resolution.Capabilities.EnforcementObservation, input.Resolution.Capabilities.RestrictiveFailure = true, true
	input.Platform.Adapter.Name, input.Platform.Adapter.Qualified = AzureCNIOverlayCilium, true
	qualified := mustCompile(t, input)
	if native.Revision == qualified.Revision || !reflect.DeepEqual(native.Network.Policy.Spec, qualified.Network.Policy.Spec) {
		t.Fatal("bounded profile must have its own revision without weakening baseline network rules")
	}
}

func TestValidateNativeTeamPolicyBindingChecksAllNativePolicyControls(t *testing.T) {
	input := nativeCompilerFixture()
	input.Resolution.AgentPolicy.Network.BlockedFQDNs = []string{"blocked.example.com"}
	input.Resolution.Capabilities.BlockedFQDNs = false
	policy := &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "native", UID: "native-policy-uid"},
		Spec:       input.Resolution.AgentPolicy,
	}
	err := ValidateNativeTeamPolicyBinding(context.Background(), policy, input.Resolution.Capabilities, input.Platform, nil, true)
	if !errors.Is(err, ErrUnsupportedControl) {
		t.Fatalf("native compatibility omitted unsupported FQDN control: %v", err)
	}
}

func TestCompileNativeProxyProjectionPreservesV1ControlRevision(t *testing.T) {
	input := compilerFixture()
	input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated
	withoutProjection := mustCompile(t, input)
	input.NativeExecution = true
	withProjection := mustCompile(t, input)
	if withProjection.Revision != withoutProjection.Revision || len(withProjection.Network.TrustedProxyIPs) == 0 ||
		!reflect.DeepEqual(withProjection.Resources, withoutProjection.Resources) {
		t.Fatal("derived proxy projection changed the existing v1 control revision or network resources")
	}
	input.Platform.Topology.Ingress[0].Peer.IPs = []string{"10.96.1.61"}
	if changed := mustCompile(t, input); changed.Revision == withProjection.Revision {
		t.Fatal("preserving v1 hashing stopped binding actual ingress topology changes")
	}
}

func TestCompileNativeProfileFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*CompileInput){
		"default mode":     func(i *CompileInput) { i.NativeExecution = false },
		"unknown profile":  func(i *CompileInput) { i.Resolution.Capabilities.Profile = "unknown" },
		"default contract": func(i *CompileInput) { i.Resolution.Capabilities.Profile = "" },
		"unavailable":      func(i *CompileInput) { i.Platform.Adapter.Available = false },
		"qualified claim":  func(i *CompileInput) { i.Platform.Adapter.Qualified = true },
		"Azure label":      func(i *CompileInput) { i.Platform.Adapter.Name = AzureCNIOverlayCilium },
		"observer claim":   func(i *CompileInput) { i.Resolution.Capabilities.EnforcementObservation = true },
		"fencing claim":    func(i *CompileInput) { i.Resolution.Capabilities.RestrictiveFailure = true },
		"non-Kata":         func(i *CompileInput) { i.Resolution.Sandbox.Type = api.SandboxGVisor },
		"Kubernetes access": func(i *CompileInput) {
			i.Resolution.AgentPolicy.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "operations",
				RoleBindings:       []api.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: "view"}},
			}
		},
		"missing inventory": func(i *CompileInput) { i.Platform.Topology.ManagementCIDRs = nil },
		"missing DNS":       func(i *CompileInput) { i.Platform.Topology.DNSResolverIPs = nil },
		"missing UDP":       func(i *CompileInput) { i.Resolution.Capabilities.Protocols = []corev1.Protocol{corev1.ProtocolTCP} },
		"token audience":    func(i *CompileInput) { i.Platform.KubernetesAudience = "https://kubernetes.default.svc" },
		"unqualified blocked ports": func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 18080, Protocol: corev1.ProtocolTCP}}
		},
		"unregistered SCTP port": func(i *CompileInput) {
			i.Resolution.Capabilities.BlockedPorts = true
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 18080, Protocol: corev1.ProtocolSCTP}}
		},
		"custom port": func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 25}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := nativeCompilerFixture()
			mutate(&input)
			if _, err := Compile(input); err == nil {
				t.Fatal("unsupported or unavailable native registration compiled")
			}
		})
	}
	input := compilerFixture()
	input.Platform.Adapter.Name = Cilium
	if _, err := Compile(input); !errors.Is(err, ErrUnsupportedControl) {
		t.Fatal("generic Cilium expanded the default qualified contract:", err)
	}
}

func TestNativeProxyTrustUsesOnlyCanonicalIngressIPs(t *testing.T) {
	input := nativeCompilerFixture()
	input.Platform.Topology.Ingress[0].Peer.IPs = []string{"2001:db8::60", "10.96.1.60", "10.244.0.60", "10.244.0.60"}
	plan := mustCompile(t, input)
	want := []string{"10.244.0.60", "10.96.1.60", "2001:db8::60"}
	if !slices.Equal(plan.Network.TrustedProxyIPs, want) {
		t.Fatalf("proxy trust does not match exact canonical ingress IPs: %v", plan.Network.TrustedProxyIPs)
	}
	input.Platform.Topology.Ingress[0].Peer.IPs = slices.Clone(want)
	if canonical := mustCompile(t, input); canonical.Revision != plan.Revision {
		t.Fatal("peer IP ordering/duplicates changed canonical proxy trust")
	}
	input.Platform.Topology.Ingress[0].Peer.IPs[0] = "10.244.0.61"
	if changed := mustCompile(t, input); changed.Revision == plan.Revision ||
		slices.Equal(changed.Network.TrustedProxyIPs, plan.Network.TrustedProxyIPs) {
		t.Fatal("ingress membership did not change proxy trust and the effective revision")
	}
	input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressDisabled
	if disabled := mustCompile(t, input); len(disabled.Network.TrustedProxyIPs) != 0 {
		t.Fatal("disabled ingress granted proxy trust")
	}
	if qualified := mustCompile(t, compilerFixture()); len(qualified.Network.TrustedProxyIPs) != 0 {
		t.Fatal("default non-native compilation gained proxy trust")
	}
	for _, address := range []string{"0.0.0.0/0", "::/0", "10.244.0.0/16", "0.0.0.0", "::", "127.0.0.1", "::1", "169.254.1.1", "fe80::1", "224.0.0.1", "proxy.example.com"} {
		t.Run(address, func(t *testing.T) {
			input := nativeCompilerFixture()
			input.Platform.Topology.Ingress[0].Peer.IPs = []string{address}
			if _, err := Compile(input); err == nil {
				t.Fatal("non-unicast address or broad proxy trust accepted")
			}
		})
	}
}

func adminFixture() CompileInput {
	input := compilerFixture()
	input.Resolution.AgentPolicy.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName:  "example-dedicated",
		RoleBindings:        []api.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: "view"}},
		ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: "read-nodes"}},
	}
	input.Platform.Identities = []ApprovedIdentity{{
		AgentUID: input.Resolution.Binding.AgentUID, Namespace: input.Namespace, ServiceAccountName: "example-dedicated",
		Revision: "synthetic-identity", Kubernetes: true,
	}}
	input.Platform.Grants = []ApprovedGrant{
		{PolicyUID: input.Resolution.Binding.AgentPolicyUID, Namespace: "operations", RoleUID: "example-view-uid",
			RoleResourceVersion: "1", Revision: "synthetic-view", RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}}},
		{PolicyUID: input.Resolution.Binding.AgentPolicyUID, RoleUID: "example-nodes-uid",
			RoleResourceVersion: "2", Revision: "synthetic-nodes", RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "read-nodes"},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}}}},
	}
	return input
}

func TestControlRevisionUsesV1Domain(t *testing.T) {
	const expectedRevision = "e8c4036fa1c44fca437c8ee5d379937123ed0a9fbbde85115287c01848725c9d"
	plan := mustCompile(t, compilerFixture())
	if plan.Revision != expectedRevision {
		t.Fatalf("v1 control revision = %q, want %q", plan.Revision, expectedRevision)
	}
}

func TestCompileDefaultControls(t *testing.T) {
	input := compilerFixture()
	plan := mustCompile(t, input)
	if plan.Revision == input.Resolution.Revision || len(plan.Revision) != 64 ||
		plan.ResolutionRevision != input.Resolution.Revision || plan.Binding != input.Resolution.Binding {
		t.Fatal("effective and foundation revisions must remain distinct and UID-bound")
	}
	if plan.Identity.ServiceAccountName != "default" || plan.Identity.AutomountServiceAccountToken ||
		plan.Identity.ServiceAccount != nil || len(plan.Identity.Bindings) != 0 || len(plan.Identity.Volumes) != 0 {
		t.Fatal("ordinary identity must have neither a dedicated account nor projected credentials or grants")
	}
	if len(plan.Resources) != 1 || plan.Resources[0].GetKind() != "CiliumNetworkPolicy" ||
		plan.Resources[0].GetAPIVersion() != "cilium.io/v2" {
		t.Fatal("Cilium deny enforcement must not be replaced with allow-only NetworkPolicy")
	}
	if plan.Resources[0].GetAnnotations()[RevisionAnnotation] != plan.Revision {
		t.Fatal("rendered controls do not carry the effective revision")
	}
	gateway := input.Platform.Topology.Gateway[0].Peer
	dns := input.Platform.Topology.DNS[0]
	for _, tc := range []struct {
		name, ip string
		peer     *Peer
		port     int32
		protocol corev1.Protocol
		want     bool
	}{
		{"public TCP", "198.51.100.7", nil, 443, corev1.ProtocolTCP, true},
		{"public IPv6", "2001:db8::7", nil, 443, corev1.ProtocolTCP, true},
		{"private", "10.20.1.7", nil, 443, corev1.ProtocolTCP, false},
		{"management", "10.1.0.7", nil, 443, corev1.ProtocolTCP, false},
		{"metadata", "169.254.169.254", nil, 80, corev1.ProtocolTCP, false},
		{"platform wire server", "168.63.129.16", nil, 80, corev1.ProtocolTCP, false},
		{"IPv6 link local", "fe80::1", nil, 443, corev1.ProtocolTCP, false},
		{"IPv6 private", "fd00::1", nil, 443, corev1.ProtocolTCP, false},
		{"node", "192.168.1.7", nil, 443, corev1.ProtocolTCP, false},
		{"ordinary API", "203.0.113.10", nil, 6443, corev1.ProtocolTCP, false},
		{"required Gateway", gateway.IPs[0], &gateway, 8443, corev1.ProtocolTCP, true},
		{"no hardcoded Gateway port", gateway.IPs[0], &gateway, 4000, corev1.ProtocolTCP, false},
		{"required DNS TCP", dns.IPs[0], &dns, 53, corev1.ProtocolTCP, true},
		{"required DNS UDP", dns.IPs[0], &dns, 53, corev1.ProtocolUDP, true},
		{"DNS not arbitrary ports", dns.IPs[0], &dns, 443, corev1.ProtocolTCP, false},
		{"not public TCP DNS", "198.51.100.7", nil, 53, corev1.ProtocolTCP, false},
		{"not public UDP DNS", "198.51.100.7", nil, 53, corev1.ProtocolUDP, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syntheticEgressAllowed(plan.Network.Policy.Spec, tc.ip, tc.peer, tc.port, tc.protocol); got != tc.want {
				t.Fatalf("synthetic rule evaluation = %v, want %v", got, tc.want)
			}
		})
	}
	spoof := gateway
	spoof.Namespace = "untrusted"
	if syntheticEgressAllowed(plan.Network.Policy.Spec, spoof.IPs[0], &spoof, 8443, corev1.ProtocolTCP) {
		t.Fatal("peer UID in the wrong namespace must not grant Gateway access")
	}
}

func TestCompileDenyPrecedenceAndOverlappingExceptions(t *testing.T) {
	input := compilerFixture()
	input.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
		{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}},
		{CIDR: "10.20.1.0/24", Ports: []api.AgentPolicyPort{{Port: 8080}}},
	}
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"10.20.2.0/24", "198.51.100.0/24"}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 80}, {Port: 5353, Protocol: corev1.ProtocolUDP}}
	managed := Peer{Name: "internal", Namespace: "applications", UID: "example-internal-uid", IPs: []string{"10.20.2.7"}}
	input.Platform.Topology.Peers = []Peer{managed}
	plan := mustCompile(t, input)
	for _, tc := range []struct {
		ip   string
		port int32
		want bool
	}{
		{"10.20.0.7", 443, true}, {"10.20.0.7", 8080, false},
		{"10.20.1.7", 443, true}, {"10.20.1.7", 8080, true},
		{"10.20.2.7", 443, false}, {"198.51.100.7", 443, false},
		{"203.0.113.7", 80, false}, {"203.0.113.7", 443, true},
	} {
		if got := syntheticEgressAllowed(plan.Network.Policy.Spec, tc.ip, nil, tc.port, corev1.ProtocolTCP); got != tc.want {
			t.Errorf("%s:%d = %v, want %v", tc.ip, tc.port, got, tc.want)
		}
	}
	// Another permit cannot cancel a native Cilium deny, including managed peers.
	spec := plan.Network.Policy.Spec
	spec.Egress = append(spec.Egress, EgressRule{ToEntities: []string{"all"}})
	if syntheticEgressAllowed(spec, managed.IPs[0], &managed, 443, corev1.ProtocolTCP) ||
		syntheticEgressAllowed(spec, "198.51.100.7", nil, 443, corev1.ProtocolTCP) ||
		syntheticEgressAllowed(spec, "203.0.113.7", nil, 80, corev1.ProtocolTCP) {
		t.Fatal("a broad permit bypassed explicit deny rules")
	}
}

func TestCompileRequiredConflictsAndUnavailableControls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CompileInput)
		want   error
	}{
		{"Gateway port conflict", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8443}}
		}, ErrConflict},
		{"DNS TCP conflict", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53}}
		}, ErrConflict},
		{"DNS UDP conflict", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolUDP}}
		}, ErrConflict},
		{"Gateway CIDR conflict", func(i *CompileInput) { i.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"10.96.1.0/24"} }, ErrConflict},
		{"node exception", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "192.168.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}}}
		}, ErrConflict},
		{"management exception", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "10.1.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}}}
		}, ErrConflict},
		{"missing DNS", func(i *CompileInput) { i.Platform.Topology.DNS = nil }, ErrUnavailable},
		{"missing Gateway", func(i *CompileInput) { i.Platform.Topology.Gateway = nil }, ErrUnavailable},
		{"missing peer inventory", func(i *CompileInput) { i.Platform.Topology.ManagedPeersComplete = false }, ErrUnavailable},
		{"missing exclusions", func(i *CompileInput) { i.Platform.Topology.NodeCIDRs = nil }, ErrUnavailable},
		{"missing ingress", func(i *CompileInput) { i.Platform.Topology.Ingress = nil }, ErrUnavailable},
		{"unprotected ingress peer", func(i *CompileInput) { i.Platform.Topology.Ingress[0].Peer.Namespace = "" }, ErrUnsupportedControl},
		{"partial managed peer permissions", func(i *CompileInput) {
			i.Platform.Topology.Peers = []Peer{{
				Name: "internal", Namespace: "applications", UID: "example-internal",
				IPs: []string{"10.20.1.7", "10.20.2.7"},
			}}
			i.Resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{
				CIDR: "10.20.1.0/24", Ports: []api.AgentPolicyPort{{Port: 443}},
			}}
		}, ErrUnsupportedControl},
		{"unqualified", func(i *CompileInput) { i.Platform.Adapter.Qualified = false }, ErrUnsupportedControl},
		{"unsupported adapter", func(i *CompileInput) { i.Platform.Adapter.Name = "standard-networkpolicy" }, ErrUnsupportedControl},
		{"adapter unavailable", func(i *CompileInput) { i.Platform.Adapter.Available = false }, ErrUnavailable},
		{"FQDN even with claimed flag", func(i *CompileInput) {
			i.Resolution.Capabilities.BlockedFQDNs = true
			i.Resolution.AgentPolicy.Network.BlockedFQDNs = []string{"blocked.example.com"}
		}, ErrUnsupportedControl},
		{"mTLS even with claimed flag", func(i *CompileInput) {
			i.Resolution.Capabilities.MutualTLS = true
			i.Resolution.AgentPolicy.Network.Ingress.RequireMTLS = true
		}, ErrUnsupportedControl},
		{"DNS needs UDP", func(i *CompileInput) { i.Resolution.Capabilities.Protocols = []corev1.Protocol{corev1.ProtocolTCP} }, ErrUnsupportedControl},
		{"unqualified SCTP", func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 9000, Protocol: corev1.ProtocolSCTP}}
		}, ErrUnsupportedControl},
		{"Gateway on DNS port", func(i *CompileInput) { i.Platform.Topology.Gateway[0].Ports[0].Port = 53 }, ErrConflict},
		{"Gateway on node", func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.IPs = []string{"192.168.1.7"} }, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := compilerFixture()
			tc.mutate(&input)
			plan, err := Compile(input)
			if !errors.Is(err, tc.want) || plan != nil {
				t.Fatalf("expected no partial plan and %v, got %+v, %v", tc.want, plan, err)
			}
		})
	}
	input := adminFixture()
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 6443}}
	if _, err := Compile(input); !errors.Is(err, ErrConflict) {
		t.Fatal("declared API access must reject a required-port conflict", err)
	}
}

func TestCompileCanonicalRevisionAndNoInputMutation(t *testing.T) {
	input := adminFixture()
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"198.51.100.1/24", "203.0.113.128/25"}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 80}, {Port: 25}}
	before, _ := json.Marshal(input)
	first := mustCompile(t, input)
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("compilation mutated a trusted input")
	}
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"203.0.113.128/25", "198.51.100.0/24", "198.51.100.0/24"}
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 25, Protocol: corev1.ProtocolTCP}, {Port: 80, Protocol: corev1.ProtocolTCP}, {Port: 80}}
	slices.Reverse(input.Platform.Grants)
	for i := range input.Platform.Grants {
		slices.Reverse(input.Platform.Grants[i].Rules[0].Verbs)
	}
	slices.Reverse(input.Resolution.Capabilities.Protocols)
	slices.Reverse(input.Resolution.Capabilities.IPFamilies)
	second := mustCompile(t, input)
	if first.Revision != second.Revision || !reflect.DeepEqual(first.Resources, second.Resources) {
		t.Fatal("canonical ordering or duplicate rules changed effective controls")
	}
	input.Resolution.AgentPolicy.SandboxSelection.AllowMultiple = true
	if mustCompile(t, input).Revision != first.Revision {
		t.Fatal("selection-only changes must not change an already selected execution")
	}
	input.Resolution.AgentPolicy.ModelAccess = api.AgentPolicyModelAccess{
		Copilot:       api.AgentPolicyCopilotAccess{Allowed: true},
		GatewayBudget: &api.AgentPolicyGatewayBudget{DefaultMonthlyUSD: "10", MaxMonthlyUSD: "20"},
	}
	input.Resolution.AgentPolicy.AgentIdentity = api.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}
	input.Resolution.AgentPolicy.Skills.Allowed = []string{"unrelated-skill"}
	if mustCompile(t, input).Revision != first.Revision {
		t.Fatal("non-Sandbox policy fields changed compiled Sandbox controls")
	}
}

func TestCompileEffectiveTopologyAndGrantChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CompileInput)
	}{
		{"Gateway port", func(i *CompileInput) { i.Platform.Topology.Gateway[0].Ports[0].Port = 9443 }},
		{"Gateway IP", func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.IPs = []string{"10.96.1.51"} }},
		{"Gateway UID", func(i *CompileInput) { i.Platform.Topology.Gateway[0].Peer.UID = "replacement-gateway" }},
		{"topology revision", func(i *CompileInput) { i.Platform.Topology.Revision = "synthetic-new-topology" }},
		{"node inventory", func(i *CompileInput) {
			i.Platform.Topology.NodeCIDRs = append(i.Platform.Topology.NodeCIDRs, "198.51.100.0/24")
		}},
		{"role contents without version change", func(i *CompileInput) { i.Platform.Grants[0].Rules[0].Resources = []string{"configmaps"} }},
		{"role recreated", func(i *CompileInput) { i.Platform.Grants[0].RoleUID = "replacement-role" }},
		{"role resourceVersion", func(i *CompileInput) { i.Platform.Grants[0].RoleResourceVersion = "3" }},
		{"identity approval", func(i *CompileInput) { i.Platform.Identities[0].Revision = "replacement-identity" }},
		{"API audience", func(i *CompileInput) { i.Platform.KubernetesAudience = "https://api.example.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := adminFixture()
			before := mustCompile(t, input)
			tc.mutate(&input)
			after := mustCompile(t, input)
			if before.Revision == after.Revision {
				t.Fatal("effective trusted input change did not change revision")
			}
		})
	}
	input := compilerFixture()
	input.Platform.Topology.Gateway[0].Ports[0].Port = 9443
	input.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 9443}}
	if _, err := Compile(input); !errors.Is(err, ErrConflict) {
		t.Fatal("endpoint churn must recheck required access", err)
	}
}

func TestCompileIdentityModes(t *testing.T) {
	for _, mode := range []string{"ordinary", "entra", "admin", "combined"} {
		t.Run(mode, func(t *testing.T) {
			input := compilerFixture()
			admin, entra := mode == "admin" || mode == "combined", mode == "entra" || mode == "combined"
			if admin {
				input = adminFixture()
			}
			if entra {
				input.Resolution.AgentPolicy.EntraID.Enabled = true
				if len(input.Platform.Identities) == 0 {
					input.Platform.Identities = []ApprovedIdentity{{
						AgentUID: input.Resolution.Binding.AgentUID, Namespace: input.Namespace,
						ServiceAccountName: "example-dedicated", Revision: "synthetic-entra",
					}}
				}
				input.Platform.Identities[0].Entra = &EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"}
			}
			plan := mustCompile(t, input)
			if plan.Identity.AutomountServiceAccountToken ||
				(plan.Identity.ServiceAccount != nil && *plan.Identity.ServiceAccount.AutomountServiceAccountToken) {
				t.Fatal("automatic Kubernetes token mounting must always remain disabled")
			}
			audiences := map[string]bool{}
			for _, volume := range plan.Identity.Volumes {
				for _, source := range volume.Projected.Sources {
					if token := source.ServiceAccountToken; token != nil {
						if *token.ExpirationSeconds != 600 {
							t.Fatal("tokens must be explicitly short-lived")
						}
						audiences[token.Audience] = true
					}
				}
			}
			if audiences[input.Platform.KubernetesAudience] != admin || audiences[EntraAudience] != entra {
				t.Fatalf("identity projections crossed modes: %v", audiences)
			}
			if len(plan.Identity.Bindings) != 0 && !admin || admin && len(plan.Identity.Bindings) != 2 {
				t.Fatal("explicit Kubernetes grants must be confined to the administrative identity mode")
			}
			if entra && len(plan.Identity.Env) != 3 || !entra && len(plan.Identity.Env) != 0 {
				t.Fatal("Entra token exchange environment crossed modes")
			}
			if admin && !syntheticEgressAllowed(plan.Network.Policy.Spec, "203.0.113.10", nil, 6443, corev1.ProtocolTCP) {
				t.Fatal("approved admin identity lacks required API destination access")
			}
			if !admin && syntheticEgressAllowed(plan.Network.Policy.Spec, "203.0.113.10", nil, 6443, corev1.ProtocolTCP) {
				t.Fatal("ordinary or Entra-only identity gained API network access")
			}
			for _, object := range plan.Resources {
				if object.GetKind() == "Role" || object.GetKind() == "ClusterRole" {
					t.Fatal("shared roles must never be part of per-Agent chart ownership")
				}
			}
		})
	}
}

func TestCompileRejectsUnapprovedAndIncompatibleIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CompileInput)
	}{
		{"no approvals", func(i *CompileInput) { i.Platform.Grants = nil }},
		{"wrong policy approval", func(i *CompileInput) { i.Platform.Grants[0].PolicyUID = "foreign-policy" }},
		{"wrong namespace scope", func(i *CompileInput) { i.Platform.Grants[0].Namespace = "foreign-namespace" }},
		{"Role is not ClusterRole alias", func(i *CompileInput) { i.Platform.Grants[0].RoleRef.Kind = "Role" }},
		{"missing role contents", func(i *CompileInput) { i.Platform.Grants[0].Rules = nil }},
		{"missing role identity", func(i *CompileInput) { i.Platform.Grants[0].RoleUID = "" }},
		{"missing role resourceVersion", func(i *CompileInput) { i.Platform.Grants[0].RoleResourceVersion = "" }},
		{"duplicate grant", func(i *CompileInput) { i.Platform.Grants = append(i.Platform.Grants, i.Platform.Grants[0]) }},
		{"no per-Agent identity", func(i *CompileInput) { i.Platform.Identities = nil }},
		{"wrong Agent identity", func(i *CompileInput) { i.Platform.Identities[0].AgentUID = "foreign-agent" }},
		{"shared default account", func(i *CompileInput) {
			i.Platform.Identities[0].ServiceAccountName = "default"
			i.Resolution.AgentPolicy.KubernetesAccess.ServiceAccountName = "default"
		}},
		{"unapproved Kubernetes identity", func(i *CompileInput) { i.Platform.Identities[0].Kubernetes = false }},
		{"incompatible account names", func(i *CompileInput) { i.Platform.Identities[0].ServiceAccountName = "other-account" }},
		{"exchange audience is not API audience", func(i *CompileInput) { i.Platform.KubernetesAudience = EntraAudience }},
		{"unbounded token", func(i *CompileInput) { i.Platform.TokenExpirationSeconds = 7200 }},
		{"unapproved Entra", func(i *CompileInput) { i.Resolution.AgentPolicy.EntraID.Enabled = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := adminFixture()
			tc.mutate(&input)
			if plan, err := Compile(input); !errors.Is(err, ErrUnapprovedGrant) || plan != nil {
				t.Fatalf("expected explicit identity rejection, got %+v, %v", plan, err)
			}
		})
	}
	input := compilerFixture()
	input.Resolution.AgentPolicy.EntraID.Enabled = true
	input.Platform.Identities = []ApprovedIdentity{{
		AgentUID: input.Resolution.Binding.AgentUID, Namespace: input.Namespace,
		ServiceAccountName: "example-entra-only", Revision: "synthetic-entra",
		Entra: &EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"},
	}}
	input.Platform.KubernetesAudience = EntraAudience
	if _, err := Compile(input); !errors.Is(err, ErrUnapprovedGrant) {
		t.Fatal("Entra-only mode must also reject an API audience accepting the exchange token", err)
	}
}

func TestCompileIngressModesAndMergedPeerPorts(t *testing.T) {
	input := compilerFixture()
	first := input.Platform.Topology.Ingress[0]
	first.Ports = []api.AgentPolicyPort{{Port: 9090, Protocol: corev1.ProtocolTCP}}
	input.Platform.Topology.Ingress = append(input.Platform.Topology.Ingress, first)
	plan := mustCompile(t, input)
	if len(plan.Network.Policy.Spec.Ingress) != 1 || len(plan.Network.Policy.Spec.Ingress[0].ToPorts[0].Ports) != 2 {
		t.Fatal("same authenticated peer ports must merge before complementary denies")
	}
	input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressDisabled
	input.Platform.Topology.Ingress = nil
	disabled := mustCompile(t, input)
	if len(disabled.Network.Policy.Spec.Ingress) != 0 || len(disabled.Network.Policy.Spec.IngressDeny) != 1 ||
		!slices.Contains(disabled.Network.Policy.Spec.IngressDeny[0].FromEntities, "all") {
		t.Fatal("disabled ingress must explicitly deny every inbound peer")
	}
	gateway := input.Platform.Topology.Gateway[0].Peer
	if !syntheticEgressAllowed(disabled.Network.Policy.Spec, gateway.IPs[0], &gateway, 8443, corev1.ProtocolTCP) {
		t.Fatal("disabled ingress must retain required outbound Gateway access")
	}
	if plan.Revision == disabled.Revision {
		t.Fatal("ingress mode must affect the compiled revision")
	}
}

// This evaluator checks generated rule intent only. It is not a Cilium runtime,
// schema/managed-version acceptance test, or live dataplane qualification.
func syntheticEgressAllowed(spec CiliumPolicySpec, rawIP string, peer *Peer, port int32, protocol corev1.Protocol) bool {
	matches := func(rule EgressRule) bool {
		l3 := slices.Contains(rule.ToEntities, "all") || peer == nil && slices.Contains(rule.ToEntities, "world")
		if peer == nil {
			ip := netip.MustParseAddr(rawIP)
			for _, cidr := range rule.ToCIDRSet {
				matched := netip.MustParsePrefix(cidr.CIDR).Contains(ip)
				for _, except := range cidr.Except {
					matched = matched && !netip.MustParsePrefix(except).Contains(ip)
				}
				l3 = l3 || matched
			}
		}
		set := map[string]string{}
		if peer != nil {
			set["k8s:io.kubernetes.pod.namespace"] = peer.Namespace
			if peer.UID != "" {
				set["k8s:"+PeerUIDLabel] = string(peer.UID)
			}
			for key, value := range peer.PodSelector {
				set["k8s:"+key] = value
			}
		}
		for _, selected := range rule.ToEndpoints {
			l3 = l3 || syntheticSelectorMatches(selected, set)
		}
		if !l3 {
			return false
		}

		return syntheticPortsMatch(rule.ToPorts, port, protocol)
	}
	for _, rule := range spec.EgressDeny {
		if matches(rule) {
			return false
		}
	}
	for _, rule := range spec.Egress {
		if matches(rule) {
			return true
		}
	}
	return false
}

func syntheticPortsMatch(groups []PortRule, port int32, protocol corev1.Protocol) bool {
	if len(groups) == 0 {
		return true
	}
	for _, group := range groups {
		for _, allowed := range group.Ports {
			start, _ := strconv.Atoi(allowed.Port)
			end := allowed.EndPort
			if end == 0 {
				end = int32(start)
			}
			if protocol == allowed.Protocol && port >= int32(start) && port <= end {
				return true
			}
		}
	}
	return false
}

func syntheticSelectorMatches(selector metav1.LabelSelector, values map[string]string) bool {
	for key, expected := range selector.MatchLabels {
		if actual, exists := values[key]; !exists || actual != expected {
			return false
		}
	}
	for _, expression := range selector.MatchExpressions {
		actual, exists := values[expression.Key]
		switch expression.Operator {
		case metav1.LabelSelectorOpExists:
			if !exists {
				return false
			}
		case metav1.LabelSelectorOpNotIn:
			if exists && slices.Contains(expression.Values, actual) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
