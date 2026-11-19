// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

func TestHermesWebappRequiresCoordinatedRuntimeProfile(t *testing.T) {
	for _, replacements := range [][2]string{
		{"0.22.7", "0.21.5"},
		{"b200f5bc60e31588ba2cb60e288029c4a54ba5fa", strings.Repeat("0", 40)},
		{"webappPort: 9120", "webappPort: 9119"},
	} {
		chrt := testChart(t)
		for _, file := range chrt.Files {
			if file.Name == "agents/hermes/profile.yaml" {
				file.Data = []byte(strings.ReplaceAll(string(file.Data), replacements[0], replacements[1]))
			}
		}
		if _, err := PrepareChart(chrt, sampleInput("hermes", "openai", false)); err == nil {
			t.Fatal("incompatible Hermes profile was accepted")
		}
	}
}

func TestHermesReconciledAPIPortMatchesResolvedBinding(t *testing.T) {
	fixture := gatewayHeldFixture("hermes")
	fixture.Input.Reconciled = true
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput("hermes", "openai", false)
	input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	input["agentRequest"].(map[string]any)["port"] = 8080
	prepared, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	for index := range template.Spec.Containers[0].Env {
		entry := &template.Spec.Containers[0].Env[index]
		if entry.Name == "API_SERVER_PORT" {
			entry.Value = "8081"
		}
	}
	for index := range template.Spec.Containers[0].Ports {
		port := &template.Spec.Containers[0].Ports[index]
		if port.Name == "api" {
			port.ContainerPort = 8081
		}
	}
	if err := bundle.ValidateReconciledTemplate(template, prepared.Agent.Image, false, prepared.Agent.Port); err == nil {
		t.Fatal("matching Pod environment and listener ports replaced the resolved API binding")
	}
}
