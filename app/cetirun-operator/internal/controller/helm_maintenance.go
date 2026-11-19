// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"helm.sh/helm/v3/pkg/release"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Storage observations and no-hooks application are separate from the owning
// manager's confirmation that its prior release writer has finished.
type helmMaintenanceClient interface {
	CurrentRelease(context.Context, string, string) (*release.Release, error)
	InstallOrUpgradeMaintenance(context.Context, string, string, *helm.PreparedRelease, int, func(context.Context) error) error
}

type helmMaintenanceQuiescence interface {
	MaintenanceQuiescent(context.Context, string, string) error
}

func (r *AgentReconciler) maintenanceQuiescent(ctx context.Context, target cetiadmin.TargetIdentity) error {
	agent := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: target.Agent.Name}, agent); err != nil {
		return err
	}
	if !identityMatches(target.Agent, agent) {
		return cetiadmin.ErrConflict
	}
	_, checkpoint, err := r.readMaintenance(ctx, agent)
	if err != nil {
		return err
	}
	if checkpoint != nil && checkpoint.Target.Agent == target.Agent && checkpoint.Target.Namespace == target.Namespace &&
		(checkpoint.Phase == api.HelmMaintenancePhaseHeld || checkpoint.Phase == api.HelmMaintenancePhaseReapplying || checkpoint.Phase == api.HelmMaintenancePhaseReleased) {
		return r.maintenanceGuard(ctx, agent, checkpoint)
	}
	coordinator, ok := r.HelmClient.(helmMaintenanceQuiescence)
	if !ok || coordinator == nil {
		return fmt.Errorf("Helm maintenance requires positive confirmation that the previous Operator writer has finished")
	}
	return coordinator.MaintenanceQuiescent(ctx, target.Namespace.Name, target.Release)
}

type helmMaintenanceTargets interface {
	MaintenanceTargetForAgent(context.Context, *api.Agent) (cetiadmin.TargetIdentity, error)
}

type helmMaintenanceRestorationAuthority interface {
	MaintenanceRestorationReady(context.Context, *api.Agent) error
}

const (
	maintenanceRecordKey     = "maintenance.json"
	maintenanceCoverageLabel = "app.kubernetes.io/managed-by"
	maintenanceCoverageValue = "cetirun-operator"
)

// One retained handoff checkpoint per Agent, never a command or credential log.
type helmMaintenanceRecord struct {
	Version            int                         `json:"version"`
	Target             cetiadmin.TargetIdentity    `json:"target"`
	Request            api.HelmMaintenanceRequest  `json:"request"`
	RequestGeneration  int64                       `json:"requestGeneration"`
	IntentHash         string                      `json:"intentHash"`
	Phase              api.HelmMaintenancePhase    `json:"phase"`
	ApplyRevision      int                         `json:"applyRevision,omitempty"`
	AppliedRevision    int                         `json:"appliedRevision,omitempty"`
	PreparedHash       string                      `json:"preparedHash,omitempty"`
	ValuesHash         string                      `json:"valuesHash,omitempty"`
	PreviousRequestID  string                      `json:"previousRequestID,omitempty"`
	ReleasedGeneration int64                       `json:"releasedGeneration,omitempty"`
	Resumed            bool                        `json:"resumed,omitempty"`
	Restoration        *helmMaintenanceRestoration `json:"restoration,omitempty"`
}

// This inventory records observed absence in one pending-install manifest. UID
// stays empty until the fenced Operator creation is read back from the API.
type helmMaintenanceRestoration struct {
	Revision     int                            `json:"revision"`
	ManifestHash string                         `json:"manifestHash"`
	Objects      []helmMaintenanceRestoreObject `json:"objects"`
}

type helmMaintenanceRestoreObject struct {
	cetiadmin.ManagedResource
	PreviousUID types.UID `json:"previousUID,omitempty"`
}

func (r *CetiAdminReconciler) MaintenanceRestorationReady(ctx context.Context, agent *api.Agent) error {
	if r.Runtime == nil || agent == nil || agent.UID == "" || r.AdmissionReady == nil {
		return cetiadmin.ErrUnbound
	}
	if err := r.AdmissionReady(ctx); err != nil {
		return err
	}
	hooks := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: cetiAdminAdmissionName}, hooks); err != nil {
		return err
	}
	covered := false
	for _, hook := range hooks.Webhooks {
		if hook.Name == "cetiadmin-workloads.cetirun.microsoft.com" {
			covered = ruleCovers(hook.Rules, "", "v1", "configmaps", admissionregistrationv1.Create) &&
				ruleCovers(hook.Rules, "", "v1", "configmaps", admissionregistrationv1.Update)
		}
	}
	if !covered {
		return fmt.Errorf("maintenance restoration requires managed-namespace ConfigMap admission coverage")
	}
	key := r.Runtime.maintenanceKey(agent)
	access := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               "system:serviceaccount:" + r.Source.Namespace + ":cetirun-sandbox-admission",
		Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + r.Source.Namespace, "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: key.Namespace, Verb: "get", Group: "", Version: "v1", Resource: "configmaps", Name: key.Name},
	}}
	if err := r.Create(ctx, access); err != nil {
		return err
	}
	if !access.Status.Allowed || access.Status.Denied || access.Status.EvaluationError != "" {
		return fmt.Errorf("maintenance admission cannot read this Agent's exact checkpoint")
	}
	return nil
}

// MaintenanceTargetForAgent never reads Helm: apply guards run inside release
// coordination. Only the matching operation may supply recorded resource UIDs.
func (r *CetiAdminReconciler) MaintenanceTargetForAgent(ctx context.Context, expected *api.Agent) (cetiadmin.TargetIdentity, error) {
	var none cetiadmin.TargetIdentity
	if r.Runtime == nil || r.AdmissionReady == nil {
		return none, cetiadmin.ErrUnbound
	}
	if err := r.AdmissionReady(ctx); err != nil {
		return none, err
	}
	_, state, err := r.readState(ctx)
	if err != nil {
		return none, err
	}
	source := &corev1.ConfigMap{}
	if err := r.reader().Get(ctx, r.Source, source); err != nil {
		return none, err
	}
	if !identityMatches(state.Source, source) || expected == nil || expected.UID == "" || expected.UID == state.Agent.UID {
		return none, cetiadmin.ErrConflict
	}
	config, err := cetiadmin.ParseConfig(source.Data[cetiadmin.ConfigKey])
	if err != nil {
		return none, err
	}
	current, target, err := cetiadmin.ResolveRuntimeTarget(ctx, r.reader(), config, state.Record, expected.Name, instanceNamespace(expected.Name))
	if err != nil {
		return none, err
	}
	if current.UID != expected.UID || current.Spec.PlannedMigration != nil || current.Status.PlannedMigration != nil || target.Protected {
		return none, cetiadmin.ErrConflict
	}
	if slices.Contains([]string{r.Runtime.OperatorNamespace, r.Runtime.registryNamespace(), r.Runtime.ConnectionsMCPBrokerNamespace}, target.Namespace.Name) {
		return none, cetiadmin.ErrUnbound
	}
	_, record, err := r.Runtime.readMaintenance(ctx, current)
	if err != nil {
		return none, err
	}
	if record == nil {
		return target, nil
	}
	if request := current.Spec.Helm.Maintenance; request != nil {
		if request.RequestID != record.Request.RequestID && record.Phase == api.HelmMaintenancePhaseReleased {
			return target, nil
		}
		if *request != record.Request || record.Resumed {
			return none, cetiadmin.ErrConflict
		}
	} else if current.Status.HelmMaintenance == nil || current.Status.HelmMaintenance.RequestID != record.Request.RequestID {
		return none, cetiadmin.ErrConflict
	}
	saved := record.Target
	saved.Resources = nil
	if !reflect.DeepEqual(saved, target) {
		return none, cetiadmin.ErrConflict
	}
	target.Resources = slices.Clone(record.Target.Resources)
	return target, nil
}

func (r *AgentReconciler) maintenanceKey(agent *api.Agent) client.ObjectKey {
	hash := sha256.Sum256([]byte(agent.UID))
	return client.ObjectKey{Namespace: r.OperatorNamespace, Name: "cetiadmin-maintenance-" + hex.EncodeToString(hash[:16])}
}

func (r *AgentReconciler) readMaintenance(ctx context.Context, agent *api.Agent) (*corev1.ConfigMap, *helmMaintenanceRecord, error) {
	if r.OperatorNamespace == "" {
		return nil, nil, fmt.Errorf("Helm maintenance requires the configured Operator namespace")
	}
	object := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, r.maintenanceKey(agent), object); apierrors.IsNotFound(err) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	var record helmMaintenanceRecord
	decoder := json.NewDecoder(strings.NewReader(object.Data[maintenanceRecordKey]))
	decoder.DisallowUnknownFields()
	if object.UID == "" || object.DeletionTimestamp != nil || !hasAgentOwner(object, agent) || len(object.Data) != 1 || len(object.BinaryData) != 0 ||
		decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Version != 1 ||
		record.Target.Agent != identityOf(agent) || record.Target.Namespace.UID == "" || record.Target.Release != helmReleaseName(agent.Name) ||
		record.Request.RequestID == "" || record.Request.ExpectedRevision < 1 || record.RequestGeneration < 1 || record.IntentHash == "" {
		return nil, nil, fmt.Errorf("Helm maintenance checkpoint is unbound or invalid")
	}
	if err := validMaintenanceRestoration(&record); err != nil {
		return nil, nil, err
	}
	return object, &record, nil
}

func (r *AgentReconciler) saveMaintenance(ctx context.Context, agent *api.Agent, object *corev1.ConfigMap, record *helmMaintenanceRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	key := r.maintenanceKey(agent)
	if object == nil {
		object = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(agent, api.GroupVersion.WithKind("Agent"))}},
			Data: map[string]string{maintenanceRecordKey: string(data)}}
		err = r.Create(ctx, object)
	} else if object.Data[maintenanceRecordKey] != string(data) {
		before := object.DeepCopy()
		object.Data[maintenanceRecordKey] = string(data)
		err = r.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if err != nil {
		return fmt.Errorf("persist Helm maintenance checkpoint: %w", err)
	}
	actual, saved, err := r.readMaintenance(ctx, agent)
	if err != nil {
		return err
	}
	if actual == nil || (object.UID != "" && actual.UID != object.UID) || !reflect.DeepEqual(saved, record) {
		return fmt.Errorf("Helm maintenance checkpoint readback changed")
	}
	*object = *actual
	return nil
}

