// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

// AdminPolicyHandler implements administrative CRUD for the Portal-managed
// portion of cluster-scoped AgentPolicy resources. Runtime components remain
// responsible for interpreting and enforcing the policy fields they own.
type AdminPolicyHandler struct {
	Kube *kube.Client
	// Config enables the Skill Marketplace check of removed skills.
	Config config.Config
}

var policyNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
var monthlyUSDAmountPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$`)

func validAgentPolicyResourceName(name string) bool {
	return len(validation.IsDNS1123Subdomain(name)) == 0
}

type AgentPolicyHarnessesDTO struct {
	Allowed []cetirunv1alpha1.AgentType `json:"allowed"`
}

type AgentPolicySkillsDTO struct {
	Allowed []string `json:"allowed,omitempty"`
}

type AgentPolicyCopilotAccessDTO struct {
	Allowed bool `json:"allowed,omitempty"`
}

type AgentPolicySharedModelsAccessDTO struct {
	Allowed []string `json:"allowed,omitempty"`
}

type AgentPolicyGatewayBudgetDTO struct {
	DefaultMonthlyUSD string `json:"defaultMonthlyUSD"`
	MaxMonthlyUSD     string `json:"maxMonthlyUSD"`
}

type AgentPolicyModelAccessDTO struct {
	Copilot       AgentPolicyCopilotAccessDTO      `json:"copilot,omitempty"`
	SharedModels  AgentPolicySharedModelsAccessDTO `json:"sharedModels,omitempty"`
	GatewayBudget *AgentPolicyGatewayBudgetDTO     `json:"gatewayBudget,omitempty"`
	// RateLimits replaces the policy's limits on save; omitting it removes them.
	RateLimits *AgentPolicyRateLimitsDTO `json:"rateLimits,omitempty"`
}

type AgentPolicyAgentIdentityDTO struct {
	Allowed            bool     `json:"allowed,omitempty"`
	Required           bool     `json:"required,omitempty"`
	CredentialServices []string `json:"credentialServices,omitempty"`
}

type AgentPolicyEntraIDDTO struct {
	Enabled bool `json:"enabled,omitempty"`
}

type AgentPolicySandboxSelectionDTO struct {
	AllowMultiple bool `json:"allowMultiple,omitempty"`
}

type AgentPolicySandboxSelectionRequestDTO struct {
	AllowMultiple *bool `json:"allowMultiple,omitempty"`
}

type AgentPolicySecurityDTO struct {
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
}

type AgentPolicyPortDTO struct {
	Port     int32           `json:"port"`
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

type AgentPolicyCIDRRuleDTO struct {
	CIDR  string               `json:"cidr"`
	Ports []AgentPolicyPortDTO `json:"ports"`
}

type AgentPolicyIngressDTO struct {
	Mode        cetirunv1alpha1.AgentPolicyIngressMode `json:"mode,omitempty"`
	RequireMTLS bool                                   `json:"requireMTLS,omitempty"`
}

type AgentPolicyNetworkDTO struct {
	BlockedCIDRs         []string                 `json:"blockedCIDRs,omitempty"`
	BlockedFQDNs         []string                 `json:"blockedFQDNs,omitempty"`
	BlockedPorts         []AgentPolicyPortDTO     `json:"blockedPorts,omitempty"`
	AllowedInternalCIDRs []AgentPolicyCIDRRuleDTO `json:"allowedInternalCIDRs,omitempty"`
	Ingress              AgentPolicyIngressDTO    `json:"ingress,omitempty"`
}

type AgentPolicyRoleBindingDTO struct {
	Namespace       string `json:"namespace"`
	ClusterRoleName string `json:"clusterRoleName"`
}

type AgentPolicyClusterRoleBindingDTO struct {
	ClusterRoleName string `json:"clusterRoleName"`
}

type AgentPolicyKubernetesAccessDTO struct {
	ServiceAccountName  string                             `json:"serviceAccountName"`
	RoleBindings        []AgentPolicyRoleBindingDTO        `json:"roleBindings,omitempty"`
	ClusterRoleBindings []AgentPolicyClusterRoleBindingDTO `json:"clusterRoleBindings,omitempty"`
}

// Request-only types use pointers so an update can distinguish a field that
// was omitted from an explicit empty value.
type AgentPolicyIngressRequestDTO struct {
	Mode        *cetirunv1alpha1.AgentPolicyIngressMode `json:"mode,omitempty"`
	RequireMTLS *bool                                   `json:"requireMTLS,omitempty"`
}

type AgentPolicyNetworkRequestDTO struct {
	BlockedCIDRs         *[]string                     `json:"blockedCIDRs,omitempty"`
	BlockedFQDNs         *[]string                     `json:"blockedFQDNs,omitempty"`
	BlockedPorts         *[]AgentPolicyPortDTO         `json:"blockedPorts,omitempty"`
	AllowedInternalCIDRs *[]AgentPolicyCIDRRuleDTO     `json:"allowedInternalCIDRs,omitempty"`
	Ingress              *AgentPolicyIngressRequestDTO `json:"ingress,omitempty"`
}

// AgentPolicyDTO exposes the fields managed by the Portal. Updates preserve
// fields owned by runtime components that are not represented here.
type AgentPolicyDTO struct {
	Name               string                          `json:"name"`
	UID                string                          `json:"uid"`
	ResourceVersion    string                          `json:"resourceVersion"`
	Harnesses          AgentPolicyHarnessesDTO         `json:"harnesses"`
	Skills             AgentPolicySkillsDTO            `json:"skills,omitempty"`
	ModelAccess        AgentPolicyModelAccessDTO       `json:"modelAccess,omitempty"`
	AgentIdentity      AgentPolicyAgentIdentityDTO     `json:"agentIdentity,omitempty"`
	EntraID            AgentPolicyEntraIDDTO           `json:"entraID,omitempty"`
	SandboxSelection   AgentPolicySandboxSelectionDTO  `json:"sandboxSelection,omitempty"`
	Security           AgentPolicySecurityDTO          `json:"security,omitempty"`
	Network            AgentPolicyNetworkDTO           `json:"network,omitempty"`
	KubernetesAccess   *AgentPolicyKubernetesAccessDTO `json:"kubernetesAccess,omitempty"`
	Generation         int64                           `json:"generation,omitempty"`
	ObservedGeneration int64                           `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition              `json:"conditions,omitempty"`
	Deleting           bool                            `json:"deleting"`
	CreatedAt          string                          `json:"createdAt,omitempty"`
}

