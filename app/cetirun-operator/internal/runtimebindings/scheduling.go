// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateScheduling checks syntax, not RuntimeClass availability or runtime
// qualification. Nil and empty values retain Helm's clearing/default behavior.
func ValidateScheduling(values map[string]any) error {
	if value := values["runtimeClassName"]; value != nil {
		name, ok := value.(string)
		if !ok || (name != "" && len(validation.IsDNS1123Subdomain(name)) != 0) {
			return fmt.Errorf("Custom runtimeClassName must be empty or a valid RFC 1123 name")
		}
	}
	validateLabel := func(key, value string) error {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("Custom nodeSelector must contain valid Kubernetes label keys and values")
		}
		return nil
	}
	switch selector := values["nodeSelector"].(type) {
	case nil:
	case map[string]string:
		for key, value := range selector {
			if err := validateLabel(key, value); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, raw := range selector {
			value, ok := raw.(string)
			if !ok {
				return fmt.Errorf("Custom nodeSelector values must be strings")
			}
			if err := validateLabel(key, value); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("Custom nodeSelector must be a label map")
	}
	return nil
}
