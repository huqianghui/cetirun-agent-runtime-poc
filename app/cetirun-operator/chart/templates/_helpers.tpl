{{/*
Shared registry and version defaults.
*/}}
{{- define "cetirun-operator.registry" -}}
{{- .Values.global.registry | trimSuffix "/" -}}
{{- end }}

{{- define "cetirun-operator.version" -}}
{{- .Values.global.version -}}
{{- end }}

{{- define "cetirun-operator.chartRepoURL" -}}
{{- $registry := include "cetirun-operator.registry" . -}}
{{- if $registry -}}
{{- printf "oci://%s/helm/cetirun" $registry -}}
{{- else -}}
{{- .Values.cetirun.chart.repoURL -}}
{{- end -}}
{{- end }}

{{- define "cetirun-operator.chartVersion" -}}
{{- include "cetirun-operator.version" . | default .Values.cetirun.chart.version | default .Chart.AppVersion -}}
{{- end }}

{{/*
Container image reference with optional digest pinning.
When image.digest is set, produces: repository:tag@digest
Otherwise: repository:tag
*/}}
{{- define "cetirun-operator.image" -}}
{{- $registry := include "cetirun-operator.registry" . -}}
{{- $repo := .Values.image.repository -}}
{{- if $registry }}{{- $repo = printf "%s/cetirun-operator" $registry -}}{{- end -}}
{{- $tag := include "cetirun-operator.version" . | default .Values.image.tag | default .Chart.AppVersion -}}
{{ $repo }}:{{ $tag }}{{ if .Values.image.digest }}@{{ .Values.image.digest }}{{ end }}
{{- end }}