// AgentPolicyUpsertRequest is the public create/update body.
type AgentPolicyUpsertRequest struct {
	Name             string                                 `json:"name,omitempty"`
	UID              string                                 `json:"uid,omitempty"`
	ResourceVersion  string                                 `json:"resourceVersion,omitempty"`
	Harnesses        AgentPolicyHarnessesDTO                `json:"harnesses"`
	Skills           AgentPolicySkillsDTO                   `json:"skills,omitempty"`
	ModelAccess      AgentPolicyModelAccessDTO              `json:"modelAccess,omitempty"`
	AgentIdentity    *AgentPolicyAgentIdentityDTO           `json:"agentIdentity,omitempty"`
	EntraID          *AgentPolicyEntraIDDTO                 `json:"entraID,omitempty"`
	SandboxSelection *AgentPolicySandboxSelectionRequestDTO `json:"sandboxSelection,omitempty"`
	Security         *AgentPolicySecurityDTO                `json:"security,omitempty"`
	Network          *AgentPolicyNetworkRequestDTO          `json:"network,omitempty"`
	KubernetesAccess *AgentPolicyKubernetesAccessDTO        `json:"kubernetesAccess,omitempty"`
	present          map[string]bool
}

// UnmarshalJSON records top-level field presence so the remaining read-only
// fields can distinguish omission from an explicit request to change them.
func (r *AgentPolicyUpsertRequest) UnmarshalJSON(data []byte) error {
	type plain AgentPolicyUpsertRequest
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = AgentPolicyUpsertRequest(decoded)
	r.present = make(map[string]bool, len(raw))
	for key := range raw {
		r.present[key] = true
	}
	return nil
}

func (r AgentPolicyUpsertRequest) has(name string) bool { return r.present[name] }

