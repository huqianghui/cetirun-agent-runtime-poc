// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AnnotationRuntimeGrantAgent        = "cetirun.microsoft.com/cetiadmin-grant-agent"
	AnnotationRuntimeGrantAgentUID     = "cetirun.microsoft.com/cetiadmin-grant-agent-uid"
	AnnotationRuntimeGrantNamespace    = "cetirun.microsoft.com/cetiadmin-grant-namespace"
	AnnotationRuntimeGrantNamespaceUID = "cetirun.microsoft.com/cetiadmin-grant-namespace-uid"

	// Keep these stable Sandbox wire markers local to the access contract;
	// the policy package receives its platform verifier from orchestration.
	sandboxAgentUIDLabel       = "sandbox.cetirun.microsoft.com/agent-uid"
	sandboxGrantRevisionMarker = "sandbox.cetirun.microsoft.com/grant-revision"
)

// AccessName is the stable per-installation prefix for CetiAdmin RBAC.
func AccessName(recordUID types.UID) string {
	sum := sha256.Sum256([]byte(recordUID))
	return "cetiadmin-" + hex.EncodeToString(sum[:12])
}

// MaintenanceRecordName and CustomOwnershipRecordName identify the only
// source-namespace receipts that runtime access may read.
func MaintenanceRecordName(agentUID types.UID) string {
	sum := sha256.Sum256([]byte(agentUID))
	return "cetiadmin-maintenance-" + hex.EncodeToString(sum[:16])
}

func CustomOwnershipRecordName(agentUID types.UID) string {
	sum := sha256.Sum256([]byte(agentUID))
	return "custom-ownership-" + hex.EncodeToString(sum[:16])
}

func RuntimeReaderRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps", "services", "serviceaccounts", "persistentvolumeclaims"}, Verbs: []string{"get"}},
		{APIGroups: []string{"apps"}, Resources: []string{"statefulsets", "deployments"}, Verbs: []string{"get"}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get"}},
		{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"roles", "rolebindings"}, Verbs: []string{"get"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies", "ingresses"}, Verbs: []string{"get"}},
		{APIGroups: []string{"networking.istio.io"}, Resources: []string{"sidecars"}, Verbs: []string{"get"}},
		{APIGroups: []string{"security.istio.io"}, Resources: []string{"authorizationpolicies", "peerauthentications"}, Verbs: []string{"get"}},
		{APIGroups: []string{"autoscaling"}, Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"get"}},
		{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}, Verbs: []string{"get"}},
	}
}

// RuntimeAccessBindingVerifier binds classification to the configured source,
// without making the Sandbox policy package depend on CetiAdmin orchestration.
func RuntimeAccessBindingVerifier(source client.ObjectKey) func(context.Context, client.Reader, client.Object, string, types.UID) (bool, error) {
	if source.Name == "" || source.Namespace == "" {
		return nil
	}
	return func(ctx context.Context, reader client.Reader, object client.Object, agentName string, agentUID types.UID) (bool, error) {
		return OwnedAccessBinding(ctx, reader, source, object, agentName, agentUID)
	}
}

// OwnedAccessBinding recognizes an exact, current CetiAdmin runtime access
// binding. The source is deployment configuration, never data supplied by the
// candidate binding. A CetiAdmin-looking candidate that cannot be fully
// confirmed returns an error so callers fail closed rather than treating it as
// an ordinary Sandbox grant.
func OwnedAccessBinding(ctx context.Context, reader client.Reader, configuredSource client.ObjectKey, object client.Object, targetName string, targetUID types.UID) (bool, error) {
	if reader == nil || configuredSource.Name == "" || configuredSource.Namespace == "" || targetName == "" || targetUID == "" {
		return false, nil
	}
	if !runtimeGrantCandidate(object) {
		return false, nil
	}
	if object.GetLabels()[sandboxAgentUIDLabel] != "" || object.GetAnnotations()[sandboxGrantRevisionMarker] != "" {
		return false, fmt.Errorf("%w: runtime access binding contains Sandbox grant markers", ErrConflict)
	}
	if object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil {
		return false, fmt.Errorf("%w: runtime access binding identity is incomplete", ErrConflict)
	}

	source := &corev1.ConfigMap{}
	if err := reader.Get(ctx, configuredSource, source); err != nil {
		return false, fmt.Errorf("%w: read configured CetiAdmin source: %w", ErrConflict, err)
	}
	if source.UID == "" || source.DeletionTimestamp != nil {
		return false, fmt.Errorf("%w: configured CetiAdmin source is unavailable", ErrConflict)
	}
	stateObject := &corev1.ConfigMap{}
	stateKey := client.ObjectKey{Namespace: configuredSource.Namespace, Name: InstallationName(configuredSource.Namespace, configuredSource.Name)}
	if err := reader.Get(ctx, stateKey, stateObject); err != nil {
		return false, fmt.Errorf("%w: read CetiAdmin installation: %w", ErrConflict, err)
	}
	state, err := ParseInstallation(stateObject.Data[BindingKey])
	if err != nil || stateObject.UID == "" || stateObject.DeletionTimestamp != nil || state.RecordUID == "" || state.RecordUID != stateObject.UID ||
		state.Source.Name != configuredSource.Name || state.Source.Namespace != configuredSource.Namespace || state.Source.UID != source.UID ||
		stateObject.Annotations[AnnotationSourceUID] != string(source.UID) || !state.CanRun() {
		return false, fmt.Errorf("%w: current CetiAdmin installation is not an active configured source", ErrConflict)
	}
	config, err := ParseConfig(source.Data[ConfigKey])
	if err != nil {
		return false, fmt.Errorf("%w: parse configured CetiAdmin source: %w", ErrConflict, err)
	}
	observed, err := observeInstallation(ctx, reader, state.Record)
	if err != nil {
		return false, err
	}
	binding, err := ResolveBinding(config, &state.Record, observed)
	if err != nil {
		return false, err
	}
	agent, target, err := ResolveRuntimeTarget(ctx, reader, binding.Config(), state.Record, targetName, "")
	if err != nil || agent.UID != targetUID || target.Agent.UID != targetUID {
		return false, fmt.Errorf("%w: runtime target is not current", ErrConflict)
	}
	if err := verifyRuntimeBinding(ctx, reader, object, state, target); err != nil {
		return false, err
	}
	return true, nil
}

