// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"slices"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/ptr"
)

func (b *Bundle) proxyContainer() corev1.Container {
	plan := b.input.Controls.EgressSNI
	var container corev1.Container
	if b.input.Controls.IngressMTLS != nil {
		container = b.mtlsContainer()
	} else {
		container = corev1.Container{
			Name: ingressmtls.ContainerName, ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/usr/local/bin/envoy"},
			Args:            []string{"--disable-hot-restart", "--concurrency", "2", "--config-path", ingressmtls.ConfigPath, "--log-level", "warning"},
			SecurityContext: b.SecurityContext(), Resources: b.helperResources(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: "ingress-config", MountPath: "/etc/cetirun-ingress", ReadOnly: true},
			},
		}
	}
	container.Image = plan.EnvoyImage
	container.SecurityContext.RunAsUser = ptr.To[int64](egresscapture.ProxyUID)
	container.SecurityContext.ReadOnlyRootFilesystem = ptr.To(true)
	container.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: egresscapture.PrivateVolumeName, MountPath: egresscapture.PrivatePath})
	return container
}

func (b *Bundle) captureContainer() corev1.Container {
	security := b.SecurityContext()
	security.RunAsNonRoot, security.RunAsUser, security.RunAsGroup = ptr.To(false), ptr.To[int64](0), ptr.To[int64](0)
	security.ReadOnlyRootFilesystem = ptr.To(true)
	security.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
	return corev1.Container{
		Name: egresscapture.InitContainerName, Image: b.input.Controls.EgressSNI.InitImage,
		ImagePullPolicy: corev1.PullIfNotPresent, SecurityContext: security,
		Command:   []string{"/bin/sh", egresscapture.CapturePath + "/capture.sh"},
		Resources: b.helperResources(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: egresscapture.CaptureVolumeName, MountPath: egresscapture.CapturePath, ReadOnly: true},
			{Name: egresscapture.LockVolumeName, MountPath: egresscapture.LockPath},
		},
	}
}

func (b *Bundle) egressVolumes() []corev1.Volume {
	limit := b.input.Resolution.Sandbox.Storage.Scratch.SizeLimit.DeepCopy()
	volumes := []corev1.Volume{
		{Name: egresscapture.PrivateVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit},
		}},
		{Name: egresscapture.LockVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit},
		}},
		{Name: egresscapture.CaptureVolumeName, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: ingressmtls.ConfigName},
				Items:                []corev1.KeyToPath{{Key: "capture.sh", Path: "capture.sh"}},
				DefaultMode:          ptr.To[int32](0444),
			},
		}},
	}
	if b.input.Controls.IngressMTLS == nil {
		volumes = append(volumes, corev1.Volume{Name: "ingress-config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: ingressmtls.ConfigName},
			},
		}})
	}
	return volumes
}

func (b *Bundle) applyEgressSNI(values map[string]any) error {
	if !b.input.Reconciled {
		return fmt.Errorf("observed-SNI capture requires the common executor")
	}
	if nonzeroValue(values["initContainers"]) {
		return fmt.Errorf("observed-SNI capture cannot authorize arbitrary initializer extensions")
	}
	if nonzeroValue(values["sidecars"]) {
		return fmt.Errorf("observed-SNI capture cannot authorize arbitrary sidecar extensions")
	}
	values["sidecars"] = []any{}
	volumes, _ := values["extraVolumes"].([]any)
	for _, expected := range b.egressVolumes() {
		encoded, err := jsonValue(expected)
		if err != nil {
			return err
		}
		found := false
		for _, volume := range volumes {
			object, _ := volume.(map[string]any)
			if object["name"] == expected.Name {
				if found || !equalJSON(volume, encoded) {
					return fmt.Errorf("runtime volumes collide with managed egress state")
				}
				found = true
			}
		}
		if !found {
			volumes = append(volumes, encoded)
		}
	}
	values["extraVolumes"] = volumes
	probe, _ := values["readinessProbe"].(map[string]any)
	if probe == nil {
		probe = map[string]any{}
	}
	if enabled, ok := probe["enabled"]; ok && enabled != true {
		return fmt.Errorf("observed-SNI capture requires proxy-aware readiness")
	}
	probe["enabled"] = true
	values["readinessProbe"] = probe
	protected := values[ValuesKey].(map[string]any)
	protected["egressSNI"] = true
	initializers, err := jsonValue([]corev1.Container{b.proxyContainer(), b.captureContainer()})
	if err != nil {
		return err
	}
	protected["managedInitContainers"] = initializers
	values["podAnnotations"].(map[string]any)[ingressmtls.ConfigDigestAnnotation] = b.input.Controls.ProxyDigest
	return nil
}