func toAgentPolicyDTO(policy *cetirunv1alpha1.AgentPolicy) AgentPolicyDTO {
	return AgentPolicyDTO{
		Name:            policy.Name,
		UID:             string(policy.UID),
		ResourceVersion: policy.ResourceVersion,
		Harnesses:       AgentPolicyHarnessesDTO{Allowed: append([]cetirunv1alpha1.AgentType(nil), policy.Spec.Harnesses.Allowed...)},
		Skills:          AgentPolicySkillsDTO{Allowed: append([]string(nil), policy.Spec.Skills.Allowed...)},
		ModelAccess: AgentPolicyModelAccessDTO{
			Copilot:       AgentPolicyCopilotAccessDTO{Allowed: policy.Spec.ModelAccess.Copilot.Allowed},
			SharedModels:  AgentPolicySharedModelsAccessDTO{Allowed: append([]string(nil), policy.Spec.ModelAccess.SharedModels.Allowed...)},
			GatewayBudget: toAgentPolicyGatewayBudgetDTO(policy.Spec.ModelAccess.GatewayBudget),
			RateLimits:    toAgentPolicyRateLimitsDTO(policy.Spec.ModelAccess.RateLimits),
		},
		AgentIdentity: AgentPolicyAgentIdentityDTO{
			Allowed:            policy.Spec.AgentIdentity.Allowed,
			Required:           policy.Spec.AgentIdentity.Required,
			CredentialServices: append([]string(nil), policy.Spec.AgentIdentity.CredentialServices...),
		},
		EntraID:            AgentPolicyEntraIDDTO{Enabled: policy.Spec.EntraID.Enabled},
		SandboxSelection:   AgentPolicySandboxSelectionDTO{AllowMultiple: policy.Spec.SandboxSelection.AllowMultiple},
		Security:           AgentPolicySecurityDTO{ReadOnlyRootFilesystem: cloneBool(policy.Spec.Security.ReadOnlyRootFilesystem)},
		Network:            toAgentPolicyNetworkDTO(policy.Spec.Network),
		KubernetesAccess:   toAgentPolicyKubernetesAccessDTO(policy.Spec.KubernetesAccess),
		Generation:         policy.Generation,
		ObservedGeneration: policy.Status.ObservedGeneration,
		Conditions:         append([]metav1.Condition(nil), policy.Status.Conditions...),
		Deleting:           !policy.DeletionTimestamp.IsZero(),
		CreatedAt:          policy.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func toAgentPolicyNetworkDTO(network cetirunv1alpha1.AgentPolicyNetwork) AgentPolicyNetworkDTO {
	out := AgentPolicyNetworkDTO{
		BlockedCIDRs: append([]string(nil), network.BlockedCIDRs...),
		BlockedFQDNs: append([]string(nil), network.BlockedFQDNs...),
		Ingress:      AgentPolicyIngressDTO{Mode: network.Ingress.Mode, RequireMTLS: network.Ingress.RequireMTLS},
	}
	for _, port := range network.BlockedPorts {
		out.BlockedPorts = append(out.BlockedPorts, AgentPolicyPortDTO{Port: port.Port, Protocol: port.Protocol})
	}
	for _, rule := range network.AllowedInternalCIDRs {
		dto := AgentPolicyCIDRRuleDTO{CIDR: rule.CIDR}
		for _, port := range rule.Ports {
			dto.Ports = append(dto.Ports, AgentPolicyPortDTO{Port: port.Port, Protocol: port.Protocol})
		}
		out.AllowedInternalCIDRs = append(out.AllowedInternalCIDRs, dto)
	}
	return out
}

func toAgentPolicyKubernetesAccessDTO(access *cetirunv1alpha1.AgentPolicyAdminKubernetesAccess) *AgentPolicyKubernetesAccessDTO {
	if access == nil {
		return nil
	}
	out := &AgentPolicyKubernetesAccessDTO{ServiceAccountName: access.ServiceAccountName}
	for _, binding := range access.RoleBindings {
		out.RoleBindings = append(out.RoleBindings, AgentPolicyRoleBindingDTO{Namespace: binding.Namespace, ClusterRoleName: binding.ClusterRoleName})
	}
	for _, binding := range access.ClusterRoleBindings {
		out.ClusterRoleBindings = append(out.ClusterRoleBindings, AgentPolicyClusterRoleBindingDTO{ClusterRoleName: binding.ClusterRoleName})
	}
	return out
}

func toAgentPolicyGatewayBudgetDTO(budget *cetirunv1alpha1.AgentPolicyGatewayBudget) *AgentPolicyGatewayBudgetDTO {
	if budget == nil {
		return nil
	}
	return &AgentPolicyGatewayBudgetDTO{
		DefaultMonthlyUSD: budget.DefaultMonthlyUSD,
		MaxMonthlyUSD:     budget.MaxMonthlyUSD,
	}
}

func toAgentPolicySpec(req AgentPolicyUpsertRequest) cetirunv1alpha1.AgentPolicySpec {
	readOnly := true
	spec := cetirunv1alpha1.AgentPolicySpec{
		Harnesses: cetirunv1alpha1.AgentPolicyHarnesses{Allowed: append([]cetirunv1alpha1.AgentType(nil), req.Harnesses.Allowed...)},
		Skills:    cetirunv1alpha1.AgentPolicySkills{Allowed: append([]string(nil), req.Skills.Allowed...)},
		ModelAccess: cetirunv1alpha1.AgentPolicyModelAccess{
			Copilot:       cetirunv1alpha1.AgentPolicyCopilotAccess{Allowed: req.ModelAccess.Copilot.Allowed},
			SharedModels:  cetirunv1alpha1.AgentPolicySharedModelsAccess{Allowed: append([]string(nil), req.ModelAccess.SharedModels.Allowed...)},
			GatewayBudget: toAgentPolicyGatewayBudget(req.ModelAccess.GatewayBudget),
			RateLimits:    toAgentPolicyRateLimits(req.ModelAccess.RateLimits),
		},
		Security: cetirunv1alpha1.AgentPolicySecurity{ReadOnlyRootFilesystem: &readOnly},
		Network:  cetirunv1alpha1.AgentPolicyNetwork{Ingress: cetirunv1alpha1.AgentPolicyIngress{Mode: cetirunv1alpha1.AgentPolicyIngressAuthenticated}},
	}
	applyAgentPolicyRequest(&spec, req)
	return spec
}

func toAgentPolicyGatewayBudget(budget *AgentPolicyGatewayBudgetDTO) *cetirunv1alpha1.AgentPolicyGatewayBudget {
	if budget == nil {
		return nil
	}
	return &cetirunv1alpha1.AgentPolicyGatewayBudget{
		DefaultMonthlyUSD: budget.DefaultMonthlyUSD,
		MaxMonthlyUSD:     budget.MaxMonthlyUSD,
	}
}

// applyAgentPolicyRequest applies the Portal-managed fields. Identity groups
// replace their complete stored value: omitted or null identity groups disable
// the corresponding capability.
func applyAgentPolicyRequest(spec *cetirunv1alpha1.AgentPolicySpec, req AgentPolicyUpsertRequest) {
	spec.AgentIdentity = cetirunv1alpha1.AgentPolicyAgentIdentity{}
	if req.AgentIdentity != nil {
		spec.AgentIdentity = cetirunv1alpha1.AgentPolicyAgentIdentity{
			Allowed:            req.AgentIdentity.Allowed,
			Required:           req.AgentIdentity.Required,
			CredentialServices: append([]string(nil), req.AgentIdentity.CredentialServices...),
		}
	}
	spec.EntraID = cetirunv1alpha1.AgentPolicyEntraID{}
	if req.EntraID != nil {
		spec.EntraID.Enabled = req.EntraID.Enabled
	}
	if req.SandboxSelection != nil && req.SandboxSelection.AllowMultiple != nil {
		spec.SandboxSelection.AllowMultiple = *req.SandboxSelection.AllowMultiple
	}
	if req.Security != nil && req.Security.ReadOnlyRootFilesystem != nil {
		spec.Security.ReadOnlyRootFilesystem = cloneBool(req.Security.ReadOnlyRootFilesystem)
	}
	if network := req.Network; network != nil {
		if network.BlockedCIDRs != nil {
			spec.Network.BlockedCIDRs = append([]string(nil), (*network.BlockedCIDRs)...)
		}
		if network.BlockedPorts != nil {
			spec.Network.BlockedPorts = toAgentPolicyPorts(*network.BlockedPorts)
		}
		if network.AllowedInternalCIDRs != nil {
			spec.Network.AllowedInternalCIDRs = toAgentPolicyCIDRRules(*network.AllowedInternalCIDRs)
		}
		if network.Ingress != nil && network.Ingress.Mode != nil {
			spec.Network.Ingress.Mode = *network.Ingress.Mode
		}
	}
}

func toAgentPolicyPorts(ports []AgentPolicyPortDTO) []cetirunv1alpha1.AgentPolicyPort {
	out := make([]cetirunv1alpha1.AgentPolicyPort, 0, len(ports))
	for _, port := range ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		out = append(out, cetirunv1alpha1.AgentPolicyPort{Port: port.Port, Protocol: protocol})
	}
	return out
}

func toAgentPolicyCIDRRules(rules []AgentPolicyCIDRRuleDTO) []cetirunv1alpha1.AgentPolicyCIDRRule {
	out := make([]cetirunv1alpha1.AgentPolicyCIDRRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, cetirunv1alpha1.AgentPolicyCIDRRule{CIDR: rule.CIDR, Ports: toAgentPolicyPorts(rule.Ports)})
	}
	return out
}

