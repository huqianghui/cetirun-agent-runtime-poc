// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
)

func TestConnectionsProofRequiresCurrentCommonExecutor(t *testing.T) {
	input := testfixture.New("openclaw").Input
	input.ConnectionsMCP = &w.ConnectionsMCP{URL: "https://connections.tools.svc.cluster.local/mcp",
		Namespace: "tools", Audience: "cetirun-connections-mcp", CASecretName: "connections-ca"}
	if _, err := w.Build(input); err == nil {
		t.Fatal("held execution accepted the native Connections contract")
	}
	input.Reconciled = true
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	input.ConnectionsMCP.Audience = "api://AzureADTokenExchange"
	if _, err := w.Build(input); err == nil {
		t.Fatal("generic Entra projection substituted for a Connections purpose proof")
	}
	if bundle.Identity().ConnectionsMCP.Audience != "cetirun-connections-mcp" {
		t.Fatal("caller mutation changed the sealed endpoint contract")
	}
	if err := w.ValidateReconciledUserValues(map[string]any{"connectionsMCP": map[string]any{"enabled": true}}, nil); err == nil {
		t.Fatal("per-Agent values overrode compiler-owned Connections inputs")
	}
}

func TestConnectionsProofCannotBeMountedByHelpers(t *testing.T) {
	contract := &w.ConnectionsMCP{URL: "https://connections.tools.svc.cluster.local/mcp",
		Namespace: "tools", Audience: "cetirun-connections-mcp", CASecretName: "connections-ca"}
	main := corev1.Container{Name: "cetirun", VolumeMounts: []corev1.VolumeMount{{
		Name: "connections-identity", MountPath: "/var/run/cetirun/connections", ReadOnly: true,
	}}}
	spec := corev1.PodSpec{Volumes: []corev1.Volume{contract.TokenVolume()}, Containers: []corev1.Container{main}}
	if err := contract.ValidatePod(spec); err != nil {
		t.Fatal(err)
	}
	always := corev1.ContainerRestartPolicyAlways
	spec.InitContainers = []corev1.Container{
		{Name: "managed-egress", RestartPolicy: &always},
		{Name: "managed-capture"},
	}
	if err := contract.ValidatePod(spec); err != nil {
		t.Fatal("init containers without a proof mount must remain subject to common admission, not a blanket rejection:", err)
	}
	spec.InitContainers = []corev1.Container{main}
	if err := contract.ValidatePod(spec); err == nil {
		t.Fatal("helper received the native proof")
	}
}
