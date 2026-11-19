{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}

{{- define "cetirun.agent.openclaw.validateSecrets" -}}
{{- if and (not .Values.secrets.existingSecret) (not .Values.secrets.openclawGatewayToken) }}
{{- fail "secrets.openclawGatewayToken is required when not using an existing secret" }}
{{- end }}
{{- end }}

{{- define "cetirun.agent.openclaw.validatePorts" -}}
{{- if and .Values.desktop.enabled (eq (int .Values.service.targetPort) 5900) -}}
{{- fail "gateway port must not collide with desktop VNC port (5900)" }}
{{- end -}}
{{- if ne (int .Values.service.targetPort) (int .Values.openclaw.config.gateway.port) -}}
{{- fail (printf "service.targetPort (%d) must match openclaw.config.gateway.port (%d)" (int .Values.service.targetPort) (int .Values.openclaw.config.gateway.port)) }}
{{- end -}}
{{- end }}

{{- define "cetirun.agent.openclaw.validateTtydPort" -}}
{{- if .Values.ttyd.enabled -}}
{{- if and .Values.desktop.enabled (eq (int .Values.ttyd.port) 5900) -}}
{{- fail "ttyd.port must not collide with desktop VNC port (5900)" }}
{{- end -}}
{{- if or (eq (int .Values.ttyd.port) (int .Values.service.targetPort)) (eq (int .Values.ttyd.port) 6080) -}}
{{- fail (printf "ttyd.port (%d) must not collide with gateway port (%d) or noVNC port (6080)" (int .Values.ttyd.port) (int .Values.service.targetPort)) }}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "cetirun.agent.openclaw.validateDesktopBrowserConfig" -}}
{{- if and (not .Values.desktop.enabled) (dig "browser" "enabled" true .Values.openclaw.config) (dig "browser" "attachOnly" false .Values.openclaw.config) -}}
{{- fail "desktop.enabled=false is incompatible with browser.attachOnly=true; enable the desktop sidecar or set openclaw.config.browser.attachOnly=false" -}}
{{- end -}}
{{- end }}

{{- define "cetirun.agent.openclaw.image" -}}
{{- $registry := include "cetirun.registry" . -}}
{{- $version := include "cetirun.version" . -}}
{{- $repo := .Values.image.repository | default .Values.openclaw.image.repository | default "mcr.microsoft.com/aks/ai-runtime/cetirun/openclaw-gateway" -}}
{{- if $registry }}{{- $repo = printf "%s/openclaw-gateway" $registry -}}{{- end -}}
{{- $tag := $version | default .Values.image.tag | default .Values.openclaw.image.tag | default .Chart.AppVersion -}}
{{- $digest := .Values.image.digest | default .Values.openclaw.image.digest -}}
{{ $repo }}:{{ $tag }}{{ if $digest }}@{{ $digest }}{{ end }}
{{- end }}

{{- define "cetirun.agent.openclaw.imagePullPolicy" -}}
{{ .Values.image.pullPolicy | default .Values.openclaw.image.pullPolicy | default "Always" }}
{{- end }}

{{/*
OpenClaw agent-specific named templates.
These are called by dispatcher templates in _helpers.tpl based on .Values.agentType.
*/}}

