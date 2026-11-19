// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func aiGatewayInput(typ, provider string) map[string]any {
	input := sampleInput(typ, provider, false)
	input["secrets"].(map[string]any)["provider"] = provider
	input["aiGateway"] = map[string]any{"enabled": true, "keySecretName": "ai-gateway-virtual-key", "keySecretKey": "api-key"}
	input["networkPolicy"] = map[string]any{"gatewayNamespace": "cetirun-system", "gatewayPort": 4000}
	return input
}

func renderAIGateway(t *testing.T, input map[string]any) (map[string]string, error) {
	t.Helper()
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareChart(chrt, input)
	if err != nil {
		return nil, err
	}
	return renderChartValues(t, chrt, prepared.Values)
}

// renderStandalone renders the chart from input as standalone Helm does,
// without the operator's preparation.
func renderStandalone(t *testing.T, input map[string]any) (map[string]string, error) {
	t.Helper()
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	return renderChartValues(t, chrt, input)
}

func renderChartValues(t *testing.T, chrt *chart.Chart, input map[string]any) (map[string]string, error) {
	t.Helper()
	caps := chartutil.DefaultCapabilities.Copy()
	caps.APIVersions = append(caps.APIVersions, "networking.istio.io/v1/Sidecar")
	values, err := chartutil.ToRenderValues(chrt, input, chartutil.ReleaseOptions{
		Name: "openclaw-example", Namespace: "cetirun-example", IsInstall: true, Revision: 1,
	}, caps)
	if err != nil {
		t.Fatal(err)
	}
	return engine.Render(chrt, values)
}

func openAIKeyEnv(t *testing.T, rendered map[string]string) *corev1.EnvVar {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), sts); err != nil {
		t.Fatal(err)
	}
	for _, container := range sts.Spec.Template.Spec.Containers {
		for i := range container.Env {
			if container.Env[i].Name == "OPENAI_API_KEY" {
				return &container.Env[i]
			}
		}
	}
	return nil
}

// initGatewayMarker returns the value of CETIRUN_AI_GATEWAY on the OpenClaw
// init container, which tells init that the Gateway owns the model list.
func initGatewayMarker(t *testing.T, rendered map[string]string) (string, bool) {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), sts); err != nil {
		t.Fatal(err)
	}
	for _, container := range sts.Spec.Template.Spec.InitContainers {
		if container.Name != "init-home-data" {
			continue
		}
		for _, env := range container.Env {
			if env.Name == "CETIRUN_AI_GATEWAY" {
				return env.Value, true
			}
		}
		return "", false
	}
	t.Fatal("no init-home-data init container")
	return "", false
}

// openClawInitContainers returns the init containers of the rendered workload.
func openClawInitContainers(t *testing.T, rendered map[string]string) []corev1.Container {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), sts); err != nil {
		t.Fatal(err)
	}
	return sts.Spec.Template.Spec.InitContainers
}

