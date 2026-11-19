// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"time"

	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

type SandboxRuntimePrerequisite = sandboxplatform.RuntimePrerequisite

type SandboxStoragePrerequisite = sandboxplatform.StoragePrerequisite

type SandboxConfigurationSnapshot = sandboxplatform.Snapshot

type SandboxConfigurationSource = sandboxplatform.Source

type SandboxConfigurationSourceFunc func(context.Context) (SandboxConfigurationSnapshot, error)

func (f SandboxConfigurationSourceFunc) Snapshot(ctx context.Context) (SandboxConfigurationSnapshot, error) {
	return f(ctx)
}

type SandboxReconciler struct {
	client.Client
	NativeExecution bool
	APIReader       client.Reader
	Source          SandboxConfigurationSource
	Now             func() time.Time
	MaxSnapshotAge  time.Duration
}

type AgentPolicyReconciler struct {
	client.Client
	NativeExecution bool
	APIReader       client.Reader
	Source          SandboxConfigurationSource
	Now             func() time.Time
	MaxSnapshotAge  time.Duration
}

// TeamReconciler records whether the Team's policy and Sandbox bindings are
// structurally complete.
type TeamReconciler struct {
	client.Client
	NativeExecution bool
	APIReader       client.Reader
	Source          SandboxConfigurationSource
	Now             func() time.Time
	MaxSnapshotAge  time.Duration
}

func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).For(&api.Sandbox{}).Complete(r)
}

func (r *AgentPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).For(&api.AgentPolicy{}).Complete(r)
}

func (r *TeamReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Team{}, builder.WithPredicates(ConfigurationChangePredicate())).
		Watches(&api.AgentPolicy{}, handler.EnqueueRequestsFromMapFunc(r.enqueueTeamsForAgentPolicy), builder.WithPredicates(ConfigurationChangePredicate())).
		Watches(&api.Sandbox{}, handler.EnqueueRequestsFromMapFunc(r.enqueueTeamsForSandbox), builder.WithPredicates(ConfigurationChangePredicate())).
		Complete(r)
}

// ConfigurationChangePredicate prevents status writes from immediately
// requeueing configuration reconcilers. Creates and deletes remain observable;
// updates are meaningful only when the persisted configuration identity,
// generation, or deletion state changes.
func ConfigurationChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(update event.UpdateEvent) bool {
			if update.ObjectOld == nil || update.ObjectNew == nil {
				return true
			}
			return update.ObjectOld.GetGeneration() != update.ObjectNew.GetGeneration() ||
				update.ObjectOld.GetUID() != update.ObjectNew.GetUID() ||
				!reflect.DeepEqual(update.ObjectOld.GetDeletionTimestamp(), update.ObjectNew.GetDeletionTimestamp())
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func (r *TeamReconciler) enqueueTeams(ctx context.Context, matches func(*api.Team) bool) []reconcile.Request {
	teams := &api.TeamList{}
	if err := r.List(ctx, teams); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(teams.Items))
	for i := range teams.Items {
		if matches(&teams.Items[i]) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: teams.Items[i].Name}})
		}
	}
	return requests
}

func (r *TeamReconciler) enqueueTeamsForAgentPolicy(ctx context.Context, object client.Object) []reconcile.Request {
	return r.enqueueTeams(ctx, func(team *api.Team) bool {
		return team.Spec.AgentPolicyRef == object.GetName()
	})
}

func (r *TeamReconciler) enqueueTeamsForSandbox(ctx context.Context, object client.Object) []reconcile.Request {
	return r.enqueueTeams(ctx, func(team *api.Team) bool {
		return slices.Contains(team.Spec.SandboxRefs, object.GetName())
	})
}

type invalidTeamConfigurationError struct{ err error }

func (e *invalidTeamConfigurationError) Error() string { return e.err.Error() }
func (e *invalidTeamConfigurationError) Unwrap() error { return e.err }

func invalidTeamConfiguration(format string, args ...any) error {
	return &invalidTeamConfigurationError{err: fmt.Errorf(format, args...)}
}

func isInvalidTeamConfiguration(err error) bool {
	var invalid *invalidTeamConfigurationError
	return errors.As(err, &invalid)
}

