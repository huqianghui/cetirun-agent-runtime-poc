// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import "testing"

func TestCustomSchedulingInputsAndChartDefaultsAreChecked(t *testing.T) {
	for _, fromDefaults := range []bool{false, true} {
		for _, invalid := range []map[string]any{
			{"runtimeClassName": "INVALID_CLASS"},
			{"runtimeClassName": 123},
			{"nodeSelector": map[string]any{"example.com/node": "value/with/slash"}},
			{"nodeSelector": map[string]any{"bad key": ""}},
			{"nodeSelector": map[string]any{"example.com/node": true}},
		} {
			ch := testChart(t)
			input := customInput(nil)
			for key, value := range invalid {
				if fromDefaults {
					ch.Values[key] = value
				} else {
					input[key] = value
				}
			}
			if _, err := prepareChart(ch, input, false); err == nil {
				t.Fatalf("invalid scheduling accepted before action preflight: defaults=%v values=%v", fromDefaults, invalid)
			}
		}
	}
}

func TestCustomSchedulingEmptyAndQualifiedValuesRemainUsable(t *testing.T) {
	for _, values := range []map[string]any{
		{"runtimeClassName": "", "nodeSelector": map[string]any{}},
		{"runtimeClassName": nil, "nodeSelector": nil},
		{"runtimeClassName": "runtime.example.com", "nodeSelector": map[string]any{"example.com/node": ""}},
	} {
		input := customInput(nil)
		for key, value := range values {
			input[key] = value
		}
		if _, err := PrepareChart(testChart(t), input); err != nil {
			t.Fatalf("valid scheduling rejected: %v: %v", values, err)
		}
	}
}
