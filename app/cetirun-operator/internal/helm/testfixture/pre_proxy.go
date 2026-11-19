// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package testfixture

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"helm.sh/helm/v3/pkg/chart"
)

// laterAdditions are chart changes made after the proxy hook, removed before
// the original bytes are checked.
var laterAdditions = map[string][]string{
	"templates/_sandbox.tpl": {`{{- if $s.ingressMTLS -}}
{{- if or (not $reconciled) (include "cetirun.tailscaleEnabled" .) (gt (int .Values.msteamsWebhookPort) 0) -}}
{{- fail "managed ingress mTLS requires common built-in API/UI/terminal/desktop routing without alternate ingress" -}}
{{- end -}}
{{- end -}}
`, `{{- if $s.egressSNI -}}
{{- if or (not $reconciled) (ne (len ($s.managedInitContainers | default list)) 2) .Values.initContainers .Values.sidecars (not .Values.readinessProbe.enabled) -}}
{{- fail "observed-SNI capture requires the managed Envoy/init prefix and proxy-aware common-executor readiness" -}}
{{- end -}}
{{- else if $s.managedInitContainers -}}
{{- fail "managed capture initializers require observed-SNI controls" -}}
{{- end -}}
`},
	"agents/openclaw/config/native.tpl": {`{{- /* Portal sign-in: the ingress lets only the Agent's owner through, after
   their Portal sign-in, and passes their email from that sign-in. The only
   trusted proxy is the Istio sidecar or the managed in-Pod mTLS endpoint.
   OpenClaw takes the owner from that header and approves their browsers. The
   gateway token becomes a password for clients on loopback, such as the CLI
   in the Terminal, as OpenClaw refuses a token in this mode. */}}
{{- if and (dig "portalSignIn" "enabled" false .Values.openclaw) (or (not .Values.sandboxWorkload) (dig "ingressMTLS" false (.Values.sandboxWorkload | default dict))) (not .TailscaleEnabled) }}
{{- $gateway := dig "gateway" (dict) $config }}
{{- /* OpenClaw falls back to the remote token even when gateway.mode is local. */}}
{{- $_ := unset (dig "remote" (dict) $gateway | default dict) "token" }}
{{- $trustedProxies := dig "trustedProxies" (list) $gateway }}
{{- if .Values.sandboxWorkload }}
{{- if not (and (eq (len $trustedProxies) 1) (has "127.0.0.1" $trustedProxies)) }}
{{- $_ := required "Sandbox openclaw.portalSignIn needs exactly 127.0.0.1, the managed ingress mTLS proxy, in sandboxWorkload.trustedProxyIPs" "" }}
{{- end }}
{{- if ne (dig "bind" "" $gateway) "loopback" }}
{{- $_ := required "Sandbox openclaw.portalSignIn needs gateway.bind=loopback behind managed ingress mTLS" "" }}
{{- end }}
{{- else if not (has "127.0.0.6" $trustedProxies) }}
{{- $_ := required "openclaw.portalSignIn needs 127.0.0.6, the Istio sidecar, in openclaw.config.gateway.trustedProxies" "" }}
{{- end }}
{{- $_ := set $gateway "auth" (dict
    "mode" "trusted-proxy"
    "trustedProxy" (dict
      "userHeader" "x-auth-request-email"
      "allowLoopback" true
      "deviceAutoApprove" (dict "enabled" true))) }}
{{- $_ := set $config "gateway" $gateway }}
{{- end }}
`},
	"values.schema.json": {`                "ingressMTLS": {"const": true, "description": "Internal common-executor managed Envoy endpoint; absent for ordinary ingress"},
`, `                "ingressServerName": {"type": "string", "minLength": 1},
`, `                "egressSNI": {"const": true, "description": "Internal compiler-owned observed-SNI capture; never a user policy override"},
                "managedInitContainers": {"type": "array", "minItems": 2, "maxItems": 2, "description": "Exact managed Envoy and trusted capture initializer prefix"},
`, `                "context": {
                    "type": "object", "additionalProperties": false,
                    "properties": {"name": {"type": "string", "minLength": 1}, "files": {"type": "object", "additionalProperties": false, "properties": {"AGENTS.md": {"const": "AGENTS.md"}}, "required": ["AGENTS.md"]}},
                    "required": ["name", "files"]
                },
                "loopbackProbe": {
                    "type": "object", "additionalProperties": false,
                    "properties": {"image": {"type": "string", "pattern": "^[^\\s@]+@sha256:[a-f0-9]{64}$"}}, "required": ["image"]
                },
                "terminal": {
                    "type": "object", "additionalProperties": false,
                    "properties": {"enabled": {"type": "boolean"}, "image": {"type": "string", "pattern": "^[^\\s@]+@sha256:[a-f0-9]{64}$"}, "port": {"type": "integer", "minimum": 1024, "maximum": 65535}, "sessionSeconds": {"type": "integer", "minimum": 60, "maximum": 3600}, "serviceAccountName": {"type": "string", "minLength": 1}, "tlsContainer": {"type": "object"}, "tlsConfigDigest": {"type": "string", "pattern": "^[a-f0-9]{64}$"}, "tlsVolumes": {"type": "array", "items": {"type": "object"}}},
                    "required": ["enabled", "image", "port", "sessionSeconds", "serviceAccountName"]
                },
`, `                "portalSignIn": {
                    "type": "object",
                    "description": "Sign the owner in to the Control UI with their Portal sign-in (OpenClaw trusted-proxy mode)",
                    "additionalProperties": false,
                    "properties": {
                        "enabled": {
                            "type": "boolean",
                            "default": false
                        }
                    }
                },
`, `                        },
                        "HERMES_DASHBOARD_PUBLIC_URL": {
                            "type": "string",
                            "description": "Operator-owned external Dashboard URL including its instance prefix",
                            "default": ""
`, `                "subPath": {
                    "type": "string",
                    "description": "Operator-set Skill Marketplace Team subtree (teams/<team-uid>) of the shared skills claim; only the HOME initializer mounts it, read-only",
                    "pattern": "^$|^teams/[A-Za-z0-9][A-Za-z0-9._-]{0,127}$",
                    "default": ""
                },
                "marketplace": {
                    "type": "object",
                    "description": "Operator-set Skill Marketplace selection of the Agent's Team; set together with subPath",
                    "additionalProperties": false,
                    "properties": {
                        "selectionDigest": {
                            "type": "string",
                            "description": "Team selection digest that the HOME initializer waits for",
                            "pattern": "^$|^sha256:[a-f0-9]{64}$",
                            "default": ""
                        }
                    }
                },
`},
}

