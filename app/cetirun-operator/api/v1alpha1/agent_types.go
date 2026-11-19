// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// SkillOverrideSpec defines per-instance skill customization as a delta against team defaults.
type SkillOverrideSpec struct {
	// Add lists skills to install in addition to team defaults.
	// Each entry is a ClawHub package ID (e.g. "@anthropic/mcp-server-fetch")
	// or an npm package prefixed with "npm:" (e.g. "npm:@openclaw/matrix").
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:Pattern=`^(npm:)?(@[a-z0-9][\w.-]*\/)?[a-z0-9][\w.-]*(@[\w.+-]+)?$`
	// +listType=set
	// +optional
	Add []string `json:"add,omitempty"`

	// Remove lists team-default skills to exclude from this instance.
	// Uses the same format as Add. Entries not present in the team skill
	// list are silently ignored.
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:Pattern=`^(npm:)?(@[a-z0-9][\w.-]*\/)?[a-z0-9][\w.-]*(@[\w.+-]+)?$`
	// +listType=set
	// +optional
	Remove []string `json:"remove,omitempty"`
}

// AgentSpec defines the desired state of Agent.
// +kubebuilder:validation:XValidation:rule="self.agent.type == 'custom' ? !has(self.gatewayTokenSecretRef) : has(self.gatewayTokenSecretRef)",message="gatewayTokenSecretRef is required for built-in agents and forbidden for custom agents",fieldPath=".gatewayTokenSecretRef"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.restartRequestID) || has(self.restartRequestID)",message="restartRequestID cannot be removed once set",fieldPath=".restartRequestID"
// +kubebuilder:validation:XValidation:rule="!(has(self.suspended) && self.suspended) || !has(self.restartRequestID) || (has(oldSelf.restartRequestID) && self.restartRequestID == oldSelf.restartRequestID)",message="a suspended Agent cannot request a new restart",fieldPath=".restartRequestID"
// +kubebuilder:validation:XValidation:rule="!has(self.plannedMigration) || !has(oldSelf.plannedMigration) || self.plannedMigration.requestID != oldSelf.plannedMigration.requestID || self.plannedMigration == oldSelf.plannedMigration",message="a migration request ID cannot select a different destination",fieldPath=".plannedMigration"
// +kubebuilder:validation:XValidation:rule="!has(self.helm) || !has(self.helm.maintenance) || (has(self.suspended) && self.suspended)",message="a Helm maintenance request requires suspension",fieldPath=".helm.maintenance"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.helm) || !has(oldSelf.helm.maintenance) || (has(self.helm) && has(self.helm.maintenance)) || (has(self.suspended) && self.suspended)",message="clearing Helm maintenance must retain suspension; resume is a separate update",fieldPath=".suspended"
type AgentSpec struct {
	// Agent is the required per-instance Agent configuration.
	// +required
	Agent HarnessSpec `json:"agent"`

	// User information for this instance.
	// +kubebuilder:validation:Required
	User UserSpec `json:"user"`

	// Team is the name of the Team this instance belongs to.
	// The operator merges team defaults (user context, AI model, skills) into
	// the instance configuration. Per-instance fields take precedence.
	// +optional
	Team string `json:"team,omitempty"`

	// SandboxRef opts into Sandbox execution and selects a Sandbox permitted
	// by the Team. Legacy Agents omit it and use the existing runtime path.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	SandboxRef string `json:"sandboxRef,omitempty"`

	// Model is the Agent's preferred default model as exposed by the platform
	// AI Gateway, such as a shared model or a GitHub Copilot model;
	// copilot/<model> names a Copilot model by its ID. Policy resolution rejects
	// an explicitly requested model that is outside the effective policy.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Model string `json:"model,omitempty"`

	// ModelAccess requests model sources when the platform AI Gateway is
	// enabled. Policy-bound Agents must explicitly request each source.
	// Legacy Agents retain default-on sources unless explicitly disabled.
	// +optional
	ModelAccess *ModelAccessSpec `json:"modelAccess,omitempty"`

	// SkillOverrides customizes the set of skills installed for this instance.
	// Add appends skills on top of the team defaults; Remove excludes
	// specific team skills from this instance.
	// +optional
	SkillOverrides SkillOverrideSpec `json:"skillOverrides,omitempty"`

	// AgentIdentity enables the Microsoft Entra Agent Identity integration for
	// this instance. Identity identifiers and owner claims are platform-managed
	// and are deliberately not accepted in spec.
	// +optional
	AgentIdentity *AgentIdentitySpec `json:"agentIdentity,omitempty"`

	// GatewayTokenSecretRef names the built-in Agent's multi-purpose Secret
	// in the instance namespace. Required for openclaw/hermes and forbidden for
	// custom; it is not the model-access or registry credential.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	GatewayTokenSecretRef string `json:"gatewayTokenSecretRef,omitempty"`

	// Tailscale configures the Tailscale mesh VPN sidecar for this instance.
	// When set, the operator reads the auth key from the referenced Secret
	// and passes it to the OpenClaw Helm chart. Other Tailscale settings
	// (hostname and serve) use chart defaults and can be
	// overridden via spec.helm.values if needed.
	// +optional
	Tailscale *TailscaleSpec `json:"tailscale,omitempty"`

	// Helm chart configuration overrides.
	// +optional
	Helm HelmSpec `json:"helm,omitempty"`

	// Suspended, when true, scales the instance to zero replicas and removes
	// ingress routing. The instance data (PVC) is preserved. Set this field
	// to restrict access for a user without deleting their instance.
	// Default: false.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// RestartRequestID requests a retained restart through normal reconciliation.
	// Reusing the current ID does not request another restart. The Agent and HOME
	// identities are retained; suspension and deletion take precedence.
	// +kubebuilder:validation:MinLength=16
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	// +optional
	RestartRequestID string `json:"restartRequestID,omitempty"`

	// PlannedMigration requests a healthy-source cold move of a reconciled
	// OpenClaw/Hermes Agent. It does not authorize force deletion or failover.
	// +optional
	PlannedMigration *PlannedMigrationRequest `json:"plannedMigration,omitempty"`
}

