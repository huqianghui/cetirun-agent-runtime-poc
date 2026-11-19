// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestMemoryOptionsAndHashDoNotFollowPlatformChartVersion(t *testing.T) {
	r := &AgentReconciler{MemoryEnabled: true, MemoryRegistry: "registry.example.com/staging", ChartVersion: "1.2.0"}
	instance := &api.Agent{}
	instance.Spec.Helm.Version = "1.1.0"
	before, hash := r.memoryOptions(), r.configHash(instance, nil, "", "")
	r.ChartVersion = "1.3.0"
	if r.memoryOptions() != before || r.configHash(instance, nil, "", "") != hash {
		t.Fatal("platform default upgrade changed a pinned Agent's Memory configuration")
	}
	r.MemoryImage = "registry.example.com/memory:2.0.0"
	if r.configHash(instance, nil, "", "") == hash || r.memoryOptions().Image != r.MemoryImage {
		t.Fatal("explicit asset override must invalidate the runtime configuration")
	}
	r.MemoryImage, r.MemoryEnabled = "", false
	if r.memoryOptions().Enabled || r.configHash(instance, nil, "", "") == hash {
		t.Fatal("disabling Memory must invalidate the enabled configuration")
	}
}
