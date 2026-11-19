// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

var privateCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "fc00::/7", "fe80::/10", "ff00::/8",
}

var mandatoryCIDRs = []string{
	"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "168.63.129.16/32", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "fe80::/10", "ff00::/8",
}

var dnsPorts = []api.AgentPolicyPort{
	{Port: 53, Protocol: corev1.ProtocolTCP},
	{Port: 53, Protocol: corev1.ProtocolUDP},
}

type addressGrant struct {
	Prefix netip.Prefix
	Ports  []api.AgentPolicyPort
}

// Cilium validates both allow and deny toPorts[].ports arrays against this limit.
const maxPortsPerRule = 40

func canonicalCustomIngress(input *CustomIngress) (*CustomIngress, error) {
	if input == nil {
		return nil, nil
	}
	if input.Port < 1 || input.Port > 65535 || (!input.WebUI && len(input.ChatCallers) == 0) {
		return nil, fmt.Errorf("%w: Custom ingress requires a typed HTTP port and an exposure mode", ErrUnsupportedControl)
	}
	copy := *input
	copy.ChatCallers = nil
	for _, caller := range input.ChatCallers {
		if len(validation.IsDNS1123Label(caller.Namespace)) != 0 || len(caller.PodLabels) == 0 || len(caller.PodLabels) > 16 {
			return nil, fmt.Errorf("%w: Custom Chat callers require a bounded namespace and Pod selector", ErrUnsupportedControl)
		}
		labels := make(map[string]string, len(caller.PodLabels))
		for key, value := range caller.PodLabels {
			if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 || value == "" || key == "io.kubernetes.pod.namespace" {
				return nil, fmt.Errorf("%w: Custom Chat caller selector is invalid", ErrUnsupportedControl)
			}
			labels[key] = value
		}
		copy.ChatCallers = append(copy.ChatCallers, IngressCaller{Namespace: caller.Namespace, PodLabels: labels})
	}
	var err error
	copy.ChatCallers, err = canonicalObjects(copy.ChatCallers)
	if err != nil {
		return nil, err
	}
	return &copy, nil
}

func canonicalTerminalIngress(input *TerminalIngress) (*TerminalIngress, error) {
	if input == nil {
		return nil, nil
	}
	if input.Port < 1024 || input.Port > 65535 {
		return nil, fmt.Errorf("%w: Custom terminal requires an unprivileged TCP port", ErrUnsupportedControl)
	}
	return &TerminalIngress{Port: input.Port}, nil
}

