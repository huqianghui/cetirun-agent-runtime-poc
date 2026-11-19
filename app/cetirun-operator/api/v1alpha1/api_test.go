// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"bytes"
	"encoding/json"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/yaml"
)

func TestAPIRegistration(t *testing.T) {
	wantVersion := schema.GroupVersion{Group: "cetirun.microsoft.com", Version: "v1alpha1"}
	if GroupVersion != wantVersion {
		t.Fatalf("GroupVersion = %v, want %v", GroupVersion, wantVersion)
	}
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for kind, object := range map[string]runtime.Object{
		"Agent": &Agent{}, "AgentList": &AgentList{},
		"Team": &Team{}, "TeamList": &TeamList{},
		"Sandbox": &Sandbox{}, "SandboxList": &SandboxList{},
		"AgentPolicy": &AgentPolicy{}, "AgentPolicyList": &AgentPolicyList{},
	} {
		t.Run(kind, func(t *testing.T) {
			kinds, unversioned, err := scheme.ObjectKinds(object)
			want := []schema.GroupVersionKind{wantVersion.WithKind(kind)}
			if err != nil || unversioned || !reflect.DeepEqual(kinds, want) {
				t.Fatalf("registration = %v, unversioned=%v, err=%v; want %v", kinds, unversioned, err, want)
			}
			created, err := scheme.New(want[0])
			if err != nil || reflect.TypeOf(created) != reflect.TypeOf(object) {
				t.Fatalf("scheme.New = %T, err=%v; want %T", created, err, object)
			}
		})
	}
	for _, version := range []string{"v1alpha1", "v1beta1"} {
		for _, kind := range []string{"OpenClawInstance", "OpenClawInstanceList", "OpenClawTeam", "OpenClawTeamList"} {
			if scheme.Recognizes(schema.GroupVersionKind{Group: GroupVersion.Group, Version: version, Kind: kind}) {
				t.Errorf("legacy type unexpectedly registered: %s/%s", version, kind)
			}
		}
	}
	for _, kind := range []string{"Agent", "AgentList", "Team", "TeamList", "Sandbox", "SandboxList", "AgentPolicy", "AgentPolicyList"} {
		if scheme.Recognizes(schema.GroupVersionKind{Group: GroupVersion.Group, Version: "v1beta1", Kind: kind}) {
			t.Errorf("unexpected beta API for %s", kind)
		}
	}
}

func TestAPIWireFormat(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	codecs := serializer.NewCodecFactory(scheme)
	for kind, object := range map[string]runtime.Object{
		"Agent": &Agent{ObjectMeta: metav1.ObjectMeta{Name: "example-agent"}},
		"AgentList": &AgentList{Items: []Agent{
			{ObjectMeta: metav1.ObjectMeta{Name: "example-agent"}},
		}},
		"Team": &Team{ObjectMeta: metav1.ObjectMeta{Name: "example-team"}},
		"TeamList": &TeamList{Items: []Team{
			{ObjectMeta: metav1.ObjectMeta{Name: "example-team"}},
		}},
		"Sandbox": &Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "example-sandbox"}},
		"SandboxList": &SandboxList{Items: []Sandbox{
			{ObjectMeta: metav1.ObjectMeta{Name: "example-sandbox"}},
		}},
		"AgentPolicy": &AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "example-policy"}},
		"AgentPolicyList": &AgentPolicyList{Items: []AgentPolicy{
			{ObjectMeta: metav1.ObjectMeta{Name: "example-policy"}},
		}},
	} {
		t.Run(kind, func(t *testing.T) {
			encoded, err := runtime.Encode(codecs.LegacyCodec(GroupVersion), object)
			if err != nil {
				t.Fatal(err)
			}
			var wire metav1.TypeMeta
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.APIVersion != "cetirun.microsoft.com/v1alpha1" || wire.Kind != kind {
				t.Fatalf("unexpected wire identity: %+v", wire)
			}
			decoded, gvk, err := codecs.UniversalDeserializer().Decode(encoded, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if *gvk != GroupVersion.WithKind(kind) || reflect.TypeOf(decoded) != reflect.TypeOf(object) {
				t.Fatalf("decoded %T with GVK %v, want %T and %s", decoded, gvk, object, kind)
			}
		})
	}
}