func (r *AgentReconciler) maintenanceIntentHash(ctx context.Context, agent *api.Agent) (string, error) {
	copy := agent.DeepCopy()
	copy.Spec.Suspended, copy.Spec.Helm.Maintenance = true, nil
	var team *api.Team
	if copy.Spec.Team != "" {
		team = &api.Team{}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: copy.Spec.Team}, team); err != nil {
			return "", err
		}
		if team.UID == "" || team.DeletionTimestamp != nil || team.Spec.PolicyConfigured() {
			return "", fmt.Errorf("Helm maintenance requires a current ordinary Team")
		}
		team.ResourceVersion, team.Generation, team.Status = "", 0, api.TeamStatus{}
		team.ManagedFields, team.CreationTimestamp = nil, metav1.Time{}
	}
	customRevision := ""
	if provider, ok := r.CustomBindings.(*KubernetesCustomBindings); ok && copy.Spec.Agent.Type == api.AgentTypeCustom {
		customRevision = provider.revision
	}
	data, err := json.Marshal([]any{copy.Spec, copy.Annotations, copy.Labels, team,
		r.CetiRunHelmValues, r.ChartRegistry, r.ChartVersion, r.DomainSuffix, r.SkillsStorageAccount, r.DefaultModel,
		r.AIGateway, r.MemoryEnabled, r.MemoryImage, r.MemoryRegistry, r.ChatAccess, customRevision,
		r.AgentIdentityEnabled, r.AgentIdentityTenantID, r.AgentIdentityBrokerURL, r.ConnectionsMCPURL,
		r.ConnectionsMCPAudience, r.ConnectionsMCPCASecretName, r.ConnectionsMCPRuntimeTypes})
	if err != nil {
		return "", fmt.Errorf("Helm maintenance intent cannot be hashed")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func maintenanceMarkersMatch(namespace *corev1.Namespace, record *helmMaintenanceRecord) bool {
	return namespace.Labels[maintenanceCoverageLabel] == maintenanceCoverageValue && maintenanceMarkerIdentityMatches(namespace, record)
}

func maintenanceMarkerIdentityMatches(namespace *corev1.Namespace, record *helmMaintenanceRecord) bool {
	return identityMatches(record.Target.Namespace, namespace) &&
		namespace.Annotations[cetiadmin.MaintenanceAgentName] == record.Target.Agent.Name &&
		namespace.Annotations[cetiadmin.MaintenanceAgentUID] == string(record.Target.Agent.UID) &&
		namespace.Annotations[cetiadmin.MaintenanceRequestID] == record.Request.RequestID
}

func (r *AgentReconciler) ensureMaintenanceMarkers(ctx context.Context, record *helmMaintenanceRecord) error {
	namespace := &corev1.Namespace{}
	key := client.ObjectKey{Name: record.Target.Namespace.Name}
	if err := r.customReader().Get(ctx, key, namespace); err != nil {
		return err
	}
	if !identityMatches(record.Target.Namespace, namespace) {
		return fmt.Errorf("Helm maintenance namespace identity changed")
	}
	if maintenanceMarkersMatch(namespace, record) {
		return nil
	}
	if label := namespace.Labels[maintenanceCoverageLabel]; label != "" && label != maintenanceCoverageValue {
		return fmt.Errorf("Helm maintenance namespace admission selector has foreign ownership")
	}
	sameIdentity := maintenanceMarkerIdentityMatches(namespace, record)
	if sameIdentity && record.Phase != api.HelmMaintenancePhaseRequested {
		return fmt.Errorf("Helm maintenance namespace admission coverage was lost")
	}
	previous := record.PreviousRequestID != "" && record.Phase == api.HelmMaintenancePhaseRequested &&
		namespace.Annotations[cetiadmin.MaintenanceAgentName] == record.Target.Agent.Name &&
		namespace.Annotations[cetiadmin.MaintenanceAgentUID] == string(record.Target.Agent.UID) &&
		namespace.Annotations[cetiadmin.MaintenanceRequestID] == record.PreviousRequestID
	if !previous && !sameIdentity {
		for _, marker := range []string{cetiadmin.MaintenanceAgentName, cetiadmin.MaintenanceAgentUID, cetiadmin.MaintenanceRequestID} {
			if namespace.Annotations[marker] != "" {
				return fmt.Errorf("Helm maintenance startup markers conflict")
			}
		}
	}
	if record.Phase != api.HelmMaintenancePhaseRequested {
		return fmt.Errorf("Helm maintenance startup markers were lost")
	}
	before := namespace.DeepCopy()
	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	namespace.Labels[maintenanceCoverageLabel] = maintenanceCoverageValue
	namespace.Annotations[cetiadmin.MaintenanceAgentName] = record.Target.Agent.Name
	namespace.Annotations[cetiadmin.MaintenanceAgentUID] = string(record.Target.Agent.UID)
	namespace.Annotations[cetiadmin.MaintenanceRequestID] = record.Request.RequestID
	if err := r.Patch(ctx, namespace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	if err := r.customReader().Get(ctx, key, namespace); err != nil {
		return err
	}
	if !maintenanceMarkersMatch(namespace, record) {
		return fmt.Errorf("Helm maintenance startup markers were not observed")
	}
	return nil
}

func (r *AgentReconciler) maintenanceGuard(ctx context.Context, expected *api.Agent, record *helmMaintenanceRecord) error {
	current := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
		return err
	}
	if current.UID != expected.UID || current.Generation != expected.Generation || !reflect.DeepEqual(current.Spec, expected.Spec) ||
		current.DeletionTimestamp != nil || current.Spec.PlannedMigration != nil || current.Status.PlannedMigration != nil || sandboxRequested(current, nil) || current.Status.Sandbox != nil {
		return fmt.Errorf("Helm maintenance Agent intent changed")
	}
	_, saved, err := r.readMaintenance(ctx, current)
	if err != nil {
		return err
	}
	// A failed first checkpoint must still stop the current suspended request.
	// Held and later phases always require the persisted operation snapshot.
	unrecordedStop := saved == nil && record.Phase == api.HelmMaintenancePhaseRequested && record.ApplyRevision == 0 &&
		current.Spec.Helm.Maintenance != nil && *current.Spec.Helm.Maintenance == record.Request &&
		current.Spec.Suspended && current.Generation == record.RequestGeneration
	if !unrecordedStop && (saved == nil || saved.Request != record.Request || saved.RequestGeneration != record.RequestGeneration ||
		saved.IntentHash != record.IntentHash || !reflect.DeepEqual(saved.Target, record.Target)) {
		return fmt.Errorf("Helm maintenance operation snapshot changed")
	}
	provider, ok := r.CetiAdmin.(helmMaintenanceTargets)
	if !ok || provider == nil {
		return fmt.Errorf("Helm maintenance target authority is unavailable")
	}
	target, err := provider.MaintenanceTargetForAgent(ctx, current)
	if err != nil {
		return err
	}
	if target.Agent != record.Target.Agent || target.Namespace != record.Target.Namespace || target.Release != record.Target.Release ||
		target.Runtime != record.Target.Runtime || target.Team != record.Target.Team || target.Protected {
		return fmt.Errorf("Helm maintenance target identity or scope changed")
	}
	intent, err := r.maintenanceIntentHash(ctx, current)
	if err != nil {
		return err
	}
	if intent != record.IntentHash {
		return fmt.Errorf("Helm maintenance configuration changed during the handoff")
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: record.Target.Namespace.Name}, namespace); err != nil {
		return err
	}
	if !maintenanceMarkersMatch(namespace, record) {
		return fmt.Errorf("Helm maintenance startup marker identity changed")
	}
	if record.Restoration != nil && record.ApplyRevision != 0 {
		return r.maintenanceRestorationReady(ctx, current)
	}
	return nil
}

func (r *AgentReconciler) maintenanceRestorationReady(ctx context.Context, agent *api.Agent) error {
	authority, ok := r.CetiAdmin.(helmMaintenanceRestorationAuthority)
	if !ok || authority == nil {
		return fmt.Errorf("maintenance restoration admission authority is unavailable")
	}
	return authority.MaintenanceRestorationReady(ctx, agent)
}

func (r *AgentReconciler) maintenanceStatus(ctx context.Context, agent *api.Agent, request api.HelmMaintenanceRequest, phase api.HelmMaintenancePhase, revision int64, reason string) (ctrl.Result, bool, error) {
	agent.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: request.RequestID, ObservedGeneration: agent.Generation,
		Revision: revision, Phase: phase, Reason: reason, Message: reason}
	ready := metav1.ConditionFalse
	if cetiadmin.MaintenanceHeld(agent) {
		ready = metav1.ConditionTrue
	}
	setCondition(&agent.Status.Conditions, api.ConditionHelmMaintenanceReady, ready, reason, reason)
	err := r.updateStatus(ctx, agent)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, true, err
}

func (r *AgentReconciler) maintenanceBlocked(ctx context.Context, agent *api.Agent, request api.HelmMaintenanceRequest, reason string, cause error) (ctrl.Result, bool, error) {
	if request.RequestID == "" {
		setCondition(&agent.Status.Conditions, api.ConditionHelmMaintenanceReady, metav1.ConditionFalse, reason, reason)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, errors.Join(cause, r.updateStatus(ctx, agent))
	}
	result, handled, err := r.maintenanceStatus(ctx, agent, request, api.HelmMaintenancePhaseBlocked, 0, reason)
	return result, handled, errors.Join(cause, err)
}