{{/*
OpenClaw init containers
*/}}
{{- define "cetirun.agent.openclaw.initContainers" -}}
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
  {{- if .Values.sandboxWorkload }}
  command: ["/usr/bin/tini", "-g", "--", "/bin/sh", "/etc/cetirun/init-home.sh"]
  {{- else }}
  command: ["/bin/sh", "/etc/cetirun/init-home.sh"]
  {{- end }}
  env:
    {{- include "cetirun.context.initEnv" . | nindent 4 }}
    - name: CETIRUN_INIT_IMAGE
      value: {{ include "cetirun.image" . | quote }}
    {{- if dig "git" "azureDevOps" "enabled" true .Values.openclaw }}
    - name: AZDEVOPS_GIT_CREDENTIAL_ENABLED
      value: "true"
    {{- end }}
    # The Gateway owns the model list: init replaces the saved one.
    - name: CETIRUN_AI_GATEWAY
      value: "true"
    {{- if .Values.sandboxWorkload }}
    - name: CETIRUN_NATIVE_SANDBOX
      value: "true"
    {{- end }}
  volumeMounts:
    {{- include "cetirun.context.initMounts" . | nindent 4 }}
    - name: data
      mountPath: /home-data
    - name: tmp
      mountPath: /tmp
    - name: config
      mountPath: /etc/cetirun
      readOnly: true
{{- if and .Values.openclaw.doctor.enabled .Values.doctor.enabled }}
- name: doctor
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
  {{- if .Values.sandboxWorkload }}
  command: ["/usr/bin/tini", "-g", "--", "openclaw", "doctor", "--fix", "--non-interactive", "--yes"]
  {{- else }}
  command: ["openclaw", "doctor", "--fix", "--non-interactive", "--yes"]
  {{- end }}
  env:
    - name: OPENCLAW_STATE_DIR
      value: "/home/vibe/.openclaw"
    - name: OPENCLAW_CONFIG_PATH
      value: "/home/vibe/.openclaw/openclaw.json"
    {{- if .Values.sandboxWorkload }}
    - name: NODE_DISABLE_COMPILE_CACHE
      value: "1"
    {{- end }}
  volumeMounts:
    - name: data
      mountPath: /home/vibe
    - name: tmp
      mountPath: /tmp
    {{- if include "cetirun.memory.enabled" . }}
    {{- include "cetirun.memory.mounts" . | nindent 4 }}
    {{- include "cetirun.context.mount" . | nindent 4 }}
    {{- end }}
{{- end }}
{{ include "cetirun.initSkillsContainer" . }}
{{- end }}

{{/*
OpenClaw container args
*/}}
{{- define "cetirun.agent.openclaw.args" -}}
args:
  - --bind
  {{- if (.Values.sandboxWorkload | default dict).ingressMTLS }}
  - loopback
  {{- else }}
  - lan
  {{- end }}
  - --port
  - {{ .Values.openclaw.config.gateway.port | quote }}
{{- end }}

{{/*
OpenClaw container ports (agent-specific only)
*/}}
{{- define "cetirun.agent.openclaw.ports" -}}
- name: gateway
  containerPort: {{ .Values.service.targetPort }}
  protocol: TCP
{{- end }}

{{/*
OpenClaw container env vars (agent-specific only)
*/}}
{{- define "cetirun.agent.openclaw.envVars" -}}
- name: OPENCLAW_STATE_DIR
  value: "/home/vibe/.openclaw"
{{- if .Values.skills.shared }}
- name: CHOKIDAR_USEPOLLING
  value: "true"
- name: CHOKIDAR_INTERVAL
  value: "5000"
{{- end }}
{{- if .Values.sandboxWorkload }}
- name: NODE_DISABLE_COMPILE_CACHE
  value: "1"
{{- end }}
- name: BRAVE_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun.secretName" . }}
      key: BRAVE_API_KEY
      optional: true
- name: PERPLEXITY_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun.secretName" . }}
      key: PERPLEXITY_API_KEY
      optional: true
- name: OPENAI_API_KEY
  valueFrom:
    secretKeyRef:
      # This Agent's AI Gateway virtual key, managed by the CetiRun operator.
      name: {{ .Values.aiGateway.keySecretName }}
      key: {{ .Values.aiGateway.keySecretKey | default "api-key" }}
# Gateway token (required). With Portal sign-in, OpenClaw refuses a token and
# the same secret becomes the password of clients on loopback.
{{- if eq (include "cetirun.agent.openclaw.portalSignIn" .) "true" }}
- name: OPENCLAW_GATEWAY_PASSWORD
{{- else }}
- name: OPENCLAW_GATEWAY_TOKEN
{{- end }}
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun.secretName" . }}
      key: OPENCLAW_GATEWAY_TOKEN
# Config file path
- name: OPENCLAW_CONFIG_PATH
  value: "/home/vibe/.openclaw/openclaw.json"
{{- end }}

{{/*
OpenClaw liveness probe path and port
*/}}
{{- define "cetirun.agent.openclaw.probePathPort" -}}
path: /healthz
port: gateway
{{- end }}

{{/*
OpenClaw readiness probe path and port
*/}}
{{- define "cetirun.agent.openclaw.readinessProbePathPort" -}}
path: /readyz
port: gateway
{{- end }}

