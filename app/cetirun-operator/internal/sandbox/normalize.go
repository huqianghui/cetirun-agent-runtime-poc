// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"cmp"
	"math/big"
	"net/netip"
	"slices"
	"strings"

	v1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	pathvalidation "k8s.io/apimachinery/pkg/api/validation/path"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// DefaultSandbox applies schema defaults to an independent copy without
// validating or canonicalizing explicitly supplied values.
func DefaultSandbox(spec v1alpha1.SandboxSpec) v1alpha1.SandboxSpec {
	defaulted := *spec.DeepCopy()
	if defaulted.Type == "" {
		defaulted.Type = v1alpha1.SandboxKata
	}
	return defaulted
}

// NormalizeSandbox validates local values and returns an independent canonical copy.
// It does not establish runtime availability, storage approval or enforcement.
func NormalizeSandbox(spec v1alpha1.SandboxSpec) (v1alpha1.SandboxSpec, field.ErrorList) {
	normalized := DefaultSandbox(spec)
	path := field.NewPath("spec")
	var errs field.ErrorList
	switch normalized.Type {
	case v1alpha1.SandboxKata, v1alpha1.SandboxGVisor, v1alpha1.SandboxContainer:
	default:
		errs = append(errs, field.NotSupported(path.Child("type"), normalized.Type, []string{"kata", "gvisor", "container"}))
	}

	errs = append(errs, normalizeContainerResources(&normalized.Resources.Agent, path.Child("resources", "agent"))...)
	errs = append(errs, normalizeContainerResources(&normalized.Resources.Helpers, path.Child("resources", "helpers"))...)
	homePath := path.Child("storage", "home")
	errs = append(errs, normalizationNameErrors(normalized.Storage.Home.StorageClassName, homePath.Child("storageClassName"), validation.IsDNS1123Subdomain)...)
	var quantityErrors field.ErrorList
	normalized.Storage.Home.Size, quantityErrors = normalizePositiveQuantity(normalized.Storage.Home.Size, homePath.Child("size"))
	errs = append(errs, quantityErrors...)
	normalized.Storage.Scratch.SizeLimit, quantityErrors = normalizePositiveQuantity(normalized.Storage.Scratch.SizeLimit, path.Child("storage", "scratch", "sizeLimit"))
	errs = append(errs, quantityErrors...)
	switch normalized.Storage.Home.AccessMode {
	case corev1.ReadWriteOncePod, corev1.ReadWriteOnce:
	default:
		errs = append(errs, field.NotSupported(homePath.Child("accessMode"), normalized.Storage.Home.AccessMode, []string{string(corev1.ReadWriteOncePod), string(corev1.ReadWriteOnce)}))
	}
	return normalized, errs
}

func normalizeContainerResources(resources *v1alpha1.SandboxContainerResources, path *field.Path) field.ErrorList {
	errs := normalizeResourceList(resources.Requests, path.Child("requests"))
	errs = append(errs, normalizeResourceList(resources.Limits, path.Child("limits"))...)
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		request, hasRequest := resources.Requests[name]
		limit, hasLimit := resources.Limits[name]
		if hasRequest && hasLimit && request.Sign() > 0 && limit.Sign() > 0 && request.Cmp(limit) > 0 {
			errs = append(errs, field.Invalid(path.Child("requests").Key(string(name)), request.String(), "must not exceed the corresponding limit"))
		}
	}
	return errs
}

func normalizeResourceList(resources corev1.ResourceList, path *field.Path) field.ErrorList {
	allowed := []string{"cpu", "memory", "ephemeral-storage"}
	var errs field.ErrorList
	for _, name := range allowed {
		if _, found := resources[corev1.ResourceName(name)]; !found {
			errs = append(errs, field.Required(path.Key(name), "a positive quantity is required"))
		}
	}
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, string(name))
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(allowed, name) {
			errs = append(errs, field.NotSupported(path.Key(name), name, allowed))
		}
		quantity, quantityErrors := normalizePositiveQuantity(resources[corev1.ResourceName(name)], path.Key(name))
		resources[corev1.ResourceName(name)] = quantity
		errs = append(errs, quantityErrors...)
	}
	return errs
}

