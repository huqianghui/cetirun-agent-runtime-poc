// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// SkillsMode's zero value retains the existing PVC contract and wire shape.
// Empty is an internal native-only source, never a publication or claim identity.
type SkillsMode string

const SkillsModeEmpty SkillsMode = "empty"

func (mode SkillsMode) ValidateIdentity(claim ObjectIdentity) error {
	switch mode {
	case "":
		if claim.Name == "" || claim.UID == "" {
			return fmt.Errorf("skills require an explicit persisted PVC identity")
		}
	case SkillsModeEmpty:
		if claim != (ObjectIdentity{}) {
			return fmt.Errorf("empty skills cannot also reference a PVC")
		}
	default:
		return fmt.Errorf("unsupported internal skills mode %q", mode)
	}
	return nil
}

func skillsIdentity(claim *corev1.PersistentVolumeClaim) ObjectIdentity {
	if claim == nil {
		return ObjectIdentity{}
	}
	return ObjectIdentity{Name: claim.Name, UID: claim.UID}
}

func (mode SkillsMode) volume(claim ObjectIdentity) corev1.Volume {
	if mode == SkillsModeEmpty {
		return corev1.Volume{Name: "empty-skills", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}
	return corev1.Volume{Name: "shared-skills", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name, ReadOnly: true},
	}}
}

// ValidateSkills checks the internal source union even when no live skills
// resource exists. Cleanup can validate it without reading current selection.
func (expected Expectation) ValidateSkills() error {
	if err := expected.SkillsMode.ValidateIdentity(expected.Skills); err != nil {
		return err
	}
	if expected.SkillsMode != SkillsModeEmpty {
		for _, volume := range expected.Template.Spec.Volumes {
			if volume.Name == "empty-skills" {
				return fmt.Errorf("an empty skills volume requires the explicit empty source")
			}
		}
		return nil
	}
	if (!expected.ExecutionHeld && !expected.SinglePod) ||
		!slices.Equal(expected.Template.Finalizers, []string{NativePodFinalizer}) {
		return fmt.Errorf("empty skills require a native lifetime-protected expectation")
	}
	wanted := expected.SkillsMode.volume(expected.Skills)
	volumes, mounts := 0, 0
	for _, volume := range expected.Template.Spec.Volumes {
		if volume.Name == wanted.Name {
			if !equality.Semantic.DeepEqual(volume, wanted) {
				return fmt.Errorf("empty skills require an unpopulated in-Pod emptyDir")
			}
			volumes++
		}
		if volume.Name == "shared-skills" || (volume.PersistentVolumeClaim != nil &&
			(volume.Name != "data" || volume.PersistentVolumeClaim.ClaimName != expected.Home.Name)) {
			return fmt.Errorf("empty skills cannot introduce another PVC or shared skills volume")
		}
	}
	for i, containers := range [][]corev1.Container{expected.Template.Spec.Containers, expected.Template.Spec.InitContainers} {
		for _, container := range containers {
			for _, mount := range container.VolumeMounts {
				if mount.Name != wanted.Name && mount.MountPath != SkillsPath {
					continue
				}
				if i != 0 || container.Name != "cetirun" ||
					!equality.Semantic.DeepEqual(mount, corev1.VolumeMount{Name: wanted.Name, MountPath: SkillsPath, ReadOnly: true}) {
					return fmt.Errorf("empty skills may only be mounted read-only at the native skills path")
				}
				mounts++
			}
		}
	}
	if volumes != 1 || mounts != 1 {
		return fmt.Errorf("empty skills require exactly one volume and one read-only runtime mount")
	}
	return nil
}
