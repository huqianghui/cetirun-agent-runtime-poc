// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"
)

// Validate the generated schemas rather than only the Go markers.
func TestGeneratedPolicyHarnessSchema(t *testing.T) {
	for _, directory := range []string{"config/crd/bases", "chart/templates/crds"} {
		t.Run(directory, func(t *testing.T) {
			policy := readHarnessSchema(t, directory, "agentpolicies", "spec", "harnesses", "allowed")
			for _, test := range []struct {
				name  string
				value []any
				valid bool
			}{
				{"openclaw", []any{"openclaw"}, true},
				{"hermes", []any{"hermes"}, true},
				{"both", []any{"openclaw", "hermes"}, true},
				{"custom", []any{"custom"}, true},
				{"mixed custom", []any{"hermes", "custom"}, true},
				{"unknown", []any{"unknown"}, false},
				{"empty", []any{}, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					errs := policy.Validate(test.value).Errors
					if (len(errs) == 0) != test.valid {
						t.Fatalf("validation = %v, want valid=%v", errs, test.valid)
					}
				})
			}
			agent := readHarnessSchema(t, directory, "agents", "spec", "agent", "type")
			for _, harness := range []string{"openclaw", "hermes", "custom"} {
				if errs := agent.Validate(harness).Errors; len(errs) != 0 {
					t.Fatalf("Agent harness %s must remain valid: %v", harness, errs)
				}
			}
			if errs := agent.Validate("unknown").Errors; len(errs) == 0 {
				t.Fatal("Agent harness schema must still reject unknown values")
			}
		})
	}
}

func readHarnessSchema(t *testing.T, directory, resource string, properties ...string) *validate.SchemaValidator {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../..", directory, "cetirun.microsoft.com_"+resource+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &definition); err != nil {
		t.Fatal(err)
	}
	schema := *definition.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, name := range properties {
		var found bool
		schema, found = schema.Properties[name]
		if !found {
			t.Fatalf("missing schema property %s", name)
		}
	}
	data, err = json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var openapi spec.Schema
	if err := json.Unmarshal(data, &openapi); err != nil {
		t.Fatal(err)
	}
	return validate.NewSchemaValidator(&openapi, nil, "", strfmt.Default)
}
