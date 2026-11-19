// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// HelmMaintenanceRequest identifies one suspended release maintenance request.
// +kubebuilder:validation:XValidation:rule="self.requestID != oldSelf.requestID || self.expectedRevision == oldSelf.expectedRevision",message="a Helm maintenance request ID cannot change its expected revision",fieldPath=".expectedRevision"
type HelmMaintenanceRequest struct {
	// RequestID correlates this request with the controller's handoff status.
	// +kubebuilder:validation:MinLength=16
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	RequestID string `json:"requestID"`

	// ExpectedRevision is the positive Helm revision observed before the request.
	// +kubebuilder:validation:Minimum=1
	ExpectedRevision int64 `json:"expectedRevision"`
}

// HelmMaintenancePhase describes the controller's maintenance observation.
// +kubebuilder:validation:Enum=Requested;Held;Reapplying;Released;Blocked
type HelmMaintenancePhase string

const (
	HelmMaintenancePhaseRequested  HelmMaintenancePhase = "Requested"
	HelmMaintenancePhaseHeld       HelmMaintenancePhase = "Held"
	HelmMaintenancePhaseReapplying HelmMaintenancePhase = "Reapplying"
	HelmMaintenancePhaseReleased   HelmMaintenancePhase = "Released"
	HelmMaintenancePhaseBlocked    HelmMaintenancePhase = "Blocked"
)

// HelmMaintenanceStatus records observations, not an approval or execution log.
// The controller derives HelmMaintenanceReady only from a matching current Held
// state. Released does not itself resume the Agent or remove startup blocking.
type HelmMaintenanceStatus struct {
	// RequestID identifies the request whose progress is recorded here.
	// +kubebuilder:validation:MinLength=16
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	RequestID string `json:"requestID"`

	// ObservedGeneration is the Agent generation observed for this handoff.
	// Zero means no generation has been observed.
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration"`

	// Revision is the last observed Helm revision; zero means not yet observed.
	// +kubebuilder:validation:Minimum=0
	Revision int64 `json:"revision"`

	// Phase reports the handoff state observed by the controller.
	Phase HelmMaintenancePhase `json:"phase"`

	// Reason is a short programmatic explanation of the current phase.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message explains the current observation for an operator.
	// +kubebuilder:validation:MaxLength=32768
	// +optional
	Message string `json:"message,omitempty"`
}
