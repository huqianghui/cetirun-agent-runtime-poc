// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/builtin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// AgentRequest is assembled by the controller after user overrides are checked.
// It is removed before values are passed to Helm.
type AgentRequest struct {
	agents.Request
	Registry         string                       `json:"registry,omitempty"`
	UIPrefix         string                       `json:"uiPrefix,omitempty"`
	TailscaleEnabled bool                         `json:"tailscaleEnabled,omitempty"`
	ReleaseName      string                       `json:"releaseName,omitempty"`
	PullSecretName   string                       `json:"pullSecretName,omitempty"`
	CetiAdmin        *cetiadmin.RuntimeProjection `json:"cetiadmin,omitempty"`
}

type PreparedRelease struct {
	Values                map[string]any
	Agent                 agents.ResolvedAgent
	ChartVersion          string
	PlanHash              string
	WorkloadHash          string
	basePlanHash          string
	releaseName           string
	namespace             string
	maintenance           *maintenanceMode
	manifestHash          string
	manifest              []byte
	sealedInput           string
	actionRevision        int
	custom                bool
	chart                 *chart.Chart
	sidecarAPIVersion     string
	pdbAPIVersion         string
	homeRetention         bool
	apiValidated          bool
	sandboxBundle         *sandboxworkload.Bundle
	sandboxTemplate       *corev1.PodTemplateSpec
	sandboxReconcile      *sandboxworkload.ReconcileExpectation
	sandboxPreflight      func(context.Context) error
	sandboxRenderGuard    func(context.Context) error
	sandboxGuard          func(context.Context) error
	sandboxBindingCreate  func(context.Context, unstructured.Unstructured) (func(context.Context) error, error)
	sandboxWorkloadCreate func(context.Context) (func(context.Context) error, error)
}

// CacheKey includes actual rendered output as well as the consumed inputs.
func (p *PreparedRelease) CacheKey() string {
	return p.PlanHash + ":" + p.manifestHash
}

// SupportsValue reports whether the loaded chart declares a top-level value.
// The controller uses it before relying on a value that older pinned chart
// versions would silently ignore.
func (p *PreparedRelease) SupportsValue(name string) bool {
	return p.SupportsValuePath(name)
}

// SupportsValuePath reports whether the loaded chart declares a nested value,
// such as aiGateway.models.
func (p *PreparedRelease) SupportsValuePath(path ...string) bool {
	if p == nil || p.chart == nil {
		return false
	}
	return declaresValuePath(p.chart.Values, path)
}

// ChartValues are the values a chart version declares, for checking what it
// supports before an Agent uses it.
type ChartValues struct {
	values        map[string]any
	customGateway bool
}

// NewChartValues describes the values a loaded chart declares.
func NewChartValues(chrt *chart.Chart) *ChartValues {
	if chrt == nil {
		return &ChartValues{}
	}
	return &ChartValues{values: chrt.Values, customGateway: chrt.Metadata != nil && chrt.Metadata.Annotations["cetirun.microsoft.com/custom-copilot"] == "v1"}
}

// SupportsValuePath reports whether the chart declares a nested value.
func (v *ChartValues) SupportsValuePath(path ...string) bool {
	return v != nil && declaresValuePath(v.values, path)
}

func declaresValuePath(values map[string]any, path []string) bool {
	if len(path) == 0 {
		return false
	}
	current := values
	for i, name := range path {
		value, ok := current[name]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		next, ok := value.(map[string]any)
		if !ok {
			return false
		}
		current = next
	}
	return false
}

// Rebuild uses the same loaded chart after platform integration bindings have
// been resolved. It never pulls a second, potentially changed chart.
func (p *PreparedRelease) Rebuild(input map[string]any) (*PreparedRelease, error) {
	if p.chart == nil {
		return nil, fmt.Errorf("prepared release has no chart")
	}
	if p.sandboxBundle != nil {
		return nil, fmt.Errorf("Sandbox rebuild requires separately validated user inputs and a current bundle")
	}
	prepared, err := PrepareChart(p.chart, input)
	if err == nil {
		prepared.namespace = p.namespace
	}
	return prepared, err
}

