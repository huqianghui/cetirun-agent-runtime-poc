{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}

{{- define "cetirun.agent.hermes.validateSecrets" -}}
{{- $_ := required "hermes.env.HERMES_WEBAPP_PUBLIC_URL is required for Hermes" .Values.hermes.env.HERMES_WEBAPP_PUBLIC_URL -}}
{{- $source := dig "secrets" "onepassword" (dict) .Values.hermes.config -}}
{{- $keyEnv := dig "hermes" "keyEnv" "CETIRUN_GATEWAY_API_KEY" .Values.aiGateway -}}
{{- if or (hasKey (get $source "env" | default dict) $keyEnv) (eq (get $source "service_account_token_env" | default "OP_SERVICE_ACCOUNT_TOKEN") $keyEnv) -}}
{{- fail "Hermes secret sources must not replace the AI Gateway key" -}}
{{- end -}}
{{- end }}
{{- define "cetirun.agent.hermes.validateDesktopBrowserConfig" -}}{{- end }}

{{- define "cetirun.agent.hermes.validatePorts" -}}
{{- $apiPort := int .Values.hermes.env.API_SERVER_PORT -}}
{{- if and .Values.desktop.enabled (eq $apiPort 5900) -}}
{{- fail "hermes API port must not collide with desktop VNC port (5900)" }}
{{- end -}}
{{- if eq $apiPort 9120 -}}
{{- fail "hermes API port must not collide with Webapp port (9120)" }}
{{- end -}}
{{- if and .Values.desktop.enabled (eq $apiPort 6080) -}}
{{- fail "hermes API port must not collide with noVNC port (6080)" }}
{{- end -}}
{{- end }}

{{- define "cetirun.agent.hermes.validateTtydPort" -}}
{{- if .Values.ttyd.enabled -}}
{{- $apiPort := int .Values.hermes.env.API_SERVER_PORT -}}
{{- $ttydPort := int .Values.ttyd.port -}}
{{- if and .Values.desktop.enabled (eq $ttydPort 5900) -}}
{{- fail "ttyd.port must not collide with desktop VNC port (5900)" }}
{{- end -}}
{{- if eq $ttydPort 9120 -}}
{{- fail "ttyd.port (9120) must not collide with Hermes Webapp port (9120)" }}
{{- end -}}
{{- if eq $ttydPort $apiPort -}}
{{- fail (printf "ttyd.port (%d) must not collide with hermes API port (%d)" $ttydPort $apiPort) }}
{{- end -}}
{{- if and .Values.desktop.enabled (eq $ttydPort 6080) -}}
{{- fail "ttyd.port (6080) must not collide with noVNC port (6080)" }}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "cetirun.agent.hermes.image" -}}
{{- $registry := include "cetirun.registry" . -}}
{{- $version := include "cetirun.version" . -}}
{{- $repo := .Values.hermes.image.repository -}}
{{- if $registry }}{{- $repo = printf "%s/hermes-gateway" $registry -}}{{- end -}}
{{- $tag := $version | default .Values.hermes.image.tag | default .Chart.AppVersion -}}
{{- $digest := .Values.hermes.image.digest -}}
{{ $repo }}:{{ $tag }}{{ if $digest }}@{{ $digest }}{{ end }}
{{- end }}

{{- define "cetirun.agent.hermes.imagePullPolicy" -}}
{{ .Values.hermes.image.pullPolicy | default .Values.image.pullPolicy | default "Always" }}
{{- end }}

{{/*
Hermes agent-specific named templates.
These are called by dispatcher templates in _helpers.tpl based on .Values.agentType.
*/}}

{{/*
Hermes init containers
*/}}
{{- define "cetirun.agent.hermes.initContainers" -}}
- name: init-home-data
  image: "{{ include "cetirun.image" . }}"
  imagePullPolicy: {{ include "cetirun.imagePullPolicy" . }}
  securityContext:
    {{- if .Values.sandboxWorkload }}
    {{- toYaml .Values.sandboxWorkload.securityContext | nindent 4 }}
    {{- else }}
    runAsUser: 1024
    runAsNonRoot: true
    allowPrivilegeEscalation: false
    capabilities:
      drop:
        - ALL
    seccompProfile:
      type: RuntimeDefault
    {{- end }}
  {{- with .Values.sandboxWorkload }}
  resources:
    {{- toYaml .helperResources | nindent 4 }}
  {{- end }}
  command: ["/bin/sh", "/etc/cetirun/hermes-init-home.sh"]
  env:
    {{- include "cetirun.context.initEnv" . | nindent 4 }}
    - name: CETIRUN_INIT_IMAGE
      value: {{ include "cetirun.image" . | quote }}
    - name: HERMES_HOME
      value: "/home/vibe/.hermes"
    {{- if (.Values.sandboxWorkload | default dict).ingressMTLS }}
    - name: CETIRUN_INGRESS_MTLS
      value: "true"
    {{- end }}
    # The Gateway owns the model choice.
    - name: CETIRUN_AI_GATEWAY
      value: "true"
    - name: CETIRUN_GATEWAY_KEY_ENV
      value: {{ dig "hermes" "keyEnv" "CETIRUN_GATEWAY_API_KEY" .Values.aiGateway | quote }}
  volumeMounts:
    {{- include "cetirun.context.initMounts" . | nindent 4 }}
    - name: data
      mountPath: /home-data
    - name: tmp
      mountPath: /tmp
    - name: config
      mountPath: /etc/cetirun
      readOnly: true
    - name: secrets
      mountPath: /etc/cetirun-secrets
      readOnly: true
{{ include "cetirun.initSkillsContainer" . }}
{{- end }}

