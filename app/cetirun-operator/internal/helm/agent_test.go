// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"sigs.k8s.io/yaml"
)

func testChart(t *testing.T) *chart.Chart {
	t.Helper()
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	return chrt
}

func useNativeBaselineVersion(chrt *chart.Chart) {
	// Keep the original fixtures independent of release-version changes.
	// TestNativeReleaseVersionWiring checks the current release separately.
	chrt.Metadata.Version = "0.1.0"
	chrt.Metadata.AppVersion = "0.1.0"
}

func sampleInput(typ, provider string, integrations bool) map[string]any {
	v := map[string]any{
		"agentType":    typ,
		"agentRequest": map[string]any{"type": typ},
		"secrets": map[string]any{
			"existingSecret": "test-secret", "provider": "openai", "model": "test-model",
			"apiBase": "http://cetirun-ai-gateway.cetirun-system.svc.cluster.local:4000/v1", "authMode": "api_key",
		},
		"aiGateway": map[string]any{
			"enabled": true, "keySecretName": "ai-gateway-virtual-key", "keySecretKey": "api-key",
			"models":       []any{map[string]any{"id": "test-model", "name": "Test model"}},
			"copilotProxy": map[string]any{"enabled": false},
			"hermes":       map[string]any{"keyEnv": "CETIRUN_GATEWAY_API_KEY"},
		},
		"networkPolicy": map[string]any{"gatewayNamespace": "cetirun-system", "gatewayPort": 4000},
	}
	if typ == "hermes" {
		v["hermes"] = map[string]any{"env": map[string]any{"HERMES_WEBAPP_PUBLIC_URL": "https://agents.example.com/ui/example"}}
	}
	if integrations {
		v["skills"] = map[string]any{
			"shared": true, "resourceGroup": "test", "storageAccount": "test",
			"containerName": "skills", "npm": []any{"@example/skill@1.0.0", "example-skill@2.0.0"},
		}
		v["ttyd"] = map[string]any{"enabled": true}
		secrets := v["secrets"].(map[string]any)
		for _, key := range []string{"tailscaleAuthKey", "telegramBotToken", "slackBotToken", "slackAppToken", "discordBotToken", "feishuAppId", "feishuAppSecret"} {
			secrets[key] = "test-value"
		}
		if provider == "github_copilot" {
			secrets["braveApiKey"] = "test-value"
		} else {
			secrets["perplexityApiKey"] = "test-value"
		}
	}
	return v
}

type renderBaseline struct {
	Native map[string]any    `json:"native"`
	Hashes map[string]string `json:"manifestHashes"`
}

