// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"bytes"
	"reflect"
	"regexp"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	nativeAuthValuePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
	hermesAPIKeyLine       = regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?API_SERVER_KEY[ \t]*=[^\r\n]*`)
)

func cetiAdminNativeAuth(request admission.Request, agent *api.Agent) admission.Response {
	var next, old corev1.Secret
	if request.Operation != admissionv1.Update || decodeJSON(request.Object.Raw, &next, false) != nil || decodeJSON(request.OldObject.Raw, &old, false) != nil ||
		!matchingObjectIdentity(&next, &old) || old.Type != corev1.SecretTypeOpaque || (old.Immutable != nil && *old.Immutable) ||
		agent.Spec.Suspended || agent.Spec.PlannedMigration != nil || cetiadmin.MaintenanceUnfinished(agent) ||
		agent.Status.Phase != api.PhaseReady || old.Name != agent.Spec.GatewayTokenSecretRef ||
		old.Labels["app.kubernetes.io/managed-by"] != "cetirun-operator" || old.Labels["app.kubernetes.io/instance"] != agent.Spec.User.Alias ||
		(len(old.OwnerReferences) != 0 && !cetiAdminAgentOwner(&old, agent)) {
		return admission.Denied("native auth rotation requires the existing ordinary runtime Secret and a ready target")
	}
	requestID := agent.Annotations[nativeAuthRotationAnnotation]
	if !rotationIDPattern.MatchString(requestID) || next.Annotations[nativeAuthRotationAnnotation] != requestID {
		return admission.Denied("native auth rotation must match the target Agent request ID")
	}
	nativeKey := "OPENCLAW_GATEWAY_TOKEN"
	if agent.Spec.Agent.Type == api.AgentTypeHermes {
		nativeKey = "API_SERVER_KEY"
	} else if agent.Spec.Agent.Type != api.AgentTypeOpenClaw {
		return admission.Denied("native auth rotation is available only for Hermes and OpenClaw")
	}
	if len(old.Data[nativeKey]) == 0 || len(old.Data["CETIRUN_AUTH_KEY"]) == 0 ||
		!nativeAuthValuePattern.Match(next.Data[nativeKey]) || !nativeAuthValuePattern.Match(next.Data["CETIRUN_AUTH_KEY"]) ||
		bytes.Equal(old.Data[nativeKey], next.Data[nativeKey]) || bytes.Equal(old.Data["CETIRUN_AUTH_KEY"], next.Data["CETIRUN_AUTH_KEY"]) {
		return admission.Denied("native auth rotation changes both native and CetiRun keys without exposing them")
	}
	if agent.Spec.Agent.Type == api.AgentTypeHermes {
		if len(old.Data["hermes.env"]) == 0 || !bytes.Equal(next.Data["hermes.env"], rotatedHermesEnv(old.Data["hermes.env"], next.Data[nativeKey])) {
			return admission.Denied("Hermes environment may change only its matching API_SERVER_KEY")
		}
		next.Data["hermes.env"] = old.Data["hermes.env"]
	}
	next.Data[nativeKey], next.Data["CETIRUN_AUTH_KEY"] = old.Data[nativeKey], old.Data["CETIRUN_AUTH_KEY"]
	if previous, exists := old.Annotations[nativeAuthRotationAnnotation]; exists {
		next.Annotations[nativeAuthRotationAnnotation] = previous
	} else {
		delete(next.Annotations, nativeAuthRotationAnnotation)
		if len(next.Annotations) == 0 && old.Annotations == nil {
			next.Annotations = nil
		}
	}
	next.ManagedFields = old.ManagedFields
	if !reflect.DeepEqual(next, old) {
		return admission.Denied("native auth rotation must preserve every other Secret field")
	}
	return admission.Allowed("target native auth fields and receipt only; model credentials are unchanged")
}

func rotatedHermesEnv(previous, key []byte) []byte {
	entry := append([]byte("API_SERVER_KEY="), key...)
	if hermesAPIKeyLine.Match(previous) {
		return hermesAPIKeyLine.ReplaceAll(previous, entry)
	}
	result := bytes.Clone(previous)
	if len(result) != 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	result = append(result, entry...)
	return append(result, '\n')
}
