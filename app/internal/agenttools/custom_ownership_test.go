// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools_test

import (
	"testing"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func TestCustomOwnershipNamespace(t *testing.T) {
	raw := `{"agentName":"sample","agentUID":"agent-uid","namespace":"cetirun-sample","namespaceUID":"namespace-uid","home":"ignored"}`
	receipt, err := agenttools.ParseCustomOwnershipNamespace(raw)
	if err != nil || !receipt.Matches("sample", "agent-uid", "cetirun-sample", "namespace-uid") {
		t.Fatalf("receipt = %#v, %v", receipt, err)
	}
	if receipt.Matches("sample", "new-agent", "cetirun-sample", "namespace-uid") {
		t.Fatal("receipt matched a recreated Agent")
	}
	if _, err := agenttools.ParseCustomOwnershipNamespace(`{"agentName":"sample"}`); err == nil {
		t.Fatal("incomplete receipt was accepted")
	}
	if agenttools.CustomOwnershipRecordName("agent-uid") != agenttools.CustomOwnershipRecordName("agent-uid") ||
		agenttools.CustomOwnershipRecordName("agent-uid") == agenttools.CustomOwnershipRecordName("other-agent") {
		t.Fatal("receipt name is not stable and UID-bound")
	}
}