func validateAgentPolicyRequest(req AgentPolicyUpsertRequest) string {
	if len(req.Harnesses.Allowed) == 0 {
		return "harnesses.allowed must contain at least one harness"
	}
	seenHarness := map[cetirunv1alpha1.AgentType]bool{}
	for _, harness := range req.Harnesses.Allowed {
		if harness != cetirunv1alpha1.AgentTypeOpenClaw && harness != cetirunv1alpha1.AgentTypeHermes &&
			harness != cetirunv1alpha1.AgentTypeCustom {
			return "harnesses.allowed may contain only openclaw, hermes or custom"
		}
		if seenHarness[harness] {
			return "harnesses.allowed must not contain duplicate entries"
		}
		seenHarness[harness] = true
	}
	hasModelEntitlement := req.ModelAccess.Copilot.Allowed || len(req.ModelAccess.SharedModels.Allowed) > 0
	if hasModelEntitlement && req.ModelAccess.GatewayBudget == nil {
		return "model access requires gatewayBudget"
	}
	if !hasModelEntitlement && req.ModelAccess.GatewayBudget != nil {
		return "gatewayBudget requires Copilot or a shared model"
	}
	if budget := req.ModelAccess.GatewayBudget; budget != nil {
		if !monthlyUSDAmountPattern.MatchString(budget.DefaultMonthlyUSD) || !monthlyUSDAmountPattern.MatchString(budget.MaxMonthlyUSD) {
			return "gatewayBudget amounts must be positive USD amounts with at most six decimal places"
		}
		defaultBudget, _ := new(big.Rat).SetString(budget.DefaultMonthlyUSD)
		maxBudget, _ := new(big.Rat).SetString(budget.MaxMonthlyUSD)
		if defaultBudget.Sign() <= 0 || maxBudget.Sign() <= 0 {
			return "gatewayBudget amounts must be greater than zero"
		}
		if defaultBudget.Cmp(maxBudget) > 0 {
			return "gatewayBudget.defaultMonthlyUSD must not exceed gatewayBudget.maxMonthlyUSD"
		}
	}
	for _, model := range req.ModelAccess.SharedModels.Allowed {
		if strings.TrimSpace(model) == "" || strings.ContainsAny(model, "\t\r\n ") || utf8.RuneCountInString(model) > 256 {
			return "sharedModels.allowed entries must be non-empty identifiers without whitespace and at most 256 characters"
		}
	}
	for _, skill := range req.Skills.Allowed {
		if strings.TrimSpace(skill) == "" {
			return "skills.allowed entries must not be blank"
		}
	}
	if hasDuplicateStrings(req.Skills.Allowed) {
		return "skills.allowed must not contain duplicate entries"
	}
	if hasDuplicateStrings(req.ModelAccess.SharedModels.Allowed) {
		return "sharedModels.allowed must not contain duplicate entries"
	}
	if message := validateAgentPolicyIdentity(req.AgentIdentity); message != "" {
		return message
	}
	return validateAgentPolicyRateLimits(req.ModelAccess.RateLimits)
}

