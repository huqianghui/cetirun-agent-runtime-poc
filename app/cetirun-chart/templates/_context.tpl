{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}
{{- define "cetirun.context.enabled" -}}
{{- $nativeReconciled := and .Values.sandboxWorkload .Values.sandboxWorkload.reconciled (ne (include "cetirun.agent.type" .) "custom") -}}
{{- if and (or (not .Values.sandboxWorkload) $nativeReconciled) (or .Values.memoryContext.enabled (and (eq (include "cetirun.agent.type" .) "custom") .Values.skills.shared)) -}}true{{- end -}}
{{- end -}}

{{- define "cetirun.context.home" -}}
{{- if eq (include "cetirun.agent.type" .) "custom" -}}
{{- .Values.runtime.agent.home -}}
{{- else -}}/home/vibe{{- end -}}
{{- end -}}

{{- define "cetirun.context.validate" -}}
{{- $marketplaceSubPath := .Values.skills.subPath -}}
{{- $marketplaceSelection := (.Values.skills.marketplace | default dict).selectionDigest -}}
{{- if or $marketplaceSubPath $marketplaceSelection -}}
{{- /* A reconciled Custom Sandbox composes its context view in its own
   template (agents/custom/_sandbox.tpl). */ -}}
{{- $customSandbox := and .Values.sandboxWorkload .Values.sandboxWorkload.reconciled (eq (include "cetirun.agent.type" .) "custom") -}}
{{- if not (or (include "cetirun.context.enabled" .) $customSandbox) -}}
{{- fail "Skill Marketplace skills require the composed Agent context" -}}
{{- end -}}
{{- /* The copied release lives in the context view, which only a reconciled
   Sandbox sizes for it. Native Agents mount it at skills.mountPath, and
   Custom Agents read it in .agents/skills. */ -}}
{{- if not (and .Values.sandboxWorkload .Values.sandboxWorkload.reconciled) -}}
{{- fail "Skill Marketplace skills require a reconciled Sandbox workload" -}}
{{- end -}}
{{- if not (and $marketplaceSubPath $marketplaceSelection) -}}
{{- fail "Skill Marketplace skills require both skills.subPath and skills.marketplace.selectionDigest" -}}
{{- end -}}
{{- if not .Values.skills.shared -}}
{{- fail "Skill Marketplace skills require skills.shared" -}}
{{- end -}}
{{- end -}}
{{- if include "cetirun.context.enabled" . -}}
{{- $root := printf "%s/.agents" (include "cetirun.context.home" .) -}}
{{- if and .Values.skills.shared (ne .Values.skills.mountPath (printf "%s/skills" $root)) -}}
{{- $alias := .Values.skills.mountPath -}}
{{- if or (not (hasPrefix "/" $alias)) (ne $alias (clean $alias)) (eq $alias "/") -}}{{- fail "skills.mountPath must be a clean absolute path" -}}{{- end -}}
{{- range list $root (printf "%s/.cetirun/memory" (include "cetirun.context.home" .)) "/opt/cetirun-memory" "/etc/cetirun" "/tmp" -}}
{{- if or (eq . $alias) (hasPrefix (printf "%s/" .) $alias) (hasPrefix (printf "%s/" $alias) .) -}}
{{- fail "skills.mountPath overlaps a platform path" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range .Values.extraVolumeMounts -}}
{{- $mount := clean .mountPath -}}
{{- if or (eq $mount "/") (eq $mount $root) (hasPrefix (printf "%s/" $root) $mount) (hasPrefix (printf "%s/" $mount) $root) -}}
{{- fail "the Agent context path is reserved" -}}
{{- end -}}
{{- end -}}
{{- range .Values.extraVolumes -}}
{{- if eq .name "context-view" -}}{{- fail "the Agent context volume is reserved" -}}{{- end -}}
{{- end -}}
{{- range concat (.Values.initContainers | default list) (.Values.sidecars | default list) -}}
{{- range .volumeMounts -}}
{{- if eq .name "context-view" -}}{{- fail "additional containers cannot mount the Agent context volume" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "cetirun.context.initEnv" -}}
{{- if or (not .Values.sandboxWorkload) (and .Values.sandboxWorkload.reconciled (ne (include "cetirun.agent.type" .) "custom")) }}
{{- if include "cetirun.context.enabled" . }}
- name: CETIRUN_CONTEXT_VIEW
  value: /etc/cetirun-context-view
{{- end }}
{{- /* Custom Agents compose shared skills into the view; for a Skill
   Marketplace Team, the initializer copies the Team's release. */}}
{{- if and .Values.skills.shared (or (eq (include "cetirun.agent.type" .) "custom") (.Values.skills.marketplace | default dict).selectionDigest) (include "cetirun.context.enabled" .) }}
- name: CETIRUN_SHARED_SKILLS_DIR
  value: /etc/cetirun-shared-skills
{{- with (.Values.skills.marketplace | default dict).selectionDigest }}
- name: CETIRUN_SKILLS_RELEASE_SELECTION
  value: {{ . | quote }}
{{- end }}
{{- end }}
{{- if .Values.memoryContext.enabled }}
- name: CETIRUN_MEMORY_ASSETS
  value: /opt/cetirun-memory
{{- end }}
{{- end }}
{{- end -}}

