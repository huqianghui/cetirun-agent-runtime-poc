// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"fmt"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

const ProfileNativeSingleStart = "native-single-start"

// Capabilities is trusted platform registration, not an Agent-controlled input.
// An empty Profile retains the qualified contract. The native profile declares
// only bounded Kubernetes support, not external observation or physical fencing.
type Capabilities struct {
	Profile                           string                              `json:"profile,omitempty"`
	RuntimeClassName                  string                              `json:"runtimeClassName"`
	Revision                          string                              `json:"revision"`
	HostResourceLimits                bool                                `json:"hostResourceLimits"`
	PerContainerResourceLimits        bool                                `json:"perContainerResourceLimits"`
	RequirePerContainerResourceLimits bool                                `json:"requirePerContainerResourceLimits"`
	ReadOnlyRootFilesystem            bool                                `json:"readOnlyRootFilesystem"`
	HomeAccessModes                   []corev1.PersistentVolumeAccessMode `json:"homeAccessModes"`
	IPFamilies                        []corev1.IPFamily                   `json:"ipFamilies"`
	Protocols                         []corev1.Protocol                   `json:"protocols"`
	GuestExternalNetwork              bool                                `json:"guestExternalNetwork"`
	DenyPrecedence                    bool                                `json:"denyPrecedence"`
	NodeTraffic                       bool                                `json:"nodeTraffic"`
	DNSIPRestrictions                 bool                                `json:"dnsIPRestrictions"`
	BlockedPorts                      bool                                `json:"blockedPorts"`
	BlockedFQDNs                      bool                                `json:"blockedFQDNs"`
	AuthenticatedIngress              bool                                `json:"authenticatedIngress"`
	DisabledIngress                   bool                                `json:"disabledIngress"`
	MutualTLS                         bool                                `json:"mutualTLS"`
	KubernetesAccess                  bool                                `json:"kubernetesAccess"`
	EntraID                           bool                                `json:"entraID"`
	EnforcementObservation            bool                                `json:"enforcementObservation"`
	RestrictiveFailure                bool                                `json:"restrictiveFailure"`
}

type Registration struct {
	Capabilities Capabilities
	Available    bool
}

// CheckExecutionProfile prevents a native-only registration from authorizing
// the default held/externally observed execution path.
func CheckExecutionProfile(capabilities Capabilities, nativeExecution bool) error {
	if errs := CheckCapabilityProfile(capabilities); len(errs) != 0 {
		return errs.ToAggregate()
	}
	if capabilities.Profile == ProfileNativeSingleStart && !nativeExecution {
		return fmt.Errorf("registration profile %q requires explicit nativeExecution", ProfileNativeSingleStart)
	}
	return nil
}

func CheckAgentProfile(capabilities Capabilities, agentType cetirunv1alpha1.AgentType) error {
	if errs := CheckCapabilityProfile(capabilities); len(errs) != 0 {
		return errs.ToAggregate()
	}
	if capabilities.Profile == ProfileNativeSingleStart && agentType != cetirunv1alpha1.AgentTypeOpenClaw &&
		agentType != cetirunv1alpha1.AgentTypeHermes && agentType != cetirunv1alpha1.AgentTypeCustom {
		return fmt.Errorf("native execution requires a supported Agent harness")
	}
	return nil
}

