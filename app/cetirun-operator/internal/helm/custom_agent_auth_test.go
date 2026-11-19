// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
)

func TestCustomRequiresGatewayAuthentication(t *testing.T) {
	for _, port := range []any{nil, 8080} {
		input := customInput(port)
		b := input["customBindings"].(runtimebindings.Custom)
		b.Model = runtimebindings.ModelAccess{}
		b.Network.Gateway = runtimebindings.Peer{}
		b.Network.GatewayPort = 0
		input["customBindings"] = b
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatalf("image-owned authentication was accepted: %v", err)
		}
	}
}
