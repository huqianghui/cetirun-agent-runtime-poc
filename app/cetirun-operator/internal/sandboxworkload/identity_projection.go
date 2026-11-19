// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// ValidateIdentityProjection confines compiler identity to the main Agent
// container. It validates desired rendering, not current approval, Pod admission,
// ServiceAccount ownership, federation or lifecycle recovery.
func (b *Bundle) ValidateIdentityProjection(spec corev1.PodSpec) error {
	if spec.ServiceAccountName != b.input.Controls.ServiceAccountName ||
		spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		return fmt.Errorf("runtime identity must use the selected ServiceAccount without automount")
	}
	volumes := map[string]corev1.Volume{}
	mounts := map[string]corev1.VolumeMount{}
	for _, volume := range b.input.Controls.Volumes {
		volumes[volume.Name] = volume
	}
	for _, mount := range b.input.Controls.VolumeMounts {
		mounts[mount.Name] = mount
	}
	seen := map[string]bool{}
	for _, volume := range spec.Volumes {
		expected, wanted := volumes[volume.Name]
		if wanted {
			if seen[volume.Name] || !equality.Semantic.DeepEqual(volume, expected) {
				return fmt.Errorf("runtime identity volume differs from compiler projection")
			}
			seen[volume.Name] = true
		} else if volume.Name == "sandbox-entra-exchange" || volume.Name == "sandbox-kubernetes-api" {
			return fmt.Errorf("runtime contains an unrequested identity volume")
		} else if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil && source.ServiceAccountToken.Audience == sandboxpolicy.EntraAudience {
					return fmt.Errorf("runtime contains an unapproved Entra token projection")
				}
			}
		}
	}
	if len(seen) != len(volumes) {
		return fmt.Errorf("runtime omits a compiler identity volume")
	}
	main := 0
	for group, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for _, container := range containers {
			if len(volumes) != 0 && len(container.EnvFrom) != 0 {
				return fmt.Errorf("projected runtime identity cannot inherit unbounded environment sources")
			}
			isMain := group == 0 && container.Name == "cetirun"
			if isMain {
				main++
			}
			projected := map[string]bool{}
			for _, mount := range container.VolumeMounts {
				expected, wanted := mounts[mount.Name]
				if wanted {
					if !isMain || projected[mount.Name] || !equality.Semantic.DeepEqual(mount, expected) {
						return fmt.Errorf("runtime identity mounts must be read-only and confined to the main Agent")
					}
					projected[mount.Name] = true
				} else if mount.Name == "sandbox-entra-exchange" || mount.Name == "sandbox-kubernetes-api" ||
					overlappingPaths(mount.MountPath, "/var/run/secrets/azure/tokens") ||
					overlappingPaths(mount.MountPath, "/var/run/secrets/kubernetes.io/serviceaccount") {
					return fmt.Errorf("runtime contains an unapproved identity mount")
				}
			}
			if isMain && len(projected) != len(mounts) {
				return fmt.Errorf("main Agent omits a compiler identity mount")
			}
			for _, device := range container.VolumeDevices {
				if _, wanted := volumes[device.Name]; wanted {
					return fmt.Errorf("runtime identity cannot be a volume device")
				}
			}
			environment := map[string]bool{}
			for _, env := range container.Env {
				if !strings.HasPrefix(env.Name, "AZURE_") {
					continue
				}
				if !isMain || environment[env.Name] || !slices.ContainsFunc(b.input.Controls.Environment, func(expected corev1.EnvVar) bool {
					return equality.Semantic.DeepEqual(expected, env)
				}) {
					return fmt.Errorf("runtime contains an unapproved Entra environment projection")
				}
				environment[env.Name] = true
			}
			if isMain && len(environment) != len(b.input.Controls.Environment) {
				return fmt.Errorf("main Agent omits compiler identity environment")
			}
		}
	}
	if main != 1 || len(spec.EphemeralContainers) != 0 {
		return fmt.Errorf("runtime identity requires exactly one main Agent and no ephemeral containers")
	}
	return nil
}