func CheckCapabilityProfile(capabilities Capabilities) field.ErrorList {
	root := field.NewPath("registration")
	switch capabilities.Profile {
	case "":
		return nil
	case ProfileNativeSingleStart:
	default:
		return field.ErrorList{field.NotSupported(root.Child("profile"), capabilities.Profile, []string{"", ProfileNativeSingleStart})}
	}
	var errs field.ErrorList
	if !capabilities.HostResourceLimits {
		errs = append(errs, field.Required(root.Child("hostResourceLimits"), "native runtime resource limits must be supported"))
	}
	if len(capabilities.HomeAccessModes) == 0 {
		errs = append(errs, field.Required(root.Child("homeAccessModes"), "native HOME requires an explicitly supported access mode"))
	}
	for i, mode := range capabilities.HomeAccessModes {
		path := root.Child("homeAccessModes").Index(i)
		switch mode {
		case corev1.ReadWriteOncePod, corev1.ReadWriteOnce:
			if slices.Contains(capabilities.HomeAccessModes[:i], mode) {
				errs = append(errs, field.Duplicate(path, mode))
			}
		default:
			errs = append(errs, field.NotSupported(path, mode, []string{string(corev1.ReadWriteOncePod), string(corev1.ReadWriteOnce)}))
		}
	}
	for _, control := range []struct {
		name     string
		declared bool
	}{
		{"enforcementObservation", capabilities.EnforcementObservation},
		{"restrictiveFailure", capabilities.RestrictiveFailure},
	} {
		if control.declared {
			errs = append(errs, field.Forbidden(root.Child(control.name), "not a guarantee of the native single-start profile"))
		}
	}
	return errs
}

// CheckRuntimeCapabilities binds the common profile to backend-specific resource
// semantics. Container support does not qualify gVisor or strengthen Kata quotas.
func CheckRuntimeCapabilities(kind cetirunv1alpha1.SandboxType, capabilities Capabilities) field.ErrorList {
	errs := CheckCapabilityProfile(capabilities)
	root := field.NewPath("registration")
	switch kind {
	case cetirunv1alpha1.SandboxKata, cetirunv1alpha1.SandboxGVisor:
		if capabilities.Profile == ProfileNativeSingleStart {
			if kind != cetirunv1alpha1.SandboxKata {
				errs = append(errs, field.Forbidden(field.NewPath("spec", "type"), "native execution requires Kata or container"))
			}
			if capabilities.PerContainerResourceLimits || capabilities.RequirePerContainerResourceLimits {
				errs = append(errs, field.Forbidden(root.Child("perContainerResourceLimits"), "independent container quotas are not a guarantee of native Kata"))
			}
		}
	case cetirunv1alpha1.SandboxContainer:
		if capabilities.Profile != ProfileNativeSingleStart {
			errs = append(errs, field.Forbidden(root.Child("profile"), "container requires the native common executor"))
		}
		if !capabilities.PerContainerResourceLimits {
			errs = append(errs, field.Required(root.Child("perContainerResourceLimits"), "container requires ordinary per-container resource limits"))
		}
	default:
		errs = append(errs, field.NotSupported(field.NewPath("spec", "type"), kind, []string{"kata", "gvisor", "container"}))
	}
	return errs
}

func CheckNetworkCapabilities(capabilities Capabilities) field.ErrorList {
	errs := CheckCapabilityProfile(capabilities)
	for _, requirement := range []struct {
		name          string
		supported     bool
		qualifiedOnly bool
	}{
		{"guestExternalNetwork", capabilities.GuestExternalNetwork, false},
		{"denyPrecedence", capabilities.DenyPrecedence, false},
		{"nodeTraffic", capabilities.NodeTraffic, false},
		{"dnsIPRestrictions", capabilities.DNSIPRestrictions, false},
		{"enforcementObservation", capabilities.EnforcementObservation, true},
		{"restrictiveFailure", capabilities.RestrictiveFailure, true},
	} {
		if requirement.qualifiedOnly && capabilities.Profile == ProfileNativeSingleStart {
			continue
		}
		if !requirement.supported {
			errs = append(errs, field.Required(field.NewPath("registration", requirement.name), "mandatory control is not supported by the selected profile"))
		}
	}
	return errs
}

// CheckNativePolicy bounds configuration support independently of declarations
// for optional controls. It does not authorize an Agent or release a hold.
func CheckNativePolicy(policy cetirunv1alpha1.AgentPolicySpec) field.ErrorList {
	root := field.NewPath("spec")
	var errs field.ErrorList
	for _, kind := range policy.Harnesses.Allowed {
		if kind != cetirunv1alpha1.AgentTypeOpenClaw && kind != cetirunv1alpha1.AgentTypeHermes && kind != cetirunv1alpha1.AgentTypeCustom {
			errs = append(errs, field.Forbidden(root.Child("harnesses", "allowed"), "native execution requires supported harnesses"))
		}
	}
	errs = append(errs, CheckNativeNetworkPolicy(policy.Network)...)
	return errs
}

