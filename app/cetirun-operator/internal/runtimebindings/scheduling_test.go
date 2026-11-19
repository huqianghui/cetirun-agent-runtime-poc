// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"strings"
	"testing"
)

func TestSchedulingSyntax(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		valid  bool
	}{
		{"omitted", nil, true},
		{"cleared", map[string]any{"runtimeClassName": nil, "nodeSelector": nil}, true},
		{"empty", map[string]any{"runtimeClassName": "", "nodeSelector": map[string]any{}}, true},
		{"runtime", map[string]any{"runtimeClassName": "kata-vm-isolation"}, true},
		{"dns-subdomain", map[string]any{"runtimeClassName": "runtime.example.com"}, true},
		{"empty-label", map[string]any{"nodeSelector": map[string]any{"example.com/node": ""}}, true},
		{"typed-map", map[string]any{"nodeSelector": map[string]string{"kubernetes.io/arch": "arm64"}}, true},
		{"uppercase-runtime", map[string]any{"runtimeClassName": "INVALID_CLASS"}, false},
		{"bad-runtime-type", map[string]any{"runtimeClassName": true}, false},
		{"long-runtime", map[string]any{"runtimeClassName": strings.Repeat("a", 254)}, false},
		{"bad-label-value", map[string]any{"nodeSelector": map[string]any{"example.com/node": "value/with/slash"}}, false},
		{"bad-label-key", map[string]any{"nodeSelector": map[string]any{"bad key": "value"}}, false},
		{"bad-label-type", map[string]any{"nodeSelector": map[string]any{"example.com/node": false}}, false},
		{"bad-selector-type", map[string]any{"nodeSelector": []string{"node"}}, false},
		{"long-label", map[string]any{"nodeSelector": map[string]any{"node": strings.Repeat("a", 64)}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateScheduling(test.values); (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}
