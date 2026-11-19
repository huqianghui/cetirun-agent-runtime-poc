// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxType selects a prepared runtime.
// +kubebuilder:validation:Enum=kata;gvisor;container
type SandboxType string

const (
	SandboxKata   SandboxType = "kata"
	SandboxGVisor SandboxType = "gvisor"
	// SandboxContainer uses ordinary containers sharing the node kernel, not VM isolation.
	SandboxContainer SandboxType = "container"
)

// SandboxSpec defines reusable runtime, resource and private-storage settings.
type SandboxSpec struct {
	// +kubebuilder:default=kata
	// +optional
	Type SandboxType `json:"type,omitempty"`

	Resources SandboxResources `json:"resources"`
	Storage   SandboxStorage   `json:"storage"`
}

type SandboxResources struct {
	Agent SandboxContainerResources `json:"agent"`

	// Helpers applies to each approved init or auxiliary container.
	Helpers SandboxContainerResources `json:"helpers"`
}

// +kubebuilder:validation:XValidation:rule="self.requests.all(k, k in self.limits && isQuantity(string(self.requests[k])) && isQuantity(string(self.limits[k])) && !quantity(string(self.requests[k])).isGreaterThan(quantity(string(self.limits[k]))))",message="resource requests must not exceed their limits"
type SandboxContainerResources struct {
	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:MinProperties=3
	// +kubebuilder:validation:MaxProperties=3
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['cpu', 'memory', 'ephemeral-storage'])",message="exactly cpu, memory and ephemeral-storage are required"
	// +kubebuilder:validation:XValidation:rule="self.all(k, isQuantity(string(self[k])) && quantity(string(self[k])).isGreaterThan(quantity('0')))",message="resource quantities must be positive"
	Requests corev1.ResourceList `json:"requests"`

	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:MinProperties=3
	// +kubebuilder:validation:MaxProperties=3
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['cpu', 'memory', 'ephemeral-storage'])",message="exactly cpu, memory and ephemeral-storage are required"
	// +kubebuilder:validation:XValidation:rule="self.all(k, isQuantity(string(self[k])) && quantity(string(self[k])).isGreaterThan(quantity('0')))",message="resource quantities must be positive"
	Limits corev1.ResourceList `json:"limits"`
}

type SandboxStorage struct {
	Home    SandboxHomeStorage    `json:"home"`
	Scratch SandboxScratchStorage `json:"scratch"`
}

type SandboxHomeStorage struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	StorageClassName string `json:"storageClassName"`

	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="home size must be positive"
	Size resource.Quantity `json:"size"`

	// AccessMode alone does not establish private ownership or writer exclusion.
	// +kubebuilder:validation:Enum=ReadWriteOncePod;ReadWriteOnce
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode"`
}

type SandboxScratchStorage struct {
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="scratch size must be positive"
	SizeLimit resource.Quantity `json:"sizeLimit"`
}

// SandboxConfigurationStatus observes prerequisites, not per-Agent enforcement.
type SandboxConfigurationStatus struct {
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
// +kubebuilder:resource:path=sandboxes,scope=Cluster,shortName=crs,singular=sandbox
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Sandbox defines shared configuration; it does not install or start a runtime.
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SandboxSpec `json:"spec"`

	// +optional
	Status SandboxConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