// normalizeNativeGatewayModel removes the configuration owned by the model
// transport. TestNativeRenderBaseline deliberately retains the pre-Gateway
// fixtures so it can continue to detect unrelated native configuration
// changes. Model/provider/auth wiring is intentionally different when every
// native Agent reaches a model through the AI Gateway and is asserted by the
// focused Gateway tests instead.
func normalizeNativeGatewayModel(native map[string]any) map[string]any {
	raw, err := json.Marshal(native)
	if err != nil {
		panic(err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		panic(err)
	}

	if hermes, ok := normalized["hermes-config.yaml"].(map[string]any); ok {
		delete(hermes, "model")
		delete(hermes, "providers")
	}
	if openclaw, ok := normalized["openclaw.json"].(map[string]any); ok {
		delete(openclaw, "auth")
		delete(openclaw, "models")
		defaults := object(object(openclaw["agents"])["defaults"])
		for _, key := range []string{"model", "models", "modelPolicy", "utilityModel"} {
			delete(defaults, key)
		}
	}
	return normalized
}

func renderSnapshot(t *testing.T, chrt *chart.Chart, values map[string]any) renderBaseline {
	t.Helper()
	options := chartutil.ReleaseOptions{Name: "openclaw-example", Namespace: "cetirun-example", Revision: 1, IsInstall: true}
	renderValues, err := chartutil.ToRenderValues(chrt, values, options, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chrt, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	out := renderBaseline{Native: map[string]any{}, Hashes: map[string]string{}}
	for filename, content := range rendered {
		if strings.HasSuffix(filename, "NOTES.txt") || strings.TrimSpace(content) == "" {
			continue
		}
		// Compare the pre-refactor manifests except the intentionally rewritten
		// init assets and seed-image binding, covered by TestInitRuntimeWiring
		// and the executable initialization tests.
		var objects []any
		for _, doc := range strings.Split(content, "\n---") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				t.Fatalf("%s: %v", filename, err)
			}
			if obj == nil {
				continue
			}
			if obj["kind"] == "ValidatingAdmissionPolicy" && os.Getenv("HARNESS_BASELINE_CHART") == "" {
				spec := object(obj["spec"])
				if _, present := spec["matchConditions"]; present {
					t.Fatal("current native policy must not contain an admin exception")
				}
				// Restore only this intentionally removed field in the comparison
				// copy, keeping the original fixture hashes and all other checks.
				spec["matchConditions"] = []any{map[string]any{"name": "exclude-adminclaw",
					"expression": `request.userInfo.username != "system:serviceaccount:cetirun-admin:openclaw-admin-cetirun"`}}
			}
			if obj["kind"] == "ConfigMap" {
				data := object(obj["data"])
				for _, key := range []string{"init-home.sh", "hermes-init-home.sh", "init-home-common.sh", "hermes-apply-config.py"} {
					delete(data, key)
				}
				for _, key := range []string{"openclaw.json", "hermes-config.yaml"} {
					if source, ok := data[key].(string); ok {
						var native map[string]any
						if err := yaml.Unmarshal([]byte(source), &native); err != nil {
							t.Fatal(err)
						}
						data[key] = native
						out.Native[key] = native
					}
				}
			}
			if obj["kind"] == "StatefulSet" {
				if chrt.Metadata.Annotations["cetirun.microsoft.com/registry-credentials"] == "v1" && object(values["runtime"])["preserveRunningPod"] != true {
					spec, metadata := object(obj["spec"]), object(obj["metadata"])
					annotations := object(metadata["annotations"])
					if !reflect.DeepEqual(spec["updateStrategy"], map[string]any{"type": "RollingUpdate"}) || annotations["cetirun.microsoft.com/registry-binding-only"] != "false" {
						t.Fatal("normal runtime does not explicitly restore rolling updates")
					}
					// Assert the explicit defaults before comparing the older manifests.
					// Registry three-way merge tests cover their update behavior.
					delete(spec, "updateStrategy")
					delete(annotations, "cetirun.microsoft.com/registry-binding-only")
					if len(annotations) == 0 {
						delete(metadata, "annotations")
					}
				}
				template := object(object(obj["spec"])["template"])
				annotations := object(object(template["metadata"])["annotations"])
				delete(annotations, "checksum/config")
				for _, container := range array(object(template["spec"])["initContainers"]) {
					c := object(container)
					if c["name"] != "init-home-data" {
						continue
					}
					var env []any
					for _, item := range array(c["env"]) {
						if object(item)["name"] != "CETIRUN_INIT_IMAGE" {
							env = append(env, item)
						}
					}
					if len(env) == 0 {
						delete(c, "env")
					} else {
						c["env"] = env
					}
				}
			}
			objects = append(objects, obj)
		}
		raw, err := json.Marshal(objects)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		out.Hashes[filepath.Base(filename)] = hex.EncodeToString(sum[:])
	}
	return out
}

