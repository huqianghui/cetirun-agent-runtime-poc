// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"fmt"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/builtin"
)

// validateAgentRuntime prevents the existing chart path from ignoring fields
// whose runtime adapters have not been implemented yet.
func validateAgentRuntime(agent cetirunv1alpha1.HarnessSpec) error {
	if _, err := builtin.NewRegistry().Lookup(string(agent.Type)); err != nil {
		return err
	}
	if agent.Port != nil && (*agent.Port < 1 || *agent.Port > 65535) {
		return fmt.Errorf("spec.agent.port must be between 1 and 65535")
	}
	if agent.Image != nil && *agent.Image == "" {
		return fmt.Errorf("spec.agent.image must not be empty")
	}
	if agent.Type == cetirunv1alpha1.AgentTypeCustom {
		request := agents.Request{Type: "custom", Port: agent.Port, WebUI: agent.WebUI}
		if agent.Image != nil {
			request.Image = *agent.Image
		}
		if agent.Username != nil {
			request.Username = *agent.Username
		}
		_, err := agents.ResolveCustom(request)
		return err
	}
	if agent.WebUI {
		return fmt.Errorf("spec.agent.webUI requires a custom agent")
	}
	if agent.Username != nil && *agent.Username != cetirunv1alpha1.DefaultAgentUsername {
		return fmt.Errorf("non-default spec.agent.username requires Agent runtime support that is not implemented")
	}
	return nil
}
