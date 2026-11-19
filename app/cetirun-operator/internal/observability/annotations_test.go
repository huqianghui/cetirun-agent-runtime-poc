// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import "testing"

func TestCollectorAnnotation(t *testing.T) {
	for key, want := range map[string]bool{
		"adx-mon/scrape":          true,
		"adx-mon/log-destination": true,
		"adx-mon/log-sources":     true,
		"adx-mon/":                true,
		"example.com/adx-mon":     false,
		"adx-monitor/scrape":      false,
		"prometheus.io/scrape":    false,
		"sidecar.istio.io/inject": false,
	} {
		if got := CollectorAnnotation(key); got != want {
			t.Errorf("CollectorAnnotation(%q) = %v", key, got)
		}
	}
}
