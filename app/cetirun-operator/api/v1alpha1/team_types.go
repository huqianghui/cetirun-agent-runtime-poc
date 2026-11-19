// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TeamSpec defines team-wide configuration applied to every member instance.
type TeamSpec struct {
	// AgentPolicyRef names the policy applied to every Agent in this Team.
	// A Team without any policy or Sandbox fields retains legacy execution.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	AgentPolicyRef string `json:"agentPolicyRef,omitempty"`

	// SandboxRefs lists the Sandboxes permitted for this Team.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=set
	// +optional
	SandboxRefs []string `json:"sandboxRefs,omitempty"`

	// DefaultSandboxRef selects a listed Sandbox for newly created Agents.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	DefaultSandboxRef string `json:"defaultSandboxRef,omitempty"`

	// MemberContext defines the team context injected into USER.md for every member instance.
	// +optional
	MemberContext TeamContextSpec `json:"memberContext,omitempty"`

	// Skills is the list of default skills for all team members.
	// Each entry is a Skill Marketplace skill name (when the marketplace is
	// enabled), or an npm package prefixed with "npm:" (e.g. "npm:@openclaw/matrix").
	// Without the marketplace, other entries are passed to the reserved
	// ClawHub list and are not installed.
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:Pattern=`^(npm:)?(@[a-z0-9][\w.-]*\/)?[a-z0-9][\w.-]*(@[\w.+-]+)?$`
	// +listType=set
	// +optional
	Skills []string `json:"skills,omitempty"`

	// SkillStorage describes a shared Azure Blob NFS container that the operator
	// mounts read-only into every member instance's pod. When set, the operator
	// populates the chart's skills.shared mode with the given storage account
	// coordinates so that a PV+PVC are auto-provisioned per instance namespace.
	// +optional
	SkillStorage *SkillStorageSpec `json:"skillStorage,omitempty"`

	// ModelRouters are the Team's smart-routing models, which administrators
	// define. Each is registered in the AI Gateway under an internal name and
	// granted to the Agents everything it uses is available to.
	// +kubebuilder:validation:MaxItems=10
	// +listType=map
	// +listMapKey=name
	// +optional
	ModelRouters []TeamModelRouter `json:"modelRouters,omitempty"`
}

// PolicyConfigured reports policy intent, including incomplete configurations.
func (s TeamSpec) PolicyConfigured() bool {
	return s.AgentPolicyRef != "" || len(s.SandboxRefs) != 0 || s.DefaultSandboxRef != ""
}

