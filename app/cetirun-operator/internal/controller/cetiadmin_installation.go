// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type CetiAdminReconciler struct {
	client.Client
	APIReader      client.Reader
	Source         types.NamespacedName
	Runtime        *AgentReconciler
	OperatorImage  string
	ChartVersion   string
	AdmissionReady func(context.Context) error
	GrantAccess    func(context.Context, cetiadmin.Binding, *cetiadmin.Installation) error
	RevokeAccess   func(context.Context, *cetiadmin.Installation) error
}

func (r *CetiAdminReconciler) OperatorImageReference() string { return r.OperatorImage }

// CetiAdminSource exposes the deployment-configured source to components that
// must distinguish CetiAdmin's runtime bindings from Sandbox grants.
func (r *CetiAdminReconciler) CetiAdminSource() client.ObjectKey { return r.Source }

func (r *CetiAdminReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Source.Name == "" || r.Source.Namespace == "" || r.Runtime == nil || r.OperatorImage == "" ||
		r.AdmissionReady == nil || r.GrantAccess == nil || r.RevokeAccess == nil {
		return fmt.Errorf("CetiAdmin requires its exact source, runtime, image and access callbacks")
	}
	if r.APIReader == nil {
		r.APIReader = manager.GetAPIReader()
	}
	queue := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: r.Source}}
	})
	return ctrl.NewControllerManagedBy(manager).Named("cetiadmin-installation").
		Watches(&corev1.ConfigMap{}, queue).Watches(&api.Agent{}, queue).Watches(&api.Team{}, queue).
		Watches(&corev1.Namespace{}, queue).Watches(&corev1.ServiceAccount{}, queue).
		Watches(&rbacv1.RoleBinding{}, queue).Watches(&rbacv1.ClusterRoleBinding{}, queue).Complete(r)
}

func (r *CetiAdminReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *CetiAdminReconciler) stateKey() types.NamespacedName {
	return types.NamespacedName{Namespace: r.Source.Namespace, Name: cetiadmin.InstallationName(r.Source.Namespace, r.Source.Name)}
}

func identityOf(object metav1.Object) cetiadmin.ObjectIdentity {
	return cetiadmin.ObjectIdentity{Name: object.GetName(), Namespace: object.GetNamespace(), UID: object.GetUID()}
}

func identityMatches(expected cetiadmin.ObjectIdentity, actual metav1.Object) bool {
	return expected.UID != "" && expected == identityOf(actual) && actual.GetDeletionTimestamp() == nil
}

func (r *CetiAdminReconciler) readState(ctx context.Context) (*corev1.ConfigMap, *cetiadmin.Installation, error) {
	object := &corev1.ConfigMap{}
	if err := r.reader().Get(ctx, r.stateKey(), object); err != nil {
		return nil, nil, err
	}
	state, err := cetiadmin.ParseInstallation(object.Data[cetiadmin.BindingKey])
	if err != nil || object.UID == "" || object.DeletionTimestamp != nil ||
		state.Source.Name != r.Source.Name || state.Source.Namespace != r.Source.Namespace ||
		object.Annotations[cetiadmin.AnnotationSourceUID] != string(state.Source.UID) ||
		(state.RecordUID != "" && state.RecordUID != object.UID) {
		return object, nil, fmt.Errorf("%w: installation record", cetiadmin.ErrConflict)
	}
	state.RecordUID = object.UID
	return object, &state, nil
}

