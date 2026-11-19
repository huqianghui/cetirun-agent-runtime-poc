// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

func nativePlatformBranches(t *testing.T, directory string) map[string]map[string]string {
	t.Helper()
	results := map[string]map[string]string{}
	for _, typ := range []string{"openclaw", "hermes"} {
		for _, desktop := range []bool{false, true} {
			for _, terminal := range []bool{false, true} {
				for _, mesh := range []string{"none", "v1", "v1beta1"} {
					for _, serviceAccount := range []bool{false, true} {
						ch, err := loader.Load(directory)
						if err != nil {
							t.Fatal(err)
						}
						useNativeBaselineVersion(ch)
						input := sampleInput(typ, "openai", false)
						input["desktop"] = map[string]any{"enabled": desktop}
						input["ttyd"] = map[string]any{"enabled": terminal}
						input["serviceAccount"] = map[string]any{"create": serviceAccount}
						input["podDisruptionBudget"] = map[string]any{"enabled": true}
						input["autoscaling"] = map[string]any{"enabled": true, "minReplicas": 1, "maxReplicas": 1}
						if typ == "openclaw" && !desktop {
							input["openclaw"] = map[string]any{"config": map[string]any{"browser": map[string]any{"attachOnly": false}}}
						}
						prepared, err := PrepareChart(ch, input)
						if err != nil {
							t.Fatal(err)
						}
						caps := chartutil.DefaultCapabilities.Copy()
						if mesh != "none" {
							caps.APIVersions = append(caps.APIVersions, "networking.istio.io/"+mesh+"/Sidecar")
						}
						values, err := chartutil.ToRenderValues(ch, prepared.Values, chartutil.ReleaseOptions{
							Name: "openclaw-example", Namespace: "cetirun-example", IsInstall: true, Revision: 1,
						}, caps)
						if err != nil {
							t.Fatal(err)
						}
						rendered, err := engine.Render(ch, values)
						if err != nil {
							t.Fatal(err)
						}
						hashes := map[string]string{}
						for _, file := range []string{"networkpolicy.yaml", "sidecar.yaml", "service.yaml", "serviceaccount.yaml", "poddisruptionbudget.yaml", "hpa.yaml", "NOTES.txt"} {
							sum := sha256.Sum256([]byte(strings.TrimSpace(rendered[ch.Name()+"/templates/"+file])))
							hashes[file] = hex.EncodeToString(sum[:])
						}
						key := fmt.Sprintf("%s/desktop=%v/ttyd=%v/mesh=%s/sa=%v", typ, desktop, terminal, mesh, serviceAccount)
						results[key] = hashes
					}
				}
			}
		}
	}
	return results
}

func TestNativePlatformBranchBaseline(t *testing.T) {
	const fixture = "testdata/native-platform-branches.json"
	// Regeneration is explicit and uses a clean archive of the pre-feature HEAD,
	// never the current worktree as its own expected result.
	if source := os.Getenv("CETIRUN_BASELINE_CHART"); source != "" {
		data, err := json.MarshalIndent(nativePlatformBranches(t, source), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture, append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]map[string]string
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	actual := nativePlatformBranches(t, "../../../cetirun-chart")
	for key, want := range expected {
		got := actual[key]
		// Gateway-only routing intentionally changes the Service, Sidecar and
		// NetworkPolicy. Keep the pre-feature branch baseline for the platform
		// resources that remain independent of model transport.
		for _, file := range []string{"serviceaccount.yaml", "poddisruptionbudget.yaml", "hpa.yaml"} {
			if got[file] != want[file] {
				t.Errorf("%s %s differs from the pre-feature platform baseline", key, file)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s differs from the pre-feature platform baseline", key)
		}
	}
	if len(actual) != len(expected) {
		t.Fatal("platform branch inventory changed")
	}
}
