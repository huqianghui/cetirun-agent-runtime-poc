// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	corev1 "k8s.io/api/core/v1"
)

func TestLegacyHeldHermesHarnessKeepsProfileAndSeal(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	prepared := f.installer.prepared
	profile := prepared.Values["runtime"].(map[string]any)["profileID"]
	if profile != f.installer.chart.Metadata.Version+"/hermes/0.21.5" || prepared.Agent.Port == nil || *prepared.Agent.Port != 8642 {
		t.Fatal("held fixture lost its historical Hermes profile")
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	ports := template.Spec.Containers[0].Ports
	if !slices.Contains(ports, corev1.ContainerPort{Name: "dashboard", ContainerPort: 9119, Protocol: corev1.ProtocolTCP}) ||
		slices.ContainsFunc(ports, func(port corev1.ContainerPort) bool { return port.Name == "webapp" }) {
		t.Fatal("held fixture lost its historical Dashboard listener")
	}
	if _, err := helm.PrepareChart(f.installer.chart, map[string]any{
		"agentType": "hermes", "agentRequest": map[string]any{"type": "hermes"},
	}); err == nil || !strings.Contains(err.Error(), "chart lacks a supported hermes runtime profile") {
		t.Fatalf("default preparation must reject the historical profile: %v", err)
	}
	*prepared.Agent.Port = 8080
	if _, err := prepared.SandboxManifest(); err == nil {
		t.Fatal("held manifest accepted a changed runtime after sealing")
	}
}