func (r *CetiAdminReconciler) saveState(ctx context.Context, object *corev1.ConfigMap, state *cetiadmin.Installation) error {
	if object == nil || state == nil {
		return cetiadmin.ErrUnbound
	}
	if object.UID != "" {
		if state.RecordUID != "" && state.RecordUID != object.UID {
			return cetiadmin.ErrConflict
		}
		state.RecordUID = object.UID
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	want := object.DeepCopy()
	if want.Data == nil {
		want.Data = map[string]string{}
	}
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	want.Data[cetiadmin.BindingKey] = string(data)
	want.Annotations[cetiadmin.AnnotationSourceUID] = string(state.Source.UID)
	if object.UID != "" && reflect.DeepEqual(object.Data, want.Data) && reflect.DeepEqual(object.Annotations, want.Annotations) {
		return nil
	}
	if object.UID == "" {
		err = r.Create(ctx, want)
	} else {
		err = r.Patch(ctx, want, client.MergeFromWithOptions(object.DeepCopy(), client.MergeFromWithOptimisticLock{}))
	}
	current := &corev1.ConfigMap{}
	readErr := r.reader().Get(ctx, client.ObjectKeyFromObject(object), current)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if current.UID == "" || (object.UID != "" && current.UID != object.UID) || current.DeletionTimestamp != nil ||
		current.Data[cetiadmin.BindingKey] != string(data) ||
		current.Annotations[cetiadmin.AnnotationSourceUID] != string(state.Source.UID) ||
		current.Annotations[cetiadmin.AnnotationCreationID] != want.Annotations[cetiadmin.AnnotationCreationID] {
		return errors.Join(err, fmt.Errorf("%w: installation checkpoint readback", cetiadmin.ErrConflict))
	}
	*object = *current
	state.RecordUID = current.UID
	return nil
}

func (r *CetiAdminReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (result ctrl.Result, reconcileErr error) {
	result.RequeueAfter = 5 * time.Second
	object, state, stateErr := r.readState(ctx)
	if stateErr != nil {
		found, recoveryErr := r.holdOrphanedInstallation(ctx)
		if found || recoveryErr != nil {
			return result, errors.Join(stateErr, recoveryErr)
		}
		if !apierrors.IsNotFound(stateErr) {
			return result, stateErr
		}
	}
	defer func() {
		if reconcileErr != nil && state != nil {
			reconcileErr = errors.Join(reconcileErr, r.deactivate(ctx, object, state, "ReconcileFailed"))
		} else if state != nil && state.CanRun() {
			reconcileErr = r.reconcileDiagnosticAccess(ctx, state)
		}
	}()
	source := &corev1.ConfigMap{}
	if err := r.reader().Get(ctx, r.Source, source); err != nil {
		if state != nil {
			return result, errors.Join(client.IgnoreNotFound(err), r.deactivate(ctx, object, state, "InstallationSourceMissing"))
		}
		return result, client.IgnoreNotFound(err)
	}
	var retained *cetiadmin.Record
	if state != nil {
		retained = &state.Record
	}
	config, err := cetiadmin.ReadSourceConfig(source, retained)
	if err != nil {
		return result, err
	}
	if source.UID == "" {
		return result, cetiadmin.ErrUnbound
	}
	if state == nil {
		if source.DeletionTimestamp != nil {
			return ctrl.Result{}, nil
		}
		if config.RetainedAgentUID != "" {
			return result, fmt.Errorf("retainedAgentUID requires the original installation record")
		}
		state = &cetiadmin.Installation{Record: cetiadmin.Record{SchemaVersion: 1, Source: identityOf(source), ConfigHash: installationConfigHash(config)},
			Configuration: config, Phase: cetiadmin.PhaseInstalling, Hold: true, OperatorImage: r.OperatorImage, RuntimeVersion: r.ChartVersion}
		if !config.Enabled {
			state.Phase, state.Reason = cetiadmin.PhaseDisabled, "Disabled"
		}
		key := r.stateKey()
		object = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace,
			Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID), cetiadmin.AnnotationCreationID: uuid.NewString()}}}
		return result, r.saveState(ctx, object, state)
	}
	if !config.Enabled || source.DeletionTimestamp != nil {
		if err := r.deactivate(ctx, object, state, "Disabled"); err != nil {
			return result, err
		}
		if state.Phase == cetiadmin.PhaseDisabled && state.Source.UID == source.UID {
			state.Configuration, state.ConfigHash = config, installationConfigHash(config)
			state.OperatorImage, state.RuntimeVersion = r.OperatorImage, r.ChartVersion
			return result, r.saveState(ctx, object, state)
		}
		return result, nil
	}
	if config.RetainedAgentUID != "" && state.Agent.UID != "" && config.RetainedAgentUID != string(state.Agent.UID) {
		return result, fmt.Errorf("retainedAgentUID does not match the recorded Agent")
	}
	if state.Source.UID != source.UID {
		if err := r.deactivate(ctx, object, state, "InstallationSourceReplaced"); err != nil {
			return result, err
		}
		if state.Phase != cetiadmin.PhaseDisabled {
			return result, nil
		}
		if state.Agent.UID == "" || config.RetainedAgentUID != string(state.Agent.UID) {
			return result, fmt.Errorf("replacement source requires the retained Agent UID and unchanged installation identity")
		}
		if err := r.validateInstallationIdentity(ctx, state, config); err != nil {
			return result, err
		}
		previous := state.Source
		state.PreviousSource, state.Source = &previous, identityOf(source)
		state.Configuration, state.ConfigHash = config, installationConfigHash(config)
		state.Phase, state.Hold, state.Reason = cetiadmin.PhaseInstalling, true, ""
		state.ResolvedRuntime = cetiadmin.RuntimeCoordinates{}
		state.OperatorImage, state.RuntimeVersion = r.OperatorImage, r.ChartVersion
		return result, r.saveState(ctx, object, state)
	}
	if r.Runtime == nil || r.Runtime.HelmClient == nil || r.AdmissionReady == nil || r.GrantAccess == nil || r.RevokeAccess == nil ||
		r.OperatorImage == "" || r.ChartVersion == "" {
		return result, fmt.Errorf("CetiAdmin runtime, image, version and access callbacks are required")
	}
	if err := config.ValidateMCPAudience(r.Runtime.ConnectionsMCPAudience); err != nil {
		return result, err
	}
	if err := r.AdmissionReady(ctx); err != nil {
		return result, fmt.Errorf("AdmissionUnavailable: %w", err)
	}
	if err := r.validateInstallationIdentity(ctx, state, config); err != nil {
		return result, err
	}
	if state.Creation != nil && (state.ConfigHash != installationConfigHash(config) || state.Phase == cetiadmin.PhaseStopping) {
		if err := r.settleCreation(ctx, object, state); err != nil {
			return result, err
		}
	}
	if state.Phase == cetiadmin.PhaseStopping {
		return result, r.deactivate(ctx, object, state, state.Reason)
	}
	if state.ConfigHash != installationConfigHash(config) || state.OperatorImage != r.OperatorImage || state.RuntimeVersion != r.ChartVersion {
		if err := r.deactivate(ctx, object, state, "ConfigurationChanging"); err != nil {
			return result, err
		}
		if state.Phase != cetiadmin.PhaseDisabled {
			return result, nil
		}
		state.Configuration, state.ConfigHash = config, installationConfigHash(config)
		state.OperatorImage, state.RuntimeVersion = r.OperatorImage, r.ChartVersion
		state.ResolvedRuntime = cetiadmin.RuntimeCoordinates{}
		state.Phase, state.Hold, state.Reason = cetiadmin.PhaseInstalling, true, ""
		return result, r.saveState(ctx, object, state)
	}
	if err := r.migrationSourceStopped(ctx, config); err != nil {
		return result, err
	}
	if state.Bootstrapped && state.Phase == cetiadmin.PhaseDisabled && len(state.Resources) == 0 {
		observed, err := r.observe(ctx, source, state.Record)
		if err != nil {
			return result, err
		}
		if observed.Agent.Spec.Suspended && r.validateInstalled(state, observed) == nil {
			if err := r.validateRetainedObjects(ctx, state); err != nil {
				return result, err
			}
			return result, r.deactivate(ctx, object, state, "UserSuspended")
		}
	}
	if state.Phase == cetiadmin.PhaseDisabled || state.Phase == cetiadmin.PhaseBlocked {
		state.Phase, state.Hold, state.Reason = cetiadmin.PhaseInstalling, true, ""
		if err := r.saveState(ctx, object, state); err != nil {
			return result, err
		}
	}
	if state.ResolvedRuntime.Image == "" {
		resolved, err := r.resolveRuntime(config, source)
		if err != nil {
			return result, err
		}
		state.ResolvedRuntime = resolved
		return result, r.saveState(ctx, object, state)
	}
	if changed, err := r.installObjects(ctx, source, object, state); changed || err != nil {
		return result, err
	}
	observed, err := r.observe(ctx, source, state.Record)
	if err != nil {
		return result, err
	}
	if err := r.validateInstalled(state, observed); err != nil {
		return result, err
	}
	if state.Bootstrapped && observed.Agent.Spec.Suspended {
		return result, r.deactivate(ctx, object, state, "UserSuspended")
	}
	binding, err := cetiadmin.ResolveBinding(config, &state.Record, observed)
	if err != nil {
		return result, err
	}
	if err := r.GrantAccess(ctx, binding, state); err != nil {
		return result, err
	}
	object, err = r.checkpointObject(ctx, state)
	if err != nil {
		return result, err
	}
	if err := r.saveRecovery(ctx, object, state, observed.Agent); err != nil {
		return result, err
	}
	state.Phase, state.Hold, state.Reason, state.PreviousSource = cetiadmin.PhaseActive, false, "", nil
	if !state.Bootstrapped {
		state.BootstrapReleasePending = true
		if err := r.saveState(ctx, object, state); err != nil {
			return result, err
		}
		if observed.Agent.Spec.Suspended {
			before := observed.Agent.DeepCopy()
			observed.Agent.Spec.Suspended = false
			if err := r.patchAndRead(ctx, before, observed.Agent); err != nil {
				return result, err
			}
		}
		state.Bootstrapped, state.BootstrapReleasePending = true, false
		return result, r.saveState(ctx, object, state)
	}
	if observed.Agent.Status.Phase == api.PhaseReady && observed.Agent.Status.ObservedGeneration == observed.Agent.Generation {
		state.Phase = cetiadmin.PhaseReady
		result.RequeueAfter = 30 * time.Second
	}
	return result, r.saveState(ctx, object, state)
}

