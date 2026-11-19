// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package custom

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
)

type Harness struct{}

var webBasePathPattern = regexp.MustCompile(`^/ui/[a-z0-9][a-z0-9-]*/$`)

func (Harness) Describe() agents.Descriptor {
	return agents.Descriptor{Type: "custom", PortName: "http", HealthPath: "/healthz", ReadinessPath: "/healthz", StartupPath: "/healthz"}
}

func (Harness) Validate(agent agents.ResolvedAgent, bindings agents.Bindings) error {
	resolved, err := agents.ResolveCustom(agents.Request{
		Type: agent.Type, Image: agent.Image, Username: agent.Username, Port: agent.Port, WebUI: agent.WebUI,
	})
	if err != nil || resolved.HomeDir != agent.HomeDir {
		return fmt.Errorf("invalid resolved custom Agent")
	}
	if agent.WebUI && (!webBasePathPattern.MatchString(bindings.WebBasePath) || bindings.WebInstance == "") {
		return fmt.Errorf("Custom Web UI requires a platform path and instance identity")
	}
	return agents.ValidatePortableBindings(bindings)
}

func (h Harness) Build(agent agents.ResolvedAgent, bindings agents.Bindings) (agents.Contribution, error) {
	if err := h.Validate(agent, bindings); err != nil {
		return agents.Contribution{}, err
	}
	c := agents.Contribution{
		Agent: agent, ConfigFiles: map[string]string{}, Ports: []agents.PortBinding{},
		Environment: []agents.EnvironmentValue{
			{Name: "HOME", Value: agent.HomeDir},
		},
	}
	c.Environment = append(c.Environment,
		agents.EnvironmentValue{Name: "OPENAI_BASE_URL", Value: bindings.Model.BaseURL},
		agents.EnvironmentValue{Name: "OPENAI_API_KEY", CredentialSlot: "model"})
	if agent.Port != nil {
		c.Environment = append(c.Environment, agents.EnvironmentValue{Name: "PORT", Value: strconv.Itoa(int(*agent.Port))})
		c.Ports = []agents.PortBinding{{Name: "http", Number: *agent.Port}}
		c.HTTP = &agents.HTTPBinding{PortName: "http", HealthPath: "/healthz", ReadinessPath: "/healthz", StartupPath: "/healthz"}
		if !agent.WebUI {
			c.HTTP.ChatPath = "/v1/chat/completions"
			c.HTTP.ModelsPath = "/v1/models"
		}
	}
	if agent.WebUI {
		c.Environment = append(c.Environment,
			agents.EnvironmentValue{Name: "CETIRUN_WEB_BASE_PATH", Value: bindings.WebBasePath},
			agents.EnvironmentValue{Name: "CETIRUN_WEB_INSTANCE", Value: bindings.WebInstance})
	}
	return c, nil
}