func runtimeGrantCandidate(object client.Object) bool {
	annotations := object.GetAnnotations()
	for _, key := range []string{AnnotationSourceUID, AnnotationInstallationUID, AnnotationCreationID, AnnotationRuntimeGrantAgent, AnnotationRuntimeGrantAgentUID, AnnotationRuntimeGrantNamespace, AnnotationRuntimeGrantNamespaceUID} {
		if annotations[key] != "" {
			return true
		}
	}
	return false
}

func observeInstallation(ctx context.Context, reader client.Reader, record Record) (ObservedInstallation, error) {
	observed := ObservedInstallation{Source: &corev1.ConfigMap{}, Agent: &api.Agent{}, Team: &api.Team{}, Namespace: &corev1.Namespace{}, ServiceAccount: &corev1.ServiceAccount{}}
	for _, item := range []struct {
		key client.ObjectKey
		obj client.Object
	}{
		{key: client.ObjectKey{Namespace: record.Source.Namespace, Name: record.Source.Name}, obj: observed.Source},
		{key: client.ObjectKey{Name: record.Agent.Name}, obj: observed.Agent}, {key: client.ObjectKey{Name: record.Team.Name}, obj: observed.Team},
		{key: client.ObjectKey{Name: record.Namespace.Name}, obj: observed.Namespace}, {key: client.ObjectKey{Namespace: record.ServiceAccount.Namespace, Name: record.ServiceAccount.Name}, obj: observed.ServiceAccount},
	} {
		if err := reader.Get(ctx, item.key, item.obj); err != nil {
			return ObservedInstallation{}, fmt.Errorf("%w: read CetiAdmin installation identity: %w", ErrConflict, err)
		}
	}
	return observed, nil
}

func verifyRuntimeBinding(ctx context.Context, reader client.Reader, object client.Object, state Installation, target TargetIdentity) error {
	annotations := object.GetAnnotations()
	if annotations[AnnotationSourceUID] != string(state.Source.UID) || annotations[AnnotationInstallationUID] != string(state.RecordUID) || annotations[AnnotationCreationID] == "" ||
		annotations[AnnotationRuntimeGrantAgent] != target.Agent.Name || annotations[AnnotationRuntimeGrantAgentUID] != string(target.Agent.UID) ||
		annotations[AnnotationRuntimeGrantNamespace] != target.Namespace.Name || annotations[AnnotationRuntimeGrantNamespaceUID] != string(target.Namespace.UID) ||
		!agentOwner(object, target.Agent) {
		return fmt.Errorf("%w: runtime access binding ownership changed", ErrConflict)
	}
	name := AccessName(state.RecordUID)
	admin := rbacv1.Subject{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}
	readerSubject := rbacv1.Subject{Kind: "ServiceAccount", Name: "cetirun-sandbox-admission", Namespace: state.Source.Namespace}
	switch binding := object.(type) {
	case *rbacv1.RoleBinding:
		if binding.Namespace == target.Namespace.Name && binding.Name == name+"-runtime" {
			capability := "runtime"
			if target.Protected {
				capability = "protected"
			}
			if !exactBinding(binding.Subjects, admin, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name + "-" + capability}, binding.RoleRef) {
				return fmt.Errorf("%w: runtime access binding contents changed", ErrConflict)
			}
			return verifyAccessClusterRole(ctx, reader, state, name+"-"+capability, runtimeRoleRules(target.Protected))
		}
		if binding.Namespace == target.Namespace.Name && binding.Name == name+"-reader" {
			if !exactBinding(binding.Subjects, readerSubject, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name + "-reader"}, binding.RoleRef) {
				return fmt.Errorf("%w: runtime reader binding contents changed", ErrConflict)
			}
			return verifyAccessClusterRole(ctx, reader, state, name+"-reader", RuntimeReaderRules())
		}
		receiptName := name + "-" + shortUIDHash(target.Agent.UID)
		if binding.Namespace != state.Source.Namespace || binding.Name != receiptName || !exactBinding(binding.Subjects, readerSubject, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: receiptName}, binding.RoleRef) {
			return fmt.Errorf("%w: runtime receipt binding contents changed", ErrConflict)
		}
		return verifyReceiptRole(ctx, reader, state, target, receiptName)
	default:
		return fmt.Errorf("%w: runtime access requires a RoleBinding", ErrConflict)
	}
}