func (r *TeamReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	team := &api.Team{}
	if err := r.Get(ctx, request.NamespacedName, team); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := team.DeepCopy()
	now := configurationNow(r.Now)
	var observeErr error
	condition := configurationCondition(metav1.ConditionTrue, "Valid", "Team Policy and Sandbox bindings are valid.")
	if team.DeletionTimestamp != nil {
		condition = configurationCondition(metav1.ConditionFalse, "Terminating", "Team configuration is terminating.")
	} else if err := r.validate(ctx, team); err != nil {
		if isInvalidTeamConfiguration(err) {
			condition = configurationCondition(metav1.ConditionFalse, "InvalidConfiguration", err.Error())
		} else {
			condition = configurationCondition(metav1.ConditionUnknown, "DependenciesUnavailable", "Team Policy or Sandbox bindings cannot currently be verified.")
			observeErr = err
		}
	}
	condition.Type, condition.ObservedGeneration, condition.LastTransitionTime = api.ConditionTeamAccepted, team.Generation, metav1.NewTime(now)
	apimeta.SetStatusCondition(&team.Status.Conditions, condition)
	team.Status.ObservedGeneration = team.Generation
	if team.DeletionTimestamp != nil {
		team.Status.Compatibility = nil
	} else {
		compatibility, compatibilityErr := r.assessCompatibility(ctx, team, now)
		if compatibilityErr != nil {
			team.Status.Compatibility = r.unknownCompatibility(team, now)
			observeErr = errors.Join(observeErr, compatibilityErr)
		} else {
			team.Status.Compatibility = compatibility
		}
	}
	if !reflect.DeepEqual(before.Status, team.Status) {
		if err := r.Status().Patch(ctx, team, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, errors.Join(observeErr, err)
		}
	}
	if observeErr != nil {
		return ctrl.Result{}, observeErr
	}
	if team.Spec.PolicyConfigured() && team.DeletionTimestamp == nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// assessCompatibility reports the saved Team configuration only. It never
// releases an execution hold or replaces the Agent controller's authorization
// and enforcement checks.
func (r *TeamReconciler) assessCompatibility(ctx context.Context, team *api.Team, now time.Time) (*api.TeamCompatibilityStatus, error) {
	if !team.Spec.PolicyConfigured() {
		return nil, nil
	}
	reader := configurationReader(r.APIReader, r.Client)
	resources := sandbox.Resources{Sandboxes: map[string]*api.Sandbox{}, AgentPolicies: map[string]*api.AgentPolicy{}}
	if team.Spec.AgentPolicyRef != "" {
		policy := &api.AgentPolicy{}
		if err := reader.Get(ctx, types.NamespacedName{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("read Team AgentPolicy compatibility input: %w", err)
			}
		} else {
			resources.AgentPolicies[policy.Name] = policy
		}
	}
	for _, name := range team.Spec.SandboxRefs {
		box := &api.Sandbox{}
		if err := reader.Get(ctx, types.NamespacedName{Name: name}, box); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("read Team Sandbox compatibility input: %w", err)
			}
			continue
		}
		resources.Sandboxes[box.Name] = box
	}
	if r.Source == nil {
		return r.unknownCompatibility(team, now), nil
	}
	snapshot, err := r.Source.Snapshot(ctx)
	if err != nil {
		return r.unknownCompatibility(team, now), nil
	}
	// Capture the assessment time after Snapshot: a synchronous source may have
	// observed its inventory between the caller's previous clock read and now.
	now = configurationNow(r.Now)
	if err := configurationSnapshotTime(snapshot.ObservedAt, now, r.MaxSnapshotAge); err != nil {
		return r.unknownCompatibility(team, now), nil
	}
	report := sandbox.TeamCompatibilityReport(team, resources, snapshot.Registrations, r.NativeExecution)
	if err := r.assessNativeCompatibility(ctx, &report, resources, snapshot); err != nil {
		if errors.Is(err, sandboxpolicy.ErrUnavailable) {
			return r.unknownCompatibility(team, now), nil
		}
		return nil, err
	}
	report.CheckedAt, report.ValidUntil = metav1.NewTime(now), metav1.NewTime(now.Add(30*time.Second))
	if err := r.verifyCompatibilityInputs(ctx, team, resources, snapshot, report); err != nil {
		return r.unknownCompatibility(team, now), nil
	}
	return &report, nil
}

