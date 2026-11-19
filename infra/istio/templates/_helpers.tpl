{{/*
Chart name.
*/}}
{{- define "cetirun-istio.name" -}}
{{- .Chart.Name }}
{{- end }}

{{/*
Common labels applied to all resources.
*/}}
{{- define "cetirun-istio.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: cetirun
{{- end }}

{{/*
Lua-safe domain pattern (dots escaped as %.).
*/}}
{{- define "cetirun-istio.luaDomainPattern" -}}
{{- .Values.domain | replace "." "%." }}
{{- end }}

{{/* New ingress options retain their defaults when upgrading older values. */}}
{{- define "cetirun-istio.ingressSettings" -}}
{{- $settings := dict "includeWildcardHost" true "cookieSecretLegacyNamespace" "istio-system" "skipJwtBearerTokens" (eq .Values.oauth2Proxy.mode "entra-id") -}}
{{- if hasKey .Values.gateway "includeWildcardHost" -}}
{{- $value := .Values.gateway.includeWildcardHost -}}
{{- if not (kindIs "bool" $value) -}}
{{- fail "gateway.includeWildcardHost must be a boolean" -}}
{{- end -}}
{{- $_ := set $settings "includeWildcardHost" $value -}}
{{- end -}}
{{- if hasKey .Values "cookieSecretLegacyNamespace" -}}
{{- $value := .Values.cookieSecretLegacyNamespace -}}
{{- if not (kindIs "string" $value) -}}
{{- fail "cookieSecretLegacyNamespace must be a namespace string or empty to disable migration" -}}
{{- end -}}
{{- if and $value (or (gt (len $value) 63) (not (regexMatch `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` $value))) -}}
{{- fail "cookieSecretLegacyNamespace must be a valid namespace or empty to disable migration" -}}
{{- end -}}
{{- $_ := set $settings "cookieSecretLegacyNamespace" $value -}}
{{- end -}}
{{- $bearer := .Values.oauth2Proxy.skipJwtBearerTokens -}}
{{- if not (kindIs "invalid" $bearer) -}}
{{- if not (kindIs "bool" $bearer) -}}
{{- fail "oauth2Proxy.skipJwtBearerTokens must be a boolean or null for the mode default" -}}
{{- end -}}
{{- if and (has .Values.oauth2Proxy.mode (list "oidc" "local-oidc")) $bearer -}}
{{- fail (printf "oauth2Proxy.skipJwtBearerTokens=true is not supported in %s mode" .Values.oauth2Proxy.mode) -}}
{{- end -}}
{{- $_ := set $settings "skipJwtBearerTokens" $bearer -}}
{{- end -}}
{{- $logging := dict "request" true "auth" true "standard" true -}}
{{- if hasKey .Values.oauth2Proxy "logging" -}}
{{- if not (kindIs "map" .Values.oauth2Proxy.logging) -}}
{{- fail "oauth2Proxy.logging must be an object" -}}
{{- end -}}
{{- range $key, $value := .Values.oauth2Proxy.logging -}}
{{- if not (hasKey $logging $key) -}}
{{- fail (printf "oauth2Proxy.logging.%s is not supported" $key) -}}
{{- end -}}
{{- if not (kindIs "bool" $value) -}}
{{- fail (printf "oauth2Proxy.logging.%s must be a boolean" $key) -}}
{{- end -}}
{{- $_ := set $logging $key $value -}}
{{- end -}}
{{- end -}}
{{- $_ := set $settings "logging" $logging -}}
{{- toJson $settings -}}
{{- end }}

{{/* Restrict the unauthenticated OAuth callback to the supported endpoint. */}}
{{- define "cetirun-istio.validateOAuthCallback" -}}
{{- if and .Values.authPolicy.oauthCallbackPath (ne .Values.authPolicy.oauthCallbackPath "/api/v1/connections/callback") -}}
{{- fail "authPolicy.oauthCallbackPath supports only /api/v1/connections/callback" -}}
{{- end -}}
{{- end }}

{{/* An explicit external URL preserves existing Redis deployments. */}}
{{- define "cetirun-istio.sessionSettings" -}}
{{- $store := default dict .Values.oauth2Proxy.sessionStore -}}
{{- $type := default "redis" $store.type -}}
{{- if not (has $type (list "cookie" "redis")) -}}
{{- fail "oauth2Proxy.sessionStore.type must be cookie or redis" -}}
{{- end -}}
{{- $redis := default dict $store.redis -}}
{{- $bundled := default dict $redis.bundled -}}
{{- $enabled := true -}}
{{- if hasKey $bundled "enabled" -}}
{{- if not (kindIs "bool" $bundled.enabled) -}}
{{- fail "oauth2Proxy.sessionStore.redis.bundled.enabled must be a boolean" -}}
{{- end -}}
{{- $enabled = $bundled.enabled -}}
{{- end -}}
{{- $url := default "" $redis.connectionURL -}}
{{- $secret := default "" $redis.existingSecret -}}
{{- $key := default "redis-password" $redis.passwordKey -}}
{{- $useBundled := and (eq $type "redis") $enabled (not $url) -}}
{{- if $useBundled -}}
{{- $url = printf "redis://oauth2-proxy-sessions.%s.svc.cluster.local:6379/0" .Release.Namespace -}}
{{- $secret = default "oauth2-proxy-sessions" $secret -}}
{{- if not (kindIs "bool" $bundled.persistence.enabled) -}}
{{- fail "oauth2Proxy.sessionStore.redis.bundled.persistence.enabled must be a boolean" -}}
{{- end -}}
{{- else if eq $type "redis" -}}
{{- $_ := required "oauth2Proxy.sessionStore.redis.connectionURL is required for external Redis sessions" $url -}}
{{- $_ := required "oauth2Proxy.sessionStore.redis.existingSecret is required for external Redis sessions" $secret -}}
{{- if not (regexMatch `^rediss?://[^/@[:space:]]+(/[0-9]+)?$` $url) -}}
{{- fail "oauth2Proxy.sessionStore.redis.connectionURL must use redis:// or rediss:// without credentials" -}}
{{- end -}}
{{- end -}}
{{- toJson (dict "type" $type "bundled" $useBundled "connectionURL" $url "secretName" $secret "passwordKey" $key) -}}
{{- end }}
