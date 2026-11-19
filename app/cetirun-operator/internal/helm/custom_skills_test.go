// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
)

func sharedSkillValues() map[string]any {
	return map[string]any{
		"shared": true, "resourceGroup": "test-group", "storageAccount": "testskills",
		"containerName": "approved-skills", "storageClassName": "azureblob-nfs-premium",
		"storageSize": "100Gi", "protocol": "nfs", "npm": []any{},
	}
}

func TestCustomSharedSkillsPlan(t *testing.T) {
	for _, port := range []any{nil, 8080} {
		for _, protocol := range []string{"nfs", "fuse", "fuse2"} {
			for _, desktop := range []bool{true, false} {
				t.Run(fmt.Sprintf("%v/%s/desktop=%v", port, protocol, desktop), func(t *testing.T) {
					input := customInput(port)
					skills := sharedSkillValues()
					skills["protocol"] = protocol
					skills["secretName"] = "approved-fuse-credentials"
					skills["mountOptions"] = []any{"ro"}
					input["skills"], input["desktop"] = skills, map[string]any{"enabled": desktop}
					p, err := PrepareChart(testChart(t), input)
					if err != nil {
						t.Fatal(err)
					}
					objects := platformRender(t, p, chartutil.DefaultCapabilities)
					for _, kind := range []string{"PersistentVolume", "PersistentVolumeClaim", "NetworkPolicy", "ValidatingAdmissionPolicy"} {
						if objects[kind] == nil {
							t.Fatalf("missing %s", kind)
						}
					}
					var pod corev1.PodSpec
					if err := decodeObject(object(object(object(objects["StatefulSet"]["spec"])["template"])["spec"]), &pod); err != nil {
						t.Fatal(err)
					}
					main := pod.Containers[0]
					if len(main.Command) != 0 || len(pod.InitContainers) != 1 ||
						main.VolumeMounts[1].Name != "context-view" || !main.VolumeMounts[1].ReadOnly ||
						main.VolumeMounts[1].MountPath != "/home/worker/.agents" || main.VolumeMounts[1].SubPath != "current" {
						t.Fatal("Custom image contract or approved read-only view changed")
					}
					var sourceReadOnly bool
					for _, volume := range pod.Volumes {
						if volume.Name == "shared-skills" {
							sourceReadOnly = volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ReadOnly
						}
					}
					if !sourceReadOnly || pod.InitContainers[0].Image != main.Image {
						t.Fatal("shared source is writable or init does not use the Custom image")
					}
					if object(p.Values["skills"])["mountPath"] != "/home/worker/.agents/skills" {
						t.Fatal("Custom discovery path inherited the native HOME")
					}
					for _, typ := range []string{"hermes", "openclaw"} {
						nativeInput := sampleInput(typ, "openai", false)
						nativeInput["skills"] = skills
						native, err := PrepareChart(testChart(t), nativeInput)
						if err != nil {
							t.Fatal(err)
						}
						native.releaseName = p.releaseName
						nativeObjects := platformRender(t, native, chartutil.DefaultCapabilities)
						for _, kind := range []string{"PersistentVolume", "PersistentVolumeClaim"} {
							if !reflect.DeepEqual(objects[kind], nativeObjects[kind]) {
								t.Fatalf("%s and Custom differ in shared %s", typ, kind)
							}
						}
					}
				})
			}
		}
	}
}

func TestCustomSkillsValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"npm requires published material", func(v map[string]any) { v["npm"] = []any{"example-skill"} }},
		{"clawHub requires published material", func(v map[string]any) { v["clawHub"] = []any{"example-skill"} }},
		{"unknown field", func(v map[string]any) { v["command"] = []any{"sh"} }},
		{"missing source", func(v map[string]any) { v["storageAccount"] = "" }},
		{"negative size", func(v map[string]any) { v["storageSize"] = "-1Gi" }},
		{"invalid size", func(v map[string]any) { v["storageSize"] = "invalid" }},
		{"missing fuse secret", func(v map[string]any) { v["protocol"] = "fuse2" }},
		{"foreign fuse secret", func(v map[string]any) { v["protocol"], v["secretName"] = "fuse2", "other/secret" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := customInput(nil)
			skills := sharedSkillValues()
			test.edit(skills)
			input["skills"] = skills
			if _, err := PrepareChart(testChart(t), input); err == nil {
				t.Fatal("invalid skills configuration accepted")
			}
		})
	}
	input := customInput(nil)
	input["skills"] = map[string]any{"shared": false, "npm": []any{}}
	p, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	objects := platformRender(t, p, chartutil.DefaultCapabilities)
	if objects["PersistentVolume"] != nil || objects["PersistentVolumeClaim"] != nil {
		t.Fatal("disabled shared skills created storage")
	}
}

