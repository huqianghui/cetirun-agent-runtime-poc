// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package podtemplate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestSameAcceptsOnlyAPIDefaults(t *testing.T) {
	expected := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		ServiceAccountName: "agent",
		Containers: []corev1.Container{{
			Name: "cetirun", Image: "registry.example.com/agent:1",
			Env: []corev1.EnvVar{{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			}}},
			Ports:         []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			Resources:     corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)}}},
		}},
		Volumes: []corev1.Volume{{Name: "settings", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}},
		}}},
	}}
	actual := expected.DeepCopy()
	actual.Spec.RestartPolicy = corev1.RestartPolicyAlways
	actual.Spec.DNSPolicy = corev1.DNSClusterFirst
	actual.Spec.SchedulerName = corev1.DefaultSchedulerName
	actual.Spec.TerminationGracePeriodSeconds = ptr.To(int64(30))
	actual.Spec.DeprecatedServiceAccount = "agent"
	container := &actual.Spec.Containers[0]
	container.ImagePullPolicy = corev1.PullIfNotPresent
	container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	container.Env[0].ValueFrom.FieldRef.APIVersion = "v1"
	container.Ports[0].Protocol = corev1.ProtocolTCP
	container.LivenessProbe.TimeoutSeconds, container.LivenessProbe.PeriodSeconds = 1, 10
	container.LivenessProbe.SuccessThreshold, container.LivenessProbe.FailureThreshold = 1, 3
	container.LivenessProbe.HTTPGet.Scheme, container.LivenessProbe.HTTPGet.Path = corev1.URISchemeHTTP, "/"
	actual.Spec.Volumes[0].ConfigMap.DefaultMode = ptr.To(int32(0644))
	if !Same(*actual, expected) {
		t.Fatal("explicit API defaults were rejected")
	}
	for name, mutate := range map[string]func(*corev1.PodTemplateSpec){
		"image":   func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].Image = "registry.example.com/agent:2" },
		"command": func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].Command = []string{"changed"} },
		"environment": func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "EXTRA", Value: "changed"})
		},
		"privilege": func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		},
		"resources": func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
		},
		"volume":   func(p *corev1.PodTemplateSpec) { p.Spec.Volumes[0].ConfigMap.Name = "other" },
		"priority": func(p *corev1.PodTemplateSpec) { p.Spec.Priority = ptr.To[int32](1) },
		"probe":    func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].LivenessProbe.HTTPGet.Path = "/changed" },
		"toleration": func(p *corev1.PodTemplateSpec) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := actual.DeepCopy()
			mutate(bad)
			if Same(*bad, expected) {
				t.Fatal("material template change was ignored")
			}
		})
	}
}
