// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kubejson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestSandboxBindingWireFormat(t *testing.T) {
	agent := Agent{
		Spec: AgentSpec{
			Agent: HarnessSpec{Type: AgentTypeHermes},
			Team:  "example-team", SandboxRef: "example-sandbox"},
		Status: AgentStatus{Sandbox: &SandboxStatus{
			TeamUID: "team-uid", SandboxUID: "sandbox-uid", AgentPolicyUID: "policy-uid", AppliedRevision: "example-revision",
		}},
	}
	wire := apiJSONMap(t, agent)
	spec := wire["spec"].(map[string]interface{})
	if spec["team"] != "example-team" || spec["sandboxRef"] != "example-sandbox" {
		t.Fatalf("binding fields changed wire identity: %#v", spec)
	}
	if spec["agent"].(map[string]interface{})["type"] != "hermes" {
		t.Fatal("Sandbox binding must preserve the required Harness configuration")
	}
	status := wire["status"].(map[string]interface{})["sandbox"].(map[string]interface{})
	want := map[string]interface{}{
		"teamUID": "team-uid", "sandboxUID": "sandbox-uid",
		"agentPolicyUID": "policy-uid", "appliedRevision": "example-revision",
	}
	if !reflect.DeepEqual(status, want) || ConditionSandboxReady != "SandboxReady" {
		t.Fatalf("enforced binding wire format changed: %#v", status)
	}
	team := apiJSONMap(t, Team{Spec: TeamSpec{
		AgentPolicyRef: "normal-policy",
		SandboxRefs:    []string{"example-sandbox"}, DefaultSandboxRef: "example-sandbox",
	}})["spec"].(map[string]interface{})
	for _, key := range []string{"agentPolicyRef", "sandboxRefs", "defaultSandboxRef"} {
		if _, found := team[key]; !found {
			t.Errorf("Team wire format omits %s", key)
		}
	}
}

func TestSandboxAPIRejectsUnknownAndMalformedWireFields(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	decoder := serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, data := range []string{
		`{"apiVersion":"cetirun.microsoft.com/v1alpha1","kind":"Sandbox","spec":{"privileged":true}}`,
		`{"apiVersion":"cetirun.microsoft.com/v1alpha1","kind":"Sandbox","spec":{"storage":{"home":{"size":"not-a-quantity"}}}}`,
		`{"apiVersion":"cetirun.microsoft.com/v1alpha1","kind":"AgentPolicy","spec":{"security":{"privileged":true}}}`,
		`{"apiVersion":"cetirun.microsoft.com/v1alpha1","kind":"AgentPolicy","spec":{"network":{"blockedPorts":[{"port":"https"}]}}}`,
	} {
		if _, _, err := decoder.Decode([]byte(data), nil, nil); err == nil {
			t.Errorf("strict decoding accepted malformed or unknown fields: %s", data)
		}
	}
}

