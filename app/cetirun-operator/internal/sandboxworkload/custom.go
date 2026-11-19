// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"path"
	"regexp"
	"slices"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

type Context struct {
	ObjectIdentity
	Files map[string]string `json:"files"`
}

type Terminal struct {
	Image              string `json:"image"`
	Port               int32  `json:"port"`
	SessionSeconds     int32  `json:"sessionSeconds"`
	ServiceAccountName string `json:"serviceAccountName"`
}

type LoopbackProbe struct {
	Image string `json:"image"`
}

// CustomAgent is a Harness-validated contribution, not an arbitrary Pod spec.
// Command and arguments remain owned by the approved image.
type CustomAgent struct {
	RunAsUser   *int64              `json:"runAsUser,omitempty"`
	RunAsGroup  *int64              `json:"runAsGroup,omitempty"`
	Image       string              `json:"image"`
	Username    string              `json:"username"`
	Home        string              `json:"home"`
	Port        *int32              `json:"port,omitempty"`
	WebUI       bool                `json:"webUI,omitempty"`
	WebBasePath string              `json:"webBasePath,omitempty"`
	WebInstance string              `json:"webInstance,omitempty"`
	Environment []CustomEnvironment `json:"environment,omitempty"`
	Ports       []CustomPort        `json:"ports,omitempty"`
	HTTP        *CustomHTTP         `json:"http,omitempty"`
}

type CustomEnvironment struct {
	Name           string `json:"name"`
	Value          string `json:"value,omitempty"`
	CredentialSlot string `json:"credentialSlot,omitempty"`
}

type CustomPort struct {
	Name   string `json:"name"`
	Number int32  `json:"number"`
}

type CustomHTTP struct {
	PortName      string `json:"portName"`
	HealthPath    string `json:"healthPath"`
	ReadinessPath string `json:"readinessPath"`
	StartupPath   string `json:"startupPath"`
	ChatPath      string `json:"chatPath,omitempty"`
	ModelsPath    string `json:"modelsPath,omitempty"`
}

var customUsername = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var terminalImage = regexp.MustCompile(`^[^[:space:]@]+@sha256:[a-f0-9]{64}$`)

func validateCustomAgent(input Input) error {
	if input.AgentType != "custom" {
		if input.CustomAgent != nil || input.GatewayAPIBase != "" || input.Context != nil || input.LoopbackProbe != nil {
			return fmt.Errorf("only Custom workloads may set a portable runtime identity")
		}
		return nil
	}
	custom := input.CustomAgent
	if !input.Reconciled || custom == nil || custom.Image == "" || !customUsername.MatchString(custom.Username) ||
		custom.Home != path.Join("/home", custom.Username) || custom.Username == "root" ||
		(custom.Port != nil && (*custom.Port < 1 || *custom.Port > 65535)) ||
		(custom.WebUI && custom.Port == nil) {
		return fmt.Errorf("Custom requires common execution and an approved image, user, HOME and port mode")
	}
	if (custom.HTTP == nil) != (custom.Port == nil) {
		return fmt.Errorf("Custom HTTP bindings must match the configured port mode")
	}
	if input.GatewayKey == nil || input.GatewayAPIBase == "" || input.Context == nil ||
		input.Context.Name == "" || input.Context.UID == "" ||
		len(validation.IsDNS1123Subdomain(input.Context.Name)) != 0 ||
		len(input.Context.Files) != 1 || input.Context.Files["AGENTS.md"] != "AGENTS.md" {
		return fmt.Errorf("Custom requires typed AI Gateway and canonical context bindings")
	}
	needsProbe := custom.HTTP != nil || input.Controls.EgressSNI != nil
	if needsProbe && (input.LoopbackProbe == nil || !terminalImage.MatchString(input.LoopbackProbe.Image)) {
		return fmt.Errorf("Custom HTTP or FQDN capture requires an immutable platform loopback probe image")
	}
	if !needsProbe && input.LoopbackProbe != nil {
		return fmt.Errorf("headless Custom cannot configure a loopback probe")
	}
	if input.Controls.EgressSNI != nil && (custom.RunAsUser == nil || custom.RunAsGroup == nil ||
		*custom.RunAsUser < 1 || *custom.RunAsGroup < 1 || *custom.RunAsUser > 2147483647 ||
		*custom.RunAsGroup > 2147483647 || *custom.RunAsUser == egresscapture.ProxyUID) {
		return fmt.Errorf("Custom FQDN capture requires an approved non-root image identity distinct from the proxy")
	}
	return nil
}