// PrepareChart is also used by offline rendering tests. It has no Kubernetes or
// Helm actions; chart loading and applying the prepared result stay in Client.
func PrepareChart(chrt *chart.Chart, input map[string]any) (*PreparedRelease, error) {
	prepared, err := prepareChart(chrt, input, true)
	return prepared, configurationError(err)
}

func prepareChart(chrt *chart.Chart, input map[string]any, validateRender bool) (*PreparedRelease, error) {
	if input[sandboxworkload.ValuesKey] != nil {
		return nil, fmt.Errorf("sandboxWorkload is reserved for the protected Sandbox renderer")
	}
	if input["runtime"] != nil {
		return nil, fmt.Errorf("runtime is reserved for the platform")
	}
	if input["cetiadmin"] != nil {
		return nil, fmt.Errorf("cetiadmin is reserved for the platform")
	}
	return prepareRuntimeChart(chrt, input, validateRender, nil)
}

func prepareRuntimeChart(chrt *chart.Chart, input map[string]any, validateRender bool, harness agents.Harness) (*PreparedRelease, error) {
	var request AgentRequest
	requestJSON, err := json.Marshal(input["agentRequest"])
	if err != nil {
		return nil, fmt.Errorf("invalid agent runtime request")
	}
	if err := json.Unmarshal(requestJSON, &request); err != nil {
		return nil, fmt.Errorf("invalid agent runtime request")
	}
	if request.CetiAdmin != nil {
		if request.Type != "hermes" || input[sandboxworkload.ValuesKey] != nil {
			return nil, fmt.Errorf("CetiAdmin requires the ordinary Hermes runtime")
		}
		if !declaresValuePath(chrt.Values, []string{"cetiadmin", "enabled"}) {
			return nil, fmt.Errorf("the selected runtime chart does not support CetiAdmin")
		}
		if err := request.CetiAdmin.Validate(); err != nil {
			return nil, err
		}
		copy := make(map[string]any, len(input))
		for key, value := range input {
			copy[key] = value
		}
		encoded, err := json.Marshal(request.CetiAdmin)
		if err != nil {
			return nil, err
		}
		var projection map[string]any
		if err := json.Unmarshal(encoded, &projection); err != nil {
			return nil, err
		}
		copy["cetiadmin"] = projection
		input = copy
	}
	if request.PullSecretName != "" && chrt.Metadata.Annotations["cetirun.microsoft.com/registry-credentials"] != "v1" {
		return nil, fmt.Errorf("the selected chart does not support managed registry credentials; upgrade the pinned chart first")
	}
	if harness == nil {
		harness, err = builtin.NewRegistry().Lookup(request.Type)
		if err != nil {
			return nil, err
		}
	}
	descriptor := harness.Describe()
	if descriptor.Type != request.Type {
		return nil, fmt.Errorf("harness does not match the runtime request")
	}
	if descriptor.Type == "custom" && input[sandboxworkload.ValuesKey] == nil {
		return prepareCustomChart(chrt, input, request, validateRender)
	}
	memoryHome := "/home/vibe"
	if descriptor.Type == "custom" {
		memoryHome = text(object(object(input["runtime"])["agent"])["home"])
	}
	if memory, configured, err := resolveMemory(input, chrt, memoryHome); err != nil {
		return nil, err
	} else if configured {
		inputCopy := make(map[string]any, len(input))
		for key, value := range input {
			if key != "memoryOptions" && key != "memoryContext" {
				inputCopy[key] = value
			}
		}
		if declaresValuePath(chrt.Values, []string{"memoryContext"}) {
			inputCopy["memoryContext"] = memory.Values()
		}
		input = inputCopy
	}
	if descriptor.Type == "custom" {
		return prepareSandboxCustomChart(chrt, input)
	}
	profile, err := loadRuntimeProfile(chrt, descriptor, request.Registry)
	if err != nil {
		return nil, err
	}
	resolved, err := agents.Resolve(request.Request, profile)
	if err != nil {
		return nil, err
	}
	values, err := prepareRuntimeValues(chrt, input)
	if err != nil {
		return nil, err
	}
	if values["agentType"] != descriptor.Type {
		return nil, fmt.Errorf("agent type does not match runtime request")
	}
	secrets := object(values["secrets"])
	native := object(values[descriptor.Type])
	config := object(native["config"])
	if request.CetiAdmin != nil {
		projection := request.CetiAdmin
		values["serviceAccount"] = map[string]any{"create": false, "name": projection.ServiceAccountName, "role": "", "automountToken": false}
		values["azureWorkloadIdentity"] = map[string]any{"enabled": projection.AzureWorkloadIdentity.Enabled, "clientId": projection.AzureWorkloadIdentity.ClientID}
		if object(values["connectionsMCP"])["enabled"] == true && text(object(values["connectionsMCP"])["audience"]) == projection.Kubernetes.APIAudience {
			return nil, fmt.Errorf("CetiAdmin Kubernetes and MCP audiences must differ")
		}
		skillConfig := ensureObject(config, "skills")
		directories := array(skillConfig["external_dirs"])
		const legacySharedPath = "/home/vibe/.aks/skills"
		shared := object(values["skills"])
		explicit := array(object(object(object(input[descriptor.Type])["config"])["skills"])["external_dirs"])
		if (shared["shared"] != true || text(shared["mountPath"]) != legacySharedPath) && !slices.Contains(explicit, any(legacySharedPath)) {
			// The chart's legacy default is not a source when its alias is unmounted.
			kept := make([]any, 0, len(directories))
			for _, directory := range directories {
				if directory != legacySharedPath {
					kept = append(kept, directory)
				}
			}
			directories = kept
		}
		found := false
		for _, directory := range directories {
			if directory == cetiadmin.AssetsMountPath+"/skills" {
				found = true
			}
		}
		if !found {
			directories = append(directories, cetiadmin.AssetsMountPath+"/skills")
		}
		skillConfig["external_dirs"] = directories
	}
	if descriptor.PreserveUIPrefix {
		gateway := ensureObject(config, "gateway")
		gateway["port"] = *resolved.Port
		if request.UIPrefix != "" {
			ensureObject(gateway, "controlUi")["basePath"] = request.UIPrefix
		}
		ensureObject(values, "service")["port"] = *resolved.Port
		ensureObject(values, "service")["targetPort"] = *resolved.Port
	} else {
		ensureObject(native, "env")["API_SERVER_PORT"] = strconv.Itoa(int(*resolved.Port))
	}
	native["config"] = config
	values[descriptor.Type] = native
	skills := object(values["skills"])
	integrations := agents.Integrations{
		ExistingSecret:    present(secrets["existingSecret"]),
		Brave:             present(secrets["braveApiKey"]),
		Perplexity:        present(secrets["perplexityApiKey"]),
		Telegram:          present(secrets["telegramBotToken"]),
		TelegramTokenFile: text(secrets["telegramTokenFile"]),
		Slack:             present(secrets["slackBotToken"]) && present(secrets["slackAppToken"]),
		Discord:           present(secrets["discordBotToken"]),
		Feishu:            present(secrets["feishuAppId"]) && present(secrets["feishuAppSecret"]),
		Tailscale:         request.TailscaleEnabled || present(secrets["tailscaleAuthKey"]),
	}
	provider := text(secrets["provider"])
	if provider == "" {
		provider = "github_copilot"
	}
	gateway := object(values["aiGateway"])
	var gatewayModels []agents.GatewayModel
	for _, item := range array(gateway["models"]) {
		model := object(item)
		gatewayModels = append(gatewayModels, agents.GatewayModel{
			ID: text(model["id"]), Name: text(model["name"]),
			ContextWindow: integer(model["contextWindow"]), MaxTokens: integer(model["maxTokens"]),
		})
	}
	var visionModels []string
	for _, item := range array(gateway["visionModels"]) {
		visionModels = append(visionModels, text(item))
	}
	bindings := agents.Bindings{
		MemoryEnabled:     (values[sandboxworkload.ValuesKey] == nil || object(values[sandboxworkload.ValuesKey])["reconciled"] == true) && object(values["memoryContext"])["enabled"] == true,
		DesktopEnabled:    object(values["desktop"])["enabled"] == true,
		ProfileID:         profile.ID,
		ChartVersion:      chrt.Metadata.Version,
		NativeConfig:      config,
		NativeOptions:     map[string]any{"acpx": native["acpx"], "plugins": native["plugins"], "portalSignIn": native["portalSignIn"]},
		NativeSandbox:     values[sandboxworkload.ValuesKey] != nil,
		ReconciledSandbox: object(values[sandboxworkload.ValuesKey])["reconciled"] == true,
		IngressMTLS:       object(values[sandboxworkload.ValuesKey])["ingressMTLS"] == true,
		ConnectionsMCP:    agents.ConnectionsMCPBinding{Enabled: object(values["connectionsMCP"])["enabled"] == true, URL: text(object(values["connectionsMCP"])["url"])},
		Model: agents.ModelBinding{
			Provider:            provider,
			Name:                text(secrets["model"]),
			BaseURL:             text(secrets["apiBase"]),
			AuthMode:            text(secrets["authMode"]),
			Gateway:             gateway["enabled"] == true,
			GatewayModels:       gatewayModels,
			GatewayKeyEnv:       text(object(gateway["hermes"])["keyEnv"]),
			GatewayVisionModels: visionModels,
		},
		Skills: agents.SkillsBinding{
			Shared:    skills["shared"] == true,
			Empty:     object(values[sandboxworkload.ValuesKey])["skillsMode"] == string(sandboxworkload.SkillsModeEmpty),
			MountPath: text(skills["mountPath"]),
		},
		Integrations:   integrations,
		ConfigTemplate: string(asset(chrt, "agents/"+descriptor.Type+"/config/native.tpl")),
		// OpenClaw in a chart with the per-Agent Copilot proxy signs in
		// through the Portal and carries no in-agent helper.
		NoSignInHelper: descriptor.Type == "openclaw" &&
			(&PreparedRelease{chart: chrt}).SupportsValuePath("aiGateway", "copilotProxy"),
	}
	if proxies, present := object(values[sandboxworkload.ValuesKey])["trustedProxyIPs"]; present {
		if err := decodeObject(proxies, &bindings.TrustedProxyIPs); err != nil || len(bindings.TrustedProxyIPs) == 0 {
			return nil, fmt.Errorf("native proxy trust requires an explicit compiler-selected IP list")
		}
	}
	// dig expects missing optional maps rather than explicit null.
	for key, value := range bindings.NativeOptions {
		if value == nil {
			delete(bindings.NativeOptions, key)
		}
	}
	for _, item := range array(skills["npm"]) {
		bindings.Skills.NPM = append(bindings.Skills.NPM, text(item))
	}
	if object(values["ttyd"])["enabled"] == true {
		bindings.ReservedPorts = append(bindings.ReservedPorts, agents.PortBinding{
			Name: "ttyd", Number: number(object(values["ttyd"])["port"]),
		})
	}
	if object(values["desktop"])["enabled"] == true {
		bindings.ReservedPorts = append(bindings.ReservedPorts,
			agents.PortBinding{Name: "vnc", Number: 5900},
			agents.PortBinding{Name: "desktop", Number: 6080},
			agents.PortBinding{Name: "browser", Number: 18800})
	}
	contribution, err := harness.Build(resolved, bindings)
	if err != nil {
		return nil, err
	}
	contributionJSON, err := json.Marshal(contribution)
	if err != nil {
		return nil, err
	}
	var runtimeValues map[string]any
	if err := json.Unmarshal(contributionJSON, &runtimeValues); err != nil {
		return nil, err
	}
	runtimeValues["contractVersion"] = "v1"
	if request.PullSecretName != "" {
		runtimeValues["imagePullSecrets"] = []any{map[string]any{"name": request.PullSecretName}}
	}
	values["runtime"] = runtimeValues
	if err := chartutil.ValidateAgainstSchema(chrt, values); err != nil {
		return nil, fmt.Errorf("runtime values do not match the selected chart schema")
	}
	return &PreparedRelease{
		Values:       values,
		Agent:        resolved,
		ChartVersion: chrt.Metadata.Version,
		releaseName:  request.ReleaseName,
		chart:        chrt,
	}, nil
}

