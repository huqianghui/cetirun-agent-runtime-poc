{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}

{{- define "cetirun-platform.publicTLS.validate" -}}
{{- $settings := .Values.publicTLS -}}
{{- $domain := required "publicTLS.domain is required when publicTLS.enabled is true" $settings.domain -}}
{{- if or (gt (len $domain) 253) (not (regexMatch `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$` $domain)) -}}
{{- fail "publicTLS.domain must be one lowercase DNS hostname without a wildcard" -}}
{{- end -}}
{{- if not (regexMatch `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` $settings.ingressNamespace) -}}
{{- fail "publicTLS.ingressNamespace must be a Kubernetes namespace name" -}}
{{- end -}}
{{- $serviceSuffix := printf ".%s.svc.cluster.local" $settings.ingressNamespace -}}
{{- if or (not (hasSuffix $serviceSuffix $settings.ingressServiceHostname)) (not (regexMatch `^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$` $settings.ingressServiceHostname)) -}}
{{- fail "publicTLS.ingressServiceHostname must be an explicit Service hostname in publicTLS.ingressNamespace" -}}
{{- end -}}
{{- if not (regexMatch `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` $settings.gateway.name) -}}
{{- fail "publicTLS.gateway.name must be a Kubernetes name" -}}
{{- end -}}
{{- range $setting := list $settings.acme.stagingCertificateEnabled $settings.acme.productionCertificateEnabled -}}
{{- if not (kindIs "bool" $setting) -}}
{{- fail "publicTLS.acme certificate enabled settings must be booleans" -}}
{{- end -}}
{{- end -}}
{{- range $name := list $settings.acme.stagingIssuerName $settings.acme.productionIssuerName $settings.acme.stagingCertificateName $settings.acme.productionCertificateName $settings.acme.stagingSecretName $settings.acme.productionSecretName -}}
{{- if not (regexMatch `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` $name) -}}
{{- fail "publicTLS.acme issuer, certificate, and Secret names must be Kubernetes names" -}}
{{- end -}}
{{- end -}}
{{- if eq $settings.acme.stagingIssuerName $settings.acme.productionIssuerName -}}
{{- fail "publicTLS.acme staging and production issuer names must differ" -}}
{{- end -}}
{{- if and $settings.acme.stagingCertificateEnabled $settings.acme.productionCertificateEnabled (eq $settings.acme.stagingSecretName $settings.acme.productionSecretName) -}}
{{- fail "publicTLS.acme staging and production Secret names must differ" -}}
{{- end -}}
{{- if and $settings.acme.stagingCertificateEnabled $settings.acme.productionCertificateEnabled (eq $settings.acme.stagingCertificateName $settings.acme.productionCertificateName) -}}
{{- fail "publicTLS.acme staging and production certificate names must differ" -}}
{{- end -}}
{{- end -}}

{{- define "cetirun-platform.publicTLS.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: cetirun
{{- end -}}

{{- define "cetirun-platform.publicTLS.keep" -}}
helm.sh/resource-policy: keep
{{- end -}}
