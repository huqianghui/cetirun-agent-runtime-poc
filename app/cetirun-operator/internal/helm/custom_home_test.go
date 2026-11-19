// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestCustomHomeClaimTemplate(t *testing.T) {
	for _, version := range []string{"v1.24.0", "v1.30.0"} {
		t.Run(version, func(t *testing.T) {
			input := customInput(nil)
			p, err := prepareChart(testChart(t), input, false)
			if err != nil {
				t.Fatal(err)
			}
			cfg := customActionConfig()
			cfg.Capabilities.KubeVersion.Version = version
			if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err != nil {
				t.Fatal(err)
			}
			objects := platformRender(t, p, cfg.Capabilities)
			var sts appsv1.StatefulSet
			if err := decodeObject(objects["StatefulSet"], &sts); err != nil {
				t.Fatal(err)
			}
			if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Name != "data" {
				t.Fatal("missing HOME claim template")
			}
			b := input["customBindings"].(runtimebindings.Custom)
			if !b.Home.Claim.Matches(&sts.Spec.VolumeClaimTemplates[0]) {
				t.Fatal("HOME defaults differ from the approved template")
			}
			for _, volume := range sts.Spec.Template.Spec.Volumes {
				if volume.Name == "data" {
					t.Fatal("claim template was replaced by a pre-existing PVC mount")
				}
			}
			if _, ok := sts.Annotations["cetirun.microsoft.com/home-uid"]; ok {
				t.Fatal("Chart still requires a HOME PVC UID")
			}
			if version == "v1.30.0" && (sts.Spec.PersistentVolumeClaimRetentionPolicy == nil ||
				sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType ||
				sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType) {
				t.Fatal("HOME is not retained during suspension and StatefulSet deletion")
			}
			if objects["PersistentVolumeClaim"] != nil {
				t.Fatal("HOME must not require a separate PVC provisioning phase")
			}
		})
	}
}

func TestCustomHomePlanHasNoPVCIdentity(t *testing.T) {
	input := customInput(nil)
	prepared, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	home := object(object(object(prepared.Values["runtime"])["bindings"])["home"])
	if !onlyKeys(home, "name", "fsGroup", "claim") || len(home) != 3 {
		t.Fatalf("HOME plan contains PVC identity fields: %v", home)
	}
}

func TestCustomHomeRejectsMissingClaimTemplate(t *testing.T) {
	input := customInput(nil)
	b := input["customBindings"].(runtimebindings.Custom)
	b.Home.Claim = nil
	input["customBindings"] = b
	if _, err := PrepareChart(testChart(t), input); err == nil {
		t.Fatal("pre-existing HOME mount was accepted without a claim template")
	}
}

func TestCustomHomeClaimTemplateRejectsMutation(t *testing.T) {
	for _, change := range [][2]string{
		{"- {{ $b.home.claim.accessMode }}", "- ReadWriteMany"},
		{"storageClassName: {{ $b.home.claim.storageClass | quote }}", "storageClassName: foreign"},
		{"storage: {{ $b.home.claim.storageSize | quote }}", "storage: 1Gi"},
		{"whenScaled: Retain", "whenScaled: Delete"},
	} {
		t.Run(change[1], func(t *testing.T) {
			chart := testChart(t)
			for _, file := range chart.Templates {
				if file.Name == "templates/agents/custom/_runtime.tpl" {
					if !strings.Contains(string(file.Data), change[0]) {
						t.Fatal("mutation did not match")
					}
					file.Data = []byte(strings.Replace(string(file.Data), change[0], change[1], 1))
				}
			}
			p, err := prepareChart(chart, customInput(nil), false)
			if err != nil {
				t.Fatal(err)
			}
			cfg := customActionConfig()
			cfg.Capabilities.KubeVersion.Version = "v1.30.0"
			if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err == nil {
				t.Fatal("mutated HOME template was accepted")
			}
		})
	}
}

func TestCustomHomeAndSharedSkillsStaySeparate(t *testing.T) {
	input := customInput(nil)
	input["skills"] = sharedSkillValues()
	p, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	objects := platformRender(t, p, chartutil.DefaultCapabilities)
	var skills corev1.PersistentVolumeClaim
	if err := decodeObject(objects["PersistentVolumeClaim"], &skills); err != nil {
		t.Fatal(err)
	}
	if skills.Spec.StorageClassName == nil || *skills.Spec.StorageClassName != "azureblob-nfs-premium" ||
		len(skills.Spec.AccessModes) != 1 || skills.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Fatal("HOME defaults changed the shared skills PVC")
	}
}
