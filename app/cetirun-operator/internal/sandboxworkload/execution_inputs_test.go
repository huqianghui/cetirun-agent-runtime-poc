// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestExecutionDigestSeparatesInputsFromObservations(t *testing.T) {
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input", Namespace: "example", UID: "input-uid", ResourceVersion: "1",
		OwnerReferences: []metav1.OwnerReference{AgentOwner("agent", "agent-uid")}},
		Data: map[string]string{"input": "synthetic"}}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		t.Fatal(err)
	}
	current := &unstructured.Unstructured{Object: raw}
	before := current.DeepCopy()
	first, err := ExecutionObjectDigest(current)
	if err != nil || !reflect.DeepEqual(current, before) {
		t.Fatal("digest mutated the unstructured source:", err)
	}
	current.SetResourceVersion("2")
	current.SetLabels(map[string]string{"example.com/unrelated": "changed"})
	current.SetAnnotations(map[string]string{"example.com/unrelated": "changed"})
	current.Object["status"] = map[string]any{"observedGeneration": int64(3)}
	second, err := ExecutionObjectDigest(current)
	if err != nil || first != second {
		t.Fatal("observation metadata changed effective digest:", err)
	}
	for _, mutation := range []string{"data", "owner", "control annotation", "control label", "deletion", "lost"} {
		t.Run(mutation, func(t *testing.T) {
			changed := current.DeepCopy()
			switch mutation {
			case "data":
				changed.Object["data"] = map[string]any{"input": "different"}
			case "owner":
				changed.SetOwnerReferences([]metav1.OwnerReference{AgentOwner("other", "other-uid")})
			case "control annotation":
				changed.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "other"})
			case "control label":
				changed.SetLabels(map[string]string{ProtectedNamespaceLabel: "false"})
			case "deletion":
				now := metav1.Now()
				changed.SetDeletionTimestamp(&now)
			case "lost":
				changed.Object["status"] = map[string]any{"phase": "Lost"}
			}
			got, err := ExecutionObjectDigest(changed)
			if err == nil && got == first {
				t.Fatal("effective control, ownership or lifecycle drift was ignored")
			}
		})
	}
}

func TestExecutionDigestRejectsSecretsAndMetadataOnlyEvidence(t *testing.T) {
	metadata := metav1.ObjectMeta{Name: "input", UID: "input-uid", ResourceVersion: "1"}
	if _, err := ExecutionObjectDigest(&corev1.Secret{ObjectMeta: metadata, Data: map[string][]byte{"example": []byte("synthetic")}}); err == nil {
		t.Fatal("Secret contents accepted as execution inputs")
	}
	if _, err := ExecutionObjectDigest(&metav1.PartialObjectMetadata{ObjectMeta: metadata}); err == nil {
		t.Fatal("metadata alone accepted as effective inputs")
	}
}

func executionPolicyFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.GroupVersion.String(), "kind": "AgentPolicy",
		"metadata": map[string]any{"name": "policy", "uid": "policy-uid", "resourceVersion": "1"},
		"spec": map[string]any{
			"harnesses":        map[string]any{"allowed": []any{"openclaw"}},
			"skills":           map[string]any{"allowed": []any{}},
			"modelAccess":      map[string]any{"copilot": map[string]any{"allowed": false}, "sharedModels": map[string]any{"allowed": []any{}}},
			"agentIdentity":    map[string]any{"allowed": false, "required": false, "credentialServices": []any{}},
			"sandboxSelection": map[string]any{"allowMultiple": false},
			"security":         map[string]any{"readOnlyRootFilesystem": true},
			"entraID":          map[string]any{"enabled": false},
			"network": map[string]any{
				"blockedCIDRs": []any{}, "blockedFQDNs": []any{},
				"blockedPorts": []any{}, "allowedInternalCIDRs": []any{},
				"ingress": map[string]any{"mode": "authenticated", "requireMTLS": false},
			},
		},
	}}
}