func canonicalPlatform(input Platform, capabilities sandbox.Capabilities) (Platform, error) {
	var platform Platform
	if err := validateControlInput(api.AgentPolicySpec{}, input); err != nil {
		return platform, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return platform, err
	}
	if err := json.Unmarshal(encoded, &platform); err != nil {
		return platform, err
	}
	adapter := platform.Adapter
	if errs := sandbox.CheckCapabilityProfile(capabilities); len(errs) != 0 {
		return platform, fmt.Errorf("%w: %v", ErrUnsupportedControl, errs.ToAggregate())
	}
	if capabilities.Profile == sandbox.ProfileNativeSingleStart {
		if adapter.Name != Cilium || adapter.Qualified || adapter.Revision == "" {
			return platform, fmt.Errorf("%w: native single-start requires an explicitly revisioned cilium adapter without full qualification", ErrUnsupportedControl)
		}
		if errs := sandbox.CheckNetworkCapabilities(capabilities); len(errs) != 0 {
			return platform, fmt.Errorf("%w: %v", ErrUnsupportedControl, errs.ToAggregate())
		}
		if !capabilities.KubernetesAccess && len(platform.Grants) != 0 {
			return platform, fmt.Errorf("%w: native Kubernetes grants require explicitly registered support", ErrUnsupportedControl)
		}
		if len(platform.Identities) != 0 || platform.KubernetesAudience != "" || platform.TokenExpirationSeconds != 0 {
			if !capabilities.KubernetesAccess && !capabilities.EntraID {
				return platform, fmt.Errorf("%w: native identity requires explicitly registered Kubernetes or Entra support", ErrUnsupportedControl)
			}
			if err := ValidateIdentityApprovals(platform.Identities); err != nil {
				return platform, err
			}
			for _, identity := range platform.Identities {
				if (identity.Kubernetes && !capabilities.KubernetesAccess) || (identity.Entra != nil && !capabilities.EntraID) {
					return platform, fmt.Errorf("%w: native identity exceeds its registered Kubernetes or Entra support", ErrUnsupportedControl)
				}
			}
		}
	} else if adapter.Name != AzureCNIOverlayCilium || !adapter.Qualified || adapter.Revision == "" {
		return platform, fmt.Errorf("%w: no qualified candidate adapter registration", ErrUnsupportedControl)
	}
	if capabilities.BlockedFQDNs {
		if capabilities.Profile != sandbox.ProfileNativeSingleStart || platform.EgressSNI == nil {
			return platform, fmt.Errorf("%w: observed-SNI capture requires the common executor and explicit image registration", ErrUnsupportedControl)
		}
		if err := platform.EgressSNI.Validate(); err != nil {
			return platform, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if platform.IngressMTLS != nil && platform.EgressSNI.EnvoyImage != platform.IngressMTLS.Image {
			return platform, fmt.Errorf("%w: ingress and egress registrations must share one Envoy image", ErrConflict)
		}
	} else if platform.EgressSNI != nil {
		return platform, fmt.Errorf("%w: observed-SNI registration requires declared capture support", ErrUnsupportedControl)
	}
	if capabilities.MutualTLS && capabilities.Profile != sandbox.ProfileNativeSingleStart {
		return platform, fmt.Errorf("%w: this adapter has no qualified ingress mTLS implementation", ErrUnsupportedControl)
	}
	if platform.IngressMTLS != nil {
		if capabilities.Profile != sandbox.ProfileNativeSingleStart || !capabilities.MutualTLS {
			return platform, fmt.Errorf("%w: mTLS registration requires native endpoint support", ErrUnsupportedControl)
		}
		if err := platform.IngressMTLS.Validate(); err != nil {
			return platform, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	} else if capabilities.MutualTLS {
		return platform, fmt.Errorf("%w: mTLS support requires explicit certificate and endpoint registration", ErrUnavailable)
	}
	if !adapter.Available {
		return platform, fmt.Errorf("%w: the registered network adapter is unavailable", ErrUnavailable)
	}
	if !slices.Contains(capabilities.Protocols, corev1.ProtocolTCP) || !slices.Contains(capabilities.Protocols, corev1.ProtocolUDP) {
		return platform, fmt.Errorf("%w: DNS requires both TCP and UDP", ErrUnsupportedControl)
	}
	if len(capabilities.IPFamilies) == 0 {
		return platform, fmt.Errorf("%w: at least one IP family is required", ErrUnsupportedControl)
	}
	for _, family := range capabilities.IPFamilies {
		if family != corev1.IPv4Protocol && family != corev1.IPv6Protocol {
			return platform, fmt.Errorf("%w: unknown IP family in registration", ErrUnsupportedControl)
		}
	}
	for _, protocol := range capabilities.Protocols {
		if !slices.Contains([]corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP}, protocol) {
			return platform, fmt.Errorf("%w: unknown protocol in registration", ErrUnsupportedControl)
		}
	}
	topology := &platform.Topology
	if topology.Revision == "" || !topology.ManagedPeersComplete {
		return platform, fmt.Errorf("%w: a revisioned complete managed-peer inventory is required", ErrUnavailable)
	}
	for _, inventory := range []struct {
		name   string
		values *[]string
	}{
		{"pod", &topology.PodCIDRs}, {"service", &topology.ServiceCIDRs}, {"node", &topology.NodeCIDRs},
		{"management", &topology.ManagementCIDRs}, {"metadata", &topology.MetadataCIDRs},
	} {
		name, values := inventory.name, inventory.values
		if len(*values) == 0 {
			return platform, fmt.Errorf("%w: %s exclusion inventory is required", ErrUnavailable, name)
		}
		for i, value := range *values {
			prefix, err := parsePrefix(value, capabilities)
			if err != nil {
				return platform, err
			}
			(*values)[i] = prefix.String()
		}
		*values = sortedUnique(*values)
	}
	if len(topology.DNS) == 0 || len(topology.Gateway) == 0 {
		return platform, fmt.Errorf("%w: required DNS and Gateway bindings must be resolved", ErrUnavailable)
	}
	normalizeIP := func(raw string) (string, error) {
		address, err := netip.ParseAddr(raw)
		if err != nil || address.Is4In6() || address.Zone() != "" {
			return "", fmt.Errorf("%w: a resolved endpoint contains an invalid IP address", ErrUnavailable)
		}
		if !supportedFamily(address, capabilities) {
			return "", fmt.Errorf("%w: a resolved IP family is not qualified", ErrUnsupportedControl)
		}
		return address.String(), nil
	}
	normalizePeer := func(peer *Peer) error {
		if len(validation.IsDNS1123Subdomain(peer.Name)) != 0 || peer.UID == "" ||
			len(validation.IsValidLabelValue(string(peer.UID))) != 0 ||
			(peer.Namespace != "" && len(validation.IsDNS1123Label(peer.Namespace)) != 0) {
			return fmt.Errorf("%w: peers require names, immutable UIDs and valid namespaces", ErrUnavailable)
		}
		if len(peer.PodSelector) != 0 {
			if capabilities.Profile != sandbox.ProfileNativeSingleStart || peer.Namespace == "" {
				return fmt.Errorf("%w: Pod selectors require a native namespaced peer", ErrUnsupportedControl)
			}
			for key, value := range peer.PodSelector {
				if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 ||
					key == "io.kubernetes.pod.namespace" {
					return fmt.Errorf("%w: Pod selectors require valid Pod labels, not namespace identity overrides", ErrUnsupportedControl)
				}
			}
			if len(peer.IPs) == 0 {
				return nil
			}
		}
		if len(peer.IPs) == 0 {
			return fmt.Errorf("%w: IP-bound peers require resolved IPs", ErrUnavailable)
		}
		for i, raw := range peer.IPs {
			address, err := normalizeIP(raw)
			if err != nil {
				return err
			}
			peer.IPs[i] = address
		}
		peer.IPs = sortedUnique(peer.IPs)
		return nil
	}
	validateServiceSelector := func(peer Peer) error {
		if len(peer.PodSelector) == 0 {
			return nil
		}
		if len(peer.IPs) == 0 {
			return fmt.Errorf("%w: selector-bound DNS and Gateway peers require Service IPs", ErrUnavailable)
		}
		for _, raw := range peer.IPs {
			ip := netip.MustParseAddr(raw)
			prefix := netip.PrefixFrom(ip, ip.BitLen())
			if !containedByAny(prefix, topology.ServiceCIDRs) || overlapsAny(prefix, topology.PodCIDRs) {
				return fmt.Errorf("%w: selector-bound DNS and Gateway peers must pin Service IPs, not Pod or external IPs", ErrUnsupportedControl)
			}
		}
		return nil
	}
	dnsIPs := map[string]bool{}
	for i := range topology.DNS {
		if err := normalizePeer(&topology.DNS[i]); err != nil {
			return platform, err
		}
		if err := validateServiceSelector(topology.DNS[i]); err != nil {
			return platform, err
		}
		for _, ip := range topology.DNS[i].IPs {
			dnsIPs[ip] = true
		}
	}
	if len(topology.DNSResolverIPs) == 0 {
		return platform, fmt.Errorf("%w: explicit Pod DNS resolver addresses are required", ErrUnavailable)
	}
	for i, raw := range topology.DNSResolverIPs {
		address, err := normalizeIP(raw)
		if err != nil {
			return platform, err
		}
		if !dnsIPs[address] {
			return platform, fmt.Errorf("%w: each Pod resolver must belong to an approved DNS peer", ErrUnavailable)
		}
		topology.DNSResolverIPs[i] = address
	}
	topology.DNSResolverIPs = sortedUnique(topology.DNSResolverIPs)
	if len(topology.DNSResolverIPs) > 3 {
		return platform, fmt.Errorf("%w: the protected Pod can configure at most three DNS server IPs", ErrUnsupportedControl)
	}
	topology.DNS, err = canonicalObjects(topology.DNS)
	if err != nil {
		return platform, err
	}
	proxyExclusions := append(slices.Clone(mandatoryCIDRs), topology.ServiceCIDRs...)
	proxyExclusions = append(proxyExclusions, topology.NodeCIDRs...)
	proxyExclusions = append(proxyExclusions, topology.ManagementCIDRs...)
	proxyExclusions = append(proxyExclusions, topology.MetadataCIDRs...)
	for _, group := range []struct {
		destinations     *[]Destination
		ingress          bool
		serviceSelectors bool
	}{
		{&topology.Gateway, false, true}, {&topology.Ingress, true, false}, {&topology.KubernetesAPI, false, false},
	} {
		destinations := group.destinations
		for i := range *destinations {
			dest := &(*destinations)[i]
			if err := normalizePeer(&dest.Peer); err != nil {
				return platform, err
			}
			if len(dest.TrustedProxyCIDRs) != 0 && (!group.ingress || len(dest.Peer.PodSelector) == 0) {
				return platform, fmt.Errorf("%w: proxy CIDRs require a selector-bound native ingress peer", ErrUnsupportedControl)
			}
			if len(dest.Peer.PodSelector) != 0 && group.ingress {
				if len(dest.Peer.IPs) != 0 {
					return platform, fmt.Errorf("%w: selector-bound ingress must not pin Pod IPs", ErrUnsupportedControl)
				}
				if len(dest.TrustedProxyCIDRs) == 0 {
					return platform, fmt.Errorf("%w: selector-bound ingress requires explicit trusted proxy Pod CIDRs", ErrUnavailable)
				}
				for j, raw := range dest.TrustedProxyCIDRs {
					prefix, err := parsePrefix(raw, capabilities)
					if err != nil {
						return platform, err
					}
					if err := ValidateProxyTrust(prefix.String()); err != nil ||
						!containedByAny(prefix, topology.PodCIDRs) || overlapsAny(prefix, proxyExclusions) {
						return platform, fmt.Errorf("%w: trusted proxy CIDRs must be bounded Pod networks excluding services, nodes, management and metadata", ErrUnsupportedControl)
					}
					dest.TrustedProxyCIDRs[j] = prefix.String()
				}
				dest.TrustedProxyCIDRs = sortedUnique(dest.TrustedProxyCIDRs)
			} else if len(dest.Peer.PodSelector) != 0 {
				if !group.serviceSelectors {
					return platform, fmt.Errorf("%w: Pod selectors are supported only for native DNS, Gateway and ingress", ErrUnsupportedControl)
				}
				if err := validateServiceSelector(dest.Peer); err != nil {
					return platform, err
				}
			}
			dest.Ports, err = canonicalPorts(dest.Ports, capabilities)
			if err != nil {
				return platform, err
			}
		}
		merged := make(map[string]Destination)
		for _, destination := range *destinations {
			peerKey, err := json.Marshal(destination.Peer)
			if err != nil {
				return platform, fmt.Errorf("encode destination peer: %w", err)
			}
			current := merged[string(peerKey)]
			current.Peer = destination.Peer
			current.TrustedProxyCIDRs = sortedUnique(append(current.TrustedProxyCIDRs, destination.TrustedProxyCIDRs...))
			current.Ports = append(current.Ports, destination.Ports...)
			current.Ports, err = canonicalObjects(current.Ports)
			if err != nil {
				return platform, err
			}
			merged[string(peerKey)] = current
		}
		*destinations = nil
		for _, destination := range merged {
			*destinations = append(*destinations, destination)
		}
		*destinations, err = canonicalObjects(*destinations)
		if err != nil {
			return platform, err
		}
	}
	for _, peer := range topology.Peers {
		if len(peer.PodSelector) != 0 {
			return platform, fmt.Errorf("%w: Pod selectors must be registered as native DNS, Gateway or ingress destinations", ErrUnsupportedControl)
		}
	}
	allPeers := slices.Clone(topology.Peers)
	allPeers = append(allPeers, topology.DNS...)
	for _, destinations := range [][]Destination{topology.Gateway, topology.Ingress, topology.KubernetesAPI} {
		for _, destination := range destinations {
			allPeers = append(allPeers, destination.Peer)
		}
	}
	for i := range allPeers {
		if err := normalizePeer(&allPeers[i]); err != nil {
			return platform, err
		}
	}
	allPeers, err = canonicalObjects(allPeers)
	if err != nil {
		return platform, err
	}
	seenUIDs, seenIPs := map[string]bool{}, map[string]string{}
	for _, peer := range allPeers {
		if seenUIDs[string(peer.UID)] {
			return platform, fmt.Errorf("%w: a peer UID has inconsistent resolved membership", ErrUnavailable)
		}
		seenUIDs[string(peer.UID)] = true
		for _, ip := range peer.IPs {
			if uid, exists := seenIPs[ip]; exists && uid != string(peer.UID) {
				return platform, fmt.Errorf("%w: an IP is shared by distinct peer identities", ErrUnavailable)
			}
			seenIPs[ip] = string(peer.UID)
		}
	}
	topology.Peers = allPeers
	return platform, nil
}

// ValidatePlatform checks trusted compiler prerequisites, not their live
// realization. The source supplying Available/Qualified owns that evidence.
func ValidatePlatform(platform Platform, capabilities sandbox.Capabilities) error {
	canonical, err := canonicalPlatform(platform, capabilities)
	if err != nil {
		return err
	}
	policy, errs := sandbox.NormalizeAgentPolicy(api.AgentPolicySpec{
		// Platform validation compiles only the mandatory network baseline, but
		// normalization now requires every policy-shaped input to identify a
		// permitted harness. Hermes is a valid built-in harness and does not
		// affect the network plan being qualified here.
		Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}},
		Network:   api.AgentPolicyNetwork{Ingress: api.AgentPolicyIngress{Mode: api.AgentPolicyIngressDisabled}},
	})
	if len(errs) != 0 {
		return errs.ToAggregate()
	}
	_, err = compileNetwork(CompileInput{
		AgentName: "configuration", Namespace: "configuration",
		Resolution: sandbox.Resolution{
			Binding: sandbox.Binding{AgentUID: "configuration"}, AgentPolicy: policy, Capabilities: capabilities,
		},
		Platform: canonical,
	})
	return err
}