// RestorePreProxyContract reconstructs the three chart assets changed by the
// proxy hook and verifies their exact bytes at public revision 6414494154151b2daf74d39d1e989ebeb7ea5020.
func RestorePreProxyContract(chrt *chart.Chart) error {
	// Restore only the later publication defaults before checking the
	// immutable historical schema checksum below.
	for _, image := range []string{"openclaw-gateway", "hermes-gateway", "cetirun-desktop"} {
		chrt.Schema = bytes.ReplaceAll(chrt.Schema,
			[]byte(`"default": "mcr.microsoft.com/aks/ai-runtime/cetirun/`+image+`"`),
			[]byte(`"default": "cetirun.azurecr.io/`+image+`"`))
	}
	var err error
	chrt.Schema, err = restoreHermesWebappSchema(chrt.Schema)
	if err != nil {
		return err
	}
	chrt.Schema, err = stripCetiAdminSchema(chrt.Schema)
	if err != nil {
		return err
	}
	if startup, ok := chrt.Values["startupProbe"].(map[string]any); ok {
		startup["failureThreshold"] = 30
	}
	if gateway, ok := chrt.Values["aiGateway"].(map[string]any); ok {
		if proxy, ok := gateway["copilotProxy"].(map[string]any); ok {
			delete(proxy, "replicaCount")
		}
		delete(gateway, "visionModels")
	}
	assets := map[string]*[]byte{"values.schema.json": &chrt.Schema}
	for _, files := range [][]*chart.File{chrt.Files, chrt.Templates} {
		for _, file := range files {
			assets[file.Name] = &file.Data
		}
	}
	if err := restoreRemovedTeamsAssets(assets); err != nil {
		return err
	}

	// Reverse the Gateway-only source changes before checking the immutable
	// historical assets. These bytes are test inputs, never release defaults.
	for name, replacements := range map[string][][2]string{
		"agents/openclaw/config/native.tpl": {
			{`{{- $directSkills := or .Values.skills.shared $emptySkills }}`, `{{- $directSkills := and (not $contextSkills) (or .Values.skills.shared $emptySkills) }}`},
			{"{{- $_ := set $load \"extraDirs\" (append (dig \"extraDirs\" (list) $load) \"/opt/cetirun-memory/skills\" | uniq) }}\n", ""},
			{`{{- $_ := set $load "extraDirs" (append (dig "extraDirs" (list) $load) .Values.skills.mountPath | uniq) }}`, `{{- $_ := set $load "extraDirs" (list .Values.skills.mountPath) }}`},
			{`    "trustedProxy" (dict
      "userHeader" "x-auth-request-email"
      "allowLoopback" true
      "deviceAutoApprove" (dict "enabled" true))) }}
{{- $_ := set $config "gateway" $gateway }}
{{- end }}
{{- /* The AI Gateway is the only platform model provider. */}}
{{- $model := required "secrets.model must be set by the CetiRun operator" .Values.secrets.model }}
{{- $gatewayRouted := true }}
{{- $nativeGateway := true }}
{{- $_ := set $config "auth" (dict "profiles" (dict)) }}
{{- $providerPrefix := "openai" }}
{{- $modelFull := printf "openai/%s" $model }}
{{- /* Models the platform AI Gateway offers this Agent. Each becomes a model
   of the Gateway provider, with its limits, and is allowed; the rest of the
   configuration keeps naming the default model. */}}
{{- $gatewayModels := list }}
{{- if $gatewayRouted }}
{{- $gatewayModels = dig "models" (list) .Values.aiGateway }}
`, `    "trustedProxy" (dict
      "userHeader" "x-auth-request-email"
      "allowLoopback" true
      "deviceAutoApprove" (dict "enabled" true))) }}
{{- $_ := set $config "gateway" $gateway }}
{{- end }}
{{- /* Model provider auth profile and default model.
   Supports: github_copilot (token-based), openai (api-key), anthropic (api-key). */}}
{{- $provider := .Values.secrets.provider | default "github_copilot" }}
{{- $model := required "secrets.model must be set by the CetiRun operator or Helm values" .Values.secrets.model }}
{{- $gatewayRouted := and (dig "enabled" false (.Values.aiGateway | default dict)) (eq $provider "openai") }}
{{- $nativeGateway := and $gatewayRouted .Values.sandboxWorkload }}
{{- $authMode := .Values.secrets.authMode }}
{{- $authProfileMode := $authMode }}
{{- if eq $authProfileMode "api-key" }}
{{- $authProfileMode = "api_key" }}
{{- end }}
{{- $auth := dig "auth" (dict) $config }}
{{- if $nativeGateway }}
{{- $auth = dict "profiles" (dict) }}
{{- else if eq $provider "github_copilot" }}
{{- $_ := set $auth "profiles" (dict "github-copilot:github" (dict "provider" "github-copilot" "mode" "token")) }}
{{- else if eq $provider "openai" }}
{{- $profile := dict "provider" "openai" }}
{{- if $authProfileMode }}
{{- $_ := set $profile "mode" $authProfileMode }}
{{- end }}
{{- $_ := set $auth "profiles" (dict "openai:default" $profile) }}
{{- else if eq $provider "anthropic" }}
{{- $profile := dict "provider" "anthropic" }}
{{- if $authProfileMode }}
{{- $_ := set $profile "mode" $authProfileMode }}
{{- end }}
{{- $_ := set $auth "profiles" (dict "anthropic:default" $profile) }}
{{- end }}
{{- $_ := set $config "auth" $auth }}
{{- /* Compose full model identifier as <provider>/<model> */}}
{{- $providerPrefix := $provider | replace "_" "-" }}
{{- $modelFull := printf "%s/%s" $providerPrefix $model }}
{{- /* Models the platform AI Gateway offers this Agent. Each becomes a model
   of the Gateway provider, with its limits, and is allowed; the rest of the
   configuration keeps naming the default model. */}}
{{- $gatewayModels := list }}
{{- if $gatewayRouted }}
{{- $gatewayModels = dig "models" (list) .Values.aiGateway }}
`},
			{`{{- /* Default model and compaction settings */}}
{{- $compaction := dict "memoryFlush" (dict "enabled" true "softThresholdTokens" (int 4000)) }}
{{- $modelConfig := dict }}
{{- $agentsCurrent := dig "agents" (dict) $config }}
{{- $defaultsCurrent := dig "defaults" (dict) $agentsCurrent }}
{{- $_ := set $defaultsCurrent "compaction" $compaction }}
{{- if $gatewayModels }}
{{- $modelsDict := dict }}
{{- $allow := list }}
{{- range $gatewayModels }}
{{- $ref := printf "%s/%s" $providerPrefix .id }}
{{- $_ := set $modelsDict $ref (dict) }}
{{- $allow = append $allow $ref }}
`, `{{- /* Default model and compaction settings */}}
{{- $compaction := dict "memoryFlush" (dict "enabled" true "softThresholdTokens" (int 4000)) }}
{{- $modelConfig := dict }}
{{- $agentsCurrent := dig "agents" (dict) $config }}
{{- $defaultsCurrent := dig "defaults" (dict) $agentsCurrent }}
{{- $_ := set $defaultsCurrent "compaction" $compaction }}
{{- if eq $provider "github_copilot" }}
{{- $fallbacks := list }}
{{- $_ := set $defaultsCurrent "model" (dict "primary" $modelFull "fallbacks" $fallbacks) }}
{{- $modelsDict := dict $modelFull $modelConfig }}
{{- range $fallbacks }}
{{- $_ := set $modelsDict . (dict) }}
{{- end }}
{{- $_ := set $defaultsCurrent "models" $modelsDict }}
{{- $_ := set $defaultsCurrent "modelPolicy" (dict "allow" (concat (list $modelFull) $fallbacks)) }}
{{- else if $gatewayModels }}
{{- $modelsDict := dict }}
{{- $allow := list }}
{{- range $gatewayModels }}
{{- $ref := printf "%s/%s" $providerPrefix .id }}
{{- $_ := set $modelsDict $ref (dict) }}
{{- $allow = append $allow $ref }}
`},
			{`   model serves them instead. */}}
{{- $_ := set $defaultsCurrent "utilityModel" $modelFull }}
{{- end }}
{{- $_ := set $agentsCurrent "defaults" $defaultsCurrent }}
{{- $_ := set $config "agents" $agentsCurrent }}
{{- /* Custom API base URL → models.providers.<provider>.baseUrl */}}
{{- if .Values.secrets.apiBase }}
{{- $providerModels := list }}
{{- $listed := false }}
{{- range $gatewayModels }}
{{- $entry := dict "id" .id "name" (.name | default .id) }}
{{- if .contextWindow }}
{{- $_ := set $entry "contextWindow" (int .contextWindow) }}
`, `   model serves them instead. */}}
{{- $_ := set $defaultsCurrent "utilityModel" $modelFull }}
{{- end }}
{{- $_ := set $agentsCurrent "defaults" $defaultsCurrent }}
{{- $_ := set $config "agents" $agentsCurrent }}
{{- /* Custom API base URL → models.providers.<provider>.baseUrl */}}
{{- if and (ne $provider "github_copilot") .Values.secrets.apiBase }}
{{- $providerModels := list }}
{{- $listed := false }}
{{- range $gatewayModels }}
{{- $entry := dict "id" .id "name" (.name | default .id) }}
{{- if .contextWindow }}
{{- $_ := set $entry "contextWindow" (int .contextWindow) }}
`},
		},
		"values.schema.json": {
			{`            "type": "object",
            "description": "Operator-owned platform AI Gateway access for the Agent's model traffic",
            "additionalProperties": false,
            "properties": {
                "enabled": {
                    "type": "boolean",
                    "const": true,
                    "default": true
                },
                "keySecretName": {
                    "type": "string",
                    "description": "Secret in the release namespace holding the Agent's virtual key",
                    "default": ""
                },
`, `            "type": "object",
            "description": "Operator-owned platform AI Gateway access for the Agent's model traffic",
            "additionalProperties": false,
            "properties": {
                "enabled": {
                    "type": "boolean",
                    "default": false
                },
                "keySecretName": {
                    "type": "string",
                    "description": "Secret in the release namespace holding the Agent's virtual key",
                    "default": ""
                },
`},
			{`                "openclawGatewayToken": {
                    "type": "string",
                    "description": "Gateway authentication token"
                },
                "provider": {
                    "type": "string",
                    "description": "AI Gateway protocol; upstream providers are managed by the platform",
                    "enum": ["openai"],
                    "default": "openai"
                },
                "model": {
                    "type": "string",
                    "description": "Model name supplied by the operator from spec.model or its deployment default",
                    "default": ""
                },
                "apiKey": {
                    "type": "string",
                    "description": "Removed direct credentials; use the AI Gateway",
                    "const": "",
                    "default": ""
                },
                "apiBase": {
                    "type": "string",
                    "description": "Custom API base URL (uses provider default if empty)",
                    "default": ""
`, `                "openclawGatewayToken": {
                    "type": "string",
                    "description": "Gateway authentication token"
                },
                "provider": {
                    "type": "string",
                    "description": "Model provider: github_copilot, openai, or anthropic",
                    "enum": [
                        "github_copilot",
                        "openai",
                        "anthropic"
                    ],
                    "default": "github_copilot"
                },
                "model": {
                    "type": "string",
                    "description": "Model name supplied by the operator from spec.model or its deployment default",
                    "default": ""
                },
                "apiKey": {
                    "type": "string",
                    "description": "API key for openai/anthropic providers (not needed for github_copilot)",
                    "default": ""
                },
                "apiBase": {
                    "type": "string",
                    "description": "Custom API base URL (uses provider default if empty)",
                    "default": ""
`},
			{`                    "default": false
                }
            }
        },
        "litellm": {
            "type": "object",
            "description": "The removed per-Agent model proxy is unsupported; use the platform AI Gateway.",
            "not": {},
            "properties": {
                "enabled": {
                    "type": "boolean",
                    "description": "Enable LiteLLM sidecar container",
                    "default": true
                },
`, `                    "default": false
                }
            }
        },
        "litellm": {
            "type": "object",
            "description": "LiteLLM sidecar configuration",
            "properties": {
                "enabled": {
                    "type": "boolean",
                    "description": "Enable LiteLLM sidecar container",
                    "default": true
                },
`},
		},
	} {
		data := assets[name]
		for _, replacement := range replacements {
			if data == nil || bytes.Count(*data, []byte(replacement[0])) != 1 {
				return fmt.Errorf("pre-proxy fixture requires the exact Gateway change in %s", name)
			}
			*data = bytes.Replace(*data, []byte(replacement[0]), []byte(replacement[1]), 1)
		}
	}
	// The current chart also contains native MCP support. Remove only those
	// additions before reconstructing and checking the fixed pre-proxy revision.
	for name, addition := range map[string]string{
		"agents/openclaw/config/native.tpl": `{{- if .Values.connectionsMCP.enabled }}
{{- $mcp := dig "mcp" (dict) $config }}
{{- $servers := dig "servers" (dict) $mcp }}
{{- $_ := set $servers "cetirun-connections" (dict "url" .Values.connectionsMCP.url "transport" "streamable-http" "sslVerify" true) }}
{{- $_ := set $mcp "servers" $servers }}
{{- $_ := set $config "mcp" $mcp }}
{{- else }}
{{- $servers := dig "mcp" "servers" (dict) $config }}
{{- $_ := unset $servers "cetirun-connections" }}
{{- end }}
`,
		"templates/_sandbox.tpl": "(and .Values.connectionsMCP.enabled (not $reconciled)) ",
	} {
		data, exists := assets[name]
		if !exists || bytes.Count(*data, []byte(addition)) != 1 {
			return fmt.Errorf("pre-proxy fixture requires the exact MCP addition in %s", name)
		}
		*data = bytes.Replace(*data, []byte(addition), nil, 1)
	}
	// Reverse later source changes before verifying the immutable old fixture.
	for name, replacements := range map[string][][2]string{
		"agents/openclaw/config/native.tpl": {
			{`{{- $contextSkills := and (or (not .Values.sandboxWorkload) (dig "reconciled" false (.Values.sandboxWorkload | default dict))) (dig "enabled" false (.Values.memoryContext | default dict)) }}`,
				`{{- $contextSkills := and (not .Values.sandboxWorkload) (dig "enabled" false (.Values.memoryContext | default dict)) }}`},
			{"{{- if $contextSkills }}\n{{- $plugins :=", "{{- if and (not .Values.sandboxWorkload) (dig \"enabled\" false (.Values.memoryContext | default dict)) }}\n{{- $plugins :="},
			{`{{- /* A model the Gateway reports as accepting images gets them with a
   request. Without input, OpenClaw takes the value from its own catalog,
   which knows only some model names; for the rest, another model describes
   images. */}}
{{- if has .id (dig "visionModels" (list) $.Values.aiGateway) }}
{{- $_ := set $entry "input" (list "text" "image") }}
{{- end }}
`, ""},
		},
		"values.schema.json": {
			{`"pattern": "^([0-9./]+|[0-9a-fA-F:]+/[0-9]{1,3})$"`, `"pattern": "^[0-9./]+$"`},
			{`"type": ["integer", "null"],
                    "minimum": 1,
                    "default": null,
                    "description": "Null selects 60 for common-executor OpenClaw Sandboxes, otherwise 30; explicit positive integers are preserved"`,
				`"type": "integer",
                    "default": 30`},
			{"                        \"replicaCount\": {\"type\": [\"integer\", \"null\"], \"minimum\": 0, \"maximum\": 1, \"default\": null},\n", ""},
			{`                "visionModels": {
                    "type": "array",
                    "description": "Ids of the models in models that accept images",
                    "default": [],
                    "maxItems": 256,
                    "items": {"type": "string", "minLength": 1, "maxLength": 256}
                },
`, ""},
			{`"executionHeld": {"type": "boolean"},
                "reconciled": {"const": true}`, `"executionHeld": {"const": true}`},
			{`"agentType": {"enum": ["openclaw", "hermes", "custom"]}`, `"agentType": {"const": "openclaw"}`},
			{`"sandboxWorkload": {
                        "anyOf": [
                            {"required": ["podFinalizers"]},
                            {"required": ["reconciled"], "properties": {"reconciled": {"const": true}}}
                        ]
                    }`, `"sandboxWorkload": {"required": ["podFinalizers"]}`},
		},
		"templates/_sandbox.tpl": {
			{`{{- if and (eq (include "cetirun.agent.type" .) "custom") (not $reconciled) -}}
{{- fail "Custom requires common reconciled Sandbox execution" -}}`,
				`{{- if eq (include "cetirun.agent.type" .) "custom" -}}
{{- fail "governed Custom rendering is unsupported" -}}`},
			{`{{- $reconciled := $s.reconciled | default false -}}
{{- if or (ne $s.contractVersion "v1") (and (not $reconciled) (or (ne $s.executionHeld true) (ne (int .Values.replicaCount) 0))) (and $reconciled (or $s.executionHeld (gt (int .Values.replicaCount) 1) (lt (int .Values.replicaCount) 0))) -}}`,
				`{{- if or (ne $s.contractVersion "v1") (ne $s.executionHeld true) (ne (int .Values.replicaCount) 0) -}}`},
			{`(and (not $reconciled) (ne (include "cetirun.agent.type" .) "openclaw"))`, `(ne (include "cetirun.agent.type" .) "openclaw")`},
			{`(and (not $reconciled) (ne (toJson ($s.podFinalizers | default list)) (toJson (list "cetirun.microsoft.com/native-pod-lifetime"))))`,
				`(ne (toJson ($s.podFinalizers | default list)) (toJson (list "cetirun.microsoft.com/native-pod-lifetime")))`},
			{`(and (not $reconciled) (or .Values.initContainers .Values.sidecars .Values.extraVolumes .Values.extraVolumeMounts .Values.extraEnvFrom))`,
				`.Values.initContainers .Values.sidecars .Values.extraVolumes .Values.extraVolumeMounts .Values.extraEnvFrom`},
		},
	} {
		data := assets[name]
		if data == nil {
			return fmt.Errorf("missing historical chart input %s", name)
		}
		for _, replacement := range replacements {
			*data = bytes.ReplaceAll(*data, []byte(replacement[0]), []byte(replacement[1]))
		}
	}
	for name, change := range map[string]struct {
		addition string
		checksum string
	}{
		"agents/openclaw/config/native.tpl": {
			// Remove later browser defaulting too; retain the original checksum.
			addition: `{{- /* Use the desktop's headed browser when available; otherwise let OpenClaw
   launch a headless browser. Explicit boolean preferences take precedence. */}}
{{- $browser := dig "browser" (dict) $config | default dict }}
{{- if eq (index $browser "attachOnly") nil }}
{{- $_ := set $browser "attachOnly" .Values.desktop.enabled }}
{{- end }}
{{- if eq (index $browser "headless") nil }}
{{- $_ := set $browser "headless" (not .Values.desktop.enabled) }}
{{- end }}
{{- $_ := set $config "browser" $browser }}
{{- if and .Values.sandboxWorkload (hasKey .Values.sandboxWorkload "trustedProxyIPs") }}
{{- $gateway := dig "gateway" (dict) $config }}
{{- $_ := set $gateway "trustedProxies" (.Values.sandboxWorkload.trustedProxyIPs | default list) }}
{{- $_ := set $config "gateway" $gateway }}
{{- end }}
`,
			checksum: "8bf84a1d564eba85382a71e29f9e2c3261e70c0b7ca4acf95e520377fdbf8abf",
		},
		"templates/_sandbox.tpl": {
			addition: `{{- if hasKey $s "trustedProxyIPs" -}}
{{- if not (kindIs "slice" $s.trustedProxyIPs) -}}
{{- fail "Sandbox trustedProxyIPs must be a compiler-selected list" -}}
{{- end -}}
{{- range $s.trustedProxyIPs -}}
{{- if or (not (kindIs "string" .)) (empty .) -}}
{{- fail "Sandbox trustedProxyIPs must contain nonempty compiler-selected IPs" -}}
{{- end -}}
{{- end -}}
{{- end -}}
`,
			checksum: "3eb73fb04375e6fdcb9280e966565028f40338d73136c5b319047f584ff6b090",
		},
		"values.schema.json": {
			addition: `                "trustedProxyIPs": {
                    "type": "array",
                    "uniqueItems": true,
                    "description": "Compiler-selected ingress IPs or registered Pod CIDRs; not tenant proxy trust",
                    "items": {
                        "type": "string",
                        "minLength": 1,
                        "anyOf": [
                            {"format": "ipv4"},
                            {"format": "ipv6"},
                            {"pattern": "^([0-9]{1,3}\\.){3}[0-9]{1,3}/([1-9]|[12][0-9]|3[0-2])$"},
                            {"pattern": "^[0-9a-fA-F]*:[0-9a-fA-F:]+/([1-9]|[1-9][0-9]|1[01][0-9]|12[0-8])$"}
                        ]
                    }
                },
`,
			checksum: "8bdf6a6367048be1d1d818061cf0a268181b0e82324c1834259f2b069320aaaa",
		},
	} {
		data, exists := assets[name]
		if !exists || bytes.Count(*data, []byte(change.addition)) != 1 {
			return fmt.Errorf("pre-proxy fixture requires the exact added hook in %s", name)
		}
		original := bytes.Replace(*data, []byte(change.addition), nil, 1)
		for _, later := range laterAdditions[name] {
			if bytes.Count(original, []byte(later)) != 1 {
				return fmt.Errorf("pre-proxy fixture requires the later addition in %s", name)
			}
			original = bytes.Replace(original, []byte(later), nil, 1)
		}
		if fmt.Sprintf("%x", sha256.Sum256(original)) != change.checksum {
			return fmt.Errorf("pre-proxy fixture %s differs from the original chart asset", name)
		}
		*data = original
	}
	return nil
}