func TestSandboxConfigurationDeepCopyIsolation(t *testing.T) {
	quantity := resource.MustParse("1Gi")
	quantity.AsDec()
	sandbox := &Sandbox{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"example": "original"}},
		Spec: SandboxSpec{
			Resources: SandboxResources{
				Agent: SandboxContainerResources{Requests: corev1.ResourceList{corev1.ResourceMemory: quantity.DeepCopy()}},
				Helpers: SandboxContainerResources{
					Limits: corev1.ResourceList{corev1.ResourceMemory: quantity.DeepCopy()},
				},
			},
			Storage: SandboxStorage{
				Home: SandboxHomeStorage{Size: quantity.DeepCopy()}, Scratch: SandboxScratchStorage{SizeLimit: quantity.DeepCopy()},
			},
		},
		Status: SandboxConfigurationStatus{Conditions: []metav1.Condition{{Type: ConditionReady}}},
	}
	before := apiJSONMap(t, sandbox)
	copy := sandbox.DeepCopy()
	copy.Labels["example"] = "changed"
	memory := copy.Spec.Resources.Agent.Requests[corev1.ResourceMemory]
	memory.AsDec().SetUnscaled(1)
	copy.Spec.Resources.Helpers.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
	copy.Spec.Storage.Home.Size.AsDec().SetUnscaled(1)
	copy.Spec.Storage.Scratch.SizeLimit.AsDec().SetUnscaled(1)
	copy.Status.Conditions[0].Type = "Changed"
	list := &SandboxList{Items: []Sandbox{*sandbox}}
	list.DeepCopyObject().(*SandboxList).Items[0].Labels["example"] = "changed-again"
	if !reflect.DeepEqual(apiJSONMap(t, sandbox), before) {
		t.Fatal("Sandbox or SandboxList DeepCopy shares mutable state")
	}

	readOnly := true
	policy := &AgentPolicy{
		Spec: AgentPolicySpec{
			Security: AgentPolicySecurity{ReadOnlyRootFilesystem: &readOnly},
			Network: AgentPolicyNetwork{
				BlockedCIDRs: []string{"192.0.2.0/24"}, BlockedFQDNs: []string{"example.com"},
				BlockedPorts:         []AgentPolicyPort{{Port: 25}},
				AllowedInternalCIDRs: []AgentPolicyCIDRRule{{CIDR: "198.51.100.0/24", Ports: []AgentPolicyPort{{Port: 443}}}},
			},
			KubernetesAccess: &AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "example-agent", RoleBindings: []AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "reader"}},
				ClusterRoleBindings: []AgentPolicyClusterRoleBinding{{ClusterRoleName: "reader"}},
			},
		},
		Status: AgentPolicyStatus{Conditions: []metav1.Condition{{Type: ConditionAgentPolicyAccepted}}},
	}
	policyBefore := apiJSONMap(t, policy)
	policyCopy := policy.DeepCopy()
	*policyCopy.Spec.Security.ReadOnlyRootFilesystem = false
	policyCopy.Spec.Network.BlockedCIDRs[0] = "0.0.0.0/0"
	policyCopy.Spec.Network.BlockedFQDNs[0] = "changed.example.com"
	policyCopy.Spec.Network.BlockedPorts[0].Port = 26
	policyCopy.Spec.Network.AllowedInternalCIDRs[0].Ports[0].Port = 80
	policyCopy.Spec.KubernetesAccess.ServiceAccountName = "changed"
	policyCopy.Spec.KubernetesAccess.RoleBindings[0].Namespace = "changed"
	policyCopy.Spec.KubernetesAccess.ClusterRoleBindings[0].ClusterRoleName = "changed"
	policyCopy.Status.Conditions[0].Type = "Changed"
	policyList := &AgentPolicyList{Items: []AgentPolicy{*policy}}
	*policyList.DeepCopyObject().(*AgentPolicyList).Items[0].Spec.Security.ReadOnlyRootFilesystem = false
	if !reflect.DeepEqual(apiJSONMap(t, policy), policyBefore) {
		t.Fatal("AgentPolicy or AgentPolicyList DeepCopy shares mutable state")
	}
}

func TestGeneratedSchemaDefaultTypes(t *testing.T) {
	for _, plural := range []string{"agents", "teams", "sandboxes", "agentpolicies"} {
		t.Run(plural, func(t *testing.T) {
			assertSchemaDefaultTypes(t, apiContractCRD(t, plural).Spec.Versions[0].Schema.OpenAPIV3Schema)
		})
	}
}

