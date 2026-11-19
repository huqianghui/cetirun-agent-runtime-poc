// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	MigrationPodFinalizer          = "cetirun.microsoft.com/planned-migration-pod"
	MigrationAttachmentFinalizer   = "cetirun.microsoft.com/planned-migration-attachment"
	AnnotationMigrationRequest     = "cetirun.microsoft.com/migration-request-id"
	ConditionPlannedMigrationReady = "PlannedMigrationReady"
	MigrationProtecting            = "Protecting"
	MigrationStopping              = "Stopping"
	MigrationSourceTerminated      = "SourceTerminated"
	MigrationDetached              = "Detached"
	MigrationStarting              = "Starting"
	MigrationCompleted             = "Completed"
)

type MigrationObject struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
}

type PlannedMigrationRequest struct {
	// Reusing the current ID only observes that request.
	// +kubebuilder:validation:MinLength=16
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	RequestID string          `json:"requestID"`
	Target    MigrationObject `json:"target"`
}

type MigrationNode struct {
	MigrationObject `json:",inline"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	ProviderID string `json:"providerID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	BootID  string          `json:"bootID"`
	CSINode MigrationObject `json:"csiNode"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	CSINodeID string `json:"csiNodeID"`
}

type PlannedMigrationStatus struct {
	PlannedMigrationRequest `json:",inline"`
	// +kubebuilder:validation:Enum=Protecting;Stopping;SourceTerminated;Detached;Starting;Completed
	Phase     string          `json:"phase"`
	Namespace MigrationObject `json:"namespace"`
	Workload  MigrationObject `json:"workload"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	WorkloadTemplateHash string          `json:"workloadTemplateHash"`
	SourcePod            MigrationObject `json:"sourcePod"`
	Source               MigrationNode   `json:"source"`
	Destination          MigrationNode   `json:"destination"`
	Home                 MigrationObject `json:"home"`
	Volume               MigrationObject `json:"volume"`
	Attachment           MigrationObject `json:"attachment"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Driver string `json:"driver"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	VolumeHandle string      `json:"volumeHandle"`
	StartedAt    metav1.Time `json:"startedAt"`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	PreviousRequestID string `json:"previousRequestID,omitempty"`
	// +optional
	ReplacementPod *MigrationObject `json:"replacementPod,omitempty"`
	// +optional
	ReplacementAttachment *MigrationObject `json:"replacementAttachment,omitempty"`
}

func (s *PlannedMigrationStatus) SourceTerminated() bool {
	return s != nil && (s.Phase == MigrationSourceTerminated || s.Phase == MigrationDetached ||
		s.Phase == MigrationStarting || s.Phase == MigrationCompleted)
}

func (s *PlannedMigrationStatus) CanStart() bool {
	return s != nil && (s.Phase == MigrationStarting || s.Phase == MigrationCompleted)
}

func (a *Agent) MigrationNeedsCapture() bool {
	request, status := a.Spec.PlannedMigration, a.Status.PlannedMigration
	return request != nil && (status == nil ||
		(status.Phase == MigrationCompleted && status.PlannedMigrationRequest != *request))
}

func (a *Agent) MigrationPending() bool {
	request, status := a.Spec.PlannedMigration, a.Status.PlannedMigration
	if request == nil {
		return status != nil
	}
	return status == nil || status.PlannedMigrationRequest != *request || status.Phase != MigrationCompleted
}