// handled prevents the caller from entering any ordinary Helm/config/route
// path. The caller must invoke this before those paths, including suspension.
func (r *AgentReconciler) reconcileHelmMaintenance(ctx context.Context, agent *api.Agent) (ctrl.Result, bool, error) {
	if agent == nil || agent.UID == "" {
		return ctrl.Result{}, true, fmt.Errorf("Helm maintenance requires a current Agent UID")
	}
	current := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		return ctrl.Result{}, true, err
	}
	if current.UID != agent.UID || current.ResourceVersion != agent.ResourceVersion {
		return ctrl.Result{Requeue: true}, true, nil
	}
	var request api.HelmMaintenanceRequest
	if current.Spec.Helm.Maintenance != nil {
		request = *current.Spec.Helm.Maintenance
	} else if status := current.Status.HelmMaintenance; status != nil {
		request = api.HelmMaintenanceRequest{RequestID: status.RequestID, ExpectedRevision: status.Revision}
	}
	if current.Spec.Helm.Maintenance == nil && current.Status.HelmMaintenance == nil {
		// A lost status is not permission to bypass a retained namespace fence.
		namespace := &corev1.Namespace{}
		err := r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, namespace)
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, false, nil
		}
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if namespace.Annotations[cetiadmin.MaintenanceAgentName] == "" && namespace.Annotations[cetiadmin.MaintenanceAgentUID] == "" &&
			namespace.Annotations[cetiadmin.MaintenanceRequestID] == "" {
			return ctrl.Result{}, false, nil
		}
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceStatusLost", nil)
	}
	if current.DeletionTimestamp != nil || current.Spec.PlannedMigration != nil || current.Status.PlannedMigration != nil || sandboxRequested(current, nil) || current.Status.Sandbox != nil || current.Status.Policy != nil {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceTargetProtected", nil)
	}
	backend, maintenanceAvailable := r.HelmClient.(helmMaintenanceClient)
	object, record, err := r.readMaintenance(ctx, current)
	if err != nil {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceCheckpointUnavailable", err)
	}
	if record != nil && record.Resumed && current.Spec.Helm.Maintenance == nil {
		namespace := &corev1.Namespace{}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: record.Target.Namespace.Name}, namespace); err != nil {
			return r.maintenanceBlocked(ctx, current, record.Request, "MaintenanceNamespaceUnavailable", err)
		}
		if identityMatches(record.Target.Namespace, namespace) && namespace.Annotations[cetiadmin.MaintenanceAgentName] == "" &&
			namespace.Annotations[cetiadmin.MaintenanceAgentUID] == "" && namespace.Annotations[cetiadmin.MaintenanceRequestID] == "" {
			return r.completeHelmMaintenance(ctx, agent, current)
		}
	}
	provider, ok := r.CetiAdmin.(helmMaintenanceTargets)
	if !ok || provider == nil {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceScopeUnavailable", nil)
	}
	target, err := provider.MaintenanceTargetForAgent(ctx, current)
	if err != nil {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceTargetUnbound", err)
	}
	if target.Agent != identityOf(current) || target.Namespace.Name != instanceNamespace(current.Name) || target.Namespace.UID == "" ||
		target.Release != helmReleaseName(current.Name) || target.Protected {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceTargetUnbound", nil)
	}
	if current.Status.Namespace == "" || current.Status.HelmReleaseName == "" {
		// First-install failures can precede normal status publication. The
		// namespace/receipt above, not the proposed status, establishes ownership.
		current.Status.Namespace, current.Status.HelmReleaseName = target.Namespace.Name, target.Release
		if err := r.updateStatus(ctx, current); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	fresh := record != nil && current.Spec.Helm.Maintenance != nil && record.Phase == api.HelmMaintenancePhaseReleased && request.RequestID != record.Request.RequestID
	if record == nil || fresh {
		if current.Spec.Helm.Maintenance == nil || !current.Spec.Suspended || !api.ValidRestartRequestID(request.RequestID) || request.ExpectedRevision < 1 {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceRequestInvalid", nil)
		}
		if record == nil && current.Status.HelmMaintenance != nil && current.Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseRequested &&
			current.Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseBlocked {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceCheckpointLost", nil)
		}
		intent, err := r.maintenanceIntentHash(ctx, current)
		if err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceIntentUnavailable", err)
		}
		previousID := ""
		if fresh && !record.Resumed {
			if !maintenanceAvailable || current.Generation != record.ReleasedGeneration+1 {
				return r.maintenanceBlocked(ctx, current, request, "MaintenancePreviousReleaseUnconfirmed", nil)
			}
			rel, err := backend.CurrentRelease(ctx, record.Target.Namespace.Name, record.Target.Release)
			if err != nil || !maintenanceReleaseSettled(rel, record.Target) || rel.Info.Status != release.StatusDeployed || rel.Version != record.AppliedRevision ||
				rel.Chart == nil || rel.Chart.Metadata == nil || record.ValuesHash == "" || record.ValuesHash != maintenanceValuesHash(rel.Chart.Metadata.Version, rel.Config) {
				return r.maintenanceBlocked(ctx, current, request, "MaintenancePreviousReleaseUnconfirmed", err)
			}
			if applied, err := r.maintenanceApplied(ctx, rel, record.Target); err != nil || !applied {
				return r.maintenanceBlocked(ctx, current, request, "MaintenancePreviousReleaseUnconfirmed", err)
			}
			previousID = record.Request.RequestID
		}
		target.Resources, err = r.CetiAdminRuntimeResources(ctx, current, target)
		if err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceManifestUnavailable", err)
		}
		record = &helmMaintenanceRecord{Version: 1, Target: target, Request: request, RequestGeneration: current.Generation,
			IntentHash: intent, Phase: api.HelmMaintenancePhaseRequested, PreviousRequestID: previousID}
		if err := r.ensureMaintenanceMarkers(ctx, record); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceStartupFenceUnconfirmed", err)
		}
		if err := r.saveMaintenance(ctx, current, object, record); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceCheckpointUnconfirmed", errors.Join(err, r.stopMaintenanceTarget(ctx, current, record)))
		}
		object, _, err = r.readMaintenance(ctx, current)
		if err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceCheckpointUnavailable", err)
		}
	}
	request = record.Request
	if err := r.ensureMaintenanceMarkers(ctx, record); err != nil {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceStartupFenceUnconfirmed", err)
	}
	if err := r.maintenanceGuard(ctx, current, record); err != nil {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceIntentChanged", err)
	}
	if !maintenanceAvailable || backend == nil {
		var stopErr error
		if current.Spec.Suspended {
			stopErr = r.stopMaintenanceTarget(ctx, current, record)
		}
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceHelmUnavailable", stopErr)
	}
	if current.Spec.Helm.Maintenance != nil {
		if *current.Spec.Helm.Maintenance != record.Request || current.Generation != record.RequestGeneration || !current.Spec.Suspended || record.Resumed {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceRequestChanged", nil)
		}
		if record.Phase == api.HelmMaintenancePhaseHeld {
			// Native Helm may advance history and restore desired replicas here.
			// Keep the original handoff anchor; admission still denies every start.
			if err := r.maintenanceRestorationIdentities(ctx, current, record, false); err != nil {
				return r.maintenanceBlocked(ctx, current, request, "MaintenanceManifestUnbound", err)
			}
			stopped, err := maintenanceComputeStopped(ctx, r.customReader(), record.Target.Namespace.Name)
			if err != nil || !stopped {
				return r.maintenanceBlocked(ctx, current, request, "MaintenanceWriterUnconfirmed", err)
			}
			return r.maintenanceStatus(ctx, current, request, api.HelmMaintenancePhaseHeld, request.ExpectedRevision, "MaintenanceHeld")
		}
		if record.Phase != api.HelmMaintenancePhaseRequested {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceRequestChanged", nil)
		}
		if err := r.stopMaintenanceTarget(ctx, current, record); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceStopUnconfirmed", err)
		}
		if err := r.maintenanceQuiescent(ctx, record.Target); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceWriterUnconfirmed", err)
		}
		rel, err := backend.CurrentRelease(ctx, target.Namespace.Name, target.Release)
		if err != nil || !maintenanceReleaseCanBeHeld(rel, target) {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceOperationUnknown", err)
		}
		if int64(rel.Version) != request.ExpectedRevision {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceRevisionChanged", nil)
		}
		restoration, err := r.maintenanceHandoffManifest(ctx, rel, record.Target)
		if err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceManifestUnbound", err)
		}
		record.Restoration = restoration
		if err := maintenanceHooksStopped(ctx, r.customReader(), rel, record.Target); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceHookUnconfirmed", err)
		}
		stopped, err := maintenanceWritersStopped(ctx, r.customReader(), target.Namespace.Name)
		if err != nil || !stopped {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceWriterUnconfirmed", err)
		}
		if err := r.maintenanceGuard(ctx, current, record); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceIntentChanged", err)
		}
		record.Phase = api.HelmMaintenancePhaseHeld
		if err := r.saveMaintenance(ctx, current, object, record); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceCheckpointUnconfirmed", err)
		}
		return r.maintenanceStatus(ctx, current, request, api.HelmMaintenancePhaseHeld, request.ExpectedRevision, "MaintenanceHeld")
	}
	if !current.Spec.Suspended {
		result, handled, err := r.resumeHelmMaintenance(ctx, current, object, record, backend)
		if handled || err != nil {
			return result, handled, err
		}
		return r.completeHelmMaintenance(ctx, agent, current)
	}
	if record.Phase == api.HelmMaintenancePhaseHeld {
		if current.Generation != record.RequestGeneration+1 || current.Status.HelmMaintenance == nil ||
			current.Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseHeld || current.Status.HelmMaintenance.RequestID != request.RequestID ||
			current.Status.HelmMaintenance.ObservedGeneration != record.RequestGeneration || current.Status.HelmMaintenance.Revision != request.ExpectedRevision {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceReleaseIntentInvalid", nil)
		}
		record.Phase = api.HelmMaintenancePhaseReapplying
		if err := r.saveMaintenance(ctx, current, object, record); err != nil {
			return r.maintenanceBlocked(ctx, current, request, "MaintenanceCheckpointUnconfirmed", err)
		}
		r.customPlans.Delete(current.Name)
		current.Status.ObservedGeneration, current.Status.ObservedOperatorHash = 0, ""
		setCondition(&current.Status.Conditions, api.ConditionHelmDeployed, metav1.ConditionFalse, "MaintenanceReapplying", "Authoritative stopped configuration must be applied without hooks")
		return r.maintenanceStatus(ctx, current, request, api.HelmMaintenancePhaseReapplying, request.ExpectedRevision, "MaintenanceReapplying")
	}
	if (record.Phase != api.HelmMaintenancePhaseReapplying && record.Phase != api.HelmMaintenancePhaseReleased) || current.Generation != record.RequestGeneration+1 {
		return r.maintenanceBlocked(ctx, current, request, "MaintenanceReleaseIntentInvalid", nil)
	}
	return r.reapplyHelmMaintenance(ctx, current, object, record, backend)
}

func (r *AgentReconciler) completeHelmMaintenance(ctx context.Context, agent, current *api.Agent) (ctrl.Result, bool, error) {
	current.Status.HelmMaintenance = nil
	meta.RemoveStatusCondition(&current.Status.Conditions, api.ConditionHelmMaintenanceReady)
	if err := r.updateStatus(ctx, current); err != nil {
		return ctrl.Result{}, true, err
	}
	// Ordinary reconciliation continues with the status and resource version
	// just persisted, not the Released checkpoint it originally read.
	*agent = *current
	return ctrl.Result{}, false, nil
}

func maintenanceReleaseSettled(rel *release.Release, target cetiadmin.TargetIdentity) bool {
	return rel != nil && rel.Name == target.Release && rel.Namespace == target.Namespace.Name && rel.Version > 0 && rel.Info != nil &&
		(rel.Info.Status == release.StatusDeployed || rel.Info.Status == release.StatusFailed)
}