func TestGeneratedSandboxSchemaShapes(t *testing.T) {
	sandbox := apiContractCRD(t, "sandboxes").Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := sandbox.Properties["spec"]
	requireSchemaProperties(t, spec, "resources", "storage", "type")
	if !slices.Contains(sandbox.Required, "spec") || !slices.Contains(spec.Required, "resources") || !slices.Contains(spec.Required, "storage") {
		t.Fatal("Sandbox spec, resources and storage must be required")
	}
	requireSchemaDefault(t, spec.Properties["type"], `"kata"`)
	resources := spec.Properties["resources"]
	requireSchemaProperties(t, resources, "agent", "helpers")
	for _, group := range []string{"agent", "helpers"} {
		container := resources.Properties[group]
		requireSchemaProperties(t, container, "requests", "limits")
		if len(container.XValidations) == 0 {
			t.Error("missing request/limit comparison")
		}
		for _, name := range []string{"requests", "limits"} {
			list := container.Properties[name]
			if list.MinProperties == nil || *list.MinProperties != 3 || list.MaxProperties == nil || *list.MaxProperties != 3 ||
				list.AdditionalProperties == nil || list.AdditionalProperties.Schema == nil || !list.AdditionalProperties.Schema.XIntOrString ||
				len(list.XValidations) != 2 {
				t.Fatalf("resource-list shape lost exact positive quantity constraints: %+v", list)
			}
		}
	}
	storage := spec.Properties["storage"]
	requireSchemaProperties(t, storage, "home", "scratch")
	requireSchemaProperties(t, storage.Properties["home"], "storageClassName", "size", "accessMode")
	requireSchemaProperties(t, storage.Properties["scratch"], "sizeLimit")
	requireSchemaProperties(t, sandbox.Properties["status"], "observedGeneration", "conditions")

	policy := apiContractCRD(t, "agentpolicies").Spec.Versions[0].Schema.OpenAPIV3Schema
	policySpec := policy.Properties["spec"]
	requireSchemaProperties(t, policySpec, "agentIdentity", "entraID", "harnesses", "kubernetesAccess", "modelAccess", "network", "sandboxSelection", "security", "skills")
	if !slices.Contains(policy.Required, "spec") || policySpec.Default != nil {
		t.Fatal("AgentPolicy spec must be required and must not have an incomplete object default")
	}
	for _, name := range []string{"sandboxSelection", "security", "network", "entraID"} {
		requireSchemaDefault(t, policySpec.Properties[name], `{}`)
	}
	requireSchemaDefault(t, policySpec.Properties["security"].Properties["readOnlyRootFilesystem"], `true`)
	network := policySpec.Properties["network"]
	requireSchemaProperties(t, network, "blockedCIDRs", "blockedFQDNs", "blockedPorts", "allowedInternalCIDRs", "ingress")
	for _, name := range []string{"blockedCIDRs", "blockedFQDNs", "blockedPorts", "allowedInternalCIDRs"} {
		requireSchemaDefault(t, network.Properties[name], `[]`)
	}
	requireSchemaDefault(t, network.Properties["ingress"], `{}`)
	requireSchemaDefault(t, network.Properties["ingress"].Properties["mode"], `"authenticated"`)
	requireSchemaProperties(t, network.Properties["ingress"], "mode", "requireMTLS")
	requireSchemaProperties(t, *network.Properties["blockedPorts"].Items.Schema, "port", "protocol")
	requireSchemaProperties(t, *network.Properties["allowedInternalCIDRs"].Items.Schema, "cidr", "ports")
	requireSchemaProperties(t, policySpec.Properties["kubernetesAccess"], "serviceAccountName", "roleBindings", "clusterRoleBindings")
	requireSchemaProperties(t, policySpec.Properties["harnesses"], "allowed")
	if !slices.Contains(policySpec.Properties["harnesses"].Required, "allowed") {
		t.Fatal("AgentPolicy harness allowlist must be required")
	}
	requireSchemaProperties(t, policySpec.Properties["modelAccess"], "copilot", "gatewayBudget", "rateLimits", "sharedModels")
	requireSchemaProperties(t, policySpec.Properties["modelAccess"].Properties["gatewayBudget"], "defaultMonthlyUSD", "maxMonthlyUSD")
	requireSchemaProperties(t, policySpec.Properties["modelAccess"].Properties["rateLimits"], "maxParallelRequests", "perModel", "rpm", "tpm")
	requireSchemaProperties(t, policy.Properties["status"], "observedGeneration", "conditions")

	agent := apiContractCRD(t, "agents").Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, name := range []string{"sandboxRef", "team"} {
		if _, ok := agent.Properties["spec"].Properties[name]; !ok || slices.Contains(agent.Properties["spec"].Required, name) {
			t.Fatalf("Agent binding field %s must exist but remain optional for legacy Agents", name)
		}
	}
	if _, ok := agent.Properties["spec"].Properties["teamAuthorization"]; ok {
		t.Fatal("Agent must not require a separate Team authorization record")
	}
	requireSchemaProperties(t, agent.Properties["spec"].Properties["modelAccess"], "githubCopilot", "monthlyBudgetUSD", "sharedModels")
	binding := agent.Properties["status"].Properties["sandbox"]
	requireSchemaProperties(t, binding, "teamUID", "sandboxUID", "agentPolicyUID", "appliedRevision")
	if len(binding.Required) != 4 || slices.Contains(agent.Properties["status"].Required, "sandbox") {
		t.Fatal("an enforced binding must be complete when present and omitted before enforcement")
	}
	team := apiContractCRD(t, "teams").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, name := range []string{"agentPolicyRef", "sandboxRefs", "defaultSandboxRef"} {
		if _, ok := team.Properties[name]; !ok || slices.Contains(team.Required, name) {
			t.Fatalf("Team binding field %s must exist but remain optional for legacy Teams", name)
		}
	}
	if _, ok := team.Properties["membership"]; ok {
		t.Fatal("Team selection must not require a membership allowlist")
	}

	for _, root := range []*apiextensionsv1.JSONSchemaProps{sandbox, policy} {
		assertClosedSchema(t, root)
		conditions := root.Properties["status"].Properties["conditions"]
		if conditions.XListType == nil || *conditions.XListType != "map" || !reflect.DeepEqual(conditions.XListMapKeys, []string{"type"}) {
			t.Fatal("configuration conditions must merge by type")
		}
	}
}

