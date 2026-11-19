{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}

{{- define "cetirun-connections-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "cetirun-connections-mcp.fullname" -}}
{{- if .Values.fullnameOverride }}{{ .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}{{ else }}{{ include "cetirun-connections-mcp.name" . }}{{ end }}
{{- end }}
{{- define "cetirun-connections-mcp.labels" -}}
app.kubernetes.io/name: {{ include "cetirun-connections-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "cetirun-connections-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cetirun-connections-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "cetirun-connections-mcp.serviceAccountName" -}}
{{- default (include "cetirun-connections-mcp.fullname" .) .Values.serviceAccount.name }}
{{- end }}
{{- define "cetirun-connections-mcp.image" -}}
{{- $repository := .repository -}}
{{- if (.global | default dict).registry -}}
{{- $repository = printf "%s/cetirun-connections-mcp" (trimSuffix "/" .global.registry) -}}
{{- end -}}
{{- $tag := default .defaultTag (default ((.global | default dict).version) .tag) -}}
{{- if .digest }}{{ printf "%s:%s@%s" $repository $tag .digest }}{{ else }}{{ printf "%s:%s" $repository $tag }}{{ end }}
{{- end }}