func exactBinding(subjects []rbacv1.Subject, subject rbacv1.Subject, expected, actual rbacv1.RoleRef) bool {
	return len(subjects) == 1 && subjects[0] == subject && expected == actual
}

func agentOwner(object metav1.Object, identity ObjectIdentity) bool {
	owners := object.GetOwnerReferences()
	return len(owners) == 1 && owners[0].APIVersion == api.GroupVersion.String() && owners[0].Kind == "Agent" && owners[0].Name == identity.Name && owners[0].UID == identity.UID
}

func shortUIDHash(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:8])
}

func verifyAccessClusterRole(ctx context.Context, reader client.Reader, state Installation, name string, allowed []rbacv1.PolicyRule) error {
	ref := accessResource(state, "ClusterRole", "", name)
	if ref == nil || ref.UID == "" || ref.CreationID == "" {
		return fmt.Errorf("%w: runtime ClusterRole is not recorded", ErrConflict)
	}
	role := &rbacv1.ClusterRole{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, role); err != nil {
		return fmt.Errorf("%w: read runtime ClusterRole: %w", ErrConflict, err)
	}
	if role.UID != ref.UID || role.ResourceVersion == "" || role.DeletionTimestamp != nil || role.AggregationRule != nil || role.Annotations[AnnotationCreationID] != ref.CreationID || role.Annotations[AnnotationInstallationUID] != string(state.RecordUID) || role.Annotations[AnnotationSourceUID] != string(state.Source.UID) || !agentOwner(role, state.Agent) || !rulesAllowed(role.Rules, allowed) {
		return fmt.Errorf("%w: runtime ClusterRole changed", ErrConflict)
	}
	return nil
}

func accessResource(state Installation, kind, namespace, name string) *ManagedResource {
	for i := range state.Resources {
		ref := &state.Resources[i]
		if ref.Kind == kind && ref.Namespace == namespace && ref.Name == name {
			return ref
		}
	}
	return nil
}

func verifyReceiptRole(ctx context.Context, reader client.Reader, state Installation, target TargetIdentity, name string) error {
	role := &rbacv1.Role{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: state.Source.Namespace, Name: name}, role); err != nil {
		return fmt.Errorf("%w: read runtime receipt Role: %w", ErrConflict, err)
	}
	names := []string{}
	if !target.Protected {
		names = append(names, MaintenanceRecordName(target.Agent.UID))
	}
	if target.Runtime == string(api.AgentTypeCustom) {
		names = append(names, CustomOwnershipRecordName(target.Agent.UID))
	}
	wanted := []rbacv1.PolicyRule(nil)
	if len(names) != 0 {
		wanted = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: names, Verbs: []string{"get"}}}
	}
	if role.UID == "" || role.ResourceVersion == "" || role.DeletionTimestamp != nil || role.Annotations[AnnotationCreationID] == "" || role.Annotations[AnnotationInstallationUID] != string(state.RecordUID) || role.Annotations[AnnotationSourceUID] != string(state.Source.UID) || !agentOwner(role, target.Agent) || !reflect.DeepEqual(role.Rules, wanted) {
		return fmt.Errorf("%w: runtime receipt Role changed", ErrConflict)
	}
	return nil
}

func runtimeRoleRules(protected bool) []rbacv1.PolicyRule {
	rules := NamespaceOperationRules(protected)
	for _, resource := range DiagnosticResources() {
		rules = append(rules, ReadRule(resource.Group, resource.Resource))
	}
	rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}})
	return rules
}

func rulesAllowed(actual, allowed []rbacv1.PolicyRule) bool {
	if len(actual) == 0 {
		return false
	}
	for _, rule := range actual {
		if !slices.ContainsFunc(allowed, func(candidate rbacv1.PolicyRule) bool { return reflect.DeepEqual(rule, candidate) }) {
			return false
		}
	}
	return true
}
