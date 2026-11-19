{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}

{{- define "cetirun-observability.collectorName" -}}cetirun-adx-mon-collector{{- end }}
{{- define "cetirun-observability.ingestorName" -}}cetirun-adx-mon-ingestor{{- end }}
{{- define "cetirun-observability.kubeStateMetricsName" -}}cetirun-kube-state-metrics{{- end }}
{{- define "cetirun-observability.telemetryName" -}}cetirun-telemetry{{- end }}

{{- define "cetirun-observability.labels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
app.kubernetes.io/version: {{ .context.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .context.Release.Service }}
app.kubernetes.io/part-of: cetirun-observability
{{- end }}

{{- define "cetirun-observability.selectorLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
{{- end }}

{{/*
Image of a CetiRun component: global.registry replaces the repository's
registry, and global.version is the tag when the image sets none.
*/}}
{{- define "cetirun-observability.image" -}}
{{- $global := .context.Values.global | default dict -}}
{{- $repository := .image.repository -}}
{{- if $global.registry -}}
{{- $repository = printf "%s/%s" (trimSuffix "/" $global.registry) .name -}}
{{- end -}}
{{- $tag := .image.tag | default $global.version | default .context.Chart.AppVersion -}}
{{- if .image.digest }}{{ printf "%s:%s@%s" $repository $tag .image.digest }}{{ else }}{{ printf "%s:%s" $repository $tag }}{{ end }}
{{- end }}

{{- define "cetirun-observability.ingestorEndpoint" -}}
https://{{ include "cetirun-observability.ingestorName" . }}.{{ .Release.Namespace }}.svc:9090
{{- end }}

{{- define "cetirun-observability.ingestorTLSSecret" -}}
{{- .Values.ingestor.tls.secretName | default (printf "%s-tls" (include "cetirun-observability.ingestorName" .)) -}}
{{- end }}

{{/*
The settings every template needs, checked once. Fails with a message that
names the value to set.
*/}}
{{- define "cetirun-observability.validate" -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$" (.Values.clusterName | toString)) }}
{{- fail "clusterName must name this CetiRun cluster: lowercase letters, digits and hyphens, at most 63 characters" }}
{{- end }}
{{- if not (regexMatch "^https://[^/?#]+$" (.Values.adx.endpoint | toString)) }}
{{- fail "adx.endpoint must be the query URI of the Kusto cluster, such as https://<name>.<region>.kusto.windows.net" }}
{{- end }}
{{- range $role, $name := .Values.adx.databases }}
{{- if not (regexMatch "^[A-Za-z][A-Za-z0-9]{0,99}$" ($name | toString)) }}
{{- fail (printf "adx.databases.%s must be a database name of letters and digits" $role) }}
{{- end }}
{{- end }}
{{- if not (regexMatch "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$" (.Values.ingestor.clientId | toString)) }}
{{- fail "ingestor.clientId must be the client ID of the managed identity that the ingestor uses through workload identity" }}
{{- end }}
{{- if not (has (.Values.telemetry.events | toString) (list "all" "warnings-and-cetirun" "none")) }}
{{- fail "telemetry.events must be all, warnings-and-cetirun or none" }}
{{- end }}
{{- if lt (int .Values.metrics.scrapeInterval) 10 }}
{{- fail "metrics.scrapeInterval must be at least 10 seconds" }}
{{- end }}
{{- end }}

{{/*
A TOML string: JSON string escapes are valid TOML basic string escapes.
*/}}
{{- define "cetirun-observability.toml" -}}{{ . | toString | toJson }}{{- end }}

{{/*
The log routes, in order: the first route that matches a Pod decides where all
its containers' logs go. Agent routes come first and match by Pod label in any
namespace, so an Agent's logs always reach the Agent logs database. Platform
routes match the release namespace and the configured platform namespaces.
Pods that match no route are not collected; kube-system is not collected.
*/}}
{{- define "cetirun-observability.logRoutes" -}}
{{- $logs := .Values.adx.databases.logs -}}
{{- $agents := .Values.adx.databases.agentLogs -}}
{{- $routes := list -}}
{{- if .Values.logs.agents.enabled }}
{{- $routes = append $routes (dict "labels" (dict "app.kubernetes.io/name" "cetirun") "destination" (printf "%s:AgentLogs" $agents)) -}}
{{- $routes = append $routes (dict "labels" (dict "app.kubernetes.io/name" "cetirun-terminal") "destination" (printf "%s:AgentLogs" $agents)) -}}
{{- $routes = append $routes (dict "labels" (dict "app.kubernetes.io/name" "cetirun-copilot-proxy") "destination" (printf "%s:CopilotProxy" $agents)) -}}
{{- end }}
{{- $routes = append $routes (dict "namespace" .Release.Namespace "labels" (dict "app.kubernetes.io/name" "cetirun-ai-gateway" "app.kubernetes.io/component" "gateway") "destination" (printf "%s:Gateway" $logs)) -}}
{{- $routes = append $routes (dict "namespace" .Release.Namespace "labels" (dict "app.kubernetes.io/name" (include "cetirun-observability.telemetryName" .)) "destination" (printf "%s:Telemetry" $logs)) -}}
{{- with .Values.logs.ingressNamespace }}
{{- $routes = append $routes (dict "namespace" . "destination" (printf "%s:Ingress" $logs)) -}}
{{- end }}
{{- range (prepend .Values.logs.platformNamespaces .Release.Namespace | uniq) }}
{{- $routes = append $routes (dict "namespace" . "destination" (printf "%s:Platform" $logs)) -}}
{{- end }}
{{- toJson $routes -}}
{{- end }}

{{/*
The collector's configuration. Its listener binds the Pod's loopback address:
it serves only the collector's own metrics, which it scrapes itself, and no
write endpoint. The kubelet is scraped at the node address with the
collector's service account, which may read node metrics only.
*/}}
{{- define "cetirun-observability.collectorConfig" -}}
{{- $toml := "cetirun-observability.toml" -}}
# Rendered by the cetirun-observability chart.
endpoint = {{ include $toml (include "cetirun-observability.ingestorEndpoint" .) }}
storage-dir = "/var/lib/cetirun-adx-mon/collector"
listen-addr = "127.0.0.1:8080"
max-batch-size = 10000
max-segment-age-seconds = 30
max-disk-usage = {{ int64 .Values.collector.maxDiskUsage }}
lift-labels = [
  { name = "cluster", column = "Cluster" },
  { name = "adxmon_namespace", column = "Namespace" },
  { name = "adxmon_pod", column = "Pod" },
  { name = "adxmon_container", column = "Container" },
]
lift-resources = [
  { name = "cluster", column = "Cluster" },
  { name = "namespace", column = "Namespace" },
  { name = "pod", column = "Pod" },
  { name = "container", column = "Container" },
]

[add-labels]
cluster = {{ include $toml .Values.clusterName }}

[prometheus-scrape]
database = {{ include $toml .Values.adx.databases.metrics }}
scrape-interval = {{ int .Values.metrics.scrapeInterval }}
scrape-timeout = 10
default-drop-metrics = true
keep-metrics = [
{{- range .Values.metrics.keep }}
  {{ include $toml . }},
{{- end }}
]
drop-metrics = [
{{- range .Values.metrics.drop }}
  {{ include $toml . }},
{{- end }}
]
static-scrape-target = [
  { host-regex = ".*", url = "http://127.0.0.1:8080/metrics", namespace = {{ include $toml .Release.Namespace }}, pod = {{ include $toml (include "cetirun-observability.collectorName" .) }}, container = "collector" },
  { host-regex = ".*", url = "https://$(HOSTIP):10250/metrics/cadvisor", namespace = "kube-system", pod = "kubelet", container = "cadvisor" },
  { host-regex = ".*", url = "https://$(HOSTIP):10250/metrics/resource", namespace = "kube-system", pod = "kubelet", container = "resource" },
  { host-regex = ".*", url = "https://$(HOSTIP):10250/metrics", namespace = "kube-system", pod = "kubelet", container = "kubelet" },
]

[[host-log]]
{{- range (include "cetirun-observability.logRoutes" . | fromJsonArray) }}

[[host-log.static-pod-target]]
{{- with .namespace }}
namespace = {{ include $toml . }}
{{- end }}
destination = {{ include $toml .destination }}
parsers = ["json"]
{{- with .labels }}
[host-log.static-pod-target.label-targets]
{{- range $key, $value := . }}
{{ include $toml $key }} = {{ include $toml $value }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