func stripCetiAdminSchema(schema []byte) ([]byte, error) {
	startMarker, endMarker := []byte("        \"cetiadmin\": {\n"), []byte("        \"memoryContext\": {\n")
	start, end := bytes.Index(schema, startMarker), bytes.Index(schema, endMarker)
	if start < 0 || end <= start || bytes.Count(schema, startMarker) != 1 || bytes.Count(schema, endMarker) != 1 {
		return nil, fmt.Errorf("pre-proxy fixture requires the later CetiAdmin schema addition")
	}
	// Pin only the later addition being removed; the historical schema SHA below
	// remains unchanged and still checks every retained byte.
	if fmt.Sprintf("%x", sha256.Sum256(schema[start:end])) != "8fd902511e9b1effe4b0c46a2827eb1a91b2b1d42d46045c8bb96ab03101a45c" {
		return nil, fmt.Errorf("pre-proxy fixture CetiAdmin schema addition changed")
	}
	return append(append([]byte(nil), schema[:start]...), schema[end:]...), nil
}

func restoreRemovedTeamsAssets(assets map[string]*[]byte) error {
	replace := func(name, old, new string) error {
		data := assets[name]
		if data == nil || bytes.Count(*data, []byte(old)) != 1 {
			return fmt.Errorf("pre-proxy fixture requires the current Teams removal point in %s", name)
		}
		*data = bytes.Replace(*data, []byte(old), []byte(new), 1)
		return nil
	}
	if err := replace("templates/_sandbox.tpl", `{{- if or (not $reconciled) (include "cetirun.tailscaleEnabled" .) -}}`, `{{- if or (not $reconciled) (include "cetirun.tailscaleEnabled" .) (gt (int .Values.msteamsWebhookPort) 0) -}}`); err != nil {
		return err
	}
	const teamsPlugin = `{{- $msteamsAuthType := default "" .Values.msteamsAuthType }}
{{- if or
      (and
        (gt (int .Values.msteamsWebhookPort) 0)
        .Values.secrets.msteamsAppId
        .Values.secrets.msteamsTenantId
        (or
          (and (or (eq $msteamsAuthType "") (eq $msteamsAuthType "secret")) .Values.secrets.msteamsAppPassword)
          (or (eq $msteamsAuthType "managedIdentity") (eq $msteamsAuthType "certificate"))
        )
      )
      (and (gt (int .Values.msteamsWebhookPort) 0) .Values.secrets.existingSecret)
    }}
{{- $_ := set $pluginEntries "msteams" (dict "enabled" true) }}
{{- end }}
`
	if err := replace("agents/openclaw/config/native.tpl", `{{- /* Enable ACPX plugin when acpx.enabled is true */}}`, teamsPlugin+`{{- /* Enable ACPX plugin when acpx.enabled is true */}}`); err != nil {
		return err
	}
	if err := replace("agents/openclaw/config/native.tpl", `{{- if $pluginEntries }}
{{- $plugins := dig "plugins" (dict) $config }}`, `{{- if or $pluginEntries (gt (int .Values.msteamsWebhookPort) 0) }}
{{- $plugins := dig "plugins" (dict) $config }}`); err != nil {
		return err
	}
	const teamsAllow = `{{- /* Plugins (msteams, acpx) are npm-installed and auto-discovered — only allowlist needed */}}
{{- if gt (int $.Values.msteamsWebhookPort) 0 }}
{{- if not (has "msteams" $pluginsAllow) }}
{{- $pluginsAllow = append $pluginsAllow "msteams" }}
{{- $_ := set $plugins "allow" $pluginsAllow }}
{{- end }}
{{- end }}
`
	if err := replace("agents/openclaw/config/native.tpl", `{{- end }}
{{- $_ := set $config "plugins" $plugins }}
{{- end }}
{{- /* Fix Telegram connectivity`, `{{- end }}
`+teamsAllow+`{{- $_ := set $config "plugins" $plugins }}
{{- end }}
{{- /* Fix Telegram connectivity`); err != nil {
		return err
	}
	const teamsChannel = `{{- /* Configure msteams channel when webhook port is set AND credentials are available */}}
{{- if and (gt (int $.Values.msteamsWebhookPort) 0) (or (and .Values.secrets.msteamsAppId .Values.secrets.msteamsTenantId) .Values.secrets.existingSecret) }}
{{- $msteams := dig "msteams" (dict) $channels }}
{{- $_ := set $msteams "enabled" true }}
{{- $_ := set $msteams "dmPolicy" "allowlist" }}
{{- $_ := set $msteams "groupPolicy" "allowlist" }}
{{- $_ := set $msteams "replyStyle" "thread" }}
{{- $_ := set $msteams "webhook" (dict "port" (int $.Values.msteamsWebhookPort) "path" "/api/messages") }}
{{- $_ := set $channels "msteams" $msteams }}
{{- end }}
`
	if err := replace("agents/openclaw/config/native.tpl", `{{- if $channels }}
{{- $_ := set $config "channels" $channels }}`, teamsChannel+`{{- if $channels }}
{{- $_ := set $config "channels" $channels }}`); err != nil {
		return err
	}
	const teamsSecrets = `                "msteamsAppId": {
                    "type": "string",
                    "description": "Microsoft Teams app ID",
                    "default": ""
                },
                "msteamsAppPassword": {
                    "type": "string",
                    "description": "Microsoft Teams app password",
                    "default": ""
                },
                "msteamsTenantId": {
                    "type": "string",
                    "description": "Microsoft Teams tenant ID",
                    "default": ""
                },
`
	if err := replace("values.schema.json", `                "braveApiKey": {`, teamsSecrets+`                "braveApiKey": {`); err != nil {
		return err
	}
	const teamsValues = `        "msteamsWebhookPort": {
            "type": "integer",
            "description": "MS Teams webhook listener port (0 = disabled, set by operator when spec.msteams is configured)",
            "default": 0,
            "minimum": 0,
            "maximum": 65535
        },
        "msteamsAuthType": {
            "type": "string",
            "description": "MS Teams auth type. Empty string uses secret-based auth, \"managedIdentity\" uses Azure Workload Identity, \"certificate\" uses client certificate.",
            "default": "",
            "enum": [
                "",
                "managedIdentity",
                "certificate"
            ]
        },
        "msteamsAllowedUsers": {
            "type": "string",
            "description": "Comma-separated AAD object IDs allowed to message the Teams bot. Empty allows all (relies on bot-side gating).",
            "default": ""
        }
`
	if err := replace("values.schema.json", `        }
    },
    "allOf": [`, `        },
`+teamsValues+`    },
    "allOf": [`); err != nil {
		return err
	}
	return nil
}