func TestAgentPolicyExplicitFalseWireFormat(t *testing.T) {
	readOnly := false
	policy := AgentPolicy{Spec: AgentPolicySpec{Security: AgentPolicySecurity{ReadOnlyRootFilesystem: &readOnly}}}
	security := apiJSONMap(t, policy)["spec"].(map[string]interface{})["security"].(map[string]interface{})
	if value, found := security["readOnlyRootFilesystem"]; !found || value != false {
		t.Fatal("explicit false must survive serialization rather than receive the true default")
	}
}

func TestGeneratedSandboxValidationMarkers(t *testing.T) {
	for _, tc := range []struct {
		plural   string
		path     []string
		required []string
		enum     []string
		rules    int
	}{
		{plural: "sandboxes", path: []string{"spec", "type"}, enum: []string{"kata", "gvisor", "container"}},
		{plural: "sandboxes", path: []string{"spec", "resources"}, required: []string{"agent", "helpers"}},
		{plural: "sandboxes", path: []string{"spec", "resources", "agent"}, required: []string{"requests", "limits"}, rules: 1},
		{plural: "sandboxes", path: []string{"spec", "resources", "helpers"}, required: []string{"requests", "limits"}, rules: 1},
		{plural: "sandboxes", path: []string{"spec", "storage"}, required: []string{"home", "scratch"}},
		{plural: "sandboxes", path: []string{"spec", "storage", "home"}, required: []string{"storageClassName", "size", "accessMode"}},
		{plural: "sandboxes", path: []string{"spec", "storage", "scratch"}, required: []string{"sizeLimit"}},
		{plural: "sandboxes", path: []string{"spec", "storage", "home", "size"}, rules: 1},
		{plural: "sandboxes", path: []string{"spec", "storage", "scratch", "sizeLimit"}, rules: 1},
		{plural: "sandboxes", path: []string{"spec", "storage", "home", "accessMode"}, enum: []string{"ReadWriteOncePod", "ReadWriteOnce"}},
		{plural: "agentpolicies", path: []string{"spec", "network", "ingress"}, rules: 1},
		{plural: "agentpolicies", path: []string{"spec", "network", "ingress", "mode"}, enum: []string{"authenticated", "disabled"}},
		{plural: "agentpolicies", path: []string{"spec", "kubernetesAccess"}, required: []string{"serviceAccountName"}, rules: 1},
	} {
		t.Run(tc.plural+"/"+field.NewPath(tc.path[0], tc.path[1:]...).String(), func(t *testing.T) {
			schema := *apiContractCRD(t, tc.plural).Spec.Versions[0].Schema.OpenAPIV3Schema
			for _, name := range tc.path {
				schema = schema.Properties[name]
			}
			for _, name := range tc.required {
				if !slices.Contains(schema.Required, name) {
					t.Errorf("%s is not required", name)
				}
			}
			if len(schema.XValidations) != tc.rules {
				t.Errorf("CEL rules = %d, want %d", len(schema.XValidations), tc.rules)
			}
			if len(schema.Enum) != len(tc.enum) {
				t.Fatalf("enum = %v, want %v", schema.Enum, tc.enum)
			}
			for i, value := range tc.enum {
				if string(schema.Enum[i].Raw) != `"`+value+`"` {
					t.Fatalf("enum[%d] = %s, want %s", i, schema.Enum[i].Raw, value)
				}
			}
		})
	}
	network := apiContractCRD(t, "agentpolicies").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["network"]
	port := network.Properties["blockedPorts"].Items.Schema.Properties["port"]
	if port.Minimum == nil || *port.Minimum != 1 || port.Maximum == nil || *port.Maximum != 65535 {
		t.Fatal("destination port bounds must be 1 through 65535")
	}
	protocol := network.Properties["blockedPorts"].Items.Schema.Properties["protocol"]
	requireSchemaDefault(t, protocol, `"TCP"`)
	if len(protocol.Enum) != 3 || string(protocol.Enum[0].Raw) != `"TCP"` || string(protocol.Enum[1].Raw) != `"UDP"` || string(protocol.Enum[2].Raw) != `"SCTP"` {
		t.Fatal("protocols must be limited to TCP, UDP and SCTP")
	}
	internalPorts := network.Properties["allowedInternalCIDRs"].Items.Schema.Properties["ports"]
	if internalPorts.MinItems == nil || *internalPorts.MinItems != 1 {
		t.Fatal("internal CIDR exceptions must require at least one port")
	}
}

