// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

func validateCustomSkillValues(values map[string]any) error {
	if err := runtimebindings.ValidateCustomSkills(values["skills"]); err != nil {
		return err
	}
	skills := object(values["skills"])
	if skills["shared"] != true {
		return nil
	}
	for _, key := range []string{"resourceGroup", "storageAccount", "containerName", "storageClassName"} {
		if strings.TrimSpace(text(skills[key])) == "" {
			return fmt.Errorf("shared skills require skills.%s", key)
		}
	}
	if len(validation.IsDNS1123Subdomain(text(skills["storageClassName"]))) != 0 {
		return fmt.Errorf("invalid skills.storageClassName")
	}
	size, err := resource.ParseQuantity(text(skills["storageSize"]))
	if err != nil || size.Sign() <= 0 {
		return fmt.Errorf("skills.storageSize must be a positive quantity")
	}
	switch text(skills["protocol"]) {
	case "nfs":
	case "fuse", "fuse2":
		if len(validation.IsDNS1123Subdomain(text(skills["secretName"]))) != 0 {
			return fmt.Errorf("Blob Fuse skills require a valid namespace-local skills.secretName")
		}
	default:
		return fmt.Errorf("unsupported skills.protocol")
	}
	return nil
}

func (p *PreparedRelease) sharedSkillsEnabled() bool {
	return object(p.Values["skills"])["shared"] == true
}

func (p *PreparedRelease) sharedSkillsNames(namespace string) (string, string) {
	name := text(p.Values["fullnameOverride"])
	pv := name + "-" + namespace + "-shared-skills"
	pvc := name + "-shared-skills"
	return strings.TrimSuffix(pv[:min(len(pv), 253)], "-"), strings.TrimSuffix(pvc[:min(len(pvc), 63)], "-")
}

func (p *PreparedRelease) customSkillsObjects(namespace string) map[string]platformObject {
	objects := map[string]platformObject{}
	if !p.sharedSkillsEnabled() {
		return objects
	}
	skills := object(p.Values["skills"])
	pv, pvc := p.sharedSkillsNames(namespace)
	csi := map[string]any{
		"driver": "blob.csi.azure.com", "volumeHandle": pv,
		"volumeAttributes": map[string]any{
			"resourceGroup": skills["resourceGroup"], "storageAccount": skills["storageAccount"],
			"containerName": skills["containerName"], "protocol": skills["protocol"],
		},
	}
	if skills["protocol"] != "nfs" {
		csi["nodeStageSecretRef"] = map[string]any{"name": skills["secretName"], "namespace": namespace}
	}
	spec := map[string]any{
		"capacity": map[string]any{"storage": skills["storageSize"]}, "accessModes": []string{"ReadWriteMany"},
		"persistentVolumeReclaimPolicy": "Retain", "storageClassName": skills["storageClassName"],
		"claimRef": map[string]any{"namespace": namespace, "name": pvc}, "csi": csi,
	}
	if options := array(skills["mountOptions"]); len(options) != 0 {
		spec["mountOptions"] = options
	}
	objects["PersistentVolume"] = platformObject{"v1", pv, spec}
	objects["PersistentVolumeClaim"] = platformObject{"v1", pvc, map[string]any{
		"accessModes": []string{"ReadWriteMany"}, "resources": map[string]any{"requests": map[string]any{"storage": skills["storageSize"]}},
		"storageClassName": skills["storageClassName"], "volumeName": pv,
	}}
	return objects
}

func (p *PreparedRelease) customMainMounts() []corev1.VolumeMount {
	context := corev1.VolumeMount{Name: "context", MountPath: p.Agent.HomeDir + "/.agents", ReadOnly: true}
	if p.sharedSkillsEnabled() || object(p.Values["memoryContext"])["enabled"] == true {
		context = runtimebindings.ContextMount(p.Agent.HomeDir)
	}
	mounts := []corev1.VolumeMount{{Name: "data", MountPath: p.Agent.HomeDir}, context, {Name: "tmp", MountPath: "/tmp"}}
	return mounts
}

func (p *PreparedRelease) customInitMounts() []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "data", MountPath: "/home-data"}, {Name: "config", MountPath: "/etc/cetirun", ReadOnly: true},
		{Name: "context", MountPath: "/etc/cetirun-context", ReadOnly: true}, {Name: "tmp", MountPath: "/tmp"},
	}
	return append(mounts, runtimebindings.ContextInitMounts(p.sharedSkillsEnabled(), object(p.Values["memoryContext"])["enabled"] == true)...)
}

func (p *PreparedRelease) customInitEnv() []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "SOURCE_HOME", Value: p.Agent.HomeDir}, {Name: "TARGET_HOME", Value: "/home-data"},
		{Name: "CETIRUN_INIT_IMAGE", Value: p.Agent.Image},
		{Name: "CETIRUN_INIT_COMMON", Value: "/etc/cetirun/init-home-common.sh"},
		{Name: "CETIRUN_CONTEXT_DIR", Value: "/etc/cetirun-context"},
	}
	return append(env, runtimebindings.ContextInitEnvironment(p.sharedSkillsEnabled(), object(p.Values["memoryContext"])["enabled"] == true)...)
}