func normalizePositiveQuantity(value resource.Quantity, path *field.Path) (resource.Quantity, field.ErrorList) {
	// A single format makes equivalent binary, decimal and exponent inputs hash alike.
	normalized := *resource.NewDecimalQuantity(*value.AsDec(), resource.DecimalSI)
	if normalized.Sign() <= 0 {
		return normalized, field.ErrorList{field.Invalid(path, normalized.String(), "must be a positive quantity")}
	}
	return normalized, nil
}

// DefaultAgentPolicy applies schema defaults to an independent copy. Unlike
// normalization, it preserves explicit values, list order and duplicates.
func DefaultAgentPolicy(spec v1alpha1.AgentPolicySpec) v1alpha1.AgentPolicySpec {
	defaulted := *spec.DeepCopy()
	if defaulted.Security.ReadOnlyRootFilesystem == nil {
		readOnly := true
		defaulted.Security.ReadOnlyRootFilesystem = &readOnly
	}
	if defaulted.Network.Ingress.Mode == "" {
		defaulted.Network.Ingress.Mode = v1alpha1.AgentPolicyIngressAuthenticated
	}
	defaultPolicyPorts(defaulted.Network.BlockedPorts)
	for i := range defaulted.Network.AllowedInternalCIDRs {
		defaultPolicyPorts(defaulted.Network.AllowedInternalCIDRs[i].Ports)
	}
	return defaulted
}

func defaultPolicyPorts(ports []v1alpha1.AgentPolicyPort) {
	for i := range ports {
		if ports[i].Protocol == "" {
			ports[i].Protocol = corev1.ProtocolTCP
		}
	}
}

// NormalizeAgentPolicy validates and canonicalizes intent without inferring grants
// or platform capabilities. The returned value never shares mutable input state.
func NormalizeAgentPolicy(spec v1alpha1.AgentPolicySpec) (v1alpha1.AgentPolicySpec, field.ErrorList) {
	normalized := DefaultAgentPolicy(spec)
	path := field.NewPath("spec")
	var errs field.ErrorList
	networkPath := path.Child("network")
	network := &normalized.Network
	network.BlockedCIDRs = make([]string, len(spec.Network.BlockedCIDRs))
	for i, value := range spec.Network.BlockedCIDRs {
		cidr, cidrErrors := normalizeCIDR(value, networkPath.Child("blockedCIDRs").Index(i))
		network.BlockedCIDRs[i] = cidr
		errs = append(errs, cidrErrors...)
	}
	slices.Sort(network.BlockedCIDRs)
	network.BlockedCIDRs = slices.Compact(network.BlockedCIDRs)

	network.BlockedFQDNs = append([]string{}, spec.Network.BlockedFQDNs...)
	for i, value := range network.BlockedFQDNs {
		errs = append(errs, validatePolicyHostname(value, networkPath.Child("blockedFQDNs").Index(i))...)
	}
	slices.Sort(network.BlockedFQDNs)
	network.BlockedFQDNs = slices.Compact(network.BlockedFQDNs)

	var portErrors field.ErrorList
	network.BlockedPorts, portErrors = normalizePolicyPorts(network.BlockedPorts, networkPath.Child("blockedPorts"))
	errs = append(errs, portErrors...)
	rules := make(map[string][]v1alpha1.AgentPolicyPort, len(network.AllowedInternalCIDRs))
	for i, rule := range network.AllowedInternalCIDRs {
		rulePath := networkPath.Child("allowedInternalCIDRs").Index(i)
		cidr, cidrErrors := normalizeCIDR(rule.CIDR, rulePath.Child("cidr"))
		errs = append(errs, cidrErrors...)
		if len(rule.Ports) == 0 {
			errs = append(errs, field.Required(rulePath.Child("ports"), "at least one explicit destination port is required"))
		}
		ports, rulePortErrors := normalizePolicyPorts(rule.Ports, rulePath.Child("ports"))
		errs = append(errs, rulePortErrors...)
		rules[cidr] = append(rules[cidr], ports...)
	}
	cidrs := make([]string, 0, len(rules))
	for cidr := range rules {
		cidrs = append(cidrs, cidr)
	}
	slices.Sort(cidrs)
	network.AllowedInternalCIDRs = make([]v1alpha1.AgentPolicyCIDRRule, 0, len(cidrs))
	for _, cidr := range cidrs {
		network.AllowedInternalCIDRs = append(network.AllowedInternalCIDRs, v1alpha1.AgentPolicyCIDRRule{
			CIDR: cidr, Ports: sortPolicyPorts(rules[cidr]),
		})
	}

	switch network.Ingress.Mode {
	case v1alpha1.AgentPolicyIngressAuthenticated:
	case v1alpha1.AgentPolicyIngressDisabled:
		if network.Ingress.RequireMTLS {
			errs = append(errs, field.Invalid(networkPath.Child("ingress", "requireMTLS"), true, "disabled ingress cannot require mTLS"))
		}
	default:
		errs = append(errs, field.NotSupported(networkPath.Child("ingress", "mode"), network.Ingress.Mode, []string{"authenticated", "disabled"}))
	}
	if normalized.KubernetesAccess != nil {
		errs = append(errs, normalizeKubernetesAccess(normalized.KubernetesAccess, path.Child("kubernetesAccess"))...)
	}
	errs = append(errs, normalizePolicyConstraints(&normalized, path)...)
	return normalized, errs
}