func maintenanceReleaseCanBeHeld(rel *release.Release, target cetiadmin.TargetIdentity) bool {
	if maintenanceReleaseSettled(rel, target) {
		return true
	}
	return rel != nil && rel.Name == target.Release && rel.Namespace == target.Namespace.Name && rel.Version > 0 && rel.Info != nil && rel.Info.Status.IsPending()
}

func (r *AgentReconciler) maintenanceHandoffManifest(ctx context.Context, rel *release.Release, target cetiadmin.TargetIdentity) (*helmMaintenanceRestoration, error) {
	objects, err := maintenanceManifest(rel.Manifest, target.Namespace.Name)
	if err != nil || len(objects) == 0 {
		return nil, fmt.Errorf("Helm release has no supported manifest")
	}
	restoration := &helmMaintenanceRestoration{Revision: rel.Version, ManifestHash: maintenanceManifestHash(rel.Manifest)}
	for i := range objects {
		wanted := &objects[i]
		ref, bound := maintenanceResource(target, wanted)
		actual := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: wanted.GetAPIVersion(), Kind: wanted.GetKind()}}
		err := r.customReader().Get(ctx, client.ObjectKeyFromObject(wanted), actual)
		if apierrors.IsNotFound(err) {
			if rel.Info.Status == release.StatusPendingInstall && maintenanceRestorableObject(wanted, false) {
				restoration.Objects = append(restoration.Objects, helmMaintenanceRestoreObject{ManagedResource: cetiadmin.ManagedResource{
					APIVersion: wanted.GetAPIVersion(), Kind: wanted.GetKind(), ObjectIdentity: cetiadmin.ObjectIdentity{Name: wanted.GetName(), Namespace: wanted.GetNamespace()}}, PreviousUID: ref.UID})
			} else if bound {
				return nil, fmt.Errorf("Helm maintenance bound object is missing and cannot be restored")
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if !bound || !identityMatches(ref.ObjectIdentity, actual) {
			return nil, fmt.Errorf("Helm maintenance manifest contains an existing unbound or replaced object")
		}
	}
	if len(restoration.Objects) == 0 {
		restoration = nil
	}
	probe := &helmMaintenanceRecord{Target: target, Restoration: restoration}
	if err := r.maintenanceRestorationIdentities(ctx, nil, probe, false); err != nil {
		return nil, err
	}
	return restoration, nil
}

func (r *AgentReconciler) maintenanceBoundManifest(ctx context.Context, rel *release.Release, target cetiadmin.TargetIdentity) error {
	if err := r.maintenanceResourceIdentities(ctx, target); err != nil {
		return err
	}
	objects, err := maintenanceManifest(rel.Manifest, target.Namespace.Name)
	if err != nil || len(objects) == 0 {
		return fmt.Errorf("Helm release has no supported bound manifest")
	}
	for i := range objects {
		ref, ok := maintenanceResource(target, &objects[i])
		if !ok {
			return fmt.Errorf("Helm manifest contains an unbound object")
		}
		actual := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: ref.APIVersion, Kind: ref.Kind}}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, actual); err != nil {
			return err
		}
		if !identityMatches(ref.ObjectIdentity, actual) {
			return fmt.Errorf("Helm manifest object UID changed")
		}
	}
	return nil
}

func maintenanceManifestHash(manifest string) string {
	hash := sha256.Sum256([]byte(manifest))
	return hex.EncodeToString(hash[:])
}

func maintenanceRestorableKind(version, kind string) bool {
	return version == "v1" && kind == "ConfigMap" || version == "apps/v1" && (kind == "StatefulSet" || kind == "Deployment")
}

func maintenanceRestorableObject(object *unstructured.Unstructured, stopped bool) bool {
	if !maintenanceRestorableKind(object.GetAPIVersion(), object.GetKind()) || object.GetNamespace() == "" ||
		object.GetName() == "" || object.GetGenerateName() != "" || object.GetAnnotations()["helm.sh/hook"] != "" {
		return false
	}
	if object.GetKind() == "ConfigMap" {
		return object.Object["immutable"] == nil || object.Object["immutable"] == false
	}
	if !stopped {
		return true
	}
	replicas, found, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
	return err == nil && found && replicas == 0
}

func validMaintenanceRestoration(record *helmMaintenanceRecord) error {
	restoration := record.Restoration
	if restoration == nil {
		return nil
	}
	digest, err := hex.DecodeString(restoration.ManifestHash)
	if err != nil || len(digest) != sha256.Size || restoration.Revision < 1 || int64(restoration.Revision) != record.Request.ExpectedRevision ||
		(record.ApplyRevision != 0 && record.ApplyRevision != restoration.Revision) || len(restoration.Objects) == 0 || len(restoration.Objects) > 256 {
		return fmt.Errorf("Helm maintenance restoration checkpoint is invalid")
	}
	seen := map[string]bool{}
	for _, ref := range restoration.Objects {
		key := ref.APIVersion + "/" + ref.Kind + "/" + ref.Name
		if !maintenanceRestorableKind(ref.APIVersion, ref.Kind) || ref.Name == "" || ref.Namespace != record.Target.Namespace.Name || ref.CreationID != "" || seen[key] ||
			(ref.UID != "" && ref.UID == ref.PreviousUID) {
			return fmt.Errorf("Helm maintenance restoration identity is invalid")
		}
		seen[key] = true
	}
	return nil
}

func maintenanceRestoreIdentity(record *helmMaintenanceRecord, version, kind, namespace, name string) *helmMaintenanceRestoreObject {
	if record.Restoration != nil {
		for i := range record.Restoration.Objects {
			ref := &record.Restoration.Objects[i]
			if ref.APIVersion == version && ref.Kind == kind && ref.Namespace == namespace && ref.Name == name {
				return ref
			}
		}
	}
	return nil
}

func (r *AgentReconciler) maintenanceRestorationIdentities(ctx context.Context, agent *api.Agent, record *helmMaintenanceRecord, allowCreated bool) error {
	for _, ref := range record.Target.Resources {
		restore := maintenanceRestoreIdentity(record, ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
		if restore != nil && restore.UID == "" && restore.PreviousUID == ref.UID {
			continue // Its recorded absence is checked below, never a name adoption.
		}
		actual := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: ref.APIVersion, Kind: ref.Kind}}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, actual); err != nil {
			return err
		}
		if !identityMatches(ref.ObjectIdentity, actual) {
			return fmt.Errorf("Helm maintenance bound object UID changed")
		}
	}
	if record.Restoration == nil {
		return nil
	}
	for _, ref := range record.Restoration.Objects {
		actual := &unstructured.Unstructured{}
		actual.SetAPIVersion(ref.APIVersion)
		actual.SetKind(ref.Kind)
		err := r.customReader().Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, actual)
		if apierrors.IsNotFound(err) && ref.UID == "" {
			continue
		}
		if err != nil {
			return err
		}
		if ref.UID != "" {
			if !identityMatches(ref.ObjectIdentity, actual) {
				return fmt.Errorf("Helm maintenance restored object UID changed")
			}
			continue
		}
		// Before the durable grant there must still be no object. After it,
		// namespace admission permits only this coordinated Operator creation.
		if !allowCreated || !maintenanceCreatedRestorationMatches(actual, ref, agent, record) {
			return fmt.Errorf("Helm maintenance cannot adopt an existing restoration name")
		}
	}
	return nil
}

func maintenanceCreatedRestorationMatches(actual *unstructured.Unstructured, ref helmMaintenanceRestoreObject, agent *api.Agent, record *helmMaintenanceRecord) bool {
	return agent != nil && record.Restoration != nil && record.Phase == api.HelmMaintenancePhaseReapplying && record.ApplyRevision == record.Restoration.Revision &&
		actual.GetName() == ref.Name && actual.GetNamespace() == ref.Namespace && actual.GetAPIVersion() == ref.APIVersion && actual.GetKind() == ref.Kind &&
		actual.GetUID() != "" && actual.GetUID() != ref.PreviousUID && actual.GetDeletionTimestamp() == nil && maintenanceRestorableObject(actual, true) &&
		actual.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" && actual.GetAnnotations()["meta.helm.sh/release-name"] == record.Target.Release &&
		actual.GetAnnotations()["meta.helm.sh/release-namespace"] == record.Target.Namespace.Name &&
		(len(actual.GetOwnerReferences()) == 0 || hasAgentOwner(actual, agent))
}

func (r *AgentReconciler) maintenanceRestorationManifest(ctx context.Context, agent *api.Agent, rel *release.Release, record *helmMaintenanceRecord) error {
	if record.Restoration == nil {
		return r.maintenanceBoundManifest(ctx, rel, record.Target)
	}
	if record.ApplyRevision != 0 {
		return r.maintenanceRestorationIdentities(ctx, agent, record, true)
	}
	if rel.Version != record.Restoration.Revision || maintenanceManifestHash(rel.Manifest) != record.Restoration.ManifestHash {
		return fmt.Errorf("pending-install restoration requires the original repaired manifest and revision")
	}
	objects, err := maintenanceManifest(rel.Manifest, record.Target.Namespace.Name)
	if err != nil || len(objects) == 0 {
		return fmt.Errorf("Helm restoration manifest cannot be read")
	}
	for i := range objects {
		object := &objects[i]
		if restore := maintenanceRestoreIdentity(record, object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName()); restore != nil {
			if !maintenanceRestorableObject(object, false) || object.GetUID() != "" || object.GetResourceVersion() != "" {
				return fmt.Errorf("Helm restoration contains a non-restorable original object")
			}
		} else if _, bound := maintenanceResource(record.Target, object); !bound {
			return fmt.Errorf("Helm manifest contains an unbound non-restorable object")
		}
	}
	return r.maintenanceRestorationIdentities(ctx, agent, record, false)
}

func (r *AgentReconciler) recordMaintenanceRestoredUIDs(ctx context.Context, agent *api.Agent, checkpoint *corev1.ConfigMap, record *helmMaintenanceRecord) error {
	if record.Restoration == nil {
		return nil
	}
	if err := r.maintenanceGuard(ctx, agent, record); err != nil {
		return err
	}
	if err := r.maintenanceRestorationIdentities(ctx, agent, record, true); err != nil {
		return err
	}
	changed := false
	for i := range record.Restoration.Objects {
		ref := &record.Restoration.Objects[i]
		if ref.UID != "" {
			continue
		}
		actual := &unstructured.Unstructured{}
		actual.SetAPIVersion(ref.APIVersion)
		actual.SetKind(ref.Kind)
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, actual); err != nil {
			return err
		}
		if !maintenanceCreatedRestorationMatches(actual, *ref, agent, record) {
			return fmt.Errorf("restored object has no current attributable API UID")
		}
		ref.UID = actual.GetUID()
		record.Target.Resources = slices.DeleteFunc(record.Target.Resources, func(old cetiadmin.ManagedResource) bool {
			return old.APIVersion == ref.APIVersion && old.Kind == ref.Kind && old.Name == ref.Name && old.Namespace == ref.Namespace
		})
		record.Target.Resources = append(record.Target.Resources, ref.ManagedResource)
		changed = true
	}
	if !changed {
		return nil
	}
	sort.Slice(record.Target.Resources, func(i, j int) bool {
		a, b := record.Target.Resources[i], record.Target.Resources[j]
		return a.APIVersion+"/"+a.Kind+"/"+a.Name < b.APIVersion+"/"+b.Kind+"/"+b.Name
	})
	return r.saveMaintenance(ctx, agent, checkpoint, record)
}