func restoreHermesWebappSchema(schema []byte) ([]byte, error) {
	for _, change := range [][2]string{
		{`                    "description": "Hermes native config.yaml content",
                    "properties": {
                        "webapp": false,
                        "api_server": false,
                        "platforms": {"properties": {"api_server": false}},
                        "gateway": {"properties": {
                            "api_server": false,
                            "platforms": {"properties": {"api_server": false}}
                        }},
                        "dashboard": {"properties": {"basic_auth": false}},
                        "secrets": {
                            "type": "object",
                            "properties": {
                                "sources": {"type": "array", "items": {"type": "string"}},
                                "preserve_existing": {"type": "array", "items": {"type": "string"}},
                                "profile_alias": {"type": "boolean"},
                                "onepassword": {
                                    "type": "object",
                                    "properties": {
                                        "enabled": {"type": "boolean"},
                                        "env": {"type": "object", "propertyNames": {"pattern": "^[A-Za-z_][A-Za-z0-9_]*$", "not": {"pattern": "^(API_SERVER_|HERMES_WEBAPP_|HERMES_DASHBOARD_|CETIRUN_|HERMES_(HOME$|PROFILE|CONFIG|ENV|MANAGED|PYTHON$|INSTALL_|RUNTIME_DIR$|IGNORE_USER_CONFIG$|GATEWAY_CONFIG$)|HOME$|PATH$|PYTHONPATH$|PYTHONHOME$|NODE_PATH$|NODE_OPTIONS$)"}}},
                                        "service_account_token_env": {"type": "string", "not": {"pattern": "^(API_SERVER_|HERMES_WEBAPP_|HERMES_DASHBOARD_|CETIRUN_|HERMES_(HOME$|PROFILE|CONFIG|ENV|MANAGED|PYTHON$|INSTALL_|RUNTIME_DIR$|IGNORE_USER_CONFIG$|GATEWAY_CONFIG$)|HOME$|PATH$|PYTHONPATH$|PYTHONHOME$|NODE_PATH$|NODE_OPTIONS$)"}}
                                    }
                                }
                            },
                            "additionalProperties": {"type": "object", "properties": {"enabled": {"const": false}}}
                        }
                    },
`, `                    "description": "Hermes native config.yaml content",
`},
		{`                    "description": "Environment variables written into ~/.hermes/.env",
                    "additionalProperties": false,
`, `                    "description": "Environment variables written into ~/.hermes/.env",
`},
		{`                        "API_SERVER_ENABLED": {
                            "type": "string",
                            "const": "true",
                            "default": "true"
                        },
`, `                        "API_SERVER_ENABLED": {
                            "type": "string",
                            "default": "true"
                        },
`},
		{`                        "API_SERVER_HOST": {
                            "type": "string",
                            "enum": ["0.0.0.0", "127.0.0.1"],
                            "default": "0.0.0.0"
                        },
`, `                        "API_SERVER_HOST": {
                            "type": "string",
                            "default": "0.0.0.0"
                        },
`},

		{`                        "HERMES_WEBAPP_PUBLIC_URL": {
                            "type": "string",
                            "description": "Operator-owned external Webapp URL including its instance prefix",
                            "pattern": "^$|^https://[A-Za-z0-9.-]+(?::[0-9]+)?/ui/[a-z0-9][a-z0-9-]*$",
                            "default": ""
                        }
`, `                        "HERMES_DASHBOARD_PUBLIC_URL": {
                            "type": "string",
                            "description": "Operator-owned external Dashboard URL including its instance prefix",
                            "default": ""
                        }
`},
		{`        {
            "if": {"properties": {"agentType": {"const": "hermes"}}},
            "then": {"properties": {
                "extraEnv": {"items": {"properties": {"name": {"not": {"pattern": "^(API_SERVER_|HERMES_WEBAPP_|HERMES_DASHBOARD_|CETIRUN_(AI_GATEWAY$|AUTH_KEY$|GATEWAY_.*|INGRESS_.*)|HERMES_(HOME|PROFILE.*|CONFIG.*|ENV.*|MANAGED.*|PYTHON|INSTALL_.*|RUNTIME_DIR|IGNORE_USER_CONFIG|GATEWAY_CONFIG)$|HOME$|PATH$|PYTHONPATH$|PYTHONHOME$)"}}}}},
                "extraEnvFrom": {"maxItems": 0}
            }}
        },
`, ``},
	} {
		if bytes.Count(schema, []byte(change[0])) != 1 {
			return nil, fmt.Errorf("pre-proxy fixture requires the exact Hermes Webapp schema")
		}
		schema = bytes.Replace(schema, []byte(change[0]), []byte(change[1]), 1)
	}
	return schema, nil
}