func validateAgentPolicyIdentity(identity *AgentPolicyAgentIdentityDTO) string {
	if identity == nil {
		return ""
	}
	if identity.Required && !identity.Allowed {
		return "agentIdentity.required requires agentIdentity.allowed=true"
	}
	if !identity.Allowed && len(identity.CredentialServices) != 0 {
		return "agentIdentity.credentialServices requires agentIdentity.allowed=true"
	}
	if identity.Allowed && !slices.Contains(identity.CredentialServices, "azure-resource-manager") {
		return "agentIdentity.allowed requires the azure-resource-manager credential service"
	}
	for _, service := range identity.CredentialServices {
		if service != "azure-resource-manager" {
			return "agentIdentity.credentialServices may contain only azure-resource-manager"
		}
	}
	if hasDuplicateStrings(identity.CredentialServices) {
		return "agentIdentity.credentialServices must not contain duplicate entries"
	}
	return ""
}

func hasDuplicateStrings(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func validateAgentPolicyReadOnlyCreate(req AgentPolicyUpsertRequest) string {
	if req.KubernetesAccess != nil {
		return "kubernetesAccess is read-only in the Portal"
	}
	if req.Network != nil {
		if req.Network.BlockedFQDNs != nil && len(*req.Network.BlockedFQDNs) != 0 {
			return "network.blockedFQDNs is read-only in the Portal"
		}
		if req.Network.Ingress != nil && req.Network.Ingress.RequireMTLS != nil && *req.Network.Ingress.RequireMTLS {
			return "network.ingress.requireMTLS is read-only in the Portal"
		}
	}
	return ""
}

func validateAgentPolicyReadOnlyUpdate(req AgentPolicyUpsertRequest, current cetirunv1alpha1.AgentPolicySpec) string {
	if req.has("kubernetesAccess") && !equalKubernetesAccess(req.KubernetesAccess, current.KubernetesAccess) {
		return "kubernetesAccess is read-only in the Portal"
	}
	if req.Network != nil {
		if req.Network.BlockedFQDNs != nil && !slices.Equal(*req.Network.BlockedFQDNs, current.Network.BlockedFQDNs) {
			return "network.blockedFQDNs is read-only in the Portal"
		}
		if req.Network.Ingress != nil && req.Network.Ingress.RequireMTLS != nil && *req.Network.Ingress.RequireMTLS != current.Network.Ingress.RequireMTLS {
			return "network.ingress.requireMTLS is read-only in the Portal"
		}
	}
	return ""
}

func equalKubernetesAccess(request *AgentPolicyKubernetesAccessDTO, current *cetirunv1alpha1.AgentPolicyAdminKubernetesAccess) bool {
	if request == nil || current == nil {
		return request == nil && current == nil
	}
	if request.ServiceAccountName != current.ServiceAccountName || len(request.RoleBindings) != len(current.RoleBindings) || len(request.ClusterRoleBindings) != len(current.ClusterRoleBindings) {
		return false
	}
	for i, binding := range request.RoleBindings {
		if binding.Namespace != current.RoleBindings[i].Namespace || binding.ClusterRoleName != current.RoleBindings[i].ClusterRoleName {
			return false
		}
	}
	for i, binding := range request.ClusterRoleBindings {
		if binding.ClusterRoleName != current.ClusterRoleBindings[i].ClusterRoleName {
			return false
		}
	}
	return true
}

// validatePortalManagedPolicyFields mirrors the local shape checks for fields
// Portal can edit. The operator keeps the authoritative admission checks; the
// Portal validation makes malformed requests actionable before a cluster write.
func validatePortalManagedPolicyFields(network cetirunv1alpha1.AgentPolicyNetwork) string {
	for _, cidr := range network.BlockedCIDRs {
		if !validPolicyCIDR(cidr) {
			return "network.blockedCIDRs entries must be IPv4 or IPv6 CIDRs"
		}
	}
	if hasDuplicateStrings(network.BlockedCIDRs) {
		return "network.blockedCIDRs must not contain duplicate entries"
	}
	if message := validatePolicyPorts(network.BlockedPorts, "network.blockedPorts"); message != "" {
		return message
	}
	seenCIDRs := make(map[string]bool, len(network.AllowedInternalCIDRs))
	for _, rule := range network.AllowedInternalCIDRs {
		if !validPolicyCIDR(rule.CIDR) {
			return "network.allowedInternalCIDRs CIDRs must be IPv4 or IPv6 CIDRs"
		}
		if seenCIDRs[rule.CIDR] {
			return "network.allowedInternalCIDRs must not contain duplicate CIDRs"
		}
		seenCIDRs[rule.CIDR] = true
		if len(rule.Ports) == 0 {
			return "network.allowedInternalCIDRs ports must contain at least one port"
		}
		if message := validatePolicyPorts(rule.Ports, "network.allowedInternalCIDRs.ports"); message != "" {
			return message
		}
	}
	switch network.Ingress.Mode {
	case "", cetirunv1alpha1.AgentPolicyIngressAuthenticated, cetirunv1alpha1.AgentPolicyIngressDisabled:
	default:
		return "network.ingress.mode must be authenticated or disabled"
	}
	if network.Ingress.Mode == cetirunv1alpha1.AgentPolicyIngressDisabled && network.Ingress.RequireMTLS {
		return "network.ingress.disabled cannot require mTLS"
	}
	return ""
}

// validatePortalManagedPolicyUpdateFields validates only network fields supplied
// by an update. Existing AgentPolicies may contain repeated network rules, so an
// unrelated Portal update must preserve them without revalidating retained data.
func validatePortalManagedPolicyUpdateFields(req AgentPolicyUpsertRequest, desired cetirunv1alpha1.AgentPolicySpec) string {
	if req.Network == nil {
		return ""
	}
	network := desired.Network
	if req.Network.BlockedCIDRs == nil {
		network.BlockedCIDRs = nil
	}
	if req.Network.BlockedPorts == nil {
		network.BlockedPorts = nil
	}
	if req.Network.AllowedInternalCIDRs == nil {
		network.AllowedInternalCIDRs = nil
	}
	if req.Network.Ingress == nil || req.Network.Ingress.Mode == nil {
		network.Ingress = cetirunv1alpha1.AgentPolicyIngress{Mode: cetirunv1alpha1.AgentPolicyIngressAuthenticated}
	}
	return validatePortalManagedPolicyFields(network)
}

func validPolicyCIDR(value string) bool {
	prefix, err := netip.ParsePrefix(value)
	return err == nil && !prefix.Addr().Is4In6()
}

func validatePolicyPorts(ports []cetirunv1alpha1.AgentPolicyPort, field string) string {
	seen := make(map[cetirunv1alpha1.AgentPolicyPort]bool, len(ports))
	for _, port := range ports {
		if port.Port < 1 || port.Port > 65535 {
			return field + ".port must be between 1 and 65535"
		}
		if port.Protocol != corev1.ProtocolTCP && port.Protocol != corev1.ProtocolUDP && port.Protocol != corev1.ProtocolSCTP {
			return field + ".protocol must be TCP, UDP, or SCTP"
		}
		if seen[port] {
			return field + " must not contain duplicate ports"
		}
		seen[port] = true
	}
	return ""
}

func agentPolicyVersion(req AgentPolicyUpsertRequest) (kube.AgentPolicyVersion, string) {
	if req.UID == "" || req.ResourceVersion == "" {
		return kube.AgentPolicyVersion{}, "uid and resourceVersion from the current AgentPolicy are required"
	}
	return kube.AgentPolicyVersion{UID: types.UID(req.UID), ResourceVersion: req.ResourceVersion}, ""
}

// ListPolicies handles GET /api/v1/admin/policies.
func (h *AdminPolicyHandler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.Kube.ListAgentPolicies(r.Context())
	if err != nil {
		slog.Error("admin: list agent policies failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list agent policies")
		return
	}
	out := make([]AgentPolicyDTO, 0, len(policies))
	for i := range policies {
		out = append(out, toAgentPolicyDTO(&policies[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

// GetPolicy handles GET /api/v1/admin/policies/{name}.
func (h *AdminPolicyHandler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validAgentPolicyResourceName(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid policy name")
		return
	}
	policy, err := h.Kube.GetAgentPolicy(r.Context(), name)
	if err != nil {
		if kube.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "policy_not_found", "AgentPolicy "+name+" not found")
			return
		}
		slog.Error("admin: get agent policy failed", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to get agent policy")
		return
	}
	writeJSON(w, http.StatusOK, toAgentPolicyDTO(policy))
}

// CreatePolicy handles POST /api/v1/admin/policies.
func (h *AdminPolicyHandler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	var req AgentPolicyUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return
	}
	if !policyNamePattern.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "name must be a DNS-1123 label (start with letter, lowercase, max 63 chars)")
		return
	}
	if msg := validateAgentPolicyRequest(req); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_policy", msg)
		return
	}
	if msg := validateAgentPolicyReadOnlyCreate(req); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_policy", msg)
		return
	}
	spec := toAgentPolicySpec(req)
	if msg := validatePortalManagedPolicyFields(spec.Network); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_policy", msg)
		return
	}
	policy := &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: req.Name}, Spec: spec}
	created, err := h.Kube.CreateAgentPolicy(r.Context(), policy)
	if err != nil {
		if writeCompatibilityError(w, err) {
			return
		}
		if kube.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, "policy_exists", "AgentPolicy "+req.Name+" already exists")
			return
		}
		if kube.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "policy_forbidden", "Kubernetes policy or RBAC denied creating this AgentPolicy")
			return
		}
		if apierrors.IsInvalid(err) {
			slog.Error("admin: create agent policy rejected", "name", req.Name, "error", err)
			writeError(w, http.StatusUnprocessableEntity, "invalid_policy", "AgentPolicy configuration was rejected by Kubernetes")
			return
		}
		slog.Error("admin: create agent policy failed", "name", req.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create agent policy")
		return
	}
	writeJSON(w, http.StatusCreated, toAgentPolicyDTO(created))
}

