// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminalpolicy

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	WorkloadLabel = "cetirun.microsoft.com/terminal"
	ExecRole      = "cetirun-terminal-exec"
)

func WorkloadName(agentWorkload string) string {
	if len(agentWorkload) <= 54 {
		return agentWorkload + "-terminal"
	}
	sum := sha256.Sum256([]byte(agentWorkload))
	return agentWorkload[:44] + "-" + hex.EncodeToString(sum[:4]) + "-terminal"
}

func AgentReaderName(agentUID string) string {
	sum := sha256.Sum256([]byte(agentUID))
	return "cetirun-terminal-" + hex.EncodeToString(sum[:16])
}
