// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const (
	AnnotationSourceUID = "cetirun.microsoft.com/cetiadmin-source-uid"
	BindingKey          = "binding.json"
)

var (
	ErrUnbound  = errors.New("CetiAdminBindingUnavailable")
	ErrConflict = errors.New("CetiAdminBindingConflict")
)

type ObjectIdentity struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace,omitempty"`
	UID       types.UID `json:"uid"`
}

// Record is controller-owned recovery data. Reading a Record does not grant
// authority; ResolveBinding must check its identities against current objects.
type Record struct {
	SchemaVersion  int            `json:"schemaVersion"`
	Source         ObjectIdentity `json:"source"`
	ConfigHash     string         `json:"configHash"`
	Agent          ObjectIdentity `json:"agent"`
	Team           ObjectIdentity `json:"team"`
	Namespace      ObjectIdentity `json:"namespace"`
	ServiceAccount ObjectIdentity `json:"serviceAccount"`
}

type ObservedInstallation struct {
	Source         *corev1.ConfigMap
	Agent          *api.Agent
	Team           *api.Team
	Namespace      *corev1.Namespace
	ServiceAccount *corev1.ServiceAccount
}

type Binding struct {
	config Config
	record Record
	valid  bool
}

func ConfigHash(config Config) (string, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func ResolveBinding(config Config, record *Record, observed ObservedInstallation) (Binding, error) {
	if !config.Enabled || record == nil || observed.Source == nil || observed.Agent == nil ||
		observed.Team == nil || observed.Namespace == nil || observed.ServiceAccount == nil {
		return Binding{}, ErrUnbound
	}
	if record.SchemaVersion != 1 {
		return Binding{}, fmt.Errorf("%w: unsupported record version", ErrConflict)
	}
	for _, item := range []struct {
		name     string
		expected ObjectIdentity
		actual   metav1.Object
	}{
		{"source", record.Source, observed.Source}, {"Agent", record.Agent, observed.Agent},
		{"Team", record.Team, observed.Team}, {"namespace", record.Namespace, observed.Namespace},
		{"ServiceAccount", record.ServiceAccount, observed.ServiceAccount},
	} {
		if !item.expected.matches(item.actual) {
			return Binding{}, fmt.Errorf("%w: %s identity changed or is terminating", ErrConflict, item.name)
		}
	}
	current, err := ParseConfig(observed.Source.Data[ConfigKey])
	if err != nil {
		return Binding{}, fmt.Errorf("%w: current installation configuration is invalid", ErrConflict)
	}
	hash, err := ConfigHash(config)
	if err != nil {
		return Binding{}, err
	}
	currentHash, err := ConfigHash(current)
	if err != nil {
		return Binding{}, err
	}
	if currentHash != hash || record.ConfigHash != hash {
		return Binding{}, fmt.Errorf("%w: installation configuration changed", ErrConflict)
	}
	agent := observed.Agent
	if observed.Source.Namespace == "" || agent.Namespace != "" || agent.Name != config.InstanceName ||
		agent.Spec.Agent.Type != api.AgentTypeHermes || agent.Spec.SandboxRef != "" ||
		agent.Spec.User.Alias != config.Owner.Alias || agent.Spec.Team != config.InstanceName ||
		!strings.EqualFold(agent.Annotations[api.AnnotationOwnerTenantID], config.Owner.TenantID) ||
		!strings.EqualFold(agent.Annotations[api.AnnotationOwnerObjectID], config.Owner.ObjectID) {
		return Binding{}, fmt.Errorf("%w: Agent does not match the installation owner and runtime", ErrConflict)
	}
	if observed.Team.Name != config.InstanceName || observed.Team.Namespace != "" || observed.Team.Spec.PolicyConfigured() {
		return Binding{}, fmt.Errorf("%w: CetiAdmin requires its recorded ordinary Team", ErrConflict)
	}
	if observed.Namespace.Name != "cetirun-"+agent.Name || observed.ServiceAccount.Namespace != observed.Namespace.Name {
		return Binding{}, fmt.Errorf("%w: namespace and ServiceAccount do not belong to this Agent", ErrConflict)
	}
	for _, object := range []metav1.Object{agent, observed.Team, observed.ServiceAccount} {
		if object.GetAnnotations()[AnnotationSourceUID] != string(observed.Source.UID) {
			return Binding{}, fmt.Errorf("%w: installation source marker is missing or changed", ErrConflict)
		}
	}
	if !ownedByAgent(observed.Namespace, agent) || !ownedByAgent(observed.ServiceAccount, agent) {
		return Binding{}, fmt.Errorf("%w: namespace or ServiceAccount has another owner", ErrConflict)
	}
	if observed.ServiceAccount.AutomountServiceAccountToken == nil || *observed.ServiceAccount.AutomountServiceAccountToken {
		return Binding{}, fmt.Errorf("%w: ServiceAccount must disable automatic token mounts", ErrConflict)
	}
	if config.Runtime.Version != "" && config.Runtime.Version != agent.Spec.Helm.Version {
		return Binding{}, fmt.Errorf("%w: Agent chart does not match the configured version", ErrConflict)
	}
	if config.Runtime.Image != "" && (agent.Spec.Agent.Image == nil || config.Runtime.Image != *agent.Spec.Agent.Image) {
		return Binding{}, fmt.Errorf("%w: Agent image does not match the configured image", ErrConflict)
	}
	if config.Runtime.Port != nil && (agent.Spec.Agent.Port == nil || *config.Runtime.Port != *agent.Spec.Agent.Port) {
		return Binding{}, fmt.Errorf("%w: Agent port does not match the configured port", ErrConflict)
	}
	return Binding{config: cloneConfig(config), record: *record, valid: true}, nil
}

func (i ObjectIdentity) matches(object metav1.Object) bool {
	return i.Name != "" && i.UID != "" && i.Name == object.GetName() &&
		i.Namespace == object.GetNamespace() && i.UID == object.GetUID() && object.GetDeletionTimestamp() == nil
}

func ownedByAgent(object metav1.Object, agent *api.Agent) bool {
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" && owner.Name == agent.Name && owner.UID == agent.UID {
			return true
		}
	}
	return false
}

func (b Binding) ServiceAccountUsername() string {
	if !b.valid {
		return ""
	}
	return "system:serviceaccount:" + b.record.ServiceAccount.Namespace + ":" + b.record.ServiceAccount.Name
}

func (b Binding) Record() Record {
	return b.record
}

func (b Binding) Config() Config {
	return cloneConfig(b.config)
}

func cloneConfig(config Config) Config {
	if config.MigrationSource != nil {
		source := *config.MigrationSource
		config.MigrationSource = &source
	}
	config.PlatformReadNamespaces = slices.Clone(config.PlatformReadNamespaces)
	config.Kubernetes.APIEgressCIDRs = slices.Clone(config.Kubernetes.APIEgressCIDRs)
	if config.Runtime.Port != nil {
		config.Runtime.Port = ptr.To(*config.Runtime.Port)
	}
	return config
}
