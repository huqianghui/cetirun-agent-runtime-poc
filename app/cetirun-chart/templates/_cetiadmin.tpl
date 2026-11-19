{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}
{{- define "cetirun.cetiadmin.validate" -}}
{{- $admin := .Values.cetiadmin | default dict -}}
{{- if $admin.enabled -}}
{{- $runtime := .Values.runtime | default dict -}}
{{- if or (ne .Values.agentType "hermes") .Values.sandboxWorkload (not $runtime) (not $runtime.agent) $runtime.bindings -}}
{{- fail "CetiAdmin requires a prepared ordinary Hermes runtime, never a protected Sandbox" -}}
{{- end -}}
{{- if or (ne $runtime.contractVersion "v1") (ne $runtime.agent.type "hermes") (not $runtime.agent.image) (ne $runtime.agent.username "vibe") (ne $runtime.agent.home "/home/vibe") -}}
{{- fail "CetiAdmin requires the prepared native Hermes runtime contract" -}}
{{- end -}}
{{- if or (ne .Release.Name (printf "openclaw-%s" $admin.agentName)) (ne .Release.Namespace (printf "cetirun-%s" $admin.agentName)) -}}
{{- fail "CetiAdmin release and namespace must match the bound Agent name" -}}
{{- end -}}
{{- $sa := .Values.serviceAccount -}}
{{- if or $sa.create (ne $sa.name $admin.serviceAccountName) $sa.role (not (kindIs "bool" $sa.automountToken)) $sa.automountToken -}}
{{- fail "CetiAdmin must reference its prepared ServiceAccount with create=false, role empty and automountToken=false" -}}
{{- end -}}
{{- if or (ne (int .Values.podSecurityContext.fsGroup) 1024) (ne (int .Values.securityContext.runAsUser) 1024) (and (hasKey .Values.securityContext "runAsGroup") (ne (int .Values.securityContext.runAsGroup) 1024)) -}}
{{- fail "CetiAdmin requires the native UID/GID and volume group 1024" -}}
{{- end -}}
{{- $config := $admin.runtimeConfig | fromJson -}}
{{- $expected := dict "instance" (dict "name" $admin.agentName "namespace" .Release.Namespace "uid" $admin.agentUID) "monitor" $admin.monitor -}}
{{- if ne (toJson $config) (toJson $expected) -}}
{{- fail "CetiAdmin runtimeConfig must contain exactly the bound instance and monitor" -}}
{{- end -}}
{{- $kubeconfig := $admin.kubeconfig | fromJson -}}
{{- $expectedKubeconfig := dict "apiVersion" "v1" "kind" "Config" "current-context" "cetiadmin"
      "clusters" (list (dict "name" "cetiadmin" "cluster" (dict "server" $admin.kubernetes.apiServer "certificate-authority" "/var/run/cetirun/kubernetes/ca.crt")))
      "contexts" (list (dict "name" "cetiadmin" "context" (dict "cluster" "cetiadmin" "user" "cetiadmin" "namespace" .Release.Namespace)))
      "users" (list (dict "name" "cetiadmin" "user" (dict "tokenFile" "/var/run/cetirun/kubernetes/token"))) -}}
{{- if ne (toJson $kubeconfig) (toJson $expectedKubeconfig) -}}
{{- fail "CetiAdmin kubeconfig must use only its fixed context, tokenFile and cluster CA" -}}
{{- end -}}
{{- $native := (index $runtime.configFiles "hermes-config.yaml" | default "") | fromYaml -}}
{{- $dirs := ($native.skills | default dict).external_dirs | default list -}}
{{- if or (not (kindIs "slice" $dirs)) (not (has "/opt/cetirun/cetiadmin/skills" $dirs)) -}}
{{- fail "CetiAdmin requires its assets directory in prepared Hermes skills.external_dirs" -}}
{{- end -}}
{{- if eq $admin.kubernetes.apiAudience .Values.connectionsMCP.audience -}}
{{- fail "CetiAdmin Kubernetes and Connections MCP token audiences must differ" -}}
{{- end -}}
{{- if and .Values.azureWorkloadIdentity.enabled (or (not $admin.azureWorkloadIdentity.enabled) (ne .Values.azureWorkloadIdentity.clientId $admin.azureWorkloadIdentity.clientId)) -}}
{{- fail "CetiAdmin Azure identity must match its prepared binding" -}}
{{- end -}}