func validateTerminal(input Input) error {
	terminal := input.Terminal
	if terminal == nil {
		return nil
	}
	if input.AgentType != "custom" || !input.Reconciled || !terminalImage.MatchString(terminal.Image) ||
		terminal.Port < 1024 || terminal.Port > 65535 || terminal.SessionSeconds < 60 || terminal.SessionSeconds > 3600 ||
		len(validation.IsDNS1123Label(terminal.ServiceAccountName)) != 0 ||
		terminal.ServiceAccountName != terminalpolicy.WorkloadName(input.WorkloadName) {
		return fmt.Errorf("Custom terminal requires a platform image, port, session and service account")
	}
	return nil
}

func customEnvironmentValues(values []CustomEnvironment) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, map[string]any{"name": value.Name, "value": value.Value, "credentialSlot": value.CredentialSlot})
	}
	return out
}

func customPortValues(values []CustomPort) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, map[string]any{"name": value.Name, "number": value.Number})
	}
	return out
}

func customHTTPValues(value *CustomHTTP) any {
	if value == nil {
		return nil
	}
	return map[string]any{"portName": value.PortName, "healthPath": value.HealthPath,
		"readinessPath": value.ReadinessPath, "startupPath": value.StartupPath,
		"chatPath": value.ChatPath, "modelsPath": value.ModelsPath}
}