// CheckNativeNetworkPolicy bounds native network controls. Compatibility and
// compilation still require explicit protocol/capability registration, qualified
// address families and no conflicts with required destinations.
func CheckNativeNetworkPolicy(network cetirunv1alpha1.AgentPolicyNetwork) field.ErrorList {
	root := field.NewPath("spec", "network")
	var errs field.ErrorList
	checkPorts := func(path *field.Path, ports []cetirunv1alpha1.AgentPolicyPort) {
		for i, port := range ports {
			if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP &&
				port.Protocol != corev1.ProtocolUDP && port.Protocol != corev1.ProtocolSCTP {
				errs = append(errs, field.NotSupported(path.Index(i).Child("protocol"),
					port.Protocol, []string{string(corev1.ProtocolTCP), string(corev1.ProtocolUDP), string(corev1.ProtocolSCTP)}))
			}
		}
	}
	checkPorts(root.Child("blockedPorts"), network.BlockedPorts)
	for i, rule := range network.AllowedInternalCIDRs {
		checkPorts(root.Child("allowedInternalCIDRs").Index(i).Child("ports"), rule.Ports)
	}
	return errs
}

// CheckCompatibility checks declared support, not runtime health or enforcement.
func CheckCompatibility(sandbox cetirunv1alpha1.SandboxSpec, policy cetirunv1alpha1.AgentPolicySpec, capabilities Capabilities) field.ErrorList {
	sandbox, errs := NormalizeSandbox(sandbox)
	policy, policyErrs := NormalizeAgentPolicy(policy)
	errs = append(errs, policyErrs...)
	if len(errs) != 0 {
		return errs
	}

	root := field.NewPath("registration")
	if problems := validation.IsDNS1123Subdomain(capabilities.RuntimeClassName); len(problems) != 0 {
		errs = append(errs, field.Invalid(root.Child("runtimeClassName"), capabilities.RuntimeClassName, "must name a registered RuntimeClass"))
	}
	if capabilities.Revision == "" {
		errs = append(errs, field.Required(root.Child("revision"), "a platform revision is required"))
	}
	errs = append(errs, CheckNetworkCapabilities(capabilities)...)
	errs = append(errs, CheckRuntimeCapabilities(sandbox.Type, capabilities)...)
	if !capabilities.HostResourceLimits {
		errs = append(errs, field.Required(root.Child("hostResourceLimits"), "mandatory control is not qualified"))
	}
	if capabilities.Profile == ProfileNativeSingleStart {
		errs = append(errs, CheckNativePolicy(policy)...)
	}
	if capabilities.RequirePerContainerResourceLimits && !capabilities.PerContainerResourceLimits {
		errs = append(errs, field.Required(root.Child("perContainerResourceLimits"), "the platform requires independent container guarantees"))
	}
	if !slices.Contains(capabilities.HomeAccessModes, sandbox.Storage.Home.AccessMode) {
		errs = append(errs, field.Invalid(field.NewPath("spec", "storage", "home", "accessMode"), sandbox.Storage.Home.AccessMode, "writer exclusion is not qualified for this access mode"))
	}
	if *policy.Security.ReadOnlyRootFilesystem && !capabilities.ReadOnlyRootFilesystem {
		errs = append(errs, field.Required(root.Child("readOnlyRootFilesystem"), "read-only root filesystems are not qualified"))
	}
	if len(capabilities.IPFamilies) == 0 {
		errs = append(errs, field.Required(root.Child("ipFamilies"), "at least one qualified address family is required"))
	}
	for i, family := range capabilities.IPFamilies {
		if family != corev1.IPv4Protocol && family != corev1.IPv6Protocol {
			errs = append(errs, field.NotSupported(root.Child("ipFamilies").Index(i), family, []string{"IPv4", "IPv6"}))
		}
	}
	if !slices.Contains(capabilities.Protocols, corev1.ProtocolTCP) {
		errs = append(errs, field.Required(root.Child("protocols"), "TCP must be qualified"))
	}
	for i, protocol := range capabilities.Protocols {
		if protocol != corev1.ProtocolTCP && protocol != corev1.ProtocolUDP && protocol != corev1.ProtocolSCTP {
			errs = append(errs, field.NotSupported(root.Child("protocols").Index(i), protocol, []string{"TCP", "UDP", "SCTP"}))
		}
	}
	checkCIDR := func(cidr string, path *field.Path) {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			errs = append(errs, field.Invalid(path, cidr, err.Error()))
			return
		}
		family := corev1.IPv6Protocol
		if prefix.Addr().Is4() {
			family = corev1.IPv4Protocol
		}
		if !slices.Contains(capabilities.IPFamilies, family) {
			errs = append(errs, field.Invalid(path, cidr, "address family is not qualified"))
		}
	}
	checkPorts := func(ports []cetirunv1alpha1.AgentPolicyPort, path *field.Path) {
		for i, port := range ports {
			if !slices.Contains(capabilities.Protocols, port.Protocol) {
				errs = append(errs, field.Invalid(path.Index(i).Child("protocol"), port.Protocol, "protocol is not qualified"))
			}
		}
	}
	network := field.NewPath("spec", "network")
	for i, cidr := range policy.Network.BlockedCIDRs {
		checkCIDR(cidr, network.Child("blockedCIDRs").Index(i))
	}
	for i, rule := range policy.Network.AllowedInternalCIDRs {
		checkCIDR(rule.CIDR, network.Child("allowedInternalCIDRs").Index(i).Child("cidr"))
		checkPorts(rule.Ports, network.Child("allowedInternalCIDRs").Index(i).Child("ports"))
	}
	checkPorts(policy.Network.BlockedPorts, network.Child("blockedPorts"))
	if len(policy.Network.BlockedPorts) > 0 && !capabilities.BlockedPorts {
		errs = append(errs, field.Required(root.Child("blockedPorts"), "destination-port blocking is not qualified"))
	}
	if len(policy.Network.BlockedFQDNs) > 0 && !capabilities.BlockedFQDNs {
		errs = append(errs, field.Required(root.Child("blockedFQDNs"), "observed-SNI blocking is not registered"))
	}
	switch policy.Network.Ingress.Mode {
	case cetirunv1alpha1.AgentPolicyIngressAuthenticated:
		if !capabilities.AuthenticatedIngress {
			errs = append(errs, field.Required(root.Child("authenticatedIngress"), "authenticated ingress is not qualified"))
		}
	case cetirunv1alpha1.AgentPolicyIngressDisabled:
		if !capabilities.DisabledIngress {
			errs = append(errs, field.Required(root.Child("disabledIngress"), "route and inbound-permission removal is not qualified"))
		}
	}
	if policy.Network.Ingress.RequireMTLS && !capabilities.MutualTLS {
		errs = append(errs, field.Required(root.Child("mutualTLS"), "managed Gateway-to-Agent mutual TLS is not registered"))
	}
	if policy.KubernetesAccess != nil && !capabilities.KubernetesAccess {
		errs = append(errs, field.Required(root.Child("kubernetesAccess"), "administrative Kubernetes access is not qualified"))
	}
	if policy.EntraID.Enabled && !capabilities.EntraID {
		errs = append(errs, field.Required(root.Child("entraID"), "workload identity is not qualified"))
	}
	return errs
}

func canonicalCapabilities(input Capabilities) Capabilities {
	input.HomeAccessModes = slices.Clone(input.HomeAccessModes)
	slices.Sort(input.HomeAccessModes)
	input.HomeAccessModes = slices.Compact(input.HomeAccessModes)
	input.IPFamilies = slices.Clone(input.IPFamilies)
	slices.Sort(input.IPFamilies)
	input.IPFamilies = slices.Compact(input.IPFamilies)
	input.Protocols = slices.Clone(input.Protocols)
	slices.Sort(input.Protocols)
	input.Protocols = slices.Compact(input.Protocols)
	return input
}