func compileNetwork(input CompileInput, tlsPlan ...*ingressmtls.Plan) (NetworkPlan, error) {
	policy, capabilities, topology := input.Resolution.AgentPolicy, input.Resolution.Capabilities, input.Platform.Topology
	plan := NetworkPlan{
		Adapter: input.Platform.Adapter.Name, IngressMode: policy.Network.Ingress.Mode,
		DNSIPs: slices.Clone(topology.DNSResolverIPs),
	}
	var dnsIPs []string
	for _, peer := range topology.DNS {
		dnsIPs = append(dnsIPs, peer.IPs...)
	}
	dnsIPs = sortedUnique(dnsIPs)
	hardDenied := append(slices.Clone(mandatoryCIDRs), topology.NodeCIDRs...)
	hardDenied = append(hardDenied, topology.ManagementCIDRs...)
	hardDenied = append(hardDenied, topology.MetadataCIDRs...)
	hardDenied = qualifiedCIDRs(hardDenied, capabilities)
	internal := append(slices.Clone(privateCIDRs), hardDenied...)
	internal = append(internal, topology.PodCIDRs...)
	internal = append(internal, topology.ServiceCIDRs...)
	for _, destination := range topology.KubernetesAPI {
		internal = append(internal, peerCIDRs(destination.Peer)...)
	}
	internal = qualifiedCIDRs(internal, capabilities)

	var grants []addressGrant
	blockedCIDRs := make([]netip.Prefix, 0, len(policy.Network.BlockedCIDRs))
	for _, cidr := range policy.Network.BlockedCIDRs {
		prefix, err := parsePrefix(cidr, capabilities)
		if err != nil {
			return plan, err
		}
		blockedCIDRs = append(blockedCIDRs, prefix)
	}
	for _, rule := range policy.Network.AllowedInternalCIDRs {
		prefix, err := parsePrefix(rule.CIDR, capabilities)
		if err != nil {
			return plan, err
		}
		if !containedByAny(prefix, internal) {
			return plan, fmt.Errorf("%w: internal exceptions must be within registered internal networks", ErrUnsupportedControl)
		}
		if overlapsAny(prefix, hardDenied) {
			return plan, fmt.Errorf("%w: internal exceptions cannot include management, metadata or nodes", ErrConflict)
		}
		ports, err := canonicalPorts(rule.Ports, capabilities)
		if err != nil {
			return plan, err
		}
		if capabilities.Profile == sandbox.ProfileNativeSingleStart && slices.ContainsFunc(ports, func(port api.AgentPolicyPort) bool {
			return port.Protocol == corev1.ProtocolSCTP
		}) {
			// Cilium does not support SCTP pod-to-VIP policies. Native SCTP
			// grants target direct endpoints, never Service or API exceptions.
			if overlapsAny(prefix, topology.ServiceCIDRs) {
				return plan, fmt.Errorf("%w: native SCTP internal exceptions cannot include Service VIP ranges", ErrUnsupportedControl)
			}
			for _, destination := range topology.KubernetesAPI {
				if overlapsAny(prefix, peerCIDRs(destination.Peer)) {
					return plan, fmt.Errorf("%w: native SCTP internal exceptions cannot include Kubernetes API destinations", ErrConflict)
				}
			}
		}
		grants = append(grants, addressGrant{prefix, ports})
	}
	blockedPorts, err := canonicalPortsOptional(policy.Network.BlockedPorts, capabilities)
	if err != nil {
		return plan, err
	}
	var required []Destination
	for _, peer := range topology.DNS {
		required = append(required, Destination{Peer: peer, Ports: dnsPorts})
	}
	required = append(required, topology.Gateway...)
	if input.ConnectionsMCP != nil {
		required = append(required, *input.ConnectionsMCP)
	}
	if policy.KubernetesAccess != nil {
		if len(topology.KubernetesAPI) == 0 {
			return plan, fmt.Errorf("%w: explicit Kubernetes access requires resolved API destinations", ErrUnavailable)
		}
		required = append(required, topology.KubernetesAPI...)
	}
	for _, destination := range required {
		for _, port := range destination.Ports {
			if slices.Contains(blockedPorts, port) {
				return plan, fmt.Errorf("%w: a blocked port prevents required DNS, Gateway or Kubernetes API access", ErrConflict)
			}
			if port.Port == 53 && (port.Protocol == corev1.ProtocolTCP || port.Protocol == corev1.ProtocolUDP) {
				for _, ip := range destination.Peer.IPs {
					if !slices.Contains(dnsIPs, ip) {
						return plan, fmt.Errorf("%w: port 53 is reserved to the approved DNS peers", ErrConflict)
					}
				}
			}
		}
		for _, raw := range destination.Peer.IPs {
			ip := netip.MustParseAddr(raw)
			prefix := netip.PrefixFrom(ip, ip.BitLen())
			if overlapsAny(prefix, hardDenied) {
				return plan, fmt.Errorf("%w: a required destination overlaps management, metadata or node exclusions", ErrConflict)
			}
			for _, blocked := range blockedCIDRs {
				if blocked.Contains(ip) {
					return plan, fmt.Errorf("%w: a blocked CIDR prevents required DNS, Gateway or Kubernetes API access", ErrConflict)
				}
			}
			grants = append(grants, addressGrant{prefix, destination.Ports})
		}
		if len(destination.Peer.PodSelector) != 0 {
			for _, blocked := range blockedCIDRs {
				// Changing backend IPs cannot be mapped to stable endpoint denies.
				// Reject this combination rather than bypass a Pod CIDR restriction.
				if overlapsAny(blocked, topology.PodCIDRs) {
					return plan, fmt.Errorf("%w: blocked Pod CIDRs cannot be enforced on selector-bound service backends", ErrUnsupportedControl)
				}
			}
		}
	}
	exceptions := make([]string, 0, len(grants))
	for _, grant := range grants {
		exceptions = append(exceptions, grant.Prefix.String())
	}
	exceptions = sortedUnique(exceptions)
	spec := CiliumPolicySpec{
		EndpointSelector: metav1.LabelSelector{MatchLabels: map[string]string{
			"k8s:" + AgentUIDLabel:            string(input.Resolution.Binding.AgentUID),
			"k8s:io.kubernetes.pod.namespace": input.Namespace,
		}},
		EnableDefaultDeny: DefaultDeny{Ingress: true, Egress: true},
		Ingress:           []IngressRule{}, IngressDeny: []IngressRule{}, Egress: []EgressRule{}, EgressDeny: []EgressRule{},
	}
	// The terminal has the same Agent and namespace labels as the primary
	// workload. Exclude it from this policy when its isolated policy exists,
	// otherwise Cilium unions the main workload's model and internet grants into
	// the terminal's default-deny policy.
	if input.TerminalIngress != nil {
		spec.EndpointSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
			Key: "k8s:cetirun.microsoft.com/terminal", Operator: metav1.LabelSelectorOpDoesNotExist,
		}}
	}
	budget := networkRuleBudget{}
	// Public-internet egress is intentionally permitted except for explicit
	// exclusions/denies below. Default-deny is not an internet allow-list; only
	// approved peers and ports are exceptions to internal-network isolation.
	for _, root := range familyRoots(capabilities) {
		ranges := cidrExcept(root, internal)
		if len(ranges) == 0 {
			return plan, fmt.Errorf("%w: internal CIDRs cover the entire %s address space", ErrConflict, root)
		}
		if err := budget.egress(&spec.Egress, EgressRule{ToCIDRSet: ranges}); err != nil {
			return plan, err
		}
	}
	for _, cidr := range internal {
		if ranges := cidrExcept(cidr, exceptions); len(ranges) != 0 {
			if err := budget.egress(&spec.EgressDeny, EgressRule{ToCIDRSet: ranges}); err != nil {
				return plan, err
			}
		}
	}
	if err := budget.egress(&spec.EgressDeny,
		EgressRule{ToCIDRSet: cidrRules(hardDenied)},
		EgressRule{ToEntities: []string{"host", "remote-node"}},
	); err != nil {
		return plan, err
	}
	for _, prefix := range blockedCIDRs {
		if err := budget.egress(&spec.EgressDeny, EgressRule{ToCIDRSet: []CIDRRule{{CIDR: prefix.String()}}}); err != nil {
			return plan, err
		}
	}
	if len(blockedPorts) != 0 {
		if err := budget.egress(&spec.EgressDeny, EgressRule{ToEntities: []string{"all"}, ToPorts: portRules(blockedPorts)}); err != nil {
			return plan, err
		}
	}
	sectors, err := partitionGrants(grants, capabilities)
	if err != nil {
		return plan, err
	}
	for _, sector := range sectors {
		if err := budget.egress(&spec.Egress, EgressRule{
			ToCIDRSet: []CIDRRule{{CIDR: sector.Prefix.String()}}, ToPorts: portRules(sector.Ports),
		}); err != nil {
			return plan, err
		}
		if err := budget.egress(&spec.EgressDeny, EgressRule{
			ToCIDRSet: []CIDRRule{{CIDR: sector.Prefix.String()}}, ToPorts: deniedPorts(sector.Ports),
		}); err != nil {
			return plan, err
		}
	}
	var allowedPeers, dnsPeers []Peer
	for _, peer := range topology.Peers {
		if peer.Namespace == "" {
			continue
		}
		ports, err := managedPeerPorts(peer, grants, blockedCIDRs, blockedPorts)
		if err != nil {
			return plan, err
		}
		if len(ports) == 0 {
			continue
		}
		allowedPeers = append(allowedPeers, peer)
		selector := []metav1.LabelSelector{peerSelector(peer)}
		if err := budget.egress(&spec.Egress, EgressRule{ToEndpoints: selector, ToPorts: portRules(ports)}); err != nil {
			return plan, err
		}
		if err := budget.egress(&spec.EgressDeny, EgressRule{ToEndpoints: selector, ToPorts: deniedPorts(ports)}); err != nil {
			return plan, err
		}
	}
	for _, peer := range topology.DNS {
		if peer.Namespace != "" {
			dnsPeers = append(dnsPeers, peer)
		}
	}
	selectors, err := excludingPeers(allowedPeers)
	if err != nil {
		return plan, err
	}
	for _, selector := range selectors {
		if err := budget.egress(&spec.EgressDeny, EgressRule{ToEndpoints: []metav1.LabelSelector{selector}}); err != nil {
			return plan, err
		}
	}
	for _, root := range familyRoots(capabilities) {
		if err := budget.egress(&spec.EgressDeny, EgressRule{
			ToCIDRSet: cidrExcept(root, ipCIDRs(dnsIPs)), ToPorts: portRules(dnsPorts),
		}); err != nil {
			return plan, err
		}
	}
	selectors, err = excludingPeers(dnsPeers)
	if err != nil {
		return plan, err
	}
	for _, selector := range selectors {
		if err := budget.egress(&spec.EgressDeny, EgressRule{
			ToEndpoints: []metav1.LabelSelector{selector}, ToPorts: portRules(dnsPorts),
		}); err != nil {
			return plan, err
		}
	}
	if policy.Network.Ingress.Mode == api.AgentPolicyIngressDisabled {
		if err := budget.ingress(&spec.IngressDeny, IngressRule{FromEntities: []string{"all"}}); err != nil {
			return plan, err
		}
	} else {
		if len(topology.Ingress) == 0 {
			return plan, fmt.Errorf("%w: authenticated ingress requires a registered authenticating Gateway", ErrUnavailable)
		}
		var ingressPeers []Peer
		for _, destination := range topology.Ingress {
			if len(tlsPlan) != 0 && tlsPlan[0] != nil {
				if custom := input.CustomIngress; custom != nil {
					destination.Ports = append(slices.Clone(destination.Ports), api.AgentPolicyPort{Port: custom.Port, Protocol: corev1.ProtocolTCP})
				}
				destination, err = mtlsDestination(destination, *tlsPlan[0])
				if err != nil {
					return plan, err
				}
			}
			if destination.Peer.Namespace == "" {
				return plan, fmt.Errorf("%w: authenticated ingress requires protected managed Gateway identities", ErrUnsupportedControl)
			}
			if input.NativeExecution {
				if len(destination.Peer.PodSelector) != 0 {
					plan.TrustedProxyIPs = append(plan.TrustedProxyIPs, destination.TrustedProxyCIDRs...)
				} else {
					for _, ip := range destination.Peer.IPs {
						if !netip.MustParseAddr(ip).IsGlobalUnicast() {
							return plan, fmt.Errorf("%w: native proxy trust requires unicast ingress peer IPs", ErrUnsupportedControl)
						}
						plan.TrustedProxyIPs = append(plan.TrustedProxyIPs, ip)
					}
				}
			}
			ingressPeers = append(ingressPeers, destination.Peer)
			ports := destination.Ports
			if custom := input.CustomIngress; custom != nil && custom.WebUI && (len(tlsPlan) == 0 || tlsPlan[0] == nil) {
				ports = append(slices.Clone(ports), api.AgentPolicyPort{Port: custom.Port, Protocol: corev1.ProtocolTCP})
				ports, err = canonicalObjects(ports)
				if err != nil {
					return plan, err
				}
			}
			selector := []metav1.LabelSelector{peerSelector(destination.Peer)}
			if err := budget.ingress(&spec.Ingress, IngressRule{FromEndpoints: selector, ToPorts: portRules(ports)}); err != nil {
				return plan, err
			}
			if err := budget.ingress(&spec.IngressDeny, IngressRule{FromEndpoints: selector, ToPorts: deniedPorts(ports)}); err != nil {
				return plan, err
			}
		}
		if custom := input.CustomIngress; custom != nil {
			for index, caller := range custom.ChatCallers {
				peer := Peer{Name: fmt.Sprintf("portal-chat-%d", index), UID: types.UID(fmt.Sprintf("portal-chat-%d", index)),
					Namespace: caller.Namespace, PodSelector: caller.PodLabels}
				ingressPeers = append(ingressPeers, peer)
				selector := []metav1.LabelSelector{peerSelector(peer)}
				ports := []api.AgentPolicyPort{{Port: custom.Port, Protocol: corev1.ProtocolTCP}}
				if len(tlsPlan) != 0 && tlsPlan[0] != nil {
					ports[0].Port = ingressmtls.APIPort
				}
				if err := budget.ingress(&spec.Ingress, IngressRule{FromEndpoints: selector, ToPorts: portRules(ports)}); err != nil {
					return plan, err
				}
				if err := budget.ingress(&spec.IngressDeny, IngressRule{FromEndpoints: selector, ToPorts: deniedPorts(ports)}); err != nil {
					return plan, err
				}
			}
		}
		selectors, err := excludingPeers(ingressPeers)
		if err != nil {
			return plan, err
		}
		for _, selector := range selectors {
			if err := budget.ingress(&spec.IngressDeny, IngressRule{FromEndpoints: []metav1.LabelSelector{selector}}); err != nil {
				return plan, err
			}
		}
		if err := budget.ingress(&spec.IngressDeny, IngressRule{FromEntities: []string{"world", "host", "remote-node"}}); err != nil {
			return plan, err
		}
		plan.TrustedProxyIPs = sortedUnique(plan.TrustedProxyIPs)
	}
	for _, rules := range []*[]EgressRule{&spec.Egress, &spec.EgressDeny} {
		*rules, err = canonicalObjects(*rules)
		if err != nil {
			return plan, err
		}
	}
	for _, rules := range []*[]IngressRule{&spec.Ingress, &spec.IngressDeny} {
		*rules, err = canonicalObjects(*rules)
		if err != nil {
			return plan, err
		}
	}
	name, err := digest(input.Resolution.Binding.AgentUID)
	if err != nil {
		return plan, err
	}
	metadata, err := ownedMetadata(input, "sandbox-"+name[:32], input.Namespace)
	if err != nil {
		return plan, err
	}
	plan.Policy = CiliumNetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "cilium.io/v2", Kind: "CiliumNetworkPolicy"},
		ObjectMeta: metadata, Spec: spec,
	}
	if terminal := input.TerminalIngress; terminal != nil {
		copy := *terminal
		if len(tlsPlan) != 0 && tlsPlan[0] != nil {
			copy.Port = ingressmtls.TerminalPort
		}
		policy, err := compileTerminalNetwork(input, copy)
		if err != nil {
			return plan, err
		}
		plan.TerminalPolicy = policy
	}
	return plan, validateNetworkPolicySize(plan.Policy)
}

