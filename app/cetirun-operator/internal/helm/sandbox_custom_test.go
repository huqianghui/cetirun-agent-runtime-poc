// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func customSandboxRenderInput(port *int32, webUI bool) (w.Input, map[string]any) {
	fixture := testfixture.New("openclaw")
	input := fixture.Input
	input.AgentType, input.Reconciled = "custom", true
	input.Helpers = w.Helpers{}
	input.Skills, input.SkillsMode = nil, w.SkillsModeEmpty
	input.Resolution.AgentPolicy.Harnesses.Allowed = append(input.Resolution.AgentPolicy.Harnesses.Allowed, api.AgentTypeCustom)
	input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "key-uid"}
	input.AuthorizedSecrets = append(input.AuthorizedSecrets, input.GatewayKey.Name)
	input.GatewayAPIBase = "http://gateway.example.svc.cluster.local:4000/v1"
	custom := &w.CustomAgent{
		Image:    "registry.example.com/worker@sha256:" + strings.Repeat("a", 64),
		Username: "worker", Home: "/home/worker", Port: port, WebUI: webUI,
		Environment: []w.CustomEnvironment{
			{Name: "HOME", Value: "/home/worker"},
			{Name: "OPENAI_BASE_URL", Value: input.GatewayAPIBase},
			{Name: "OPENAI_API_KEY", CredentialSlot: "model"},
		},
	}
	if port != nil {
		custom.Environment = append(custom.Environment, w.CustomEnvironment{Name: "PORT", Value: "8080"})
		custom.Ports = []w.CustomPort{{Name: "http", Number: *port}}
		custom.HTTP = &w.CustomHTTP{PortName: "http", HealthPath: "/healthz", ReadinessPath: "/healthz", StartupPath: "/healthz"}
		input.LoopbackProbe = &w.LoopbackProbe{Image: "registry.example.com/cetirun-terminal@sha256:" + strings.Repeat("b", 64)}
	}
	if webUI {
		custom.WebBasePath, custom.WebInstance = "/ui/example/", "agent-uid"
		custom.Environment = append(custom.Environment,
			w.CustomEnvironment{Name: "CETIRUN_WEB_BASE_PATH", Value: custom.WebBasePath},
			w.CustomEnvironment{Name: "CETIRUN_WEB_INSTANCE", Value: custom.WebInstance})
	} else if custom.HTTP != nil {
		custom.HTTP.ChatPath, custom.HTTP.ModelsPath = "/v1/chat/completions", "/v1/models"
	}
	input.CustomAgent = custom
	input.Context = &w.Context{ObjectIdentity: w.ObjectIdentity{Name: "custom-context-example", UID: "context-uid"},
		Files: map[string]string{"AGENTS.md": "AGENTS.md"}}
	values := sampleInput("custom", "openai", false)
	values["agentRequest"] = map[string]any{"type": "custom", "image": custom.Image}
	values["secrets"] = map[string]any{"existingSecret": "runtime-secret"}
	values["aiGateway"] = map[string]any{
		"enabled": true, "keySecretName": input.GatewayKey.Name, "keySecretKey": "api-key",
		"models": []any{map[string]any{"id": "example-model", "name": "Example"}},
	}
	values["ttyd"] = map[string]any{"enabled": false}
	values["desktop"] = map[string]any{"enabled": false}
	return input, values
}

func TestCustomSandboxRenderPreservesHarnessAndGatewayContract(t *testing.T) {
	for _, scenario := range []string{"headless", "chat", "web-ui"} {
		t.Run(scenario, func(t *testing.T) {
			var port *int32
			if scenario != "headless" {
				port = ptr.To[int32](8080)
			}
			input, values := customSandboxRenderInput(port, scenario == "web-ui")
			bundle, err := w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			main := template.Spec.Containers[0]
			if len(main.Command) != 0 || len(main.Args) != 0 || main.Image != input.CustomAgent.Image ||
				main.SecurityContext.RunAsUser != nil || template.Spec.SecurityContext.RunAsUser != nil ||
				!ptr.Deref(main.SecurityContext.RunAsNonRoot, false) {
				t.Fatal("Custom rendering changed its approved image entrypoint or non-root image user")
			}
			if !slices.ContainsFunc(main.Env, func(value corev1.EnvVar) bool {
				return value.Name == "OPENAI_API_KEY" && value.ValueFrom != nil && value.ValueFrom.SecretKeyRef != nil &&
					value.ValueFrom.SecretKeyRef.Name == input.GatewayKey.Name && value.ValueFrom.SecretKeyRef.Key == "api-key"
			}) {
				t.Fatal("Custom did not receive only its approved Gateway key")
			}
			if !slices.ContainsFunc(main.VolumeMounts, func(mount corev1.VolumeMount) bool {
				return mount.Name == "context-view" && mount.MountPath == "/home/worker/.agents" && mount.ReadOnly
			}) {
				t.Fatal("Custom did not receive its canonical read-only context")
			}
			for _, mutate := range []func(*corev1.PodTemplateSpec){
				func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].Command = []string{"/bin/sh"} },
				func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To[int64](0) },
				func(p *corev1.PodTemplateSpec) {
					p.Spec.Containers[0].Env[2].ValueFrom.SecretKeyRef.Name = "foreign-key"
				},
			} {
				bad := template.DeepCopy()
				mutate(bad)
				if err := bundle.ValidateReconciledTemplate(bad, prepared.Agent.Image, false, prepared.Agent.Port); err == nil {
					t.Fatal("Custom render validation accepted a changed runtime contract")
				}
			}
		})
	}
}