func TestInitRuntimeWiring(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw"} {
		chrt := testChart(t)
		prepared, err := PrepareChart(chrt, sampleInput(typ, "openai", true))
		if err != nil {
			t.Fatal(err)
		}
		values, err := chartutil.ToRenderValues(chrt, prepared.Values,
			chartutil.ReleaseOptions{Name: "example", Namespace: "example", IsInstall: true}, chartutil.DefaultCapabilities)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := engine.Render(chrt, values)
		if err != nil {
			t.Fatal(err)
		}
		var stateful, config map[string]any
		if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), &stateful); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/configmap.yaml"]), &config); err != nil {
			t.Fatal(err)
		}
		data := object(config["data"])
		if data["init-home-common.sh"] != string(asset(chrt, "configs/init-home-common.sh")) {
			t.Fatal("common init asset missing or truncated")
		}
		if typ == "hermes" && data["hermes-apply-config.py"] != string(asset(chrt, "agents/hermes/bootstrap/hermes-apply-config.py")) {
			t.Fatal("Hermes file applier missing or truncated")
		}
		pod := object(object(object(stateful["spec"])["template"])["spec"])
		image := object(array(pod["containers"])[0])["image"]
		init := array(pod["initContainers"])
		names := []string{}
		for _, item := range init {
			c := object(item)
			names = append(names, text(c["name"]))
			if c["name"] == "memory-assets" {
				if c["image"] != "mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-memory-context:"+chrt.Metadata.AppVersion {
					t.Fatal("Memory init does not use the release image")
				}
				continue
			}
			if c["image"] != image {
				t.Fatalf("%s init image differs from main image", c["name"])
			}
		}
		want := []string{"memory-assets", "init-home-data", "init-skills"}
		if typ == "openclaw" {
			want = []string{"memory-assets", "init-home-data", "doctor", "init-skills"}
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("%s init order: %v", typ, names)
		}
		seed := object(init[1])
		bound := false
		for _, item := range array(seed["env"]) {
			if object(item)["name"] == "CETIRUN_INIT_IMAGE" {
				bound = object(item)["value"] == image
			}
		}
		if !bound {
			t.Fatal("seed image identity is not bound to the selected image")
		}
		for _, item := range array(seed["volumeMounts"]) {
			if object(item)["name"] == "data" && object(item)["mountPath"] != "/home-data" {
				t.Fatal("seed volume shadows image HOME")
			}
		}
	}
}

func TestNativeRenderBaseline(t *testing.T) {
	chrt := testChart(t)
	baselineChart := os.Getenv("HARNESS_BASELINE_CHART")
	if baselineChart != "" {
		var err error
		chrt, err = loader.Load(baselineChart)
		if err != nil {
			t.Fatal(err)
		}
	}
	useNativeBaselineVersion(chrt)
	for _, typ := range []string{"hermes", "openclaw"} {
		for _, provider := range []string{"github_copilot", "openai", "anthropic"} {
			for _, enabled := range []bool{false, true} {
				name := typ + "-" + provider
				if enabled {
					name += "-integrations"
				}
				t.Run(name, func(t *testing.T) {
					input := sampleInput(typ, provider, enabled)
					// Keep historical render inputs fixed; current MCR/private
					// coordinates are covered by TestNativeReleaseVersionWiring.
					input["global"] = map[string]any{"registry": "cetirun.azurecr.io"}
					input["agentRequest"].(map[string]any)["registry"] = "cetirun.azurecr.io"
					// The original snapshots predate Memory; its default-on delivery
					// has dedicated tests and must not rewrite these baselines.
					input["memoryContext"] = map[string]any{"enabled": false}
					// Pin the original StorageClass input; defaults are tested separately.
					input["persistence"] = map[string]any{"storageClass": ""}
					path := filepath.Join("testdata", "harness", name+".json")
					if baselineChart != "" || os.Getenv("HARNESS_UPDATE_FIXTURES") == "true" {
						snapshot := renderSnapshot(t, chrt, input)
						raw, err := json.MarshalIndent(snapshot, "", "  ")
						if err != nil {
							t.Fatal(err)
						}
						if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, append(raw, '\n'), 0644); err != nil {
							t.Fatal(err)
						}
						return
					}
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var want renderBaseline
					if err := json.Unmarshal(raw, &want); err != nil {
						t.Fatal(err)
					}
					before, _ := json.Marshal(input)
					prepared, err := PrepareChart(chrt, input)
					if err != nil {
						t.Fatal(err)
					}
					after, _ := json.Marshal(input)
					if string(before) != string(after) {
						t.Fatal("PrepareChart mutated input")
					}
					for mode, values := range map[string]map[string]any{"prepared": prepared.Values, "standalone": input} {
						got := renderSnapshot(t, chrt, values)
						if !reflect.DeepEqual(normalizeNativeGatewayModel(got.Native), normalizeNativeGatewayModel(want.Native)) {
							t.Errorf("%s native config differs from pre-refactor baseline", mode)
						}
						for _, file := range []string{"deny-exec-policy.yaml", "pre-upgrade-hook.yaml"} {
							hash := want.Hashes[file]
							if got.Hashes[file] != hash {
								t.Errorf("%s %s differs from pre-refactor manifest", mode, file)
							}
						}
						if len(got.Hashes) != len(want.Hashes) {
							t.Errorf("%s manifest inventory changed", mode)
						}
					}
					repeated, err := PrepareChart(chrt, input)
					if err != nil || !reflect.DeepEqual(prepared.Values, repeated.Values) {
						t.Fatal("non-deterministic preparation")
					}
				})
			}
		}
	}
}