func TestAgentPolicyExecutionDigestMatchesAPIDefaults(t *testing.T) {
	for _, variant := range []string{"defaults", "explicit values", "optional grant lists"} {
		t.Run(variant, func(t *testing.T) {
			raw := executionPolicyFixture()
			if variant == "explicit values" {
				for _, path := range [][]string{
					{"sandboxSelection", "allowMultiple"}, {"entraID", "enabled"}, {"network", "ingress", "requireMTLS"},
				} {
					if err := unstructured.SetNestedField(raw.Object, true, append([]string{"spec"}, path...)...); err != nil {
						t.Fatal(err)
					}
				}
				if err := unstructured.SetNestedSlice(raw.Object, []any{"203.0.113.0/24"}, "spec", "network", "blockedCIDRs"); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "optional grant lists" {
				if err := unstructured.SetNestedField(raw.Object, map[string]any{
					"serviceAccountName": "synthetic-tools", "roleBindings": []any{},
					"clusterRoleBindings": []any{map[string]any{"clusterRoleName": "view"}},
				}, "spec", "kubernetesAccess"); err != nil {
					t.Fatal(err)
				}
				access, found, err := unstructured.NestedMap(raw.Object, "spec", "kubernetesAccess")
				if err != nil || !found || access["serviceAccountName"] != "synthetic-tools" {
					t.Fatal("optional grant-list case did not populate its intended input:", err)
				}
			}
			policy := &api.AgentPolicy{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw.Object, policy); err != nil {
				t.Fatal(err)
			}
			originalRaw, originalPolicy := raw.DeepCopy(), policy.DeepCopy()
			fromAPI, err := ExecutionObjectDigest(raw)
			if err != nil {
				t.Fatal(err)
			}
			fromTyped, err := ExecutionObjectDigest(policy)
			if err != nil || fromAPI != fromTyped {
				t.Fatal("typed and API-defaulted policy digest differ:", err)
			}
			if variant != "optional grant lists" {
				body := runtime.DeepCopyJSON(originalRaw.Object)
				for _, key := range []string{"apiVersion", "kind", "metadata", "status"} {
					delete(body, key)
				}
				originalDigest, err := inputDigest(struct {
					Body     map[string]any
					Metadata executionMetadata
				}{body, effectiveMetadata(originalRaw)})
				if err != nil || originalDigest != fromAPI {
					t.Fatal("defaulted authoritative representation changed:", err)
				}
			}
			if !reflect.DeepEqual(raw, originalRaw) || !reflect.DeepEqual(policy, originalPolicy) {
				t.Fatal("execution digest changed caller-owned content")
			}
			if err := unstructured.SetNestedField(raw.Object, true, "spec", "futureControl"); err != nil {
				t.Fatal(err)
			}
			changed, err := ExecutionObjectDigest(raw)
			if err != nil || changed == fromAPI {
				t.Fatal("unknown authoritative fields were dropped from the execution digest:", err)
			}
		})
	}
}

func TestAgentPolicyExecutionDigestPreservesExplicitControls(t *testing.T) {
	policy := executionPolicyFixture()
	initial, err := ExecutionObjectDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"sandboxSelection", "allowMultiple"}, {"entraID", "enabled"}, {"network", "ingress", "requireMTLS"},
		{"network", "blockedCIDRs"}, {"network", "blockedFQDNs"}, {"network", "blockedPorts"}, {"network", "allowedInternalCIDRs"},
	} {
		omitted := policy.DeepCopy()
		unstructured.RemoveNestedField(omitted.Object, append([]string{"spec"}, path...)...)
		got, err := ExecutionObjectDigest(omitted)
		if err != nil || got != initial {
			t.Fatal("omitting a zero-valued schema default changed the digest:", path, err)
		}
	}
	for _, change := range []struct {
		name  string
		path  []string
		value any
	}{
		{"selection", []string{"sandboxSelection", "allowMultiple"}, true},
		{"identity", []string{"entraID", "enabled"}, true},
		{"mTLS", []string{"network", "ingress", "requireMTLS"}, true},
		{"read-only root", []string{"security", "readOnlyRootFilesystem"}, false},
		{"budget", []string{"modelAccess", "gatewayBudget"}, map[string]any{"defaultMonthlyUSD": "10", "maxMonthlyUSD": "20"}},
		{"shared models", []string{"modelAccess", "sharedModels", "allowed"}, []any{"example-model"}},
		{"Copilot", []string{"modelAccess", "copilot", "allowed"}, true},
		{"unknown model control", []string{"modelAccess", "futureControl"}, true},
		{"CIDR", []string{"network", "blockedCIDRs"}, []any{"203.0.113.0/24"}},
		{"hostname", []string{"network", "blockedFQDNs"}, []any{"blocked.example.com"}},
		{"port", []string{"network", "blockedPorts"}, []any{map[string]any{"port": int64(8080), "protocol": "TCP"}}},
		{"internal access", []string{"network", "allowedInternalCIDRs"}, []any{
			map[string]any{"cidr": "203.0.113.0/24", "ports": []any{map[string]any{"port": int64(8443), "protocol": "TCP"}}},
		}},
		{"unknown control", []string{"network", "futureControl"}, true},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := policy.DeepCopy()
			if err := unstructured.SetNestedField(changed.Object, change.value, append([]string{"spec"}, change.path...)...); err != nil {
				t.Fatal(err)
			}
			got, err := ExecutionObjectDigest(changed)
			if err != nil || got == initial {
				t.Fatal("explicit authoritative control was dropped:", err)
			}
		})
	}
	malformed := policy.DeepCopy()
	if err := unstructured.SetNestedField(malformed.Object, "invalid", "spec", "network"); err != nil {
		t.Fatal(err)
	}
	if _, err := ExecutionObjectDigest(malformed); err == nil {
		t.Fatal("malformed policy was silently replaced by defaults")
	}
}