func installationConfigHash(config cetiadmin.Config) string {
	hash, _ := cetiadmin.ConfigHash(config)
	return hash
}

func sameCetiAdminOwner(a, b cetiadmin.Owner) bool {
	return a.Alias == b.Alias && strings.EqualFold(a.TenantID, b.TenantID) && strings.EqualFold(a.ObjectID, b.ObjectID)
}

func (r *CetiAdminReconciler) validateInstallationIdentity(ctx context.Context, state *cetiadmin.Installation, config cetiadmin.Config) error {
	if state.Agent.UID != "" {
		retained := *state
		retained.Configuration = config
		return r.validateRetainedObjects(ctx, &retained)
	}
	if state.Team.UID != "" && state.Team.Name != config.InstanceName {
		return fmt.Errorf("installation name changes require migration")
	}
	if state.Configuration.Enabled && (state.Configuration.InstanceName != config.InstanceName || !sameCetiAdminOwner(state.Configuration.Owner, config.Owner)) {
		return fmt.Errorf("installation owner or name changes require migration")
	}
	return nil
}

func (r *CetiAdminReconciler) resolveRuntime(config cetiadmin.Config, source *corev1.ConfigMap) (cetiadmin.RuntimeCoordinates, error) {
	agent, err := desiredCetiAdminAgent(config, source, r.ChartVersion)
	if err != nil {
		return cetiadmin.RuntimeCoordinates{}, err
	}
	team, err := desiredCetiAdminTeam(config, source)
	if err != nil {
		return cetiadmin.RuntimeCoordinates{}, err
	}
	prepared, err := r.Runtime.prepareAgent(agent, team)
	if err != nil {
		return cetiadmin.RuntimeCoordinates{}, err
	}
	if prepared == nil || prepared.ChartVersion != agent.Spec.Helm.Version || prepared.Agent.Type != "hermes" ||
		prepared.Agent.Image == "" || prepared.Agent.Port == nil || !prepared.SupportsValue("cetiadmin") {
		return cetiadmin.RuntimeCoordinates{}, fmt.Errorf("CetiAdmin requires a paired Hermes profile and a runtime chart with CetiAdmin support")
	}
	return cetiadmin.RuntimeCoordinates{Version: prepared.ChartVersion, Image: prepared.Agent.Image, Port: ptr.To(*prepared.Agent.Port)}, nil
}

func (r *CetiAdminReconciler) checkpointObject(ctx context.Context, state *cetiadmin.Installation) (*corev1.ConfigMap, error) {
	object, current, err := r.readState(ctx)
	if err != nil {
		return nil, err
	}
	if current.RecordUID != state.RecordUID || current.Source != state.Source || current.ConfigHash != state.ConfigHash {
		return nil, cetiadmin.ErrConflict
	}
	return object, nil
}