{{/*
OpenClaw startup probe path and port
*/}}
{{- define "cetirun.agent.openclaw.startupProbePathPort" -}}
path: /startupz
port: gateway
{{- end }}

{{/*
OpenClaw Tailscale serve port value
*/}}
{{- define "cetirun.agent.openclaw.tailscaleServePort" -}}
{{ .Values.openclaw.config.gateway.port | quote }}
{{- end }}

{{/*
OpenClaw logger volumeMount
*/}}
{{- define "cetirun.agent.openclaw.loggerVolumeMount" -}}
- name: data
  mountPath: /home/vibe/.openclaw
  subPath: .openclaw
  readOnly: true
{{- end }}

{{/*
OpenClaw service ports (agent-specific only)
*/}}
{{- define "cetirun.agent.openclaw.servicePorts" -}}
- port: {{ .Values.service.port }}
  targetPort: {{ .Values.service.targetPort }}
  protocol: TCP
  name: gateway
  {{- if eq .Values.service.type "NodePort" }}
  {{- with .Values.service.nodePort }}
  nodePort: {{ . }}
  {{- end }}
  {{- end }}
{{- end }}

{{/*
OpenClaw networkpolicy ingress ports (agent-specific only)
*/}}
{{- define "cetirun.agent.openclaw.networkPolicyIngressPorts" -}}
- protocol: TCP
  port: {{ .Values.service.targetPort }}
{{- end }}

{{/*
OpenClaw configmap data
*/}}
{{- define "cetirun.agent.openclaw.configmapData" }}
  openclaw.json: |
    {{- if .Values.runtime }}
{{ index .Values.runtime.configFiles "openclaw.json" | indent 4 }}
    {{- else }}
    {{- $context := dict "Values" .Values "Chart" .Chart "SkillDirs" (include "cetirun.npmSkillDirs" . | fromJsonArray) "TailscaleEnabled" (include "cetirun.tailscaleEnabled" .) }}
{{ tpl (.Files.Get "agents/openclaw/config/native.tpl") $context | trim | indent 4 }}
    {{- end }}
  user-md: |
    # USER.md - About Your Human

    - **Name:** {{ .Values.user.displayName | default "" }}
    - **Alias:** {{ .Values.user.alias | default "" }}
    - **What to call them:** {{ .Values.user.displayName | default "" }}
    - **Pronouns:**
    - **Timezone:**
    {{- if .Values.user.role }}
    - **Role:** {{ .Values.user.role }}
    {{- end }}
    {{- if .Values.user.team }}
    - **Team:** {{ .Values.user.team }}
    {{- end }}
    {{- if .Values.user.description }}
    - **Notes:**
    {{- range .Values.user.description }}
      - {{ . }}
    {{- end }}
    {{- end }}

    ## Context
    {{- if and .Values.user.alias .Values.user.baseDomain }}

    - Has access to this instance at https://{{ .Values.user.alias }}.{{ .Values.user.baseDomain }}
    {{- end }}

    ---

    _Update this as you learn more about them._
  init-home.sh: |
{{ .Files.Get "agents/openclaw/bootstrap/init-home.sh" | indent 4 }}
  {{- if .Values.sandboxWorkload }}
  health-probe.cjs: |
{{ .Files.Get "agents/openclaw/bootstrap/health-probe.cjs" | indent 4 }}
  {{- end }}
  init-skills.sh: |
{{ .Files.Get "configs/init-skills.sh" | indent 4 }}
  session-logger.sh: |
{{ .Files.Get "configs/session-logger.sh" | indent 4 }}
{{- end }}

{{/*
OpenClaw secret data (agent-specific only)
*/}}
{{- define "cetirun.agent.openclaw.secretData" -}}
{{- end }}

{{/*
Whether OpenClaw signs the owner in from their Portal sign-in: asked for, and
reached through the Istio sidecar or the managed in-Pod mTLS endpoint.
Plain Sandbox ingress and Tailscale Serve keep the gateway token.
*/}}
{{- define "cetirun.agent.openclaw.portalSignIn" -}}
{{- if and (dig "portalSignIn" "enabled" false .Values.openclaw) (or (not .Values.sandboxWorkload) (dig "ingressMTLS" false (.Values.sandboxWorkload | default dict))) (ne (include "cetirun.tailscaleEnabled" .) "true") -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}