func (r *TeamReconciler) assessNativeCompatibility(ctx context.Context, report *api.TeamCompatibilityStatus, resources sandbox.Resources, snapshot SandboxConfigurationSnapshot) error {
	policy := resources.AgentPolicies[report.Policy.Name]
	if policy == nil {
		return nil
	}
	reader := configurationReader(r.APIReader, r.Client)
	for index := range report.Sandboxes {
		status := &report.Sandboxes[index]
		box := resources.Sandboxes[status.Sandbox.Name]
		if box == nil || status.State == api.CompatibilityIncompatible {
			continue
		}
		normalized, errs := sandbox.NormalizeSandbox(box.Spec)
		if len(errs) != 0 {
			continue
		}
		registration, found := snapshot.Registrations[normalized.Type]
		if !found {
			continue
		}
		platform, found := snapshot.Platforms[normalized.Type]
		if !found {
			return sandboxpolicy.ErrUnavailable
		}
		if err := sandboxpolicy.ValidateNativeTeamPolicyBinding(ctx, policy, registration.Capabilities, platform, reader, r.NativeExecution); err != nil {
			if errors.Is(err, sandboxpolicy.ErrUnavailable) {
				return err
			}
			code := "sandbox_incompatible"
			if policy.Spec.KubernetesAccess != nil {
				code = "kubernetes_access_unsupported"
			}
			message, _ := api.CompatibilityIssueMessage(code)
			issue := api.CompatibilityIssue{Code: code, Field: "spec.agentPolicyRef", Message: message,
				Team: &report.Team, Policy: &report.Policy, Sandbox: &status.Sandbox}
			status.State, status.Reason = api.CompatibilityIncompatible, "PlatformControlUnsupported"
			status.Issues = append(status.Issues, issue)
			report.Issues = append(report.Issues, issue)
			report.State, report.Reason, report.Message = api.CompatibilityIncompatible, "ConfigurationIncompatible", "The Team Policy and selected Sandboxes are incompatible."
		}
	}
	return nil
}

