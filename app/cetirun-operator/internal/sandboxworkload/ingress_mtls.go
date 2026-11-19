// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func (b *Bundle) mtlsContainer() corev1.Container {
	plan := b.input.Controls.IngressMTLS
	return b.mtlsContainerFor(plan)
}

func (b *Bundle) mtlsContainerFor(plan *ingressmtls.Plan) corev1.Container {
	sc := b.SecurityContext()
	sc.ReadOnlyRootFilesystem = ptr.To(true)
	if b.input.AgentType == "custom" {
		sc.RunAsUser, sc.RunAsGroup = ptr.To[int64](65532), ptr.To[int64](65532)
	}
	container := corev1.Container{
		Name: ingressmtls.ContainerName, Image: plan.Image, ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/usr/local/bin/envoy"},
		Args:            []string{"--disable-hot-restart", "--concurrency", "2", "--config-path", ingressmtls.ConfigPath, "--log-level", "warning"},
		SecurityContext: sc, Resources: b.helperResources(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "ingress-tls", MountPath: ingressmtls.TLSPath, ReadOnly: true},
			{Name: "ingress-config", MountPath: "/etc/cetirun-ingress", ReadOnly: true},
		},
	}
	for _, port := range plan.Ports() {
		container.Ports = append(container.Ports, corev1.ContainerPort{Name: "mtls-" + port.Name, ContainerPort: port.TLS, Protocol: corev1.ProtocolTCP})
	}
	return container
}

func (b *Bundle) TerminalTLS() (*corev1.Container, []corev1.Volume) {
	plan := b.input.Controls.IngressMTLS
	if plan == nil || plan.Harness != "custom" || !plan.Terminal {
		return nil, nil
	}
	endpoint := plan.TerminalEndpoint()
	container := b.mtlsContainerFor(&endpoint)
	container.Args = []string{"--disable-hot-restart", "--concurrency", "2", "--config-path", "/etc/cetirun-ingress/terminal-envoy.json", "--log-level", "warning"}
	return &container, b.mtlsVolumes()
}

func (b *Bundle) mtlsVolumes() []corev1.Volume {
	secret := b.input.Controls.IngressMTLS.ServerSecret.Name
	return []corev1.Volume{
		{Name: "ingress-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secret, DefaultMode: ptr.To[int32](0440),
			Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}, {Key: "ca.crt", Path: "ca.crt"}},
		}}},
		{Name: "ingress-public", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secret, DefaultMode: ptr.To[int32](0440),
			Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "ca.crt", Path: "ca.crt"}},
		}}},
		{Name: "ingress-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: ingressmtls.ConfigName},
		}}},
	}
}