// SkillStorageSpec describes a shared Azure Blob NFS container holding
// team-wide skills. Mounted read-only at MountPath in every member's pod.
type SkillStorageSpec struct {
	// ResourceGroup of the Azure storage account.
	ResourceGroup string `json:"resourceGroup"`

	// StorageAccount name (globally unique, lowercase alphanumeric, 3-24 chars).
	StorageAccount string `json:"storageAccount"`

	// ContainerName of the blob container holding the skills tree.
	ContainerName string `json:"containerName"`

	// StorageClassName for the auto-provisioned PV/PVC. Defaults to the
	// chart default ("azureblob-nfs-premium") when empty.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// StorageSize requested for the PVC. Defaults to the chart default
	// ("100Gi") when empty.
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// MountPath selects the read-only shared Skills mount for native Agents.
	// Custom Agents compose shared Skills into "$HOME/.agents/skills".
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// TeamContextSpec defines the team-level context injected into every member's environment.
type TeamContextSpec struct {
	// Role is the team member's role, e.g. "Engineer".
	// +optional
	Role string `json:"role,omitempty"`

	// Description contains lines injected into the ## Context section of USER.md.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Description []string `json:"description,omitempty"`
}

// TeamStatus defines the observed state of Team.
type TeamStatus struct {
	// Conditions contains the latest available observations of the team's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last CR generation that was fully reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ModelRouters reports the AI Gateway registration of each router in
	// spec.modelRouters.
	// +listType=map
	// +listMapKey=name
	// +optional
	ModelRouters []TeamModelRouterStatus `json:"modelRouters,omitempty"`

	// Compatibility is the latest configuration-only assessment of this Team's
	// Policy and selected Sandboxes. It does not authorize an Agent or prove
	// that a workload, Gateway key, or credential projection is applied.
	// +optional
	Compatibility *TeamCompatibilityStatus `json:"compatibility,omitempty"`
}

const ConditionTeamAccepted = "Accepted"

// CompatibilityState is the configuration compatibility of a Team and its
// selected Sandboxes. It deliberately does not describe runtime enforcement.
// +kubebuilder:validation:Enum=Compatible;Incompatible;Unknown
type CompatibilityState string

const (
	CompatibilityCompatible   CompatibilityState = "Compatible"
	CompatibilityIncompatible CompatibilityState = "Incompatible"
	CompatibilityUnknown      CompatibilityState = "Unknown"
)

// CompatibilityReference pins a configuration object that an assessment read.
// A Portal must not show a compatible result after any of these values changes.
type CompatibilityReference struct {
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

// CompatibilityIssue is safe for administrative display and for Kubernetes
// admission details. It contains no platform topology or credential data.
type CompatibilityIssue struct {
	Code    string                  `json:"code"`
	Field   string                  `json:"field,omitempty"`
	Message string                  `json:"message"`
	Team    *CompatibilityReference `json:"team,omitempty"`
	Policy  *CompatibilityReference `json:"policy,omitempty"`
	Sandbox *CompatibilityReference `json:"sandbox,omitempty"`
}

// CompatibilityIssueMessage returns the fixed, safe message for an issue code.
// Portal clients must use this contract instead of rendering admission text.
func CompatibilityIssueMessage(code string) (string, bool) {
	message, ok := compatibilityIssueMessages[code]
	return message, ok
}

// Valid reports whether an issue uses the public compatibility contract.
func (i CompatibilityIssue) Valid() bool {
	message, ok := CompatibilityIssueMessage(i.Code)
	return ok && i.Message == message
}

var compatibilityIssueMessages = map[string]string{
	"policy_missing":                        "Choose an Agent Policy.",
	"sandbox_missing":                       "Choose at least one Sandbox.",
	"default_sandbox_invalid":               "Choose a default Sandbox from the selected Sandboxes.",
	"policy_not_found":                      "The selected Agent Policy was not found.",
	"policy_invalid":                        "The selected Agent Policy is invalid.",
	"multiple_sandboxes_not_allowed":        "The selected Agent Policy allows one Sandbox per Team.",
	"sandbox_not_found":                     "The selected Sandbox was not found.",
	"sandbox_invalid":                       "The selected Sandbox is invalid.",
	"runtime_not_registered":                "The Sandbox runtime is not registered by the platform.",
	"execution_profile_unsupported":         "The Sandbox execution profile is not enabled by this platform.",
	"home_access_mode_unsupported":          "The Sandbox HOME access mode is not supported by this platform.",
	"network_address_family_unsupported":    "The Policy requests a network address family that the Sandbox does not support.",
	"network_protocol_unsupported":          "The Policy requests a network protocol that the Sandbox does not support.",
	"read_only_root_filesystem_unsupported": "The Sandbox does not support the Policy read-only root filesystem requirement.",
	"ingress_unsupported":                   "The Sandbox does not support the Policy ingress configuration.",
	"network_control_unsupported":           "The Sandbox does not support one or more requested Policy network controls.",
	"kubernetes_access_unsupported":         "The Sandbox does not support the Policy Kubernetes access requirement.",
	"entra_id_unsupported":                  "The Sandbox does not support the Policy Entra ID requirement.",
	"sandbox_incompatible":                  "The selected Sandbox does not support one or more requested Policy controls.",
}

// SandboxCompatibilityStatus reports one selected Sandbox within a Team.
type SandboxCompatibilityStatus struct {
	Sandbox CompatibilityReference `json:"sandbox"`
	State   CompatibilityState     `json:"state"`
	Reason  string                 `json:"reason,omitempty"`
	Issues  []CompatibilityIssue   `json:"issues,omitempty"`
}

// TeamCompatibilityStatus records a point-in-time compatibility assessment.
// ValidUntil bounds the report so stale compatibility is never interpreted as
// current after its dependencies or platform inventory change.
type TeamCompatibilityStatus struct {
	State      CompatibilityState           `json:"state"`
	Reason     string                       `json:"reason,omitempty"`
	Message    string                       `json:"message,omitempty"`
	Team       CompatibilityReference       `json:"team"`
	Policy     CompatibilityReference       `json:"policy"`
	Sandboxes  []SandboxCompatibilityStatus `json:"sandboxes,omitempty"`
	Issues     []CompatibilityIssue         `json:"issues,omitempty"`
	CheckedAt  metav1.Time                  `json:"checkedAt"`
	ValidUntil metav1.Time                  `json:"validUntil"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=teams,scope=Cluster,shortName=crt,singular=team
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.memberContext.role`
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.agentPolicyRef`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Team is a cluster-scoped resource that defines team-wide configuration
// (user context, policy, Sandbox selection, and skills) applied to every Agent that
// references this team via spec.team.
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TeamSpec   `json:"spec,omitempty"`
	Status TeamStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TeamList contains a list of Team.
type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Team{}, &TeamList{})
}
