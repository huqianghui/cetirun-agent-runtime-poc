// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (h *CetiAdminHandler) maintenanceGuard(ctx context.Context, request admission.Request, snapshot cetiAdminSnapshot) (admission.Response, bool) {
	if request.Resource.Group == "" && request.Resource.Resource == "namespaces" && request.SubResource != "status" {
		var next, old corev1.Namespace
		_ = decodeJSON(request.Object.Raw, &next, false)
		_ = decodeJSON(request.OldObject.Raw, &old, false)
		if next.Labels["app.kubernetes.io/managed-by"] != old.Labels["app.kubernetes.io/managed-by"] &&
			(next.Labels["app.kubernetes.io/managed-by"] == "cetirun-operator" || old.Labels["app.kubernetes.io/managed-by"] == "cetirun-operator") &&
			(!authenticated(request.UserInfo) || request.UserInfo.Username != h.config.ControllerUsername) {
			return admission.Denied("only the Operator changes the managed namespace admission selector"), true
		}
		for _, key := range []string{cetiadmin.MaintenanceAgentName, cetiadmin.MaintenanceAgentUID, cetiadmin.MaintenanceRequestID} {
			if next.Annotations[key] != old.Annotations[key] &&
				(!authenticated(request.UserInfo) || request.UserInfo.Username != h.config.ControllerUsername) {
				return admission.Denied("only the Operator changes Helm maintenance startup markers"), true
			}
		}
	}
	if request.Resource.Group == api.GroupVersion.Group && request.Resource.Resource == "agents" && request.SubResource == "" &&
		(request.Operation == admissionv1.Update || request.Operation == admissionv1.Create) {
		return cetiAdminMaintenanceIntent(request), true
	}
	if request.Namespace == "" || (request.Operation != admissionv1.Create && request.Operation != admissionv1.Update && request.Operation != admissionv1.Connect) {
		return admission.Response{}, false
	}
	resource := request.Resource.Group + "/" + request.Resource.Resource
	if resource != "/configmaps" && resource != "/pods" && resource != "apps/statefulsets" && resource != "apps/deployments" &&
		resource != "apps/replicasets" && resource != "apps/daemonsets" && resource != "batch/jobs" && resource != "batch/cronjobs" {
		return admission.Response{}, false
	}
	if request.SubResource == "status" {
		return admission.Response{}, false
	}
	namespace := &corev1.Namespace{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: request.Namespace}, namespace); err != nil {
		return admission.Denied("maintenance startup check cannot read the target namespace"), true
	}
	marked := namespace.Annotations[cetiadmin.MaintenanceAgentUID] != "" || namespace.Annotations[cetiadmin.MaintenanceAgentName] != "" || namespace.Annotations[cetiadmin.MaintenanceRequestID] != ""
	agent, err := h.maintenanceNamespaceAgent(ctx, namespace)
	if err != nil {
		return admission.Denied("maintenance startup cannot confirm the current runtime ownership"), true
	}
	if marked {
		if agent == nil || namespace.Annotations[cetiadmin.MaintenanceAgentName] != agent.Name ||
			string(agent.UID) != namespace.Annotations[cetiadmin.MaintenanceAgentUID] || agent.Status.Namespace != namespace.Name {
			return admission.Denied("maintenance marker cannot be matched to the current Agent UID"), true
		}
	} else if agent == nil {
		return admission.Response{}, false
	}
	if !marked && !cetiadmin.MaintenanceUnfinished(agent) {
		return admission.Response{}, false
	}
	if request.Operation == admissionv1.Create && request.SubResource == "" &&
		(resource == "/configmaps" || resource == "apps/statefulsets" || resource == "apps/deployments") {
		return h.maintenanceRestorationCreate(ctx, request, snapshot, namespace, agent), true
	}
	if resource == "/configmaps" && request.Operation == admissionv1.Update && request.SubResource == "" {
		return admission.Response{}, false // Existing bound config follows the normal Held write checks.
	}
	if request.Operation == admissionv1.Connect || request.Operation == admissionv1.Create || request.SubResource != "" {
		return admission.Denied("Helm maintenance blocks new Pods, hooks, exec and workload creation until explicit resume"), true
	}
	if resource == "/pods" || resource == "apps/daemonsets" || resource == "batch/jobs" || resource == "batch/cronjobs" {
		if resource == "/pods" && rawMetadataOnly(request) {
			return admission.Allowed("Pod cleanup metadata during maintenance"), true
		}
		return admission.Denied("Helm maintenance cannot modify an active Pod or restart a job"), true
	}
	var object struct {
		Spec struct {
			Replicas *int32 `json:"replicas"`
		} `json:"spec"`
	}
	if decodeJSON(request.Object.Raw, &object, false) != nil || object.Spec.Replicas == nil || *object.Spec.Replicas < 0 {
		return admission.Denied("Helm maintenance requires explicit nonnegative replicas"), true
	}
	if *object.Spec.Replicas != 0 {
		// Native rollback restores the historical replica intent. Pod admission
		// remains closed for every creator; the Operator reapplies zero on release.
		if snapshot.state == nil || !cetiadmin.MaintenanceHeld(agent) {
			return admission.Denied("only the current Held writer can restore historical replica intent"), true
		}
		if _, err := h.currentBinding(ctx, snapshot, request.UserInfo); err != nil {
			return admission.Denied("historical replica intent requires the current CetiAdmin binding"), true
		}
	}
	return admission.Allowed("runtime intent update while Pod and hook startup remain blocked"), true
}