func (b *Bundle) validateEgressTemplate(template *corev1.PodTemplateSpec) error {
	plan := b.input.Controls.EgressSNI
	spec := template.Spec
	if plan == nil {
		for index, group := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
			for _, container := range group {
				if container.Name == egresscapture.InitContainerName ||
					index == 1 && container.Name == ingressmtls.ContainerName {
					return fmt.Errorf("unrequested capture or restartable initializer")
				}
			}
		}
		return nil
	}
	digest, err := sandboxpolicy.ManagedProxyDigest(b.input.Controls.IngressMTLS, plan)
	if err != nil || digest != b.input.Controls.ProxyDigest ||
		template.Annotations[ingressmtls.ConfigDigestAnnotation] != digest {
		return fmt.Errorf("observed-SNI configuration differs from its protected source digest")
	}
	if spec.SecurityContext == nil || len(spec.InitContainers) < 3 || !equality.Semantic.DeepEqual(spec.InitContainers[0], b.proxyContainer()) ||
		!equality.Semantic.DeepEqual(spec.InitContainers[1], b.captureContainer()) ||
		!slices.ContainsFunc(spec.InitContainers[2:], func(container corev1.Container) bool { return container.Name == "init-home-data" }) {
		return fmt.Errorf("capture requires the exact managed Envoy then trusted init before runtime initialization")
	}
	probeCommand := []string{"node", "/etc/cetirun/health-probe.cjs"}
	if b.input.AgentType == "custom" {
		probeCommand = []string{"/cetirun-probe/http-probe"}
	} else if b.input.AgentType == "hermes" {
		probeCommand = []string{"/usr/local/bin/hermes-python", "/etc/cetirun/hermes-runtime.py", "readiness"}
	}
	if len(spec.Containers) == 0 || spec.Containers[0].ReadinessProbe == nil ||
		spec.Containers[0].ReadinessProbe.Exec == nil ||
		!slices.Equal(spec.Containers[0].ReadinessProbe.Exec.Command[:min(len(probeCommand), len(spec.Containers[0].ReadinessProbe.Exec.Command))], probeCommand) ||
		b.input.AgentType == "custom" && !slices.Contains(spec.Containers[0].ReadinessProbe.Exec.Command, "--egress-sni") ||
		!slices.Contains(spec.Containers[0].Env, corev1.EnvVar{Name: "CETIRUN_EGRESS_SNI", Value: "true"}) {
		return fmt.Errorf("capture requires the managed runtime readiness path")
	}
	egressFlags := 0
	for _, env := range spec.Containers[0].Env {
		if env.Name == "CETIRUN_EGRESS_SNI" {
			egressFlags++
		}
	}
	if egressFlags != 1 {
		return fmt.Errorf("capture readiness cannot be overridden by a duplicate environment binding")
	}
	for i, group := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for index, container := range group {
			if i == 1 && index < 2 {
				continue
			}
			if container.SecurityContext == nil || container.Name == ingressmtls.ContainerName || container.Name == egresscapture.InitContainerName ||
				ptr.Deref(container.SecurityContext.RunAsUser, ptr.Deref(spec.SecurityContext.RunAsUser, 0)) == egresscapture.ProxyUID {
				return fmt.Errorf("runtime containers cannot impersonate the managed proxy or initializer")
			}
			for _, port := range container.Ports {
				if port.ContainerPort == egresscapture.CapturePort || port.ContainerPort == ingressmtls.AdminPort {
					return fmt.Errorf("runtime listener collides with a reserved managed proxy port")
				}
			}
			for _, mount := range container.VolumeMounts {
				if slices.Contains([]string{egresscapture.PrivateVolumeName, egresscapture.CaptureVolumeName,
					egresscapture.LockVolumeName, "ingress-config"}, mount.Name) {
					return fmt.Errorf("managed proxy files cannot be mounted by ordinary containers")
				}
			}
		}
	}
	for _, expected := range b.egressVolumes() {
		count := 0
		for _, actual := range spec.Volumes {
			if actual.Name == expected.Name {
				count++
				if !equality.Semantic.DeepEqual(actual, expected) {
					return fmt.Errorf("managed proxy volumes differ from compiler ownership")
				}
			}
		}
		if count != 1 {
			return fmt.Errorf("managed proxy volumes must be present exactly once")
		}
	}
	return nil
}
