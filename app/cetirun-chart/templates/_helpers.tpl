{{/*
Expand the name of the chart.
*/}}
{{- define "cetirun.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cetirun.agent.type" -}}
{{- $type := .Values.agentType | default "hermes" -}}
{{- if not (has $type (list "hermes" "openclaw" "custom")) -}}
{{- fail (printf "unsupported agentType %q" $type) -}}
{{- end -}}
{{- if .Values.runtime -}}
{{- if or (ne .Values.runtime.contractVersion "v1") (ne .Values.runtime.agent.type $type) -}}
{{- fail "runtime contract/type mismatch" -}}
{{- end -}}
{{- end -}}
{{- $type -}}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cetirun.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Shared registry and version defaults. Explicit component image values remain
higher-priority overrides.
*/}}
{{- define "cetirun.registry" -}}
{{- .Values.global.registry | trimSuffix "/" -}}
{{- end }}

{{- define "cetirun.version" -}}
{{- .Values.global.version -}}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "cetirun.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "cetirun.labels" -}}
helm.sh/chart: {{ include "cetirun.chart" . }}
{{ include "cetirun.selectorLabels" . }}
{{- $version := include "cetirun.version" . | default .Chart.AppVersion }}
{{- if $version }}
app.kubernetes.io/version: {{ $version | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "cetirun.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cetirun.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "cetirun.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cetirun.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Resolve the default Kubernetes API token mount with an explicit override. */}}
{{- define "cetirun.automountServiceAccountToken" -}}
{{- if kindIs "bool" .Values.serviceAccount.automountToken -}}
{{ .Values.serviceAccount.automountToken }}
{{- else -}}
{{ or (not (empty .Values.serviceAccount.role)) (not (empty .Values.serviceAccount.name)) .Values.azureWorkloadIdentity.enabled }}
{{- end -}}
{{- end }}