func TestAgentPolicyExecutionDigestIgnoresGatewayRateLimits(t *testing.T) {
	policy := executionPolicyFixture()
	initial, err := ExecutionObjectDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, limits := range []map[string]any{
		{"rpm": int64(60), "tpm": int64(200000), "maxParallelRequests": int64(4)},
		{"rpm": int64(30), "perModel": map[string]any{"example-model": map[string]any{"rpm": int64(10), "tpm": int64(8192)}}},
		{"perModel": map[string]any{"auto": map[string]any{"rpm": int64(20)}}},
		{},
		nil,
	} {
		current := policy.DeepCopy()
		if limits != nil {
			if err := unstructured.SetNestedMap(current.Object, limits, "spec", "modelAccess", "rateLimits"); err != nil {
				t.Fatal(err)
			}
		}
		current.SetResourceVersion("2")
		current.SetGeneration(2)
		typed := &api.AgentPolicy{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, typed); err != nil {
			t.Fatal(err)
		}
		beforeRaw, beforeTyped := current.DeepCopy(), typed.DeepCopy()
		rawDigest, err := ExecutionObjectDigest(current)
		if err != nil || rawDigest != initial {
			t.Fatalf("Gateway-only limits changed raw execution inputs: limits=%v err=%v", limits, err)
		}
		typedDigest, err := ExecutionObjectDigest(typed)
		if err != nil || typedDigest != initial {
			t.Fatalf("Gateway-only limits changed typed execution inputs: limits=%v err=%v", limits, err)
		}
		if !reflect.DeepEqual(current, beforeRaw) || !reflect.DeepEqual(typed, beforeTyped) {
			t.Fatal("digest calculation mutated the policy or discarded its Gateway limits")
		}
	}
}

