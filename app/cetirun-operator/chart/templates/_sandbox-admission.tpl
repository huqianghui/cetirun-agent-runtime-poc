{{/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT License.
*/}}
{{- define "cetirun-operator.admissionCA" -}}
{{- $encodedCA := required "sandboxAdmission.caBundle is required for fail-closed admission" .Values.sandboxAdmission.caBundle -}}
{{- $encodedCA = regexReplaceAll "[\\r\\n]" $encodedCA "" -}}
{{- $decodedCA := b64dec $encodedCA -}}
{{- if ne (b64enc $decodedCA) $encodedCA -}}{{- fail "sandboxAdmission.caBundle must be a base64-encoded PEM certificate bundle" -}}{{- end -}}
{{- $pattern := "-----BEGIN CERTIFICATE-----\\r?\\n(?:[A-Za-z0-9+/=]+\\r?\\n)+-----END CERTIFICATE-----" -}}
{{- $certificates := regexFindAll $pattern $decodedCA -1 -}}
{{- if or (empty $certificates) (ne (trim (regexReplaceAll $pattern $decodedCA "")) "") -}}{{- fail "sandboxAdmission.caBundle must be a base64-encoded PEM certificate bundle" -}}{{- end -}}
{{- range $certificate := $certificates -}}
{{- $body := regexReplaceAll "-----BEGIN CERTIFICATE-----|-----END CERTIFICATE-----|[[:space:]]" $certificate "" -}}
{{- if or (empty $body) (ne (b64enc (b64dec $body)) $body) -}}{{- fail "sandboxAdmission.caBundle contains invalid PEM certificate encoding" -}}{{- end -}}
{{- end -}}
{{- $encodedCA -}}
{{- end -}}

{{- define "cetirun-operator.sandboxCleanupMatch" -}}
- name: controller-cleanup-only
  expression: >-
    !(
      request.operation == "UPDATE" &&
      request.resource.group == "cetirun.microsoft.com" &&
      request.resource.resource == "agents" &&
      (has(request.subResource) ? request.subResource : "") == "" &&
      request.userInfo.username == {{ . | toJson }} &&
      object != null && oldObject != null &&
      has(oldObject.metadata.deletionTimestamp) &&
      oldObject.metadata.deletionTimestamp != null &&
      (has(object.apiVersion) ? object.apiVersion : "") ==
        (has(oldObject.apiVersion) ? oldObject.apiVersion : "") &&
      (has(object.kind) ? object.kind : "") ==
        (has(oldObject.kind) ? oldObject.kind : "") &&
      (has(object.spec) ? object.spec : {}) ==
        (has(oldObject.spec) ? oldObject.spec : {}) &&
      (has(object.status) ? object.status : {}) ==
        (has(oldObject.status) ? oldObject.status : {}) &&
      (has(object.metadata.name) ? object.metadata.name : "") ==
        (has(oldObject.metadata.name) ? oldObject.metadata.name : "") &&
      (has(object.metadata.namespace) ? object.metadata.namespace : "") ==
        (has(oldObject.metadata.namespace) ? oldObject.metadata.namespace : "") &&
      (has(object.metadata.uid) ? object.metadata.uid : "") ==
        (has(oldObject.metadata.uid) ? oldObject.metadata.uid : "") &&
      (has(object.metadata.labels) ? object.metadata.labels : {}) ==
        (has(oldObject.metadata.labels) ? oldObject.metadata.labels : {}) &&
      (has(object.metadata.annotations) ? object.metadata.annotations : {}) ==
        (has(oldObject.metadata.annotations) ? oldObject.metadata.annotations : {}) &&
      (has(object.metadata.ownerReferences) ? object.metadata.ownerReferences : []) ==
        (has(oldObject.metadata.ownerReferences) ? oldObject.metadata.ownerReferences : []) &&
      (has(object.metadata.deletionTimestamp) ? object.metadata.deletionTimestamp : null) ==
        (has(oldObject.metadata.deletionTimestamp) ? oldObject.metadata.deletionTimestamp : null) &&
      (has(object.metadata.deletionGracePeriodSeconds) ? object.metadata.deletionGracePeriodSeconds : null) ==
        (has(oldObject.metadata.deletionGracePeriodSeconds) ? oldObject.metadata.deletionGracePeriodSeconds : null) &&
      (has(object.metadata.generateName) ? object.metadata.generateName : "") ==
        (has(oldObject.metadata.generateName) ? oldObject.metadata.generateName : "") &&
      (has(object.metadata.creationTimestamp) ? object.metadata.creationTimestamp : null) ==
        (has(oldObject.metadata.creationTimestamp) ? oldObject.metadata.creationTimestamp : null) &&
      (has(object.metadata.generation) ? object.metadata.generation : 0) ==
        (has(oldObject.metadata.generation) ? oldObject.metadata.generation : 0) &&
      (has(object.metadata.resourceVersion) ? object.metadata.resourceVersion : "") ==
        (has(oldObject.metadata.resourceVersion) ? oldObject.metadata.resourceVersion : "") &&
      (has(object.metadata.selfLink) ? object.metadata.selfLink : "") ==
        (has(oldObject.metadata.selfLink) ? oldObject.metadata.selfLink : "") &&
      size(has(object.metadata.finalizers) ? object.metadata.finalizers : []) <
        size(has(oldObject.metadata.finalizers) ? oldObject.metadata.finalizers : []) &&
      (has(object.metadata.finalizers) ? object.metadata.finalizers : []).all(
        finalizer, finalizer in (has(oldObject.metadata.finalizers) ? oldObject.metadata.finalizers : [])
      ) &&
      (has(oldObject.metadata.finalizers) ? oldObject.metadata.finalizers : []).exists(
        finalizer, !(finalizer in (has(object.metadata.finalizers) ? object.metadata.finalizers : []))
      ) &&
      (has(oldObject.metadata.finalizers) ? oldObject.metadata.finalizers : []).all(
        finalizer,
        finalizer in (has(object.metadata.finalizers) ? object.metadata.finalizers : []) ||
        finalizer in [
          "cetirun.microsoft.com/finalizer",
          "cetirun.microsoft.com/sandbox-hold",
          "cetirun.microsoft.com/sandbox-preparation",
          "cetirun.microsoft.com/sandbox-bootstrap",
          "cetirun.microsoft.com/custom-runtime-ownership",
          "cetirun.microsoft.com/custom-apply-completion"
        ]
      )
    )
{{- end -}}