func (r *AgentReconciler) maintenanceResourceIdentities(ctx context.Context, target cetiadmin.TargetIdentity) error {
	for _, ref := range target.Resources {
		actual := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: ref.APIVersion, Kind: ref.Kind}}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, actual); err != nil {
			return err
		}
		if !identityMatches(ref.ObjectIdentity, actual) {
			return fmt.Errorf("Helm maintenance bound object UID changed")
		}
	}
	return nil
}

func (r *AgentReconciler) stopMaintenanceTarget(ctx context.Context, agent *api.Agent, record *helmMaintenanceRecord) error {
	if err := r.maintenanceGuard(ctx, agent, record); err != nil {
		return err
	}
	// Compute stop remains independent of the Gateway's availability.
	keyErr := r.suspendGatewayKey(ctx, agent)
	if keyErr == nil && aiGatewayKeyWasIssued(agent) {
		_, _, keyErr = r.maintenanceGateway(ctx, agent, false)
	}
	var failures []error
	for _, ref := range record.Target.Resources {
		var object client.Object
		switch {
		case ref.APIVersion == "apps/v1" && ref.Kind == "StatefulSet":
			object = &appsv1.StatefulSet{}
		case ref.APIVersion == "apps/v1" && ref.Kind == "Deployment":
			object = &appsv1.Deployment{}
		default:
			continue
		}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, object); apierrors.IsNotFound(err) {
			// Absence is a stop observation, not permission to replace the UID.
			// Only the pending-install manifest inventory below may permit restore.
			continue
		} else if err != nil {
			failures = append(failures, err)
			continue
		}
		if !identityMatches(ref.ObjectIdentity, object) || !attributableRuntimeObject(object, agent, record.Target.Release) {
			failures = append(failures, fmt.Errorf("Helm maintenance writer ownership changed"))
			continue
		}
		before := object.DeepCopyObject().(client.Object)
		switch workload := object.(type) {
		case *appsv1.StatefulSet:
			workload.Spec.Replicas = ptr.To[int32](0)
			if workload.Spec.PersistentVolumeClaimRetentionPolicy == nil {
				workload.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType}
			}
			workload.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
		case *appsv1.Deployment:
			workload.Spec.Replicas = ptr.To[int32](0)
		}
		if !reflect.DeepEqual(before, object) {
			if err := r.maintenanceGuard(ctx, agent, record); err != nil {
				failures = append(failures, err)
				break
			}
			failures = append(failures, r.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
		}
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		failures = append(failures, r.removeCustomPlatformRoute(ctx, agent))
		_, completion, err := r.readCustomCompletion(ctx, agent)
		if err == nil && completion != nil && completion.Outcome == CustomApplyUnknown {
			err = fmt.Errorf("Custom apply outcome is unknown")
		} else if err == nil {
			err = r.retryCustomCompletion(ctx, agent)
		}
		failures = append(failures, err)
		if provider, ok := r.CustomBindings.(*KubernetesCustomBindings); ok && provider != nil {
			_, operation, err := provider.readOperation(ctx, agent)
			if apierrors.IsNotFound(err) {
				err = nil
			} else if err == nil && !operation.Finished {
				err = fmt.Errorf("Custom apply still owns storage coordination")
			}
			failures = append(failures, err)
		} else {
			failures = append(failures, fmt.Errorf("Custom maintenance coordination owner is unavailable"))
		}
	} else {
		failures = append(failures, r.removeRegistryBuiltinRoute(ctx, agent))
	}
	failures = append(failures, r.removeRegistryCredentials(ctx, agent))
	agent.Status.AccessURL, agent.Status.Phase, agent.Status.ObservedGeneration = "", api.PhaseSuspended, 0
	for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionIngressReady} {
		setCondition(&agent.Status.Conditions, condition, metav1.ConditionFalse, "HelmMaintenance", "Stopped for the current Helm maintenance request")
	}
	return errors.Join(append(failures, keyErr)...)
}

// A native rollback may restore replica intent while the namespace remains
// fenced. Held observation checks actual compute, not that historical intent.
func maintenanceComputeStopped(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	pods, sets, deployments, replicas, jobs, daemons := &corev1.PodList{}, &appsv1.StatefulSetList{}, &appsv1.DeploymentList{}, &appsv1.ReplicaSetList{}, &batchv1.JobList{}, &appsv1.DaemonSetList{}
	for _, list := range []client.ObjectList{pods, sets, deployments, replicas, jobs, daemons} {
		if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return false, err
		}
	}
	for i := range pods.Items {
		if !maintenancePodStopped(&pods.Items[i]) {
			return false, nil
		}
	}
	for _, set := range sets.Items {
		if set.DeletionTimestamp != nil || set.Status.Replicas != 0 || set.Status.CurrentReplicas != 0 || set.Status.ReadyReplicas != 0 || set.Status.UpdatedReplicas != 0 {
			return false, nil
		}
	}
	for _, deploy := range deployments.Items {
		if deploy.DeletionTimestamp != nil || deploy.Status.Replicas != 0 || deploy.Status.ReadyReplicas != 0 || deploy.Status.AvailableReplicas != 0 || deploy.Status.UpdatedReplicas != 0 {
			return false, nil
		}
	}
	for _, set := range replicas.Items {
		if set.DeletionTimestamp != nil || set.Status.Replicas != 0 || set.Status.ReadyReplicas != 0 || set.Status.AvailableReplicas != 0 {
			return false, nil
		}
	}
	for _, job := range jobs.Items {
		if job.DeletionTimestamp != nil || job.Status.Active != 0 || !maintenanceJobTerminal(&job) {
			return false, nil
		}
	}
	return len(daemons.Items) == 0, nil
}

func (r *AgentReconciler) reapplyHelmMaintenance(ctx context.Context, agent *api.Agent, object *corev1.ConfigMap, record *helmMaintenanceRecord, backend helmMaintenanceClient) (ctrl.Result, bool, error) {
	request := record.Request
	if record.Phase != api.HelmMaintenancePhaseReleased {
		if err := r.stopMaintenanceTarget(ctx, agent, record); err != nil {
			return r.maintenanceBlocked(ctx, agent, request, "MaintenanceStopUnconfirmed", err)
		}
	}
	prepared, err := r.prepareHelmMaintenance(ctx, agent, record)
	if err != nil {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceConfigurationUnavailable", err)
	}
	rel, err := backend.CurrentRelease(ctx, record.Target.Namespace.Name, record.Target.Release)
	if err != nil || !maintenanceReleaseSettled(rel, record.Target) {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceOperationUnknown", err)
	}
	if err := r.maintenanceRestorationManifest(ctx, agent, rel, record); err != nil {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceManifestUnbound", err)
	}
	if err := maintenanceHooksStopped(ctx, r.customReader(), rel, record.Target); err != nil {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceHookUnconfirmed", err)
	}
	if record.ApplyRevision == 0 {
		if record.Restoration != nil {
			if err := r.maintenanceRestorationReady(ctx, agent); err != nil {
				return r.maintenanceBlocked(ctx, agent, request, "MaintenanceRestorationAdmissionUnavailable", err)
			}
		}
		if status := agent.Status.HelmMaintenance; record.Restoration != nil &&
			(status == nil || status.Phase != api.HelmMaintenancePhaseReapplying || status.ObservedGeneration != agent.Generation || status.Revision != int64(rel.Version)) {
			// A prerequisite failure did not submit an SDK attempt. Publish the
			// current Reapplying state before admitting that first restoration.
			return r.maintenanceStatus(ctx, agent, request, api.HelmMaintenancePhaseReapplying, int64(rel.Version), "MaintenanceReapplying")
		}
		stopped, err := maintenanceWritersStopped(ctx, r.customReader(), record.Target.Namespace.Name)
		if err != nil || !stopped {
			return r.maintenanceBlocked(ctx, agent, request, "MaintenanceWriterUnconfirmed", err)
		}
		if err := r.maintenanceGuard(ctx, agent, record); err != nil {
			return r.maintenanceBlocked(ctx, agent, request, "MaintenanceIntentChanged", err)
		}
		if err := r.maintenanceQuiescent(ctx, record.Target); err != nil {
			return r.maintenanceBlocked(ctx, agent, request, "MaintenanceWriterUnconfirmed", err)
		}
		record.ApplyRevision, record.PreparedHash = rel.Version, maintenancePreparedHash(prepared)
		record.ValuesHash = maintenanceValuesHash(prepared.ChartVersion, prepared.Values)
		if err := r.saveMaintenance(ctx, agent, object, record); err != nil {
			return r.maintenanceBlocked(ctx, agent, request, "MaintenanceCheckpointUnconfirmed", err)
		}
		guard := func(ctx context.Context) error {
			if err := r.maintenanceGuard(ctx, agent, record); err != nil {
				return err
			}
			if err := r.maintenanceRestorationIdentities(ctx, agent, record, true); err != nil {
				return err
			}
			stopped, err := maintenanceComputeStopped(ctx, r.customReader(), record.Target.Namespace.Name)
			if err != nil {
				return err
			}
			if !stopped {
				return fmt.Errorf("Helm maintenance writers are not stopped")
			}
			return nil
		}
		if err := backend.InstallOrUpgradeMaintenance(ctx, record.Target.Namespace.Name, record.Target.Release, prepared, record.ApplyRevision, guard); err != nil {
			if errors.Is(err, helm.ErrMaintenanceNotSubmitted) {
				record.ApplyRevision, record.PreparedHash, record.ValuesHash = 0, "", ""
				err = errors.Join(err, r.saveMaintenance(ctx, agent, object, record))
			}
			// The durable attempt prevents an uncertain SDK result from submitting
			// a second revision. A later pass can only recognize its actual result.
			return r.maintenanceBlocked(ctx, agent, request, "MaintenanceApplyUnconfirmed", err)
		}
		rel, err = backend.CurrentRelease(ctx, record.Target.Namespace.Name, record.Target.Release)
		if err != nil {
			return r.maintenanceBlocked(ctx, agent, request, "MaintenanceApplyUnconfirmed", err)
		}
	}
	if record.PreparedHash != maintenancePreparedHash(prepared) || !maintenancePreparedRelease(rel, prepared, record) {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceApplyUnconfirmed", nil)
	}
	if err := r.recordMaintenanceRestoredUIDs(ctx, agent, object, record); err != nil {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceRestorationReadbackUnconfirmed", err)
	}
	applied, err := r.maintenanceApplied(ctx, rel, record.Target)
	if err != nil {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceReadbackUnavailable", err)
	}
	if !applied {
		return r.maintenanceStatus(ctx, agent, request, api.HelmMaintenancePhaseReapplying, int64(rel.Version), "MaintenanceReadbackPending")
	}
	if err := r.maintenanceGuard(ctx, agent, record); err != nil {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceIntentChanged", err)
	}
	record.Phase, record.AppliedRevision, record.ReleasedGeneration = api.HelmMaintenancePhaseReleased, rel.Version, agent.Generation
	if err := r.saveMaintenance(ctx, agent, object, record); err != nil {
		return r.maintenanceBlocked(ctx, agent, request, "MaintenanceCheckpointUnconfirmed", err)
	}
	return r.maintenanceStatus(ctx, agent, request, api.HelmMaintenancePhaseReleased, int64(rel.Version), "MaintenanceReleased")
}