func TestRuntimeProfileValidation(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw"} {
		t.Run(typ, func(t *testing.T) {
			chrt := testChart(t)
			input := sampleInput(typ, "openai", false)
			prepared, err := PrepareChart(chrt, input)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Agent.Image != "mcr.microsoft.com/aks/ai-runtime/cetirun/"+typ+"-gateway:"+chrt.Metadata.AppVersion || prepared.Agent.Username != "vibe" {
				t.Fatalf("wrong profile defaults: %+v", prepared.Agent)
			}
			request := input["agentRequest"].(map[string]any)
			request["image"] = prepared.Agent.Image
			request["port"] = 8080
			prepared, err = PrepareChart(chrt, input)
			if err != nil || prepared.Agent.Port == nil || *prepared.Agent.Port != 8080 {
				t.Fatalf("qualified image and non-conflicting port: %v", err)
			}
			renderSnapshot(t, chrt, prepared.Values)
			for _, port := range []int{18790, 5900, 6080, 18800, -1, 65536} {
				request["port"] = port
				if _, err := PrepareChart(chrt, input); err == nil {
					t.Errorf("invalid/conflicting port %d accepted", port)
				}
			}
			request["port"] = 5900
			input["desktop"] = map[string]any{"enabled": false}
			prepared, err = PrepareChart(chrt, input)
			if err != nil || prepared.Agent.Port == nil || *prepared.Agent.Port != 5900 {
				t.Fatalf("disabled desktop should release port 5900: %v", err)
			}
			renderSnapshot(t, chrt, prepared.Values)
			request["port"] = 8080
			request["image"] = "registry.example.com/unqualified:1"
			if _, err := PrepareChart(chrt, input); err == nil {
				t.Fatal("unqualified image accepted")
			}
			delete(request, "image")
			input["runtime"] = map[string]any{"agent": map[string]any{"image": "injected"}}
			if _, err := PrepareChart(chrt, input); err == nil {
				t.Fatal("injected plan accepted")
			}
		})
	}
	for _, typ := range []string{"", "unknown", "custom"} {
		if _, err := PrepareChart(testChart(t), sampleInput(typ, "openai", false)); err == nil {
			t.Errorf("unsupported type %q accepted", typ)
		}
	}
}

func TestNativeReleaseVersionWiring(t *testing.T) {
	currentVersion := testChart(t).Metadata.Version
	for _, typ := range []string{"hermes", "openclaw"} {
		for _, version := range []string{currentVersion, "9.8.7-dev.1"} {
			for _, registry := range []string{"", "registry.example.com", "registry.example.com:5000/team/test"} {
				t.Run(typ+"/"+version+"/"+registry, func(t *testing.T) {
					chrt := testChart(t)
					chrt.Metadata.Version = version
					chrt.Metadata.AppVersion = version
					input := sampleInput(typ, "openai", true)
					input["agentRequest"].(map[string]any)["registry"] = registry
					input["global"] = map[string]any{"registry": registry}
					prepared, err := PrepareChart(chrt, input)
					if err != nil {
						t.Fatal(err)
					}
					prefix := registry
					if prefix == "" {
						prefix = "mcr.microsoft.com/aks/ai-runtime/cetirun"
					}
					wantImage := prefix + "/" + typ + "-gateway:" + version
					if prepared.ChartVersion != version || prepared.Agent.Image != wantImage {
						t.Fatal("prepared runtime does not use the release version")
					}
					for mode, input := range map[string]map[string]any{"prepared": prepared.Values, "standalone": input} {
						values, err := chartutil.ToRenderValues(chrt, input,
							chartutil.ReleaseOptions{Name: "example", Namespace: "example", IsInstall: true}, chartutil.DefaultCapabilities)
						if err != nil {
							t.Fatal(err)
						}
						rendered, err := engine.Render(chrt, values)
						if err != nil {
							t.Fatal(err)
						}
						var stateful map[string]any
						if err := yaml.Unmarshal([]byte(rendered["cetirun/templates/statefulset.yaml"]), &stateful); err != nil {
							t.Fatal(err)
						}
						labels := object(object(stateful["metadata"])["labels"])
						if labels["helm.sh/chart"] != "cetirun-"+version || labels["app.kubernetes.io/version"] != version {
							t.Errorf("%s release labels do not use %s", mode, version)
						}
						pod := object(object(object(stateful["spec"])["template"])["spec"])
						images := map[string]string{}
						for _, item := range array(pod["containers"]) {
							container := object(item)
							images[text(container["name"])] = text(container["image"])
						}
						for name, want := range map[string]string{
							"cetirun": wantImage,
							"desktop": prefix + "/cetirun-desktop:" + version,
						} {
							if images[name] != want {
								t.Errorf("%s %s image: got %q, want %q", mode, name, images[name], want)
							}
						}
						for _, item := range array(pod["initContainers"]) {
							expected := wantImage
							if object(item)["name"] == "memory-assets" {
								expected = prefix + "/cetirun-memory-context:" + version
							}
							if object(item)["image"] != expected {
								t.Errorf("%s init image does not use %s", mode, version)
							}
						}
					}
				})
			}
		}
	}
}

