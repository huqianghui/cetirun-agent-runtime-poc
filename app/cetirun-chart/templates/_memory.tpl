{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}
{{- define "cetirun.memory.enabled" -}}
{{- if and .Values.memoryContext.enabled (or (not .Values.sandboxWorkload) ((.Values.sandboxWorkload | default dict).reconciled | default false)) -}}true{{- end -}}
{{- end -}}

{{- define "cetirun.memory.image" -}}
{{- .Values.memoryContext.image | default (printf "%s/cetirun-memory-context:%s" (include "cetirun.registry" . | default "mcr.microsoft.com/aks/ai-runtime/cetirun") (include "cetirun.version" . | default .Chart.AppVersion)) -}}
{{- end -}}

{{- define "cetirun.memory.validate" -}}
{{- if include "cetirun.memory.enabled" . -}}
{{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9./:_-]*(@sha256:[a-f0-9]{64}|:[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?)$" (include "cetirun.memory.image" .)) -}}
{{- fail "Memory Context requires a versioned or digest-pinned image" -}}
{{- end -}}
{{- if and (ne (include "cetirun.agent.type" .) "custom") (ne .Values.memoryContext.home "/home/vibe") -}}
{{- fail "Built-in Memory Context must use the native HOME" -}}
{{- end -}}
{{- range .Values.extraEnv -}}
{{- if hasPrefix "CETIRUN_MEMORY_" .name -}}{{- fail "Memory environment is platform-owned" -}}{{- end -}}
{{- end -}}
{{- $reserved := list "/opt/cetirun-memory" (printf "%s/.cetirun/memory" .Values.memoryContext.home) -}}
{{- range .Values.extraVolumeMounts -}}
{{- $mount := clean .mountPath -}}
{{- if eq $mount "/" -}}{{- fail "Memory paths are reserved" -}}{{- end -}}
{{- range $reserved -}}
{{- if or (eq $mount .) (hasPrefix (printf "%s/" .) $mount) (hasPrefix (printf "%s/" $mount) .) -}}{{- fail "Memory paths are reserved" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- range .Values.extraVolumes -}}
{{- if eq .name "memory-assets" -}}{{- fail "Memory volume names are reserved" -}}{{- end -}}
{{- end -}}
{{- range concat (.Values.initContainers | default list) (.Values.sidecars | default list) -}}
{{- range .volumeMounts -}}
{{- if eq .name "memory-assets" -}}{{- fail "Memory volumes cannot be mounted by additional containers" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "cetirun.memory.init" -}}
{{- if include "cetirun.memory.enabled" . }}
- name: memory-assets
  image: {{ include "cetirun.memory.image" . | quote }}
  imagePullPolicy: IfNotPresent
  command: ["/cetirun-memory", "install-assets", "/memory-assets"]
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
    # The asset volume is bounded below; an init-only disk limit caps the whole Pod.
    requests:
      cpu: 10m
      memory: 16Mi
      ephemeral-storage: 40Mi
    limits:
      cpu: 100m
      memory: 64Mi
  volumeMounts:
    - name: memory-assets
      mountPath: /memory-assets
{{- end }}
{{- end -}}

{{- define "cetirun.memory.env" -}}
{{- if include "cetirun.memory.enabled" . }}
- name: CETIRUN_MEMORY_ASSETS
  value: /opt/cetirun-memory
- name: CETIRUN_MEMORY_ROOT
  value: {{ printf "%s/.cetirun/memory" .Values.memoryContext.home | quote }}
{{- end }}
{{- end -}}

{{- define "cetirun.memory.mounts" -}}
{{- if include "cetirun.memory.enabled" . }}
- name: memory-assets
  mountPath: /opt/cetirun-memory
  readOnly: true
{{- end }}
{{- end -}}

{{- define "cetirun.memory.volumes" -}}
{{- if include "cetirun.memory.enabled" . }}
- name: memory-assets
  emptyDir:
    sizeLimit: 40Mi
{{- end }}
{{- end -}}
