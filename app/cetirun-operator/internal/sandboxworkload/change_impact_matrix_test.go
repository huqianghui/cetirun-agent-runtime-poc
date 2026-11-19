// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type impactChange struct {
	name, field, path string
	value             any
	neutral, rejected bool
}

type impactResource struct {
	name    string
	object  client.Object
	changes []impactChange
	outside []string
}

func impactResources() []impactResource {
	resources := api.SandboxContainerResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
		},
	}
	box := &api.Sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "selected", UID: "sandbox-uid", ResourceVersion: "1", Generation: 1,
		},
		Spec: api.SandboxSpec{
			Type: api.SandboxKata, Resources: api.SandboxResources{Agent: resources, Helpers: *resources.DeepCopy()},
			Storage: api.SandboxStorage{
				Home:    api.SandboxHomeStorage{StorageClassName: "csi-example", Size: resource.MustParse("10Gi"), AccessMode: corev1.ReadWriteOncePod},
				Scratch: api.SandboxScratchStorage{SizeLimit: resource.MustParse("64Mi")},
			},
		},
	}
	boxChanges := []impactChange{
		{field: "type", value: "container"},
		{field: "storage.home.storageClassName", value: "other-csi"},
		{field: "storage.home.size", value: "20Gi"},
		{field: "storage.home.accessMode", value: "ReadWriteOnce"},
		{field: "storage.scratch.sizeLimit", value: "128Mi"},
	}
	for _, container := range []string{"agent", "helpers"} {
		for _, bound := range []string{"requests", "limits"} {
			for index, key := range []string{"cpu", "memory", "ephemeral-storage"} {
				values := []string{"200m", "256Mi", "128Mi"}
				if bound == "limits" {
					values = []string{"1", "1Gi", "512Mi"}
				}
				field := "resources." + container + "." + bound
				boxChanges = append(boxChanges, impactChange{
					name: field + "." + key, field: field, path: field + "." + key, value: values[index],
				})
			}
		}
	}
	policy := &api.AgentPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "AgentPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "policy", UID: "policy-uid", ResourceVersion: "1", Generation: 1},
		Spec: api.AgentPolicySpec{
			Harnesses:     api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
			Skills:        api.AgentPolicySkills{Allowed: []string{"skill-example"}},
			AgentIdentity: api.AgentPolicyAgentIdentity{Allowed: true},
			Security:      api.AgentPolicySecurity{ReadOnlyRootFilesystem: ptr.To(true)},
			Network: api.AgentPolicyNetwork{
				BlockedCIDRs: []string{"203.0.113.0/24"}, BlockedFQDNs: []string{"blocked.example.com"},
				BlockedPorts: []api.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
				AllowedInternalCIDRs: []api.AgentPolicyCIDRRule{{
					CIDR: "192.0.2.0/24", Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}},
				}},
				Ingress: api.AgentPolicyIngress{Mode: api.AgentPolicyIngressAuthenticated},
			},
			KubernetesAccess: &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName:  "reader",
				RoleBindings:        []api.AgentPolicyRoleBinding{{Namespace: "example-data", ClusterRoleName: "view"}},
				ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: "view"}},
			},
		},
	}
	policyChanges := []impactChange{
		{field: "harnesses.allowed", value: []any{"openclaw", "hermes"}},
		// The skill allowlist only gates which skills Teams and Agents select;
		// the Team's selection is the execution input.
		{field: "skills.allowed", value: []any{"other-skill"}, neutral: true},
		{field: "agentIdentity.allowed", value: false},
		{field: "agentIdentity.required", value: true},
		{field: "agentIdentity.credentialServices", value: []any{"azure-resource-manager"}},
		{field: "entraID.enabled", value: true},
		{field: "sandboxSelection.allowMultiple", value: true},
		{field: "security.readOnlyRootFilesystem", value: false},
		{field: "network.blockedCIDRs", value: []any{"198.51.100.0/24"}},
		{field: "network.blockedFQDNs", value: []any{"other.example.com"}},
		{field: "network.blockedPorts[].port", path: "network.blockedPorts", value: impactPorts(9090, "TCP")},
		{field: "network.blockedPorts[].protocol", path: "network.blockedPorts", value: impactPorts(8080, "UDP")},
		{field: "network.allowedInternalCIDRs[].cidr", path: "network.allowedInternalCIDRs", value: impactCIDRs("198.51.100.0/24", 8443, "TCP")},
		{field: "network.allowedInternalCIDRs[].ports[].port", path: "network.allowedInternalCIDRs", value: impactCIDRs("192.0.2.0/24", 9443, "TCP")},
		{field: "network.allowedInternalCIDRs[].ports[].protocol", path: "network.allowedInternalCIDRs", value: impactCIDRs("192.0.2.0/24", 8443, "UDP")},
		{field: "network.ingress.mode", value: "disabled"},
		{field: "network.ingress.requireMTLS", value: true},
		{field: "kubernetesAccess.serviceAccountName", value: "other-reader"},
		{field: "kubernetesAccess.roleBindings[].namespace", path: "kubernetesAccess.roleBindings", value: []any{map[string]any{"namespace": "other-data", "clusterRoleName": "view"}}},
		{field: "kubernetesAccess.roleBindings[].clusterRoleName", path: "kubernetesAccess.roleBindings", value: []any{map[string]any{"namespace": "example-data", "clusterRoleName": "edit"}}},
		{field: "kubernetesAccess.clusterRoleBindings[].clusterRoleName", path: "kubernetesAccess.clusterRoleBindings", value: []any{map[string]any{"clusterRoleName": "edit"}}},
	}
	team := &api.Team{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "Team"},
		ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "team-uid", ResourceVersion: "1", Generation: 1},
		Spec: api.TeamSpec{
			AgentPolicyRef: "policy", SandboxRefs: []string{"selected", "spare"}, DefaultSandboxRef: "selected",
			MemberContext: api.TeamContextSpec{Role: "reader", Description: []string{"Synthetic context"}},
			Skills:        []string{"skill-a", "skill-b"},
			SkillStorage: &api.SkillStorageSpec{
				ResourceGroup: "synthetic-resources", StorageAccount: "syntheticstorage", ContainerName: "skills",
				StorageClassName: "blob-example", StorageSize: "1Gi", MountPath: "/opt/skills",
			},
		},
	}
	teamChanges := []impactChange{
		{field: "agentPolicyRef", value: "other-policy"},
		{name: "add-unselected-sandbox", field: "sandboxRefs", value: []any{"selected", "spare", "other"}, neutral: true},
		{name: "reorder-sandbox-selection", field: "sandboxRefs", value: []any{"spare", "selected"}, neutral: true},
		{name: "remove-selected-sandbox", field: "sandboxRefs", value: []any{"spare"}, rejected: true},
		{field: "defaultSandboxRef", value: "spare", neutral: true},
		{field: "memberContext.role", value: "writer"},
		{field: "memberContext.description", value: []any{"Changed synthetic context"}},
		{field: "skills", value: []any{"other-skill"}},
		{name: "reorder-skills", field: "skills", value: []any{"skill-b", "skill-a"}, neutral: true},
		{field: "skillStorage.resourceGroup", value: "other-synthetic-resources"},
		{field: "skillStorage.storageAccount", value: "othersyntheticstorage"},
		{field: "skillStorage.containerName", value: "other-skills"},
		{field: "skillStorage.storageClassName", value: "other-blob"},
		{field: "skillStorage.storageSize", value: "2Gi"},
		{field: "skillStorage.mountPath", value: "/opt/other-skills"},
	}
	return []impactResource{
		{name: "Sandbox", object: box, changes: boxChanges},
		{name: "AgentPolicy", object: policy, changes: policyChanges, outside: []string{"modelAccess"}},
		{name: "Team", object: team, changes: teamChanges, outside: []string{"modelRouters"}},
	}
}

