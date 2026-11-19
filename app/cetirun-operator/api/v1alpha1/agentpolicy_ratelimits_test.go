// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestAgentPolicyRateLimitSchema pins the generated AgentPolicy rate-limit
// schema and evaluates its CEL rules with the CEL library. The API server's
// own admission and CEL cost estimate are covered by make test-envtest.
func TestAgentPolicyRateLimitSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "cetirun.microsoft.com_agentpolicies.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	limits := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["modelAccess"].Properties["rateLimits"]
	perModel := limits.Properties["perModel"]
	if perModel.AdditionalProperties == nil || perModel.AdditionalProperties.Schema == nil {
		t.Fatal("perModel must be a map of per-model limits")
	}
	value := *perModel.AdditionalProperties.Schema
	// The Gateway stores max_parallel_requests as a 32-bit integer.
	if parallel := limits.Properties["maxParallelRequests"]; parallel.Maximum == nil || *parallel.Maximum != 2147483647 {
		t.Errorf("maxParallelRequests maximum = %v; want 2147483647", parallel.Maximum)
	}
	for name, tc := range map[string]struct {
		schema  apiextensionsv1.JSONSchemaProps
		minimum float64
	}{
		"rpm":          {limits.Properties["rpm"], 1},
		"tpm":          {limits.Properties["tpm"], 4096},
		"perModel rpm": {value.Properties["rpm"], 1},
		"perModel tpm": {value.Properties["tpm"], 4096},
	} {
		if tc.schema.Type != "integer" || tc.schema.Minimum == nil || *tc.schema.Minimum != tc.minimum || tc.schema.Maximum != nil {
			t.Errorf("%s schema = %+v; want an integer with minimum %v and no maximum", name, tc.schema, tc.minimum)
		}
	}
	if perModel.MaxProperties == nil || *perModel.MaxProperties != 64 {
		t.Errorf("perModel maxProperties = %v; want 64", perModel.MaxProperties)
	}
	if len(limits.Required) != 0 || len(value.Required) != 0 {
		t.Errorf("every rate limit must be optional: %v, %v", limits.Required, value.Required)
	}
	if len(perModel.XValidations) != 1 || len(value.XValidations) != 1 {
		t.Fatalf("perModel must carry one key rule and one value rule")
	}
	keyRule := compileRule(t, perModel.XValidations[0].Rule)
	valueRule := compileRule(t, value.XValidations[0].Rule)

	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"example-model", true},
		{"provider/example-model:latest", true},
		{strings.Repeat("m", 256), true},
		{"", false},
		{strings.Repeat("m", 257), false},
		{"example model", false},
		{"example\tmodel", false},
		{"example\nmodel", false},
		{"example\rmodel", false},
	} {
		if got := evaluateRule(t, keyRule, map[string]any{tc.model: map[string]any{"rpm": int64(1)}}); got != tc.want {
			t.Errorf("key rule for %q = %v, want %v", tc.model, got, tc.want)
		}
	}
	if !evaluateRule(t, keyRule, map[string]any{}) {
		t.Error("an empty perModel map must pass the key rule")
	}
	for _, tc := range []struct {
		value map[string]any
		want  bool
	}{
		{map[string]any{"rpm": int64(1)}, true},
		{map[string]any{"tpm": int64(4096)}, true},
		{map[string]any{"rpm": int64(1), "tpm": int64(4096)}, true},
		{map[string]any{}, false},
	} {
		if got := evaluateRule(t, valueRule, tc.value); got != tc.want {
			t.Errorf("value rule for %v = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func compileRule(t *testing.T, rule string) cel.Program {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(rule)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("rule %q does not compile: %v", rule, issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	return program
}

func evaluateRule(t *testing.T, program cel.Program, self any) bool {
	t.Helper()
	result, _, err := program.Eval(map[string]any{"self": self})
	if err != nil {
		t.Fatalf("evaluate rule: %v", err)
	}
	passed, ok := result.Value().(bool)
	return ok && passed
}