{{- define "cetirun.context.initMounts" -}}
{{- if or (not .Values.sandboxWorkload) (and .Values.sandboxWorkload.reconciled (ne (include "cetirun.agent.type" .) "custom")) }}
{{- if include "cetirun.context.enabled" . }}
- name: context-view
  mountPath: /etc/cetirun-context-view
{{- end }}
{{- if and .Values.skills.shared (or (eq (include "cetirun.agent.type" .) "custom") (.Values.skills.marketplace | default dict).selectionDigest) (include "cetirun.context.enabled" .) }}
- name: shared-skills
  mountPath: /etc/cetirun-shared-skills
  readOnly: true
{{- with .Values.skills.subPath }}
  subPath: {{ . | quote }}
{{- end }}
{{- end }}
{{- include "cetirun.memory.mounts" . }}
{{- end }}
{{- end -}}

{{- define "cetirun.context.mount" -}}
{{- if include "cetirun.context.enabled" . }}
{{- if ne (include "cetirun.agent.type" .) "custom" }}
{{- if (.Values.skills.marketplace | default dict).selectionDigest }}
{{- /* Skill Marketplace: the Team's release that the initializer copied, not
   the claim, which holds every Team's subtree. */}}
- name: context-view
  mountPath: {{ .Values.skills.mountPath | quote }}
  subPath: current/team-skills
  readOnly: true
{{- else if .Values.skills.shared }}
- name: shared-skills
  mountPath: {{ .Values.skills.mountPath | quote }}
  readOnly: true
{{- end }}
- name: context-view
  mountPath: /home/vibe/.agents/AGENTS.md
  subPath: current/AGENTS.md
  readOnly: true
{{- else }}
- name: context-view
  mountPath: {{ printf "%s/.agents" (include "cetirun.context.home" .) | quote }}
  subPath: current
  readOnly: true
{{- if and .Values.skills.shared (ne .Values.skills.mountPath (printf "%s/.agents/skills" (include "cetirun.context.home" .))) }}
- name: context-view
  mountPath: {{ .Values.skills.mountPath | quote }}
  subPath: current/skills
  readOnly: true
{{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "cetirun.context.volume" -}}
{{- if include "cetirun.context.enabled" . }}
- name: context-view
  {{- if and .Values.sandboxWorkload .Values.sandboxWorkload.reconciled }}
  emptyDir:
    sizeLimit: {{ .Values.sandboxWorkload.scratchSizeLimit | quote }}
  {{- else if ne (include "cetirun.agent.type" .) "custom" }}
  emptyDir:
    sizeLimit: 40Mi
  {{- else }}
  emptyDir: {}
  {{- end }}
{{- end }}
{{- end -}}