// UpdatePolicy handles PUT /api/v1/admin/policies/{name}.
func (h *AdminPolicyHandler) UpdatePolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validAgentPolicyResourceName(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid policy name")
		return
	}
	var req AgentPolicyUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return
	}
	if req.Name != "" && req.Name != name {
		writeError(w, http.StatusBadRequest, "name_mismatch", "body.name does not match URL name")
		return
	}
	if msg := validateAgentPolicyRequest(req); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_policy", msg)
		return
	}
	version, msg := agentPolicyVersion(req)
	if msg != "" {
		writeError(w, http.StatusPreconditionRequired, "version_required", msg)
		return
	}
	current, err := h.Kube.GetAgentPolicy(r.Context(), name)
	if err != nil {
		if kube.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "policy_not_found", "AgentPolicy "+name+" not found")
			return
		}
		if kube.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "policy_forbidden", "Kubernetes policy or RBAC denied reading this AgentPolicy")
			return
		}
		slog.Error("admin: read agent policy for update failed", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to read AgentPolicy")
		return
	}
	if msg := validateAgentPolicyReadOnlyUpdate(req, current.Spec); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_policy", msg)
		return
	}
	desired := *current.Spec.DeepCopy()
	// Replace entitlements and identity groups; applyAgentPolicyRequest retains
	// omitted Sandbox, security, and network controls.
	requested := toAgentPolicySpec(req)
	desired.Harnesses = requested.Harnesses
	desired.Skills = requested.Skills
	desired.ModelAccess = requested.ModelAccess
	applyAgentPolicyRequest(&desired, req)
	if msg := validatePortalManagedPolicyUpdateFields(req, desired); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_policy", msg)
		return
	}
	if h.Config.SkillMarketplaceEnabled && !h.allowSkillRemoval(w, r, name, current.Spec.Skills.Allowed, desired.Skills.Allowed) {
		return
	}
	updated, err := h.Kube.UpdateAgentPolicySpec(r.Context(), name, desired, version)
	if err != nil {
		if writeCompatibilityError(w, err) {
			return
		}
		if kube.IsConfigurationCoordinationUnavailable(err) {
			writeError(w, http.StatusServiceUnavailable, "policy_change_unavailable", "Policy coordination is unavailable or the write outcome is uncertain; do not resubmit until an administrator recovers the recorded operation")
			return
		}
		if kube.IsConfigurationCoordinationConflict(err) {
			writeError(w, http.StatusConflict, "policy_change_conflict", "Another coordinated policy change or governed execution is in progress; retry after it completes")
			return
		}
		if kube.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "policy_not_found", "AgentPolicy "+name+" not found")
			return
		}
		if kube.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "policy_forbidden", "Kubernetes policy or RBAC denied updating this AgentPolicy")
			return
		}
		if errors.Is(err, kube.ErrAgentPolicyChanged) || apierrors.IsConflict(err) {
			writeError(w, http.StatusConflict, "policy_changed", kube.ErrAgentPolicyChanged.Error())
			return
		}
		if apierrors.IsInvalid(err) {
			slog.Error("admin: update agent policy rejected", "name", name, "error", err)
			writeError(w, http.StatusUnprocessableEntity, "invalid_policy", "AgentPolicy configuration was rejected by Kubernetes")
			return
		}
		slog.Error("admin: update agent policy failed", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update agent policy")
		return
	}
	writeJSON(w, http.StatusOK, toAgentPolicyDTO(updated))
}

