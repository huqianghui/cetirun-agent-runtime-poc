// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/api/equality"

	corev1 "k8s.io/api/core/v1"
)

func (b *Bundle) validateHermesTemplate(spec corev1.PodSpec, apiPort *int32) error {
	if b.input.AgentType != "hermes" {
		return nil
	}
	if len(spec.Containers) == 0 {
		return fmt.Errorf("Hermes requires its gateway container")
	}
	if apiPort == nil || *apiPort < 1 || *apiPort > 65535 || *apiPort == 9120 || *apiPort == 18790 {
		return fmt.Errorf("Hermes requires a nonconflicting resolved API port")
	}
	main := spec.Containers[0]
	env := map[string]corev1.EnvVar{}
	for _, entry := range main.Env {
		if _, exists := env[entry.Name]; exists {
			return fmt.Errorf("Hermes environment names must be unique")
		}
		env[entry.Name] = entry
	}
	host := "0.0.0.0"
	if b.input.Controls.IngressMTLS != nil {
		if b.input.Controls.IngressMTLS.Port != *apiPort {
			return fmt.Errorf("Hermes API port differs from the approved mTLS binding")
		}
		host = "127.0.0.1"
	}
	for name, value := range map[string]string{
		"HERMES_HOME": "/home/vibe/.hermes", "HERMES_MANAGED_DIR": "/etc/cetirun/hermes-managed",
		"HERMES_PYTHON": "/usr/local/bin/hermes-python", "HERMES_WEBAPP_ENABLED": "true",
		"HERMES_WEBAPP_HOST": host, "HERMES_WEBAPP_PORT": "9120",
		"API_SERVER_ENABLED": "true", "API_SERVER_HOST": host, "API_SERVER_PORT": strconv.Itoa(int(*apiPort)),
		"HERMES_DASHBOARD_BASIC_AUTH_USERNAME": "cetirun",
	} {
		if env[name].Value != value || env[name].ValueFrom != nil {
			return fmt.Errorf("Hermes gateway configuration differs from platform authority")
		}
	}
	publicURL, err := url.Parse(env["HERMES_WEBAPP_PUBLIC_URL"].Value)
	if err != nil || publicURL.Scheme != "https" || publicURL.Host == "" || publicURL.Path != "/ui/"+b.input.AgentName || publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" || env["HERMES_WEBAPP_PUBLIC_URL"].ValueFrom != nil {
		return fmt.Errorf("Hermes Webapp requires its full platform HTTPS URL")
	}
	var credentialSecret string
	for name, key := range map[string]string{
		"API_SERVER_KEY": "API_SERVER_KEY", "HERMES_DASHBOARD_BASIC_AUTH_PASSWORD": "API_SERVER_KEY",
		"HERMES_DASHBOARD_BASIC_AUTH_SECRET": "CETIRUN_AUTH_KEY", "CETIRUN_AUTH_KEY": "CETIRUN_AUTH_KEY",
	} {
		entry := env[name]
		if entry.Value != "" || entry.ValueFrom == nil || entry.ValueFrom.SecretKeyRef == nil || entry.ValueFrom.SecretKeyRef.Key != key || entry.ValueFrom.SecretKeyRef.Name == "" || (entry.ValueFrom.SecretKeyRef.Optional != nil && *entry.ValueFrom.SecretKeyRef.Optional) {
			return fmt.Errorf("Hermes requires independent API and session-signing Secret keys")
		}
		if credentialSecret != "" && credentialSecret != entry.ValueFrom.SecretKeyRef.Name {
			return fmt.Errorf("Hermes authentication must use the instance Secret")
		}
		credentialSecret = entry.ValueFrom.SecretKeyRef.Name
	}
	if !slices.ContainsFunc(main.VolumeMounts, func(mount corev1.VolumeMount) bool {
		return equality.Semantic.DeepEqual(mount, corev1.VolumeMount{Name: "hermes-managed", MountPath: "/etc/cetirun/hermes-managed", ReadOnly: true})
	}) {
		return fmt.Errorf("Hermes requires its read-only managed configuration mount")
	}
	if !slices.ContainsFunc(spec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == "hermes-managed" && volume.ConfigMap != nil && volume.ConfigMap.Name != "" && volume.ConfigMap.DefaultMode != nil && *volume.ConfigMap.DefaultMode == 0444 &&
			slices.Equal(volume.ConfigMap.Items, []corev1.KeyToPath{{Key: "hermes-managed.yaml", Path: "config.yaml"}})
	}) {
		return fmt.Errorf("Hermes requires its managed ConfigMap projection")
	}
	for _, port := range []corev1.ContainerPort{{Name: "api", ContainerPort: *apiPort, Protocol: corev1.ProtocolTCP}, {Name: "webapp", ContainerPort: 9120, Protocol: corev1.ProtocolTCP}} {
		if !slices.Contains(main.Ports, port) {
			return fmt.Errorf("Hermes API and Webapp ports differ from the supported profile")
		}
	}
	for _, port := range main.Ports {
		if port.Name != "api" && port.ContainerPort == *apiPort {
			return fmt.Errorf("Hermes API port conflicts with another container listener")
		}
	}
	for name, probe := range map[string]*corev1.Probe{"readiness": main.ReadinessProbe, "startup": main.StartupProbe} {
		matches := func(command []string) bool {
			return probe != nil && probe.Exec != nil && len(probe.Exec.Command) == len(command)+1 && slices.Equal(probe.Exec.Command[:len(command)], command)
		}
		command := []string{"/usr/local/bin/hermes-python", "/etc/cetirun/hermes-runtime.py", "readiness", "--timeout"}
		if name == "startup" {
			// Existing Agents can remain pinned to a chart with the PM startup probe.
			if matches([]string{"/usr/local/bin/hermes-python", "/etc/cetirun/hermes-runtime.py", "liveness", "--timeout"}) {
				continue
			}
			command = []string{"node", "/etc/cetirun/hermes-health-probe.cjs", "--timeout"}
		}
		if !matches(command) {
			return fmt.Errorf("Hermes %s probe must use its managed health check", name)
		}
	}
	return nil
}