func (r *CetiAdminReconciler) installObjects(ctx context.Context, source, checkpoint *corev1.ConfigMap, state *cetiadmin.Installation) (bool, error) {
	config := state.Configuration
	config.Runtime = state.ResolvedRuntime
	team, err := desiredCetiAdminTeam(config, source)
	if err != nil {
		return false, err
	}
	if changed, err := r.ensureCreated(ctx, checkpoint, state, "Team", team, &state.Team); changed || err != nil {
		return changed, err
	}
	agent, err := desiredCetiAdminAgent(config, source, r.ChartVersion)
	if err != nil {
		return false, err
	}
	if changed, err := r.ensureCreated(ctx, checkpoint, state, "Agent", agent, &state.Agent); changed || err != nil {
		return changed, err
	}
	currentAgent := &api.Agent{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: state.Agent.Name}, currentAgent); err != nil {
		return false, err
	}
	owner := registryauth.Owner(currentAgent)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(agent.Name),
		Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID)}, OwnerReferences: []metav1.OwnerReference{owner},
		Labels: map[string]string{"app.kubernetes.io/name": "cetirun", "app.kubernetes.io/instance": agent.Spec.User.Alias, "app.kubernetes.io/managed-by": "cetirun-operator"}}}
	if revision := r.Runtime.resolveIstioRevision(ctx); revision != "" {
		namespace.Labels["istio.io/rev"] = revision
	}
	if changed, err := r.ensureCreated(ctx, checkpoint, state, "Namespace", namespace, &state.Namespace); changed || err != nil {
		return changed, err
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: namespace.Name,
		Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID)}, OwnerReferences: []metav1.OwnerReference{owner}}, AutomountServiceAccountToken: ptr.To(false)}
	if config.AzureWorkloadIdentity.Enabled {
		account.Annotations["azure.workload.identity/client-id"] = config.AzureWorkloadIdentity.ClientID
	}
	if changed, err := r.ensureCreated(ctx, checkpoint, state, "ServiceAccount", account, &state.ServiceAccount); changed || err != nil {
		return changed, err
	}
	observed, err := r.observe(ctx, source, state.Record)
	if err != nil {
		return false, err
	}
	if err := r.validateRetainedObjects(ctx, state); err != nil {
		return false, err
	}
	changed := false
	for _, actual := range []client.Object{observed.Team, observed.Agent, observed.Namespace, observed.ServiceAccount} {
		before := actual.DeepCopyObject().(client.Object)
		annotations := maps.Clone(actual.GetAnnotations())
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[cetiadmin.AnnotationSourceUID] = string(state.Source.UID)
		actual.SetAnnotations(annotations)
		switch value := actual.(type) {
		case *api.Agent:
			value.Spec.Helm.Version = agent.Spec.Helm.Version
			value.Spec.Agent.Image, value.Spec.Agent.Port = agent.Spec.Agent.Image, agent.Spec.Agent.Port
			value.Spec.Model = agent.Spec.Model
			if value.Spec.ModelAccess == nil {
				value.Spec.ModelAccess = &api.ModelAccessSpec{}
			}
			value.Spec.ModelAccess.SharedModels, value.Spec.ModelAccess.GitHubCopilot = agent.Spec.ModelAccess.SharedModels, agent.Spec.ModelAccess.GitHubCopilot
			if value.Spec.Helm.Values == nil {
				value.Spec.Helm.Values = map[string]runtime.RawExtension{}
			}
			skills := map[string]any{}
			if raw := value.Spec.Helm.Values["skills"].Raw; len(raw) != 0 {
				if err := json.Unmarshal(raw, &skills); err != nil || skills == nil {
					return false, fmt.Errorf("CetiAdmin skills configuration must be an object")
				}
			}
			skills["shared"] = config.SharedSkills.Enabled
			raw, _ := json.Marshal(skills)
			value.Spec.Helm.Values["skills"] = runtime.RawExtension{Raw: raw}
		case *api.Team:
			value.Spec.MemberContext = team.Spec.MemberContext
		case *corev1.ServiceAccount:
			if config.AzureWorkloadIdentity.Enabled {
				value.Annotations["azure.workload.identity/client-id"] = config.AzureWorkloadIdentity.ClientID
			} else {
				delete(value.Annotations, "azure.workload.identity/client-id")
			}
		}
		if !reflect.DeepEqual(before, actual) {
			if state.CanRun() {
				return false, fmt.Errorf("installation fields changed while runtime is active")
			}
			if err := r.patchAndRead(ctx, before, actual); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

func (r *CetiAdminReconciler) patchAndRead(ctx context.Context, before, desired client.Object) error {
	err := r.Patch(ctx, desired, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	current := desired.DeepCopyObject().(client.Object)
	readErr := r.reader().Get(ctx, client.ObjectKeyFromObject(desired), current)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if current.GetUID() != before.GetUID() || !sameCetiAdminCreation(desired, current) {
		return errors.Join(err, fmt.Errorf("CetiAdmin object update was not observed"))
	}
	reflect.ValueOf(desired).Elem().Set(reflect.ValueOf(current).Elem())
	return nil
}

func installationObject(kind string) (client.Object, schema.GroupVersionKind, error) {
	var object client.Object
	version := corev1.SchemeGroupVersion
	switch kind {
	case "Agent":
		object, version = &api.Agent{}, api.GroupVersion
	case "Team":
		object, version = &api.Team{}, api.GroupVersion
	case "Namespace":
		object = &corev1.Namespace{}
	case "ServiceAccount":
		object = &corev1.ServiceAccount{}
	case "ConfigMap":
		object = &corev1.ConfigMap{}
	case "Role":
		object, version = &rbacv1.Role{}, rbacv1.SchemeGroupVersion
	case "RoleBinding":
		object, version = &rbacv1.RoleBinding{}, rbacv1.SchemeGroupVersion
	case "ClusterRole":
		object, version = &rbacv1.ClusterRole{}, rbacv1.SchemeGroupVersion
	case "ClusterRoleBinding":
		object, version = &rbacv1.ClusterRoleBinding{}, rbacv1.SchemeGroupVersion
	default:
		return nil, schema.GroupVersionKind{}, fmt.Errorf("unsupported installation object %s", kind)
	}
	gvk := version.WithKind(kind)
	object.GetObjectKind().SetGroupVersionKind(gvk)
	return object, gvk, nil
}

func (r *CetiAdminReconciler) ensureCreated(ctx context.Context, checkpoint *corev1.ConfigMap, state *cetiadmin.Installation, kind string, desired client.Object, identity *cetiadmin.ObjectIdentity) (bool, error) {
	current, gvk, err := installationObject(kind)
	if err != nil {
		return false, err
	}
	err = r.reader().Get(ctx, client.ObjectKeyFromObject(desired), current)
	if identity.UID != "" {
		if err != nil {
			return false, err
		}
		if !identityMatches(*identity, current) || identity.Name != desired.GetName() || identity.Namespace != desired.GetNamespace() {
			return false, fmt.Errorf("%w: %s identity changed", cetiadmin.ErrConflict, kind)
		}
		return false, nil
	}
	if state.Creation == nil {
		if err == nil {
			return false, fmt.Errorf("%w: %s already exists without a creation receipt", cetiadmin.ErrConflict, kind)
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		if state.PendingCreate != "" {
			return false, fmt.Errorf("legacy incomplete creation needs explicit recovery")
		}
		request := desired.DeepCopyObject().(client.Object)
		request.GetObjectKind().SetGroupVersionKind(gvk)
		annotations := maps.Clone(request.GetAnnotations())
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[cetiadmin.AnnotationSourceUID] = string(state.Source.UID)
		annotations[cetiadmin.AnnotationInstallationUID] = string(state.RecordUID)
		annotations[cetiadmin.AnnotationCreationID] = uuid.NewString()
		request.SetAnnotations(annotations)
		raw, marshalErr := json.Marshal(request)
		if marshalErr != nil {
			return false, marshalErr
		}
		state.PendingCreate, state.Creation = kind, &cetiadmin.CreationIntent{Object: raw, Resource: cetiadmin.ManagedResource{
			APIVersion: gvk.GroupVersion().String(), Kind: kind, ObjectIdentity: identityOf(request), CreationID: annotations[cetiadmin.AnnotationCreationID]}}
		if err := r.saveState(ctx, checkpoint, state); err != nil {
			return false, err
		}
	}
	if state.PendingCreate != kind || state.Creation.Resource.Kind != kind ||
		state.Creation.Resource.Name != desired.GetName() || state.Creation.Resource.Namespace != desired.GetNamespace() {
		return false, fmt.Errorf("another installation creation is unfinished")
	}
	request, err := r.creationObject(state)
	if err != nil {
		return false, err
	}
	err = r.reader().Get(ctx, client.ObjectKeyFromObject(request), current)
	if apierrors.IsNotFound(err) {
		createErr := r.Create(ctx, request)
		if readErr := r.reader().Get(ctx, client.ObjectKeyFromObject(request), current); readErr != nil {
			return false, errors.Join(createErr, readErr)
		}
	} else if err != nil {
		return false, err
	}
	if current.GetUID() == "" || !sameCetiAdminCreation(request, current) {
		return false, fmt.Errorf("%w: %s create cannot be attributed", cetiadmin.ErrConflict, kind)
	}
	*identity = identityOf(current)
	if kind != "Agent" && kind != "Team" && kind != "Namespace" && kind != "ServiceAccount" {
		resource := state.Creation.Resource
		resource.ObjectIdentity = *identity
		state.Resources = append(state.Resources, resource)
	}
	state.PendingCreate, state.Creation = "", nil
	return true, r.saveState(ctx, checkpoint, state)
}

func (r *CetiAdminReconciler) creationObject(state *cetiadmin.Installation) (client.Object, error) {
	intent := state.Creation
	if intent == nil || state.RecordUID == "" || intent.Resource.CreationID == "" {
		return nil, cetiadmin.ErrConflict
	}
	object, gvk, err := installationObject(intent.Resource.Kind)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(intent.Object, object) != nil || object.GetName() != intent.Resource.Name || object.GetNamespace() != intent.Resource.Namespace ||
		intent.Resource.APIVersion != gvk.GroupVersion().String() || object.GetUID() != "" ||
		object.GetAnnotations()[cetiadmin.AnnotationCreationID] != intent.Resource.CreationID ||
		object.GetAnnotations()[cetiadmin.AnnotationInstallationUID] != string(state.RecordUID) ||
		object.GetAnnotations()[cetiadmin.AnnotationSourceUID] != string(state.Source.UID) {
		return nil, cetiadmin.ErrConflict
	}
	return object, nil
}

func sameCetiAdminCreation(desired, actual client.Object) bool {
	if desired.GetName() != actual.GetName() || desired.GetNamespace() != actual.GetNamespace() || actual.GetDeletionTimestamp() != nil ||
		!reflect.DeepEqual(desired.GetOwnerReferences(), actual.GetOwnerReferences()) {
		return false
	}
	for key, value := range desired.GetAnnotations() {
		if actual.GetAnnotations()[key] != value {
			return false
		}
	}
	for key, value := range desired.GetLabels() {
		if actual.GetLabels()[key] != value {
			return false
		}
	}
	switch want := desired.(type) {
	case *api.Agent:
		got, ok := actual.(*api.Agent)
		return ok && reflect.DeepEqual(want.Spec, got.Spec)
	case *api.Team:
		got, ok := actual.(*api.Team)
		return ok && reflect.DeepEqual(want.Spec, got.Spec)
	case *corev1.Namespace:
		_, ok := actual.(*corev1.Namespace)
		return ok
	case *corev1.ServiceAccount:
		got, ok := actual.(*corev1.ServiceAccount)
		return ok && reflect.DeepEqual(want.AutomountServiceAccountToken, got.AutomountServiceAccountToken) &&
			reflect.DeepEqual(want.Secrets, got.Secrets) && reflect.DeepEqual(want.ImagePullSecrets, got.ImagePullSecrets) &&
			want.Annotations["azure.workload.identity/client-id"] == got.Annotations["azure.workload.identity/client-id"]
	case *corev1.ConfigMap:
		got, ok := actual.(*corev1.ConfigMap)
		return ok && reflect.DeepEqual(want.Data, got.Data) && reflect.DeepEqual(want.BinaryData, got.BinaryData) && reflect.DeepEqual(want.Immutable, got.Immutable)
	case *rbacv1.Role:
		got, ok := actual.(*rbacv1.Role)
		return ok && reflect.DeepEqual(want.Rules, got.Rules)
	case *rbacv1.ClusterRole:
		got, ok := actual.(*rbacv1.ClusterRole)
		return ok && reflect.DeepEqual(want.Rules, got.Rules) && reflect.DeepEqual(want.AggregationRule, got.AggregationRule)
	case *rbacv1.RoleBinding:
		got, ok := actual.(*rbacv1.RoleBinding)
		return ok && reflect.DeepEqual(want.Subjects, got.Subjects) && want.RoleRef == got.RoleRef
	case *rbacv1.ClusterRoleBinding:
		got, ok := actual.(*rbacv1.ClusterRoleBinding)
		return ok && reflect.DeepEqual(want.Subjects, got.Subjects) && want.RoleRef == got.RoleRef
	}
	return false
}

// Settle a pending request using observation only. Disable and config changes
// never submit the old CREATE again.
func (r *CetiAdminReconciler) settleCreation(ctx context.Context, checkpoint *corev1.ConfigMap, state *cetiadmin.Installation) error {
	if state.Creation == nil {
		return nil
	}
	request, err := r.creationObject(state)
	if err != nil {
		return err
	}
	actual := request.DeepCopyObject().(client.Object)
	err = r.reader().Get(ctx, client.ObjectKeyFromObject(request), actual)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if actual.GetUID() == "" || !sameCetiAdminCreation(request, actual) {
			return cetiadmin.ErrConflict
		}
		identity := identityOf(actual)
		switch state.Creation.Resource.Kind {
		case "Agent":
			state.Agent = identity
		case "Team":
			state.Team = identity
		case "Namespace":
			state.Namespace = identity
		case "ServiceAccount":
			state.ServiceAccount = identity
		default:
			resource := state.Creation.Resource
			resource.ObjectIdentity = identity
			state.Resources = append(state.Resources, resource)
		}
	}
	state.Creation, state.PendingCreate = nil, ""
	return r.saveState(ctx, checkpoint, state)
}

func (r *CetiAdminReconciler) observe(ctx context.Context, source *corev1.ConfigMap, record cetiadmin.Record) (cetiadmin.ObservedInstallation, error) {
	observed := cetiadmin.ObservedInstallation{Source: source, Agent: &api.Agent{}, Team: &api.Team{}, Namespace: &corev1.Namespace{}, ServiceAccount: &corev1.ServiceAccount{}}
	for _, item := range []struct {
		identity cetiadmin.ObjectIdentity
		object   client.Object
	}{
		{record.Agent, observed.Agent}, {record.Team, observed.Team}, {record.Namespace, observed.Namespace}, {record.ServiceAccount, observed.ServiceAccount},
	} {
		if item.identity.UID == "" {
			return observed, cetiadmin.ErrUnbound
		}
		if err := r.reader().Get(ctx, client.ObjectKey{Name: item.identity.Name, Namespace: item.identity.Namespace}, item.object); err != nil {
			return observed, err
		}
		if !identityMatches(item.identity, item.object) {
			return observed, cetiadmin.ErrConflict
		}
	}
	return observed, nil
}

func (r *CetiAdminReconciler) validateRetainedObjects(ctx context.Context, state *cetiadmin.Installation) error {
	observed := cetiadmin.ObservedInstallation{Agent: &api.Agent{}, Team: &api.Team{}, Namespace: &corev1.Namespace{}, ServiceAccount: &corev1.ServiceAccount{}}
	if state.Agent.UID == "" || state.Team.UID == "" || (state.Namespace.UID == "" && state.ServiceAccount.UID != "") {
		return cetiadmin.ErrUnbound
	}
	for _, item := range []struct {
		ref    cetiadmin.ObjectIdentity
		object client.Object
	}{
		{state.Agent, observed.Agent}, {state.Team, observed.Team}, {state.Namespace, observed.Namespace}, {state.ServiceAccount, observed.ServiceAccount},
	} {
		if item.ref.UID == "" {
			continue
		}
		if err := r.reader().Get(ctx, client.ObjectKey{Name: item.ref.Name, Namespace: item.ref.Namespace}, item.object); err != nil {
			return err
		}
		if !identityMatches(item.ref, item.object) {
			return cetiadmin.ErrConflict
		}
	}
	a, config := observed.Agent, state.Configuration
	if a.Name != config.InstanceName || a.Namespace != "" || a.Spec.Team != state.Team.Name || state.Team.Name != config.InstanceName ||
		a.Spec.Agent.Type != api.AgentTypeHermes || ptr.Deref(a.Spec.Agent.Username, "vibe") != "vibe" || a.Spec.SandboxRef != "" ||
		a.Spec.User.Alias != config.Owner.Alias || !strings.EqualFold(a.Annotations[api.AnnotationOwnerTenantID], config.Owner.TenantID) ||
		!strings.EqualFold(a.Annotations[api.AnnotationOwnerObjectID], config.Owner.ObjectID) || observed.Team.Spec.PolicyConfigured() {
		return fmt.Errorf("%w: installation owner, runtime, Team or ServiceAccount", cetiadmin.ErrConflict)
	}
	if state.Namespace.UID != "" && (observed.Namespace.Name != instanceNamespace(a.Name) || !hasAgentOwner(observed.Namespace, a)) {
		return cetiadmin.ErrConflict
	}
	if state.ServiceAccount.UID != "" && (observed.ServiceAccount.Name != instanceServiceName(a.Name) || observed.ServiceAccount.Namespace != observed.Namespace.Name ||
		!hasAgentOwner(observed.ServiceAccount, a) || observed.ServiceAccount.AutomountServiceAccountToken == nil || *observed.ServiceAccount.AutomountServiceAccountToken || len(observed.ServiceAccount.Secrets) != 0) {
		return cetiadmin.ErrConflict
	}
	for _, object := range []metav1.Object{a, observed.Team, observed.Namespace, observed.ServiceAccount} {
		if object.GetUID() == "" {
			continue
		}
		marker := object.GetAnnotations()[cetiadmin.AnnotationSourceUID]
		if marker != string(state.Source.UID) && (state.PreviousSource == nil || marker != string(state.PreviousSource.UID)) {
			return cetiadmin.ErrConflict
		}
		if object.GetAnnotations()[cetiadmin.AnnotationInstallationUID] != string(state.RecordUID) {
			return cetiadmin.ErrConflict
		}
	}
	return nil
}

func (r *CetiAdminReconciler) validateInstalled(state *cetiadmin.Installation, observed cetiadmin.ObservedInstallation) error {
	a, config := observed.Agent, state.Configuration
	if a.Spec.Helm.Version != state.ResolvedRuntime.Version || a.Spec.Agent.Image == nil || *a.Spec.Agent.Image != state.ResolvedRuntime.Image ||
		a.Spec.Agent.Port == nil || state.ResolvedRuntime.Port == nil || *a.Spec.Agent.Port != *state.ResolvedRuntime.Port ||
		a.Spec.Model != config.Model.Default || !a.Spec.SharedModelsEnabled() || a.Spec.GitHubCopilotEnabled() != config.Model.GitHubCopilot {
		return fmt.Errorf("installation runtime or model fields are not current")
	}
	var skills map[string]any
	if json.Unmarshal(a.Spec.Helm.Values["skills"].Raw, &skills) != nil || skills["shared"] != config.SharedSkills.Enabled {
		return fmt.Errorf("installation skills are not current")
	}
	clientID := ""
	if config.AzureWorkloadIdentity.Enabled {
		clientID = config.AzureWorkloadIdentity.ClientID
	}
	if observed.ServiceAccount.Annotations["azure.workload.identity/client-id"] != clientID {
		return fmt.Errorf("installation workload identity is not current")
	}
	if !state.Bootstrapped && !state.BootstrapReleasePending && !a.Spec.Suspended {
		return fmt.Errorf("bootstrap suspension changed before access was ready")
	}
	return nil
}

func (r *CetiAdminReconciler) saveRecovery(ctx context.Context, checkpoint *corev1.ConfigMap, state *cetiadmin.Installation, agent *api.Agent) error {
	if !identityMatches(state.Agent, agent) {
		return cetiadmin.ErrConflict
	}
	ref := cetiadmin.RecoveryIdentity{Record: identityOf(checkpoint), Source: state.Source, Agent: state.Agent, Namespace: state.Namespace, ServiceAccount: state.ServiceAccount, Resources: state.Resources}
	raw, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	if agent.Annotations[cetiadmin.AnnotationRecovery] == string(raw) {
		return nil
	}
	before := agent.DeepCopy()
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[cetiadmin.AnnotationRecovery] = string(raw)
	return r.patchAndRead(ctx, before, agent)
}

func (r *CetiAdminReconciler) recoveryIdentity(agent *api.Agent) (*cetiadmin.Installation, bool) {
	var ref cetiadmin.RecoveryIdentity
	if agent == nil || json.Unmarshal([]byte(agent.Annotations[cetiadmin.AnnotationRecovery]), &ref) != nil ||
		ref.Agent.UID == "" || ref.Agent != identityOf(agent) || ref.Source.Name != r.Source.Name || ref.Source.Namespace != r.Source.Namespace ||
		ref.Source.UID == "" || ref.Record.Name != r.stateKey().Name || ref.Record.Namespace != r.stateKey().Namespace || ref.Record.UID == "" ||
		agent.Annotations[cetiadmin.AnnotationInstallationUID] != string(ref.Record.UID) {
		return nil, false
	}
	return &cetiadmin.Installation{Record: cetiadmin.Record{SchemaVersion: 1, Source: ref.Source, Agent: ref.Agent, Namespace: ref.Namespace, ServiceAccount: ref.ServiceAccount},
		RecordUID: ref.Record.UID, Resources: ref.Resources, Hold: true, Phase: cetiadmin.PhaseBlocked}, true
}

func (r *CetiAdminReconciler) holdOrphanedInstallation(ctx context.Context) (bool, error) {
	list := &api.AgentList{}
	if err := r.reader().List(ctx, list); err != nil {
		return false, err
	}
	found := false
	var errs []error
	for i := range list.Items {
		state, ok := r.recoveryIdentity(&list.Items[i])
		if !ok {
			continue
		}
		found = true
		errs = append(errs, r.revokeNamespaceAccess(ctx, state), r.withdrawResources(ctx, state))
		_, err := r.stop(ctx, state.Record)
		errs = append(errs, err)
	}
	return found, errors.Join(errs...)
}

func (r *CetiAdminReconciler) BindingForAgent(ctx context.Context, agent *api.Agent) (cetiadmin.Binding, bool, error) {
	if agent == nil || agent.UID == "" {
		return cetiadmin.Binding{}, false, nil
	}
	_, state, err := r.readState(ctx)
	if err != nil {
		_, handled := r.recoveryIdentity(agent)
		if !handled {
			return cetiadmin.Binding{}, false, nil
		}
		return cetiadmin.Binding{}, handled, err
	}
	handled := state.Agent == identityOf(agent)
	if !handled && state.Creation != nil && state.PendingCreate == "Agent" {
		request, creationErr := r.creationObject(state)
		handled = creationErr == nil && sameCetiAdminCreation(request, agent)
	}
	if !handled {
		return cetiadmin.Binding{}, false, nil
	}
	if !state.CanRun() || state.OperatorImage != r.OperatorImage || state.RuntimeVersion != r.ChartVersion || r.AdmissionReady == nil {
		return cetiadmin.Binding{}, true, cetiadmin.ErrUnbound
	}
	if err := r.AdmissionReady(ctx); err != nil {
		return cetiadmin.Binding{}, true, err
	}
	source := &corev1.ConfigMap{}
	if err := r.reader().Get(ctx, r.Source, source); err != nil {
		return cetiadmin.Binding{}, true, err
	}
	config, err := cetiadmin.ParseConfig(source.Data[cetiadmin.ConfigKey])
	if err != nil {
		return cetiadmin.Binding{}, true, err
	}
	observed, err := r.observe(ctx, source, state.Record)
	if err != nil {
		return cetiadmin.Binding{}, true, err
	}
	if observed.Agent.Generation != agent.Generation || !reflect.DeepEqual(observed.Agent.Spec, agent.Spec) {
		return cetiadmin.Binding{}, true, cetiadmin.ErrConflict
	}
	if err := r.validateRetainedObjects(ctx, state); err != nil {
		return cetiadmin.Binding{}, true, err
	}
	if err := r.validateInstalled(state, observed); err != nil {
		return cetiadmin.Binding{}, true, err
	}
	binding, err := cetiadmin.ResolveBinding(config, &state.Record, observed)
	if err != nil {
		return cetiadmin.Binding{}, true, err
	}
	if err := r.migrationSourceStopped(ctx, config); err != nil {
		return cetiadmin.Binding{}, true, err
	}
	if err := r.verifyInstallationAccess(ctx, binding, state); err != nil {
		return cetiadmin.Binding{}, true, err
	}
	return binding, true, nil
}

func (r *CetiAdminReconciler) deactivate(ctx context.Context, checkpoint *corev1.ConfigMap, state *cetiadmin.Installation, reason string) error {
	state.Hold, state.Reason = true, reason
	if state.Phase != cetiadmin.PhaseDisabled {
		state.Phase = cetiadmin.PhaseStopping
	}
	saveErr := r.saveState(ctx, checkpoint, state)
	if state.Creation != nil {
		if err := r.settleCreation(ctx, checkpoint, state); err != nil {
			saveErr = errors.Join(saveErr, err)
		}
	}
	var revokeErr error
	if r.RevokeAccess == nil {
		// Missing wiring is still an error, but must not leave existing grants.
		revokeErr = errors.Join(r.withdrawResources(ctx, state), fmt.Errorf("access revocation callback is unavailable"))
	} else {
		revokeErr = r.RevokeAccess(ctx, state)
	}
	stopped, err := r.stop(ctx, state.Record)
	if err != nil || !stopped || revokeErr != nil {
		return errors.Join(saveErr, revokeErr, err)
	}
	if err := cetiadmin.CheckMaintenanceResolved(ctx, r.reader()); err != nil {
		return errors.Join(saveErr, err)
	}
	state.Phase = cetiadmin.PhaseDisabled
	current, readErr := r.checkpointObject(ctx, state)
	if readErr != nil {
		return errors.Join(saveErr, readErr)
	}
	finalErr := r.saveState(ctx, current, state)
	if finalErr == nil {
		*checkpoint = *current
	}
	return errors.Join(saveErr, finalErr)
}

func (r *CetiAdminReconciler) stop(ctx context.Context, record cetiadmin.Record) (bool, error) {
	if record.Agent.UID == "" {
		return true, nil
	}
	agent := &api.Agent{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: record.Agent.Name}, agent); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}
	if !identityMatches(record.Agent, agent) {
		return false, cetiadmin.ErrConflict
	}
	if record.Namespace.UID == "" {
		return true, nil
	}
	namespace := &corev1.Namespace{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: record.Namespace.Name}, namespace); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}
	if !identityMatches(record.Namespace, namespace) || !hasAgentOwner(namespace, agent) {
		return false, cetiadmin.ErrConflict
	}
	if r.Runtime == nil {
		return false, fmt.Errorf("runtime controller is unavailable")
	}
	workload := &appsv1.StatefulSet{}
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: instanceServiceName(agent.Name)}, workload)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && !attributableRuntimeObject(workload, agent, helmReleaseName(agent.Name)) {
		return false, fmt.Errorf("runtime ownership changed before stop")
	}
	if _, err := r.Runtime.suspendRegistryBuiltin(ctx, agent); err != nil {
		return false, err
	}
	return r.writersStopped(ctx, namespace.Name)
}

