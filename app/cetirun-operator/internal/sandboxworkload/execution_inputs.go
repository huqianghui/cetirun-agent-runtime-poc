// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TeamInputs retains shared runtime inputs separately from Sandbox selection.
// It contains digests and reference names, never rendered configuration.
type TeamInputs struct {
	SharedDigest string   `json:"sharedDigest"`
	Policy       string   `json:"policy"`
	Sandboxes    []string `json:"sandboxes"`
}

func (inputs TeamInputs) DeepCopy() TeamInputs {
	inputs.Sandboxes = slices.Clone(inputs.Sandboxes)
	return inputs
}

func (inputs TeamInputs) DigestFor(box string) (string, error) {
	if err := inputs.Validate(); err != nil {
		return "", err
	}
	if box == "" || !slices.Contains(inputs.Sandboxes, box) {
		return "", fmt.Errorf("Team inputs do not authorize the selected Sandbox")
	}
	if inputs.Policy == "" {
		return "", fmt.Errorf("Team inputs do not authorize an AgentPolicy")
	}
	return inputDigest(struct{ Shared, Sandbox, Policy string }{inputs.SharedDigest, box, inputs.Policy})
}

func (inputs TeamInputs) Validate() error {
	if !ValidInputDigest(inputs.SharedDigest) || len(validation.IsDNS1123Subdomain(inputs.Policy)) != 0 ||
		len(inputs.Sandboxes) == 0 || len(inputs.Sandboxes) > 128 {
		return fmt.Errorf("Team input identity and selection inventory are invalid")
	}
	for i, box := range inputs.Sandboxes {
		if len(validation.IsDNS1123Subdomain(box)) != 0 || (i > 0 && inputs.Sandboxes[i-1] >= box) {
			return fmt.Errorf("Team Sandbox input names must be valid, unique and sorted")
		}
	}
	return nil
}

func ExecutionTeamInputs(team *api.Team) (TeamInputs, error) {
	if team == nil || team.UID == "" || team.ResourceVersion == "" || !team.DeletionTimestamp.IsZero() {
		return TeamInputs{}, fmt.Errorf("current persisted Team identity is required")
	}
	spec := team.Spec.DeepCopy()
	spec.AgentPolicyRef, spec.DefaultSandboxRef = "", ""
	spec.SandboxRefs = nil
	// Gateway reconciliation owns router grants and Agent model configuration.
	spec.ModelRouters = nil
	spec.Skills = slices.Clone(spec.Skills)
	slices.Sort(spec.Skills)
	shared, err := inputDigest(struct {
		Spec     *api.TeamSpec
		Metadata executionMetadata
	}{spec, effectiveMetadata(team)})
	if err != nil {
		return TeamInputs{}, err
	}
	boxes := slices.Clone(team.Spec.SandboxRefs)
	slices.Sort(boxes)
	return TeamInputs{SharedDigest: shared, Policy: team.Spec.AgentPolicyRef, Sandboxes: slices.Compact(boxes)}, nil
}

func SourceKey(source SourceReference) string {
	return source.APIVersion + "/" + source.Kind + "/" + source.Namespace + "/" + source.Name
}

func ExecutionSourceDigest(object client.Object, box string) (string, error) {
	if team, ok := object.(*api.Team); ok {
		inputs, err := ExecutionTeamInputs(team)
		if err != nil {
			return "", err
		}
		return inputs.DigestFor(box)
	}
	return ExecutionObjectDigest(object)
}

// EffectiveSource identifies non-secret resources whose configuration can be
// read without credentials. Secret contents remain unread and version-pinned;
// Agent intent and StatefulSet inputs have their own lifecycle-aware checks.
func EffectiveSource(source SourceReference) bool {
	switch source.APIVersion + "/" + source.Kind {
	case api.GroupVersion.String() + "/Team", api.GroupVersion.String() + "/Sandbox", api.GroupVersion.String() + "/AgentPolicy",
		"v1/Namespace", "v1/PersistentVolumeClaim", "v1/PersistentVolume", "v1/ConfigMap", "v1/ServiceAccount", "v1/Service",
		"node.k8s.io/v1/RuntimeClass", "storage.k8s.io/v1/StorageClass",
		"rbac.authorization.k8s.io/v1/Role", "rbac.authorization.k8s.io/v1/RoleBinding",
		"rbac.authorization.k8s.io/v1/ClusterRole", "rbac.authorization.k8s.io/v1/ClusterRoleBinding",
		"apps/v1/ControllerRevision", "cilium.io/v2/CiliumNetworkPolicy",
		"networking.istio.io/v1beta1/Gateway":
		return true
	}
	return false
}

