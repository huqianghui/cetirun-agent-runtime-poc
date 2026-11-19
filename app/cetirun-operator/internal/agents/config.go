// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"sigs.k8s.io/yaml"
)

// RenderConfig evaluates the same native asset used by standalone Helm. Only
// deterministic formatting/collection functions are exposed, with no lookup,
// environment access, network access, or nested template evaluation.
func RenderConfig(agent ResolvedAgent, bindings Bindings) (string, error) {
	if bindings.Skills.Empty && (!bindings.NativeSandbox || !bindings.Model.Gateway || (agent.Type != "openclaw" && !bindings.ReconciledSandbox) ||
		bindings.Skills.Shared || len(bindings.Skills.NPM) != 0 || bindings.Skills.MountPath != "/home/vibe/.aks/skills") {
		return "", fmt.Errorf("empty skills require the native OpenClaw Gateway's read-only skills directory without shared storage or packages")
	}
	if len(bindings.TrustedProxyIPs) != 0 &&
		(!bindings.NativeSandbox || !bindings.Model.Gateway || (agent.Type != "openclaw" && !bindings.ReconciledSandbox) || bindings.Integrations.Tailscale) {
		return "", fmt.Errorf("compiler proxy trust requires native OpenClaw Gateway inputs without Tailscale")
	}
	if bindings.IngressMTLS && (!bindings.NativeSandbox || !bindings.ReconciledSandbox || !bindings.Model.Gateway ||
		bindings.Integrations.Tailscale || !slices.Equal(bindings.TrustedProxyIPs, []string{"127.0.0.1"})) {
		return "", fmt.Errorf("managed ingress mTLS requires common Sandbox Gateway inputs and exact loopback proxy trust without Tailscale")
	}
	renderer, err := template.New("native-config").Funcs(nativeTemplateFunctions()).Parse(bindings.ConfigTemplate)
	if err != nil {
		return "", fmt.Errorf("invalid native config asset")
	}
	context := nativeTemplateContext(agent, bindings)
	var output bytes.Buffer
	if err := renderer.Execute(&output, context); err != nil {
		// Native preferences can contain user data; don't echo template errors.
		return "", fmt.Errorf("native config does not match the selected profile")
	}
	return strings.TrimSpace(output.String()) + "\n", nil
}

func nativeTemplateFunctions() template.FuncMap {
	available := sprig.TxtFuncMap()
	funcs := template.FuncMap{}
	for _, name := range []string{
		"deepCopy", "default", "dict", "list", "dig", "set", "int", "replace",
		"append", "concat", "uniq", "has", "hasKey", "unset",
	} {
		funcs[name] = available[name]
	}
	funcs["toPrettyJson"] = func(value any) (string, error) {
		raw, err := json.MarshalIndent(value, "", "  ")
		return string(raw), err
	}
	funcs["toYaml"] = func(value any) (string, error) {
		raw, err := yaml.Marshal(value)
		return strings.TrimSuffix(string(raw), "\n"), err
	}
	funcs["required"] = func(message string, value string) (string, error) {
		if value == "" {
			return "", fmt.Errorf("%s", message)
		}
		return value, nil
	}
	return funcs
}