{{- $reservedEnv := list "KUBECONFIG" "HERMES_HOME" "HERMES_PYTHON" "HERMES_INSTALL_DIR" "HERMES_TIMEZONE" "SLACK_BOT_TOKEN" "SLACK_APP_TOKEN" "AZURE_CLIENT_ID" "AZURE_TENANT_ID" "AZURE_AUTHORITY_HOST" "AZURE_FEDERATED_TOKEN_FILE" -}}
{{- range .Values.extraEnv -}}
{{- if or (has .name $reservedEnv) (hasPrefix "CETIRUN_CETIADMIN_" .name) (hasPrefix "CETIADMIN_" .name) -}}
{{- fail "CetiAdmin environment is supplied by the prepared binding" -}}
{{- end -}}
{{- end -}}
{{- $reservedVolumes := list "cetiadmin-assets" "cetiadmin-kubernetes" "connections-identity" -}}
{{- range .Values.extraVolumes -}}
{{- if has .name $reservedVolumes -}}{{- fail "CetiAdmin volume names are reserved" -}}{{- end -}}
{{- range (.projected | default dict).sources -}}
{{- if .serviceAccountToken -}}{{- fail "CetiAdmin token projections cannot be added through extraVolumes" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- range .Values.extraVolumeMounts -}}
{{- $mount := clean .mountPath -}}
{{- if eq $mount "/" -}}{{- fail "CetiAdmin mount paths are reserved" -}}{{- end -}}
{{- range list "/opt/cetirun/cetiadmin" "/var/run/cetirun/kubernetes" "/etc/cetirun" "/var/run/secrets/azure/tokens" -}}
{{- if or (eq $mount .) (hasPrefix (printf "%s/" .) $mount) (hasPrefix (printf "%s/" $mount) .) -}}{{- fail "CetiAdmin mount paths are reserved" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- range concat (.Values.initContainers | default list) (.Values.sidecars | default list) -}}
{{- if has .name (list "cetiadmin-assets" $.Chart.Name) -}}{{- fail "CetiAdmin container names are reserved" -}}{{- end -}}
{{- range .volumeMounts -}}
{{- if has .name $reservedVolumes -}}{{- fail "CetiAdmin identity and assets cannot be mounted by additional containers" -}}{{- end -}}
{{- end -}}
{{- range .env -}}
{{- if or (has .name $reservedEnv) (hasPrefix "CETIRUN_CETIADMIN_" .name) (hasPrefix "CETIADMIN_" .name) -}}{{- fail "CetiAdmin identity environment is main-container only" -}}{{- end -}}
{{- end -}}
{{- range .envFrom -}}
{{- if and $admin.monitor.slackSecretName (eq ((.secretRef | default dict).name | default "") $admin.monitor.slackSecretName) -}}{{- fail "CetiAdmin Slack credentials are main-container only" -}}{{- end -}}
{{- end -}}
{{- end -}}

{{- if or (not .Values.networkPolicy.enabled) (not .Values.sidecarScope.enabled) -}}
{{- fail "CetiAdmin requires NetworkPolicy and SidecarScope" -}}
{{- end -}}
{{- $authority := $admin.kubernetes.apiServer | trimPrefix "https://" | trimSuffix "/" -}}
{{- $port := regexFind ":[0-9]+$" $authority | trimPrefix ":" | default "443" | int -}}
{{- if or (lt $port 1) (gt $port 65535) -}}{{- fail "CetiAdmin requires a valid API server port" -}}{{- end -}}
{{- $host := regexReplaceAll ":[0-9]+$" $authority "" | trimPrefix "[" | trimSuffix "]" -}}
{{- if has $host (list "kubernetes.default" "kubernetes.default.svc") -}}{{- $host = "kubernetes.default.svc.cluster.local" -}}{{- end -}}
{{- $hasHost := false -}}
{{- range .Values.sidecarScope.additionalEgressHosts -}}
{{- $parts := splitList "/" . -}}
{{- if or (ne (len $parts) 2) (not (first $parts)) (contains "*" (last $parts)) -}}{{- fail "CetiAdmin requires exact additional mesh hosts" -}}{{- end -}}
{{- if eq (last $parts) $host -}}{{- $hasHost = true -}}{{- end -}}
{{- end -}}
{{- if not $hasHost -}}{{- fail "CetiAdmin API server must be present in sidecarScope.additionalEgressHosts" -}}{{- end -}}
{{- $allowed := dict -}}
{{- range .Values.networkPolicy.allowedInternalCIDRs -}}
{{- if not (regexMatch "(/32|/128)$" .cidr) -}}{{- fail "CetiAdmin internal egress rules must use exact host CIDRs" -}}{{- end -}}
{{- if has .cidr $admin.kubernetes.apiEgressCIDRs -}}
{{- if ne (len .ports) 1 -}}{{- fail "CetiAdmin API egress must allow only the configured API port" -}}{{- end -}}
{{- $rule := first .ports -}}
{{- if or (ne ($rule.protocol | default "TCP") "TCP") (ne (int $rule.port) $port) -}}{{- fail "CetiAdmin API egress must allow only the configured TCP API port" -}}{{- end -}}
{{- $_ := set $allowed .cidr true -}}
{{- end -}}
{{- end -}}
{{- range $admin.kubernetes.apiEgressCIDRs -}}
{{- if not (hasKey $allowed .) -}}{{- fail "CetiAdmin API addresses require explicit networkPolicy.allowedInternalCIDRs rules" -}}{{- end -}}
{{- if and (hasSuffix "/32" .) (not (has . $.Values.networkPolicy.blockedCIDRs)) -}}
{{- fail "CetiAdmin API addresses must also be excluded from unrestricted external egress" -}}
{{- end -}}
{{- end -}}
{{- range list "10.0.0.0/8" "172.16.0.0/12" "192.168.0.0/16" "169.254.169.254/32" -}}
{{- if not (has . $.Values.networkPolicy.blockedCIDRs) -}}{{- fail "CetiAdmin must retain private-network and IMDS egress exclusions" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "cetirun.cetiadmin.init" -}}
{{- if .Values.cetiadmin.enabled }}
- name: cetiadmin-assets
  image: {{ .Values.cetiadmin.operatorImage | quote }}
  imagePullPolicy: IfNotPresent
  command: ["/manager"]
  args: ["--mode=cetiadmin-export", "--cetiadmin-output-dir=/cetiadmin-assets"]
  securityContext:
    runAsUser: 1024
    runAsGroup: 1024
    runAsNonRoot: true
    readOnlyRootFilesystem: true
    allowPrivilegeEscalation: false
    capabilities:
      drop: ["ALL"]
    seccompProfile:
      type: RuntimeDefault
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
      ephemeral-storage: 128Mi
    limits:
      cpu: 100m
      memory: 128Mi
  volumeMounts:
    - name: cetiadmin-assets
      mountPath: /cetiadmin-assets
{{- end }}
{{- end -}}