// normalizePolicyConstraints validates the cross-field semantics of the
// policy entitlement surface. CRD validations intentionally cover only
// local shape; acceptance must reject policies that cannot be projected to a
// typed Gateway or credential-service implementation.
func normalizePolicyConstraints(policy *v1alpha1.AgentPolicySpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	harnessPath := path.Child("harnesses", "allowed")
	if len(policy.Harnesses.Allowed) == 0 {
		errs = append(errs, field.Required(harnessPath, "at least one harness is required"))
	} else {
		slices.Sort(policy.Harnesses.Allowed)
		policy.Harnesses.Allowed = slices.Compact(policy.Harnesses.Allowed)
		for i, harness := range policy.Harnesses.Allowed {
			switch harness {
			case v1alpha1.AgentTypeOpenClaw, v1alpha1.AgentTypeHermes, v1alpha1.AgentTypeCustom:
			default:
				errs = append(errs, field.NotSupported(harnessPath.Index(i), harness, []string{string(v1alpha1.AgentTypeOpenClaw), string(v1alpha1.AgentTypeHermes), string(v1alpha1.AgentTypeCustom)}))
			}
		}
	}
	policy.Skills.Allowed = canonicalNonEmptyStrings(policy.Skills.Allowed, path.Child("skills", "allowed"), &errs)
	policy.ModelAccess.SharedModels.Allowed = canonicalNonEmptyStrings(policy.ModelAccess.SharedModels.Allowed, path.Child("modelAccess", "sharedModels", "allowed"), &errs)
	hasModelEntitlement := policy.ModelAccess.Copilot.Allowed || len(policy.ModelAccess.SharedModels.Allowed) > 0
	budgetPath := path.Child("modelAccess", "gatewayBudget")
	if hasModelEntitlement && policy.ModelAccess.GatewayBudget == nil {
		errs = append(errs, field.Required(budgetPath, "a Gateway budget is required for every model entitlement"))
	}
	if !hasModelEntitlement && policy.ModelAccess.GatewayBudget != nil {
		errs = append(errs, field.Forbidden(budgetPath, "a Gateway budget is allowed only with a model entitlement"))
	}
	if budget := policy.ModelAccess.GatewayBudget; budget != nil {
		defaultBudget, defaultOK := new(big.Rat).SetString(budget.DefaultMonthlyUSD)
		maximumBudget, maximumOK := new(big.Rat).SetString(budget.MaxMonthlyUSD)
		if !defaultOK || defaultBudget.Sign() <= 0 {
			errs = append(errs, field.Invalid(budgetPath.Child("defaultMonthlyUSD"), budget.DefaultMonthlyUSD, "must be a positive decimal"))
		}
		if !maximumOK || maximumBudget.Sign() <= 0 {
			errs = append(errs, field.Invalid(budgetPath.Child("maxMonthlyUSD"), budget.MaxMonthlyUSD, "must be a positive decimal"))
		}
		if defaultOK && maximumOK && defaultBudget.Sign() > 0 && maximumBudget.Sign() > 0 && defaultBudget.Cmp(maximumBudget) > 0 {
			errs = append(errs, field.Invalid(budgetPath.Child("defaultMonthlyUSD"), budget.DefaultMonthlyUSD, "must not exceed maxMonthlyUSD"))
		}
	}
	errs = append(errs, ValidatePolicyRateLimits(policy.ModelAccess.RateLimits, path.Child("modelAccess", "rateLimits"))...)
	identityPath := path.Child("agentIdentity")
	identity := &policy.AgentIdentity
	identity.CredentialServices = canonicalNonEmptyStrings(identity.CredentialServices, identityPath.Child("credentialServices"), &errs)
	for i, service := range identity.CredentialServices {
		if service != "azure-resource-manager" {
			errs = append(errs, field.NotSupported(identityPath.Child("credentialServices").Index(i), service, []string{"azure-resource-manager"}))
		}
	}
	if identity.Required && !identity.Allowed {
		errs = append(errs, field.Invalid(identityPath.Child("required"), true, "requires allowed=true"))
	}
	if identity.Allowed && !slices.Contains(identity.CredentialServices, "azure-resource-manager") {
		errs = append(errs, field.Required(identityPath.Child("credentialServices"), "allowed Agent Identity requires azure-resource-manager"))
	}
	if !identity.Allowed && len(identity.CredentialServices) != 0 {
		errs = append(errs, field.Forbidden(identityPath.Child("credentialServices"), "credential services require allowed=true"))
	}
	return errs
}