func nativeTemplateContext(agent ResolvedAgent, bindings Bindings) map[string]any {
	integrations := bindings.Integrations
	native := map[string]any{"config": bindings.NativeConfig}
	for key, value := range bindings.NativeOptions {
		native[key] = value
	}
	gatewayModels := make([]any, 0, len(bindings.Model.GatewayModels))
	for _, model := range bindings.Model.GatewayModels {
		entry := map[string]any{"id": model.ID, "name": model.Name}
		if model.ContextWindow > 0 {
			entry["contextWindow"] = model.ContextWindow
		}
		if model.MaxTokens > 0 {
			entry["maxTokens"] = model.MaxTokens
		}
		gatewayModels = append(gatewayModels, entry)
	}
	visionModels := make([]any, 0, len(bindings.Model.GatewayVisionModels))
	for _, id := range bindings.Model.GatewayVisionModels {
		visionModels = append(visionModels, id)
	}
	gateway := map[string]any{
		"enabled":      bindings.Model.Gateway,
		"models":       gatewayModels,
		"visionModels": visionModels,
	}
	// Hermes' provider names the variable the chart puts the key in.
	if bindings.Model.GatewayKeyEnv != "" {
		gateway["hermes"] = map[string]any{"keyEnv": bindings.Model.GatewayKeyEnv}
	}
	values := map[string]any{
		"memoryContext":  map[string]any{"enabled": bindings.MemoryEnabled},
		"desktop":        map[string]any{"enabled": bindings.DesktopEnabled},
		agent.Type:       native,
		"aiGateway":      gateway,
		"connectionsMCP": map[string]any{"enabled": bindings.ConnectionsMCP.Enabled, "url": bindings.ConnectionsMCP.URL},
		"secrets": map[string]any{
			"provider":          bindings.Model.Provider,
			"model":             bindings.Model.Name,
			"apiBase":           bindings.Model.BaseURL,
			"authMode":          bindings.Model.AuthMode,
			"existingSecret":    integrations.ExistingSecret,
			"braveApiKey":       integrations.Brave,
			"perplexityApiKey":  integrations.Perplexity,
			"telegramBotToken":  integrations.Telegram,
			"telegramTokenFile": integrations.TelegramTokenFile,
			"slackBotToken":     integrations.Slack,
			"slackAppToken":     integrations.Slack,
			"discordBotToken":   integrations.Discord,
			"feishuAppId":       integrations.Feishu,
			"feishuAppSecret":   integrations.Feishu,
		},
		"skills": map[string]any{
			"shared":    bindings.Skills.Shared,
			"mountPath": bindings.Skills.MountPath,
			"npm":       bindings.Skills.NPM,
		},
	}
	if bindings.NativeSandbox {
		values["sandboxWorkload"] = map[string]any{"contractVersion": "v1"}
		if bindings.ReconciledSandbox {
			values["sandboxWorkload"].(map[string]any)["reconciled"] = true
		}
		if bindings.IngressMTLS {
			values["sandboxWorkload"].(map[string]any)["ingressMTLS"] = true
		}
		if len(bindings.TrustedProxyIPs) != 0 {
			values["sandboxWorkload"].(map[string]any)["trustedProxyIPs"] = slices.Clone(bindings.TrustedProxyIPs)
		}
		if bindings.Skills.Empty {
			values["sandboxWorkload"].(map[string]any)["skillsMode"] = "empty"
		}
	}
	return map[string]any{
		"Values":           values,
		"Chart":            map[string]any{"Version": bindings.ChartVersion},
		"SkillDirs":        NPMSkillDirs(bindings.Skills.NPM),
		"TailscaleEnabled": integrations.Tailscale,
	}
}

func NPMSkillDirs(packages []string) []string {
	dirs := make([]string, 0, len(packages))
	for _, spec := range packages {
		prefix := ""
		if strings.HasPrefix(spec, "@") {
			prefix, spec = "@", strings.TrimPrefix(spec, "@")
		}
		name, _, _ := strings.Cut(spec, "@")
		dirs = append(dirs, "/home/vibe/.cetirun/skills/npm/"+strings.ReplaceAll(prefix+name, "/", "__"))
	}
	return dirs
}

func Build(agent ResolvedAgent, bindings Bindings, descriptor Descriptor) (Contribution, error) {
	if err := Validate(agent, bindings, descriptor); err != nil {
		return Contribution{}, err
	}
	config, err := RenderConfig(agent, bindings)
	if err != nil {
		return Contribution{}, err
	}
	ports := []PortBinding{{Name: descriptor.PortName, Number: *agent.Port}}
	if descriptor.WebappPort != 0 {
		ports = append(ports, PortBinding{Name: "webapp", Number: descriptor.WebappPort})
	}
	// The in-agent model sign-in helper serves the Portal's Copilot login in
	// charts that still carry it.
	if !bindings.NoSignInHelper && !bindings.Model.Gateway {
		ports = append(ports, PortBinding{Name: "cetirun-auth", Number: 18790})
	}
	return Contribution{
		Agent:       agent,
		ProfileID:   bindings.ProfileID,
		ConfigFiles: map[string]string{descriptor.ConfigFile: config},
		Ports:       ports,
		HTTP: &HTTPBinding{
			PortName:      descriptor.PortName,
			HealthPath:    descriptor.HealthPath,
			ReadinessPath: descriptor.ReadinessPath,
			StartupPath:   descriptor.StartupPath,
		},
	}, nil
}