func TestAgentPolicyExecutionDigestIgnoresSkillAllowlist(t *testing.T) {
	policy := executionPolicyFixture()
	initial, err := ExecutionObjectDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	// Earlier operators recorded the digest of the API body with its empty
	// allowlist; excluding the allowlist must not change that digest.
	body := runtime.DeepCopyJSON(policy.Object)
	for _, key := range []string{"apiVersion", "kind", "metadata", "status"} {
		delete(body, key)
	}
	recorded, err := inputDigest(struct {
		Body     map[string]any
		Metadata executionMetadata
	}{body, effectiveMetadata(policy)})
	if err != nil || recorded != initial {
		t.Fatal("excluding the skill allowlist changed the digest of a policy without one:", err)
	}
	for _, skills := range []map[string]any{
		{"allowed": []any{"example-skill"}},
		{"allowed": []any{"npm:@example/skill", "example-skill", "another-skill"}},
		{"allowed": []any{}},
		{},
		nil,
	} {
		current := policy.DeepCopy()
		if skills == nil {
			unstructured.RemoveNestedField(current.Object, "spec", "skills")
		} else if err := unstructured.SetNestedMap(current.Object, skills, "spec", "skills"); err != nil {
			t.Fatal(err)
		}
		current.SetResourceVersion("2")
		current.SetGeneration(2)
		typed := &api.AgentPolicy{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, typed); err != nil {
			t.Fatal(err)
		}
		beforeRaw, beforeTyped := current.DeepCopy(), typed.DeepCopy()
		rawDigest, err := ExecutionObjectDigest(current)
		if err != nil || rawDigest != initial {
			t.Fatalf("the skill allowlist changed raw execution inputs: skills=%v err=%v", skills, err)
		}
		typedDigest, err := ExecutionObjectDigest(typed)
		if err != nil || typedDigest != initial {
			t.Fatalf("the skill allowlist changed typed execution inputs: skills=%v err=%v", skills, err)
		}
		if !reflect.DeepEqual(current, beforeRaw) || !reflect.DeepEqual(typed, beforeTyped) {
			t.Fatal("digest calculation mutated the policy or discarded its skill allowlist")
		}
	}
	for _, change := range []struct {
		name  string
		path  []string
		value any
	}{
		{"read-only root", []string{"security", "readOnlyRootFilesystem"}, false},
		{"hostname", []string{"network", "blockedFQDNs"}, []any{"blocked.example.com"}},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := policy.DeepCopy()
			if err := unstructured.SetNestedSlice(changed.Object, []any{"example-skill"}, "spec", "skills", "allowed"); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(changed.Object, change.value, append([]string{"spec"}, change.path...)...); err != nil {
				t.Fatal(err)
			}
			got, err := ExecutionObjectDigest(changed)
			if err != nil || got == initial {
				t.Fatal("an execution control edited together with the skill allowlist was dropped:", err)
			}
		})
	}
}

func TestTeamEffectiveSelectionExcludesUnrelatedSlots(t *testing.T) {
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "team-uid", ResourceVersion: "1"},
		Spec: api.TeamSpec{AgentPolicyRef: "policy",
			SandboxRefs: []string{"selected", "spare"}, DefaultSandboxRef: "selected"}}
	first, err := ExecutionTeamInputs(team)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := first.DigestFor("selected")
	if err != nil {
		t.Fatal(err)
	}
	team.Spec.DefaultSandboxRef = "spare"
	team.ResourceVersion = "2"
	second, err := ExecutionTeamInputs(team)
	if err != nil {
		t.Fatal(err)
	}
	current, err := second.DigestFor("selected")
	if err != nil || current != pinned {
		t.Fatal("creation default or other class invalidated normal selection:", err)
	}
	team.Spec.AgentPolicyRef = "other-policy"
	third, err := ExecutionTeamInputs(team)
	if err != nil {
		t.Fatal(err)
	}
	current, err = third.DigestFor("selected")
	if err != nil || current == pinned {
		t.Fatal("selected policy rebind did not invalidate execution:", err)
	}
}

func TestExecutionTeamInputsIgnoreModelRouters(t *testing.T) {
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "team-uid", ResourceVersion: "1"},
		Spec: api.TeamSpec{AgentPolicyRef: "policy", SandboxRefs: []string{"selected"}, DefaultSandboxRef: "selected"}}
	baseline, err := ExecutionSourceDigest(team, "selected")
	if err != nil {
		t.Fatal(err)
	}
	router := api.TeamModelRouter{Name: "auto-team", Classifier: api.TeamModelRouterClassifier{Type: api.ModelRouterClassifierHeuristic},
		Tiers: api.TeamModelRouterTiers{Simple: "small", Medium: "large", Complex: "large"}}
	for _, routers := range [][]api.TeamModelRouter{{router}, {router, router}, nil} {
		current := team.DeepCopy()
		current.Spec.ModelRouters = routers
		current.ResourceVersion, current.Generation = "2", 2
		if len(current.Spec.ModelRouters) == 2 {
			current.Spec.ModelRouters[0].Tiers.Simple = "large"
			current.Spec.ModelRouters[1].Name = "another-router"
		}
		before := current.DeepCopy()
		digest, err := ExecutionSourceDigest(current, "selected")
		if err != nil || digest != baseline {
			t.Fatalf("Gateway-only routers changed Team execution inputs: %v", err)
		}
		if !reflect.DeepEqual(before, current) {
			t.Fatal("execution digest mutated the Team's router configuration")
		}
		current.Spec.MemberContext.Role = "changed-runtime-context"
		digest, err = ExecutionSourceDigest(current, "selected")
		if err != nil || digest == baseline {
			t.Fatal("shared runtime context stopped invalidating execution:", err)
		}
	}
}
