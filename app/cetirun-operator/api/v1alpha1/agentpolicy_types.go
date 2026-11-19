// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentPolicySpec defines controls independently of runtime selection.
type AgentPolicySpec struct {
	// Harnesses limits runtimes that can receive this policy.
	Harnesses AgentPolicyHarnesses `json:"harnesses"`

	// Skills is the allowlist for Team defaults and per-Agent skill requests.
	// +optional
	Skills AgentPolicySkills `json:"skills,omitempty"`

	// ModelAccess defines the Gateway entitlements and one native USD budget
	// for the Agent virtual key.
	// +optional
	ModelAccess AgentPolicyModelAccess `json:"modelAccess,omitempty"`

	// AgentIdentity permits the fixed v1 credential-service entitlement.
	// +optional
	AgentIdentity AgentPolicyAgentIdentity `json:"agentIdentity,omitempty"`

	// EntraID requests explicitly approved workload identity projection into
	// the main container. It is independent of AgentIdentity, which controls
	// the directory identity and Broker credential-service entitlement.
	// +kubebuilder:default={}
	// +optional
	EntraID AgentPolicyEntraID `json:"entraID,omitempty"`
	// +kubebuilder:default={}
	// +optional
	SandboxSelection SandboxSelectionPolicy `json:"sandboxSelection,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Security AgentPolicySecurity `json:"security,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Network AgentPolicyNetwork `json:"network,omitempty"`

	// KubernetesAccess requires separate authorization; its presence grants nothing.
	// +optional
	KubernetesAccess *AgentPolicyAdminKubernetesAccess `json:"kubernetesAccess,omitempty"`
}

type AgentPolicyHarnesses struct {
	// +kubebuilder:validation:MinItems=1
	// Custom execution additionally requires an approved Gateway-qualified image.
	// +kubebuilder:validation:items:Enum=openclaw;hermes;custom
	// +listType=set
	Allowed []AgentType `json:"allowed"`
}

type AgentPolicySkills struct {
	// +listType=set
	// +optional
	Allowed []string `json:"allowed,omitempty"`
}

type AgentPolicyModelAccess struct {
	// +optional
	Copilot AgentPolicyCopilotAccess `json:"copilot,omitempty"`
	// +optional
	SharedModels AgentPolicySharedModelsAccess `json:"sharedModels,omitempty"`
	// +optional
	GatewayBudget *AgentPolicyGatewayBudget `json:"gatewayBudget,omitempty"`
	// RateLimits applies the same Gateway request limits to the virtual key
	// of every Agent that uses this policy. Each key keeps its own counters,
	// so Agents do not share a limit. An omitted field is unlimited.
	// +optional
	RateLimits *AgentPolicyRateLimits `json:"rateLimits,omitempty"`
}

// AgentPolicyRateLimits are per-key AI Gateway limits. The Gateway rejects
// every request of a key with a zero limit, so a limit is either a positive
// value or omitted; there is no zero or "unlimited" value.
type AgentPolicyRateLimits struct {
	// MaxParallelRequests limits the key's requests in flight at once. The
	// Gateway stores it as a 32-bit integer, unlike its RPM and TPM.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	// +optional
	MaxParallelRequests *int64 `json:"maxParallelRequests,omitempty"`
	// RPM limits the key's requests per minute across all of its models.
	// +kubebuilder:validation:Minimum=1
	// +optional
	RPM *int64 `json:"rpm,omitempty"`
	// TPM limits the key's tokens per minute across all of its models. The
	// Gateway counts the tokens of finished requests and refuses requests
	// once the minute's count has reached the limit.
	// +kubebuilder:validation:Minimum=4096
	// +optional
	TPM *int64 `json:"tpm,omitempty"`
	// PerModel limits requests for individual models. A key is the exact
	// Gateway model name the Agent requests, not a display label. For a
	// router of the Agent's Team, the router's name is also accepted: the
	// operator also sets the limit under the router's internal name, which
	// the Gateway counts for the router. The limit under the router's name
	// stays. It applies to another model that keeps the name, for example a
	// GitHub Copilot model of the Agent.
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(model, size(model) >= 1 && size(model) <= 256 && !model.matches('[[:space:]]'))",message="perModel keys must be Gateway model names of 1 to 256 characters without whitespace"
	// +optional
	PerModel map[string]AgentPolicyModelRateLimit `json:"perModel,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.rpm) || has(self.tpm)",message="a per-model limit must set rpm, tpm, or both"
type AgentPolicyModelRateLimit struct {
	// RPM limits the key's requests per minute for this model.
	// +kubebuilder:validation:Minimum=1
	// +optional
	RPM *int64 `json:"rpm,omitempty"`
	// TPM limits the key's tokens per minute for this model.
	// +kubebuilder:validation:Minimum=4096
	// +optional
	TPM *int64 `json:"tpm,omitempty"`
}

type AgentPolicyCopilotAccess struct {
	// +optional
	Allowed bool `json:"allowed,omitempty"`
}

type AgentPolicySharedModelsAccess struct {
	// Model identifiers are exact Gateway catalog names, not display labels.
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[^\t\r\n ]+$`
	// +listType=set
	// +optional
	Allowed []string `json:"allowed,omitempty"`
}