// Maintenance observation includes deleting and bootstrapping Agents. It does
// not grant the caller permission to manage the resolved runtime.
func (h *CetiAdminHandler) maintenanceNamespaceAgent(ctx context.Context, namespace *corev1.Namespace) (*api.Agent, error) {
	name := namespace.Annotations[w.AgentNameAnnotation]
	hasOwner := false
	for _, owner := range namespace.OwnerReferences {
		if owner.APIVersion != api.GroupVersion.String() || owner.Kind != "Agent" {
			continue
		}
		if hasOwner || owner.Name == "" || owner.UID == "" || (name != "" && name != owner.Name) {
			return nil, fmt.Errorf("maintenance namespace has conflicting Agent ownership")
		}
		hasOwner, name = true, owner.Name
	}
	if name == "" {
		if !strings.HasPrefix(namespace.Name, "cetirun-") {
			return nil, nil
		}
		name = strings.TrimPrefix(namespace.Name, "cetirun-")
	}
	agent := &api.Agent{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) && !hasOwner && namespace.Annotations[w.AgentNameAnnotation] == "" && namespace.Annotations[w.AgentUIDAnnotation] == "" {
			return nil, nil
		}
		return nil, err
	}
	if agent.UID == "" || agent.Namespace != "" || namespace.UID == "" || namespace.Name != "cetirun-"+agent.Name ||
		(agent.Status.Namespace != "" && agent.Status.Namespace != namespace.Name) ||
		(namespace.Annotations[w.AgentUIDAnnotation] != "" && namespace.Annotations[w.AgentUIDAnnotation] != string(agent.UID)) {
		return nil, fmt.Errorf("maintenance namespace does not match the current Agent identity")
	}
	if namespace.Labels[w.ProtectedNamespaceLabel] == "true" {
		if !reflect.DeepEqual(namespace.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}) ||
			namespace.Annotations[w.AgentNameAnnotation] != agent.Name || namespace.Annotations[w.AgentUIDAnnotation] != string(agent.UID) {
			return nil, fmt.Errorf("maintenance namespace does not match common bootstrap ownership")
		}
	} else if !cetiAdminAgentOwner(namespace, agent) && !(agent.Spec.Agent.Type == api.AgentTypeCustom && len(namespace.OwnerReferences) == 0 && namespace.Annotations[w.AgentUIDAnnotation] == string(agent.UID)) {
		return nil, fmt.Errorf("maintenance namespace ownership cannot be confirmed")
	}
	return agent, nil
}