// compileTerminalNetwork gives the isolated terminal no model, Agent, or
// internet authority. Its sole peers are registered DNS, the Kubernetes API,
// and the authenticated ingress endpoints on its fixed TCP listener.
func compileTerminalNetwork(input CompileInput, terminal TerminalIngress) (*CiliumNetworkPolicy, error) {
	topology := input.Platform.Topology
	// The terminal needs its narrowly scoped API connection to use the
	// controller-issued ServiceAccount token. Do not render a terminal policy
	// until the platform snapshot has explicitly resolved that destination.
	if len(topology.KubernetesAPI) == 0 {
		return nil, fmt.Errorf("%w: terminal requires resolved Kubernetes API destinations", ErrUnavailable)
	}
	spec := CiliumPolicySpec{
		EndpointSelector: metav1.LabelSelector{MatchLabels: map[string]string{
			"k8s:cetirun.microsoft.com/terminal": "true",
			"k8s:" + AgentUIDLabel:               string(input.Resolution.Binding.AgentUID),
			"k8s:io.kubernetes.pod.namespace":    input.Namespace,
		}},
		EnableDefaultDeny: DefaultDeny{Ingress: true, Egress: true},
		Ingress:           []IngressRule{}, IngressDeny: []IngressRule{}, Egress: []EgressRule{}, EgressDeny: []EgressRule{},
	}
	dnsPorts := portRules([]api.AgentPolicyPort{{Port: 53, Protocol: corev1.ProtocolTCP}, {Port: 53, Protocol: corev1.ProtocolUDP}})
	for _, peer := range topology.DNS {
		if err := appendTerminalEgress(&spec, peer, dnsPorts); err != nil {
			return nil, err
		}
	}
	for _, destination := range topology.KubernetesAPI {
		if err := appendTerminalEgress(&spec, destination.Peer, portRules(destination.Ports)); err != nil {
			return nil, err
		}
	}
	if input.Resolution.AgentPolicy.Network.Ingress.Mode != api.AgentPolicyIngressDisabled {
		ports := portRules([]api.AgentPolicyPort{{Port: terminal.Port, Protocol: corev1.ProtocolTCP}})
		if input.Resolution.AgentPolicy.Network.Ingress.RequireMTLS {
			spec.IngressDeny = append(spec.IngressDeny, IngressRule{FromEntities: []string{"all"},
				ToPorts: deniedPorts([]api.AgentPolicyPort{{Port: terminal.Port, Protocol: corev1.ProtocolTCP}})})
		}
		for _, destination := range topology.Ingress {
			if destination.Peer.Namespace != "" {
				spec.Ingress = append(spec.Ingress, IngressRule{FromEndpoints: []metav1.LabelSelector{peerSelector(destination.Peer)}, ToPorts: ports})
				continue
			}
			cidrs := peerCIDRs(destination.Peer)
			if len(cidrs) == 0 {
				return nil, fmt.Errorf("%w: terminal ingress requires resolved Gateway addresses", ErrUnavailable)
			}
			spec.Ingress = append(spec.Ingress, IngressRule{FromCIDRSet: cidrRules(cidrs), ToPorts: ports})
		}
	}
	var err error
	for _, rules := range []*[]EgressRule{&spec.Egress, &spec.EgressDeny} {
		*rules, err = canonicalObjects(*rules)
		if err != nil {
			return nil, err
		}
	}
	for _, rules := range []*[]IngressRule{&spec.Ingress, &spec.IngressDeny} {
		*rules, err = canonicalObjects(*rules)
		if err != nil {
			return nil, err
		}
	}
	name, err := digest(string(input.Resolution.Binding.AgentUID) + "/terminal")
	if err != nil {
		return nil, err
	}
	metadata, err := ownedMetadata(input, "sandbox-terminal-"+name[:24], input.Namespace)
	if err != nil {
		return nil, err
	}
	policy := &CiliumNetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "cilium.io/v2", Kind: "CiliumNetworkPolicy"}, ObjectMeta: metadata, Spec: spec}
	if err := validateNetworkPolicySize(*policy); err != nil {
		return nil, err
	}
	return policy, nil
}

