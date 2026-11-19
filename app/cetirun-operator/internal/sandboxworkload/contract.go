// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sandboxworkload translates a shared Sandbox resolution into protected
// chart inputs. A render plan is not permission or evidence to start execution.
package sandboxworkload

import (
	"errors"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	MaxPreparationSources   = 128
	ContractVersion         = "v1"
	ExpectationVersion      = "v1"
	ValuesKey               = "sandboxWorkload"
	RevisionAnnotation      = "cetirun.microsoft.com/sandbox-revision"
	AgentUIDAnnotation      = "cetirun.microsoft.com/agent-uid"
	AgentNameAnnotation     = "cetirun.microsoft.com/agent-name"
	ProtectedNamespaceLabel = "cetirun.microsoft.com/sandbox-protected"
	KindAnnotation          = "cetirun.microsoft.com/sandbox-workload"
	AgentWorkload           = "agent"
	HomePath                = "/home/vibe"
	SkillsPath              = "/home/vibe/.aks/skills"
	NativePodFinalizer      = "cetirun.microsoft.com/native-pod-lifetime"
)

var (
	ErrExecutionHeld             = errors.New("Sandbox execution remains held")
	ErrStorageTransitionRequired = errors.New("Sandbox HOME requires a separately approved stopped-workload transition")
)

// Controls carries compiler outputs, not user-selected destinations or grants.
// This package validates their shape but does not compile network or identity
// policy. Revision must cover both the resolution and the trusted control inputs.
type Controls struct {
	Revision             string
	ResolutionRevision   string
	Binding              sandbox.Binding
	DNSIPs               []string
	TrustedProxyIPs      []string `json:",omitempty"`
	IngressMode          api.AgentPolicyIngressMode
	ServiceAccountName   string
	ServiceAccountSource *corev1.ServiceAccount `json:",omitempty"`
	Volumes              []corev1.Volume
	VolumeMounts         []corev1.VolumeMount
	Environment          []corev1.EnvVar
	Objects              []unstructured.Unstructured
	PodLabels            map[string]string
	IngressMTLS          *ingressmtls.Plan   `json:",omitempty"`
	EgressSNI            *egresscapture.Plan `json:",omitempty"`
	ProxyDigest          string              `json:",omitempty"`
}

type ObjectIdentity struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// Helpers selects only chart-owned helpers. Arbitrary init/sidecar paths are not
// part of the contract, including the not-yet-defined separate Copilot workload.
type Helpers struct {
	Doctor  bool `json:"doctor"`
	Logger  bool `json:"logger"`
	Desktop bool `json:"desktop"`
}

type Input struct {
	ConnectionsMCP    *ConnectionsMCP `json:",omitempty"`
	Reconciled        bool
	Suspended         bool                         `json:",omitempty"`
	PlannedMigration  *api.PlannedMigrationRequest `json:",omitempty"`
	Resolution        *sandbox.Resolution
	AgentName         string
	AgentGeneration   int64
	AgentType         string
	CustomAgent       *CustomAgent `json:",omitempty"`
	GatewayAPIBase    string       `json:",omitempty"`
	Namespace         ObjectIdentity
	WorkloadName      string
	ReleaseName       string
	RuntimeClass      *nodev1.RuntimeClass
	Home              *corev1.PersistentVolumeClaim
	Skills            *corev1.PersistentVolumeClaim
	SkillsMode        SkillsMode             `json:",omitempty"`
	SkillMarketplace  *SkillMarketplaceInput `json:",omitempty"`
	Helpers           Helpers
	Controls          Controls
	AuthorizedSecrets []string
	GatewayKey        *ObjectIdentity
	ImagePullSecret   *ObjectIdentity `json:",omitempty"`
	Context           *Context        `json:",omitempty"`
	Terminal          *Terminal       `json:",omitempty"`
	LoopbackProbe     *LoopbackProbe  `json:",omitempty"`
}

// Accounting uses Kubernetes effective init/sidecar accounting. Pod contains
// the supported Pod-level CPU/memory declarations; Effective includes runtime
// overhead and ephemeral-storage accounting, not an independent disk quota.
type Accounting struct {
	Pod       corev1.ResourceRequirements `json:"pod"`
	Effective corev1.ResourceRequirements `json:"effective"`
}

// Bundle is immutable to callers; methods return copies of its values.
type Bundle struct {
	input      Input
	accounting Accounting
}

// SourceReference pins exact resource versions except for the status-bearing
// Agent, whose generation and authorization-relevant intent are pinned instead.
type SourceReference struct {
	APIVersion      string    `json:"apiVersion"`
	Kind            string    `json:"kind"`
	Namespace       string    `json:"namespace,omitempty"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion,omitempty"`
	Generation      int64     `json:"generation,omitempty"`
	IntentDigest    string    `json:"intentDigest,omitempty"`
}

// Expectation is persisted only by the controller in its protected state
// namespace. Admission re-reads it and its sources on every protected request.
// It cannot itself lift the lifecycle hold.
type Expectation struct {
	Version            string                 `json:"version"`
	Binding            sandbox.Binding        `json:"binding"`
	Revision           string                 `json:"revision"`
	Agent              ObjectIdentity         `json:"agent"`
	AgentGeneration    int64                  `json:"agentGeneration"`
	Namespace          ObjectIdentity         `json:"namespace"`
	Workload           ObjectIdentity         `json:"workload"`
	WorkloadKind       string                 `json:"workloadKind"`
	ControllerRevision string                 `json:"controllerRevision,omitempty"`
	ExecutionHeld      bool                   `json:"executionHeld"`
	SinglePod          bool                   `json:"singlePod,omitempty"`
	PodUID             types.UID              `json:"podUID,omitempty"`
	SourceDigests      map[string]string      `json:"sourceDigests,omitempty"`
	Template           corev1.PodTemplateSpec `json:"template"`
	Overhead           corev1.ResourceList    `json:"overhead,omitempty"`
	Sources            []SourceReference      `json:"sources"`
	Home               ObjectIdentity         `json:"home"`
	Skills             ObjectIdentity         `json:"skills"`
	SkillsMode         SkillsMode             `json:"skillsMode,omitempty"`
	Placement          *PlacementBinding      `json:"placement,omitempty"`
}

func AgentOwner(name string, uid types.UID) metav1.OwnerReference {
	controller, block := true, true
	return metav1.OwnerReference{
		APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: name,
		UID: uid, Controller: &controller, BlockOwnerDeletion: &block,
	}
}