func TestCustomSkillsStringValuesRemainStrings(t *testing.T) {
	input := customInput(nil)
	skills := sharedSkillValues()
	skills["resourceGroup"], skills["storageAccount"], skills["containerName"] = "yes", "yes", "yes"
	input["skills"] = skills
	if _, err := PrepareChart(testChart(t), input); err != nil {
		t.Fatalf("YAML-like names must not change the approved storage coordinates: %v", err)
	}
}

func TestCustomSkillsTamperingIsRejected(t *testing.T) {
	for _, test := range []struct{ file, from, to string }{
		{"shared-skills-pv.yaml", "persistentVolumeReclaimPolicy: Retain", "persistentVolumeReclaimPolicy: Delete"},
		{"shared-skills-pv.yaml", "driver: blob.csi.azure.com", "driver: disk.csi.azure.com"},
		{"shared-skills-pv.yaml", "namespace: {{ .Release.Namespace }}", "namespace: other-agent"},
		{"shared-skills-pvc.yaml", `volumeName: {{ include "cetirun.sharedSkillsPVName" . }}`, "volumeName: foreign-volume"},
		{"_context.tpl", "subPath: current\n", "subPath: other\n"},
		{"_context.tpl", "mountPath: /etc/cetirun-shared-skills\n  readOnly: true", "mountPath: /etc/cetirun-shared-skills\n  readOnly: false"},
		{"agents/custom/_runtime.tpl", "claimName: {{ include \"cetirun.sharedSkillsPVCName\" . }}\n            readOnly: true", "claimName: foreign-claim\n            readOnly: true"},
		{"_context.tpl", "mountPath: /etc/cetirun-context-view", "mountPath: /home-data"},
	} {
		t.Run(test.file+"/"+test.to, func(t *testing.T) {
			ch := testChart(t)
			matched := false
			for _, file := range ch.Templates {
				if file.Name == "templates/"+test.file && strings.Contains(string(file.Data), test.from) {
					file.Data = []byte(strings.Replace(string(file.Data), test.from, test.to, 1))
					matched = true
				}
			}
			if !matched {
				t.Fatal("mutation did not match")
			}
			input := customInput(nil)
			input["skills"] = sharedSkillValues()
			if _, err := PrepareChart(ch, input); err == nil {
				t.Fatal("modified skills plan accepted")
			}
		})
	}
}

func TestCustomSkillsWorkloadHash(t *testing.T) {
	input := customInput(nil)
	skills := sharedSkillValues()
	input["skills"] = skills
	first, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	skills["containerName"] = "updated-skills"
	second, err := PrepareChart(testChart(t), input)
	if err != nil || first.WorkloadHash == second.WorkloadHash {
		t.Fatal("shared source change did not require a new context view")
	}
	skills["npmRegistry"] = "https://registry.example.com"
	skills["mountPath"] = "/unused-native-path"
	third, err := PrepareChart(testChart(t), input)
	if err != nil || second.WorkloadHash != third.WorkloadHash {
		t.Fatal("unused native installation settings changed the Custom workload")
	}
}

func TestCustomSkillsActionSealing(t *testing.T) {
	input := customInput(nil)
	input["skills"] = sharedSkillValues()
	p, err := prepareChart(testChart(t), input, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := customActionConfig()
	if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err != nil {
		t.Fatal(err)
	}
	if err := p.installOrUpgradeCustom(cfg, "cetirun-example", p.releaseName); err != nil {
		t.Fatal(err)
	}
	if applied, err := p.customApplied(cfg, "cetirun-example", p.releaseName); err != nil || !applied {
		t.Fatalf("shared-skills plan was not retained by the sealed action: %v", err)
	}
}