func attributableRuntimeObject(object client.Object, agent *api.Agent, release string) bool {
	if object.GetUID() == "" || object.GetDeletionTimestamp() != nil {
		return false
	}
	if hasAgentOwner(object, agent) {
		return true
	}
	return len(object.GetOwnerReferences()) == 0 && object.GetAnnotations()["meta.helm.sh/release-name"] == release &&
		object.GetAnnotations()["meta.helm.sh/release-namespace"] == object.GetNamespace() && object.GetLabels()["app.kubernetes.io/managed-by"] == "Helm"
}

func (r *CetiAdminReconciler) writersStopped(ctx context.Context, namespace string) (bool, error) {
	return namespaceWritersStopped(ctx, r.reader(), namespace)
}

func (r *CetiAdminReconciler) migrationSourceStopped(ctx context.Context, config cetiadmin.Config) error {
	if config.MigrationSource == nil {
		return nil
	}
	source := config.MigrationSource
	agent, namespace := &api.Agent{}, &corev1.Namespace{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: source.Agent.Name}, agent); err != nil {
		return err
	}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: source.Namespace.Name}, namespace); err != nil {
		return err
	}
	if !identityMatches(source.Agent, agent) || !identityMatches(source.Namespace, namespace) || !agent.Spec.Suspended || agent.Status.Phase != api.PhaseSuspended ||
		(agent.Status.AIGateway != nil && (!agent.Status.AIGateway.Blocked || agent.Status.AIGateway.Routed)) {
		return fmt.Errorf("migration source identities and retained suspension must be current")
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		if r.Runtime == nil {
			return cetiadmin.ErrUnbound
		}
		receipt, err := r.Runtime.readCustomOwnership(ctx, agent)
		if err != nil {
			return err
		}
		if receipt.Namespace != namespace.Name || receipt.NamespaceUID != namespace.UID {
			return cetiadmin.ErrConflict
		}
	} else if !hasAgentOwner(namespace, agent) {
		return cetiadmin.ErrConflict
	}
	stopped, err := r.writersStopped(ctx, namespace.Name)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("migration source writers have not stopped")
	}
	return nil
}