func (r *TeamReconciler) verifyCompatibilityInputs(ctx context.Context, team *api.Team, resources sandbox.Resources, snapshot SandboxConfigurationSnapshot, report api.TeamCompatibilityStatus) error {
	reader := configurationReader(r.APIReader, r.Client)
	if team.Spec.AgentPolicyRef != "" {
		expected := resources.AgentPolicies[team.Spec.AgentPolicyRef]
		current := &api.AgentPolicy{}
		err := reader.Get(ctx, types.NamespacedName{Name: team.Spec.AgentPolicyRef}, current)
		if (expected == nil && !apierrors.IsNotFound(err)) || (expected != nil && (err != nil || current.UID != expected.UID || current.Generation != expected.Generation || !current.DeletionTimestamp.IsZero())) {
			return fmt.Errorf("Team compatibility AgentPolicy changed during assessment")
		}
	}
	for _, name := range team.Spec.SandboxRefs {
		expected := resources.Sandboxes[name]
		current := &api.Sandbox{}
		err := reader.Get(ctx, types.NamespacedName{Name: name}, current)
		if (expected == nil && !apierrors.IsNotFound(err)) || (expected != nil && (err != nil || current.UID != expected.UID || current.Generation != expected.Generation || !current.DeletionTimestamp.IsZero())) {
			return fmt.Errorf("Team compatibility Sandbox changed during assessment")
		}
	}
	if r.Source == nil {
		return fmt.Errorf("Team compatibility platform inventory is unavailable")
	}
	current, err := r.Source.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err := configurationSnapshotTime(current.ObservedAt, configurationNow(r.Now), r.MaxSnapshotAge); err != nil {
		return err
	}
	snapshot.ObservedAt, current.ObservedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(snapshot, current) {
		return fmt.Errorf("Team compatibility platform inventory changed during assessment")
	}
	if policy := resources.AgentPolicies[team.Spec.AgentPolicyRef]; policy != nil && policy.Spec.KubernetesAccess != nil {
		// The initial assessment has already classified static platform controls.
		// Re-read grants only for bindings that were compatible then, so a stable
		// unsupported control remains an actionable Incompatible result rather
		// than being downgraded to Unknown by this race check.
		for _, status := range report.Sandboxes {
			if status.State != api.CompatibilityCompatible {
				continue
			}
			box := resources.Sandboxes[status.Sandbox.Name]
			if box == nil {
				continue
			}
			normalized, errs := sandbox.NormalizeSandbox(box.Spec)
			if len(errs) != 0 {
				continue
			}
			registration, registered := current.Registrations[normalized.Type]
			platform, supported := current.Platforms[normalized.Type]
			if !registered || !supported {
				return sandboxpolicy.ErrUnavailable
			}
			if err := sandboxpolicy.ValidateNativeTeamPolicyBinding(ctx, policy, registration.Capabilities, platform, reader, r.NativeExecution); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *TeamReconciler) unknownCompatibility(team *api.Team, now time.Time) *api.TeamCompatibilityStatus {
	if !team.Spec.PolicyConfigured() {
		return nil
	}
	report := &api.TeamCompatibilityStatus{
		State: api.CompatibilityUnknown, Reason: "DependenciesUnavailable",
		Message:   "Current platform compatibility cannot be verified.",
		Team:      api.CompatibilityReference{Name: team.Name, UID: string(team.UID), Generation: team.Generation},
		Policy:    api.CompatibilityReference{Name: team.Spec.AgentPolicyRef},
		CheckedAt: metav1.NewTime(now), ValidUntil: metav1.NewTime(now.Add(30 * time.Second)),
	}
	for _, name := range team.Spec.SandboxRefs {
		report.Sandboxes = append(report.Sandboxes, api.SandboxCompatibilityStatus{
			Sandbox: api.CompatibilityReference{Name: name}, State: api.CompatibilityUnknown, Reason: "DependenciesUnavailable",
		})
	}
	return report
}

func (r *TeamReconciler) validate(ctx context.Context, team *api.Team) error {
	if !team.Spec.PolicyConfigured() {
		return nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if team.Spec.AgentPolicyRef == "" || team.Spec.DefaultSandboxRef == "" || len(team.Spec.SandboxRefs) == 0 || !slices.Contains(team.Spec.SandboxRefs, team.Spec.DefaultSandboxRef) {
		return invalidTeamConfiguration("Team policy and Sandbox bindings are incomplete")
	}
	policy := &api.AgentPolicy{}
	if err := reader.Get(ctx, types.NamespacedName{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return invalidTeamConfiguration("AgentPolicy %q does not exist", team.Spec.AgentPolicyRef)
		}
		return fmt.Errorf("AgentPolicy is unavailable: %w", err)
	}
	for _, name := range team.Spec.SandboxRefs {
		box := &api.Sandbox{}
		if err := reader.Get(ctx, types.NamespacedName{Name: name}, box); err != nil {
			if apierrors.IsNotFound(err) {
				return invalidTeamConfiguration("Sandbox %q does not exist", name)
			}
			return fmt.Errorf("Sandbox %q is unavailable: %w", name, err)
		}
	}
	return nil
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &api.Sandbox{}
	reader := configurationReader(r.APIReader, r.Client)
	if err := r.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := object.DeepCopy()
	condition, observedAt, observeErr := assessSandboxPrerequisites(ctx, reader, object, r.Source, r.MaxSnapshotAge, r.Now, r.NativeExecution)
	now := configurationNow(r.Now)
	if observeErr == nil && !observedAt.IsZero() {
		if err := configurationSnapshotTime(observedAt, now, r.MaxSnapshotAge); err != nil {
			condition, observeErr = configurationErrorCondition(err), err
		}
	}
	condition.Type, condition.ObservedGeneration = api.ConditionReady, object.Generation
	condition.LastTransitionTime = metav1.NewTime(now)
	apimeta.SetStatusCondition(&object.Status.Conditions, condition)
	object.Status.ObservedGeneration = object.Generation
	if !reflect.DeepEqual(before.Status, object.Status) {
		if err := r.Status().Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, errors.Join(observeErr, fmt.Errorf("update Sandbox prerequisite status: %w", err))
		}
	}
	if observeErr != nil {
		return ctrl.Result{}, observeErr
	}
	return ctrl.Result{RequeueAfter: configurationRefresh(r.MaxSnapshotAge, now, observedAt)}, nil
}

func (r *AgentPolicyReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &api.AgentPolicy{}
	reader := configurationReader(r.APIReader, r.Client)
	if err := r.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := object.DeepCopy()
	condition, observedAt, observeErr := r.assess(ctx, reader, object)
	now := configurationNow(r.Now)
	if observeErr == nil && !observedAt.IsZero() {
		if err := configurationSnapshotTime(observedAt, now, r.MaxSnapshotAge); err != nil {
			condition, observeErr = configurationErrorCondition(err), err
		}
	}
	condition.Type, condition.ObservedGeneration = api.ConditionAgentPolicyAccepted, object.Generation
	condition.LastTransitionTime = metav1.NewTime(now)
	apimeta.SetStatusCondition(&object.Status.Conditions, condition)
	object.Status.ObservedGeneration = object.Generation
	if !reflect.DeepEqual(before.Status, object.Status) {
		if err := r.Status().Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, errors.Join(observeErr, fmt.Errorf("update AgentPolicy configuration status: %w", err))
		}
		emitSandboxPolicyTransition(ctx, before, object)
	}
	if observeErr != nil {
		return ctrl.Result{}, observeErr
	}
	return ctrl.Result{RequeueAfter: configurationRefresh(r.MaxSnapshotAge, now, observedAt)}, nil
}

func assessSandboxPrerequisites(ctx context.Context, reader client.Reader, object *api.Sandbox,
	source SandboxConfigurationSource, maxAge time.Duration, clock func() time.Time, nativeExecution bool,
) (metav1.Condition, time.Time, error) {
	if !object.DeletionTimestamp.IsZero() {
		return configurationCondition(metav1.ConditionFalse, "Terminating", "Sandbox configuration is terminating."), time.Time{}, nil
	}
	spec, errs := sandbox.NormalizeSandbox(object.Spec)
	if len(errs) != 0 {
		return configurationCondition(metav1.ConditionFalse, "InvalidConfiguration", "Sandbox configuration is invalid."), time.Time{}, nil
	}
	snapshot, err := configurationSnapshot(ctx, source, clock, maxAge)
	if err != nil {
		return configurationCondition(metav1.ConditionUnknown, "PrerequisitesUnavailable", "Current platform prerequisite state is unavailable."), time.Time{}, err
	}
	registration, exists := snapshot.Registrations[spec.Type]
	if !exists || sandbox.CheckExecutionProfile(registration.Capabilities, nativeExecution) != nil ||
		!runtimePrerequisitesSupported(spec, registration.Capabilities) {
		return configurationCondition(metav1.ConditionFalse, "UnsupportedControl", "Sandbox runtime and mandatory controls are not supported by the configured execution profile."), snapshot.ObservedAt, nil
	}
	if !registration.Available {
		return configurationCondition(metav1.ConditionFalse, "RuntimeUnavailable", "The registered Sandbox runtime is unavailable."), snapshot.ObservedAt, nil
	}
	platform, exists := snapshot.Platforms[spec.Type]
	if !exists {
		return configurationCondition(metav1.ConditionUnknown, "PrerequisitesUnavailable", "Trusted network topology and grants are unavailable."), snapshot.ObservedAt, nil
	}
	if err := sandboxpolicy.ValidatePlatform(platform, registration.Capabilities); err != nil {
		return configurationErrorCondition(err), snapshot.ObservedAt, nil
	}
	runtimeName := registration.Capabilities.RuntimeClassName
	expectedRuntime, exists := snapshot.RuntimeClasses[runtimeName]
	if !exists || expectedRuntime.UID == "" || expectedRuntime.Handler == "" {
		return configurationCondition(metav1.ConditionUnknown, "PrerequisitesUnavailable", "RuntimeClass identity approval is unavailable."), snapshot.ObservedAt, nil
	}
	runtimeClass := &nodev1.RuntimeClass{}
	if err := reader.Get(ctx, types.NamespacedName{Name: runtimeName}, runtimeClass); err != nil {
		condition, readErr := prerequisiteReadError(err)
		return condition, snapshot.ObservedAt, readErr
	}
	if runtimeClass.UID != expectedRuntime.UID || runtimeClass.Handler != expectedRuntime.Handler ||
		!runtimeClass.DeletionTimestamp.IsZero() {
		return configurationCondition(metav1.ConditionFalse, "ReferenceChanged", "RuntimeClass no longer matches the approved prerequisite."), snapshot.ObservedAt, nil
	}
	expectedStorage, exists := snapshot.StorageClasses[spec.Storage.Home.StorageClassName]
	if !exists || expectedStorage.UID == "" || expectedStorage.Provisioner == "" {
		return configurationCondition(metav1.ConditionFalse, "UnapprovedStorage", "The home StorageClass is not approved."), snapshot.ObservedAt, nil
	}
	storageClass := &storagev1.StorageClass{}
	if err := reader.Get(ctx, types.NamespacedName{Name: spec.Storage.Home.StorageClassName}, storageClass); err != nil {
		condition, readErr := prerequisiteReadError(err)
		return condition, snapshot.ObservedAt, readErr
	}
	if storageClass.UID != expectedStorage.UID || storageClass.Provisioner != expectedStorage.Provisioner ||
		!storageClass.DeletionTimestamp.IsZero() {
		return configurationCondition(metav1.ConditionFalse, "ReferenceChanged", "StorageClass no longer matches the approved prerequisite."), snapshot.ObservedAt, nil
	}
	message := "Registered runtime, storage and control prerequisites are available; Agent enforcement is checked separately."
	if registration.Capabilities.Profile == sandbox.ProfileNativeSingleStart {
		message = "Native single-start runtime, storage and baseline network prerequisites are available; application and Node readiness are checked separately, without external observation or physical fencing."
	}
	return configurationCondition(metav1.ConditionTrue, "PrerequisitesReady", message), snapshot.ObservedAt, nil
}

func (r *AgentPolicyReconciler) assess(ctx context.Context, reader client.Reader, object *api.AgentPolicy) (metav1.Condition, time.Time, error) {
	if !object.DeletionTimestamp.IsZero() {
		return configurationCondition(metav1.ConditionFalse, "Terminating", "AgentPolicy configuration is terminating."), time.Time{}, nil
	}
	if _, errs := sandbox.NormalizeAgentPolicy(object.Spec); len(errs) != 0 {
		return configurationCondition(metav1.ConditionFalse, "InvalidConfiguration", "AgentPolicy configuration is invalid."), time.Time{}, nil
	}
	snapshot, err := configurationSnapshot(ctx, r.Source, r.Now, r.MaxSnapshotAge)
	if err != nil {
		return configurationCondition(metav1.ConditionUnknown, "PrerequisitesUnavailable", "Current platform support inventory is unavailable."), time.Time{}, err
	}
	keys := make([]api.SandboxType, 0, len(snapshot.Registrations))
	for key := range snapshot.Registrations {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var failures []error
	for _, key := range keys {
		platform, exists := snapshot.Platforms[key]
		if !exists {
			failures = append(failures, fmt.Errorf("registration %s: %w: platform inventory is missing", key, sandboxpolicy.ErrUnavailable))
			continue
		}
		capabilities := snapshot.Registrations[key].Capabilities
		if errs := sandbox.CheckRuntimeCapabilities(key, capabilities); len(errs) != 0 {
			failures = append(failures, fmt.Errorf("registration %s: %w: %v", key, sandboxpolicy.ErrUnsupportedControl, errs.ToAggregate()))
			continue
		}
		if err := sandbox.CheckExecutionProfile(capabilities, r.NativeExecution); err != nil {
			failures = append(failures, fmt.Errorf("registration %s: %w: %v", key, sandboxpolicy.ErrUnsupportedControl, err))
			continue
		}
		err := sandboxpolicy.ValidatePolicyConfiguration(object, capabilities, platform)
		if err == nil {
			err = sandboxpolicy.VerifyPolicyGrants(ctx, reader, object, platform.Grants)
		}
		if err == nil {
			return configurationCondition(metav1.ConditionTrue, "PolicyAccepted", "Standalone policy support and requested role approvals match a registered adapter; Sandbox compatibility, home access and per-Agent enforcement are not assessed."), snapshot.ObservedAt, nil
		}
		failures = append(failures, fmt.Errorf("registration %s: %w", key, err))
	}
	if len(failures) == 0 {
		failures = append(failures, sandboxpolicy.ErrUnsupportedControl)
	}
	unsupported := errors.Join(failures...)
	condition := configurationErrorCondition(unsupported)
	if errors.Is(unsupported, sandboxpolicy.ErrUnavailable) {
		return condition, snapshot.ObservedAt, unsupported
	}
	return condition, snapshot.ObservedAt, nil
}

func runtimePrerequisitesSupported(spec api.SandboxSpec, capabilities sandbox.Capabilities) bool {
	return len(validation.IsDNS1123Subdomain(capabilities.RuntimeClassName)) == 0 && capabilities.Revision != "" &&
		capabilities.HostResourceLimits && len(sandbox.CheckNetworkCapabilities(capabilities)) == 0 &&
		len(sandbox.CheckRuntimeCapabilities(spec.Type, capabilities)) == 0 &&
		(!capabilities.RequirePerContainerResourceLimits || capabilities.PerContainerResourceLimits) &&
		slices.Contains(capabilities.HomeAccessModes, spec.Storage.Home.AccessMode)
}

func prerequisiteReadError(err error) (metav1.Condition, error) {
	if apierrors.IsNotFound(err) {
		return configurationCondition(metav1.ConditionFalse, "ReferenceMissing", "A registered runtime or storage prerequisite is missing."), nil
	}
	return configurationCondition(metav1.ConditionUnknown, "PrerequisitesUnavailable", "A current runtime or storage prerequisite could not be read."), err
}

func configurationErrorCondition(err error) metav1.Condition {
	switch {
	case errors.Is(err, sandboxpolicy.ErrUnavailable):
		return configurationCondition(metav1.ConditionUnknown, "PrerequisitesUnavailable", "Current trusted prerequisite bindings are unavailable.")
	case errors.Is(err, sandboxpolicy.ErrUnapprovedGrant):
		return configurationCondition(metav1.ConditionFalse, "UnapprovedGrant", "A requested identity or role grant is not approved.")
	case errors.Is(err, sandboxpolicy.ErrConflict):
		return configurationCondition(metav1.ConditionFalse, "PolicyConflict", "Policy rules conflict with required platform controls.")
	default:
		return configurationCondition(metav1.ConditionFalse, "UnsupportedControl", "A requested control has no supported, qualified implementation.")
	}
}

func configurationCondition(status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Status: status, Reason: reason, Message: message}
}

func configurationReader(reader client.Reader, fallback client.Client) client.Reader {
	if reader != nil {
		return reader
	}
	// Standalone/offline callers may supply a direct client. Manager setup always
	// installs its uncached APIReader for identity checks; primary CRs use Client
	// and optimistic status patches, not the authoritative prerequisite reader.
	return fallback
}

func configurationNow(clock func() time.Time) time.Time {
	if clock != nil {
		return clock()
	}
	return time.Now()
}

// Successful assessments refresh in [1/2, 5/8] of the snapshot's remaining
// lifetime. Errors use controller-runtime backoff, not this periodic requeue.
func configurationRefresh(maxAge time.Duration, now, observedAt time.Time) time.Duration {
	if maxAge <= 0 {
		maxAge = 30 * time.Second
	}
	remaining := maxAge
	if !observedAt.IsZero() {
		remaining -= now.Sub(observedAt)
	}
	base, jitter := remaining/2, remaining/8
	if jitter > 0 {
		base += time.Duration(rand.Int64N(int64(jitter) + 1))
	}
	return max(time.Nanosecond, base)
}

func configurationSnapshot(ctx context.Context, source SandboxConfigurationSource, clock func() time.Time, maxAge time.Duration) (SandboxConfigurationSnapshot, error) {
	if source == nil {
		return SandboxConfigurationSnapshot{}, sandboxpolicy.ErrUnavailable
	}
	snapshot, err := source.Snapshot(ctx)
	if err != nil {
		return SandboxConfigurationSnapshot{}, err
	}
	// A synchronous source observes after the caller starts the request.
	// Sampling the clock before Snapshot would reject fresh data as future-dated.
	if err := configurationSnapshotTime(snapshot.ObservedAt, configurationNow(clock), maxAge); err != nil {
		return SandboxConfigurationSnapshot{}, err
	}
	return snapshot, nil
}

func configurationSnapshotTime(observedAt, now time.Time, maxAge time.Duration) error {
	return sandboxplatform.CheckFreshness(observedAt, now, maxAge)
}
