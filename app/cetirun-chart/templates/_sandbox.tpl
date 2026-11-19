{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}
{{- define "cetirun.sandbox.validate" -}}
{{- $s := .Values.sandboxWorkload -}}
{{- if hasKey $s "trustedProxyIPs" -}}
{{- if not (kindIs "slice" $s.trustedProxyIPs) -}}
{{- fail "Sandbox trustedProxyIPs must be a compiler-selected list" -}}
{{- end -}}
{{- range $s.trustedProxyIPs -}}
{{- if or (not (kindIs "string" .)) (empty .) -}}
{{- fail "Sandbox trustedProxyIPs must contain nonempty compiler-selected IPs" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $emptySkills := eq ($s.skillsMode | default "") "empty" -}}
{{- if and (hasKey $s "skillsMode") (not $emptySkills) -}}
{{- fail "Sandbox skillsMode must be empty when explicitly supplied" -}}
{{- end -}}
{{- $reconciled := $s.reconciled | default false -}}
{{- if or (ne $s.contractVersion "v1") (and (not $reconciled) (or (ne $s.executionHeld true) (ne (int .Values.replicaCount) 0))) (and $reconciled (or $s.executionHeld (gt (int .Values.replicaCount) 1) (lt (int .Values.replicaCount) 0))) -}}
{{- fail "Sandbox render plans must retain the execution hold" -}}
{{- end -}}
{{- if $s.ingressMTLS -}}
{{- if or (not $reconciled) (include "cetirun.tailscaleEnabled" .) -}}
{{- fail "managed ingress mTLS requires common built-in API/UI/terminal/desktop routing without alternate ingress" -}}
{{- end -}}
{{- end -}}
{{- if $s.egressSNI -}}
{{- if or (not $reconciled) (ne (len ($s.managedInitContainers | default list)) 2) .Values.initContainers .Values.sidecars (not .Values.readinessProbe.enabled) -}}
{{- fail "observed-SNI capture requires the managed Envoy/init prefix and proxy-aware common-executor readiness" -}}
{{- end -}}
{{- else if $s.managedInitContainers -}}
{{- fail "managed capture initializers require observed-SNI controls" -}}
{{- end -}}
{{- if and (eq (include "cetirun.agent.type" .) "custom") (not $reconciled) -}}
{{- fail "Custom requires common reconciled Sandbox execution" -}}
{{- end -}}
{{- if or (ne (index .Values.podAnnotations "sidecar.istio.io/inject") "false") .Values.sidecarScope.enabled (not .Values.networkPolicy.enabled) (gt (len .Values.sidecarScope.additionalEgressHosts) 0) -}}
{{- fail "Sandbox network controls are protected" -}}
{{- end -}}
{{- $dns := $s.dnsConfig | default dict -}}
{{- $servers := $dns.nameservers | default list -}}
{{- if or (eq (len $servers) 0) (gt (len $servers) 3) -}}
{{- fail "Sandbox requires one to three compiler-selected DNS server IPs" -}}
{{- end -}}
{{- if or (not .Values.runtimeClassName) (not .Values.persistence.existingClaim) .Values.persistence.useStatefulSetVolumeClaim (and (not $emptySkills) (not .Values.skills.existingClaim)) -}}
{{- fail "Sandbox requires a runtime and separately owned existing HOME/skills claims" -}}
{{- end -}}
{{- if $emptySkills -}}
{{- if or (and (not $reconciled) (ne (include "cetirun.agent.type" .) "openclaw")) (not .Values.aiGateway.enabled) .Values.skills.shared .Values.skills.existingClaim .Values.skills.npm .Values.skills.clawHub (ne .Values.skills.mountPath "/home/vibe/.aks/skills") (and (not $reconciled) (ne (toJson ($s.podFinalizers | default list)) (toJson (list "cetirun.microsoft.com/native-pod-lifetime")))) -}}
{{- fail "Sandbox empty skills require native OpenClaw Gateway lifetime protection, no shared claim or packages, and the fixed skills path" -}}
{{- end -}}
{{- end -}}
{{- if or .Values.azureWorkloadIdentity.enabled .Values.agentIdentity.enabled (and .Values.connectionsMCP.enabled (not $reconciled)) (ne (include "cetirun.automountServiceAccountToken" .) "false") -}}
{{- fail "Sandbox identities must use compiler-owned explicit projections" -}}
{{- end -}}
{{- if or (and (not $reconciled) (or .Values.initContainers .Values.sidecars .Values.extraVolumes .Values.extraVolumeMounts .Values.extraEnvFrom)) .Values.skills.npm .Values.autoscaling.enabled .Values.ingress.enabled .Values.execRestriction.enabled -}}
{{- fail "Sandbox protected extension paths are not supported" -}}
{{- end -}}
{{- if not $s.objects -}}
{{- fail "Sandbox requires compiler-owned enforcement resources" -}}
{{- end -}}
{{- end -}}