{{- define "cetirun-operator.sandboxSourcesConfig" -}}
{{- $sources := .Values.sandboxSources -}}
{{- if not (kindIs "map" $sources) -}}
{{- fail "sandboxSources must be an object" -}}
{{- end -}}
{{- if not (kindIs "bool" $sources.enabled) -}}
{{- fail "sandboxSources.enabled must be a boolean" -}}
{{- end -}}
{{- if and (hasKey $sources "nativeExecution") (not (kindIs "bool" $sources.nativeExecution)) -}}
{{- fail "sandboxSources.nativeExecution must be a boolean" -}}
{{- end -}}
{{- if hasKey $sources "reconciledExecution" -}}
{{- fail "sandboxSources.reconciledExecution is no longer supported; nativeExecution selects common reconciliation" -}}
{{- end -}}
{{- if and (hasKey $sources "plannedMigration") (not (kindIs "bool" $sources.plannedMigration)) -}}
{{- fail "sandboxSources.plannedMigration must be a boolean" -}}
{{- end -}}
{{- if and $sources.plannedMigration (not $sources.nativeExecution) -}}
{{- fail "sandboxSources.plannedMigration requires nativeExecution and upgraded readers" -}}
{{- end -}}
{{- if and $sources.nativeExecution (not (and $sources.enabled $sources.bootstrapOwnedPrerequisites .Values.aiGateway.enabled)) -}}
{{- fail "sandboxSources.nativeExecution requires enabled sources, owned bootstrap and AI Gateway" -}}
{{- end -}}
{{- if and $sources.nativeExecution $sources.placementConfigMapName -}}
{{- fail "sandboxSources.nativeExecution uses RuntimeClass scheduling; a placement registry is unsupported" -}}
{{- end -}}
{{- if and (hasKey $sources "bootstrapOwnedPrerequisites") (not (kindIs "bool" $sources.bootstrapOwnedPrerequisites)) -}}
{{- fail "sandboxSources.bootstrapOwnedPrerequisites must be a boolean" -}}
{{- end -}}
{{- if and $sources.bootstrapOwnedPrerequisites (not $sources.enabled) -}}
{{- fail "sandboxSources.bootstrapOwnedPrerequisites requires enabled sources" -}}
{{- end -}}
{{- if and (hasKey $sources "placementConfigMapName") (not (kindIs "string" $sources.placementConfigMapName)) -}}
{{- fail "sandboxSources.placementConfigMapName must be a resource name" -}}
{{- end -}}
{{- if $sources.placementConfigMapName -}}
{{- if not (and $sources.enabled (eq $sources.placementConfigMapName .Values.sandboxAdmission.placementConfigMapName)) -}}
{{- fail "sandboxSources.placementConfigMapName requires enabled sources and the same admission placement registry" -}}
{{- end -}}
{{- if or (eq $sources.placementConfigMapName $sources.platformConfigMapName) (eq $sources.placementConfigMapName $sources.skillsCatalogConfigMapName) (gt (len $sources.placementConfigMapName) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $sources.placementConfigMapName)) -}}
{{- fail "sandboxSources.placementConfigMapName must be a separate valid ConfigMap name" -}}
{{- end -}}
{{- end -}}
{{- if and (hasKey $sources "skillsCatalogConfigMapName") (not (kindIs "string" $sources.skillsCatalogConfigMapName)) -}}
{{- fail "sandboxSources.skillsCatalogConfigMapName must be a resource name" -}}
{{- end -}}
{{- if $sources.skillsCatalogConfigMapName -}}
{{- if not (and $sources.enabled $sources.bootstrapOwnedPrerequisites) -}}
{{- fail "sandboxSources.skillsCatalogConfigMapName requires enabled owned bootstrap" -}}
{{- end -}}
{{- if or (eq $sources.skillsCatalogConfigMapName $sources.platformConfigMapName) (gt (len $sources.skillsCatalogConfigMapName) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $sources.skillsCatalogConfigMapName)) -}}
{{- fail "sandboxSources.skillsCatalogConfigMapName must be a separate valid ConfigMap name" -}}
{{- end -}}
{{- end -}}
{{- if $sources.enabled -}}
{{- range $key, $_ := $sources -}}
{{- if not (has $key (list "enabled" "nativeExecution" "plannedMigration" "bootstrapOwnedPrerequisites" "platformConfigMapName" "homeClaimName" "skillsClaimName" "skillsCatalogConfigMapName" "placementConfigMapName" "helpers")) -}}
{{- fail (printf "unsupported sandboxSources field %s" $key) -}}
{{- end -}}
{{- end -}}
{{- if not (and .Values.sandboxAdmission.enabled .Values.sandboxAdmission.enforce .Values.sandboxAdmission.podsEnabled) -}}
{{- fail "sandboxSources.enabled requires enforced Sandbox configuration and Pod admission" -}}
{{- end -}}
{{- range $field := list "platformConfigMapName" "homeClaimName" "skillsClaimName" -}}
{{- $name := get $sources $field -}}
{{- if not (kindIs "string" $name) -}}
{{- fail (printf "sandboxSources.%s must be a resource name" $field) -}}
{{- end -}}
{{- $optionalSkills := and (eq $field "skillsClaimName") (eq $name "") (not $sources.skillsCatalogConfigMapName) -}}
{{- if and (not $optionalSkills) (or (gt (len $name) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $name))) -}}
{{- fail (printf "sandboxSources.%s must be a valid resource name" $field) -}}
{{- end -}}
{{- end -}}
{{- if eq $sources.homeClaimName $sources.skillsClaimName -}}
{{- fail "sandboxSources HOME and skills must use distinct claims" -}}
{{- end -}}
{{- if not (kindIs "map" $sources.helpers) -}}
{{- fail "sandboxSources.helpers must be an object" -}}
{{- end -}}
{{- range $key, $_ := $sources.helpers -}}
{{- if not (has $key (list "doctor" "logger" "desktop")) -}}
{{- fail (printf "unsupported sandboxSources.helpers field %s" $key) -}}
{{- end -}}
{{- end -}}
{{- range $field := list "doctor" "logger" "desktop" -}}
{{- if not (kindIs "bool" (get $sources.helpers $field)) -}}
{{- fail (printf "sandboxSources.helpers.%s must be a boolean" $field) -}}
{{- end -}}
{{- end -}}
{{- if and $sources.nativeExecution $sources.helpers.desktop -}}
{{- fail "sandboxSources.nativeExecution does not support the desktop helper" -}}
{{- end -}}
{{- $config := deepCopy $sources -}}
{{- if not $sources.nativeExecution -}}
{{- $_ := unset $config "nativeExecution" -}}
{{- end -}}
{{- if not $sources.plannedMigration -}}
{{- $_ := unset $config "plannedMigration" -}}
{{- end -}}
{{- if not $sources.bootstrapOwnedPrerequisites -}}
{{- $_ := unset $config "bootstrapOwnedPrerequisites" -}}
{{- end -}}
{{- if not $sources.skillsCatalogConfigMapName -}}
{{- $_ := unset $config "skillsCatalogConfigMapName" -}}
{{- end -}}
{{- if not $sources.placementConfigMapName -}}
{{- $_ := unset $config "placementConfigMapName" -}}
{{- end -}}
{{- if .Values.sandboxAdmission.coordinationConfigMapName -}}
{{- if eq .Values.sandboxAdmission.coordinationConfigMapName $sources.skillsCatalogConfigMapName -}}
{{- fail "Sandbox coordination and skills catalog require separate ConfigMaps" -}}
{{- end -}}
{{- $_ := set $config "coordinationConfigMapName" .Values.sandboxAdmission.coordinationConfigMapName -}}
{{- $_ := set $config "coordinationConfigMapUID" .Values.sandboxAdmission.coordinationConfigMapUID -}}
{{- end -}}
{{- $_ := set $config "admissionServiceAccount" "cetirun-sandbox-admission" -}}
{{- toJson $config -}}
{{- end -}}
{{- end }}