func TestCustomReleaseCoordinates(t *testing.T) {
	for _, registry := range []string{"mcr.microsoft.com/aks/ai-runtime/cetirun", "registry.example.com:5000/team/test"} {
		input := customInput(8080)
		input["global"] = map[string]any{"registry": registry, "version": "1.2.3-dev.1"}
		prepared, err := PrepareChart(testChart(t), input)
		if err != nil {
			t.Fatal(err)
		}
		if got := prepared.terminalImage(); got != registry+"/cetirun-terminal:1.2.3-dev.1" {
			t.Fatalf("terminal image: %s", got)
		}
		if got := prepared.desktopContainer().Image; got != registry+"/cetirun-desktop:1.2.3-dev.1" {
			t.Fatalf("desktop image: %s", got)
		}
	}
}

func TestNativePreferencesAndCredentialBoundary(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw"} {
		input := sampleInput(typ, "openai", true)
		input["secrets"].(map[string]any)["apiKey"] = "secret-test-sentinel"
		input[typ] = map[string]any{"config": map[string]any{"logging": map[string]any{"level": "debug"}}}
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatal("direct provider credential was accepted")
		}
		delete(input["secrets"].(map[string]any), "apiKey")
		prepared, err := PrepareChart(testChart(t), input)
		if err != nil {
			t.Fatal(err)
		}
		runtime := prepared.Values["runtime"].(map[string]any)
		raw, err := json.Marshal(runtime)
		if err != nil || strings.Contains(string(raw), "secret-test-sentinel") {
			t.Fatal("credential value reached the runtime contribution")
		}
		files := runtime["configFiles"].(map[string]any)
		for _, content := range files {
			var config map[string]any
			if err := yaml.Unmarshal([]byte(content.(string)), &config); err != nil {
				t.Fatal(err)
			}
			if object(config["logging"])["level"] != "debug" {
				t.Fatal("native preference was lost")
			}
		}
		input[typ] = map[string]any{"config": "invalid"}
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatal("malformed native config was silently discarded")
		}
	}
}

func TestOpenClawTailscaleTrustsServeLoopback(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		want    []any
	}{
		{name: "disabled", want: []any{"127.0.0.6"}},
		{name: "enabled", enabled: true, want: []any{"127.0.0.6", "127.0.0.1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := sampleInput("openclaw", "openai", false)
			if test.enabled {
				input["secrets"].(map[string]any)["tailscaleAuthKey"] = "test-value"
			}
			chrt := testChart(t)
			prepared, err := PrepareChart(chrt, input)
			if err != nil {
				t.Fatal(err)
			}
			files := object(object(prepared.Values["runtime"])["configFiles"])
			var preparedConfig map[string]any
			if err := yaml.Unmarshal([]byte(text(files["openclaw.json"])), &preparedConfig); err != nil {
				t.Fatal(err)
			}
			standaloneConfig := object(renderSnapshot(t, chrt, input).Native["openclaw.json"])
			for mode, config := range map[string]map[string]any{
				"prepared":   preparedConfig,
				"standalone": standaloneConfig,
			} {
				got := array(object(config["gateway"])["trustedProxies"])
				if !reflect.DeepEqual(got, test.want) {
					t.Errorf("%s trusted proxies: got %v, want %v", mode, got, test.want)
				}
			}
		})
	}
}

