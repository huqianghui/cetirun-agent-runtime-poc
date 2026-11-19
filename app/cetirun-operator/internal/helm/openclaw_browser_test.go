// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func TestOpenClawBrowserDefaults(t *testing.T) {
	for _, tc := range []struct {
		name       string
		desktop    *bool
		browser    map[string]any
		attachOnly bool
		headless   bool
		wantError  bool
	}{
		{name: "default desktop", attachOnly: true},
		{name: "explicit desktop", desktop: ptr.To(true), attachOnly: true},
		{name: "without desktop", desktop: ptr.To(false), headless: true},
		{name: "null preferences with desktop", browser: map[string]any{"attachOnly": nil, "headless": nil}, attachOnly: true},
		{name: "null preferences without desktop", desktop: ptr.To(false), browser: map[string]any{"attachOnly": nil, "headless": nil}, headless: true},
		{name: "explicit false preferences", desktop: ptr.To(false), browser: map[string]any{"attachOnly": false, "headless": false}},
		{name: "explicit headless with desktop", desktop: ptr.To(true), browser: map[string]any{"attachOnly": false, "headless": true}, headless: true},
		{name: "disabled browser", desktop: ptr.To(false), browser: map[string]any{"enabled": false, "attachOnly": true}, attachOnly: true, headless: true},
		{name: "explicit attach-only without desktop", desktop: ptr.To(false), browser: map[string]any{"attachOnly": true}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chrt := testChart(t)
			input := sampleInput("openclaw", "openai", false)
			if tc.desktop != nil {
				input["desktop"] = map[string]any{"enabled": *tc.desktop}
			}
			if tc.browser != nil {
				input["openclaw"] = map[string]any{"config": map[string]any{"browser": tc.browser}}
			}
			prepared, err := PrepareChart(chrt, input)
			if err != nil {
				t.Fatal(err)
			}
			for mode, values := range map[string]map[string]any{"standalone": input, "operator": prepared.Values} {
				t.Run(mode, func(t *testing.T) {
					renderValues, err := chartutil.ToRenderValues(chrt, values,
						chartutil.ReleaseOptions{Name: "browser-example", Namespace: "example", IsInstall: true}, chartutil.DefaultCapabilities)
					if err != nil {
						t.Fatal(err)
					}
					rendered, err := engine.Render(chrt, renderValues)
					if tc.wantError {
						if err == nil || !strings.Contains(err.Error(), "desktop.enabled=false is incompatible with browser.attachOnly=true") {
							t.Fatalf("expected desktop/browser conflict, got %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					var configMap struct{ Data map[string]string }
					if err := yaml.Unmarshal([]byte(rendered[chrt.Name()+"/templates/configmap.yaml"]), &configMap); err != nil {
						t.Fatal(err)
					}
					var config struct{ Browser map[string]any }
					if err := json.Unmarshal([]byte(configMap.Data["openclaw.json"]), &config); err != nil {
						t.Fatal(err)
					}
					if config.Browser["attachOnly"] != tc.attachOnly || config.Browser["headless"] != tc.headless {
						t.Fatalf("unexpected browser mode: %v", config.Browser)
					}
					wantEnabled := true
					if enabled, ok := tc.browser["enabled"].(bool); ok {
						wantEnabled = enabled
					}
					if config.Browser["enabled"] != wantEnabled {
						t.Fatalf("browser enablement changed: %v", config.Browser)
					}
				})
			}
		})
	}
}