// openClawProvider returns the openai provider entry of the rendered OpenClaw
// configuration.
func openClawProvider(t *testing.T, rendered map[string]string) map[string]any {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/configmap.yaml"]), configMap); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Models struct {
			Providers map[string]map[string]any `json:"providers"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(configMap.Data["openclaw.json"]), &config); err != nil {
		t.Fatal(err)
	}
	return config.Models.Providers["openai"]
}

func TestAIGatewayChartWiring(t *testing.T) {
	rendered, err := renderAIGateway(t, aiGatewayInput("openclaw", "openai"))
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := initGatewayMarker(t, rendered); !ok || value != "true" {
		t.Fatalf("init must know the Gateway owns the model list, got %q", value)
	}
	// The Gateway provider is bound to the virtual key: OpenClaw would
	// otherwise prefer a stored openai profile over OPENAI_API_KEY.
	provider := openClawProvider(t, rendered)
	wantKey := map[string]any{"source": "env", "provider": "default", "id": "OPENAI_API_KEY"}
	if provider["auth"] != "api-key" || !reflect.DeepEqual(provider["apiKey"], wantKey) {
		t.Fatalf("the Gateway provider must bind OPENAI_API_KEY, got auth=%v apiKey=%v", provider["auth"], provider["apiKey"])
	}
	// The sign-in the retired in-agent helper left in the home is removed
	// once, after doctor imported the auth profile files of earlier versions.
	containers := openClawInitContainers(t, rendered)
	var names []string
	for i := range containers {
		names = append(names, containers[i].Name)
	}
	env := openAIKeyEnv(t, rendered)
	if env == nil || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
		t.Fatal("OPENAI_API_KEY is not read from a Secret")
	}
	ref := env.ValueFrom.SecretKeyRef
	if ref.Name != "ai-gateway-virtual-key" || ref.Key != "api-key" || (ref.Optional != nil && *ref.Optional) {
		t.Fatalf("OPENAI_API_KEY must come from the required virtual key Secret, got %+v", ref)
	}
	policy := rendered["cetirun/templates/networkpolicy.yaml"]
	for _, want := range []string{"kubernetes.io/metadata.name: cetirun-system", "app.kubernetes.io/name: cetirun-ai-gateway",
		"app.kubernetes.io/component: gateway", "port: 4000"} {
		if !strings.Contains(policy, want) {
			t.Errorf("NetworkPolicy lacks %q", want)
		}
	}
	if !strings.Contains(rendered["cetirun/templates/sidecar.yaml"], `"cetirun-system/*"`) {
		t.Error("Sidecar egress scope lacks the Gateway namespace")
	}
}

func TestAIGatewayChartRejectsDirectProviderConfiguration(t *testing.T) {
	for name, input := range map[string]map[string]any{
		"Gateway disabled":     directProviderInput("openclaw"),
		"non-Gateway provider": aiGatewayInput("openclaw", "github_copilot"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := renderAIGateway(t, input); err == nil {
				t.Fatalf("direct provider configuration was accepted: %v", err)
			}
		})
	}
}

func directProviderInput(typ string) map[string]any {
	input := sampleInput(typ, "openai", false)
	input["aiGateway"].(map[string]any)["enabled"] = false
	return input
}

// openClawAgentDefaults returns agents.defaults of the rendered OpenClaw
// configuration.
func openClawAgentDefaults(t *testing.T, rendered map[string]string) map[string]any {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/configmap.yaml"]), configMap); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Agents struct {
			Defaults map[string]any `json:"defaults"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(configMap.Data["openclaw.json"]), &config); err != nil {
		t.Fatal(err)
	}
	return config.Agents.Defaults
}

// OpenClaw derives a utility model for titles and activity recaps from the
// primary model's provider: OpenAI's small model for the Gateway provider,
// which the Gateway does not offer. An Agent on the Gateway names its
// default model instead, with or without a model list.
func TestAIGatewayChartRoutesUtilityTasksToTheDefaultModel(t *testing.T) {
	listed := aiGatewayInput("openclaw", "openai")
	listed["aiGateway"].(map[string]any)["models"] = []any{
		map[string]any{"id": "gpt-6-sol", "name": "GPT-6 Sol", "contextWindow": 272000, "maxTokens": 128000},
		map[string]any{"id": "copilot/claude-haiku-4.5", "name": "Claude Haiku 4.5"},
	}
	for name, input := range map[string]map[string]any{"without a model list": aiGatewayInput("openclaw", "openai"), "with a model list": listed} {
		t.Run(name, func(t *testing.T) {
			rendered, err := renderAIGateway(t, input)
			if err != nil {
				t.Fatal(err)
			}
			defaults := openClawAgentDefaults(t, rendered)
			primary, _ := defaults["model"].(map[string]any)["primary"].(string)
			if primary == "" || defaults["utilityModel"] != primary {
				t.Fatalf("utilityModel = %v, want the default model %q", defaults["utilityModel"], primary)
			}
			allowed, _ := defaults["modelPolicy"].(map[string]any)["allow"].([]any)
			if !slices.Contains(allowed, any(primary)) {
				t.Fatalf("the utility model %q is not allowed: %v", primary, allowed)
			}
		})
	}
}