func canonicalNonEmptyStrings(values []string, path *field.Path, errs *field.ErrorList) []string {
	canonical := append([]string(nil), values...)
	for i, value := range canonical {
		if strings.TrimSpace(value) == "" {
			*errs = append(*errs, field.Invalid(path.Index(i), value, "must not be blank"))
		}
	}
	slices.Sort(canonical)
	return slices.Compact(canonical)
}

func normalizeCIDR(value string, path *field.Path) (string, field.ErrorList) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return value, field.ErrorList{field.Invalid(path, value, "must be an IPv4 or IPv6 CIDR")}
	}
	if prefix.Addr().Is4In6() {
		return value, field.ErrorList{field.Invalid(path, value, "IPv4-mapped IPv6 aliases are not supported; use an IPv4 CIDR")}
	}
	return prefix.Masked().String(), nil
}

func validatePolicyHostname(value string, path *field.Path) field.ErrorList {
	if errs := validation.IsDNS1123Subdomain(value); len(errs) != 0 {
		return field.ErrorList{field.Invalid(path, value, "must be an exact lowercase ASCII hostname without a trailing dot, wildcard or URL")}
	}
	for _, label := range strings.Split(value, ".") {
		if len(validation.IsDNS1123Label(label)) != 0 {
			return field.ErrorList{field.Invalid(path, value, "hostname labels must contain at most 63 characters")}
		}
	}
	if _, err := netip.ParseAddr(value); err == nil {
		return field.ErrorList{field.Invalid(path, value, "IP literals are not hostnames")}
	}
	return nil
}

