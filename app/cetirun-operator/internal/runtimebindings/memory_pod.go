// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func MemoryMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{{Name: "memory-assets", MountPath: MemoryAssetsDir, ReadOnly: true}}
}
func MemoryEnvironment(home string) []corev1.EnvVar {
	return []corev1.EnvVar{{Name: "CETIRUN_MEMORY_ASSETS", Value: MemoryAssetsDir}, {Name: "CETIRUN_MEMORY_ROOT", Value: home + "/.cetirun/memory"}}
}
func MemoryVolumes() []corev1.Volume {
	return []corev1.Volume{
		{Name: "memory-assets", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("40Mi"))}}},
	}
}
func MemoryInit(m *Memory) corev1.Container {
	return corev1.Container{Name: "memory-assets", Image: m.Image, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"/cetirun-memory", "install-assets", "/memory-assets"},
		SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](1024), RunAsGroup: ptr.To[int64](1024), RunAsNonRoot: ptr.To(true), ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("40Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}},
		VolumeMounts:    []corev1.VolumeMount{{Name: "memory-assets", MountPath: "/memory-assets"}},
	}
}