{{/*
CETIRUN_SKILL_MARKETPLACE: {"enabled":true,"storage":{...}} while the Skill
Marketplace is on, empty otherwise. A release upgraded with --reuse-values from
a chart without the setting keeps it off. The operator creates the skills
claims of native Agents, so the marketplace needs native Sandbox execution, a
skills claim name and no published skills catalog. The same checks cover the
worker's values.
*/}}
{{- define "cetirun-operator.skillMarketplaceConfig" -}}
{{- $marketplace := .Values.skillMarketplace -}}
{{- if kindIs "invalid" $marketplace -}}
{{- $marketplace = dict -}}
{{- end -}}
{{- if not (kindIs "map" $marketplace) -}}
{{- fail "skillMarketplace must be an object" -}}
{{- end -}}
{{- range $key, $_ := $marketplace -}}
{{- if not (has $key (list "enabled" "repository" "storage" "reservedNames" "teamGCGrace" "limits" "worker")) -}}
{{- fail (printf "unsupported skillMarketplace field %s" $key) -}}
{{- end -}}
{{- end -}}
{{- if and (hasKey $marketplace "enabled") (not (kindIs "bool" $marketplace.enabled)) -}}
{{- fail "skillMarketplace.enabled must be a boolean" -}}
{{- end -}}
{{- if $marketplace.enabled -}}
{{- $sources := .Values.sandboxSources -}}
{{- if or (not $sources.enabled) (not $sources.nativeExecution) $sources.skillsCatalogConfigMapName -}}
{{- fail "skillMarketplace.enabled requires sandboxSources.enabled and sandboxSources.nativeExecution, and an empty sandboxSources.skillsCatalogConfigMapName: the operator gives native Agents the Team skills claims, and the marketplace replaces the published skills catalog" -}}
{{- end -}}
{{- if not $sources.skillsClaimName -}}
{{- fail "skillMarketplace.enabled requires sandboxSources.skillsClaimName: the operator creates the Team skills claim with that name in each Agent namespace" -}}
{{- end -}}
{{- $repository := $marketplace.repository | default dict -}}
{{- $storage := $marketplace.storage | default dict -}}
{{- if not $repository.url -}}
{{- fail "skillMarketplace.repository.url is required" -}}
{{- end -}}
{{- if not (has $repository.auth (list "none" "token" "ssh")) -}}
{{- fail "skillMarketplace.repository.auth must be none, token or ssh" -}}
{{- end -}}
{{- if and (ne $repository.auth "none") (not $repository.existingSecret) -}}
{{- fail "skillMarketplace.repository.existingSecret is required for token and ssh authentication" -}}
{{- end -}}
{{- if and (hasPrefix "https://" $repository.url) (contains "@" (trimPrefix "https://" $repository.url | splitList "/" | first)) -}}
{{- fail "skillMarketplace.repository.url must not contain credentials; use skillMarketplace.repository.existingSecret" -}}
{{- end -}}
{{- if or (contains "?" $repository.url) (contains "#" $repository.url) -}}
{{- fail "skillMarketplace.repository.url must not contain a query or a fragment: the worker logs the URL and the catalog shows it; keep credentials in skillMarketplace.repository.existingSecret" -}}
{{- end -}}
{{- if and (hasPrefix "http://" $repository.url) (not $repository.allowInsecureHTTP) -}}
{{- fail "skillMarketplace.repository.url must use https://" -}}
{{- end -}}
{{- range $field := list "resourceGroup" "storageAccount" "containerName" "storageClassName" -}}
{{- if not (get $storage $field) -}}
{{- fail (printf "skillMarketplace.storage.%s is required" $field) -}}
{{- end -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]{3,24}$" $storage.storageAccount) -}}
{{- fail "skillMarketplace.storage.storageAccount must be 3-24 lowercase letters and digits" -}}
{{- end -}}
{{- if or (not (regexMatch "^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])$" $storage.containerName)) (contains "--" $storage.containerName) -}}
{{- fail "skillMarketplace.storage.containerName must be a valid Blob container name" -}}
{{- end -}}
{{- if or (not (regexMatch "^[a-zA-Z0-9_().-]{1,90}$" $storage.resourceGroup)) (hasSuffix "." $storage.resourceGroup) -}}
{{- fail "skillMarketplace.storage.resourceGroup must be a resource group name" -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $storage.storageClassName) -}}
{{- fail "skillMarketplace.storage.storageClassName must be a StorageClass name" -}}
{{- end -}}
{{- $limits := $marketplace.limits | default dict -}}
{{- range $field := list "maxRepositoryObjects" "maxSkillFiles" "maxSkillDepth" "maxDescriptionLength" -}}
{{- if not (regexMatch "^[1-9][0-9]{0,8}$" (toString (get $limits $field))) -}}
{{- fail (printf "skillMarketplace.limits.%s must be a positive whole number" $field) -}}
{{- end -}}
{{- end -}}
{{- range $field := list "maxRepositorySize" "maxSkillSize" "maxSkillMDSize" -}}
{{- if not (regexMatch "^[1-9][0-9]{0,11}(Ki|Mi|Gi)?$" (toString (get $limits $field))) -}}
{{- fail (printf "skillMarketplace.limits.%s must be a size such as 5Mi: a positive whole number, optionally with Ki, Mi or Gi" $field) -}}
{{- end -}}
{{- end -}}
{{- $config := dict "enabled" true "storage" (dict "resourceGroup" $storage.resourceGroup "storageAccount" $storage.storageAccount "containerName" $storage.containerName "storageClassName" $storage.storageClassName) -}}
{{- toJson $config -}}
{{- end -}}
{{- end }}