func appendTerminalEgress(spec *CiliumPolicySpec, peer Peer, ports []PortRule) error {
	if len(ports) == 0 {
		return fmt.Errorf("terminal egress requires resolved peers and ports")
	}
	if peer.Namespace != "" {
		spec.Egress = append(spec.Egress, EgressRule{ToEndpoints: []metav1.LabelSelector{peerSelector(peer)}, ToPorts: ports})
		return nil
	}
	cidrs := peerCIDRs(peer)
	if len(cidrs) == 0 {
		return fmt.Errorf("terminal egress requires resolved peers and ports")
	}
	spec.Egress = append(spec.Egress, EgressRule{ToCIDRSet: cidrRules(cidrs), ToPorts: ports})
	return nil
}

func parsePrefix(raw string, capabilities sandbox.Capabilities) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || prefix.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("invalid network CIDR")
	}
	if !supportedFamily(prefix.Addr(), capabilities) {
		return netip.Prefix{}, fmt.Errorf("%w: a CIDR IP family is not qualified", ErrUnsupportedControl)
	}
	return prefix.Masked(), nil
}

// ValidateProxyTrust checks the compiler/render boundary. CIDR membership in the
// registered Pod networks and the matching ingress selector are checked earlier.
func ValidateProxyTrust(value string) error {
	if address, err := netip.ParseAddr(value); err == nil {
		if !address.Is4In6() && address.Zone() == "" && address.String() == value && address.IsGlobalUnicast() {
			return nil
		}
	} else if prefix, err := netip.ParsePrefix(value); err == nil {
		if !prefix.Addr().Is4In6() && prefix.String() == value && prefix == prefix.Masked() &&
			prefix.Addr().IsGlobalUnicast() && !overlapsAny(prefix, mandatoryCIDRs) {
			return nil
		}
	}
	return fmt.Errorf("proxy trust requires a canonical unicast IP or bounded unicast CIDR")
}