// Read only the bounded restore grant, not a Helm payload or credential body.
// The checkpoint namespace/name is protected for all writers by protectSource.
func (h *CetiAdminHandler) maintenanceRestorationCreate(ctx context.Context, request admission.Request, snapshot cetiAdminSnapshot, namespace *corev1.Namespace, agent *api.Agent) admission.Response {
	deny := func() admission.Response {
		return admission.Denied("maintenance restoration requires the exact Operator Reapplying grant")
	}
	status := agent.Status.HelmMaintenance
	if !authenticated(request.UserInfo) || request.UserInfo.Username != h.config.ControllerUsername || !agent.Spec.Suspended || agent.Spec.Helm.Maintenance != nil ||
		agent.DeletionTimestamp != nil || agent.Spec.PlannedMigration != nil || agent.Status.PlannedMigration != nil || protectedCetiAdminTarget(agent) ||
		status == nil || status.Phase != api.HelmMaintenancePhaseReapplying || status.ObservedGeneration != agent.Generation ||
		namespace.Labels["app.kubernetes.io/managed-by"] != "cetirun-operator" || namespace.Annotations[cetiadmin.MaintenanceRequestID] != status.RequestID ||
		snapshot.source == nil || snapshot.state == nil || snapshot.state.Source.UID != snapshot.source.UID {
		return deny()
	}
	config, err := cetiadmin.ParseConfig(snapshot.source.Data[cetiadmin.ConfigKey])
	if err != nil {
		return deny()
	}
	_, target, err := cetiadmin.ResolveRuntimeTarget(ctx, h.reader, config, snapshot.state.Record, agent.Name, namespace.Name)
	if err != nil || target.Protected {
		return deny()
	}
	record, err := h.readCetiAdminMaintenance(ctx, agent)
	if err != nil || record.Restoration == nil || record.Resumed ||
		record.Phase != api.HelmMaintenancePhaseReapplying || !sameCetiAdminMaintenanceTarget(record.Target, target) ||
		record.RequestGeneration+1 != agent.Generation || record.Request.RequestID != status.RequestID || record.ApplyRevision < 1 ||
		record.Request.ExpectedRevision != int64(record.Restoration.Revision) || record.ApplyRevision != record.Restoration.Revision ||
		status.Revision != int64(record.ApplyRevision) || record.PreparedHash == "" || len(record.Restoration.Objects) == 0 || len(record.Restoration.Objects) > 256 {
		return deny()
	}
	if digest, err := hex.DecodeString(record.Restoration.ManifestHash); err != nil || len(digest) != sha256.Size {
		return deny()
	}
	version := request.Kind.Version
	if request.Kind.Group != "" {
		version = request.Kind.Group + "/" + version
	}
	object := &unstructured.Unstructured{}
	if object.UnmarshalJSON(request.Object.Raw) != nil || object.GetGenerateName() != "" || object.GetName() != request.Name || object.GetNamespace() != namespace.Name ||
		object.GetAPIVersion() != version || object.GetKind() != request.Kind.Kind ||
		object.GetAnnotations()["helm.sh/hook"] != "" {
		return deny()
	}
	switch object.GetAPIVersion() + "/" + object.GetKind() {
	case "v1/ConfigMap":
		if object.Object["immutable"] != nil && object.Object["immutable"] != false {
			return deny()
		}
	case "apps/v1/StatefulSet", "apps/v1/Deployment":
		replicas, found, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
		if err != nil || !found || replicas != 0 {
			return deny()
		}
	default:
		return deny()
	}
	matches := 0
	for _, ref := range record.Restoration.Objects {
		if ref.APIVersion == object.GetAPIVersion() && ref.Kind == object.GetKind() && ref.Namespace == namespace.Name && ref.Name == object.GetName() {
			if ref.UID != "" || ref.CreationID != "" || object.GetUID() != "" && object.GetUID() == ref.PreviousUID {
				return deny()
			}
			matches++
		}
	}
	if matches != 1 {
		return deny()
	}
	// CREATE cannot replace an existing object. The Operator checked absence
	// before persisting this grant; the API store enforces AlreadyExists atomically.
	// Admission need not acquire GET rights to not-yet-created runtime ConfigMaps.
	return admission.Allowed("Operator recreates the recorded missing runtime object without starting compute")
}

