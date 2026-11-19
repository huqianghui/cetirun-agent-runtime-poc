// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

func TestRegistryOnDeleteClearsAPIDefaultThroughHelmThreeWayPatch(t *testing.T) {
	prepared, err := PrepareChart(testChart(t), sampleInput("hermes", "github_copilot", false))
	if err != nil {
		t.Fatal(err)
	}
	ensureObject(prepared.Values, "runtime")["preserveRunningPod"] = true
	values, err := chartutil.ToRenderValues(prepared.chart, prepared.Values, chartutil.ReleaseOptions{Name: "example", Namespace: "example", IsUpgrade: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(prepared.chart, values)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), &manifest); err != nil {
		t.Fatal(err)
	}
	strategy := object(object(manifest["spec"])["updateStrategy"])
	desired, _ := json.Marshal(map[string]any{"spec": map[string]any{"updateStrategy": strategy}})
	original := []byte(`{"spec":{}}`)
	current := []byte(`{"spec":{"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"partition":0}}}}`)
	metadata, err := strategicpatch.NewPatchMetaFromStruct(appsv1.StatefulSet{})
	if err != nil {
		t.Fatal(err)
	}
	patch, err := strategicpatch.CreateThreeWayMergePatch(original, desired, current, metadata, true)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := strategicpatch.StrategicMergePatch(current, patch, appsv1.StatefulSet{})
	if err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := json.Unmarshal(merged, &sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType || sts.Spec.UpdateStrategy.RollingUpdate != nil {
		t.Fatal("OnDelete retained the live API default for rollingUpdate")
	}
}

func TestNativeNoopUpgradeSkipsHooksOnlyForAppliedIdenticalPlan(t *testing.T) {
	chrt := testChart(t)
	prepared, err := PrepareChart(chrt, sampleInput("hermes", "github_copilot", false))
	if err != nil {
		t.Fatal(err)
	}
	previous := &release.Release{Chart: chrt, Config: prepared.Values, Info: &release.Info{Status: release.StatusDeployed}}
	if !nativeNoopUpgrade(previous, testChart(t), prepared.Values) {
		t.Fatal("an identical retry would run a private-image upgrade hook")
	}
	changed := testChart(t)
	changed.Templates[0].Data = append([]byte("# changed template\n"), changed.Templates[0].Data...)
	if nativeNoopUpgrade(previous, changed, prepared.Values) {
		t.Fatal("a changed chart was treated as a no-op")
	}
	if nativeNoopUpgrade(previous, chrt, map[string]any{"changed": true}) {
		t.Fatal("a runtime change was treated as a no-op")
	}
	previous.Info.Status = release.StatusFailed
	if nativeNoopUpgrade(previous, chrt, prepared.Values) {
		t.Fatal("a failed unobserved plan was treated as deployed")
	}
}

func TestRegistryNormalRolloutClearsLiveOnDeleteOverride(t *testing.T) {
	prepared, err := PrepareChart(testChart(t), sampleInput("hermes", "github_copilot", false))
	if err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValues(prepared.chart, prepared.Values, chartutil.ReleaseOptions{Name: "example", Namespace: "example", IsUpgrade: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(prepared.chart, values)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := yaml.YAMLToJSON([]byte(rendered["cetirun/templates/statefulset.yaml"]))
	if err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"metadata":{"annotations":{}},"spec":{}}`)
	current := []byte(`{"metadata":{"annotations":{"cetirun.microsoft.com/registry-binding-only":"true"}},"spec":{"updateStrategy":{"type":"OnDelete"}}}`)
	metadata, err := strategicpatch.NewPatchMetaFromStruct(appsv1.StatefulSet{})
	if err != nil {
		t.Fatal(err)
	}
	patch, err := strategicpatch.CreateThreeWayMergePatch(original, desired, current, metadata, true)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := strategicpatch.StrategicMergePatch(current, patch, appsv1.StatefulSet{})
	if err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := json.Unmarshal(merged, &sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType || sts.Annotations["cetirun.microsoft.com/registry-binding-only"] == "true" {
		t.Fatal("normal rollout retained a live registry-only strategy or readiness marker")
	}
}

func TestRegistryBuiltinBindingsAndRolloutClassification(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw"} {
		t.Run(typ, func(t *testing.T) {
			chart := testChart(t)
			input := sampleInput(typ, "github_copilot", false)
			first, err := PrepareChart(chart, input)
			if err != nil {
				t.Fatal(err)
			}
			input["agentRequest"].(map[string]any)["pullSecretName"] = "registry-pull-exampleuid"
			second, err := PrepareChart(chart, input)
			if err != nil {
				t.Fatal(err)
			}
			previous := &release.Release{Chart: chart, Config: first.Values}
			if !registryBindingOnly(previous, chart, second.Values) {
				t.Fatal("adding credentials classified as a runtime change")
			}
			ensureObject(second.Values, "runtime")["preserveRunningPod"] = true
			previous.Config = second.Values
			if !registryBindingOnly(previous, chart, second.Values) {
				t.Fatal("an unchanged reconciliation would roll the deferred Pod")
			}
			if !registryBindingOnly(previous, chart, first.Values) {
				t.Fatal("removing credentials classified as a runtime change")
			}
			changedChart := testChart(t)
			changedChart.Metadata.Version = "1.2.4"
			if registryBindingOnly(previous, changedChart, second.Values) {
				t.Fatal("a chart upgrade was treated as a credential-only change")
			}
			firstTemplate := registryRenderedTemplate(t, first)
			secondTemplate := registryRenderedTemplate(t, second)
			if !reflect.DeepEqual(firstTemplate.Annotations, secondTemplate.Annotations) {
				t.Fatal("pull binding changed native Agent checksums")
			}
			if !reflect.DeepEqual(secondTemplate.Spec.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "registry-pull-exampleuid"}}) {
				t.Fatal("missing native pull binding")
			}
			input["agentRequest"].(map[string]any)["port"] = 8085
			third, err := PrepareChart(chart, input)
			if err != nil {
				t.Fatal(err)
			}
			if registryBindingOnly(previous, chart, third.Values) {
				t.Fatal("a real runtime change would be deferred")
			}
		})
	}
}

func registryRenderedTemplate(t *testing.T, prepared *PreparedRelease) corev1.PodTemplateSpec {
	t.Helper()
	values, err := chartutil.ToRenderValues(prepared.chart, prepared.Values, chartutil.ReleaseOptions{Name: "openclaw-example", Namespace: "cetirun-example", IsUpgrade: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(prepared.chart, values)
	if err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), &sts); err != nil {
		t.Fatal(err)
	}
	if object(prepared.Values["runtime"])["preserveRunningPod"] == true && (sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType || sts.Annotations["cetirun.microsoft.com/registry-binding-only"] != "true") {
		t.Fatal("pull-only template would roll the StatefulSet")
	}
	for _, document := range strings.Split(rendered["cetirun/templates/pre-upgrade-hook.yaml"], "\n---") {
		var job batchv1.Job
		if yaml.Unmarshal([]byte(document), &job) == nil && job.Kind == "Job" && !reflect.DeepEqual(job.Spec.Template.Spec.ImagePullSecrets, sts.Spec.Template.Spec.ImagePullSecrets) {
			t.Fatal("hook does not use the same pull binding as main/init")
		}
	}
	return sts.Spec.Template
}

func TestRegistryCustomBindingChangesPlanNotWriter(t *testing.T) {
	input := customInput(nil)
	first, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	bindings := input["customBindings"].(runtimebindings.Custom)
	bindings.PullSecretName = "registry-pull-exampleuid"
	input["customBindings"] = bindings
	input["agentRequest"].(map[string]any)["pullSecretName"] = bindings.PullSecretName
	second, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkloadHash != second.WorkloadHash || first.PlanHash == second.PlanHash {
		t.Fatal("Custom registry reference did not stay out of the writer hash")
	}
	if err := second.ValidateCustom("cetirun-example", "openclaw-example"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(second.Values)
	if strings.Contains(string(raw), ".dockerconfigjson") || strings.Contains(string(raw), "synthetic-token") {
		t.Fatal("secret data entered release values")
	}
	input["agentRequest"].(map[string]any)["pullSecretName"] = "foreign-secret"
	if _, err := PrepareChart(testChart(t), input); err == nil {
		t.Fatal("unapproved pull reference accepted")
	}
}

func TestRegistryRejectsPreFeatureChart(t *testing.T) {
	chart := testChart(t)
	delete(chart.Metadata.Annotations, "cetirun.microsoft.com/registry-credentials")
	input := sampleInput("hermes", "github_copilot", false)
	input["agentRequest"].(map[string]any)["pullSecretName"] = "registry-pull-example"
	if _, err := PrepareChart(chart, input); err == nil {
		t.Fatal("old chart silently ignored managed credentials")
	}
}