func loadRuntimeProfile(chrt *chart.Chart, descriptor agents.Descriptor, registry string) (agents.Profile, error) {
	profilePath := "agents/" + descriptor.Type + "/profile.yaml"
	var definition struct {
		ContractVersion string   `json:"contractVersion"`
		NativeVersion   string   `json:"nativeVersion"`
		NativeCommit    string   `json:"nativeCommit"`
		WebappPort      int32    `json:"webappPort"`
		Port            int32    `json:"port"`
		QualifiedImages []string `json:"qualifiedImages"`
	}
	if err := yaml.UnmarshalStrict(asset(chrt, profilePath), &definition); err != nil ||
		definition.ContractVersion != "v1" || definition.NativeVersion == "" || definition.Port != descriptor.DefaultPort ||
		(descriptor.NativeVersion != "" && (definition.NativeVersion != descriptor.NativeVersion || definition.NativeCommit != descriptor.NativeCommit || definition.WebappPort != descriptor.WebappPort)) {
		return agents.Profile{}, fmt.Errorf("chart lacks a supported %s runtime profile", descriptor.Type)
	}

	image, err := defaultRuntimeImage(chrt, descriptor.Type, registry)
	if err != nil {
		return agents.Profile{}, err
	}
	return agents.Profile{
		ID:              chrt.Metadata.Version + "/" + descriptor.Type + "/" + definition.NativeVersion,
		Image:           image,
		Port:            definition.Port,
		QualifiedImages: definition.QualifiedImages,
	}, nil
}

