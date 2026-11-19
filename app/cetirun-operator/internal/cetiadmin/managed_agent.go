// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ResolveManagedAgent reads the current Agent intent. An incomplete runtime
// does not prevent an administrator from correcting that intent.
func ResolveManagedAgent(ctx context.Context, reader client.Reader, config Config, self Record, name string) (*api.Agent, error) {
	if reader == nil || name == "" || name == self.Agent.Name || name == config.InstanceName {
		return nil, ErrUnbound
	}
	agent := &api.Agent{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, agent); err != nil {
		return nil, err
	}
	if agent.UID == "" || agent.UID == self.Agent.UID || agent.DeletionTimestamp != nil || agent.Namespace != "" ||
		(config.MigrationSource != nil && (agent.UID == config.MigrationSource.Agent.UID || agent.Name == config.MigrationSource.Agent.Name)) {
		return nil, ErrUnbound
	}
	switch agent.Spec.Agent.Type {
	case api.AgentTypeOpenClaw, api.AgentTypeHermes, api.AgentTypeCustom:
	default:
		return nil, ErrUnbound
	}
	namespace := "cetirun-" + agent.Name
	if namespace == self.Namespace.Name || namespace == self.Source.Namespace ||
		(agent.Status.Namespace != "" && agent.Status.Namespace != namespace) ||
		(agent.Status.HelmReleaseName != "" && agent.Status.HelmReleaseName != "openclaw-"+agent.Name) {
		return nil, ErrConflict
	}
	return agent, nil
}

// ResolveRuntimeTarget derives ownership from the API for this request. The
// returned value is not a registration or a persistent authorization snapshot.
func ResolveRuntimeTarget(ctx context.Context, reader client.Reader, config Config, self Record, name, namespace string) (*api.Agent, TargetIdentity, error) {
	var target TargetIdentity
	if reader == nil || (name == "" && namespace == "") {
		return nil, target, ErrUnbound
	}
	if name == "" {
		agents := &api.AgentList{}
		if err := reader.List(ctx, agents); err != nil {
			return nil, target, err
		}
		for i := range agents.Items {
			agent := &agents.Items[i]
			if "cetirun-"+agent.Name == namespace {
				if name != "" {
					return nil, target, ErrConflict
				}
				name = agent.Name
			}
		}
	}
	agent, err := ResolveManagedAgent(ctx, reader, config, self, name)
	if err != nil {
		return nil, target, err
	}
	if namespace != "" && namespace != "cetirun-"+agent.Name {
		return nil, target, ErrConflict
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: "cetirun-" + agent.Name}, ns); err != nil {
		return nil, target, err
	}
	if ns.UID == "" || ns.DeletionTimestamp != nil || NamespaceExcludedFromAdmission(ns.Labels) ||
		(config.MigrationSource != nil && ns.UID == config.MigrationSource.Namespace.UID) {
		return nil, target, ErrUnbound
	}
	target = TargetIdentity{
		Agent: ObjectIdentity{Name: agent.Name, UID: agent.UID}, Namespace: ObjectIdentity{Name: ns.Name, UID: ns.UID},
		Runtime: string(agent.Spec.Agent.Type), Release: "openclaw-" + agent.Name, Team: agent.Spec.Team, AuthSecret: agent.Spec.GatewayTokenSecretRef,
		Protected: agent.Spec.SandboxRef != "" || agent.Status.Sandbox != nil || agent.Status.Policy != nil ||
			slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) || ns.Labels["cetirun.microsoft.com/sandbox-protected"] == "true",
	}
	if agent.Spec.Team != "" {
		team := &api.Team{}
		if err := reader.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
			return nil, target, err
		}
		if team.UID == "" || team.DeletionTimestamp != nil {
			return nil, target, ErrConflict
		}
		target.Protected = target.Protected || team.Spec.PolicyConfigured()
	}
	if agent.Spec.Agent.Type != api.AgentTypeCustom {
		if !ownedByAgent(ns, agent) {
			return nil, target, ErrConflict
		}
		return agent, target, nil
	}
	if agent.Spec.SandboxRef != "" || agent.Status.Sandbox != nil {
		if !NativeSandboxNamespaceOwned(agent, ns) {
			return nil, target, fmt.Errorf("%w: native Custom namespace ownership", ErrConflict)
		}
		return agent, target, nil
	}
	hash := sha256.Sum256([]byte(agent.UID))
	record := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: self.Source.Namespace, Name: "custom-ownership-" + hex.EncodeToString(hash[:16])}
	if err := reader.Get(ctx, key, record); err != nil {
		return nil, target, err
	}
	var receipt struct {
		AgentName    string    `json:"agentName"`
		AgentUID     types.UID `json:"agentUID"`
		Namespace    string    `json:"namespace"`
		NamespaceUID types.UID `json:"namespaceUID"`
		Release      string    `json:"release"`
	}
	if record.UID == "" || record.DeletionTimestamp != nil || !ownedByAgent(record, agent) || record.Immutable == nil || !*record.Immutable ||
		(len(ns.OwnerReferences) != 0 && !ownedByAgent(ns, agent)) || json.Unmarshal([]byte(record.Data["ownership"]), &receipt) != nil ||
		receipt.AgentName != agent.Name || receipt.AgentUID != agent.UID || receipt.Namespace != ns.Name || receipt.NamespaceUID != ns.UID || receipt.Release != target.Release {
		return nil, target, fmt.Errorf("%w: Custom runtime ownership", ErrConflict)
	}
	return agent, target, nil
}

// Native Sandbox namespaces have exact Agent ownership and protection markers,
// not the immutable receipt used by legacy Custom namespace adoption.
func NativeSandboxNamespaceOwned(agent *api.Agent, namespace *corev1.Namespace) bool {
	return agent != nil && namespace != nil && agent.UID != "" && agent.Namespace == "" &&
		(agent.Spec.SandboxRef != "" || agent.Status.Sandbox != nil) &&
		namespace.UID != "" && namespace.Namespace == "" && namespace.DeletionTimestamp == nil &&
		namespace.Name == "cetirun-"+agent.Name &&
		namespace.Labels[sandboxworkload.ProtectedNamespaceLabel] == "true" &&
		namespace.Annotations[sandboxworkload.AgentUIDAnnotation] == string(agent.UID) &&
		namespace.Annotations[sandboxworkload.AgentNameAnnotation] == agent.Name &&
		equality.Semantic.DeepEqual(namespace.OwnerReferences, []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)})
}