func (b *Bundle) applyIngressMTLS(values map[string]any) error {
	object := func(name string) map[string]any {
		value, _ := values[name].(map[string]any)
		if value == nil {
			value = map[string]any{}
			values[name] = value
		}
		return value
	}
	plan := b.input.Controls.IngressMTLS
	configDigest, err := plan.ConfigDigest()
	if err != nil {
		return err
	}
	object("podAnnotations")[ingressmtls.ConfigDigestAnnotation] = configDigest
	if enabled, present := object("desktop")["enabled"]; present && enabled != plan.Desktop {
		return fmt.Errorf("mTLS desktop selection differs from platform approval")
	}
	if object("tailscale")["enabled"] == true || object("agentRequest")["tailscaleEnabled"] == true {
		return fmt.Errorf("mTLS supports registered built-in API, UI, terminal and desktop; Tailscale must be disabled")
	}
	if enabled, present := object("ttyd")["enabled"]; present && enabled != plan.Terminal {
		return fmt.Errorf("mTLS terminal selection differs from platform approval")
	}
	object("ttyd")["enabled"] = plan.Terminal
	if enabled, present := object("readinessProbe")["enabled"]; present && enabled != true {
		return fmt.Errorf("mTLS requires certificate-aware runtime readiness")
	}
	object("readinessProbe")["enabled"] = true
	for _, name := range []string{"initContainers", "extraEnvFrom"} {
		if nonzeroValue(values[name]) {
			return fmt.Errorf("mTLS cannot authorize extra runtime extension paths")
		}
	}
	sidecars, volumes := []corev1.Container{b.mtlsContainer()}, b.mtlsVolumes()
	if b.input.Controls.EgressSNI != nil {
		sidecars = []corev1.Container{}
		volumes = append(volumes, b.egressVolumes()...)
	}
	for name, desired := range map[string]any{
		"sidecars": sidecars, "extraVolumes": volumes,
		"extraVolumeMounts": []corev1.VolumeMount{{Name: "ingress-public", MountPath: ingressmtls.PublicPath, ReadOnly: true}},
	} {
		encoded, err := jsonValue(desired)
		if err != nil {
			return err
		}
		if existing := values[name]; nonzeroValue(existing) && !equalJSON(existing, encoded) {
			return fmt.Errorf("mTLS %s is a protected managed endpoint input", name)
		}
		values[name] = encoded
	}
	protected := object(ValuesKey)
	protected["ingressMTLS"] = true
	protected["trustedProxyIPs"] = []any{"127.0.0.1"}
	if b.input.AgentType == "custom" {
		protected["ingressServerName"] = plan.ServerName
		if terminal, _ := protected["terminal"].(map[string]any); terminal != nil {
			container, volumes := b.TerminalTLS()
			if container != nil {
				terminal["tlsConfigDigest"] = configDigest
				terminal["tlsContainer"], err = jsonValue(container)
				if err != nil {
					return err
				}
				terminal["tlsVolumes"], err = jsonValue(volumes)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	native := object(b.input.AgentType)
	if b.input.AgentType == "openclaw" {
		config, _ := native["config"].(map[string]any)
		if config == nil {
			config = map[string]any{}
			native["config"] = config
		}
		gateway, _ := config["gateway"].(map[string]any)
		if gateway == nil {
			gateway = map[string]any{}
			config["gateway"] = gateway
		}
		gateway["bind"], gateway["trustedProxies"] = "loopback", []any{"127.0.0.1"}
		gateway["tls"], gateway["tailscale"] = map[string]any{"enabled": false}, map[string]any{"mode": "off"}
	} else {
		env, _ := native["env"].(map[string]any)
		if env == nil {
			env = map[string]any{}
			native["env"] = env
		}
		env["API_SERVER_HOST"] = "127.0.0.1"
		env["API_SERVER_PORT"] = strconv.Itoa(int(plan.Port))
		env["API_SERVER_ENABLED"] = "true"
	}
	return nil
}

func nonzeroValue(value any) bool {
	raw, _ := json.Marshal(value)
	return !slices.Contains([]string{"null", "false", "0", `""`, "[]", "{}"}, string(raw))
}

// ValidateIngressService changes only the target of an exactly approved port.
// An unexpected listener is rejected, never left as a plaintext Service route.
func (b *Bundle) ValidateIngressService(service *corev1.Service) error {
	plan := b.input.Controls.IngressMTLS
	if plan == nil {
		return nil
	}
	if b.input.Terminal != nil && service.Name == b.input.Terminal.ServiceAccountName {
		terminal := plan.TerminalEndpoint()
		plan = &terminal
	}
	if (service.Name != b.input.WorkloadName && (b.input.Terminal == nil || service.Name != b.input.Terminal.ServiceAccountName)) ||
		len(service.Spec.Ports) != len(plan.Ports()) {
		return fmt.Errorf("mTLS Service inventory differs from approved API/UI/terminal ingress")
	}
	seen := map[string]bool{}
	for index := range service.Spec.Ports {
		port := &service.Spec.Ports[index]
		match := slices.IndexFunc(plan.Ports(), func(expected ingressmtls.Port) bool {
			return expected.Name == port.Name && expected.Backend == port.Port && port.Protocol == corev1.ProtocolTCP &&
				(port.TargetPort == intstr.FromInt32(expected.Backend) || port.TargetPort == intstr.FromString(expected.Name))
		})
		if match < 0 || seen[port.Name] {
			return fmt.Errorf("mTLS Service contains an unapproved or plaintext bypass port")
		}
		seen[port.Name] = true
		port.TargetPort = intstr.FromInt32(plan.Ports()[match].TLS)
	}
	return nil
}

func (b *Bundle) validateIngressTemplate(spec corev1.PodSpec) error {
	if b.input.Controls.IngressMTLS == nil {
		return nil
	}
	helper := b.mtlsContainer()
	found, desktopFound := false, false
	if b.input.Controls.EgressSNI != nil {
		helper = b.proxyContainer()
		found = len(spec.InitContainers) > 0 && equality.Semantic.DeepEqual(spec.InitContainers[0], helper)
	}
	for index, container := range spec.Containers {
		if container.Name == helper.Name {
			if found || !equality.Semantic.DeepEqual(container, helper) {
				return fmt.Errorf("mTLS helper differs from the protected endpoint")
			}
			found = true
			continue
		}
		if container.Name == "desktop" {
			if desktopFound || !b.input.Controls.IngressMTLS.Desktop ||
				!slices.Equal(container.Command, []string{"/bin/bash", "/etc/cetirun/desktop-ingress-entrypoint.sh"}) ||
				len(container.Args) != 0 || container.ReadinessProbe == nil || container.ReadinessProbe.Exec == nil ||
				!slices.Equal(container.ReadinessProbe.Exec.Command, []string{"python3", "/etc/cetirun/desktop-ingress-probe.py"}) {
				return fmt.Errorf("mTLS desktop requires its controlled loopback startup and readiness")
			}
			for name, value := range map[string]string{"CETIRUN_INGRESS_MTLS": "true", "VNC_PORT": "5900", "NOVNC_PORT": "6080"} {
				if !slices.Contains(container.Env, corev1.EnvVar{Name: name, Value: value}) {
					return fmt.Errorf("mTLS desktop listener binding is incomplete")
				}
			}
			desktopFound = true
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == "ingress-tls" || mount.Name == "ingress-config" ||
				(mount.Name == "ingress-public" && index != 0) {
				return fmt.Errorf("mTLS credentials must not be delegated to runtime helpers")
			}
		}
	}
	if !found {
		return fmt.Errorf("mTLS requires its managed helper")
	}
	if b.input.AgentType != "custom" && !slices.Equal(spec.Containers[0].Command, []string{"/bin/bash", "/etc/cetirun/ingress-entrypoint.sh"}) {
		return fmt.Errorf("mTLS requires a managed helper and current controlled entrypoint")
	}
	if desktopFound != b.input.Controls.IngressMTLS.Desktop {
		return fmt.Errorf("mTLS desktop inventory differs from platform approval")
	}
	main := spec.Containers[0]
	if b.input.AgentType == "openclaw" && !slices.Equal(main.Args, []string{
		"--bind", "loopback", "--port", strconv.Itoa(int(b.input.Controls.IngressMTLS.Port)),
	}) {
		return fmt.Errorf("mTLS OpenClaw arguments must keep the application on loopback")
	}
	if b.input.AgentType == "openclaw" && (main.ReadinessProbe == nil || main.ReadinessProbe.Exec == nil ||
		len(main.ReadinessProbe.Exec.Command) != 5 ||
		!slices.Equal(main.ReadinessProbe.Exec.Command[:2], []string{"node", "/etc/cetirun/health-probe.cjs"})) {
		return fmt.Errorf("mTLS requires the certificate-aware native readiness probe")
	}
	bindings := map[string]string{
		"CETIRUN_INGRESS_MTLS": "true", "CETIRUN_INGRESS_SERVER_NAME": b.input.Controls.IngressMTLS.ServerName,
	}
	if b.input.AgentType == "custom" {
		bindings["CETIRUN_BIND_HOST"] = "127.0.0.1"
	}
	if b.input.AgentType == "hermes" {
		bindings["API_SERVER_HOST"], bindings["API_SERVER_PORT"] = "127.0.0.1", strconv.Itoa(int(b.input.Controls.IngressMTLS.Port))
		bindings["API_SERVER_ENABLED"] = "true"
		bindings["HERMES_WEBAPP_HOST"], bindings["HERMES_WEBAPP_PORT"] = "127.0.0.1", "9120"
		bindings["HERMES_WEBAPP_ENABLED"] = "true"
		bindings["TTYD_ENABLED"] = strconv.FormatBool(b.input.Controls.IngressMTLS.Terminal)
		if b.input.Controls.IngressMTLS.Terminal {
			bindings["TTYD_PORT"] = "7681"
		}
		if !slices.ContainsFunc(spec.InitContainers, func(container corev1.Container) bool {
			return container.Name == "init-home-data" && slices.Contains(container.Env, corev1.EnvVar{Name: "CETIRUN_INGRESS_MTLS", Value: "true"})
		}) {
			return fmt.Errorf("mTLS Hermes HOME initialization must reserve listener authority")
		}
	}
	for name, value := range bindings {
		if !slices.ContainsFunc(main.Env, func(env corev1.EnvVar) bool {
			return env.Name == name && env.Value == value && env.ValueFrom == nil
		}) {
			return fmt.Errorf("mTLS native startup and readiness bindings are incomplete")
		}
	}
	for _, container := range spec.InitContainers {
		if b.input.Controls.EgressSNI != nil && container.Name == helper.Name {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if slices.Contains([]string{"ingress-tls", "ingress-public", "ingress-config"}, mount.Name) {
				return fmt.Errorf("mTLS materials cannot be mounted by initializers")
			}
		}
		secretName := b.input.Controls.IngressMTLS.ServerSecret.Name
		for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
			for _, container := range containers {
				for _, env := range container.Env {
					if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == secretName {
						return fmt.Errorf("mTLS material must not be exposed as process environment")
					}
				}
				for _, source := range container.EnvFrom {
					if source.SecretRef != nil && source.SecretRef.Name == secretName {
						return fmt.Errorf("mTLS material must not be exposed as process environment")
					}
				}
			}
		}
		for _, volume := range spec.Volumes {
			if volume.Secret != nil && volume.Secret.SecretName == secretName &&
				volume.Name != "ingress-tls" && volume.Name != "ingress-public" {
				return fmt.Errorf("mTLS material cannot acquire another mount alias")
			}
			if volume.Projected != nil {
				for _, source := range volume.Projected.Sources {
					if source.Secret != nil && source.Secret.Name == secretName {
						return fmt.Errorf("mTLS material cannot acquire another projection")
					}
				}
			}
		}
	}
	for _, desired := range b.mtlsVolumes() {
		if !slices.ContainsFunc(spec.Volumes, func(actual corev1.Volume) bool {
			return equality.Semantic.DeepEqual(actual, desired)
		}) {
			return fmt.Errorf("mTLS Secret and config mounts differ from approval")
		}
	}
	return nil
}