func defaultRuntimeImage(chrt *chart.Chart, kind, registry string) (string, error) {
	baseImage := object(object(chrt.Values[kind])["image"])
	repository := text(baseImage["repository"])
	if registry != "" {
		repository = registry + "/" + kind + "-gateway"
	}
	tag := text(baseImage["tag"])
	if tag == "" {
		tag = chrt.Metadata.AppVersion
	}
	if repository == "" || tag == "" || tag == "latest" {
		return "", fmt.Errorf("built-in profile requires a versioned default image")
	}
	image := repository + ":" + tag
	if digest := text(baseImage["digest"]); digest != "" {
		image += "@" + digest
	}
	return image, nil
}

func prepareRuntimeValues(chrt *chart.Chart, input map[string]any) (map[string]any, error) {
	coalesced, err := chartutil.CoalesceValues(chrt, input)
	if err != nil {
		return nil, fmt.Errorf("coalesce runtime inputs: %w", err)
	}
	if err := chartutil.ValidateAgainstSchema(chrt, coalesced); err != nil {
		return nil, fmt.Errorf("runtime inputs do not match the selected chart schema")
	}
	delete(coalesced, "agentRequest")

	// JSON round-tripping isolates the nested maps from caller-owned values.
	data, err := json.Marshal(coalesced)
	if err != nil {
		return nil, fmt.Errorf("invalid chart values")
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func asset(chrt *chart.Chart, name string) []byte {
	for _, file := range chrt.Files {
		if file.Name == name {
			return file.Data
		}
	}
	return nil
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}

func ensureObject(parent map[string]any, key string) map[string]any {
	result := object(parent[key])
	parent[key] = result
	return result
}

func text(value any) string {
	result, _ := value.(string)
	return result
}

func present(value any) bool { return text(value) != "" }

func number(value any) int32 {
	n, _ := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(value)), 10, 32)
	return int32(n)
}

// integer reads a whole number that may have been round-tripped through
// JSON as a float, where fmt would print large values in exponent form.
func integer(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func array(value any) []any {
	result, _ := value.([]any)
	return result
}

// SupportsCustomGateway reports the versioned Custom model-binding and proxy
// contract. Native aiGateway value paths alone do not establish Custom support.
func (v *ChartValues) SupportsCustomGateway() bool { return v != nil && v.customGateway }

func (p *PreparedRelease) SupportsCustomGateway() bool {
	return p != nil && p.chart != nil && NewChartValues(p.chart).SupportsCustomGateway()
}