{{- define "cetirun.cetiadmin.env" -}}
{{- if .Values.cetiadmin.enabled }}
- name: CETIRUN_CETIADMIN_ENABLED
  value: "true"
- name: CETIRUN_CETIADMIN_CONFIG
  value: /etc/cetirun/cetiadmin-runtime.json
- name: CETIADMIN_AGENT_UID
  value: {{ .Values.cetiadmin.agentUID | quote }}
- name: KUBECONFIG
  value: /var/run/cetirun/kubernetes/config
- name: HERMES_INSTALL_DIR
  value: /usr/local/lib/hermes-agent
{{- with .Values.cetiadmin.monitor }}
{{- if and .enabled (eq .deliver "slack") }}
{{- range list "SLACK_BOT_TOKEN" "SLACK_APP_TOKEN" }}
- name: {{ . }}
  valueFrom:
    secretKeyRef:
      name: {{ $.Values.cetiadmin.monitor.slackSecretName | quote }}
      key: {{ . }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "cetirun.cetiadmin.mounts" -}}
{{- if .Values.cetiadmin.enabled }}
- name: cetiadmin-assets
  mountPath: /opt/cetirun/cetiadmin
  readOnly: true
- name: cetiadmin-kubernetes
  mountPath: /var/run/cetirun/kubernetes
  readOnly: true
{{- end }}
{{- end -}}

{{- define "cetirun.cetiadmin.volumes" -}}
{{- if .Values.cetiadmin.enabled }}
- name: cetiadmin-assets
  emptyDir:
    sizeLimit: 128Mi
- name: cetiadmin-kubernetes
  projected:
    defaultMode: 0440
    sources:
      - serviceAccountToken:
          path: token
          audience: {{ .Values.cetiadmin.kubernetes.apiAudience | quote }}
          expirationSeconds: 600
      - configMap:
          name: {{ .Values.cetiadmin.kubernetes.caConfigMapName | quote }}
          items:
            - key: ca.crt
              path: ca.crt
      - configMap:
          name: {{ include "cetirun.configMapName" . }}
          items:
            - key: cetiadmin-kubeconfig.json
              path: config
{{- end }}
{{- end -}}

{{- define "cetirun.cetiadmin.azureSkipContainers" -}}
{{- $names := list "cetiadmin-assets" "istio-init" "istio-validation" "istio-proxy" -}}
{{- range concat (include "cetirun.memory.init" . | fromYamlArray) (include "cetirun.agent.initContainers" . | fromYamlArray) (.Values.initContainers | default list) (.Values.sidecars | default list) -}}
{{- $names = append $names .name -}}
{{- end -}}
{{- if .Values.desktop.enabled -}}{{- $names = append $names "desktop" -}}{{- end -}}
{{- if .Values.logger.enabled -}}{{- $names = append $names "logger" -}}{{- end -}}
{{- $existing := index (.Values.podAnnotations | default dict) "azure.workload.identity/skip-containers" | default "" -}}
{{- $names = concat $names (splitList ";" $existing) | compact | uniq | sortAlpha -}}
{{- if has .Chart.Name $names -}}{{- fail "CetiAdmin Azure identity must not skip the main container" -}}{{- end -}}
{{- join ";" $names -}}
{{- end -}}