// TailscaleSpec configures the Tailscale mesh VPN sidecar.
type TailscaleSpec struct {
	// AuthKeySecretRef is the name of a Secret in the instance namespace (cetirun-<alias>)
	// containing a Tailscale auth key under the data key "TAILSCALE_AUTH_KEY".
	// Setting this enables Tailscale (no separate toggle).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`
	AuthKeySecretRef string `json:"authKeySecretRef"`
}

// TailscaleAuthKeyField is the Secret data key for the Tailscale auth key.
const TailscaleAuthKeyField = "TAILSCALE_AUTH_KEY"

// ModelAccessSpec selects the Agent's requested model sources in the AI
// Gateway. Policy resolution intersects these requests with the effective
// AgentPolicy; both sources may be disabled for an Agent with no model access.
type ModelAccessSpec struct {
	// SharedModels requests the models administrators share through the AI
	// Gateway.
	// +optional
	SharedModels *bool `json:"sharedModels,omitempty"`

	// GitHubCopilot requests a GitHub Copilot proxy for the owner's account.
	// The effective policy must also allow it.
	// +optional
	GitHubCopilot *bool `json:"githubCopilot,omitempty"`

	// MonthlyBudgetUSD optionally lowers or raises this Agent's native Gateway
	// budget within the effective AgentPolicy maximum. Omission selects the
	// policy default.
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$`
	// +optional
	MonthlyBudgetUSD string `json:"monthlyBudgetUSD,omitempty"`
}

// SharedModelsEnabled preserves legacy defaults only without a Sandbox selection.
// Policy resolution requires a Sandbox selection before evaluating these requests.
func (s AgentSpec) SharedModelsEnabled() bool {
	if s.ModelAccess != nil && s.ModelAccess.SharedModels != nil {
		return *s.ModelAccess.SharedModels
	}
	return s.SandboxRef == ""
}

// GitHubCopilotEnabled preserves legacy defaults only without a Sandbox selection.
func (s AgentSpec) GitHubCopilotEnabled() bool {
	if s.ModelAccess != nil && s.ModelAccess.GitHubCopilot != nil {
		return *s.ModelAccess.GitHubCopilot
	}
	return s.SandboxRef == ""
}

// UserSpec identifies the user this instance is provisioned for.
type UserSpec struct {
	// Alias is the user's short name, used for the namespace, hostname, and Helm release.
	// Must be a lowercase alphanumeric string optionally separated by hyphens.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,62}$`
	Alias string `json:"alias"`

	// DisplayName is the human-readable name shown in the UI.
	// +kubebuilder:validation:Required
	DisplayName string `json:"displayName"`

	// Email address for notifications (optional).
	// +optional
	Email string `json:"email,omitempty"`

	// Team is legacy display context, not a Team or policy assignment.
	// Deprecated: use spec.team for Team configuration.
	// +optional
	Team string `json:"team,omitempty"`
}

