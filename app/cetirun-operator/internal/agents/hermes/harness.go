// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package hermes

import "github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"

type Harness struct{}

func (Harness) Describe() agents.Descriptor {
	return agents.Descriptor{
		Type: "hermes", NativeVersion: "0.22.7", NativeCommit: "b200f5bc60e31588ba2cb60e288029c4a54ba5fa", ConfigFile: "hermes-config.yaml", PortName: "api",
		DefaultPort: 8642, WebappPort: 9120, HealthPath: "/health", ReadinessPath: "/health", StartupPath: "/health",
		NativeSecretKey: "API_SERVER_KEY", EnvironmentFile: "hermes.env",
	}
}

func (h Harness) Validate(agent agents.ResolvedAgent, bindings agents.Bindings) error {
	return agents.Validate(agent, bindings, h.Describe())
}

func (h Harness) Build(agent agents.ResolvedAgent, bindings agents.Bindings) (agents.Contribution, error) {
	return agents.Build(agent, bindings, h.Describe())
}