func TestAIGatewayChartRejectsUnsupportedWiring(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"copilot provider": {func(in map[string]any) { in["secrets"].(map[string]any)["provider"] = "github_copilot" }, "runtime inputs"},
		"missing Secret":   {func(in map[string]any) { in["aiGateway"].(map[string]any)["keySecretName"] = "" }, "keySecretName"},
		"no NetworkPolicy": {func(in map[string]any) { in["networkPolicy"].(map[string]any)["enabled"] = false }, "networkPolicy.enabled"},
		"reserved extraEnv": {func(in map[string]any) {
			in["extraEnv"] = []any{map[string]any{"name": "OPENAI_BASE_URL", "value": "https://example.com"}}
		}, "reserved"},
		"Hermes' key variable in extraEnv": {func(in map[string]any) {
			in["agentType"] = "hermes"
			in["agentRequest"] = map[string]any{"type": "hermes"}
			in["extraEnv"] = []any{map[string]any{"name": "CETIRUN_GATEWAY_API_KEY", "value": "example"}}
		}, "runtime inputs"},
		"no Sidecar scoping": {func(in map[string]any) { in["sidecarScope"] = map[string]any{"enabled": false} }, "sidecarScope.enabled"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			input := aiGatewayInput("openclaw", "openai")
			tc.mutate(input)
			_, err := renderAIGateway(t, input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestAIGatewayChartBridgesResponsesOnlyCopilotModels(t *testing.T) {
	input := aiGatewayInput("openclaw", "openai")
	input["aiGateway"].(map[string]any)["copilotProxy"] = map[string]any{
		"enabled": true, "image": "registry.example.com/cetirun-ai-gateway:1.0.0",
		"masterKeySecretName": "ai-gateway-copilot-proxy", "tokenSecretName": "github-copilot-token",
		"responsesModels": []any{"gpt-5.5"},
	}
	rendered, err := renderAIGateway(t, input)
	if err != nil {
		t.Fatal(err)
	}
	proxy := rendered["cetirun/templates/copilot-proxy.yaml"]
	for _, want := range []string{
		`- model_name: "gpt-5.5"`, `model: "github_copilot/gpt-5.5"`, "mode: responses",
		`- model_name: "*"`, `model: "github_copilot/*"`,
	} {
		if !strings.Contains(proxy, want) {
			t.Errorf("proxy configuration lacks %q", want)
		}
	}
	if strings.Index(proxy, `"gpt-5.5"`) > strings.Index(proxy, `model_name: "*"`) {
		t.Error("the named model must come before the wildcard")
	}
	input["aiGateway"].(map[string]any)["copilotProxy"].(map[string]any)["responsesModels"] = []any{"../etc"}
	if _, err := renderAIGateway(t, input); err == nil {
		t.Fatal("a model ID that is not a Copilot model ID must be rejected")
	}
}

// proxyTemplateAnnotations returns the annotations of the Copilot proxy's Pod
// template.
func proxyTemplateAnnotations(t *testing.T, rendered map[string]string) map[string]string {
	t.Helper()
	return proxyDeployment(t, rendered).Spec.Template.Annotations
}

// proxyDeployment returns the Copilot proxy's Deployment.
func proxyDeployment(t *testing.T, rendered map[string]string) *appsv1.Deployment {
	t.Helper()
	for _, document := range strings.Split(rendered["cetirun/templates/copilot-proxy.yaml"], "\n---") {
		if !strings.Contains(document, "kind: Deployment") {
			continue
		}
		deployment := &appsv1.Deployment{}
		if err := yaml.Unmarshal([]byte(document), deployment); err != nil {
			t.Fatal(err)
		}
		return deployment
	}
	t.Fatal("no proxy Deployment rendered")
	return nil
}

// Container Insights copies a container's environment, Secret values included,
// into its ContainerInventory table unless collection is off for the container.
// The proxy holds its master key there.
func TestAIGatewayChartKeepsTheProxyKeyOutOfContainerInsights(t *testing.T) {
	input := aiGatewayInput("openclaw", "openai")
	input["aiGateway"].(map[string]any)["copilotProxy"] = map[string]any{
		"enabled": true, "image": "registry.example.com/cetirun-ai-gateway:1.0.0",
		"masterKeySecretName": "ai-gateway-copilot-proxy", "tokenSecretName": "github-copilot-token",
	}
	rendered, err := renderAIGateway(t, input)
	if err != nil {
		t.Fatal(err)
	}
	containers := proxyDeployment(t, rendered).Spec.Template.Spec.Containers
	if len(containers) == 0 {
		t.Fatal("the proxy Deployment has no container")
	}
	for _, container := range containers {
		if envVar(container, "LITELLM_MASTER_KEY") == nil {
			continue
		}
		if got := envVar(container, "AZMON_COLLECT_ENV"); got == nil || got.Value != "False" {
			t.Errorf("container %s holds the proxy key without AZMON_COLLECT_ENV=False: %v", container.Name, got)
		}
		return
	}
	t.Fatal("no proxy container holds the master key")
}

func TestAIGatewayChartRestartsTheProxyForANewMasterKey(t *testing.T) {
	render := func(keyID any) (map[string]string, error) {
		input := aiGatewayInput("openclaw", "openai")
		proxy := map[string]any{
			"enabled": true, "image": "registry.example.com/cetirun-ai-gateway:1.0.0",
			"masterKeySecretName": "ai-gateway-copilot-proxy", "tokenSecretName": "github-copilot-token",
		}
		if keyID != nil {
			proxy["masterKeyId"] = keyID
		}
		input["aiGateway"].(map[string]any)["copilotProxy"] = proxy
		return renderAIGateway(t, input)
	}
	rendered, err := render("0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	// The key reaches the proxy as an environment variable: only a changed
	// Pod template restarts it with a new one.
	if got := proxyTemplateAnnotations(t, rendered)["cetirun.microsoft.com/copilot-proxy-key-id"]; got != "0123456789ab" {
		t.Fatalf("the proxy's Pod template must carry the key ID, got %q", got)
	}
	rendered, err = render(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := proxyTemplateAnnotations(t, rendered)["cetirun.microsoft.com/copilot-proxy-key-id"]; ok {
		t.Fatal("no key ID, no annotation")
	}
	if _, err := render("not-a-hash"); err == nil {
		t.Fatal("a key ID that is not a hash prefix must be rejected")
	}
}

func TestAIGatewayChartRestartsTheProxyForANewSignIn(t *testing.T) {
	render := func(generation any) (map[string]string, error) {
		input := aiGatewayInput("openclaw", "openai")
		proxy := map[string]any{
			"enabled": true, "image": "registry.example.com/cetirun-ai-gateway:1.0.0",
			"masterKeySecretName": "ai-gateway-copilot-proxy", "tokenSecretName": "github-copilot-token",
		}
		if generation != nil {
			proxy["signInGeneration"] = generation
		}
		input["aiGateway"].(map[string]any)["copilotProxy"] = proxy
		return renderAIGateway(t, input)
	}
	rendered, err := render("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	// The proxy keeps the Copilot token it exchanged: only a changed Pod
	// template, and with it an empty cache, makes a new sign-in take effect.
	if got := proxyTemplateAnnotations(t, rendered)["cetirun.microsoft.com/copilot-sign-in-generation"]; got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("the proxy's Pod template must carry the sign-in generation, got %q", got)
	}
	rendered, err = render(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := proxyTemplateAnnotations(t, rendered)["cetirun.microsoft.com/copilot-sign-in-generation"]; ok {
		t.Fatal("no generation, no annotation")
	}
	if _, err := render("not a generation"); err == nil {
		t.Fatal("a generation that is not hexadecimal must be rejected")
	}
}

// hermesRender is the parts of a rendered Hermes release the Gateway wiring
// changes.
type hermesRender struct {
	config      map[string]any
	configFiles map[string]string
	main        corev1.Container
	init        corev1.Container
	service     string
	policy      string
	envFile     string
}

func renderHermes(t *testing.T, input map[string]any) hermesRender {
	t.Helper()
	rendered, err := renderAIGateway(t, input)
	if err != nil {
		t.Fatal(err)
	}
	return parseHermes(t, rendered)
}

func parseHermes(t *testing.T, rendered map[string]string) hermesRender {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/configmap.yaml"]), configMap); err != nil {
		t.Fatal(err)
	}
	out := hermesRender{configFiles: configMap.Data, service: rendered["cetirun/templates/service.yaml"],
		policy: rendered["cetirun/templates/networkpolicy.yaml"]}
	if err := yaml.Unmarshal([]byte(configMap.Data["hermes-config.yaml"]), &out.config); err != nil {
		t.Fatal(err)
	}
	sts := &appsv1.StatefulSet{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), sts); err != nil {
		t.Fatal(err)
	}
	for _, container := range sts.Spec.Template.Spec.Containers {
		if container.Name == "cetirun" {
			out.main = container
		}
	}
	for _, container := range sts.Spec.Template.Spec.InitContainers {
		if container.Name == "init-home-data" {
			out.init = container
		}
	}
	secret := &corev1.Secret{}
	if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/secret.yaml"]), secret); err == nil {
		out.envFile = string(secret.Data["hermes.env"])
	}
	return out
}

func envVar(container corev1.Container, name string) *corev1.EnvVar {
	for i := range container.Env {
		if container.Env[i].Name == name {
			return &container.Env[i]
		}
	}
	return nil
}

// A Hermes Agent on the Gateway gets one Gateway provider with its models and
// reads its key from the key Secret through the environment, never from its
// configuration or .env. It runs no in-agent sign-in helper: no port, no
// Service port, no NetworkPolicy port and no helper script.
func TestAIGatewayChartWiresHermes(t *testing.T) {
	input := aiGatewayHermesInput("")
	input["secrets"].(map[string]any)["apiBase"] = "http://cetirun-ai-gateway.cetirun-system.svc.cluster.local:4000/v1"
	input["aiGateway"].(map[string]any)["models"] = []any{
		map[string]any{"id": "test-model", "name": "Test Model", "contextWindow": 272000, "maxTokens": 128000},
		map[string]any{"id": "copilot/claude-haiku-4.5", "name": "Claude Haiku 4.5 (GitHub Copilot)", "contextWindow": 200000},
	}
	got := renderHermes(t, input)

	wantProvider := map[string]any{
		"name": "CetiRun AI Gateway", "api": "http://cetirun-ai-gateway.cetirun-system.svc.cluster.local:4000/v1",
		"key_env": "CETIRUN_GATEWAY_API_KEY", "transport": "chat_completions", "default_model": "test-model",
		"discover_models": false,
		"models": map[string]any{
			"test-model":               map[string]any{"context_length": float64(272000)},
			"copilot/claude-haiku-4.5": map[string]any{"context_length": float64(200000)},
		},
	}
	providers, _ := got.config["providers"].(map[string]any)
	if !reflect.DeepEqual(normalizeYAML(t, providers["cetirun-gateway"]), wantProvider) {
		t.Fatalf("Gateway provider %#v", providers["cetirun-gateway"])
	}
	if model := normalizeYAML(t, got.config["model"]); !reflect.DeepEqual(model, map[string]any{"default": "test-model", "provider": "cetirun-gateway"}) {
		t.Fatalf("model %v", model)
	}
	key := envVar(got.main, "CETIRUN_GATEWAY_API_KEY")
	if key == nil || key.ValueFrom == nil || key.ValueFrom.SecretKeyRef == nil ||
		key.ValueFrom.SecretKeyRef.Name != "ai-gateway-virtual-key" || key.ValueFrom.SecretKeyRef.Key != "api-key" {
		t.Fatalf("the key must come from the key Secret, got %+v", key)
	}
	for _, container := range []corev1.Container{got.main, got.init} {
		if marker := envVar(container, "CETIRUN_AI_GATEWAY"); marker == nil || marker.Value != "true" {
			t.Fatalf("%s lacks CETIRUN_AI_GATEWAY=true", container.Name)
		}
	}
	if keyEnv := envVar(got.init, "CETIRUN_GATEWAY_KEY_ENV"); keyEnv == nil || keyEnv.Value != "CETIRUN_GATEWAY_API_KEY" {
		t.Fatalf("init must know the key variable, got %+v", keyEnv)
	}
	for _, port := range got.main.Ports {
		if port.Name == "cetirun-auth" || port.ContainerPort == 18790 {
			t.Fatal("no sign-in helper port on the Gateway")
		}
	}
	if strings.Contains(got.service, "18790") || strings.Contains(got.policy, "port: 18790") {
		t.Fatal("no Service or NetworkPolicy port for the sign-in helper on the Gateway")
	}
	if _, ok := got.configFiles["hermes-auth.sh"]; ok {
		t.Fatal("no in-agent sign-in helper script on the Gateway")
	}
	if !strings.Contains(got.policy, "app.kubernetes.io/name: cetirun-ai-gateway") || !strings.Contains(got.policy, "port: 4000") {
		t.Fatal("the NetworkPolicy must allow the Gateway")
	}
	if !strings.Contains(got.envFile, "API_SERVER_KEY=") || strings.Contains(got.envFile, "CETIRUN_GATEWAY_API_KEY") {
		t.Fatal("Hermes' generated .env must be rendered, without the key variable")
	}
}

func TestAIGatewayChartRejectsHermesWithoutGateway(t *testing.T) {
	if _, err := renderAIGateway(t, directProviderInput("hermes")); err == nil {
		t.Fatalf("Hermes direct-provider configuration was accepted: %v", err)
	}
}

// normalizeYAML converts a decoded YAML value to the JSON shapes the test
// compares against.
func normalizeYAML(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// aiGatewayHermesInput is a Hermes Agent on the Gateway whose chart generates
// its own Secret, hermes.env included, with the key bound to keyEnv when set.
func aiGatewayHermesInput(keyEnv string) map[string]any {
	input := aiGatewayInput("hermes", "openai")
	delete(input["secrets"].(map[string]any), "existingSecret")
	if keyEnv != "" {
		input["aiGateway"].(map[string]any)["hermes"] = map[string]any{"keyEnv": keyEnv}
	}
	return input
}

// Standalone Helm can bind the Hermes key to another API key variable. A model
// provider key, such as OPENAI_API_KEY, can carry it: the container gets it
// once, from the key Secret, and the Gateway provider and init name it. Init
// removes the key's variable from Hermes' .env, so a variable the chart writes
// there for another setting cannot carry it. The key replaces the variable in
// Hermes' container, so neither can a variable that controls the container's
// processes, such as PATH, nor one the chart sets there.
func TestAIGatewayChartBindsTheHermesKeyToAnotherVariable(t *testing.T) {
	// The controller always binds the default variable: it replaces the
	// aiGateway values. Another one is standalone Helm's. A release the
	// operator prepares renders Hermes' configuration itself, so it must name
	// the variable its values select as well.
	for _, renderer := range []struct {
		name   string
		render func(*testing.T, map[string]any) (map[string]string, error)
	}{{"standalone", renderStandalone}, {"prepared", renderAIGateway}} {
		t.Run(renderer.name, func(t *testing.T) {
			rendered, err := renderer.render(t, aiGatewayHermesInput("OPENAI_API_KEY"))
			if err != nil {
				t.Fatal(err)
			}
			got := parseHermes(t, rendered)
			var keys []corev1.EnvVar
			for _, env := range got.main.Env {
				if env.Name == "OPENAI_API_KEY" {
					keys = append(keys, env)
				}
			}
			if len(keys) != 1 || keys[0].ValueFrom == nil || keys[0].ValueFrom.SecretKeyRef == nil ||
				keys[0].ValueFrom.SecretKeyRef.Name != "ai-gateway-virtual-key" {
				t.Fatalf("OPENAI_API_KEY must be set once, from the key Secret: %+v", keys)
			}
			providers, _ := got.config["providers"].(map[string]any)
			if provider, _ := providers["cetirun-gateway"].(map[string]any); provider["key_env"] != "OPENAI_API_KEY" {
				t.Fatalf("the Gateway provider must read OPENAI_API_KEY: %v", provider)
			}
			if keyEnv := envVar(got.init, "CETIRUN_GATEWAY_KEY_ENV"); keyEnv == nil || keyEnv.Value != "OPENAI_API_KEY" {
				t.Fatalf("init must know the key variable, got %+v", keyEnv)
			}
			if _, err := renderer.render(t, aiGatewayHermesInput("PATH")); err == nil || !strings.Contains(err.Error(), "must not be PATH") {
				t.Fatalf("binding the key to PATH must be refused, got %v", err)
			}
		})
	}

	standalone := func(t *testing.T, keyEnv string) (map[string]string, error) {
		return renderStandalone(t, aiGatewayHermesInput(keyEnv))
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		if _, err := standalone(t, name); err != nil {
			t.Fatalf("an API key variable, %s, can carry the key: %v", name, err)
		}
	}

	rendered, err := standalone(t, "")
	if err != nil {
		t.Fatal(err)
	}
	defaults := parseHermes(t, rendered)
	if defaults.envFile == "" {
		t.Fatal("hermes.env is not rendered")
	}
	names := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(defaults.envFile), "\n") {
		name, _, _ := strings.Cut(line, "=")
		names[name] = struct{}{}
	}
	for _, env := range defaults.main.Env {
		names[env.Name] = struct{}{}
	}
	for _, name := range []string{"PATH", "HOME", "SHELL", "USER", "IFS", "ENV", "BASH_ENV", "TMPDIR",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "PYTHONPATH", "PYTHONHOME"} {
		names[name] = struct{}{}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		// The model provider keys of the generated .env can carry the key,
		// as its default variable does.
		if name == "OPENAI_API_KEY" || name == "ANTHROPIC_API_KEY" || name == "CETIRUN_GATEWAY_API_KEY" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			_, err := standalone(t, name)
			if err == nil || !strings.Contains(err.Error(), "must not be "+name) {
				t.Fatalf("binding the key to %s must be refused, got %v", name, err)
			}
		})
	}
}