// HelmSpec allows per-instance Helm chart configuration overrides.
type HelmSpec struct {
	// Version pins the OpenClaw Helm chart version for this instance.
	// When empty on first reconcile, the operator backfills it with
	// its own --chart-version default so that future operator upgrades
	// do not automatically roll out a new chart version (which would
	// restart the StatefulSet pod and interrupt the user's session).
	// Note: the initial backfill bumps the CR generation, which triggers
	// a single Helm upgrade call. Because the backfilled version matches
	// the chart already installed, this upgrade is a no-op and does not
	// restart the StatefulSet pod.
	// To upgrade an instance, update this field to the desired version.
	// +optional
	Version string `json:"version,omitempty"`

	// Values contains arbitrary Helm values merged on top of operator defaults.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Values map[string]runtime.RawExtension `json:"values,omitempty"`

	// Maintenance requests a Helm handoff while the Agent is suspended.
	// The controller and admission verify the handoff before native writes.
	// +optional
	Maintenance *HelmMaintenanceRequest `json:"maintenance,omitempty"`
}

// AgentStatus defines the observed state of Agent.
type AgentStatus struct {
	// Conditions contains the latest available observations of the instance's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase represents the current lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;Deleting;Suspended
	// +optional
	Phase string `json:"phase,omitempty"`

	// AccessURL is the HTTPS endpoint for the instance's native agent UI.
	// Example: "https://cetirun.example.com/ui/alice/"
	// +optional
	AccessURL string `json:"accessUrl,omitempty"`

	// Namespace is the Kubernetes namespace where the instance is deployed.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// HelmReleaseName is the Helm release name for this instance.
	// +optional
	HelmReleaseName string `json:"helmReleaseName,omitempty"`

	// HelmMaintenance records the controller's observation of a Helm handoff.
	// A stored phase alone is not proof of current write or resume authority.
	// +optional
	HelmMaintenance *HelmMaintenanceStatus `json:"helmMaintenance,omitempty"`

	// ObservedGeneration is the last CR generation that was fully reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedRestartRequestID is the request observed on the current ready Pod,
	// not merely an accepted API request or applied workload template.
	// +kubebuilder:validation:MinLength=16
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	// +optional
	ObservedRestartRequestID string `json:"observedRestartRequestID,omitempty"`

	// PlannedMigration preserves the current migration's observations across
	// retries. Only the controller writes these checkpoints.
	// +optional
	PlannedMigration *PlannedMigrationStatus `json:"plannedMigration,omitempty"`

	// ObservedOperatorHash is a hash of the operator-level configuration (e.g. ChartVersion, DomainSuffix)
	// that was in effect during the last successful reconciliation. A change in this hash
	// triggers a Helm upgrade even when the CR generation has not changed.
	// +optional
	ObservedOperatorHash string `json:"observedOperatorHash,omitempty"`

	// LastReconcileTime is the timestamp of the last successful reconciliation.
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// AgentIdentity reports the platform-managed Entra identity lifecycle.
	// It never contains user tokens, authorization caches, or credentials.
	// +optional
	AgentIdentity *AgentIdentityStatus `json:"agentIdentity,omitempty"`

	// AIGateway reports the Agent's AI Gateway virtual key. It never contains
	// the key itself.
	// +optional
	AIGateway *AIGatewayStatus `json:"aiGateway,omitempty"`

	// Sandbox records the last fully enforced binding, not a pending selection.
	// +optional
	Sandbox *SandboxStatus `json:"sandbox,omitempty"`

	// RegistryCredentials records the objects verified by the last sync, not
	// proof that a node can pull the image.
	// +optional
	RegistryCredentials *RegistryCredentialsStatus `json:"registryCredentials,omitempty"`

	// Policy records the resolved AgentPolicy and the revisions each enforcement
	// component must apply. Detailed component evidence remains in its existing
	// status field.
	// +optional
	Policy *PolicyStatus `json:"policy,omitempty"`
}