// ExecutionObjectDigest excludes observation metadata, Gateway-only rate
// limits and the AgentPolicy skill allowlist, not execution controls.
// API reads must separately validate the requested key and immutable UID.
func ExecutionObjectDigest(object client.Object) (string, error) {
	if object == nil || object.GetUID() == "" || object.GetResourceVersion() == "" || !object.GetDeletionTimestamp().IsZero() {
		return "", fmt.Errorf("effective inputs require a persisted nonterminating object")
	}
	if _, metadataOnly := object.(*metav1.PartialObjectMetadata); metadataOnly {
		return "", fmt.Errorf("metadata alone cannot establish effective resource inputs")
	}
	if _, secret := object.(*corev1.Secret); secret || object.GetObjectKind().GroupVersionKind().Kind == "Secret" {
		return "", fmt.Errorf("Secret contents are not execution digest inputs")
	}
	body, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object.DeepCopyObject())
	if err != nil {
		return "", err
	}
	_, typedPolicy := object.(*api.AgentPolicy)
	if typedPolicy || object.GetObjectKind().GroupVersionKind() == api.GroupVersion.WithKind("AgentPolicy") {
		if err := canonicalPolicyExecutionBody(body); err != nil {
			return "", fmt.Errorf("AgentPolicy execution inputs: %w", err)
		}
	}
	if status, ok := body["status"].(map[string]any); ok && status["phase"] == string(corev1.ClaimLost) {
		return "", fmt.Errorf("lost storage cannot authorize execution")
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "status"} {
		delete(body, key)
	}
	return inputDigest(struct {
		Body     map[string]any
		Metadata executionMetadata
	}{body, effectiveMetadata(object)})
}

func canonicalPolicyExecutionBody(body map[string]any) error {
	// Typed serialization omits zero-valued leaves while retaining their parent
	// structs. Restore one semantic API shape without a lossy typed decode of
	// authoritative or unknown fields.
	for _, entry := range []struct {
		path  []string
		value any
	}{
		{[]string{"spec", "sandboxSelection", "allowMultiple"}, false},
		{[]string{"spec", "entraID", "enabled"}, false},
		{[]string{"spec", "network", "ingress", "requireMTLS"}, false},
		{[]string{"spec", "network", "blockedCIDRs"}, []any{}},
		{[]string{"spec", "network", "blockedFQDNs"}, []any{}},
		{[]string{"spec", "network", "blockedPorts"}, []any{}},
		{[]string{"spec", "network", "allowedInternalCIDRs"}, []any{}},
		{[]string{"spec", "modelAccess", "copilot", "allowed"}, false},
		{[]string{"spec", "modelAccess", "sharedModels", "allowed"}, []any{}},
		{[]string{"spec", "agentIdentity", "allowed"}, false},
		{[]string{"spec", "agentIdentity", "required"}, false},
		{[]string{"spec", "agentIdentity", "credentialServices"}, []any{}},
	} {
		if _, found, err := unstructured.NestedFieldNoCopy(body, entry.path...); err != nil {
			return err
		} else if !found {
			if err := unstructured.SetNestedField(body, entry.value, entry.path...); err != nil {
				return err
			}
		}
	}
	// Optional grant lists have no schema default; their empty form is omitted.
	for _, field := range []string{"roleBindings", "clusterRoleBindings"} {
		values, found, err := unstructured.NestedSlice(body, "spec", "kubernetesAccess", field)
		if err != nil {
			return err
		}
		if found && len(values) == 0 {
			unstructured.RemoveNestedField(body, "spec", "kubernetesAccess", field)
		}
	}
	// Gateway rate limits are applied to the Agent's key in place, and the
	// skill allowlist only gates which skills Teams and Agents may select.
	// Neither is an execution control, so editing them must not restart
	// native Agents. The allowlist is pinned to its empty default instead of
	// being removed, so a policy without one keeps the digest that earlier
	// operators recorded.
	unstructured.RemoveNestedField(body, "spec", "modelAccess", "rateLimits")
	return unstructured.SetNestedField(body, map[string]any{"allowed": []any{}}, "spec", "skills")
}

type executionMetadata struct {
	Owners              []metav1.OwnerReference
	Finalizers          []string
	Labels, Annotations map[string]string
}

func effectiveMetadata(object client.Object) executionMetadata {
	finalizers := slices.Clone(object.GetFinalizers())
	slices.Sort(finalizers)
	return executionMetadata{Owners: object.GetOwnerReferences(), Finalizers: finalizers,
		Labels: controlMetadata(object.GetLabels()), Annotations: controlMetadata(object.GetAnnotations())}
}

func controlMetadata(values map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range values {
		if key == "istio-injection" || key == "kubernetes.io/metadata.name" {
			result[key] = value
		}
		for _, prefix := range []string{
			"cetirun.microsoft.com/", "sandbox.cetirun.microsoft.com/", "meta.helm.sh/", "app.kubernetes.io/",
			"istio.io/", "sidecar.istio.io/", "security.istio.io/", "networking.istio.io/",
			"pod-security.kubernetes.io/", "cilium.io/", "io.cilium.",
			"azure.workload.identity/",
		} {
			if strings.HasPrefix(key, prefix) {
				result[key] = value
				break
			}
		}
	}
	return result
}

func ValidInputDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == value
}

func inputDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// TeamMutationDigest pins the entire proposed write, including metadata that
// does not affect existing executions. The mutation adapter must enforce it.
func TeamMutationDigest(team *api.Team) (string, error) {
	if team == nil {
		return "", fmt.Errorf("proposed Team is required")
	}
	return inputDigest(struct {
		Spec                api.TeamSpec
		Owners              []metav1.OwnerReference
		Finalizers          []string
		Labels, Annotations map[string]string
	}{team.Spec, team.OwnerReferences, team.Finalizers, team.Labels, team.Annotations})
}