func supportedFamily(address netip.Addr, capabilities sandbox.Capabilities) bool {
	family := corev1.IPv6Protocol
	if address.Is4() {
		family = corev1.IPv4Protocol
	}
	return slices.Contains(capabilities.IPFamilies, family)
}

func qualifiedCIDRs(values []string, capabilities sandbox.Capabilities) []string {
	var result []string
	for _, value := range values {
		prefix := netip.MustParsePrefix(value)
		if supportedFamily(prefix.Addr(), capabilities) {
			result = append(result, prefix.Masked().String())
		}
	}
	return sortedUnique(result)
}

func familyRoots(capabilities sandbox.Capabilities) []string {
	return qualifiedCIDRs([]string{"0.0.0.0/0", "::/0"}, capabilities)
}

func ipCIDRs(ips []string) []string {
	result := make([]string, 0, len(ips))
	for _, raw := range ips {
		ip := netip.MustParseAddr(raw)
		result = append(result, netip.PrefixFrom(ip, ip.BitLen()).String())
	}
	return sortedUnique(result)
}

func peerCIDRs(peer Peer) []string { return ipCIDRs(peer.IPs) }

func overlapsAny(prefix netip.Prefix, values []string) bool {
	for _, value := range values {
		if prefix.Overlaps(netip.MustParsePrefix(value)) {
			return true
		}
	}
	return false
}