func apiContractCRD(t *testing.T, plural string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "cetirun.microsoft.com_"+plural+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd
}

func requireSchemaProperties(t *testing.T, schema apiextensionsv1.JSONSchemaProps, want ...string) {
	t.Helper()
	got := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		got = append(got, name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("properties = %v, want %v", got, want)
	}
}

func requireSchemaDefault(t *testing.T, schema apiextensionsv1.JSONSchemaProps, want string) {
	t.Helper()
	if schema.Default == nil || string(schema.Default.Raw) != want {
		t.Fatalf("default = %v, want %s", schema.Default, want)
	}
}

func assertClosedSchema(t *testing.T, schema *apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	if schema.XPreserveUnknownFields != nil && *schema.XPreserveUnknownFields {
		t.Fatal("new typed configurations must not preserve arbitrary unknown fields")
	}
	for _, property := range schema.Properties {
		assertClosedSchema(t, &property)
	}
	if schema.Items != nil && schema.Items.Schema != nil {
		assertClosedSchema(t, schema.Items.Schema)
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil {
		assertClosedSchema(t, schema.AdditionalProperties.Schema)
	}
}

func assertSchemaDefaultTypes(t *testing.T, schema *apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	if schema.Default != nil {
		var value interface{}
		if err := json.Unmarshal(schema.Default.Raw, &value); err != nil {
			t.Fatal(err)
		}
		matches := false
		switch schema.Type {
		case "object":
			_, matches = value.(map[string]interface{})
		case "array":
			_, matches = value.([]interface{})
		case "string":
			_, matches = value.(string)
		case "boolean":
			_, matches = value.(bool)
		case "integer", "number":
			_, matches = value.(float64)
		}
		if !matches {
			t.Fatalf("default %s does not match schema type %s", schema.Default.Raw, schema.Type)
		}
		if object, ok := value.(map[string]interface{}); ok {
			for _, name := range schema.Required {
				if _, found := object[name]; !found {
					t.Fatalf("object default %s omits required field %s", schema.Default.Raw, name)
				}
			}
		}
	}
	for _, name := range schema.Required {
		if _, found := schema.Properties[name]; !found {
			t.Fatalf("required field %s has no schema", name)
		}
	}
	for _, property := range schema.Properties {
		assertSchemaDefaultTypes(t, &property)
	}
	if schema.Items != nil && schema.Items.Schema != nil {
		assertSchemaDefaultTypes(t, schema.Items.Schema)
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil {
		assertSchemaDefaultTypes(t, schema.AdditionalProperties.Schema)
	}
}

func apiJSONMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return apiDecodeJSON(t, string(data))
}

func apiDecodeJSON(t *testing.T, value string) map[string]interface{} {
	t.Helper()
	var result map[string]interface{}
	if err := kubejson.Unmarshal([]byte(value), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
