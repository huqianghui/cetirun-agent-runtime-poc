// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package openclaw

import "github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"

type Harness struct{}

func (Harness) Describe() agents.Descriptor {
	return agents.Descriptor{
		Type: "openclaw", ConfigFile: "openclaw.json", PortName: "gateway",
		DefaultPort: 18789, HealthPath: "/healthz", ReadinessPath: "/readyz", StartupPath: "/startupz",
		PreserveUIPrefix: true, NativeSecretKey: "OPENCLAW_GATEWAY_TOKEN",
	}
}

func (h Harness) Validate(agent agents.ResolvedAgent, bindings agents.Bindings) error {
	return agents.Validate(agent, bindings, h.Describe())
}

func (h Harness) Build(agent agents.ResolvedAgent, bindings agents.Bindings) (agents.Contribution, error) {
	return agents.Build(agent, bindings, h.Describe())
}