{{/*
Hermes container args (none)
*/}}
{{- define "cetirun.agent.hermes.args" -}}
{{- end }}

{{/*
Hermes container ports (agent-specific only)
*/}}
{{- define "cetirun.agent.hermes.ports" -}}
- name: api
  containerPort: {{ .Values.hermes.env.API_SERVER_PORT }}
  protocol: TCP
- name: webapp
  containerPort: 9120
  protocol: TCP
{{- end }}

{{/*
Hermes container env vars (agent-specific only)
*/}}
{{- define "cetirun.agent.hermes.envVars" -}}
- name: HERMES_HOME
  value: "/home/vibe/.hermes"
- name: HERMES_MANAGED_DIR
  value: "/etc/cetirun/hermes-managed"
- name: HERMES_PYTHON
  value: "/usr/local/bin/hermes-python"
- name: HERMES_WEBAPP_ENABLED
  value: "true"
- name: HERMES_WEBAPP_HOST
  {{- if (.Values.sandboxWorkload | default dict).ingressMTLS }}
  value: "127.0.0.1"
  {{- else }}
  value: "0.0.0.0"
  {{- end }}
- name: HERMES_WEBAPP_PORT
  value: "9120"
- name: HERMES_WEBAPP_PUBLIC_URL
  value: {{ .Values.hermes.env.HERMES_WEBAPP_PUBLIC_URL | quote }}
- name: CETIRUN_GATEWAY_KEY_ENV
  value: {{ dig "hermes" "keyEnv" "CETIRUN_GATEWAY_API_KEY" .Values.aiGateway | quote }}
- name: HERMES_DASHBOARD_BASIC_AUTH_USERNAME
  value: "cetirun"
- name: HERMES_DASHBOARD_BASIC_AUTH_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun.secretName" . }}
      key: API_SERVER_KEY
- name: HERMES_DASHBOARD_BASIC_AUTH_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun.secretName" . }}
      key: CETIRUN_AUTH_KEY
- name: API_SERVER_ENABLED
  value: {{ .Values.hermes.env.API_SERVER_ENABLED | quote }}
- name: API_SERVER_HOST
  value: {{ .Values.hermes.env.API_SERVER_HOST | quote }}
- name: API_SERVER_PORT
  value: {{ .Values.hermes.env.API_SERVER_PORT | quote }}
- name: API_SERVER_CORS_ORIGINS
  value: {{ .Values.hermes.env.API_SERVER_CORS_ORIGINS | quote }}
- name: API_SERVER_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun.secretName" . }}
      key: API_SERVER_KEY
- name: CETIRUN_AUTH_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun.secretName" . }}
      key: CETIRUN_AUTH_KEY
- name: CETIRUN_AGENT_TYPE
  value: "hermes"
- name: CETIRUN_AI_GATEWAY
  value: "true"
# This Agent's AI Gateway virtual key, managed by the CetiRun operator.
- name: {{ dig "hermes" "keyEnv" "CETIRUN_GATEWAY_API_KEY" .Values.aiGateway }}
  valueFrom:
    secretKeyRef:
      name: {{ .Values.aiGateway.keySecretName }}
      key: {{ .Values.aiGateway.keySecretKey | default "api-key" }}
{{- end }}

{{/*
Hermes liveness probe path and port
*/}}
{{- define "cetirun.agent.hermes.probePathPort" -}}
path: /health
port: api
{{- end }}

{{/*
Hermes readiness probe path and port
*/}}
{{- define "cetirun.agent.hermes.readinessProbePathPort" -}}
path: /health
port: api
{{- end }}

{{/*
Hermes startup probe path and port
*/}}
{{- define "cetirun.agent.hermes.startupProbePathPort" -}}
path: /health
port: api
{{- end }}

{{/*
Hermes Tailscale serve port value
*/}}
{{- define "cetirun.agent.hermes.tailscaleServePort" -}}
{{ .Values.hermes.env.API_SERVER_PORT | quote }}
{{- end }}

{{/*
Hermes logger volumeMount
*/}}
{{- define "cetirun.agent.hermes.loggerVolumeMount" -}}
- name: data
  mountPath: /home/vibe/.hermes
  subPath: .hermes
  readOnly: true
{{- end }}

{{/*
Hermes service ports (agent-specific only)
*/}}
{{- define "cetirun.agent.hermes.servicePorts" -}}
- port: {{ .Values.hermes.env.API_SERVER_PORT }}
  targetPort: api
  protocol: TCP
  name: api
- port: 9120
  targetPort: webapp
  protocol: TCP
  name: webapp
{{- end }}

