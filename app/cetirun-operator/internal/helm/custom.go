// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/custom"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
)

func prepareCustomChart(chrt *chart.Chart, input map[string]any, request AgentRequest, validateRender bool) (*PreparedRelease, error) {
	if len(chrt.CRDObjects()) != 0 {
		return nil, fmt.Errorf("Custom charts cannot contain CRDs")
	}
	// Typed receipts cannot be supplied through JSON Helm overrides.
	bindings, ok := input["customBindings"].(runtimebindings.Custom)
	if !ok {
		return nil, fmt.Errorf("CustomBindingsPending: platform bindings are required")
	}
	if bindings.Home.Name != "data-"+text(input["fullnameOverride"])+"-0" {
		return nil, fmt.Errorf("Custom HOME name does not match its StatefulSet claim template")
	}
	overrides := map[string]any{}
	for key, value := range input {
		switch key {
		case "agentRequest", "agentType", "customBindings", "replicaCount", "fullnameOverride", "memoryOptions":
		default:
			overrides[key] = value
		}
	}
	if err := runtimebindings.ValidateOverrides(overrides); err != nil {
		return nil, err
	}
	agent, err := agents.ResolveCustom(request.Request)
	if err != nil {
		return nil, err
	}
	if memory, configured, err := resolveMemory(input, chrt, agent.HomeDir); err != nil {
		return nil, err
	} else if configured {
		if bindings.Memory != nil {
			return nil, fmt.Errorf("Memory binding and platform options cannot both be supplied")
		}
		bindings.Memory = memory
	}
	if err := bindings.Validate(agent); err != nil {
		return nil, err
	}
	if bindings.PullSecretName != request.PullSecretName {
		return nil, fmt.Errorf("RegistryBindingInvalid: pull binding does not match the approved request")
	}
	if input["agentType"] != "custom" {
		return nil, fmt.Errorf("custom runtime type mismatch")
	}
	if request.ReleaseName == "" {
		return nil, fmt.Errorf("custom requires a platform release name")
	}
	contribution, err := (custom.Harness{}).Build(agent, bindings.HarnessBindings())
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	for key, value := range input {
		if key != "customBindings" && key != "agentRequest" && key != "memoryOptions" {
			values[key] = value
		}
	}
	// Coalesce shared desktop and control settings with the Custom contribution.
	// Native Agent configuration is not used by Custom rendering.
	coalesced, err := chartutil.CoalesceValues(chrt, values)
	if err != nil {
		return nil, fmt.Errorf("coalesce custom values")
	}
	// Native preparation is coalesced again at render time. Resolve that same
	// effective configuration before computing Custom's expected objects.
	coalesced, err = chartutil.CoalesceValues(chrt, coalesced)
	if err != nil {
		return nil, fmt.Errorf("coalesce effective custom values")
	}
	if bindings.Memory != nil {
		if _, ok := chrt.Values["memoryContext"]; !ok {
			return nil, fmt.Errorf("the selected Chart does not support Memory Context")
		}
	}
	if _, supported := chrt.Values["memoryContext"]; supported {
		for key, value := range bindings.Memory.Values() {
			ensureObject(coalesced, "memoryContext")[key] = value
		}
	}
	for _, key := range []string{"runtimeClassName", "nodeSelector"} {
		if value, present := input[key]; present && value == nil {
			delete(coalesced, key)
		}
	}
	if _, err := runtimebindings.ValidateResources(coalesced["resources"]); err != nil {
		return nil, err
	}
	if err := runtimebindings.ValidateScheduling(coalesced); err != nil {
		return nil, err
	}
	// Custom terminals are platform workloads. The Agent image retains its
	// entrypoint, environment and declared HTTP port.
	ensureObject(coalesced, "ttyd")["shell"] = "/bin/sh"
	// Older runtime Charts have no terminal image/plan support. Preserve their
	// existing Custom behavior rather than enabling a missing workload.
	if object(object(chrt.Values["ttyd"])["image"])["repository"] == nil {
		ensureObject(coalesced, "ttyd")["enabled"] = false
		bindings.Network.APIServer = nil
	}
	// Custom publishes the approved view at its contract path, independent of
	// the native agents' discovery paths retained in shared platform settings.
	ensureObject(coalesced, "skills")["mountPath"] = agent.HomeDir + "/.agents/skills"
	if err := validateCustomPlatformValues(coalesced, agent, ""); err != nil {
		return nil, err
	}
	// Use the existing chart contract so an older pin can restart and resume.
	if bindings.RestartRequestID != "" {
		ensureObject(coalesced, "podAnnotations")[api.AnnotationRestartRequestID] = bindings.RestartRequestID
	}
	raw, err := json.Marshal(contribution)
	if err != nil {
		return nil, err
	}
	var runtime map[string]any
	if err := json.Unmarshal(raw, &runtime); err != nil {
		return nil, err
	}
	raw, err = json.Marshal(bindings)
	if err != nil {
		return nil, err
	}
	var receipts map[string]any
	if err := json.Unmarshal(raw, &receipts); err != nil {
		return nil, err
	}
	runtime["bindings"] = receipts
	runtime["contractVersion"] = "v1"
	coalesced["runtime"] = runtime
	terminalPlan := &PreparedRelease{Values: coalesced, Agent: agent, chart: chrt, releaseName: request.ReleaseName}
	copilotResources, err := terminalPlan.customCopilotObjects(bindings)
	if err != nil {
		return nil, err
	}
	if len(copilotResources) > 0 {
		runtime["copilotResources"] = copilotResources
	}
	terminalResources, err := terminalPlan.terminalObjects(bindings)
	if err != nil {
		return nil, err
	}
	if len(terminalResources) > 0 {
		runtime["terminalResources"] = terminalResources
		fullname := text(coalesced["fullnameOverride"])
		runtime["terminalExecRule"] = terminalpolicy.AdmissionExpression(
			bindings.Namespace.Name, fullname+"-0", chrt.Name(), terminalpolicy.WorkloadName(fullname), bindings.AgentUID)
	}
	workloadHash, err := customWorkloadDigest(chrt, agent, bindings, contribution, coalesced)
	if err != nil {
		return nil, err
	}
	runtime["workloadHash"] = workloadHash
	// Hash the selected platform settings and typed inputs, not native configuration.
	hashInput := map[string]any{
		"runtime": runtime, "platform": runtimebindings.PlatformValues(coalesced), "chartVersion": chrt.Metadata.Version,
		"replicaCount": coalesced["replicaCount"], "fullnameOverride": coalesced["fullnameOverride"], "releaseName": request.ReleaseName,
	}
	raw, err = json.Marshal(hashInput)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	planHash := hex.EncodeToString(sum[:])
	runtime["planHash"] = planHash
	if err := chartutil.ValidateAgainstSchema(chrt, coalesced); err != nil {
		return nil, fmt.Errorf("custom values do not match chart schema")
	}
	// Helm coalesces once more when rendering the action. Retain explicit
	// clearing markers so omitted keys do not regain the chart defaults.
	for _, key := range []string{"runtimeClassName", "nodeSelector"} {
		if value, present := input[key]; present && value == nil {
			coalesced[key] = nil
		}
	}
	p := &PreparedRelease{Values: coalesced, Agent: agent, ChartVersion: chrt.Metadata.Version,
		chart: chrt, PlanHash: planHash, basePlanHash: planHash, WorkloadHash: workloadHash,
		releaseName: request.ReleaseName, custom: true}
	if validateRender {
		if err := p.ValidateCustom(bindings.Namespace.Name, request.ReleaseName); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func customWorkloadDigest(chrt *chart.Chart, agent agents.ResolvedAgent, bindings runtimebindings.Custom, contribution agents.Contribution, values map[string]any) (string, error) {
	settings := runtimebindings.WorkloadValues(values)
	home := map[string]any{"name": bindings.Home.Name, "fsGroup": bindings.Home.FSGroup, "claim": bindings.Home.Claim}
	desktop := object(values["desktop"])
	if desktop["enabled"] == true {
		settings["desktopVersion"] = chrt.Metadata.AppVersion
	}
	input := map[string]any{
		"agent": agent, "contribution": contribution, "settings": settings,
		"agentUID": bindings.AgentUID,
		"home":     home,
		"model":    bindings.Model, "context": bindings.Context,
		"commonInit": string(asset(chrt, "configs/init-home-common.sh")),
		"customInit": string(asset(chrt, "agents/custom/bootstrap/init-home.sh")),
	}
	if bindings.Memory != nil {
		input["memory"] = bindings.Memory
	}
	if bindings.RestartRequestID != "" {
		input["restartRequestID"] = bindings.RestartRequestID
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