// DeletePolicy handles DELETE /api/v1/admin/policies/{name}.
func (h *AdminPolicyHandler) DeletePolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validAgentPolicyResourceName(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid policy name")
		return
	}
	var versionRequest struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&versionRequest); err != nil || versionRequest.UID == "" || versionRequest.ResourceVersion == "" {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current AgentPolicy are required")
		return
	}
	version := kube.AgentPolicyVersion{UID: types.UID(versionRequest.UID), ResourceVersion: versionRequest.ResourceVersion}
	if err := h.Kube.DeleteAgentPolicy(r.Context(), name, version); err != nil {
		if kube.IsConfigurationCoordinationUnavailable(err) {
			writeError(w, http.StatusServiceUnavailable, "policy_change_unavailable", "Policy coordination is unavailable or the delete outcome is uncertain; do not resubmit until an administrator recovers the recorded operation")
			return
		}
		if kube.IsConfigurationCoordinationConflict(err) {
			writeError(w, http.StatusConflict, "policy_change_conflict", "Another coordinated policy change or governed execution is in progress; retry after it completes")
			return
		}
		if kube.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "policy_not_found", "AgentPolicy "+name+" not found")
			return
		}
		if kube.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "policy_forbidden", "Kubernetes policy or RBAC denied deleting this AgentPolicy; verify that no Team still references it")
			return
		}
		if errors.Is(err, kube.ErrAgentPolicyChanged) || apierrors.IsConflict(err) {
			writeError(w, http.StatusConflict, "policy_changed", kube.ErrAgentPolicyChanged.Error())
			return
		}
		slog.Error("admin: delete agent policy failed", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to delete agent policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
