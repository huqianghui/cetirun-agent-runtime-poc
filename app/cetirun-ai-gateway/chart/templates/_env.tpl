{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}

{{/*
Database connection only: all that schema migration needs, so it is the only
credential the migrate init container gets.
*/}}
{{- define "cetirun-ai-gateway.databaseEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun-ai-gateway.databaseUrlSecret" . }}
      key: {{ include "cetirun-ai-gateway.databaseUrlKey" . }}
{{- end }}

{{/*
Everything the proxy needs, including credentials held in chart-managed Secrets.
*/}}
{{- define "cetirun-ai-gateway.commonEnv" -}}
{{ include "cetirun-ai-gateway.databaseEnv" . }}
- name: LITELLM_MASTER_KEY
  valueFrom:
    secretKeyRef:
      {{- if .Values.auth.masterKey.existingSecret }}
      name: {{ .Values.auth.masterKey.existingSecret }}
      key: {{ .Values.auth.masterKey.key }}
      {{- else }}
      name: {{ include "cetirun-ai-gateway.secretName" . }}
      key: master-key
      {{- end }}
- name: LITELLM_SALT_KEY
  valueFrom:
    secretKeyRef:
      {{- if .Values.auth.saltKey.existingSecret }}
      name: {{ .Values.auth.saltKey.existingSecret }}
      key: {{ .Values.auth.saltKey.key }}
      {{- else }}
      name: {{ include "cetirun-ai-gateway.secretName" . }}
      key: salt-key
      {{- end }}
{{- end }}

{{- define "cetirun-ai-gateway.proxyEnv" -}}
{{ include "cetirun-ai-gateway.commonEnv" . }}
- name: GRACEFUL_SHUTDOWN_TIMEOUT
  value: {{ mulf .Values.terminationGracePeriodSeconds 0.9 | quote }}
- name: STORE_MODEL_IN_DB
  value: "True"
{{- if .Values.migrations.enabled }}
{{- /* The migrate init container owns schema changes, so proxies must not
       migrate on startup as well. */}}
- name: DISABLE_SCHEMA_UPDATE
  value: "True"
{{- end }}
{{- if eq (include "cetirun-ai-gateway.priceTable" .) "bundled" }}
{{ include "cetirun-ai-gateway.priceTableEnv" . }}
{{- end }}
{{- /* Set either way: LiteLLM reserves tokens unless told not to. See tpmReservation
       in values.yaml. */}}
- name: LITELLM_TPM_TOKEN_RESERVATION_ENABLED
  value: {{ ternary "true" "false" (eq (toString .Values.tpmReservation) "true") | quote }}
{{- if .Values.auth.adminUI.enabled }}
- name: UI_USERNAME
  value: {{ .Values.auth.adminUI.username | quote }}
- name: UI_PASSWORD
  valueFrom:
    secretKeyRef:
      {{- if .Values.auth.adminUI.passwordSecret.existingSecret }}
      name: {{ .Values.auth.adminUI.passwordSecret.existingSecret }}
      key: {{ .Values.auth.adminUI.passwordSecret.key }}
      {{- else }}
      name: {{ include "cetirun-ai-gateway.secretName" . }}
      key: ui-password
      {{- end }}
{{- with .Values.auth.adminUI.rootPath }}
{{- /* LiteLLM rewrites the console's asset paths for the prefix, which needs
       a writable copy of the packaged UI. Requests without the prefix, such
       as the Agents' and the probes', are routed as before. */}}
- name: SERVER_ROOT_PATH
  value: {{ . | quote }}
- name: LITELLM_UI_PATH
  value: /tmp/litellm-ui
{{- end }}
{{- else }}
- name: DISABLE_ADMIN_UI
  value: "True"
{{- end }}
{{- if include "cetirun-ai-gateway.redisPasswordSecret" . }}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "cetirun-ai-gateway.redisPasswordSecret" . }}
      key: {{ include "cetirun-ai-gateway.redisPasswordKey" . }}
{{- end }}
{{- $router := include "cetirun-ai-gateway.router" . | fromJson }}
{{- if and $router.healthChecks.enabled (include "cetirun-ai-gateway.redisEnabled" .) }}
{{- /* The replicas share probe results through Redis (use_shared_health_check).
       LiteLLM reuses a shared result for this many seconds, 300 unless set,
       whatever the probe interval; set to the interval, a result is never
       reused past the next probe. */}}
- name: DEFAULT_SHARED_HEALTH_CHECK_TTL
  value: {{ int $router.healthChecks.intervalSeconds | quote }}
{{- /* The lock that makes one replica probe for all expires after this many
       seconds, 60 unless set. A cycle that runs longer, as one with many
       deployments and a concurrency limit can, would let another replica take
       the lock and probe everything again. */}}
- name: DEFAULT_SHARED_HEALTH_CHECK_LOCK_TTL
  value: {{ int $router.healthChecks.intervalSeconds | quote }}
{{- end }}
{{- $settings := include "cetirun-ai-gateway.logsAndMetrics" . | fromJson }}
{{- if $settings.metrics.enabled }}
{{- /* The proxy records its samples in files here, and the separate metrics
       process serves them from there. Set from the start, so every sample the
       proxy records goes to the files. */}}
- name: PROMETHEUS_MULTIPROC_DIR
  value: {{ include "cetirun-ai-gateway.metricsDirectory" . }}
{{- end }}
{{- if $settings.requestLog.enabled }}
{{- /* The image's entrypoint registers the request log callback in code,
       outside LiteLLM's configuration: a configured callback depends on how
       LiteLLM combines its configuration with settings saved in its database,
       and an image without the callback cannot start with a configuration
       that names it. An image built before the request log ignores these
       variables. */}}
- name: CETIRUN_REQUEST_LOG
  value: "true"
{{- if not $settings.spendLogs.enabled }}
{{- /* Applied by the entrypoint only together with the request log, so a
       Gateway that cannot write the request log keeps LiteLLM's spend log. */}}
- name: CETIRUN_SPEND_LOGS
  value: "false"
{{- end }}
{{- end }}
{{ include "cetirun-ai-gateway.monitoringEnv" . }}
{{- end }}

{{/*
Every container of the release that holds a credential in its environment gets
this. Container Insights otherwise copies each container's environment, values
from Secrets included, into its ContainerInventory table once a minute, on any
cluster where environment variable collection is on, as it is by default. It
only applies to that collection; nothing else reads the variable.
*/}}
{{- define "cetirun-ai-gateway.monitoringEnv" -}}
- name: AZMON_COLLECT_ENV
  value: "False"
{{- end }}

{{/*
priceTable: bundled. LiteLLM reads the variable when it is imported: it then
loads the price table in the image instead of downloading the current one, and
a reload of the price table, from the console at once or on a schedule, loads
that copy again. The proxy gets it, and so does the migrate init container,
whose index build imports LiteLLM as well.
*/}}
{{- define "cetirun-ai-gateway.priceTableEnv" -}}
- name: LITELLM_LOCAL_MODEL_COST_MAP
  value: "True"
{{- end }}