func impactPorts(port int64, protocol string) []any {
	return []any{map[string]any{"port": port, "protocol": protocol}}
}

func impactCIDRs(cidr string, port int64, protocol string) []any {
	return []any{map[string]any{"cidr": cidr, "ports": impactPorts(port, protocol)}}
}

func TestSupportedChangeImpactMatrix(t *testing.T) {
	for _, resource := range impactResources() {
		t.Run(resource.name, func(t *testing.T) {
			baseline, err := ExecutionSourceDigest(resource.object, "selected")
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range resource.changes {
				name := change.name
				if name == "" {
					name = change.field
				}
				t.Run(name, func(t *testing.T) {
					body, err := runtime.DefaultUnstructuredConverter.ToUnstructured(resource.object)
					if err != nil {
						t.Fatal(err)
					}
					path := change.path
					if path == "" {
						path = change.field
					}
					if err := unstructured.SetNestedField(body, change.value, strings.Split("spec."+path, ".")...); err != nil {
						t.Fatal(err)
					}
					current := resource.object.DeepCopyObject().(client.Object)
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(body, current); err != nil {
						t.Fatal(err)
					}
					before := current.DeepCopyObject()
					actual, err := ExecutionSourceDigest(current, "selected")
					if change.rejected {
						if err == nil {
							t.Fatal("invalidated selection still has an execution digest")
						}
					} else if err != nil || (actual == baseline) != change.neutral {
						t.Fatalf("wrong execution impact: neutral=%t unchanged=%t err=%v", change.neutral, actual == baseline, err)
					}
					if !reflect.DeepEqual(before, current) {
						t.Fatal("digest calculation changed its source")
					}
					if resource.name != "Team" && !change.rejected {
						raw := &unstructured.Unstructured{Object: body}
						fromAPI, err := ExecutionSourceDigest(raw, "selected")
						if err != nil || fromAPI != actual {
							t.Fatal("typed and authoritative wire inputs disagree:", err)
						}
					}
				})
			}
		})
	}
}

func TestChangeImpactMatrixAccountsForCurrentSchema(t *testing.T) {
	for _, resource := range impactResources() {
		t.Run(resource.name, func(t *testing.T) {
			spec, ok := reflect.TypeOf(resource.object).Elem().FieldByName("Spec")
			if !ok {
				t.Fatal("resource has no spec")
			}
			actual := impactSchemaFields(t, spec.Type, "", resource.outside)
			classified := slices.Clone(resource.outside)
			for _, change := range resource.changes {
				classified = append(classified, change.field)
			}
			slices.Sort(classified)
			classified = slices.Compact(classified)
			slices.Sort(actual)
			if !slices.Equal(actual, classified) {
				t.Fatalf("classify new fields before extending delivery: schema=%v matrix=%v", actual, classified)
			}
		})
	}
}

func impactSchemaFields(t *testing.T, value reflect.Type, prefix string, outside []string) []string {
	t.Helper()
	if slices.Contains(outside, prefix) {
		return []string{prefix}
	}
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.Kind() == reflect.Slice && value.Elem().Kind() == reflect.Struct {
		return impactSchemaFields(t, value.Elem(), prefix+"[]", outside)
	}
	if value.Kind() != reflect.Struct || value.PkgPath() != reflect.TypeOf(api.SandboxSpec{}).PkgPath() {
		return []string{prefix}
	}
	var fields []string
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("unclassified JSON field %s.%s", value.Name(), field.Name)
		}
		if prefix != "" {
			name = prefix + "." + name
		}
		fields = append(fields, impactSchemaFields(t, field.Type, name, outside)...)
	}
	return fields
}