func maintenancePreparedRelease(rel *release.Release, prepared *helm.PreparedRelease, record *helmMaintenanceRecord) bool {
	if !maintenanceReleaseSettled(rel, record.Target) || rel.Info.Status != release.StatusDeployed || rel.Version != record.ApplyRevision+1 ||
		rel.Chart == nil || rel.Chart.Metadata == nil || rel.Chart.Metadata.Version != prepared.ChartVersion {
		return false
	}
	expected, expectedErr := json.Marshal(prepared.Values)
	actual, actualErr := json.Marshal(rel.Config)
	return expectedErr == nil && actualErr == nil && string(expected) == string(actual)
}

func maintenancePreparedHash(prepared *helm.PreparedRelease) string {
	raw, err := json.Marshal([]any{prepared.ChartVersion, prepared.CacheKey(), prepared.Values})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func maintenanceValuesHash(version string, values map[string]any) string {
	raw, err := json.Marshal([]any{version, values})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (r *AgentReconciler) maintenanceApplied(ctx context.Context, rel *release.Release, target cetiadmin.TargetIdentity) (bool, error) {
	if err := r.maintenanceBoundManifest(ctx, rel, target); err != nil {
		return false, err
	}
	objects, err := maintenanceManifest(rel.Manifest, target.Namespace.Name)
	if err != nil {
		return false, err
	}
	for i := range objects {
		want := &objects[i]
		// Native credentials are validated by their capability owners. Neither
		// their plaintext nor the Helm Secret payload enters the checkpoint.
		if want.GetKind() == "Secret" {
			continue
		}
		if typed, err := r.Scheme.New(want.GroupVersionKind()); err == nil {
			// Typed serialization normalizes quantities (8000m == 8) and
			// omitted zero values just as the API does, without discarding fields.
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(want.Object, typed); err != nil {
				return false, fmt.Errorf("maintenance desired object cannot be decoded")
			}
			normalized, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
			if err != nil {
				return false, err
			}
			want.Object = normalized
		}
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(want.GroupVersionKind())
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(want), actual); err != nil {
			return false, err
		}
		ref, _ := maintenanceResource(target, want)
		if !identityMatches(ref.ObjectIdentity, actual) {
			return false, fmt.Errorf("Helm maintenance object identity changed during readback")
		}
		// Compare declarative fields, allowing API defaults. Server status and
		// unrelated metadata remain owned by their existing writers.
		for key, value := range want.Object {
			if key == "metadata" || key == "status" || key == "apiVersion" || key == "kind" {
				continue
			}
			if !equality.Semantic.DeepDerivative(value, actual.Object[key]) {
				return false, nil
			}
		}
		for key, value := range want.GetLabels() {
			if actual.GetLabels()[key] != value {
				return false, nil
			}
		}
		for key, value := range want.GetAnnotations() {
			if actual.GetAnnotations()[key] != value {
				return false, nil
			}
		}
	}
	return maintenanceWritersStopped(ctx, r.customReader(), target.Namespace.Name)
}

func (r *AgentReconciler) resumeHelmMaintenance(ctx context.Context, agent *api.Agent, object *corev1.ConfigMap, record *helmMaintenanceRecord, backend helmMaintenanceClient) (ctrl.Result, bool, error) {
	status := agent.Status.HelmMaintenance
	if record.Phase != api.HelmMaintenancePhaseReleased || record.ReleasedGeneration < 1 || agent.Generation != record.ReleasedGeneration+1 ||
		status == nil || (!record.Resumed && (status.Phase != api.HelmMaintenancePhaseReleased || status.ObservedGeneration != record.ReleasedGeneration)) ||
		status.RequestID != record.Request.RequestID || record.AppliedRevision != record.ApplyRevision+1 {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceResumeIntentInvalid", nil)
	}
	if _, _, err := r.maintenanceGateway(ctx, agent, true); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceResumeBlocked", err)
	}
	stopped := agent.DeepCopy()
	stopped.Spec.Suspended, stopped.Generation = true, record.ReleasedGeneration
	prepared, err := r.prepareHelmMaintenance(ctx, stopped, record)
	if err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceResumeConfigurationUnavailable", err)
	}
	rel, err := backend.CurrentRelease(ctx, record.Target.Namespace.Name, record.Target.Release)
	if err != nil || !maintenancePreparedRelease(rel, prepared, record) || record.PreparedHash != maintenancePreparedHash(prepared) {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceResumeReadbackChanged", err)
	}
	if err := maintenanceHooksStopped(ctx, r.customReader(), rel, record.Target); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceHookUnconfirmed", err)
	}
	if applied, err := r.maintenanceApplied(ctx, rel, record.Target); err != nil || !applied {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceResumeReadbackChanged", err)
	}
	if err := r.maintenanceGuard(ctx, agent, record); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceIntentChanged", err)
	}
	if err := r.maintenanceQuiescent(ctx, record.Target); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceWriterUnconfirmed", err)
	}
	// Persist the separately observed resume before removing the fence. A lost
	// patch reply retries current evidence; it never starts work from a timer.
	record.Resumed = true
	if err := r.saveMaintenance(ctx, agent, object, record); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceCheckpointUnconfirmed", err)
	}
	namespace := &corev1.Namespace{}
	key := client.ObjectKey{Name: record.Target.Namespace.Name}
	if err := r.customReader().Get(ctx, key, namespace); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceNamespaceUnavailable", err)
	}
	if !maintenanceMarkersMatch(namespace, record) {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceStartupFenceUnconfirmed", nil)
	}
	before := namespace.DeepCopy()
	for _, key := range []string{cetiadmin.MaintenanceAgentName, cetiadmin.MaintenanceAgentUID, cetiadmin.MaintenanceRequestID} {
		delete(namespace.Annotations, key)
	}
	if err := r.maintenanceGuard(ctx, agent, record); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceIntentChanged", err)
	}
	if err := r.Patch(ctx, namespace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceResumeFenceUnconfirmed", err)
	}
	if err := r.customReader().Get(ctx, key, namespace); err != nil {
		return ctrl.Result{}, true, err
	}
	if !identityMatches(record.Target.Namespace, namespace) || namespace.Annotations[cetiadmin.MaintenanceAgentName] != "" ||
		namespace.Annotations[cetiadmin.MaintenanceAgentUID] != "" || namespace.Annotations[cetiadmin.MaintenanceRequestID] != "" {
		return r.maintenanceBlocked(ctx, agent, record.Request, "MaintenanceResumeFenceUnconfirmed", nil)
	}
	r.customPlans.Delete(agent.Name)
	return ctrl.Result{}, false, nil
}

func (r *AgentReconciler) maintenanceGateway(ctx context.Context, agent *api.Agent, resuming bool) (*corev1.Secret, *aigateway.Key, error) {
	status := agent.Status.AIGateway
	if !r.AIGateway.Enabled || status == nil || status.KeyID == "" || status.KeySecretName != aiGatewayKeySecretName {
		return nil, nil, fmt.Errorf("maintenance requires the retained AI Gateway key binding")
	}
	secret := &corev1.Secret{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: aiGatewayKeySecretName}, secret); err != nil {
		return nil, nil, fmt.Errorf("maintenance Gateway key reference is unavailable")
	}
	value := string(secret.Data[aiGatewayKeySecretField])
	if secret.UID == "" || secret.DeletionTimestamp != nil || !hasAgentOwner(secret, agent) ||
		secret.Labels[aiGatewayAgentUIDLabel] != string(agent.UID) || !aigateway.ValidKeyValue(value) {
		return nil, nil, fmt.Errorf("maintenance Gateway key ownership is invalid")
	}
	token := aigateway.Token(value)
	if token[:aiGatewayKeyIDLength] != status.KeyID {
		return nil, nil, fmt.Errorf("maintenance Gateway key identity changed")
	}
	gateway, err := r.aiGatewayClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	key, err := gateway.GetKey(ctx, token)
	if err != nil || key == nil || key.Alias != aiGatewayKeyAlias(agent) || !key.Blocked {
		return nil, nil, fmt.Errorf("maintenance Gateway block is not confirmed")
	}
	if resuming && !aiGatewayOperatorOwnedBlock(status.Reason) {
		return nil, nil, fmt.Errorf("external Gateway block requires its owner's recovery")
	}
	return secret, key, nil
}

