// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import "testing"

// ADX-Mon reads adx-mon/ Pod annotations to choose where a Pod's logs go and
// whether to scrape it: an Agent's values cannot set them.
func TestAgentValuesCannotSetCollectorAnnotations(t *testing.T) {
	for _, key := range []string{"adx-mon/scrape", "adx-mon/log-destination", "adx-mon/log-parsers", "adx-mon/targets"} {
		for _, agentType := range []string{"openclaw", "hermes"} {
			values := map[string]any{"podAnnotations": map[string]any{key: "CetiRunLogs:Platform"}}
			if err := validateRuntimeOverrides(values, agentType); err == nil {
				t.Errorf("%s Agent values set %s", agentType, key)
			}
		}
	}
	values := map[string]any{"podAnnotations": map[string]any{"example.com/team": "a"}}
	if err := validateRuntimeOverrides(values, "openclaw"); err != nil {
		t.Fatalf("unrelated annotation rejected: %v", err)
	}
}
