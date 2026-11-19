// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"
)

func TestNativeEntraBootstrapReceiptVersionMatrix(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, test := range []struct {
			name    string
			catalog bool
			version string
			objects int
		}{
			{name: "without skills publication", version: "v3", objects: 5},
			{name: "with skills publication", catalog: true, version: "v4", objects: 7},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				ctx := context.Background()
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
					withReconciledCoordination(t), withNativeEntra(t))
				if test.catalog {
					withNativePublishedSkills(t)(f.r, f.kube)
				}
				for range 4 {
					f.reconcileOK(t)
				}
				state, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
				if err != nil {
					t.Fatal("bootstrap wrote an unreadable identity receipt:", err)
				}
				if receipt.Version != test.version || len(receipt.Objects) != test.objects ||
					(receipt.Skills != nil) != test.catalog || receipt.ServiceAccount == nil ||
					receipt.ServiceAccount.UID == "" || len(receipt.inventory()) != test.objects+1 {
					t.Fatalf("identity bootstrap did not preserve its complete inventory: %+v", receipt)
				}
				expected := reconciledExpectationForTest(t, f)
				bootstrapPins := 0
				for _, source := range expected.Sources {
					if source.Kind == "ConfigMap" && source.Namespace == state.Namespace && source.Name == state.Name {
						bootstrapPins++
						if source.UID != state.UID {
							t.Fatal("execution used a different bootstrap identity")
						}
					}
				}
				if bootstrapPins != 1 {
					t.Fatalf("execution must pin the shared bootstrap record exactly once, got %d", bootstrapPins)
				}
			})
		}
	}
}