func (r *AgentReconciler) prepareHelmMaintenance(ctx context.Context, agent *api.Agent, record *helmMaintenanceRecord) (*helm.PreparedRelease, error) {
	if !agent.Spec.Suspended || agent.Spec.Helm.Maintenance != nil || agent.Spec.Agent.Image == nil || agent.Spec.Helm.Version == "" {
		return nil, fmt.Errorf("maintenance reapplication requires suspended, paired Agent runtime pins")
	}
	current := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		return nil, err
	}
	if err := r.maintenanceGuard(ctx, current, record); err != nil {
		return nil, err
	}
	var team *api.Team
	if agent.Spec.Team != "" {
		team = &api.Team{}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
			return nil, err
		}
	}
	secret, key, err := r.maintenanceGateway(ctx, agent, false)
	if err != nil {
		return nil, err
	}
	if key.TeamID != aiGatewayTeamID(agent.Spec.Team) || key.UserID != agent.Spec.User.Alias ||
		!aigateway.KeyRoutesCurrent(key.AllowedRoutes) || !slices.Equal(sortedCopy(key.Models), sortedCopy(aiGatewayKeyModels(agent.Status.AIGateway.AccessGroups))) ||
		len(key.Models) == 0 || slices.Contains(key.Models, aigateway.NoModels) {
		return nil, fmt.Errorf("maintenance Gateway grant must match its current retained owner and models")
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		return r.prepareCustomHelmMaintenance(ctx, agent, team, secret, record)
	}
	values, err := buildHelmValuesWithAgentIdentity(agent, team, r.DomainSuffix, r.SkillsStorageAccount, r.CetiRunHelmValues, r.ChartRegistry, r.DefaultModel, AgentIdentityHelmConfig{
		Enabled: r.AgentIdentityEnabled, BrokerURL: r.AgentIdentityBrokerURL, ConnectionsMCPURL: r.ConnectionsMCPURL,
		ConnectionsMCPAudience: r.ConnectionsMCPAudience, ConnectionsMCPCASecretName: r.ConnectionsMCPCASecretName,
		ConnectionsMCPRuntimeTypes: r.ConnectionsMCPRuntimeTypes,
	})
	if err != nil {
		return nil, err
	}
	namespace, port, err := aiGatewayEndpoint(r.AIGateway.URL)
	if err != nil {
		return nil, err
	}
	entries, defaultModel, _ := r.aiGatewayModelEntries(agent)
	if len(entries) == 0 || defaultModel == "" {
		return nil, fmt.Errorf("maintenance has no confirmed model configuration")
	}
	models := make([]any, 0, len(entries))
	for _, entry := range entries {
		model := map[string]any{"id": entry.ID, "name": entry.Name}
		if entry.ContextWindow > 0 {
			model["contextWindow"] = entry.ContextWindow
		}
		if entry.MaxTokens > 0 {
			model["maxTokens"] = entry.MaxTokens
		}
		models = append(models, model)
	}
	// The stopped projection is based on the key read above, not Routed status
	// or inert chart preflight values. It neither provisions nor unblocks a key.
	values["secrets"] = deepMerge(values["secrets"], map[string]any{"provider": "openai", "authMode": "api_key",
		"apiBase": r.AIGateway.URL + "/v1", "apiKey": "", "model": defaultModel})
	proxy := map[string]any{"enabled": false}
	if r.aiGatewayCopilotWanted(agent) && agent.Status.AIGateway.Copilot != nil {
		if agent.Status.AIGateway.Copilot.ProxyKeyID == "" {
			return nil, fmt.Errorf("maintenance Copilot credential binding is unavailable")
		}
		proxy = map[string]any{"enabled": true, "image": r.AIGateway.CopilotProxyImage,
			"masterKeySecretName": aiGatewayProxySecretName, "tokenSecretName": api.CopilotTokenSecretName, "replicaCount": 0}
	}
	values["aiGateway"] = map[string]any{"enabled": true, "keySecretName": secret.Name, "keySecretKey": aiGatewayKeySecretField,
		"models": models, "copilotProxy": proxy}
	values["networkPolicy"] = deepMerge(values["networkPolicy"], map[string]any{"gatewayNamespace": namespace, "gatewayPort": port})
	values["memoryOptions"] = r.memoryOptions()
	native := &corev1.Secret{}
	if agent.Spec.GatewayTokenSecretRef == "" || r.customReader().Get(ctx, client.ObjectKey{Namespace: record.Target.Namespace.Name, Name: agent.Spec.GatewayTokenSecretRef}, native) != nil ||
		!attributableRuntimeObject(native, agent, record.Target.Release) {
		return nil, fmt.Errorf("maintenance native authentication binding is unavailable")
	}
	values["agentRequest"].(map[string]any)["tailscaleEnabled"] = len(native.Data[api.TailscaleAuthKeyField]) > 0
	values["agentRequest"].(map[string]any)["releaseName"] = record.Target.Release
	values["podAnnotations"] = deepMerge(values["podAnnotations"], map[string]any{
		aiGatewayKeyIDPodAnnotation: agent.Status.AIGateway.KeyID, gatewaySecretResourceVersionAnnotation: native.ResourceVersion,
	})
	if enabled, err := r.connectionsMCPEnabledFor(agent); err != nil {
		return nil, err
	} else if enabled {
		hash, err := r.connectionsMCPCAHash(ctx)
		if err != nil {
			return nil, err
		}
		values["podAnnotations"] = deepMerge(values["podAnnotations"], map[string]any{"cetirun.microsoft.com/connections-ca-hash": hash})
	}
	prepared, err := r.HelmClient.Prepare(record.Target.Namespace.Name, values, agent.Spec.Helm.Version)
	if err != nil {
		return nil, err
	}
	r.applyCopilotChartValues(values, agent, prepared)
	return r.HelmClient.Refresh(prepared, values)
}

func (r *AgentReconciler) prepareCustomHelmMaintenance(ctx context.Context, agent *api.Agent, team *api.Team, model *corev1.Secret, record *helmMaintenanceRecord) (*helm.PreparedRelease, error) {
	provider, ok := r.CustomBindings.(*KubernetesCustomBindings)
	if !ok || provider == nil {
		return nil, fmt.Errorf("maintenance requires the ordinary Custom capability owner")
	}
	if err := provider.authorize(agent); err != nil {
		return nil, err
	}
	values, err := r.customOverrides(agent, team)
	if err != nil {
		return nil, err
	}
	receipt, err := r.readCustomOwnership(ctx, agent)
	if err != nil {
		return nil, err
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: receipt.Namespace}, namespace); err != nil {
		return nil, err
	}
	if namespace.UID != receipt.NamespaceUID || !maintenanceMarkersMatch(namespace, record) {
		return nil, fmt.Errorf("maintenance Custom namespace identity changed")
	}
	claim := &runtimebindings.HomeClaim{StorageClass: provider.config.StorageClass, StorageSize: provider.config.StorageSize, AccessMode: provider.config.AccessMode}
	if !claim.Matches(&corev1.PersistentVolumeClaim{Spec: receipt.HomeClaim.PVCSpec()}) {
		return nil, fmt.Errorf("maintenance cannot change the retained Custom HOME claim")
	}
	files, data := provider.config.contextFiles(), map[string]string{}
	for key, path := range files {
		data[key] = provider.config.Context[path]
		if path == "AGENTS.md" && team != nil {
			data[key] += "\n\n## Team context\n" + team.Spec.MemberContext.Role + "\n" + strings.Join(team.Spec.MemberContext.Description, "\n")
		}
	}
	raw, _ := json.Marshal(data)
	sum := sha256.Sum256(raw)
	snapshot := &corev1.ConfigMap{}
	contextKey := client.ObjectKey{Namespace: namespace.Name, Name: "custom-context-" + hex.EncodeToString(sum[:16])}
	if err := r.customReader().Get(ctx, contextKey, snapshot); err != nil {
		// Context preparation normally precedes the handoff. Do not resurrect a
		// deleted snapshot or change HOME ownership during native maintenance.
		return nil, fmt.Errorf("maintenance Custom context snapshot is unavailable")
	}
	if !hasAgentOwner(snapshot, agent) || !ptr.Deref(snapshot.Immutable, false) || snapshot.DeletionTimestamp != nil || !reflect.DeepEqual(snapshot.Data, data) {
		return nil, fmt.Errorf("maintenance Custom context snapshot changed")
	}
	gatewayNamespace, gatewayPort, err := aiGatewayEndpoint(r.AIGateway.URL)
	if err != nil {
		return nil, err
	}
	endpoint, _ := url.Parse(r.AIGateway.URL)
	service := &corev1.Service{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: gatewayNamespace, Name: strings.Split(endpoint.Hostname(), ".")[0]}, service); err != nil {
		return nil, err
	}
	selector := map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway"}
	if service.UID == "" || service.DeletionTimestamp != nil || service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ClusterIP == "" || service.Spec.ClusterIP == "None" ||
		!slices.ContainsFunc(service.Spec.Ports, func(port corev1.ServicePort) bool {
			return port.Port == int32(gatewayPort) && port.Protocol == corev1.ProtocolTCP
		}) {
		return nil, fmt.Errorf("maintenance Custom Gateway Service is unavailable")
	}
	for key, value := range selector {
		if service.Spec.Selector[key] != value {
			return nil, fmt.Errorf("maintenance Custom Gateway Service ownership changed")
		}
	}
	bindings := runtimebindings.Custom{AgentName: agent.Name, AgentUID: string(agent.UID), Generation: agent.Generation,
		RestartRequestID: agent.Spec.RestartRequestID, Namespace: objectReference(namespace), ApprovedType: "custom", ApprovedImage: *agent.Spec.Agent.Image,
		PolicyRevision: provider.revision, Home: runtimebindings.Home{Name: receipt.Home, FSGroup: provider.config.FSGroup, Claim: receipt.HomeClaim},
		Context: runtimebindings.Context{ObjectReference: objectReference(snapshot), Files: files},
		Model:   runtimebindings.ModelAccess{ObjectReference: objectReference(model), Key: aiGatewayKeySecretField, BaseURL: r.AIGateway.URL + "/v1"},
		Network: runtimebindings.Network{Revision: provider.revision + ":gateway:" + string(service.UID), Gateway: runtimebindings.Peer{Namespace: gatewayNamespace, PodLabels: selector},
			GatewayPort: int32(gatewayPort), DNS: runtimebindings.Peer{Namespace: "kube-system", PodLabels: map[string]string{"k8s-app": "kube-dns"}}}}
	if agent.Spec.Agent.RegistrySecretRef != nil {
		bindings.PullSecretName, err = registryauth.PullSecretName(agent.UID)
		if err != nil {
			return nil, err
		}
	}
	if r.aiGatewayCopilotWanted(agent) && agent.Status.AIGateway.Copilot != nil {
		copilot := agent.Status.AIGateway.Copilot
		bindings.Copilot = &runtimebindings.CopilotProxy{Image: r.AIGateway.CopilotProxyImage, KeyID: copilot.ProxyKeyID}
		if generation := agent.Annotations[api.AnnotationCopilotSignInGeneration]; copilotSignInGenerationPattern.MatchString(generation) {
			bindings.Copilot.SignInGeneration = generation
		}
		models, _ := parseCopilotModels(agent.Annotations[api.AnnotationCopilotModels])
		for _, model := range models {
			if model.ResponsesOnly && copilotModelName(copilot, model.ID) != "" {
				bindings.Copilot.ResponsesModels = append(bindings.Copilot.ResponsesModels, model.ID)
			}
		}
		slices.Sort(bindings.Copilot.ResponsesModels)
	}
	request := agents.Request{Type: "custom", Image: *agent.Spec.Agent.Image, Port: agent.Spec.Agent.Port, WebUI: agent.Spec.Agent.WebUI}
	if agent.Spec.Agent.Username != nil {
		request.Username = *agent.Spec.Agent.Username
	}
	resolved, err := agents.ResolveCustom(request)
	if err != nil {
		return nil, err
	}
	if r.ChatAccess.Enabled && resolved.Port != nil && !resolved.WebUI {
		for _, caller := range r.ChatAccess.Callers {
			if err := caller.Validate(); err != nil {
				return nil, err
			}
			bindings.Network.Callers = append(bindings.Network.Callers, runtimebindings.Peer{Namespace: caller.Namespace, PodLabels: caller.PodLabels})
		}
	}
	if customTerminalRequested(values) {
		bindings.Network.APIServer, err = r.terminalAPIEndpoints(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := bindings.Validate(resolved); err != nil {
		return nil, err
	}
	if _, err := r.validateCustomObjects(ctx, agent, bindings); err != nil {
		return nil, err
	}
	desktop := map[string]any{"novncPathPrefix": agent.Name + "/desktop/"}
	if r.ChartRegistry != "" {
		desktop["image"] = map[string]any{"repository": r.ChartRegistry + "/cetirun-desktop"}
		values["ttyd"] = deepMerge(values["ttyd"], map[string]any{"image": map[string]any{"repository": r.ChartRegistry + "/cetirun-terminal"}})
	}
	values["agentType"], values["desktop"] = "custom", deepMerge(values["desktop"], desktop)
	values["agentRequest"] = runtimeRequest(agent, "")
	values["agentRequest"].(map[string]any)["releaseName"] = record.Target.Release
	values["customBindings"], values["memoryOptions"] = bindings, r.memoryOptions()
	values["fullnameOverride"], values["replicaCount"] = instanceServiceName(agent.Name), 0
	prepared, err := r.HelmClient.Prepare(namespace.Name, values, agent.Spec.Helm.Version)
	if err != nil {
		return nil, err
	}
	if err := prepared.ValidateCustom(namespace.Name, record.Target.Release); err != nil {
		return nil, err
	}
	return prepared, nil
}

func namespaceWritersStopped(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	pods, sets, deployments, replicas, jobs, cronjobs, daemons := &corev1.PodList{}, &appsv1.StatefulSetList{}, &appsv1.DeploymentList{}, &appsv1.ReplicaSetList{}, &batchv1.JobList{}, &batchv1.CronJobList{}, &appsv1.DaemonSetList{}
	for _, list := range []client.ObjectList{pods, sets, deployments, replicas, jobs, cronjobs, daemons} {
		if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return false, err
		}
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || (pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) {
			return false, nil
		}
	}
	for _, set := range sets.Items {
		if set.DeletionTimestamp != nil || ptr.Deref(set.Spec.Replicas, 1) != 0 || set.Status.ObservedGeneration < set.Generation ||
			set.Status.Replicas != 0 || set.Status.CurrentReplicas != 0 || set.Status.ReadyReplicas != 0 || set.Status.UpdatedReplicas != 0 {
			return false, nil
		}
	}
	for _, deploy := range deployments.Items {
		if deploy.DeletionTimestamp != nil || ptr.Deref(deploy.Spec.Replicas, 1) != 0 || deploy.Status.ObservedGeneration < deploy.Generation ||
			deploy.Status.Replicas != 0 || deploy.Status.ReadyReplicas != 0 || deploy.Status.AvailableReplicas != 0 || deploy.Status.UpdatedReplicas != 0 {
			return false, nil
		}
	}
	for _, set := range replicas.Items {
		if ptr.Deref(set.Spec.Replicas, 1) != 0 || set.Status.Replicas != 0 || set.Status.ObservedGeneration < set.Generation {
			return false, nil
		}
	}
	for _, job := range jobs.Items {
		if job.DeletionTimestamp != nil || job.Status.Active != 0 || (!maintenanceJobTerminal(&job) && !ptr.Deref(job.Spec.Suspend, false)) {
			return false, nil
		}
	}
	for _, job := range cronjobs.Items {
		if !ptr.Deref(job.Spec.Suspend, false) || len(job.Status.Active) != 0 {
			return false, nil
		}
	}
	return len(daemons.Items) == 0, nil
}

