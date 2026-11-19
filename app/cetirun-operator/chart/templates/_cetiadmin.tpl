{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}
{{- define "cetirun-operator.cetiadminConfigName" -}}cetirun-cetiadmin-config{{- end -}}

{{- define "cetirun-operator.cetiadminBindingName" -}}
{{- printf "cetiadmin-binding-%s" (printf "%s/%s" .Values.namespace (include "cetirun-operator.cetiadminConfigName" .) | sha256sum | trunc 24) -}}
{{- end -}}

{{- define "cetirun-operator.cetiadminUsername" -}}
{{- printf "system:serviceaccount:cetirun-%s:%s" .Values.cetiadmin.instanceName (include "cetirun-operator.cetiadminServiceAccountName" .) -}}
{{- end -}}

{{- define "cetirun-operator.cetiadminServiceAccountName" -}}
{{- printf "openclaw-%s-cetirun" .Values.cetiadmin.instanceName | trunc 63 | trimAll "-" -}}
{{- end -}}

{{- define "cetirun-operator.cetiadminConfig" -}}
{{- $c := .Values.cetiadmin -}}
{{- if not (kindIs "map" $c) -}}{{- fail "cetiadmin must be an object with explicit deployment inputs" -}}{{- end -}}
{{- range $key, $_ := $c -}}
{{- if not (has $key (list "enabled" "legacyDisabledSource" "instanceName" "owner" "platformReadNamespaces" "kubernetes" "model" "runtime" "azureWorkloadIdentity" "sharedSkills" "monitor" "retainedAgentUID" "migrationSource" "hookTimeoutSeconds")) -}}
{{- fail (printf "unsupported cetiadmin field %s" $key) -}}
{{- end -}}
{{- end -}}
{{- if not (kindIs "bool" $c.enabled) -}}{{- fail "cetiadmin.enabled must be a boolean" -}}{{- end -}}
{{- if not (kindIs "bool" $c.legacyDisabledSource) -}}{{- fail "cetiadmin.legacyDisabledSource must be a boolean" -}}{{- end -}}
{{- if and $c.legacyDisabledSource $c.enabled -}}{{- fail "cetiadmin.legacyDisabledSource requires cetiadmin.enabled=false" -}}{{- end -}}
{{- if or (not (kindIs "string" $c.instanceName)) (gt (len $c.instanceName) 44) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $c.instanceName)) -}}
{{- fail "cetiadmin.instanceName must be a DNS label of at most 44 characters for the openclaw- Helm release" -}}
{{- end -}}
{{- if or (not (regexMatch "^[0-9]+$" (toString $c.hookTimeoutSeconds))) (lt (int64 $c.hookTimeoutSeconds) 1) (gt (int64 $c.hookTimeoutSeconds) 3600) -}}
{{- fail "cetiadmin.hookTimeoutSeconds must be an integer between 1 and 3600" -}}
{{- end -}}
{{- range $field, $allowed := dict "owner" (list "alias" "tenantID" "objectID") "kubernetes" (list "apiServer" "apiAudience" "caConfigMapName" "apiEgressCIDRs") "model" (list "default" "initialMonthlyBudgetUSD" "githubCopilot") "runtime" (list "version" "image" "port") "azureWorkloadIdentity" (list "enabled" "clientId") "sharedSkills" (list "enabled") "monitor" (list "enabled" "schedule" "timezone" "deliver" "slackChannel" "slackSecretName") -}}
{{- $value := get $c $field -}}
{{- if not (kindIs "map" $value) -}}{{- fail (printf "cetiadmin.%s must be an object" $field) -}}{{- end -}}
{{- range $key, $_ := $value -}}
{{- if not (has $key $allowed) -}}{{- fail (printf "unsupported cetiadmin.%s field %s" $field $key) -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- $runtime := omit $c.runtime "version" "image" "port" -}}
{{- range $field := list "version" "image" -}}
{{- if not (kindIs "string" (get $c.runtime $field)) -}}{{- fail (printf "cetiadmin.runtime.%s must be a string" $field) -}}{{- end -}}
{{- end -}}
{{- $portSet := and (hasKey $c.runtime "port") (not (kindIs "invalid" $c.runtime.port)) -}}
{{- $version := $c.runtime.version | default (include "cetirun-operator.chartVersion" .) -}}
{{- if $c.enabled -}}
{{- if not (regexMatch "^[a-z][a-z0-9-]{0,62}$" (toString $c.owner.alias)) -}}{{- fail "cetiadmin.owner.alias must identify an existing owner" -}}{{- end -}}
{{- range $field := list "tenantID" "objectID" -}}
{{- $value := get $c.owner $field -}}
{{- if or (not (kindIs "string" $value)) (not (regexMatch "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$" $value)) (eq $value "00000000-0000-0000-0000-000000000000") -}}
{{- fail (printf "cetiadmin.owner.%s must be a nonzero UUID assigned by the deployment administrator" $field) -}}
{{- end -}}
{{- end -}}
{{- if or (not (kindIs "string" $c.model.initialMonthlyBudgetUSD)) (not (regexMatch "^(0|[1-9][0-9]*)(\\.[0-9]{1,6})?$" $c.model.initialMonthlyBudgetUSD)) -}}
{{- fail "cetiadmin.model.initialMonthlyBudgetUSD requires an explicit nonnegative decimal string" -}}
{{- end -}}
{{- if or (not (kindIs "string" $c.model.default)) (empty (trim $c.model.default)) -}}{{- fail "cetiadmin.model.default must name an authorized model" -}}{{- end -}}
{{- range $field, $value := dict "model.githubCopilot" $c.model.githubCopilot "azureWorkloadIdentity.enabled" $c.azureWorkloadIdentity.enabled "sharedSkills.enabled" $c.sharedSkills.enabled "monitor.enabled" $c.monitor.enabled -}}
{{- if not (kindIs "bool" $value) -}}{{- fail (printf "cetiadmin.%s must be a boolean" $field) -}}{{- end -}}
{{- end -}}
{{- if not (kindIs "slice" $c.platformReadNamespaces) -}}{{- fail "cetiadmin.platformReadNamespaces must be a list" -}}{{- end -}}
{{- range $name := $c.platformReadNamespaces -}}
{{- if or (not (kindIs "string" $name)) (gt (len $name) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $name)) -}}{{- fail "cetiadmin.platformReadNamespaces must contain valid namespace names" -}}{{- end -}}
{{- end -}}
{{- $k := $c.kubernetes -}}
{{- if or (not (kindIs "string" $k.apiServer)) (not (regexMatch "^https://[^/@?#[:space:]]+/?$" $k.apiServer)) -}}{{- fail "cetiadmin.kubernetes.apiServer must be an HTTPS origin without credentials" -}}{{- end -}}
{{- if or (not (kindIs "string" $k.apiAudience)) (empty (trim $k.apiAudience)) (eq $k.apiAudience .Values.connectionsMCP.audience) -}}{{- fail "cetiadmin.kubernetes.apiAudience must be explicit and distinct from the MCP audience" -}}{{- end -}}
{{- if or (not (kindIs "string" $k.caConfigMapName)) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $k.caConfigMapName)) -}}{{- fail "cetiadmin.kubernetes.caConfigMapName is required" -}}{{- end -}}
{{- if or (not (kindIs "slice" $k.apiEgressCIDRs)) (empty $k.apiEgressCIDRs) -}}{{- fail "cetiadmin.kubernetes.apiEgressCIDRs must contain verified single-host API addresses" -}}{{- end -}}
{{- range $cidr := $k.apiEgressCIDRs -}}
{{- if or (not (kindIs "string" $cidr)) (not (regexMatch "^([0-9.]+/32|[0-9a-fA-F:]+/128)$" $cidr)) -}}{{- fail "cetiadmin.kubernetes.apiEgressCIDRs must contain single-host CIDRs" -}}{{- end -}}
{{- end -}}
{{- if not .Values.aiGateway.enabled -}}{{- fail "cetiadmin requires aiGateway.enabled=true" -}}{{- end -}}
{{- if empty (trim .Values.domainSuffix) -}}{{- fail "cetiadmin requires domainSuffix for its native access route" -}}{{- end -}}
{{- $operatorVersion := include "cetirun-operator.version" . | default .Values.image.tag | default .Chart.AppVersion -}}
{{- if or (empty $version) (not (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+([-+][A-Za-z0-9.+-]+)?$" $version)) (ne $version $operatorVersion) -}}{{- fail "cetiadmin.runtime.version must select the matching Operator/runtime release" -}}{{- end -}}
{{- if and $portSet (or (kindIs "string" $c.runtime.port) (ne (toString $c.runtime.port) "8642")) -}}{{- fail "cetiadmin.runtime.port must match the Hermes profile (8642)" -}}{{- end -}}
{{- if and $c.runtime.image (not (or (hasSuffix (printf ":%s" $version) $c.runtime.image) (regexMatch "@sha256:[a-f0-9]{64}$" $c.runtime.image))) -}}{{- fail "cetiadmin.runtime.image must match runtime.version or pin a digest; its profile is verified by the Operator" -}}{{- end -}}
{{- if and $c.azureWorkloadIdentity.enabled (not (regexMatch "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$" $c.azureWorkloadIdentity.clientId)) -}}{{- fail "cetiadmin.azureWorkloadIdentity.clientId must identify an existing managed identity" -}}{{- end -}}
{{- if and $c.sharedSkills.enabled (empty (dig "skills" "storageAccount" "" .Values.cetirun.helmValues)) -}}{{- fail "cetiadmin.sharedSkills.enabled requires cetirun.helmValues.skills.storageAccount" -}}{{- end -}}
{{- if not (has $c.monitor.deliver (list "local" "slack")) -}}{{- fail "cetiadmin.monitor.deliver must be local or slack" -}}{{- end -}}
{{- if or (not (kindIs "string" $c.monitor.timezone)) (empty $c.monitor.timezone) (eq $c.monitor.timezone "Local") -}}{{- fail "cetiadmin.monitor.timezone must name an IANA timezone" -}}{{- end -}}
{{- if $c.monitor.enabled -}}
{{- if empty (trim $c.monitor.schedule) -}}{{- fail "cetiadmin.monitor.schedule is required" -}}{{- end -}}
{{- if eq $c.monitor.deliver "slack" -}}
{{- if or (empty $c.monitor.slackChannel) (regexMatch "[[:space:]]" $c.monitor.slackChannel) -}}{{- fail "cetiadmin.monitor.slackChannel is required for Slack" -}}{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $c.monitor.slackSecretName) -}}{{- fail "cetiadmin.monitor.slackSecretName is required for Slack" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $version -}}{{- $_ := set $runtime "version" $version -}}{{- end -}}
{{- if $c.runtime.image -}}{{- $_ := set $runtime "image" $c.runtime.image -}}{{- end -}}
{{- if $portSet -}}{{- $_ := set $runtime "port" $c.runtime.port -}}{{- end -}}
{{- $config := omit $c "hookTimeoutSeconds" "legacyDisabledSource" -}}
{{- $_ := set $config "runtime" $runtime -}}
{{- if $c.legacyDisabledSource -}}
{{- $_ := unset $config "platformReadNamespaces" -}}
{{- $_ := set $config "scope" (dict "mode" "all-managed-agents" "teamNames" (list) "platformReadNamespaces" $c.platformReadNamespaces) -}}
{{- end -}}
{{- toJson $config -}}
{{- end -}}

{{/* Keep shared admission during disable of an existing retained installation. */}}
{{- define "cetirun-operator.cetiadminAdmission" -}}
{{- $config := include "cetirun-operator.cetiadminConfig" . | fromJson -}}
{{- $record := lookup "v1" "ConfigMap" .Values.namespace (include "cetirun-operator.cetiadminBindingName" .) -}}
{{- $bound := false -}}
{{- if $record -}}
{{- $state := dig "data" "binding.json" "" $record | mustFromJson -}}
{{- if not (kindIs "map" $state) -}}{{- fail "CetiAdmin installation record is invalid; its admission protection cannot be removed" -}}{{- end -}}
{{- $agent := get $state "agent" -}}
{{- if or (ne (toString (get $state "schemaVersion")) "1") (not (kindIs "map" $agent)) -}}{{- fail "CetiAdmin installation record lacks its schema or Agent identity" -}}{{- end -}}
{{- if or (not (hasKey $agent "uid")) (not (kindIs "string" (get $agent "uid"))) -}}{{- fail "CetiAdmin installation record has an invalid Agent UID" -}}{{- end -}}
{{- $bound = not (empty (get $agent "uid")) -}}
{{- if and $bound (ne (get $agent "name") $config.instanceName) -}}{{- fail "cetiadmin.instanceName cannot change while the installation retains an Agent" -}}{{- end -}}
{{- end -}}
{{- if or $config.enabled (not (empty $config.owner.alias)) (not (empty $config.retainedAgentUID)) $bound -}}true{{- end -}}
{{- end -}}

{{- define "cetirun-operator.cetiadminMarked" -}}
{{- range $index, $object := list "object" "oldObject" -}}
{{- if $index }} || {{ end -}}
({{ $object }} != null && has({{ $object }}.metadata) && has({{ $object }}.metadata.annotations) &&
 {{ $object }}.metadata.annotations.exists(key, key.startsWith("cetirun.microsoft.com/cetiadmin-") || key.startsWith("cetirun.microsoft.com/helm-maintenance-") || key == "cetirun.microsoft.com/native-auth-rotation-request-id"))
{{- end -}}
{{- end -}}

{{- define "cetirun-operator.cetiadminManagedNamespace" -}}
{{- range $index, $object := list "object" "oldObject" -}}
{{- if $index }} || {{ end -}}
({{ $object }} != null && has({{ $object }}.metadata) && has({{ $object }}.metadata.labels) &&
 "app.kubernetes.io/managed-by" in {{ $object }}.metadata.labels && {{ $object }}.metadata.labels["app.kubernetes.io/managed-by"] == "cetirun-operator")
{{- end -}}
{{- end -}}