func cetiAdminMaintenanceIntent(request admission.Request) admission.Response {
	var next, old api.Agent
	if decodeJSON(request.Object.Raw, &next, false) != nil {
		return admission.Denied("invalid maintenance Agent")
	}
	if request.Operation == admissionv1.Create {
		if next.Spec.Helm.Maintenance != nil {
			return admission.Denied("Helm maintenance requires an existing Agent and release")
		}
		return admission.Allowed("no Helm maintenance at creation")
	}
	if decodeJSON(request.OldObject.Raw, &old, false) != nil {
		return admission.Denied("maintenance requires the current Agent")
	}
	intent, previous, status := next.Spec.Helm.Maintenance, old.Spec.Helm.Maintenance, old.Status.HelmMaintenance
	if intent != nil {
		if !next.Spec.Suspended || protectedCetiAdminTarget(&next) || next.DeletionTimestamp != nil || next.Spec.PlannedMigration != nil ||
			!rotationIDPattern.MatchString(intent.RequestID) || intent.ExpectedRevision < 1 {
			return admission.Denied("Helm maintenance requires a suspended ordinary target and exact request/revision")
		}
		if previous != nil && (!reflect.DeepEqual(next.Spec, old.Spec) || *intent != *previous) {
			return admission.Denied("an active Helm maintenance request cannot change its target intent")
		}
		if previous == nil && status != nil && (status.RequestID == intent.RequestID || status.Phase != api.HelmMaintenancePhaseReleased) {
			return admission.Denied("new maintenance requires a released previous handoff and a fresh request ID")
		}
	}
	if previous != nil && intent == nil {
		if !next.Spec.Suspended || !cetiadmin.MaintenanceHeld(&old) {
			return admission.Denied("ending maintenance requires the current Held handoff and continued suspension")
		}
		copy := next.Spec.DeepCopy()
		copy.Helm.Maintenance = previous
		if !reflect.DeepEqual(*copy, old.Spec) {
			return admission.Denied("ending maintenance cannot change the target configuration")
		}
	}
	if old.Spec.Suspended && !next.Spec.Suspended && status != nil {
		if intent != nil || previous != nil || status.Phase != api.HelmMaintenancePhaseReleased || status.ObservedGeneration != old.Generation ||
			!rawChangesOnly(request, []string{"suspended"}, []string{"generation", "managedFields"}) {
			return admission.Denied("resume requires current Released evidence and a separate suspension-only update")
		}
	}
	if previous == nil && intent == nil && status != nil && old.Spec.Suspended && next.Spec.Suspended && !reflect.DeepEqual(next.Spec, old.Spec) {
		return admission.Denied("configuration changes require resume or a new maintenance handoff")
	}
	return admission.Allowed("Helm maintenance transition preserves stop and handoff semantics")
}

func matchingObjectIdentity(next, old metav1.Object) bool {
	return old.GetUID() != "" && next.GetUID() == old.GetUID() && next.GetResourceVersion() != "" &&
		next.GetResourceVersion() == old.GetResourceVersion() && next.GetName() == old.GetName() &&
		next.GetNamespace() == old.GetNamespace() && old.GetDeletionTimestamp() == nil
}