func containedByAny(prefix netip.Prefix, values []string) bool {
	for _, value := range values {
		candidate := netip.MustParsePrefix(value)
		if candidate.Bits() <= prefix.Bits() && candidate.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func cidrRules(values []string) []CIDRRule {
	result := make([]CIDRRule, 0, len(values))
	for _, value := range values {
		result = append(result, CIDRRule{CIDR: value})
	}
	return result
}

func cidrExcept(value string, exceptions []string) []CIDRRule {
	prefix := netip.MustParsePrefix(value)
	rule := CIDRRule{CIDR: value}
	for _, exception := range exceptions {
		except := netip.MustParsePrefix(exception)
		if !prefix.Overlaps(except) {
			continue
		}
		if except.Bits() <= prefix.Bits() {
			return nil
		}
		rule.Except = append(rule.Except, except.String())
	}
	rule.Except = sortedUnique(rule.Except)
	return []CIDRRule{rule}
}

func canonicalPortsOptional(ports []api.AgentPolicyPort, capabilities sandbox.Capabilities) ([]api.AgentPolicyPort, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	return canonicalPorts(ports, capabilities)
}

func canonicalPorts(ports []api.AgentPolicyPort, capabilities sandbox.Capabilities) ([]api.AgentPolicyPort, error) {
	if len(ports) == 0 {
		return nil, fmt.Errorf("%w: every destination requires explicit ports", ErrUnavailable)
	}
	result := slices.Clone(ports)
	for i := range result {
		port := &result[i]
		if port.Protocol == "" {
			port.Protocol = corev1.ProtocolTCP
		}
		if port.Port < 1 || port.Port > 65535 {
			return nil, fmt.Errorf("destination port must be between 1 and 65535")
		}
		if !slices.Contains([]corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP}, port.Protocol) ||
			!slices.Contains(capabilities.Protocols, port.Protocol) {
			return nil, fmt.Errorf("%w: destination protocol is not qualified", ErrUnsupportedControl)
		}
	}
	return canonicalObjects(result)
}

func portRules(ports []api.AgentPolicyPort) []PortRule {
	ranges := make([]PortRange, len(ports))
	for i, port := range ports {
		ranges[i] = PortRange{Port: strconv.Itoa(int(port.Port)), Protocol: port.Protocol}
	}
	return boundedPortRules(ranges)
}

func boundedPortRules(ports []PortRange) []PortRule {
	if len(ports) == 0 {
		return []PortRule{{}}
	}
	rules := make([]PortRule, 0, (len(ports)+maxPortsPerRule-1)/maxPortsPerRule)
	for group := range slices.Chunk(ports, maxPortsPerRule) {
		rules = append(rules, PortRule{Ports: group})
	}
	return rules
}

func deniedPorts(allowed []api.AgentPolicyPort) []PortRule {
	rule := PortRule{}
	for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP} {
		var ports []int
		for _, port := range allowed {
			if port.Protocol == protocol {
				ports = append(ports, int(port.Port))
			}
		}
		slices.Sort(ports)
		ports = append(slices.Compact(ports), 65536)
		start := 1
		for _, port := range ports {
			if port > start {
				denied := PortRange{Port: strconv.Itoa(start), Protocol: protocol}
				if port > start+1 {
					denied.EndPort = int32(port - 1)
				}
				rule.Ports = append(rule.Ports, denied)
			}
			start = port + 1
		}
	}
	return boundedPortRules(rule.Ports)
}

func peerSelector(peer Peer) metav1.LabelSelector {
	selected := metav1.LabelSelector{MatchLabels: map[string]string{
		"k8s:io.kubernetes.pod.namespace": peer.Namespace,
	}}
	if len(peer.PodSelector) == 0 {
		selected.MatchLabels["k8s:"+PeerUIDLabel] = string(peer.UID)
	} else {
		for key, value := range peer.PodSelector {
			selected.MatchLabels["k8s:"+key] = value
		}
	}
	return selected
}