type AgentPolicyGatewayBudget struct {
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$`
	DefaultMonthlyUSD string `json:"defaultMonthlyUSD"`
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$`
	MaxMonthlyUSD string `json:"maxMonthlyUSD"`
}

type AgentPolicyAgentIdentity struct {
	// +optional
	Allowed bool `json:"allowed,omitempty"`
	// +optional
	Required bool `json:"required,omitempty"`
	// +kubebuilder:validation:items:Enum=azure-resource-manager
	// +listType=set
	// +optional
	CredentialServices []string `json:"credentialServices,omitempty"`
}

type AgentPolicyEntraID struct {
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

type SandboxSelectionPolicy struct {
	// +kubebuilder:default=false
	// +optional
	AllowMultiple bool `json:"allowMultiple,omitempty"`
}

type AgentPolicySecurity struct {
	// +kubebuilder:default=true
	// +optional
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
}

type AgentPolicyNetwork struct {
	// +kubebuilder:default={}
	// +listType=atomic
	// +optional
	BlockedCIDRs []string `json:"blockedCIDRs,omitempty"`

	// BlockedFQDNs contains exact lowercase ASCII hostnames, not URLs or IPs.
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:default={}
	// +listType=atomic
	// +optional
	BlockedFQDNs []string `json:"blockedFQDNs,omitempty"`

	// +kubebuilder:default={}
	// +listType=atomic
	// +optional
	BlockedPorts []AgentPolicyPort `json:"blockedPorts,omitempty"`

	// +kubebuilder:default={}
	// +listType=atomic
	// +optional
	AllowedInternalCIDRs []AgentPolicyCIDRRule `json:"allowedInternalCIDRs,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Ingress AgentPolicyIngress `json:"ingress,omitempty"`
}

type AgentPolicyCIDRRule struct {
	// +kubebuilder:validation:MinLength=1
	CIDR string `json:"cidr"`

	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Ports []AgentPolicyPort `json:"ports"`
}

type AgentPolicyPort struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// +kubebuilder:validation:Enum=authenticated;disabled
type AgentPolicyIngressMode string

const (
	AgentPolicyIngressAuthenticated AgentPolicyIngressMode = "authenticated"
	AgentPolicyIngressDisabled      AgentPolicyIngressMode = "disabled"
)

// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'disabled' || !has(self.requireMTLS) || !self.requireMTLS",message="disabled ingress cannot require mTLS"
type AgentPolicyIngress struct {
	// +kubebuilder:default=authenticated
	// +optional
	Mode AgentPolicyIngressMode `json:"mode,omitempty"`

	// +kubebuilder:default=false
	// +optional
	RequireMTLS bool `json:"requireMTLS,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.roleBindings) && size(self.roleBindings) > 0) || (has(self.clusterRoleBindings) && size(self.clusterRoleBindings) > 0)",message="at least one explicit RBAC grant is required"
type AgentPolicyAdminKubernetesAccess struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ServiceAccountName string `json:"serviceAccountName"`

	// +listType=atomic
	// +optional
	RoleBindings []AgentPolicyRoleBinding `json:"roleBindings,omitempty"`

	// +listType=atomic
	// +optional
	ClusterRoleBindings []AgentPolicyClusterRoleBinding `json:"clusterRoleBindings,omitempty"`
}

type AgentPolicyRoleBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// ClusterRoleName is an explicit Kubernetes path-segment name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[^/%]+$`
	// +kubebuilder:validation:XValidation:rule="self != '.' && self != '..'",message="a ClusterRole name cannot be '.' or '..'"
	ClusterRoleName string `json:"clusterRoleName"`
}

type AgentPolicyClusterRoleBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[^/%]+$`
	// +kubebuilder:validation:XValidation:rule="self != '.' && self != '..'",message="a ClusterRole name cannot be '.' or '..'"
	ClusterRoleName string `json:"clusterRoleName"`
}

const ConditionAgentPolicyAccepted = "Accepted"

// AgentPolicyStatus reports configuration acceptance, not Agent enforcement.
type AgentPolicyStatus struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=agentpolicies,scope=Cluster,shortName=crp,singular=agentpolicy
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type AgentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentPolicySpec `json:"spec"`

	// +optional
	Status AgentPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AgentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentPolicy{}, &AgentPolicyList{})
}
