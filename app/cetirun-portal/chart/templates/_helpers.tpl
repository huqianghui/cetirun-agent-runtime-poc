{{/*
Expand the name of the chart.
*/}}
{{- define "cetirun-portal.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "cetirun-portal.fullname" -}}
{{- $name := default .Chart.Name .Values.fullnameOverride }}
{{- printf "%s" $name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Shared registry and version defaults.
*/}}
{{- define "cetirun-portal.registry" -}}
{{- .Values.global.registry | trimSuffix "/" -}}
{{- end }}

{{- define "cetirun-portal.version" -}}
{{- .Values.global.version -}}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "cetirun-portal.labels" -}}
app.kubernetes.io/name: {{ include "cetirun-portal.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "cetirun-portal.version" . | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "cetirun-portal.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cetirun-portal.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Container image reference with optional digest pinning.
When image.digest is set, produces: repository:tag@digest
Otherwise: repository:tag
*/}}
{{- define "cetirun-portal.image" -}}
{{- $registry := include "cetirun-portal.registry" . -}}
{{- $repo := .Values.image.repository -}}
{{- if $registry }}{{- $repo = printf "%s/cetirun-portal" $registry -}}{{- end -}}
{{- $tag := include "cetirun-portal.version" . | default .Values.image.tag | default .Chart.AppVersion -}}
{{ $repo }}:{{ $tag }}{{ if .Values.image.digest }}@{{ .Values.image.digest }}{{ end }}
{{- end }}