func excludingPeers(peers []Peer) ([]metav1.LabelSelector, error) {
	for _, peer := range peers {
		if len(peer.PodSelector) != 0 {
			return excludingSelectedPeers(peers)
		}
	}
	var uids []string
	var selectors []metav1.LabelSelector
	for _, peer := range peers {
		uids = append(uids, string(peer.UID))
		selectors = append(selectors, metav1.LabelSelector{
			MatchLabels: map[string]string{"k8s:" + PeerUIDLabel: string(peer.UID)},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "k8s:io.kubernetes.pod.namespace", Operator: metav1.LabelSelectorOpNotIn, Values: []string{peer.Namespace},
			}},
		})
	}
	if len(uids) == 0 {
		return []metav1.LabelSelector{{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "k8s:io.kubernetes.pod.namespace", Operator: metav1.LabelSelectorOpExists,
		}}}}, nil
	}
	selectors = append(selectors, metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "k8s:io.kubernetes.pod.namespace", Operator: metav1.LabelSelectorOpExists},
		{Key: "k8s:" + PeerUIDLabel, Operator: metav1.LabelSelectorOpNotIn, Values: sortedUnique(uids)},
	}})
	return canonicalObjects(selectors)
}

func excludingSelectedPeers(peers []Peer) ([]metav1.LabelSelector, error) {
	selectors := []metav1.LabelSelector{{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: "k8s:io.kubernetes.pod.namespace", Operator: metav1.LabelSelectorOpExists,
	}}}}
	remaining := MaxNetworkPartitionWork
	for _, peer := range peers {
		selected := peerSelector(peer)
		var next []metav1.LabelSelector
		budget := networkRuleBudget{}
		for _, previous := range selectors {
			// NOT (peer A OR peer B) requires a mismatch with every peer,
			// including its namespace. Bound distribution before allocating.
			for _, key := range slices.Sorted(maps.Keys(selected.MatchLabels)) {
				if err := consumeBudget(&remaining, 1+len(previous.MatchExpressions), "peer selector expansion"); err != nil {
					return nil, err
				}
				candidate := *previous.DeepCopy()
				merged := false
				for i := range candidate.MatchExpressions {
					expression := &candidate.MatchExpressions[i]
					if expression.Key == key && expression.Operator == metav1.LabelSelectorOpNotIn {
						expression.Values = sortedUnique(append(expression.Values, selected.MatchLabels[key]))
						merged = true
						break
					}
				}
				if !merged {
					candidate.MatchExpressions = append(candidate.MatchExpressions, metav1.LabelSelectorRequirement{
						Key: key, Operator: metav1.LabelSelectorOpNotIn, Values: []string{selected.MatchLabels[key]},
					})
				}
				var err error
				candidate.MatchExpressions, err = canonicalObjects(candidate.MatchExpressions)
				if err != nil {
					return nil, err
				}
				if err := budget.take(networkRuleEntries(nil, []metav1.LabelSelector{candidate}, nil, nil)); err != nil {
					return nil, err
				}
				next = append(next, candidate)
			}
		}
		var err error
		selectors, err = canonicalObjects(next)
		if err != nil {
			return nil, err
		}
	}
	return selectors, nil
}

func managedPeerPorts(peer Peer, grants []addressGrant, blocks []netip.Prefix, blockedPorts []api.AgentPolicyPort) ([]api.AgentPolicyPort, error) {
	var previous []api.AgentPolicyPort
	for i, raw := range peer.IPs {
		ip := netip.MustParseAddr(raw)
		var ports []api.AgentPolicyPort
		blocked := false
		for _, prefix := range blocks {
			blocked = blocked || prefix.Contains(ip)
		}
		if !blocked {
			for _, grant := range grants {
				if grant.Prefix.Contains(ip) {
					ports = append(ports, grant.Ports...)
				}
			}
			ports = slices.DeleteFunc(ports, func(port api.AgentPolicyPort) bool { return slices.Contains(blockedPorts, port) })
		}
		var err error
		ports, err = canonicalObjects(ports)
		if err != nil {
			return nil, err
		}
		if i > 0 && !slices.Equal(ports, previous) {
			return nil, fmt.Errorf("%w: managed peer addresses require inconsistent controls", ErrUnsupportedControl)
		}
		previous = ports
	}
	return previous, nil
}

// Partition overlapping internal exceptions before producing port complements;
// otherwise the deny for one rule would incorrectly override a sibling allow.
func partitionGrants(grants []addressGrant, capabilities sandbox.Capabilities) ([]addressGrant, error) {
	var result []addressGrant
	remaining := MaxNetworkPartitionWork
	var visit func(netip.Prefix, []addressGrant) error
	visit = func(prefix netip.Prefix, candidates []addressGrant) error {
		if err := consumeBudget(&remaining, 1+len(candidates), "network partition work"); err != nil {
			return err
		}
		var overlaps []addressGrant
		var ports []api.AgentPolicyPort
		partial := false
		for _, candidate := range candidates {
			if !prefix.Overlaps(candidate.Prefix) {
				continue
			}
			overlaps = append(overlaps, candidate)
			if candidate.Prefix.Bits() <= prefix.Bits() {
				if err := consumeBudget(&remaining, len(candidate.Ports), "network partition work"); err != nil {
					return err
				}
				ports = append(ports, candidate.Ports...)
			} else {
				partial = true
			}
		}
		if len(overlaps) == 0 {
			return nil
		}
		if !partial {
			if len(result) == MaxNetworkRules/2 {
				return fmt.Errorf("%w: partitioned grants exceed the network rule budget", ErrConflict)
			}
			canonical, err := canonicalObjects(ports)
			if err != nil {
				return err
			}
			result = append(result, addressGrant{prefix, canonical})
			return nil
		}
		bits := prefix.Bits() + 1
		left := netip.PrefixFrom(prefix.Addr(), bits)
		right := prefix.Addr()
		if right.Is4() {
			bytes := right.As4()
			bytes[(bits-1)/8] |= 1 << (7 - ((bits - 1) % 8))
			right = netip.AddrFrom4(bytes)
		} else {
			bytes := right.As16()
			bytes[(bits-1)/8] |= 1 << (7 - ((bits - 1) % 8))
			right = netip.AddrFrom16(bytes)
		}
		if err := visit(left, overlaps); err != nil {
			return err
		}
		return visit(netip.PrefixFrom(right, bits), overlaps)
	}
	for _, root := range familyRoots(capabilities) {
		if err := visit(netip.MustParsePrefix(root), grants); err != nil {
			return nil, err
		}
	}
	return result, nil
}
