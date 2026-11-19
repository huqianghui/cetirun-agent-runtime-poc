// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package testfixture

import (
	"bytes"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestPreProxyRestoresOriginalSHAAndRejectsDrift(t *testing.T) {
	for _, scenario := range []string{"original", "later addition changed", "historical field changed"} {
		t.Run(scenario, func(t *testing.T) {
			chart, err := loader.Load("../../../../cetirun-chart")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "later addition changed":
				chart.Schema = bytes.Replace(chart.Schema, []byte(`"operatorImage"`), []byte(`"unapprovedImage"`), 1)
			case "historical field changed":
				chart.Schema = bytes.Replace(chart.Schema, []byte("CetiRun Helm Chart Values Schema"), []byte("Unapproved historical schema"), 1)
			}
			err = RestorePreProxyContract(chart)
			if (err == nil) != (scenario == "original") {
				t.Fatal("fixture did not retain its exact original SHA boundary", err)
			}
		})
	}
}