{{/*
Hermes networkpolicy ingress ports (agent-specific only)
*/}}
{{- define "cetirun.agent.hermes.networkPolicyIngressPorts" -}}
- protocol: TCP
  port: {{ .Values.hermes.env.API_SERVER_PORT }}
- protocol: TCP
  port: 9120
{{- end }}

{{/*
Hermes configmap data
*/}}
{{- define "cetirun.agent.hermes.configmapData" }}
  hermes-gateway.sh: |
{{ .Files.Get "agents/hermes/bootstrap/hermes-gateway.sh" | indent 4 }}
  hermes-runtime.py: |
{{ .Files.Get "agents/hermes/bootstrap/hermes-runtime.py" | indent 4 }}
  hermes-health-probe.cjs: |
{{ .Files.Get "agents/hermes/bootstrap/hermes-health-probe.cjs" | indent 4 }}
  hermes-managed.yaml: |
    webapp:
      enabled: true
      {{- if (.Values.sandboxWorkload | default dict).ingressMTLS }}
      host: 127.0.0.1
      {{- else }}
      host: 0.0.0.0
      {{- end }}
      port: 9120
      public_url: {{ .Values.hermes.env.HERMES_WEBAPP_PUBLIC_URL | quote }}
    {{- if not (.Values.sandboxWorkload | default dict).ingressMTLS }}
    dashboard:
      trusted_proxies:
        - 127.0.0.6
    {{- end }}
  hermes-config.yaml: |
    {{- if .Values.runtime }}
{{ index .Values.runtime.configFiles "hermes-config.yaml" | indent 4 }}
    {{- else }}
    {{- $context := dict "Values" .Values "Chart" .Chart "SkillDirs" (include "cetirun.npmSkillDirs" . | fromJsonArray) "TailscaleEnabled" (include "cetirun.tailscaleEnabled" .) }}
{{ tpl (.Files.Get "agents/hermes/config/native.tpl") $context | trim | indent 4 }}
    {{- end }}
  hermes-soul.md: |
{{ .Files.Get "agents/hermes/personality/SOUL.md" | indent 4 }}
  hermes-user.md: |
    § Name: {{ .Values.user.displayName | default "" }}
    § Alias: {{ .Values.user.alias | default "" }}
  hermes-team.md: |
    {{- if or .Values.user.role .Values.user.team .Values.user.description }}
    {{- if .Values.user.role }}
    § Role: {{ .Values.user.role }}
    {{- end }}
    {{- if .Values.user.team }}
    § Team: {{ .Values.user.team }}
    {{- end }}
    {{- if .Values.user.description }}
    § Notes:
    {{- range .Values.user.description }}
    § - {{ . }}
    {{- end }}
    {{- end }}
    {{- end }}
  hermes-init-home.sh: |
{{ .Files.Get "agents/hermes/bootstrap/hermes-init-home.sh" | indent 4 }}
  hermes-apply-config.py: |
{{ .Files.Get "agents/hermes/bootstrap/hermes-apply-config.py" | indent 4 }}
  init-skills.sh: |
{{ .Files.Get "configs/init-skills.sh" | indent 4 }}
  session-logger.sh: |
{{ .Files.Get "configs/session-logger.sh" | indent 4 }}
{{- end }}

{{/*
Hermes secret data (agent-specific only).
Receives the standard chart context (.). Uses lookup to find existing secrets.
*/}}
{{- define "cetirun.agent.hermes.secretData" -}}
  {{- $secretName := include "cetirun.secretName" . -}}
  {{- $existingSecret := lookup "v1" "Secret" .Release.Namespace $secretName -}}
  {{- $apiServerKey := .Values.hermes.env.API_SERVER_KEY | default "" }}
  {{- if and (not $apiServerKey) $existingSecret $existingSecret.data (index $existingSecret.data "API_SERVER_KEY") }}
  {{- $apiServerKey = (index $existingSecret.data "API_SERVER_KEY" | b64dec) }}
  {{- else if not $apiServerKey }}
  {{- $apiServerKey = randAlphaNum 48 }}
  {{- end }}

  {{- /* API_SERVER_KEY and hermes.env for Hermes API server. */}}
  API_SERVER_KEY: {{ $apiServerKey | b64enc | quote }}
  {{- $hermesEnv := printf "API_SERVER_ENABLED=%s\nAPI_SERVER_HOST=%s\nAPI_SERVER_PORT=%s\nAPI_SERVER_KEY=%s\nAPI_SERVER_CORS_ORIGINS=%s\nBRAVE_API_KEY=%s\nPERPLEXITY_API_KEY=%s\n" (.Values.hermes.env.API_SERVER_ENABLED | toString) (.Values.hermes.env.API_SERVER_HOST | toString) (.Values.hermes.env.API_SERVER_PORT | toString) $apiServerKey (.Values.hermes.env.API_SERVER_CORS_ORIGINS | toString) (.Values.secrets.braveApiKey | default "") (.Values.secrets.perplexityApiKey | default "") }}
  hermes.env: {{ $hermesEnv | b64enc | quote }}
{{- end }}
