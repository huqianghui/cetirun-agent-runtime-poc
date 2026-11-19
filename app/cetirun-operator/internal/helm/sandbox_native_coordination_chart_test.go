// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
)

func TestNativeCoordinationChartUsesAdmissionAuthority(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "held-cleanup"
		if native {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			values := sandboxSourcesChartValues(t)
			values["sandboxSources"].(map[string]any)["nativeExecution"] = native
			values["aiGateway"] = map[string]any{"enabled": true}
			baseline, err := renderSandboxSourcesChart(t, values)
			if err != nil {
				t.Fatal(err)
			}
			previous := sandboxChartEnvironment(t, baseline, "cetirun-operator", "CETIRUN_SANDBOX_SOURCES")
			for _, key := range []string{"coordinationConfigMapName", "coordinationConfigMapUID"} {
				if _, found := previous[key]; found {
					t.Fatal("default sources serialized a native coordination setting")
				}
			}
			admission := values["sandboxAdmission"].(map[string]any)
			admission["validationMode"] = "strict"
			admission["coordinationConfigMapName"], admission["coordinationConfigMapUID"] = "shared-coordination", "shared-authority-uid"
			objects, err := renderSandboxSourcesChart(t, values)
			if err != nil {
				t.Fatal(err)
			}
			sources := sandboxChartEnvironment(t, objects, "cetirun-operator", "CETIRUN_SANDBOX_SOURCES")
			served := sandboxChartEnvironment(t, objects, "cetirun-sandbox-admission", "CETIRUN_SANDBOX_ADMISSION")
			for _, key := range []string{"coordinationConfigMapName", "coordinationConfigMapUID"} {
				if sources[key] != admission[key] || sources[key] != served[key] {
					t.Fatalf("controller and admission selected different %s", key)
				}
			}
			raw, err := json.Marshal(sources)
			if err != nil {
				t.Fatal(err)
			}
			config, err := controller.ParseSandboxSourcesConfig(string(raw))
			if err != nil || config.NativeExecution != native ||
				config.CoordinationConfigMapName != "shared-coordination" || config.CoordinationConfigMapUID != "shared-authority-uid" ||
				config.SkillsClaimName != previous["skillsClaimName"] {
				t.Fatalf("rendered source configuration is not consumable or changed old fields: %+v %v", config, err)
			}
		})
	}
}

func TestNativeCoordinationChartRejectsCompetingSourceSettings(t *testing.T) {
	for _, key := range []string{"coordinationConfigMapName", "coordinationConfigMapUID"} {
		values := sandboxSourcesChartValues(t)
		values["sandboxSources"].(map[string]any)[key] = "competing-authority"
		if _, err := renderSandboxSourcesChart(t, values); err == nil || !strings.Contains(err.Error(), "unsupported sandboxSources field") {
			t.Fatal("chart accepted a competing coordination API:", err)
		}
	}
	values := sandboxSourcesChartValues(t)
	sources, admission := values["sandboxSources"].(map[string]any), values["sandboxAdmission"].(map[string]any)
	sources["skillsCatalogConfigMapName"] = "shared-document"
	admission["validationMode"] = "strict"
	admission["coordinationConfigMapName"], admission["coordinationConfigMapUID"] = "shared-document", "authority-uid"
	if _, err := renderSandboxSourcesChart(t, values); err == nil || !strings.Contains(err.Error(), "coordination and skills catalog require separate") {
		t.Fatal("chart shared the coordination authority with skills inputs:", err)
	}
}
