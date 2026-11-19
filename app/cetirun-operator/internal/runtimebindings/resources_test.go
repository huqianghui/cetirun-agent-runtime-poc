// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"encoding/json"
	"testing"
)

func TestResourcePreflight(t *testing.T) {
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"requests":{"cpu":"8"},"limits":{"cpu":"1"}}`, false},
		{`{"requests":{"cpu":"2000m"},"limits":{"cpu":"1"}}`, false},
		{`{"requests":{"memory":"2Gi"},"limits":{"memory":"1024Mi"}}`, false},
		{`{"requests":{"cpu":"-1"}}`, false},
		{`{"requests":{"cpu":"0.0001"}}`, false},
		{`{"requests":{"cpu":"not-a-quantity"}}`, false},
		{`{"claims":[{"name":"unapproved"}]}`, false},
		{`{"requests":{"unknown":"1"}}`, false},
		{`{"requests":{"example.com/gpu":"0.5"}}`, false},
		{`{"requests":{"example.com/gpu":"1"}}`, false},
		{`{"requests":{"hugepages-2Mi":"4Mi"}}`, false},
		{`{"requests":{"hugepages-2Mi":"3Mi"},"limits":{"hugepages-2Mi":"3Mi"}}`, false},
		{`{"requests":{"hugepages-2Mi":"4Mi"},"limits":{"hugepages-2Mi":"4Mi"}}`, false},
		{`{"requests":{"cpu":"100m","hugepages-2Mi":"4Mi"},"limits":{"hugepages-2Mi":"4Mi"}}`, true},
		{`{"requests":{"requests.example.com/device":"1"},"limits":{"requests.example.com/device":"1"}}`, false},
		{`{"limits":{"example.com/gpu":"1"}}`, true},
		{`{"requests":{"cpu":"1000m","memory":"512Mi"},"limits":{"cpu":"1","memory":"1Gi"}}`, true},
		{`{"requests":{"example.com/gpu":"1"},"limits":{"example.com/gpu":"1"}}`, true},
		{`{}`, true},
	} {
		t.Run(test.body, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(test.body), &value); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateResources(value); (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}
