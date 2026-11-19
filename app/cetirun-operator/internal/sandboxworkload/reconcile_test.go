// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func TestReconciledVolumesBoundEveryDiskEmptyDir(t *testing.T) {
	limit := resource.MustParse("1Gi")
	for _, tc := range []struct {
		name   string
		volume corev1.EmptyDirVolumeSource
		valid  bool
	}{
		{name: "unbounded"},
		{name: "zero", volume: corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("0"))}},
		{name: "negative", volume: corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("-1"))}},
		{name: "one byte over", volume: corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1073741825"))}},
		{name: "one byte under", volume: corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1073741823"))}, valid: true},
		{name: "equal", volume: corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(limit)}, valid: true},
		{name: "equivalent units", volume: corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1024Mi"))}, valid: true},
		{name: "memory keeps separate accounting", volume: corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := corev1.PodSpec{Volumes: []corev1.Volume{
				{Name: "bounded-first", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(limit)}}},
				{Name: "extension", VolumeSource: corev1.VolumeSource{EmptyDir: &tc.volume}},
			}}
			before := spec.DeepCopy()
			if err := ValidateReconciledVolumes(spec, nil, nil, limit); (err == nil) != tc.valid {
				t.Fatalf("volume validation: %v, want valid=%t", err, tc.valid)
			}
			if !equality.Semantic.DeepEqual(spec, *before) {
				t.Fatal("volume validation mutated the requested template")
			}
		})
	}
}

func TestReconciledPodNormalizesActualQOSAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name string
		spec corev1.PodSpec
		want bool
	}{
		{"best effort", corev1.PodSpec{}, false},
		{"storage only", corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("128Mi")},
		}}}}, false},
		{"container CPU", corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		}}}}, true},
		{"init memory", corev1.PodSpec{InitContainers: []corev1.Container{{Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		}}}}, true},
		{"Pod budget", corev1.PodSpec{Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		}}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if err := defaultPod(&scenario.spec); err != nil {
				t.Fatal(err)
			}
			found := slices.ContainsFunc(scenario.spec.Tolerations, func(value corev1.Toleration) bool {
				return value.Key == "node.kubernetes.io/memory-pressure" && value.Operator == corev1.TolerationOpExists &&
					value.Effect == corev1.TaintEffectNoSchedule && value.Value == "" && value.TolerationSeconds == nil
			})
			if found != scenario.want {
				t.Fatalf("memory-pressure default = %v, want %v", found, scenario.want)
			}
		})
	}
}