// The operator renders an Agent's configuration from the chart's native asset
// itself; Portal sign-in must come out the same as from the chart alone, and
// stay off with Tailscale.
func TestOpenClawPortalSignInRendersTrustedProxyAuth(t *testing.T) {
	want := map[string]any{
		"mode": "trusted-proxy",
		"trustedProxy": map[string]any{
			"userHeader": "x-auth-request-email", "allowLoopback": true,
			"deviceAutoApprove": map[string]any{"enabled": true},
		},
	}
	for _, test := range []struct {
		name      string
		enabled   bool
		tailscale bool
		want      any
	}{
		{name: "off"},
		{name: "on", enabled: true, want: want},
		{name: "on with Tailscale", enabled: true, tailscale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := sampleInput("openclaw", "openai", false)
			if test.enabled {
				input["openclaw"] = map[string]any{"portalSignIn": map[string]any{"enabled": true}}
			}
			if test.tailscale {
				input["secrets"].(map[string]any)["tailscaleAuthKey"] = "test-value"
			}
			chrt := testChart(t)
			prepared, err := PrepareChart(chrt, input)
			if err != nil {
				t.Fatal(err)
			}
			files := object(object(prepared.Values["runtime"])["configFiles"])
			var preparedConfig map[string]any
			if err := yaml.Unmarshal([]byte(text(files["openclaw.json"])), &preparedConfig); err != nil {
				t.Fatal(err)
			}
			standaloneConfig := object(renderSnapshot(t, chrt, input).Native["openclaw.json"])
			for mode, config := range map[string]map[string]any{"prepared": preparedConfig, "standalone": standaloneConfig} {
				got := object(config["gateway"])["auth"]
				if test.want == nil && got != nil || test.want != nil && !reflect.DeepEqual(got, test.want) {
					t.Errorf("%s gateway auth: got %#v, want %#v", mode, got, test.want)
				}
			}
		})
	}
}
func TestOpenClawAcpxAgentOverridesUsePluginConfig(t *testing.T) {
	input := sampleInput("openclaw", "openai", false)
	input["openclaw"] = map[string]any{
		"acpx": map[string]any{
			"agents": map[string]any{
				"codex": map[string]any{
					"command": "custom-codex",
					"args":    []any{"--flag", "value with spaces"},
				},
			},
		},
	}
	prepared, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	runtime := object(prepared.Values["runtime"])
	files := object(runtime["configFiles"])
	var config map[string]any
	if err := yaml.Unmarshal([]byte(text(files["openclaw.json"])), &config); err != nil {
		t.Fatal(err)
	}
	plugins := object(config["plugins"])
	entries := object(plugins["entries"])
	acpx := object(entries["acpx"])
	pluginConfig := object(acpx["config"])
	agents := object(pluginConfig["agents"])
	codex := object(agents["codex"])
	if codex["command"] != "custom-codex" || !reflect.DeepEqual(codex["args"], []any{"--flag", "value with spaces"}) {
		t.Fatalf("ACPX agent override was not rendered into plugin config: %#v", codex)
	}
}

func TestMissingProfileAndQualifiedImage(t *testing.T) {
	input := sampleInput("hermes", "openai", false)
	chrt := testChart(t)
	override := "registry.example.com/hermes:qualified"
	for _, file := range chrt.Files {
		if file.Name == "agents/hermes/profile.yaml" {
			file.Data = []byte("contractVersion: v1\nnativeVersion: \"0.22.7\"\nnativeCommit: b200f5bc60e31588ba2cb60e288029c4a54ba5fa\nwebappPort: 9120\nport: 8642\nqualifiedImages:\n- " + override + "\n")
		}
	}
	input["agentRequest"].(map[string]any)["image"] = override
	prepared, err := PrepareChart(chrt, input)
	if err != nil || prepared.Agent.Image != override {
		t.Fatalf("exact qualified override was not honored: %v", err)
	}
	chrt.Files = nil
	if _, err := PrepareChart(chrt, input); err == nil {
		t.Fatal("chart without a profile fell through to legacy rendering")
	}
}
