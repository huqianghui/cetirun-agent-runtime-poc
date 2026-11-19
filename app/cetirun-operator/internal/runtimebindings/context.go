// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import corev1 "k8s.io/api/core/v1"

func ContextMount(home string) corev1.VolumeMount {
	return corev1.VolumeMount{Name: "context-view", MountPath: home + "/.agents", SubPath: "current", ReadOnly: true}
}

func ContextVolume() corev1.Volume {
	return corev1.Volume{Name: "context-view", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func ContextInitEnvironment(shared, memory bool) []corev1.EnvVar {
	var env []corev1.EnvVar
	if shared || memory {
		env = append(env, corev1.EnvVar{Name: "CETIRUN_CONTEXT_VIEW", Value: "/etc/cetirun-context-view"})
	}
	if shared {
		env = append(env, corev1.EnvVar{Name: "CETIRUN_SHARED_SKILLS_DIR", Value: "/etc/cetirun-shared-skills"})
	}
	if memory {
		env = append(env, corev1.EnvVar{Name: "CETIRUN_MEMORY_ASSETS", Value: MemoryAssetsDir})
	}
	return env
}

func ContextInitMounts(shared, memory bool) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	if shared || memory {
		mounts = append(mounts, corev1.VolumeMount{Name: "context-view", MountPath: "/etc/cetirun-context-view"})
	}
	if shared {
		mounts = append(mounts, corev1.VolumeMount{Name: "shared-skills", MountPath: "/etc/cetirun-shared-skills", ReadOnly: true})
	}
	if memory {
		mounts = append(mounts, MemoryMounts()...)
	}
	return mounts
}