{{/*
Bytes of a Skill Marketplace size limit such as 256Mi, which the
configuration check accepted, as a whole number.
*/}}
{{- define "cetirun-operator.skillMarketplaceBytes" -}}
{{- $value := toString . -}}
{{- $number := regexFind "^[0-9]+" $value -}}
{{- $scale := get (dict "" 1 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824) (trimPrefix $number $value) -}}
{{- printf "%.0f" (mulf (float64 $number) $scale) -}}
{{- end }}

{{/*
Image of the per-Agent GitHub Copilot proxy: the AI Gateway image, from the
same registry and release as the rest of CetiRun. Empty when the proxy is off.
*/}}
{{- define "cetirun-operator.copilotProxyImage" -}}
{{- $proxy := .Values.aiGateway.copilotProxy | default dict -}}
{{- if $proxy.enabled -}}
{{- $image := $proxy.image | default dict -}}
{{- $registry := include "cetirun-operator.registry" . -}}
{{- $repo := $image.repository | default "mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-ai-gateway" -}}
{{- if $registry }}{{- $repo = printf "%s/cetirun-ai-gateway" $registry -}}{{- end -}}
{{- $tag := include "cetirun-operator.version" . | default $image.tag | default .Chart.AppVersion -}}
{{ $repo }}:{{ $tag }}{{ if $image.digest }}@{{ $image.digest }}{{ end }}
{{- end -}}
{{- end }}
