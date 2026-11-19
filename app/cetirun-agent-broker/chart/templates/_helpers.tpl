{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}

{{- define "cetirun-agent-broker.name" -}}cetirun-agent-broker{{- end }}
{{- define "cetirun-agent-broker.fullname" -}}
{{- default (include "cetirun-agent-broker.name" .) .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}
{{- define "cetirun-agent-broker.labels" -}}
app.kubernetes.io/name: {{ include "cetirun-agent-broker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "cetirun-agent-broker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cetirun-agent-broker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "cetirun-agent-broker.serviceAccountName" -}}
{{- default (include "cetirun-agent-broker.fullname" .) .Values.serviceAccount.name -}}
{{- end }}
{{/*
Container image reference with optional digest pinning.
When digest is set, produces: repository:tag@digest.
*/}}
{{- define "cetirun-agent-broker.image" -}}
{{- $repository := required "image repository is required" .repository -}}
{{- $tag := default .defaultTag .tag -}}
{{- $image := printf "%s:%s" $repository $tag -}}
{{- if .digest -}}{{ printf "%s@%s" $image .digest }}{{- else -}}{{ $image }}{{- end -}}
{{- end }}
