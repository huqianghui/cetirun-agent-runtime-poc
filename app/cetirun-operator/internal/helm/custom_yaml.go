// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"
	"strconv"
	"strings"

	goyaml "go.yaml.in/yaml/v2"
)

func validateYAMLKeys(document []byte) error {
	var value any
	if err := goyaml.UnmarshalStrict(document, &value); err != nil {
		return err
	}
	return validateYAMLValueKeys(value)
}

func validateYAMLValueKeys(value any) error {
	switch value := value.(type) {
	case map[any]any:
		seen := map[string]bool{}
		for key, child := range value {
			// Match sigs.k8s.io/yaml's JSON key conversion, including YAML 1.1
			// booleans and its float formatting. Original YAML key equality
			// alone does not detect e.g. 1 and "1".
			var normalized string
			switch key := key.(type) {
			case string:
				normalized = key
			case int, int64, bool:
				normalized = fmt.Sprint(key)
			case float64:
				normalized = strconv.FormatFloat(key, 'g', -1, 32)
				normalized = strings.NewReplacer("+Inf", ".inf", "-Inf", "-.inf", "NaN", ".nan").Replace(normalized)
			default:
				return fmt.Errorf("unsupported YAML field name")
			}
			if seen[normalized] {
				return fmt.Errorf("duplicate field after YAML key conversion: %q", normalized)
			}
			seen[normalized] = true
			if err := validateYAMLValueKeys(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := validateYAMLValueKeys(child); err != nil {
				return err
			}
		}
	}
	return nil
}