func maintenanceJobTerminal(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func maintenancePodStopped(pod *corev1.Pod) bool {
	if pod.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Reason == "NodeLost" ||
		(pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) || len(pod.Spec.Containers) == 0 {
		return false
	}
	terminated := func(names []string, statuses []corev1.ContainerStatus) bool {
		if len(names) != len(statuses) {
			return false
		}
		seen := map[string]bool{}
		for _, status := range statuses {
			if seen[status.Name] || status.State.Terminated == nil || status.State.Terminated.FinishedAt.IsZero() ||
				status.State.Terminated.Reason == "ContainerStatusUnknown" || status.State.Terminated.Reason == "NodeLost" ||
				status.State.Running != nil || status.State.Waiting != nil || status.Ready {
				return false
			}
			seen[status.Name] = true
		}
		for _, name := range names {
			if !seen[name] {
				return false
			}
		}
		return true
	}
	var regular, init, ephemeral []string
	for _, container := range pod.Spec.Containers {
		regular = append(regular, container.Name)
	}
	for _, container := range pod.Spec.InitContainers {
		init = append(init, container.Name)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		ephemeral = append(ephemeral, container.Name)
	}
	return terminated(regular, pod.Status.ContainerStatuses) && terminated(init, pod.Status.InitContainerStatuses) &&
		terminated(ephemeral, pod.Status.EphemeralContainerStatuses)
}

func maintenanceWritersStopped(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	stopped, err := namespaceWritersStopped(ctx, reader, namespace)
	if err != nil || !stopped {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if !maintenancePodStopped(&pods.Items[i]) {
			return false, nil
		}
	}
	return true, nil
}

func maintenanceManifest(raw, namespace string) ([]unstructured.Unstructured, error) {
	if len(raw) > 8<<20 {
		return nil, fmt.Errorf("maintenance manifest exceeds the supported size")
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(raw), 4096)
	var objects []unstructured.Unstructured
	seen := map[string]bool{}
	for {
		object := unstructured.Unstructured{}
		if err := decoder.Decode(&object); err == io.EOF {
			return objects, nil
		} else if err != nil {
			return nil, fmt.Errorf("maintenance manifest cannot be decoded")
		}
		if len(object.Object) == 0 {
			continue
		}
		if len(objects) >= 256 || object.GetName() == "" || object.GetAPIVersion() == "" ||
			!cetiadmin.SupportedManifestKind(object.GroupVersionKind().Group, object.GetKind()) {
			return nil, fmt.Errorf("maintenance manifest contains an unsupported object")
		}
		switch object.GetKind() {
		case "PersistentVolume", "ClusterRoleBinding", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			if object.GetNamespace() != "" {
				return nil, fmt.Errorf("cluster-scoped manifest object has a namespace")
			}
		default:
			if object.GetNamespace() == "" {
				object.SetNamespace(namespace)
			}
			if object.GetNamespace() != namespace {
				return nil, fmt.Errorf("maintenance manifest crosses the bound namespace")
			}
		}
		key := object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
		if seen[key] {
			return nil, fmt.Errorf("maintenance manifest contains duplicate identities")
		}
		seen[key] = true
		objects = append(objects, object)
	}
}

func maintenanceResource(target cetiadmin.TargetIdentity, object client.Object) (cetiadmin.ManagedResource, bool) {
	gvk := object.GetObjectKind().GroupVersionKind()
	for _, ref := range target.Resources {
		if ref.APIVersion == gvk.GroupVersion().String() && ref.Kind == gvk.Kind && ref.Name == object.GetName() &&
			ref.Namespace == object.GetNamespace() && ref.UID != "" && ref.CreationID == "" {
			return ref, true
		}
	}
	return cetiadmin.ManagedResource{}, false
}

func maintenanceHooksStopped(ctx context.Context, reader client.Reader, current *release.Release, target cetiadmin.TargetIdentity) error {
	for _, hook := range current.Hooks {
		if hook == nil || hook.Name == "" {
			return fmt.Errorf("Helm hook identity is unknown")
		}
		objects, err := maintenanceManifest(hook.Manifest, target.Namespace.Name)
		if err != nil || len(objects) != 1 || objects[0].GetName() != hook.Name || objects[0].GetKind() != hook.Kind {
			return fmt.Errorf("Helm hook identity is unrecognized")
		}
		object := &objects[0]
		switch object.GetKind() {
		case "Job", "ServiceAccount", "Role", "RoleBinding":
		default:
			return fmt.Errorf("Helm maintenance hook kind is unsupported")
		}
		neverRun := hook.LastRun.StartedAt.IsZero() && hook.LastRun.CompletedAt.IsZero() &&
			(hook.LastRun.Phase == "" || hook.LastRun.Phase == release.HookPhaseUnknown)
		if !neverRun && (hook.LastRun.CompletedAt.IsZero() || hook.LastRun.StartedAt.IsZero() ||
			(hook.LastRun.Phase != release.HookPhaseSucceeded && hook.LastRun.Phase != release.HookPhaseFailed)) {
			return fmt.Errorf("Helm hook completion is unknown")
		}
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(object.GroupVersionKind())
		err = reader.Get(ctx, client.ObjectKeyFromObject(object), actual)
		if apierrors.IsNotFound(err) {
			deleted := neverRun
			for _, policy := range hook.DeletePolicies {
				deleted = deleted || (hook.LastRun.Phase == release.HookPhaseSucceeded && policy == release.HookSucceeded) ||
					(hook.LastRun.Phase == release.HookPhaseFailed && policy == release.HookFailed)
			}
			if deleted {
				continue
			}
		}
		if err != nil {
			return fmt.Errorf("Helm hook cannot be observed: %w", err)
		}
		if neverRun || actual.GetUID() == "" || actual.GetDeletionTimestamp() != nil ||
			actual.GetAnnotations()["helm.sh/hook"] != object.GetAnnotations()["helm.sh/hook"] {
			return fmt.Errorf("Helm hook has no matching terminal observation")
		}
		for key, value := range object.GetLabels() {
			if actual.GetLabels()[key] != value {
				return fmt.Errorf("Helm hook metadata changed")
			}
		}
		if object.GetKind() == "Job" {
			want := &batchv1.Job{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, want); err != nil {
				return fmt.Errorf("Helm hook Job cannot be decoded")
			}
			job := &batchv1.Job{}
			if err := reader.Get(ctx, client.ObjectKeyFromObject(actual), job); err != nil {
				return err
			}
			if job.UID != actual.GetUID() || job.DeletionTimestamp != nil || !maintenanceJobTerminal(job) || job.Status.Active != 0 ||
				!equality.Semantic.DeepDerivative(want.Spec, job.Spec) {
				return fmt.Errorf("Helm hook has no matching terminal observation")
			}
		}
	}
	return nil
}