{{/*
Create the name of the secret to use
*/}}
{{- define "cetirun.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- include "cetirun.fullname" . }}
{{- end }}
{{- end }}

{{/*
Create the name of the configmap to use
*/}}
{{- define "cetirun.configMapName" -}}
{{- printf "%s-config" (include "cetirun.fullname" .) }}
{{- end }}

{{/*
Create the name of the PVC to use
*/}}
{{- define "cetirun.pvcName" -}}
{{- if .Values.persistence.existingClaim }}
{{- .Values.persistence.existingClaim }}
{{- else }}
{{- printf "%s-data" (include "cetirun.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Validate that required secrets are provided when not using existing secret
*/}}
{{- define "cetirun.validateSecrets" -}}
{{- include (printf "cetirun.agent.%s.validateSecrets" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Validate Azure Workload Identity prerequisites
*/}}
{{- define "cetirun.validateWorkloadIdentity" -}}
{{- if .Values.azureWorkloadIdentity.enabled -}}
{{- if and (not .Values.serviceAccount.create) (not .Values.serviceAccount.name) -}}
{{- fail "azureWorkloadIdentity.enabled requires serviceAccount.create=true or serviceAccount.name (a pre-annotated BYO service account)" }}
{{- end -}}
{{- end -}}
{{- end }}

{{/* Validate the fixed in-cluster Agent Identity Broker boundary. */}}
{{- define "cetirun.validateAgentIdentity" -}}
{{- if .Values.agentIdentity.enabled -}}
{{- if and (eq .Values.agentType "custom") (not ((.Values.sandboxWorkload | default dict).reconciled | default false)) -}}
{{- fail "Custom runtimes do not support Agent Identity with CetiRun Connections MCP" -}}
{{- end -}}
{{- if gt (len (include "cetirun.serviceAccountName" .)) 63 -}}
{{- fail "agentIdentity.enabled requires a ServiceAccount name no longer than 63 characters" -}}
{{- end -}}
{{- if not .Values.sidecarScope.enabled -}}
{{- fail "agentIdentity.enabled requires sidecarScope.enabled" -}}
{{- end -}}
{{- if not .Values.networkPolicy.enabled -}}
{{- fail "agentIdentity.enabled requires networkPolicy.enabled" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "cetirun.validateConnectionsMCP" -}}
{{- if .Values.connectionsMCP.enabled -}}
{{- if and (eq .Values.agentType "custom") (not ((.Values.sandboxWorkload | default dict).reconciled | default false)) -}}
{{- fail "Custom runtimes do not support CetiRun Connections MCP" -}}
{{- end -}}
{{- if and .Values.sandboxWorkload (not (.Values.sandboxWorkload.reconciled | default false)) -}}
{{- fail "Connections MCP requires the current common-executor workload identity contract" -}}
{{- end -}}
{{- if not (regexMatch "^https://[a-z0-9-]+\\.[a-z0-9-]+\\.svc\\.cluster\\.local(:[0-9]+)?/mcp$" .Values.connectionsMCP.url) -}}
{{- fail "connectionsMCP.url must be an in-cluster HTTPS MCP endpoint" -}}
{{- end -}}
{{- if or (and (not .Values.sandboxWorkload) (not .Values.cetiadmin.enabled) (or (not .Values.serviceAccount.create) (gt (len .Values.serviceAccount.name) 63))) (not .Values.serviceAccount.name) (ne (include "cetirun.automountServiceAccountToken" .) "false") -}}
{{- fail "Connections MCP requires a dedicated explicitly named ServiceAccount without automount" -}}
{{- end -}}
{{- if and .Values.sandboxWorkload (or (gt (len .Values.serviceAccount.name) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" .Values.serviceAccount.name))) -}}
{{- fail "Connections MCP requires a valid Kubernetes ServiceAccount name" -}}
{{- end -}}
{{- if or (not .Values.networkPolicy.enabled) (and (not .Values.sandboxWorkload) (not .Values.sidecarScope.enabled)) (not .Values.connectionsMCP.audience) (not .Values.connectionsMCP.caSecretName) -}}
{{- fail "Connections MCP requires protected network controls and a token audience" -}}
{{- end -}}
{{- range .Values.extraEnv -}}
{{- if has (get . "name") (list "CETIRUN_CONNECTIONS_MCP_URL" "NODE_EXTRA_CA_CERTS") -}}
{{- fail "Connections MCP environment variables are platform-owned" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Validate the platform AI Gateway wiring set by the CetiRun operator. */}}
{{- define "cetirun.validateAIGateway" -}}
{{- include "cetirun.cetiadmin.validate" . -}}
{{- $gateway := .Values.aiGateway | default dict -}}
{{- $proxy := $gateway.copilotProxy | default dict -}}
{{- $agentType := include "cetirun.agent.type" . -}}
{{- if and (ne $agentType "custom") (not $gateway.enabled) -}}
{{- fail "AI Gateway is required for every Agent; direct model access is no longer supported" -}}
{{- end -}}
{{- /* Custom uses the operator-provided runtime.bindings.model Secret and
   network peer, not the native aiGateway chart values. Its renderer validates
   that binding separately. */ -}}
{{- if ne $agentType "custom" -}}
{{- if .Values.secrets.apiKey -}}
{{- fail "secrets.apiKey is no longer supported; configure model credentials in the AI Gateway" -}}
{{- end -}}
{{- if ne (.Values.secrets.provider | default "openai") "openai" -}}
{{- fail "secrets.provider must be openai for the platform AI Gateway" -}}
{{- end -}}
{{- if $gateway.enabled -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" (.Values.aiGateway.keySecretName | default "")) -}}
{{- fail "aiGateway.keySecretName must name a Secret in the release namespace" -}}
{{- end -}}
{{- if not .Values.networkPolicy.enabled -}}
{{- fail "aiGateway.enabled requires networkPolicy.enabled" -}}
{{- end -}}
{{- if and (not .Values.sandboxWorkload) (not .Values.sidecarScope.enabled) -}}
{{- fail "aiGateway.enabled requires sidecarScope.enabled" -}}
{{- end -}}
{{- if and .Values.sandboxWorkload (eq (include "cetirun.agent.type" .) "openclaw") -}}
{{- if not .Values.secrets.apiBase -}}
{{- fail "Sandbox Gateway routing requires secrets.apiBase" -}}
{{- end -}}
{{- $defaultGranted := false -}}
{{- range $gateway.models -}}
{{- if eq .id $.Values.secrets.model -}}
{{- $defaultGranted = true -}}
{{- end -}}
{{- end -}}
{{- if not $defaultGranted -}}
{{- fail "Sandbox default model must be in aiGateway.models" -}}
{{- end -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" (.Values.networkPolicy.gatewayNamespace | default "")) -}}
{{- fail "aiGateway.enabled requires networkPolicy.gatewayNamespace" -}}
{{- end -}}
{{- $hermesKeyEnv := dig "hermes" "keyEnv" "CETIRUN_GATEWAY_API_KEY" $gateway -}}
{{- /* Init removes the key's variable from Hermes' .env, so it cannot be one
   the chart writes there for another setting: the API server's, or a web
   search key. The model provider keys can carry it. */ -}}
{{- if has $hermesKeyEnv (list "API_SERVER_ENABLED" "API_SERVER_HOST" "API_SERVER_PORT" "API_SERVER_KEY" "API_SERVER_CORS_ORIGINS" "BRAVE_API_KEY" "PERPLEXITY_API_KEY") -}}
{{- fail (printf "aiGateway.hermes.keyEnv must not be %s, which the chart writes into Hermes' .env for another setting" $hermesKeyEnv) -}}
{{- end -}}
{{- /* The key replaces the variable in Hermes' container, so only an API key
   variable can carry it: not one that controls the container's processes,
   such as PATH or LD_PRELOAD, nor one the chart sets there. No list could
   name every such variable, but none of them ends in _API_KEY. */ -}}
{{- if not (regexMatch "^[A-Z][A-Z0-9_]*_API_KEY$" $hermesKeyEnv) -}}
{{- fail (printf "aiGateway.hermes.keyEnv must not be %s: the key replaces that variable in Hermes' container, so it must name an API key variable, ending in _API_KEY" $hermesKeyEnv) -}}
{{- end -}}
{{- range .Values.extraEnv -}}
{{- if has (get . "name") (list "OPENAI_API_KEY" "OPENAI_BASE_URL" "CETIRUN_AI_GATEWAY" "CETIRUN_NATIVE_SANDBOX" $hermesKeyEnv) -}}
{{- fail (printf "OPENAI_API_KEY, OPENAI_BASE_URL, CETIRUN_AI_GATEWAY, CETIRUN_NATIVE_SANDBOX and %s are reserved when aiGateway.enabled" $hermesKeyEnv) -}}
{{- end -}}
{{- end -}}
{{- range $gateway.models -}}
{{- if hasPrefix "openai/" (printf "%v" .id) -}}
{{- fail "aiGateway.models ids are Gateway model names without a provider prefix" -}}
{{- end -}}
{{- end -}}
{{- if $proxy.enabled -}}
{{- if not $proxy.image -}}
{{- fail "aiGateway.copilotProxy.image is required" -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" ($proxy.masterKeySecretName | default "")) -}}
{{- fail "aiGateway.copilotProxy.masterKeySecretName must name a Secret in the release namespace" -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" ($proxy.tokenSecretName | default "")) -}}
{{- fail "aiGateway.copilotProxy.tokenSecretName must name a Secret in the release namespace" -}}
{{- end -}}
{{- range ($proxy.responsesModels | default list) -}}
{{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$" (printf "%v" .)) -}}
{{- fail "aiGateway.copilotProxy.responsesModels must be GitHub Copilot model IDs" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Name of the per-Agent GitHub Copilot proxy's objects. The operator derives
the proxy's Service address from it, so keep the two in step.
*/}}
{{- define "cetirun.copilotProxyName" -}}
{{- printf "%s-copilot-proxy" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "cetirun.copilotProxySelectorLabels" -}}
app.kubernetes.io/name: cetirun-copilot-proxy
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "cetirun.copilotProxyLabels" -}}
helm.sh/chart: {{ include "cetirun.chart" . }}
{{ include "cetirun.copilotProxySelectorLabels" . }}
app.kubernetes.io/component: copilot-proxy
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
LiteLLM configuration of the Copilot proxy: every model the owner's account
offers, under its own name. The Gateway decides which ones the Agent may use.
*/}}
{{- define "cetirun.copilotProxyConfig" -}}
model_list:
  {{- /* GitHub Copilot serves some models only through the Responses API.
     Naming them lets LiteLLM bridge chat completion requests to it. */}}
  {{- range ((.Values.aiGateway.copilotProxy).responsesModels | default list) }}
  - model_name: {{ . | quote }}
    litellm_params:
      model: {{ printf "github_copilot/%s" . | quote }}
    model_info:
      mode: responses
  {{- end }}
  - model_name: "*"
    litellm_params:
      model: "github_copilot/*"
litellm_settings:
  # The Gateway and the Agent decide on retries.
  num_retries: 0
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
{{- end }}

{{/*
Validate autoscaling settings for single-instance app
*/}}
{{- define "cetirun.validateAutoscaling" -}}
{{- if .Values.autoscaling.enabled -}}
{{- if or (ne (int .Values.autoscaling.minReplicas) 1) (ne (int .Values.autoscaling.maxReplicas) 1) -}}
{{- fail "autoscaling.enabled requires minReplicas=1 and maxReplicas=1 for CetiRun agents" }}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Validate port consistency between service.targetPort and openclaw.config.gateway.port
*/}}
{{- define "cetirun.validatePorts" -}}
{{- include (printf "cetirun.agent.%s.validatePorts" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Validate ttyd port does not collide with gateway or noVNC ports
*/}}
{{- define "cetirun.validateTtydPort" -}}
{{- include (printf "cetirun.agent.%s.validateTtydPort" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Return the appropriate apiVersion for ingress
*/}}
{{- define "cetirun.ingress.apiVersion" -}}
{{- if and (.Capabilities.APIVersions.Has "networking.k8s.io/v1") (semverCompare ">= 1.19-0" .Capabilities.KubeVersion.Version) }}
{{- print "networking.k8s.io/v1" }}
{{- else if .Capabilities.APIVersions.Has "networking.k8s.io/v1beta1" }}
{{- print "networking.k8s.io/v1beta1" }}
{{- else }}
{{- print "extensions/v1beta1" }}
{{- end }}
{{- end }}

{{/*
Return true if the ingress supports pathType
*/}}
{{- define "cetirun.ingress.supportsPathType" -}}
{{- if eq (include "cetirun.ingress.apiVersion" .) "networking.k8s.io/v1" }}
{{- print "true" }}
{{- else }}
{{- print "false" }}
{{- end }}
{{- end }}

{{/*
Validate shared skills configuration
*/}}
{{- define "cetirun.validateSkills" -}}
{{- if and .Values.skills.shared (not .Values.skills.existingClaim) -}}
{{- if not .Values.skills.resourceGroup -}}
{{- fail "skills.resourceGroup is required when skills.shared is true" }}
{{- end -}}
{{- if not .Values.skills.storageAccount -}}
{{- fail "skills.storageAccount is required when skills.shared is true" }}
{{- end -}}
{{- if not .Values.skills.containerName -}}
{{- fail "skills.containerName is required when skills.shared is true" }}
{{- end -}}
{{- if not .Values.skills.mountPath -}}
{{- fail "skills.mountPath is required when skills.shared is true" }}
{{- end -}}
{{- if not .Values.skills.storageSize -}}
{{- fail "skills.storageSize is required when skills.shared is true" }}
{{- end -}}
{{- if not .Values.skills.storageClassName -}}
{{- fail "skills.storageClassName is required when skills.shared is true" }}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Derive whether Tailscale is enabled.
True when secrets.tailscaleAuthKey is set in values, OR when the
resolved Secret (chart-managed or existingSecret) already contains a
non-empty TAILSCALE_AUTH_KEY (covers helm-upgrade and operator-managed
scenarios where the key is written directly into the Secret).
*/}}
{{- define "cetirun.tailscaleEnabled" -}}
{{- if not .Values.sandboxWorkload -}}
{{- if .Values.secrets.tailscaleAuthKey -}}
true
{{- else -}}
{{- $secretName := include "cetirun.secretName" . -}}
{{- $existingSecret := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if and $existingSecret $existingSecret.data -}}
{{- $key := index $existingSecret.data "TAILSCALE_AUTH_KEY" | default "" -}}
{{- if $key -}}
true
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Container image reference with optional digest pinning.
When image.digest is set, produces: repository:tag@digest
Otherwise: repository:tag
*/}}
{{- define "cetirun.image" -}}
{{- if .Values.runtime -}}
{{ .Values.runtime.agent.image }}
{{- else -}}
{{- include (printf "cetirun.agent.%s.image" (include "cetirun.agent.type" .)) . -}}
{{- end -}}
{{- end }}

{{/*
Container image pull policy for the selected agent image.
*/}}
{{- define "cetirun.imagePullPolicy" -}}
{{- include (printf "cetirun.agent.%s.imagePullPolicy" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Shared skills PV name (cluster-scoped, globally unique).
*/}}
{{- define "cetirun.sharedSkillsPVName" -}}
{{- printf "%s-%s-shared-skills" (include "cetirun.fullname" .) .Release.Namespace | trunc 253 | trimSuffix "-" }}
{{- end }}

{{/*
Shared skills PVC name (namespace-scoped).
*/}}
{{- define "cetirun.sharedSkillsPVCName" -}}
{{- if .Values.skills.existingClaim -}}
{{- .Values.skills.existingClaim -}}
{{- else -}}
{{- printf "%s-shared-skills" (include "cetirun.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end -}}
{{- end }}

{{/*
Validate that browser attach-only mode does not point at the local desktop
CDP endpoint when the desktop sidecar is disabled.
*/}}
{{- define "cetirun.validateDesktopBrowserConfig" -}}
{{- include (printf "cetirun.agent.%s.validateDesktopBrowserConfig" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Desktop sidecar container image reference with optional digest pinning.
When desktop.image.digest is set, produces: repository:tag@digest
Otherwise: repository:tag
*/}}
{{- define "cetirun.desktopImage" -}}
{{- include "cetirun.validateDesktopBrowserConfig" . -}}
{{- $registry := include "cetirun.registry" . -}}
{{- $version := include "cetirun.version" . -}}
{{- $repo := .Values.desktop.image.repository -}}
{{- if $registry }}{{- $repo = printf "%s/cetirun-desktop" $registry -}}{{- end -}}
{{- $tag := $version | default .Values.desktop.image.tag | default .Chart.AppVersion -}}
{{ $repo }}:{{ $tag }}{{ if .Values.desktop.image.digest }}@{{ .Values.desktop.image.digest }}{{ end }}
{{- end }}

{{/*
=== Agent-type dispatcher templates ===
These dispatch to the appropriate agent-specific template based on .Values.agentType.
*/}}

{{/*
Dispatcher: init containers
*/}}
{{- define "cetirun.agent.initContainers" -}}
{{- include (printf "cetirun.agent.%s.initContainers" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Dispatcher: container args
*/}}
{{- define "cetirun.agent.args" -}}
{{- include (printf "cetirun.agent.%s.args" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Dispatcher: container ports
*/}}
{{- define "cetirun.agent.ports" -}}
{{- if .Values.runtime -}}
{{- range .Values.runtime.ports }}
- name: {{ .name }}
  containerPort: {{ .number }}
  protocol: TCP
{{- end }}
{{- else -}}
{{- include (printf "cetirun.agent.%s.ports" (include "cetirun.agent.type" .)) . -}}
{{- end }}
{{- end }}

{{/*
Dispatcher: container env vars (agent-specific)
*/}}
{{- define "cetirun.agent.envVars" -}}
{{- include (printf "cetirun.agent.%s.envVars" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Dispatcher: liveness/readiness probe path and port
*/}}
{{- define "cetirun.agent.probePathPort" -}}
{{- if .Values.runtime -}}
path: {{ .Values.runtime.http.healthPath }}
port: {{ .Values.runtime.http.portName }}
{{- else -}}
{{- include (printf "cetirun.agent.%s.probePathPort" (include "cetirun.agent.type" .)) . -}}
{{- end }}
{{- end }}

{{/*
Dispatcher: readiness probe path and port
*/}}
{{- define "cetirun.agent.readinessProbePathPort" -}}
{{- if .Values.runtime -}}
path: {{ default .Values.runtime.http.healthPath .Values.runtime.http.readinessPath }}
port: {{ .Values.runtime.http.portName }}
{{- else -}}
{{- include (printf "cetirun.agent.%s.readinessProbePathPort" (include "cetirun.agent.type" .)) . -}}
{{- end }}
{{- end }}

{{/*
Dispatcher: startup probe path and port
*/}}
{{- define "cetirun.agent.startupProbePathPort" -}}
{{- if .Values.runtime -}}
path: {{ .Values.runtime.http.startupPath }}
port: {{ .Values.runtime.http.portName }}
{{- else -}}
{{- include (printf "cetirun.agent.%s.startupProbePathPort" (include "cetirun.agent.type" .)) . -}}
{{- end }}
{{- end }}

{{/*
Protected built-in probes run inside the container, without host ingress grants.
*/}}
{{- define "cetirun.agent.healthProbe" -}}
{{- $root := .root -}}
{{- $kind := include "cetirun.agent.type" $root -}}
{{- if eq $kind "hermes" -}}
exec:
  command:
    {{- if eq .pathPort "cetirun.agent.readinessProbePathPort" }}
    - /usr/local/bin/hermes-python
    - /etc/cetirun/hermes-runtime.py
    - readiness
    {{- else }}
    - node
    - /etc/cetirun/hermes-health-probe.cjs
    {{- end }}
    - --timeout
    - {{ .timeoutSeconds | quote }}
{{- else if and $root.Values.sandboxWorkload (eq $kind "openclaw") -}}
{{- $endpoint := include .pathPort $root | fromYaml -}}
{{- $port := 0 -}}
{{- range (include "cetirun.agent.ports" $root | fromYamlArray) -}}
{{- if eq .name $endpoint.port -}}{{- $port = int .containerPort -}}{{- end -}}
{{- end -}}
{{- if eq $port 0 -}}{{- fail "Sandbox health probe requires its resolved runtime port" -}}{{- end -}}
exec:
  command:
    - node
    - /etc/cetirun/health-probe.cjs
    - {{ $port | quote }}
    - {{ $endpoint.path | quote }}
    - {{ mul (int .timeoutSeconds) 1000 | quote }}
{{- else -}}
httpGet:
  {{- include .pathPort $root | nindent 2 }}
{{- end -}}
{{- end }}

{{/*
Dispatcher: Tailscale serve port
*/}}
{{- define "cetirun.agent.tailscaleServePort" -}}
{{- include (printf "cetirun.agent.%s.tailscaleServePort" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Dispatcher: logger volumeMount
*/}}
{{- define "cetirun.agent.loggerVolumeMount" -}}
{{- include (printf "cetirun.agent.%s.loggerVolumeMount" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Dispatcher: service ports
*/}}
{{- define "cetirun.agent.servicePorts" -}}
{{- include (printf "cetirun.agent.%s.servicePorts" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Dispatcher: network policy ingress ports
*/}}
{{- define "cetirun.agent.networkPolicyIngressPorts" -}}
{{- if eq (include "cetirun.agent.type" .) "custom" -}}
{{- include "cetirun.agent.custom.networkPolicyIngressPorts" . -}}
{{- else if .Values.runtime -}}
{{- range .Values.runtime.ports }}
- protocol: TCP
  port: {{ .number }}
{{- end }}
{{- else -}}
{{- include (printf "cetirun.agent.%s.networkPolicyIngressPorts" (include "cetirun.agent.type" .)) . -}}
{{- end }}
{{- end }}

{{/*
Shared init container that installs npm-packaged skills onto the PVC.
Rendered only when .Values.skills.npm is non-empty. Used by both agents.
Both the openclaw-gateway and hermes-gateway images intentionally bundle
node/npm at /usr/local/bin/npm so this init container can run regardless
of agentType — see the agent Dockerfiles.
*/}}
{{- define "cetirun.initSkillsContainer" -}}
{{- if .Values.skills.npm }}
- name: init-skills
  image: "{{ include "cetirun.image" . }}"
  imagePullPolicy: {{ include "cetirun.imagePullPolicy" . }}
  securityContext:
    runAsUser: 1024
    runAsNonRoot: true
    allowPrivilegeEscalation: false
    capabilities:
      drop:
        - ALL
    seccompProfile:
      type: RuntimeDefault
  command: ["/bin/sh", "/etc/cetirun/init-skills.sh"]
  env:
    - name: NPM_PACKAGES
      value: {{ join " " .Values.skills.npm | quote }}
    - name: SKILLS_DIR
      value: "/home-data/.cetirun/skills/npm"
    - name: NPM_CACHE_DIR
      value: "/home-data/.cetirun/.npm-cache"
    {{- with .Values.skills.npmRegistry }}
    - name: NPM_REGISTRY
      value: {{ . | quote }}
    {{- end }}
  volumeMounts:
    - name: data
      mountPath: /home-data
    - name: tmp
      mountPath: /tmp
    - name: config
      mountPath: /etc/cetirun
      readOnly: true
{{- end }}
{{- end }}

{{/*
Helper: list of npm skill external dirs (one per package), used by both
agents to extend their skill load path. Sanitization mirrors init-skills.sh:
  "@scope/name@1.2.3" -> "/home/vibe/.cetirun/skills/npm/@scope__name"
  "matrix"            -> "/home/vibe/.cetirun/skills/npm/matrix"
*/}}
{{- define "cetirun.npmSkillDirs" -}}
{{- $dirs := list -}}
{{- range .Values.skills.npm -}}
  {{- $spec := . | toString -}}
  {{- $name := "" -}}
  {{- if hasPrefix "@" $spec -}}
    {{- $rest := trimPrefix "@" $spec -}}
    {{- $name = printf "@%s" (index (splitList "@" $rest) 0) -}}
  {{- else -}}
    {{- $name = index (splitList "@" $spec) 0 -}}
  {{- end -}}
  {{- $dir := replace "/" "__" $name -}}
  {{- $dirs = append $dirs (printf "/home/vibe/.cetirun/skills/npm/%s" $dir) -}}
{{- end -}}
{{- $dirs | toJson -}}
{{- end }}

{{/*
Dispatcher: configmap data
*/}}
{{- define "cetirun.agent.configmapData" -}}
{{- include (printf "cetirun.agent.%s.configmapData" (include "cetirun.agent.type" .)) . -}}
{{- end }}

{{/*
Dispatcher: secret data
*/}}
{{- define "cetirun.agent.secretData" -}}
{{- include (printf "cetirun.agent.%s.secretData" (include "cetirun.agent.type" .)) . -}}
{{- end }}
