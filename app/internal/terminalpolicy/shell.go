// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminalpolicy

import "errors"

var ErrNoShell = errors.New("没有找到终端：目标 Agent 容器中未找到 zsh、bash 或 sh")

type Shell struct {
	Name  string
	Probe []string
}

// Probe without requiring an existing sh and without sourcing an interactive
// startup file. These exact argv forms are also enforced by admission.
func ShellCandidates() []Shell {
	return []Shell{
		{Name: "zsh", Probe: []string{"zsh", "--version"}},
		{Name: "bash", Probe: []string{"bash", "--version"}},
		{Name: "sh", Probe: []string{"sh", "-c", "exit 0"}},
	}
}

func ShellCommand(shell string) ([]string, error) {
	for _, candidate := range ShellCandidates() {
		if shell == candidate.Name {
			return []string{shell, "-i"}, nil
		}
	}
	return nil, ErrNoShell
}