{{/*
Keep this GC-only exception aligned with podGarbageCollectorFinalizerUpdate.
Compare every object/metadata key, including fields introduced by newer API
servers; only finalizers and server-managed managedFields may differ.
*/}}
{{- define "cetirun-operator.sandboxPodCleanupMatch" -}}
- name: garbage-collector-cleanup-only
  expression: >-
    !(
      request.operation == "UPDATE" &&
      request.resource.group == "" &&
      request.resource.version == "v1" &&
      request.resource.resource == "pods" &&
      request.kind.group == "" &&
      request.kind.version == "v1" &&
      request.kind.kind == "Pod" &&
      (has(request.subResource) ? request.subResource : "") == "" &&
      (has(request.requestSubResource) ? request.requestSubResource : "") == "" &&
      (!has(request.requestResource) || request.requestResource == request.resource) &&
      (!has(request.requestKind) || request.requestKind == request.kind) &&
      request.userInfo.username in [
        "system:kube-controller-manager",
        "system:serviceaccount:kube-system:generic-garbage-collector"
      ] &&
      object != null && oldObject != null &&
      object.apiVersion == "v1" && object.kind == "Pod" &&
      has(request.namespace) && request.namespace != "" &&
      has(request.name) && request.name != "" &&
      object.metadata.namespace == request.namespace &&
      object.metadata.name == request.name &&
      has(object.metadata.uid) && object.metadata.uid != "" &&
      has(object.metadata.resourceVersion) && object.metadata.resourceVersion != "" &&
      has(oldObject.metadata.deletionTimestamp) &&
      oldObject.metadata.deletionTimestamp != null &&
      object.all(field,
        field == "metadata" || (field in oldObject && object[field] == oldObject[field])
      ) &&
      oldObject.all(field, field == "metadata" || field in object) &&
      object.metadata.all(field,
        field in ["finalizers", "managedFields"] ||
        (field in oldObject.metadata && object.metadata[field] == oldObject.metadata[field])
      ) &&
      oldObject.metadata.all(field,
        field in ["finalizers", "managedFields"] || field in object.metadata
      ) &&
      has(oldObject.metadata.finalizers) &&
      size(oldObject.metadata.finalizers.filter(finalizer, finalizer == "foregroundDeletion")) == 1 &&
      (has(object.metadata.finalizers) ? object.metadata.finalizers : []) ==
        oldObject.metadata.finalizers.filter(finalizer, finalizer != "foregroundDeletion")
    )
{{- end -}}
