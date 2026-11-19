// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/observability"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	strictjson "sigs.k8s.io/json"
)

// ValidateUserValues must run on the original per-Agent overrides, before
// merging any defaults. Protected input is rejected, not silently overwritten.
func ValidateUserValues(values map[string]any, authorizedSecrets []string) error {
	for key, value := range values {
		switch key {
		case "secrets":
			fields, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("secrets must be an object")
			}
			for field, raw := range fields {
				text, ok := raw.(string)
				if !ok || text == "" {
					return fmt.Errorf("secrets.%s must be a non-empty string", field)
				}
				switch field {
				case "existingSecret":
					if !slices.Contains(authorizedSecrets, text) {
						return fmt.Errorf("secrets.existingSecret is not authorized")
					}
				case "model":
				default:
					return fmt.Errorf("secrets.%s is protected", field)
				}
			}
		case "extraEnv":
			raw, err := json.Marshal(value)
			if err != nil || string(raw) == "null" {
				return fmt.Errorf("extraEnv must be a typed environment list")
			}
			var env []corev1.EnvVar
			problems, err := strictjson.UnmarshalStrict(raw, &env)
			if err != nil || len(problems) != 0 {
				return fmt.Errorf("invalid extraEnv")
			}
			seen := map[string]bool{}
			for _, item := range env {
				if len(validation.IsEnvVarName(item.Name)) != 0 || seen[item.Name] || protectedEnvironment(item.Name) {
					return fmt.Errorf("extraEnv contains a protected or duplicate name")
				}
				seen[item.Name] = true
				if item.ValueFrom != nil {
					from := item.ValueFrom
					if item.Value != "" || from.SecretKeyRef == nil || from.ConfigMapKeyRef != nil ||
						from.FieldRef != nil || from.ResourceFieldRef != nil ||
						!slices.Contains(authorizedSecrets, from.SecretKeyRef.Name) || from.SecretKeyRef.Key == "" {
						return fmt.Errorf("extraEnv references an unauthorized source")
					}
				}
			}
		default:
			return fmt.Errorf("Sandbox Helm value %q is protected or unsupported", key)
		}
	}
	return nil
}

func ValidateReconciledUserValues(values map[string]any, authorizedSecrets []string) error {
	for name, value := range values {
		switch name {
		case "secrets", "extraEnv":
			if err := ValidateUserValues(map[string]any{name: value}, authorizedSecrets); err != nil {
				return err
			}
		case ValuesKey, "runtime", "runtimeClassName", "replicaCount", "serviceAccount", "azureWorkloadIdentity", "agentIdentity", "connectionsMCP",
			"persistence", "resources", "securityContext", "podSecurityContext", "networkPolicy", "sidecarScope",
			"autoscaling", "ingress", "execRestriction":
			return fmt.Errorf("Sandbox Helm value %q is controlled by the selected Sandbox or AgentPolicy", name)
		case "podAnnotations":
			annotations, _ := value.(map[string]any)
			for key := range annotations {
				if observability.CollectorAnnotation(key) {
					return fmt.Errorf("pod annotation %s is reserved for the platform's log and metrics collection", key)
				}
			}
		}
	}
	return nil
}

