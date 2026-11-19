// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

func TestSandboxAccessPlanRejectsReservedNativeNames(t *testing.T) {
	for _, name := range []string{"api", "ui"} {
		t.Run(name, func(t *testing.T) {
			intent := sandboxcoordination.Intent{Agent: w.ObjectIdentity{Name: name, UID: "synthetic-agent"}}
			binding := &sandboxcoordination.AccessBinding{
				Route: w.ObjectIdentity{Name: "cetirun-" + name, UID: "synthetic-route"},
				Host:  "example.com", Gateway: w.SourceReference{Namespace: "gateway-system", Name: "gateway"},
			}
			for _, phase := range []string{"dormant", "published"} {
				if _, err := accessRoute(intent, binding, phase); err == nil {
					t.Fatalf("reserved name %q accepted for %s", name, phase)
				}
			}
			if _, err := accessRoute(intent, binding, "withdrawn"); err != nil {
				t.Fatal("a reserved name prevented historical withdrawal:", err)
			}
		})
	}
}
