// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminalpolicy

import (
	"encoding/json"
	"fmt"
	"strings"
)

func AdmissionExpression(namespace, pod, container, account, agentUID string) string {
	probes, shells := []string{}, []string{}
	for _, candidate := range ShellCandidates() {
		probe, _ := json.Marshal(candidate.Probe)
		command, _ := ShellCommand(candidate.Name)
		shell, _ := json.Marshal(command)
		probes = append(probes, "object.command == "+string(probe))
		shells = append(shells, "object.command == "+string(shell))
	}
	return fmt.Sprintf(
		`request.userInfo.username == %q && request.namespace == %q && request.name == %q && `+
			`namespaceObject != null && has(namespaceObject.metadata.annotations) && `+
			`"cetirun.microsoft.com/agent-uid" in namespaceObject.metadata.annotations && `+
			`namespaceObject.metadata.annotations["cetirun.microsoft.com/agent-uid"] == %q && `+
			`has(object.container) && object.container == %q && has(object.command) && `+
			`has(object.stdout) && object.stdout && `+
			`(((%s) && (!has(object.stdin) || !object.stdin) && (!has(object.tty) || !object.tty)) || `+
			`((%s) && has(object.stdin) && object.stdin && has(object.tty) && object.tty))`,
		"system:serviceaccount:"+namespace+":"+account, namespace, pod, agentUID, container,
		strings.Join(probes, " || "), strings.Join(shells, " || "))
}