func protectedEnvironment(name string) bool {
	for _, prefix := range []string{"CETIRUN_", "OPENCLAW_", "HERMES_", "AZURE_", "KUBERNETES_", "LD_", "DYLD_", "NODE_", "PYTHON", "GIT_", "NPM_", "TAILSCALE_", "TS_", "TTYD_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return slices.Contains([]string{"HOME", "PATH", "SHELL", "ENV", "BASH_ENV", "KUBECONFIG", "API_SERVER_ENABLED", "API_SERVER_HOST",
		"API_SERVER_PORT", "API_SERVER_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "BRAVE_API_KEY", "PERPLEXITY_API_KEY"}, name)
}

func (b *Bundle) SecurityContext() *corev1.SecurityContext {
	context := &corev1.SecurityContext{
		RunAsUser: ptr.To(int64(1024)), RunAsGroup: ptr.To(int64(1024)), RunAsNonRoot: ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false), Privileged: ptr.To(false),
		ReadOnlyRootFilesystem: ptr.To(*b.input.Resolution.AgentPolicy.Security.ReadOnlyRootFilesystem),
		Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:         &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if b.input.CustomAgent != nil {
		context.RunAsUser, context.RunAsGroup = b.input.CustomAgent.RunAsUser, b.input.CustomAgent.RunAsGroup
	}
	return context
}

func (b *Bundle) PodSecurityContext() *corev1.PodSecurityContext {
	context := &corev1.PodSecurityContext{
		RunAsUser: ptr.To(int64(1024)), RunAsGroup: ptr.To(int64(1024)), RunAsNonRoot: ptr.To(true),
		FSGroup: ptr.To(int64(1024)), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if b.input.CustomAgent != nil {
		context.RunAsUser, context.RunAsGroup = b.input.CustomAgent.RunAsUser, b.input.CustomAgent.RunAsGroup
	}
	return context
}

func (b *Bundle) DNSConfig() *corev1.PodDNSConfig {
	return &corev1.PodDNSConfig{Nameservers: slices.Clone(b.input.Controls.DNSIPs)}
}

// Apply replaces the complete protected values bundle after callers have
// validated and merged user input. It does not mutate the caller's maps.
func (b *Bundle) Apply(values map[string]any) (map[string]any, error) {
	out, err := clone(values)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	secrets, _ := out["secrets"].(map[string]any)
	if !slices.Contains(b.input.AuthorizedSecrets, textValue(secrets["existingSecret"])) {
		return nil, fmt.Errorf("native runtime Secret is not authorized")
	}
	gateway, _ := out["aiGateway"].(map[string]any)
	if key := b.input.GatewayKey; key != nil {
		if gateway["enabled"] != true || textValue(gateway["keySecretName"]) != key.Name ||
			textValue(gateway["keySecretKey"]) != "api-key" || key.Name == textValue(secrets["existingSecret"]) {
			return nil, fmt.Errorf("native Gateway values do not match the separately authorized key")
		}
	} else if gateway["enabled"] == true {
		return nil, fmt.Errorf("native Gateway requires a typed authorized key projection")
	}
	if out["skills"] != nil {
		skills, ok := out["skills"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("protected skills require a skills object without package requests")
		}
		for _, field := range []string{"npm", "clawHub"} {
			if skills[field] == nil {
				continue
			}
			requested, ok := skills[field].([]any)
			if !ok || len(requested) != 0 {
				return nil, fmt.Errorf("SkillsMaterializationPending: protected workloads require published skills, not runtime skills.%s requests", field)
			}
		}
		if claim, present := skills["existingClaim"]; b.input.SkillsMode == SkillsModeEmpty && present && claim != "" {
			return nil, fmt.Errorf("empty skills cannot also select skills.existingClaim")
		}
	}
	security, _ := jsonValue(b.SecurityContext())
	podSecurity, _ := jsonValue(b.PodSecurityContext())
	if b.input.CustomAgent != nil && b.input.CustomAgent.RunAsUser == nil {
		for _, context := range []any{security, podSecurity} {
			values := context.(map[string]any)
			values["runAsUser"], values["runAsGroup"] = nil, nil
		}
	}
	dnsConfig, _ := jsonValue(b.DNSConfig())
	agent, _ := jsonValue(b.agentResources())
	helper, _ := jsonValue(b.helperResources())
	podResources, _ := jsonValue(b.accounting.Pod)
	controls, _ := jsonValue(b.input.Controls.Objects)
	volumes, _ := jsonValue(b.input.Controls.Volumes)
	mounts, _ := jsonValue(b.input.Controls.VolumeMounts)
	env, _ := jsonValue(b.input.Controls.Environment)
	owners, _ := jsonValue([]metav1.OwnerReference{AgentOwner(b.input.AgentName, b.input.Resolution.Binding.AgentUID)})
	labels, _ := jsonValue(b.input.Controls.PodLabels)
	overlay := map[string]any{
		"agentType": b.input.AgentType, "fullnameOverride": b.input.WorkloadName,
		"replicaCount": 0, "runtimeClassName": b.input.RuntimeClass.Name,
		"podSecurityContext": podSecurity, "securityContext": security, "resources": agent,
		"podAnnotations": map[string]any{
			"sidecar.istio.io/inject": "false", RevisionAnnotation: b.input.Controls.Revision,
			AgentUIDAnnotation: string(b.input.Resolution.Binding.AgentUID), KindAnnotation: AgentWorkload,
		},
		"sidecarScope":  map[string]any{"enabled": false, "additionalEgressHosts": []any{}},
		"networkPolicy": map[string]any{"enabled": true},
		"serviceAccount": map[string]any{
			"create": false, "name": b.input.Controls.ServiceAccountName, "role": "", "automountToken": false, "annotations": map[string]any{},
		},
		"azureWorkloadIdentity": map[string]any{"enabled": false, "clientId": ""},
		"agentIdentity":         map[string]any{"enabled": false, "brokerURL": ""},
		"persistence": map[string]any{
			"enabled": true, "existingClaim": b.input.Home.Name, "useStatefulSetVolumeClaim": false, "retainPolicy": "Retain",
			"storageClass": *b.input.Home.Spec.StorageClassName, "accessMode": string(b.input.Home.Spec.AccessModes[0]),
			"size": b.input.Resolution.Sandbox.Storage.Home.Size.String(), "annotations": map[string]any{},
		},
		"skills": map[string]any{
			"shared": b.input.SkillsMode != SkillsModeEmpty, "existingClaim": skillsIdentity(b.input.Skills).Name, "mountPath": SkillsPath,
			"npm": []any{}, "clawHub": []any{},
		},
		"doctor":                    map[string]any{"enabled": b.input.Helpers.Doctor},
		"initContainers":            []any{},
		"sidecars":                  []any{},
		"extraVolumes":              []any{},
		"extraVolumeMounts":         []any{},
		"extraEnvFrom":              []any{},
		"imagePullSecrets":          []any{},
		"nodeSelector":              map[string]any{},
		"affinity":                  map[string]any{},
		"tolerations":               []any{},
		"topologySpreadConstraints": []any{},
		"autoscaling":               map[string]any{"enabled": false},
		"ingress":                   map[string]any{"enabled": false},
		// Protected namespaces use the operator's CONNECT guard instead of
		// the legacy per-release exec policy and its caller exemption.
		"execRestriction":     map[string]any{"enabled": false},
		"podDisruptionBudget": map[string]any{"enabled": false},
		"tailscale":           map[string]any{"enabled": false, "serve": false},
		ValuesKey: map[string]any{
			"contractVersion": ContractVersion, "executionHeld": true,
			"podLabels":    labels,
			"dnsConfig":    dnsConfig,
			"podResources": podResources, "helperResources": helper, "securityContext": security,
			"scratchSizeLimit": b.input.Resolution.Sandbox.Storage.Scratch.SizeLimit.String(),
			"ownerReferences":  owners, "identityVolumes": nonNilList(volumes),
			"identityMounts": nonNilList(mounts), "identityEnvironment": nonNilList(env),
			"objects": nonNilList(controls),
		},
	}
	if b.input.GatewayKey != nil {
		overlay[ValuesKey].(map[string]any)["podFinalizers"] = []any{NativePodFinalizer}
	}
	if marketplace := b.input.SkillMarketplace; marketplace != nil {
		// The HOME initializer mounts only the Team's subtree of the claim and
		// waits for the release of the Team's current selection.
		skills := overlay["skills"].(map[string]any)
		skills["subPath"] = marketplace.TeamPath
		skills["marketplace"] = map[string]any{"selectionDigest": marketplace.SelectionDigest}
	}
	if custom := b.input.CustomAgent; custom != nil {
		terminalEnabled := b.input.Terminal != nil
		if settings, ok := out["ttyd"].(map[string]any); ok {
			if enabled, ok := settings["enabled"].(bool); ok {
				terminalEnabled = terminalEnabled && enabled
			}
		}
		overlay["ttyd"] = map[string]any{"enabled": terminalEnabled}
		contextFiles := map[string]any{}
		for destination, source := range b.input.Context.Files {
			contextFiles[destination] = source
		}
		control := overlay[ValuesKey].(map[string]any)
		control["context"] = map[string]any{"name": b.input.Context.Name, "files": contextFiles}
		if probe := b.input.LoopbackProbe; probe != nil {
			control["loopbackProbe"] = map[string]any{"image": probe.Image}
		}
		if terminal := b.input.Terminal; terminal != nil {
			control["terminal"] = map[string]any{
				"enabled": terminalEnabled && !b.input.Suspended, "image": terminal.Image, "port": terminal.Port,
				"sessionSeconds": terminal.SessionSeconds, "serviceAccountName": terminal.ServiceAccountName,
			}
		}
		port := any(nil)
		if custom.Port != nil {
			port = *custom.Port
		}
		overlay["runtime"] = map[string]any{
			"contractVersion": ContractVersion,
			"agent": map[string]any{
				"type": "custom", "image": custom.Image, "username": custom.Username,
				"home": custom.Home, "port": port, "webUI": custom.WebUI,
				"webBasePath": custom.WebBasePath, "webInstance": custom.WebInstance,
			},
			"model": map[string]any{
				"apiBase": b.input.GatewayAPIBase, "keySecretName": b.input.GatewayKey.Name, "keySecretKey": "api-key",
			},
			"environment": customEnvironmentValues(custom.Environment),
			"ports":       customPortValues(custom.Ports),
			"http":        customHTTPValues(custom.HTTP),
		}
		if secret := b.input.ImagePullSecret; secret != nil {
			overlay["runtime"].(map[string]any)["imagePullSecrets"] = []any{map[string]any{"name": secret.Name}}
		}
	}
	if b.input.Reconciled {
		if err := b.input.ConnectionsMCP.Validate(); err != nil {
			return nil, err
		}
		if endpoint := b.input.ConnectionsMCP; endpoint != nil {
			overlay["connectionsMCP"] = map[string]any{
				"enabled": true, "url": endpoint.URL, "audience": endpoint.Audience,
				"caSecretName": endpoint.CASecretName, "namespace": endpoint.Namespace,
			}
		}
		overlay["replicaCount"] = 1
		if value, present := out["replicaCount"]; present {
			overlay["replicaCount"] = value
		}
		control := overlay[ValuesKey].(map[string]any)
		control["reconciled"], control["executionHeld"] = true, false
		delete(control, "podFinalizers")
		annotations := map[string]any{}
		if existing, ok := out["podAnnotations"].(map[string]any); ok {
			for key, value := range existing {
				annotations[key] = value
			}
		}
		for key, value := range overlay["podAnnotations"].(map[string]any) {
			annotations[key] = value
		}
		overlay["podAnnotations"] = annotations
		annotations[ReconciledAnnotation] = "true"
		for _, name := range []string{"initContainers", "sidecars", "extraVolumes", "extraVolumeMounts", "extraEnvFrom", "imagePullSecrets"} {
			delete(overlay, name)
		}
		delete(overlay, "doctor")
	}
	if len(b.input.Controls.TrustedProxyIPs) != 0 {
		proxies, err := jsonValue(b.input.Controls.TrustedProxyIPs)
		if err != nil {
			return nil, fmt.Errorf("encode protected native proxy IPs: %w", err)
		}
		overlay[ValuesKey].(map[string]any)["trustedProxyIPs"] = proxies
	}
	if b.input.SkillsMode == SkillsModeEmpty {
		overlay[ValuesKey].(map[string]any)["skillsMode"] = string(SkillsModeEmpty)
	}
	if scheduling := b.input.RuntimeClass.Scheduling; scheduling != nil {
		overlay["nodeSelector"], _ = jsonValue(scheduling.NodeSelector)
		overlay["tolerations"], _ = jsonValue(scheduling.Tolerations)
		if overlay["nodeSelector"] == nil {
			overlay["nodeSelector"] = map[string]any{}
		}
		overlay["tolerations"] = nonNilList(overlay["tolerations"])
	}
	for key, value := range overlay {
		out[key] = value
	}
	for name, enabled := range map[string]bool{"logger": b.input.Helpers.Logger, "desktop": b.input.Helpers.Desktop} {
		settings, _ := out[name].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
		}
		if !b.input.Reconciled {
			settings["enabled"] = enabled
		}
		settings["resources"] = helper
		out[name] = settings
	}
	secrets["tailscaleAuthKey"] = ""
	if b.input.AgentType == "openclaw" {
		native, _ := out["openclaw"].(map[string]any)
		if native == nil {
			native = map[string]any{}
		}
		if !b.input.Reconciled {
			native["doctor"] = map[string]any{"enabled": b.input.Helpers.Doctor}
		}
		if !b.input.Reconciled && !b.input.Helpers.Desktop {
			config, _ := native["config"].(map[string]any)
			if config == nil {
				config = map[string]any{}
			}
			config["browser"] = map[string]any{"enabled": false, "attachOnly": false}
			native["config"] = config
		}
		out["openclaw"] = native
	}
	if b.input.Controls.IngressMTLS != nil {
		if err := b.applyIngressMTLS(out); err != nil {
			return nil, err
		}
	}
	if b.input.Controls.EgressSNI != nil {
		if err := b.applyEgressSNI(out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func jsonValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	var out any
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

func nonNilList(value any) any {
	if value == nil {
		return []any{}
	}
	return value
}

func textValue(value any) string {
	valueString, _ := value.(string)
	return valueString
}