type PolicyStatus struct {
	// +kubebuilder:validation:Enum=Pending;Applied;Denied;Unknown
	Phase              string                    `json:"phase,omitempty"`
	ObservedGeneration int64                     `json:"observedGeneration,omitempty"`
	EffectivePolicyRef *EffectivePolicyReference `json:"effectivePolicyRef,omitempty"`
	Enforcement        PolicyEnforcementStatus   `json:"enforcement,omitempty"`
}

type PolicyObjectReference struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

type EffectivePolicyReference struct {
	PolicyObjectReference `json:",inline"`
}

type PolicyEnforcementStatus struct {
	Sandbox    PolicyRevisionExpectation `json:"sandbox,omitempty"`
	Gateway    PolicyRevisionExpectation `json:"gateway,omitempty"`
	Credential PolicyRevisionExpectation `json:"credential,omitempty"`
}

type PolicyRevisionExpectation struct {
	DesiredRevision string `json:"desiredRevision,omitempty"`
}

type RegistryCredentialsStatus struct {
	SourceUID             string `json:"sourceUID"`
	SourceResourceVersion string `json:"sourceResourceVersion"`
	PullSecretUID         string `json:"pullSecretUID"`
}

// AgentIdentitySpec exposes only the feature switch. Tenant, Blueprint,
// owner, and credential settings are controlled by the CetiRun deployment.
type AgentIdentitySpec struct {
	// Enabled requests a dedicated Agent Identity for this instance.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// AgentIdentityStatus reports non-secret identity lifecycle information.
type AgentIdentityStatus struct {
	// Phase is the current Agent Identity lifecycle phase.
	// +kubebuilder:validation:Enum=Disabled;Provisioning;Ready;Degraded;Failed;Deleting
	// +optional
	Phase string `json:"phase,omitempty"`

	// ClientID is the public application identifier of the Agent Identity.
	// +optional
	ClientID string `json:"clientId,omitempty"`

	// ObjectID is the directory object identifier used for deterministic cleanup.
	// It is platform-managed and is not a credential.
	// +optional
	ObjectID string `json:"objectId,omitempty"`

	// DisplayName is the immutable directory display name bound to this Agent
	// Identity. It remains stable if the deployment name prefix changes.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// CreationAttempted records a durable create checkpoint, which prevents a
	// retry after an unknown request outcome from creating a second identity.
	// +optional
	CreationAttempted bool `json:"creationAttempted,omitempty"`

	// ObservedGeneration is the Agent generation reconciled into this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastVerifiedTime records the most recent canonical directory verification.
	// +optional
	LastVerifiedTime *metav1.Time `json:"lastVerifiedTime,omitempty"`

	// Reason is a stable, non-sensitive machine-readable status reason.
	// +optional
	Reason string `json:"reason,omitempty"`

	// CredentialPolicyRevision identifies a current, owned Broker enforcement
	// record read back independently of its application. It binds the policy,
	// owner and canonical Agent Identity; token issuance is not applied evidence.
	// +optional
	CredentialPolicyRevision string `json:"credentialPolicyRevision,omitempty"`
}

// AIGatewayStatus reports non-secret information about the virtual key the
// operator manages for this Agent in the platform AI Gateway.
type AIGatewayStatus struct {
	// Phase is the current virtual key lifecycle phase.
	// +kubebuilder:validation:Enum=Provisioning;Ready;Degraded;Deleting
	// +optional
	Phase string `json:"phase,omitempty"`

	// KeyAlias is the Gateway alias of the virtual key. It is derived from the
	// Agent UID and does not change for the life of the Agent.
	// +optional
	KeyAlias string `json:"keyAlias,omitempty"`

	// KeyID identifies the current key value without revealing it: the first
	// characters of the key's SHA-256 hash, which is how the Gateway stores it.
	// +optional
	KeyID string `json:"keyId,omitempty"`

	// KeySecretName names the Secret in the instance namespace that holds the
	// virtual key.
	// +optional
	KeySecretName string `json:"keySecretName,omitempty"`

	// AccessGroups are the Gateway model access groups granted to the key.
	// +listType=set
	// +optional
	AccessGroups []string `json:"accessGroups,omitempty"`

	// Team is the Gateway team the key belongs to, derived from spec.team.
	// +optional
	Team string `json:"team,omitempty"`

	// Models are the model names the Agent is configured with, as the Agent
	// sees them in the Gateway.
	// +listType=atomic
	// +optional
	Models []string `json:"models,omitempty"`

	// DefaultModel is the model the Agent uses by default.
	// +optional
	DefaultModel string `json:"defaultModel,omitempty"`

	// Copilot reports the Agent's GitHub Copilot proxy and the Copilot models
	// registered for it in the Gateway.
	// +optional
	Copilot *AIGatewayCopilotStatus `json:"copilot,omitempty"`

	// ModelRouters reports the Agent's smart-routing models: those of its
	// Team, then, with an AgentPolicy, the routers configured in the Gateway
	// that the policy lists; whether each is granted, and if not why.
	// +listType=map
	// +listMapKey=name
	// +optional
	ModelRouters []AIGatewayModelRouterStatus `json:"modelRouters,omitempty"`

	// Routed reports whether the Agent's model traffic is configured to use
	// the Gateway.
	// +optional
	Routed bool `json:"routed,omitempty"`

	// ObservedGeneration is the Agent generation reconciled into this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastVerifiedTime records the most recent successful check of the key in
	// the Gateway.
	// +optional
	LastVerifiedTime *metav1.Time `json:"lastVerifiedTime,omitempty"`

	// BudgetUSD is the canonical native Gateway budget read back for this
	// virtual key. It is a management observation, not a provider invoice.
	// +optional
	BudgetUSD string `json:"budgetUSD,omitempty"`

	// BudgetDuration is the Gateway reset period, currently "1mo".
	// +optional
	BudgetDuration string `json:"budgetDuration,omitempty"`

	// SpendUSD is the Gateway's observed spend in the current period. It is
	// not exact upstream-provider billing.
	// +optional
	SpendUSD string `json:"spendUSD,omitempty"`

	// BudgetResetAt is the next Gateway budget reset observation.
	// +optional
	BudgetResetAt *metav1.Time `json:"budgetResetAt,omitempty"`

	// Blocked reports whether the Gateway rejects this key. Retained blocked
	// keys continue to report spend and reset evidence.
	// +optional
	Blocked bool `json:"blocked,omitempty"`

	// PolicyRevision is the exact Gateway policy projection read back by
	// the management adapter.
	// +optional
	PolicyRevision string `json:"policyRevision,omitempty"`

	// PricingRevision identifies the catalog/pricing input used by Gateway
	// read-back. It is not a provider price quote.
	// +optional
	PricingRevision string `json:"pricingRevision,omitempty"`

	// Reason is a stable, non-sensitive machine-readable status reason.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// AIGatewayCopilotStatus reports the Agent's GitHub Copilot proxy.
type AIGatewayCopilotStatus struct {
	// Phase is SignInRequired until the owner signs in to GitHub Copilot
	// through the Portal, Starting while the proxy picks up the sign-in,
	// Ready once the account's models are registered, and Degraded when
	// registration fails.
	// +kubebuilder:validation:Enum=SignInRequired;Starting;Ready;Degraded
	// +optional
	Phase string `json:"phase,omitempty"`

	// AccessGroup is the Gateway access group of this Agent's Copilot models.
	// Only this Agent's key is granted it.
	// +optional
	AccessGroup string `json:"accessGroup,omitempty"`

	// Models are the registered Copilot models, as the Agent names them: the
	// name of the shared model a Copilot model is the same model as, or the
	// Copilot model's own ID.
	// +listType=atomic
	// +optional
	Models []string `json:"models,omitempty"`

	// Routes pairs each registered Copilot model with its name in the Gateway.
	// +listType=atomic
	// +optional
	Routes []AIGatewayCopilotRoute `json:"routes,omitempty"`

	// Registered are the Gateway model IDs registered for this Agent's proxy.
	// +listType=atomic
	// +optional
	Registered []string `json:"registered,omitempty"`

	// Unpriced lists the account's models left out because the Gateway has no
	// price for them: an unpriced model would be refused or escape the budget.
	// +listType=atomic
	// +optional
	Unpriced []string `json:"unpriced,omitempty"`

	// Skipped lists the account's models left out because a model the Gateway
	// serves already has their name and is another model.
	// +listType=atomic
	// +optional
	Skipped []string `json:"skipped,omitempty"`

	// NotAllowed lists the account's models left out because the Agent's
	// AgentPolicy does not allow them: each is the same model as a shared
	// model, and the policy lists none of the shared models that are that
	// model. The policy's shared models apply to the owner's Copilot models
	// too.
	// +listType=atomic
	// +optional
	NotAllowed []string `json:"notAllowed,omitempty"`

	// ProxyKeyID identifies the proxy's master key without revealing it: the
	// first characters of the key's SHA-256 hash. The proxy reads the key
	// when it starts, so a new key restarts it.
	// +optional
	ProxyKeyID string `json:"proxyKeyId,omitempty"`

	// Reason is a stable, non-sensitive machine-readable status reason.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// AIGatewayCopilotRoute is one of the Agent's registered Copilot models.
type AIGatewayCopilotRoute struct {
	// Model is the Copilot model's ID.
	Model string `json:"model"`

	// Name is the model's name in the Gateway, which the Agent uses.
	Name string `json:"name"`

	// Shared reports that Name is a shared model's name: the Gateway serves
	// the Agent's requests for it from the shared model's deployments and
	// from this Copilot model.
	// +optional
	Shared bool `json:"shared,omitempty"`

	// Vision reports that the model accepts images, as the Gateway's price
	// table says for the entry the route is billed as. The Agent is
	// configured to send images to Name only when every model behind Name
	// accepts them.
	// +optional
	Vision bool `json:"vision,omitempty"`
}

// GitHub Copilot proxy phases.
const (
	AIGatewayCopilotPhaseSignInRequired = "SignInRequired"
	AIGatewayCopilotPhaseStarting       = "Starting"
	AIGatewayCopilotPhaseReady          = "Ready"
	AIGatewayCopilotPhaseDegraded       = "Degraded"
)

// Agent annotations and Secrets the Portal and the operator share for the
// GitHub Copilot sign-in.
const (
	// AnnotationCopilotModels holds the JSON list of models the owner's
	// Copilot account offers, written by the Portal after sign-in.
	AnnotationCopilotModels = "cetirun.microsoft.com/copilot-models"
	// AnnotationCopilotSignedInAt records when the Portal last stored a
	// GitHub Copilot sign-in, so every sign-in reaches the operator even when
	// the model list is unchanged.
	AnnotationCopilotSignedInAt = "cetirun.microsoft.com/copilot-signed-in-at"
	// AnnotationCopilotSignInGeneration identifies a GitHub Copilot sign-in.
	// The Portal sets it on the token Secret in the write that stores the
	// token, and on the Agent in the write that records the models, so the
	// operator can tell models recorded for another sign-in than the token's.
	AnnotationCopilotSignInGeneration = "cetirun.microsoft.com/copilot-sign-in-generation"
	// AnnotationCopilotSignInCompleted on the token Secret names the
	// generation of the stored token's sign-in once the Portal has recorded
	// its models too and completed it. The Portal writes it under the
	// Secret's version, and only while the Secret holds that sign-in's token
	// and its fence still admits the sign-in, so a sign-in that a cancel, a
	// newer start or the end of an expired completion fenced first never
	// gets it. The operator uses a sign-in's models only once it is
	// completed; a token stored before generations has neither, and is used
	// as before.
	AnnotationCopilotSignInCompleted = "cetirun.microsoft.com/copilot-sign-in-completed"
	// AnnotationCopilotTokenStoredAt on the token Secret records when the
	// Portal stored the token.
	AnnotationCopilotTokenStoredAt = "cetirun.microsoft.com/copilot-token-stored-at"
	// CopilotTokenSecretName is the instance-namespace Secret holding the
	// owner's GitHub token for the Copilot proxy. The operator creates it
	// empty; the Portal writes the token.
	CopilotTokenSecretName = "github-copilot-token"
	// CopilotTokenSecretKey is the data key of the token.
	CopilotTokenSecretKey = "access-token"
)

// Model names the AI Gateway gives an Agent.
const (
	// AIGatewayCopilotModelPrefix is reserved in the Gateway for an Agent's
	// GitHub Copilot models: copilot/<model ID>, such as copilot/gpt-5.5,
	// names a Copilot model by its ID. Agents use the model's Gateway name,
	// a shared model's name or the ID itself; the Gateway keeps copilot/<ID>
	// as a key alias of that name for earlier clients.
	AIGatewayCopilotModelPrefix = "copilot/"
	// AIGatewaySignInRequiredModel and AIGatewayNoModelsModel are the
	// placeholder default models of an Agent without any model yet: its
	// configuration needs a default, and the Portal explains the state.
	AIGatewaySignInRequiredModel = AIGatewayCopilotModelPrefix + "sign-in-required"
	AIGatewayNoModelsModel       = "no-models-available"
)

// CopilotModel is one entry of the AnnotationCopilotModels list.
type CopilotModel struct {
	// ID is the Copilot model ID, such as gpt-5.5.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name,omitempty"`
	// ContextWindow is the model's context window in tokens.
	ContextWindow int64 `json:"contextWindow,omitempty"`
	// MaxOutputTokens is the model's output limit in tokens.
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	// ResponsesOnly marks a model GitHub Copilot serves only through the
	// Responses API, not chat completions. The Copilot proxy bridges chat
	// requests for it.
	ResponsesOnly bool `json:"responsesOnly,omitempty"`
}

// The catalog the operator publishes for the Portal: a ConfigMap in the
// operator namespace listing the model sources new Agents can use, with the
// limits of the shared models. It holds no credentials or addresses.
const (
	// AIGatewayCatalogConfigMap is the name of the catalog ConfigMap.
	AIGatewayCatalogConfigMap = "cetirun-ai-gateway-catalog"
	// AIGatewayCatalogKey is the data key holding the catalog as JSON.
	AIGatewayCatalogKey = "catalog.json"
)

// AIGatewayCatalog is the content of the catalog ConfigMap.
type AIGatewayCatalog struct {
	// SharedModels are the models every Agent with shared models can use.
	// While the Gateway blocks requests for unpriced models, they leave out
	// the models it would refuse.
	SharedModels []AIGatewayCatalogModel `json:"sharedModels"`
	// DecisionModels are the shared decision models, such as
	// Microsoft-Decision-1, that a Team router's jev classifier may use.
	// Agents are not configured with them.
	DecisionModels []string `json:"decisionModels,omitempty"`
	// GitHubCopilot reports whether the deployment gives Agents a GitHub
	// Copilot proxy. Without it, an Agent's GitHub Copilot switch adds no
	// models.
	GitHubCopilot bool `json:"githubCopilot"`
	// Routing reports what the operator's default agent chart routes
	// through the Gateway: the agent types new Agents can take the Gateway's
	// models with. A catalog from an earlier operator has none; it routes
	// only OpenClaw.
	Routing *AIGatewayRouting `json:"routing,omitempty"`
}

// AIGatewayRouting is what a default agent chart routes through the Gateway.
type AIGatewayRouting struct {
	// Chart identifies the chart the agent types were read from: its
	// repository and version.
	Chart string `json:"chart"`
	// AgentTypes are the agent types the chart routes through the Gateway.
	// They are null while the operator has not read the chart.
	// +kubebuilder:validation:items:Enum=openclaw;hermes;custom
	AgentTypes []AgentType `json:"agentTypes"`
}

// AIGatewayCatalogModel is one shared model with the limits the Gateway
// knows for it.
type AIGatewayCatalogModel struct {
	// Name is the model name as Agents use it.
	Name string `json:"name"`
	// ContextWindow is the model's context window in tokens.
	ContextWindow int64 `json:"contextWindow,omitempty"`
	// MaxOutputTokens is the model's output limit in tokens.
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	// RouterModels are, for a shared model that is an auto router, the models
	// it sends requests to answer: its tiers, its default model and its
	// compaction model. An Agent with an AgentPolicy may use the router only
	// when the policy lists each of them. Empty for other models.
	RouterModels []string `json:"routerModels,omitempty"`
	// RouterUnverifiable reports a router whose models the operator cannot
	// read completely; it is refused to Agents with an AgentPolicy.
	RouterUnverifiable bool `json:"routerUnverifiable,omitempty"`
	// Vision reports that the model accepts images: every deployment behind
	// the name has supports_vision in its model info, and for a router, every
	// model it sends requests to answer does.
	Vision bool `json:"vision,omitempty"`
}

// SandboxStatus records the binding and revision last verified for an Agent.
type SandboxStatus struct {
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	TeamUID types.UID `json:"teamUID"`

	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	SandboxUID types.UID `json:"sandboxUID"`

	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	AgentPolicyUID types.UID `json:"agentPolicyUID"`

	// +kubebuilder:validation:MinLength=1
	AppliedRevision string `json:"appliedRevision"`
}

// Condition type constants.
const (
	ConditionReady                    = "Ready"
	ConditionHelmDeployed             = "HelmDeployed"
	ConditionHelmMaintenanceReady     = "HelmMaintenanceReady"
	ConditionIngressReady             = "IngressReady"
	ConditionPodReady                 = "PodReady"
	ConditionTailscaleReady           = "TailscaleReady"
	ConditionAgentConfigReady         = "AgentConfigReady"
	ConditionAgentIdentityReady       = "AgentIdentityReady"
	ConditionAIGatewayReady           = "AIGatewayReady"
	ConditionSandboxReady             = "SandboxReady"
	ConditionDesktopConfigured        = "DesktopConfigured"
	ConditionTerminalConfigured       = "TerminalConfigured"
	ConditionRegistryCredentialsReady = "RegistryCredentialsReady"
	ConditionPolicyResolved           = "PolicyResolved"
	ConditionPolicyApplied            = "PolicyApplied"
)

// AI Gateway virtual key lifecycle phases.
const (
	AIGatewayPhaseProvisioning = "Provisioning"
	AIGatewayPhaseReady        = "Ready"
	AIGatewayPhaseDegraded     = "Degraded"
	AIGatewayPhaseDeleting     = "Deleting"
)

// Agent Identity lifecycle phases.
const (
	AgentIdentityPhaseDisabled     = "Disabled"
	AgentIdentityPhaseProvisioning = "Provisioning"
	AgentIdentityPhaseReady        = "Ready"
	AgentIdentityPhaseDegraded     = "Degraded"
	AgentIdentityPhaseFailed       = "Failed"
	AgentIdentityPhaseDeleting     = "Deleting"
)

// Phase constants.
const (
	PhasePending      = "Pending"
	PhaseProvisioning = "Provisioning"
	PhaseReady        = "Ready"
	PhaseFailed       = "Failed"
	PhaseDeleting     = "Deleting"
	PhaseSuspended    = "Suspended"
)

// Finalizer name used by the operator.
const Finalizer = "cetirun.microsoft.com/finalizer"

// Owner binding annotations are written only by trusted platform code from
// verified Entra ID-token claims.
const (
	AnnotationOwnerTenantID = "agentidentity.cetirun.microsoft.com/owner-tenant-id"
	AnnotationOwnerObjectID = "agentidentity.cetirun.microsoft.com/owner-object-id"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=agents,scope=Cluster,shortName=cra,singular=agent
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.plannedMigration) || has(self.spec.plannedMigration)",message="a recorded migration destination cannot be removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.plannedMigration) || oldSelf.status.plannedMigration.phase == 'Completed' || (has(self.spec.plannedMigration) && has(oldSelf.spec.plannedMigration) && self.spec.plannedMigration == oldSelf.spec.plannedMigration)",message="an accepted migration must finish before requesting another destination"
// +kubebuilder:printcolumn:name="Alias",type=string,JSONPath=`.spec.user.alias`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="AccessURL",type=string,JSONPath=`.status.accessUrl`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent is a cluster-scoped, long-lived agent environment.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   AgentSpec   `json:"spec"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
