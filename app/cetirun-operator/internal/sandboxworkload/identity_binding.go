// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// ValidateIdentityBinding checks authoritative readback against a current
// compiled bundle and an already recorded ServiceAccount UID. It cannot mint a
// pin, adopt a replacement, prove cloud federation or authorize execution.
// The common executor must persist/check this pin through its existing source
// reservation and recovery path before enabling dedicated identities.
func (b *Bundle) ValidateIdentityBinding(namespace *corev1.Namespace, account *corev1.ServiceAccount, pinned ObjectIdentity) error {
	if b.input.Resolution.AgentPolicy.KubernetesAccess == nil && !b.input.Resolution.AgentPolicy.EntraID.Enabled {
		return fmt.Errorf("no dedicated runtime identity is requested")
	}
	expected := ReconcileExpectation{
		Version:   ReconcileExpectationVersion,
		Agent:     ObjectIdentity{Name: b.input.AgentName, UID: b.input.Resolution.Binding.AgentUID},
		Namespace: b.input.Namespace, Binding: b.input.Resolution.Binding,
		Revision: b.input.Controls.Revision, Controls: b.input.Controls.Objects,
		IdentityServiceAccount: b.input.Controls.ServiceAccountSource,
	}
	return expected.ValidateIdentityBinding(namespace, account, pinned)
}

// ValidateIdentityBinding also accepts the existing protected common-executor
// expectation, so observation and admission need not rebuild a Bundle. This
// checks only identity readback; it does not validate the rest of the expectation
// or supply current-source, reservation or execution authorization.
func (expected ReconcileExpectation) ValidateIdentityBinding(namespace *corev1.Namespace, account *corev1.ServiceAccount, pinned ObjectIdentity) error {
	if expected.Version != ReconcileExpectationVersion || expected.Agent.Name == "" || expected.Agent.UID == "" ||
		expected.Namespace.Name == "" || expected.Namespace.UID == "" || expected.Binding.AgentUID != expected.Agent.UID ||
		expected.Binding.TeamUID == "" || expected.Binding.SandboxUID == "" || expected.Binding.AgentPolicyUID == "" ||
		!ValidInputDigest(expected.Revision) {
		return fmt.Errorf("runtime identity requires a complete protected binding")
	}
	owner := []metav1.OwnerReference{AgentOwner(expected.Agent.Name, expected.Agent.UID)}
	if namespace == nil || namespace.Name != expected.Namespace.Name || namespace.UID != expected.Namespace.UID ||
		namespace.ResourceVersion == "" || !namespace.DeletionTimestamp.IsZero() ||
		namespace.Labels[ProtectedNamespaceLabel] != "true" ||
		namespace.Annotations[AgentNameAnnotation] != expected.Agent.Name ||
		namespace.Annotations[AgentUIDAnnotation] != string(expected.Agent.UID) ||
		!equality.Semantic.DeepEqual(namespace.OwnerReferences, owner) {
		return fmt.Errorf("runtime identity namespace no longer matches the current Agent binding")
	}
	if account == nil || pinned.Name == "" || pinned.Name == "default" || pinned.UID == "" ||
		account.Name != pinned.Name || account.UID != pinned.UID || account.Namespace != namespace.Name ||
		account.ResourceVersion == "" || !account.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(account.OwnerReferences, owner) ||
		account.Labels[sandboxpolicy.AgentUIDLabel] != string(expected.Agent.UID) ||
		account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken ||
		len(account.Secrets)+len(account.ImagePullSecrets) != 0 {
		return fmt.Errorf("runtime identity ServiceAccount no longer matches its recorded dedicated binding")
	}
	binding, err := json.Marshal(expected.Binding)
	if err != nil {
		return err
	}
	controls := slices.Clone(expected.Controls)
	if source := expected.IdentityServiceAccount; source != nil {
		if source.UID != pinned.UID || source.Name != pinned.Name {
			return fmt.Errorf("runtime identity differs from its bootstrap source pin")
		}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(source)
		if err != nil {
			return err
		}
		controls = append(controls, unstructured.Unstructured{Object: raw})
	}
	for _, desired := range controls {
		if desired.GetAPIVersion() != "v1" || desired.GetKind() != "ServiceAccount" ||
			desired.GetName() != pinned.Name || desired.GetNamespace() != namespace.Name {
			continue
		}
		if desired.GetAnnotations()[sandboxpolicy.BindingAnnotation] != string(binding) ||
			desired.GetAnnotations()[sandboxpolicy.RevisionAnnotation] != expected.Revision ||
			desired.GetLabels()[sandboxpolicy.AgentUIDLabel] != string(expected.Agent.UID) ||
			!equality.Semantic.DeepEqual(desired.GetOwnerReferences(), owner) {
			return fmt.Errorf("runtime identity control differs from the protected binding")
		}
		for key, value := range desired.GetAnnotations() {
			if account.Annotations[key] != value {
				return fmt.Errorf("runtime identity approval metadata changed")
			}
		}
		for key, value := range account.Annotations {
			if strings.HasPrefix(key, "azure.workload.identity/") {
				if approved, exists := desired.GetAnnotations()[key]; !exists || approved != value {
					return fmt.Errorf("runtime identity contains unapproved Entra metadata")
				}
			}
		}
		return nil
	}
	return fmt.Errorf("current compiler output has no matching identity ServiceAccount")
}
