// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package builtin

import (
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/custom"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/hermes"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/openclaw"
)

func NewRegistry() *agents.Registry {
	registry, err := agents.NewRegistry(hermes.Harness{}, openclaw.Harness{}, custom.Harness{})
	if err != nil {
		panic(err)
	}
	return registry
}
