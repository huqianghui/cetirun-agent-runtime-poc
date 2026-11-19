// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// teamModelRouterSchema returns the generated schema of one router in
// spec.modelRouters, from the CRD file at path.
func teamModelRouterSchema(t *testing.T, path string) (apiextensionsv1.JSONSchemaProps, apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	list, ok := spec.Properties["modelRouters"]
	if !ok || list.Items == nil || list.Items.Schema == nil {
		t.Fatalf("%s has no spec.modelRouters list", path)
	}
	return list, *list.Items.Schema
}

// evalRule evaluates a CRD validation rule against self the way the API server
// does for these rules: a dynamic self with the Kubernetes string extensions.
func evalRule(t *testing.T, rule string, self any) bool {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), ext.Strings(ext.StringsVersion(2)))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(rule)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile %q: %v", rule, issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	value, _, err := program.Eval(map[string]any{"self": self})
	if err != nil {
		t.Fatalf("evaluate %q: %v", rule, err)
	}
	result, ok := value.Value().(bool)
	if !ok {
		t.Fatalf("rule %q returned %v", rule, value)
	}
	return result
}

func TestTeamModelRouterSchema(t *testing.T) {
	for _, path := range []string{"../../config/crd/bases/cetirun.microsoft.com_teams.yaml", "../../chart/templates/crds/cetirun.microsoft.com_teams.yaml"} {
		list, router := teamModelRouterSchema(t, path)
		if list.MaxItems == nil || *list.MaxItems != 10 || list.XListType == nil || *list.XListType != "map" ||
			!slices.Equal(list.XListMapKeys, []string{"name"}) {
			t.Fatalf("%s: modelRouters = maxItems %v, list type %v, keys %v; want a map of at most 10 keyed by name",
				path, list.MaxItems, list.XListType, list.XListMapKeys)
		}
		if !slices.Equal(router.Required, []string{"name", "tiers"}) {
			t.Fatalf("%s: router required = %v", path, router.Required)
		}

		name := router.Properties["name"]
		namePattern := regexp.MustCompile(name.Pattern)
		if name.MaxLength == nil || *name.MaxLength != 64 || len(name.XValidations) != 1 {
			t.Fatalf("%s: name schema = %+v", path, name)
		}
		for value, valid := range map[string]bool{
			"auto": true, "auto-claude": true, "Auto.v2": true, "CetiRun-auto": false, "cetirun-x": false,
			"copilot/auto": false, "my auto": false, "auto\t": false,
		} {
			got := namePattern.MatchString(value) && evalRule(t, name.XValidations[0].Rule, value)
			if got != valid {
				t.Errorf("%s: name %q valid = %v, want %v", path, value, got, valid)
			}
		}

		classifier := router.Properties["classifier"]
		if classifier.Default == nil || string(classifier.Default.Raw) != `{"type":"heuristic"}` {
			t.Fatalf("%s: classifier default = %v", path, classifier.Default)
		}
		var types []string
		for _, value := range classifier.Properties["type"].Enum {
			types = append(types, strings.Trim(string(value.Raw), `"`))
		}
		// LiteLLM's licence-limited classifier types are not offered; jev,
		// with LiteLLM's own instructions, is not limited.
		if !slices.Equal(types, []string{ModelRouterClassifierHeuristic, ModelRouterClassifierLLM, ModelRouterClassifierJev}) {
			t.Fatalf("%s: classifier types = %v", path, types)
		}
		if len(classifier.XValidations) != 1 {
			t.Fatalf("%s: classifier rules = %v", path, classifier.XValidations)
		}
		for _, tc := range []struct {
			self  map[string]any
			valid bool
		}{
			{map[string]any{"type": "heuristic"}, true},
			{map[string]any{"type": "llm", "model": "small-model"}, true},
			{map[string]any{"type": "jev", "model": "decision-model"}, true},
			{map[string]any{"type": "llm"}, false},
			{map[string]any{"type": "jev"}, false},
			{map[string]any{"type": "heuristic", "model": "small-model"}, false},
			{map[string]any{"model": "small-model"}, false},
			{map[string]any{}, true},
		} {
			if got := evalRule(t, classifier.XValidations[0].Rule, tc.self); got != tc.valid {
				t.Errorf("%s: classifier %v valid = %v, want %v", path, tc.self, got, tc.valid)
			}
		}

		tiers := router.Properties["tiers"]
		if !slices.Equal(tiers.Required, []string{"complex", "medium", "simple"}) {
			t.Fatalf("%s: tiers required = %v", path, tiers.Required)
		}
		for _, tier := range []string{"simple", "medium", "complex", "reasoning"} {
			schema := tiers.Properties[tier]
			pattern := regexp.MustCompile(schema.Pattern)
			if schema.MaxLength == nil || *schema.MaxLength != 256 || !pattern.MatchString("copilot/fast-model") ||
				pattern.MatchString("") || pattern.MatchString("a b") {
				t.Errorf("%s: tier %s schema = %+v", path, tier, schema)
			}
		}
	}
}

func TestTeamModelRouterStatusSchema(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/cetirun.microsoft.com_teams.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["modelRouters"]
	if status.XListType == nil || *status.XListType != "map" || !slices.Equal(status.XListMapKeys, []string{"name"}) {
		t.Fatalf("status.modelRouters = %+v", status)
	}
}

func TestTeamModelRouterTiers(t *testing.T) {
	tiers := TeamModelRouterTiers{Simple: "a", Medium: "b", Complex: "c"}
	if got := tiers.Models(); strings.Join(got, ",") != "a,b,c,c" {
		t.Fatalf("Models() = %v, want the complex model for reasoning", got)
	}
	tiers.Reasoning = "d"
	if got := tiers.ReasoningModel(); got != "d" {
		t.Fatalf("ReasoningModel() = %q, want d", got)
	}
}
