// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// AgentType identifies a built-in integration or a user-supplied image.
type AgentType string

const (
	AgentTypeOpenClaw AgentType = "openclaw"
	AgentTypeHermes   AgentType = "hermes"
	AgentTypeCustom   AgentType = "custom"

	DefaultAgentType     = AgentTypeHermes
	DefaultAgentUsername = "vibe"
)

// HarnessSpec is the per-Agent image and runtime contract.
// Model access, ownership, skills and storage are configured separately.
// +kubebuilder:validation:XValidation:rule="self.type != 'custom' || has(self.image)",message="custom agents require image",fieldPath=".image"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="agent type is immutable",fieldPath=".type"
// +kubebuilder:validation:XValidation:rule="self.username == oldSelf.username",message="agent username is immutable",fieldPath=".username"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.image) || has(self.image)",message="a pinned image cannot be removed",fieldPath=".image"
// +kubebuilder:validation:XValidation:rule="!self.webUI || self.type == 'custom'",message="webUI is supported only for custom agents",fieldPath=".webUI"
// +kubebuilder:validation:XValidation:rule="!self.webUI || has(self.port)",message="webUI requires port",fieldPath=".port"
type HarnessSpec struct {
	// Type selects the integration. Defaults to Hermes and cannot change.
	// +kubebuilder:validation:Enum=openclaw;hermes;custom
	// +kubebuilder:default=hermes
	// +optional
	Type AgentType `json:"type,omitempty"`

	// Image is an OCI image reference. Required for custom agents; built-in
	// defaults are resolved and pinned by the runtime, not by the API schema.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	Image *string `json:"image,omitempty"`

	// RegistrySecretRef names a Portal-managed source Secret in the platform
	// credential namespace, not the Pod's imagePullSecret. Runtime code must
	// verify Agent UID ownership before creating a local pull Secret.
	// +optional
	RegistrySecretRef *RegistrySecretRef `json:"registrySecretRef,omitempty"`

	// Username identifies an existing non-root image user with HOME at
	// /home/<username>. The image must declare the corresponding numeric USER.
	// Kubernetes does not create or switch users by this name.
	// +kubebuilder:default=vibe
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_-]{0,31}$`
	// +kubebuilder:validation:XValidation:rule="self != 'root'",message="agent username must not be root"
	// +optional
	Username *string `json:"username,omitempty"`

	// Port is the native HTTP port for built-ins. For Custom it serves the
	// Chat HTTP contract unless WebUI is true. Omit it for a headless Custom.
	// Built-ins resolve their version-specific default outside the schema.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// WebUI declares that Port serves a browser application instead of Chat.
	// Custom-only; the platform publishes it at /ui/<instance>/ with authentication.
	// +kubebuilder:default=false
	// +optional
	WebUI bool `json:"webUI,omitempty"`
}

// RegistrySecretRef references one Agent's source registry credential.
// It deliberately has no namespace, key or inline credential fields.
type RegistrySecretRef struct {
	// Name is a Secret name in the platform-configured credential namespace.
	// A syntactically valid reference does not grant access to that Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +required
	Name string `json:"name"`
}