func (b *Bundle) validateCustomReconciledTemplate(spec *corev1.PodSpec, memoryEnabled bool) error {
	custom, context := b.input.CustomAgent, b.input.Context
	if custom == nil || context == nil || b.input.GatewayKey == nil || len(spec.Containers) == 0 || len(spec.InitContainers) == 0 {
		return fmt.Errorf("Custom runtime lacks its approved Harness or AI Gateway identity")
	}
	main := spec.Containers[0]
	var init *corev1.Container
	for index := range spec.InitContainers {
		if spec.InitContainers[index].Name == "init-home-data" {
			init = &spec.InitContainers[index]
			break
		}
	}
	if init == nil || main.Name != "cetirun" || main.Image != custom.Image || len(main.Command) != 0 || len(main.Args) != 0 ||
		init.Image != custom.Image ||
		!slices.Equal(init.Command, []string{"/bin/sh", "/etc/cetirun/custom-init-home.sh"}) || len(init.Args) != 0 {
		return fmt.Errorf("Custom runtime rewrites an approved image entrypoint or HOME initializer")
	}
	for _, expected := range custom.Environment {
		want := corev1.EnvVar{Name: expected.Name, Value: expected.Value}
		if expected.CredentialSlot != "" {
			if expected.CredentialSlot != "model" {
				return fmt.Errorf("Custom runtime contains an unsupported credential slot")
			}
			want.Value = ""
			want.ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: b.input.GatewayKey.Name}, Key: "api-key",
			}}
		}
		if !slices.ContainsFunc(main.Env, func(actual corev1.EnvVar) bool { return equality.Semantic.DeepEqual(actual, want) }) {
			return fmt.Errorf("Custom runtime is missing a Harness or typed AI Gateway environment value")
		}
	}
	if !slices.Equal(main.Ports, customContainerPorts(custom.Ports)) {
		return fmt.Errorf("Custom runtime ports differ from the Harness contract")
	}
	serverName := ""
	if plan := b.input.Controls.IngressMTLS; plan != nil {
		serverName = plan.ServerName
	}
	egress := b.input.Controls.EgressSNI != nil
	if custom.HTTP == nil {
		var readiness *corev1.Probe
		if egress {
			readiness = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"/cetirun-probe/http-probe", "--egress-sni"},
			}}, PeriodSeconds: 10, TimeoutSeconds: 3}
		}
		if main.StartupProbe != nil || !equality.Semantic.DeepEqual(main.ReadinessProbe, readiness) || main.LivenessProbe != nil {
			return fmt.Errorf("headless Custom cannot define HTTP probes")
		}
	} else if !customHTTPProbesMatch(main, custom.HTTP, *custom.Port, serverName, egress) {
		return fmt.Errorf("Custom probes differ from the Harness contract")
	}
	if b.input.LoopbackProbe != nil {
		probe := b.input.LoopbackProbe
		index := slices.IndexFunc(spec.InitContainers, func(container corev1.Container) bool { return container.Name == "install-loopback-probe" })
		if probe == nil || index < 0 || !equality.Semantic.DeepEqual(spec.InitContainers[index], corev1.Container{
			Name: "install-loopback-probe", Image: probe.Image, ImagePullPolicy: corev1.PullIfNotPresent,
			Command: []string{"/loopback-probe", "install", "/cetirun-probe/http-probe"}, SecurityContext: b.SecurityContext(),
			Resources: b.helperResources(), VolumeMounts: []corev1.VolumeMount{{Name: "loopback-probe", MountPath: "/cetirun-probe"}},
		}) {
			return fmt.Errorf("Custom loopback probe installer differs from the platform contract")
		}
	}
	mainMounts := []corev1.VolumeMount{
		{Name: "data", MountPath: custom.Home}, {Name: "tmp", MountPath: "/tmp"},
		{Name: "config", MountPath: "/etc/cetirun", ReadOnly: true},
		{Name: "context-view", MountPath: custom.Home + "/.agents", SubPath: "current", ReadOnly: true},
	}
	if b.input.LoopbackProbe != nil {
		mainMounts = append(mainMounts, corev1.VolumeMount{Name: "loopback-probe", MountPath: "/cetirun-probe", ReadOnly: true})
	}
	if memoryEnabled {
		mainMounts = append(mainMounts, runtimebindings.MemoryMounts()...)
	}
	mainMounts = append(mainMounts, b.input.Controls.VolumeMounts...)
	if b.input.Controls.IngressMTLS != nil {
		mainMounts = append(mainMounts, corev1.VolumeMount{Name: "ingress-public", MountPath: ingressmtls.PublicPath, ReadOnly: true})
	}
	if b.input.ConnectionsMCP != nil {
		mainMounts = append(mainMounts,
			corev1.VolumeMount{Name: "connections-identity", MountPath: "/var/run/cetirun/connections", ReadOnly: true},
			corev1.VolumeMount{Name: "connections-ca", MountPath: "/var/run/cetirun/connections-ca", ReadOnly: true})
	}
	initMounts := []corev1.VolumeMount{
		{Name: "data", MountPath: "/home-data"}, {Name: "tmp", MountPath: "/tmp"},
		{Name: "config", MountPath: "/etc/cetirun", ReadOnly: true},
		{Name: "context", MountPath: "/etc/cetirun-context", ReadOnly: true},
		{Name: "context-view", MountPath: "/etc/cetirun-context-view"},
	}
	initEnv := []corev1.EnvVar{
		{Name: "SOURCE_HOME", Value: custom.Home}, {Name: "TARGET_HOME", Value: "/home-data"},
		{Name: "CETIRUN_INIT_IMAGE", Value: custom.Image}, {Name: "CETIRUN_INIT_COMMON", Value: "/etc/cetirun/init-home-common.sh"},
		{Name: "CETIRUN_CONTEXT_DIR", Value: "/etc/cetirun-context"},
		{Name: "CETIRUN_CONTEXT_VIEW", Value: "/etc/cetirun-context-view"},
	}
	if b.input.SkillsMode != SkillsModeEmpty {
		skills := corev1.VolumeMount{Name: "shared-skills", MountPath: SkillMarketplaceMountPath, ReadOnly: true}
		initEnv = append(initEnv, corev1.EnvVar{Name: "CETIRUN_SHARED_SKILLS_DIR", Value: SkillMarketplaceMountPath})
		// Skill Marketplace: only the Team's subtree, and the selection of
		// the release that the initializer copies.
		if marketplace := b.input.SkillMarketplace; marketplace != nil {
			skills.SubPath = marketplace.TeamPath
			initEnv = append(initEnv, corev1.EnvVar{Name: SkillMarketplaceSelectionEnv, Value: marketplace.SelectionDigest})
		}
		initMounts = append(initMounts, skills)
	}
	if memoryEnabled {
		initMounts = append(initMounts, runtimebindings.MemoryMounts()...)
		initEnv = append(initEnv, corev1.EnvVar{Name: "CETIRUN_MEMORY_ASSETS", Value: runtimebindings.MemoryAssetsDir})
	}
	if !equalCustomMounts(main.VolumeMounts, mainMounts) || !equalCustomMounts(init.VolumeMounts, initMounts) ||
		!equality.Semantic.DeepEqual(init.Env, initEnv) {
		return fmt.Errorf("Custom mounts or initialization environment differ from the common contract")
	}
	contextVolume := corev1.Volume{Name: "context", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: context.Name}, DefaultMode: ptr.To[int32](0444),
		Items: []corev1.KeyToPath{{Key: "AGENTS.md", Path: "AGENTS.md"}},
	}}}
	if !hasExactCustomVolume(*spec, contextVolume) {
		return fmt.Errorf("Custom context source differs from the controller snapshot")
	}
	names := []string{"context-view"}
	if b.input.LoopbackProbe != nil {
		names = append(names, "loopback-probe")
	}
	for _, name := range names {
		if !hasExactCustomVolume(*spec, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: ptr.To(b.input.Resolution.Sandbox.Storage.Scratch.SizeLimit),
		}}}) {
			return fmt.Errorf("Custom volume %q differs from the Sandbox scratch limit", name)
		}
	}
	return nil
}