func TestGeneratedCRDContract(t *testing.T) {
	directory := filepath.Join("..", "..", "config", "crd", "bases")
	files, err := filepath.Glob(filepath.Join(directory, "*.yaml"))
	if err != nil || len(files) != 4 {
		t.Fatalf("CRD inventory: %v, err=%v; want Agent, Team, Sandbox and AgentPolicy", files, err)
	}
	chartDirectory := filepath.Join("..", "..", "chart", "templates", "crds")
	chartFiles, err := filepath.Glob(filepath.Join(chartDirectory, "*.yaml"))
	if err != nil || len(chartFiles) != 4 {
		t.Fatalf("packaged CRD inventory: %v, err=%v; want Agent, Team, Sandbox and AgentPolicy", chartFiles, err)
	}
	if legacy, err := filepath.Glob(filepath.Join("..", "..", "chart", "crds", "*.yaml")); err != nil || len(legacy) != 0 {
		t.Fatalf("CRDs must not also be installed outside the Helm manifest: %v, %v", legacy, err)
	}
	for _, tc := range []struct{ kind, plural, singular, short string }{
		{"Agent", "agents", "agent", "cra"},
		{"Team", "teams", "team", "crt"},
		{"Sandbox", "sandboxes", "sandbox", "crs"},
		{"AgentPolicy", "agentpolicies", "agentpolicy", "crp"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			name := tc.plural + ".cetirun.microsoft.com"
			data, err := os.ReadFile(filepath.Join(directory, "cetirun.microsoft.com_"+tc.plural+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			packaged, err := os.ReadFile(filepath.Join(chartDirectory, "cetirun.microsoft.com_"+tc.plural+".yaml"))
			if err != nil || !bytes.Equal(data, packaged) {
				t.Fatalf("packaged %s CRD differs from its generated source: %v", tc.kind, err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatal(err)
			}
			if crd.Annotations["helm.sh/resource-policy"] != "keep" || crd.Annotations["helm.sh/hook"] != "" {
				t.Fatal("CRDs must be retained normal Helm resources")
			}
			if crd.APIVersion != "apiextensions.k8s.io/v1" || crd.Kind != "CustomResourceDefinition" ||
				crd.Name != name || crd.Spec.Group != GroupVersion.Group || crd.Spec.Scope != apiextensionsv1.ClusterScoped {
				t.Fatalf("unexpected CRD identity or scope: %+v", crd)
			}
			wantNames := apiextensionsv1.CustomResourceDefinitionNames{
				Plural: tc.plural, Singular: tc.singular, Kind: tc.kind,
				ListKind: tc.kind + "List", ShortNames: []string{tc.short},
			}
			if !reflect.DeepEqual(crd.Spec.Names, wantNames) {
				t.Fatalf("names = %+v, want %+v", crd.Spec.Names, wantNames)
			}
			if len(crd.Spec.Versions) != 1 {
				t.Fatalf("versions = %+v; want only v1alpha1", crd.Spec.Versions)
			}
			version := crd.Spec.Versions[0]
			if version.Name != "v1alpha1" || !version.Served || !version.Storage {
				t.Fatalf("unexpected version flags: %+v", version)
			}
			if version.Subresources == nil || version.Subresources.Status == nil || version.Schema == nil {
				t.Fatal("CRD must retain schema and status subresource")
			}
			if crd.Annotations["controller-gen.kubebuilder.io/version"] != "v0.21.0" {
				t.Fatal("CRD was not generated with the pinned controller-gen version")
			}
		})
	}
	if Finalizer != "cetirun.microsoft.com/finalizer" {
		t.Fatalf("finalizer changed during API rename: %s", Finalizer)
	}
}

func TestDeepCopyIsolation(t *testing.T) {
	agent := &Agent{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"team": "example"}},
		Spec: AgentSpec{
			SkillOverrides: SkillOverrideSpec{Add: []string{"example-skill"}},
			Helm: HelmSpec{Values: map[string]runtime.RawExtension{
				"example": {Raw: []byte(`{"enabled":true}`)},
			}},
		},
		Status: AgentStatus{
			Conditions: []metav1.Condition{{Type: "Ready"}},
			Sandbox:    &SandboxStatus{AppliedRevision: "example-revision"},
		},
	}
	copy := agent.DeepCopy()
	copy.Labels["team"] = "other"
	copy.Spec.SkillOverrides.Add[0] = "other"
	copy.Spec.Helm.Values["example"].Raw[0] = 'x'
	copy.Status.Conditions[0].Type = "Other"
	copy.Status.Sandbox.AppliedRevision = "other"
	if agent.Labels["team"] != "example" || agent.Spec.SkillOverrides.Add[0] != "example-skill" ||
		string(agent.Spec.Helm.Values["example"].Raw) != `{"enabled":true}` || agent.Status.Conditions[0].Type != "Ready" ||
		agent.Status.Sandbox.AppliedRevision != "example-revision" {
		t.Fatal("Agent DeepCopy shares mutable state")
	}
	team := &Team{Spec: TeamSpec{
		Skills:       []string{"example-skill"},
		SkillStorage: &SkillStorageSpec{ContainerName: "example"},
		SandboxRefs:  []string{"example-sandbox"},
	}}
	teamCopy := team.DeepCopy()
	teamCopy.Spec.Skills[0] = "other"
	teamCopy.Spec.SkillStorage.ContainerName = "other"
	teamCopy.Spec.SandboxRefs[0] = "other"
	if team.Spec.Skills[0] != "example-skill" || team.Spec.SkillStorage.ContainerName != "example" || team.Spec.SandboxRefs[0] != "example-sandbox" {
		t.Fatal("Team DeepCopy shares mutable state")
	}
}

func TestChartRBACContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want map[string][]string
	}{
		{
			name: "operator",
			path: "../../chart/templates/clusterrole.yaml",
			want: map[string][]string{
				"agents":               {"get", "list", "watch", "create", "update", "patch", "delete"},
				"agents/status":        {"get", "update", "patch"},
				"agents/finalizers":    {"update"},
				"teams":                {"get", "list", "watch"},
				"teams/status":         {"get", "update", "patch"},
				"sandboxes":            {"get", "list", "watch"},
				"sandboxes/status":     {"get", "update", "patch"},
				"agentpolicies":        {"get", "list", "watch"},
				"agentpolicies/status": {"get", "update", "patch"},
			},
		},
		{
			name: "portal",
			path: "../../../cetirun-portal/chart/templates/clusterrole.yaml",
			want: map[string][]string{
				"agents":        {"get", "list", "create", "update", "patch", "delete"},
				"teams":         {"get", "list", "create", "update", "patch", "delete"},
				"sandboxes":     {"get", "list", "create", "update", "patch", "delete"},
				"agentpolicies": {"get", "list", "create", "update", "patch", "delete"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := loader.Load(filepath.Join(filepath.Dir(tc.path), ".."))
			if err != nil {
				t.Fatal(err)
			}
			var templates []*chart.File
			for _, file := range source.Templates {
				if file.Name == "templates/clusterrole.yaml" || strings.HasPrefix(filepath.Base(file.Name), "_") {
					templates = append(templates, file)
				}
			}
			source.Templates = templates
			for _, option := range []struct {
				name    string
				enabled bool
			}{{"default", false}, {"placement", true}} {
				t.Run(option.name, func(t *testing.T) {
					input, err := chartutil.ToRenderValues(source, map[string]any{
						"cetiadmin":      map[string]any{"enabled": false},
						"sandboxSources": map[string]any{"enabled": option.enabled, "placementConfigMapName": "sandbox-placement"},
					}, chartutil.ReleaseOptions{Name: "rbac-test", Namespace: "test", IsInstall: true}, chartutil.DefaultCapabilities)
					if err != nil {
						t.Fatal(err)
					}
					rendered, err := engine.Render(source, input)
					if err != nil {
						t.Fatal(err)
					}
					var role rbacv1.ClusterRole
					if err := yaml.Unmarshal([]byte(rendered[source.Name()+"/templates/clusterrole.yaml"]), &role); err != nil {
						t.Fatal(err)
					}
					got := map[string][]string{}
					for _, rule := range role.Rules {
						if !slices.Contains(rule.APIGroups, GroupVersion.Group) {
							continue
						}
						if len(rule.APIGroups) != 1 {
							t.Fatalf("unexpected mixed API groups: %v", rule.APIGroups)
						}
						for _, resource := range rule.Resources {
							if _, exists := got[resource]; exists {
								t.Fatalf("duplicate permission rule: %s", resource)
							}
							got[resource] = rule.Verbs
						}
					}
					if !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("RBAC resources/verbs = %v, want %v", got, tc.want)
					}
				})
			}
		})
	}
}