func normalizePolicyPorts(input []v1alpha1.AgentPolicyPort, path *field.Path) ([]v1alpha1.AgentPolicyPort, field.ErrorList) {
	ports := append([]v1alpha1.AgentPolicyPort{}, input...)
	var errs field.ErrorList
	for i := range ports {
		port := &ports[i]
		if port.Port < 1 || port.Port > 65535 {
			errs = append(errs, field.Invalid(path.Index(i).Child("port"), port.Port, "must be between 1 and 65535"))
		}
		switch port.Protocol {
		case corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP:
		default:
			errs = append(errs, field.NotSupported(path.Index(i).Child("protocol"), port.Protocol, []string{"TCP", "UDP", "SCTP"}))
		}
	}
	return sortPolicyPorts(ports), errs
}

func sortPolicyPorts(ports []v1alpha1.AgentPolicyPort) []v1alpha1.AgentPolicyPort {
	slices.SortFunc(ports, func(left, right v1alpha1.AgentPolicyPort) int {
		if order := cmp.Compare(left.Port, right.Port); order != 0 {
			return order
		}
		return cmp.Compare(left.Protocol, right.Protocol)
	})
	return slices.Compact(ports)
}

func normalizeKubernetesAccess(access *v1alpha1.AgentPolicyAdminKubernetesAccess, path *field.Path) field.ErrorList {
	errs := normalizationNameErrors(access.ServiceAccountName, path.Child("serviceAccountName"), validation.IsDNS1123Subdomain)
	if len(access.RoleBindings)+len(access.ClusterRoleBindings) == 0 {
		errs = append(errs, field.Required(path, "at least one explicit RBAC grant is required"))
	}
	for i, binding := range access.RoleBindings {
		bindingPath := path.Child("roleBindings").Index(i)
		errs = append(errs, normalizationNameErrors(binding.Namespace, bindingPath.Child("namespace"), validation.IsDNS1123Label)...)
		errs = append(errs, normalizationNameErrors(binding.ClusterRoleName, bindingPath.Child("clusterRoleName"), pathvalidation.IsValidPathSegmentName)...)
	}
	for i, binding := range access.ClusterRoleBindings {
		errs = append(errs, normalizationNameErrors(binding.ClusterRoleName, path.Child("clusterRoleBindings").Index(i).Child("clusterRoleName"), pathvalidation.IsValidPathSegmentName)...)
	}
	access.RoleBindings = append([]v1alpha1.AgentPolicyRoleBinding{}, access.RoleBindings...)
	slices.SortFunc(access.RoleBindings, func(left, right v1alpha1.AgentPolicyRoleBinding) int {
		if order := cmp.Compare(left.Namespace, right.Namespace); order != 0 {
			return order
		}
		return cmp.Compare(left.ClusterRoleName, right.ClusterRoleName)
	})
	access.RoleBindings = slices.Compact(access.RoleBindings)
	access.ClusterRoleBindings = append([]v1alpha1.AgentPolicyClusterRoleBinding{}, access.ClusterRoleBindings...)
	slices.SortFunc(access.ClusterRoleBindings, func(left, right v1alpha1.AgentPolicyClusterRoleBinding) int {
		return cmp.Compare(left.ClusterRoleName, right.ClusterRoleName)
	})
	access.ClusterRoleBindings = slices.Compact(access.ClusterRoleBindings)
	return errs
}

func normalizationNameErrors(value string, path *field.Path, validate func(string) []string) field.ErrorList {
	if value == "" {
		return field.ErrorList{field.Required(path, "an explicit name is required")}
	}
	if messages := validate(value); len(messages) != 0 {
		return field.ErrorList{field.Invalid(path, value, strings.Join(messages, "; "))}
	}
	return nil
}