func hasExactCustomVolume(spec corev1.PodSpec, expected corev1.Volume) bool {
	return slices.ContainsFunc(spec.Volumes, func(actual corev1.Volume) bool {
		return equality.Semantic.DeepEqual(actual, expected)
	})
}

func customContainerPorts(ports []CustomPort) []corev1.ContainerPort {
	result := make([]corev1.ContainerPort, 0, len(ports))
	for _, port := range ports {
		result = append(result, corev1.ContainerPort{Name: port.Name, ContainerPort: port.Number, Protocol: corev1.ProtocolTCP})
	}
	return result
}

func customHTTPProbesMatch(container corev1.Container, http *CustomHTTP, port int32, serverName string, egress bool) bool {
	probe := func(path string, period, failures int32) *corev1.Probe {
		command := []string{
			"/cetirun-probe/http-probe", "--url", fmt.Sprintf("http://127.0.0.1:%d%s", port, path),
		}
		if serverName != "" {
			command = append(command, "--mtls-server-name", serverName)
		}
		if egress {
			command = append(command, "--egress-sni")
		}
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}},
			PeriodSeconds: period, TimeoutSeconds: 3, FailureThreshold: failures}
	}
	return container.LivenessProbe == nil && container.StartupProbe != nil && container.ReadinessProbe != nil &&
		equality.Semantic.DeepEqual(container.StartupProbe, probe(http.StartupPath, 5, 60)) &&
		equality.Semantic.DeepEqual(container.ReadinessProbe, probe(http.HealthPath, 10, 0))
}

func equalCustomMounts(actual, expected []corev1.VolumeMount) bool {
	actual, expected = slices.Clone(actual), slices.Clone(expected)
	sortMounts(actual)
	sortMounts(expected)
	return equality.Semantic.DeepEqual(actual, expected)
}
