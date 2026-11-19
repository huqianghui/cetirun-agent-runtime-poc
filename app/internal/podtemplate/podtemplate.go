// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package podtemplate compares complete PodTemplates after Kubernetes API
// defaults. Final-Pod admission defaults remain the Pod validator's concern.
package podtemplate

import (
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func Same(actual, expected corev1.PodTemplateSpec) bool {
	a, b := actual.DeepCopy(), expected.DeepCopy()
	if DefaultSpec(&a.Spec) != nil || DefaultSpec(&b.Spec) != nil {
		return false
	}
	return equality.Semantic.DeepEqual(a, b)
}

// DefaultSpec applies embedded PodSpec defaults, not admission-plugin defaults.
func DefaultSpec(spec *corev1.PodSpec) error {
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = corev1.RestartPolicyAlways
	}
	if spec.DNSPolicy == "" {
		spec.DNSPolicy = corev1.DNSClusterFirst
	}
	if spec.SchedulerName == "" {
		spec.SchedulerName = corev1.DefaultSchedulerName
	}
	if spec.TerminationGracePeriodSeconds == nil {
		spec.TerminationGracePeriodSeconds = ptr.To(int64(30))
	}
	if spec.DeprecatedServiceAccount == "" {
		spec.DeprecatedServiceAccount = spec.ServiceAccountName
	}
	for i := range spec.Containers {
		defaultContainer(&spec.Containers[i])
	}
	for i := range spec.InitContainers {
		defaultContainer(&spec.InitContainers[i])
	}
	for i := range spec.Volumes {
		source := &spec.Volumes[i].VolumeSource
		if source.ConfigMap != nil && source.ConfigMap.DefaultMode == nil {
			source.ConfigMap.DefaultMode = ptr.To(int32(0644))
		}
		if source.Secret != nil && source.Secret.DefaultMode == nil {
			source.Secret.DefaultMode = ptr.To(int32(0644))
		}
		if source.Projected != nil && source.Projected.DefaultMode == nil {
			source.Projected.DefaultMode = ptr.To(int32(0644))
		}
		if source.DownwardAPI != nil {
			if source.DownwardAPI.DefaultMode == nil {
				source.DownwardAPI.DefaultMode = ptr.To(int32(0644))
			}
			defaultDownwardItems(source.DownwardAPI.Items)
		}
		if source.Projected != nil {
			for _, projection := range source.Projected.Sources {
				if projection.DownwardAPI != nil {
					defaultDownwardItems(projection.DownwardAPI.Items)
				}
			}
		}
	}
	var normalized corev1.PodSpec
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return err
	}
	*spec = normalized
	return nil
}

func defaultDownwardItems(items []corev1.DownwardAPIVolumeFile) {
	for i := range items {
		defaultFieldSelectors(items[i].FieldRef, items[i].ResourceFieldRef)
	}
}

func defaultFieldSelectors(field *corev1.ObjectFieldSelector, resourceField *corev1.ResourceFieldSelector) {
	if field != nil && field.APIVersion == "" {
		field.APIVersion = "v1"
	}
	if resourceField != nil && resourceField.Divisor.IsZero() {
		resourceField.Divisor = resource.MustParse("1")
	}
}

func defaultContainer(container *corev1.Container) {
	if container.ImagePullPolicy == "" {
		container.ImagePullPolicy = corev1.PullIfNotPresent
		tag := container.Image[strings.LastIndex(container.Image, "/")+1:]
		if !strings.Contains(container.Image, "@") && (!strings.Contains(tag, ":") || strings.HasSuffix(tag, ":latest")) {
			container.ImagePullPolicy = corev1.PullAlways
		}
	}
	if container.TerminationMessagePath == "" {
		container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	for _, env := range container.Env {
		if env.ValueFrom != nil {
			defaultFieldSelectors(env.ValueFrom.FieldRef, env.ValueFrom.ResourceFieldRef)
		}
	}
	for i := range container.Ports {
		if container.Ports[i].Protocol == "" {
			container.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe, container.StartupProbe} {
		if probe == nil {
			continue
		}
		if probe.TimeoutSeconds == 0 {
			probe.TimeoutSeconds = 1
		}
		if probe.PeriodSeconds == 0 {
			probe.PeriodSeconds = 10
		}
		if probe.SuccessThreshold == 0 {
			probe.SuccessThreshold = 1
		}
		if probe.FailureThreshold == 0 {
			probe.FailureThreshold = 3
		}
		if probe.HTTPGet != nil {
			if probe.HTTPGet.Scheme == "" {
				probe.HTTPGet.Scheme = corev1.URISchemeHTTP
			}
			if probe.HTTPGet.Path == "" {
				probe.HTTPGet.Path = "/"
			}
		}
	}
}
