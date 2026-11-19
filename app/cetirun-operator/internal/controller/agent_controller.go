// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	crbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/builtin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	"github.com/Azure/CetiRun/app/internal/registryauth"
)

// AgentReconciler reconciles Agent objects.
//
// +kubebuilder:rbac:groups=cetirun.microsoft.com,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cetirun.microsoft.com,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cetirun.microsoft.com,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=cetirun.microsoft.com,resources=teams,verbs=get;list;watch
// +kubebuilder:rbac:groups=cetirun.microsoft.com,resources=teams/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cetirun.microsoft.com,resources=sandboxes;agentpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=cetirun.microsoft.com,resources=sandboxes/status;agentpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;delete
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices;sidecars;destinationrules,verbs=get;list;watch;create;update;patch;delete
type AgentReconciler struct {
	client.Client
	APIReader                   client.Reader
	Scheme                      *runtime.Scheme
	EventRecorder               record.EventRecorder
	HelmClient                  HelmInstaller
	OperatorNamespace           string
	OperatorSAName              string
	ChartVersion                string
	IstioRevision               string // explicit override; if empty, auto-detected from aks-istio-system
	IstioGateway                string // namespace/name of the Gateway used by instance VirtualServices
	DomainSuffix                string
	SkillsStorageAccount        string
	CetiRunHelmValues           map[string]interface{} // Operator-level Helm value overrides for all instances
	MemoryImage                 string
	MemoryEnabled               bool
	MemoryRegistry              string
	ChartRegistry               string // Registry host and repository prefix, derived from the runtime chart URL.
	DefaultModel                string // Default model when spec.model is empty
	CustomBindings              CustomBindingProvider
	ChatAccess                  chataccess.Config
	SandboxPreparation          *SandboxPreparation
	SandboxNativeCoordination   *sandboxcoordination.Store
	RegistryCredentialNamespace string
	CetiAdmin                   CetiAdminBindingProvider

	// SkillMarketplace is the CETIRUN_SKILL_MARKETPLACE switch: every Team and
	// Agent skill entry without the npm: prefix names a Skill Marketplace skill.
	SkillMarketplace bool

	// Microsoft Entra Agent Identity deployment configuration.
	AgentIdentityEnabled               bool
	AgentIdentityTenantID              string
	AgentIdentityBlueprintClientID     string
	AgentIdentityNamePrefix            string
	AgentIdentityBrokerURL             string
	AgentIdentityHTTPClient            *http.Client
	ConnectionsMCPURL                  string
	ConnectionsMCPAudience             string
	ConnectionsMCPCASecretName         string
	ConnectionsMCPRuntimeTypes         []string
	ConnectionsMCPBrokerNamespace      string
	ConnectionsMCPBrokerServiceAccount string
	ConnectionsMCPNativeEnabled        bool
	// agentIdentityGraphClientFactory is test-only dependency injection. A nil
	// factory constructs the deployment's workload-identity-backed Graph client.
	agentIdentityGraphClientFactory func() (agentIdentityDirectoryClient, error)

	// AI Gateway deployment configuration for per-Agent virtual keys.
	AIGateway           AIGatewayConfig
	AIGatewayHTTPClient *http.Client
	// aiGatewayClientFactory is test-only dependency injection. A nil factory
	// reads the master key Secret and calls the configured Gateway.
	aiGatewayClientFactory func(context.Context) (aiGatewayKeyClient, error)
	// aiGatewayCatalog is the operator's view of the Gateway's models, kept
	// fresh by reconciles and the catalog publisher.
	aiGatewayCatalog aiGatewayCatalog
	aiGatewayOwners  aiGatewayOwnerCache
	aiGatewayPrices  aiGatewayPriceCache
	// aiGatewayChartRouting is what the default agent chart routes through
	// the Gateway, for the catalog.
	aiGatewayChartRouting aiGatewayChartRouting
	// aiGatewayEvents queues Agents after a shared-model change.
	aiGatewayEvents chan event.GenericEvent

	istioMu          sync.Mutex
	istioRevResolved string
	customPlans      sync.Map
}

// SetupWithManager registers the reconciler with the controller manager.
// instanceTeamIndex is the field index used to look up Agents by
// spec.team without scanning the full cluster on every team event.
const instanceTeamIndex = "spec.team"

const gatewaySecretResourceVersionAnnotation = "cetirun.microsoft.com/gateway-secret-resource-version"

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &cetirunv1alpha1.Agent{}, registrySourceIndex, func(obj client.Object) []string {
		agent := obj.(*cetirunv1alpha1.Agent)
		if agent.Spec.Agent.RegistrySecretRef == nil {
			return nil
		}
		return []string{agent.Spec.Agent.RegistrySecretRef.Name}
	}); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&cetirunv1alpha1.Agent{},
		instanceTeamIndex,
		func(obj client.Object) []string {
			inst, ok := obj.(*cetirunv1alpha1.Agent)
			if !ok || inst.Spec.Team == "" {
				return nil
			}
			return []string{inst.Spec.Team}
		},
	); err != nil {
		return fmt.Errorf("index instance by spec.team: %w", err)
	}
	if err := indexSandboxDependencies(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&cetirunv1alpha1.Agent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&istiov1beta1.VirtualService{}).
		Owns(&istiov1beta1.DestinationRule{}).
		WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAgentSecret)).
		Watches(
			&cetirunv1alpha1.Team{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueInstancesForTeam),
			crbuilder.WithPredicates(AgentTeamRuntimeInputPredicate()),
		).
		Watches(
			&cetirunv1alpha1.Sandbox{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueInstancesForSandbox),
		).
		Watches(
			&cetirunv1alpha1.AgentPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueInstancesForAgentPolicy),
		)
	if r.SandboxPreparation != nil && r.SandboxPreparation.NativeExecution {
		builder = builder.WatchesMetadata(&istiov1beta1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.enqueueIngressMTLSSource))
	}
	if r.AIGateway.Enabled {
		if r.aiGatewayEvents == nil {
			r.aiGatewayEvents = make(chan event.GenericEvent, 1024)
		}
		builder = builder.WatchesRawSource(source.Channel(r.aiGatewayEvents, &handler.EnqueueRequestForObject{}))
		if err := mgr.Add(&AIGatewayCatalogPublisher{Reconciler: r}); err != nil {
			return fmt.Errorf("add AI Gateway catalog publisher: %w", err)
		}
	}
	if access, ok := r.CetiAdmin.(interface {
		EnqueueRuntimeAccess(context.Context, client.Object) []reconcile.Request
	}); ok {
		builder = builder.Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(access.EnqueueRuntimeAccess))
	}
	return builder.
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}

// AgentTeamRuntimeInputPredicate suppresses diagnostic-only Team status
// refreshes. It deliberately retains every other status and metadata field:
// those can affect runtime policy or lifecycle behavior even when Team spec
// generation is unchanged.
func AgentTeamRuntimeInputPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(update event.UpdateEvent) bool {
			oldTeam, oldOK := update.ObjectOld.(*cetirunv1alpha1.Team)
			newTeam, newOK := update.ObjectNew.(*cetirunv1alpha1.Team)
			if !oldOK || !newOK {
				return true
			}
			return !equality.Semantic.DeepEqual(normalizedTeamRuntimeInputs(oldTeam), normalizedTeamRuntimeInputs(newTeam))
		},
	}
}

// Reconcile implements the reconciliation loop.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	logger := log.FromContext(ctx).WithValues("name", req.Name)

	// 1. Fetch the CR.
	instance := &cetirunv1alpha1.Agent{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			r.customPlans.Delete(req.Name)
			logger.V(1).Info("Agent not found, skipping")
			return ctrl.Result{}, r.reconcileCetiAdminAccess(ctx, req.Name)
		}
		return ctrl.Result{}, fmt.Errorf("get Agent: %w", err)
	}
	// The phase stored when this reconcile started. Lifecycle events report
	// transitions from it, so that the periodic full reconcile of an Agent
	// at steady state records no events.
	startPhase := instance.Status.Phase
	// Management access must not prevent this Agent's stop, cleanup or repair.
	defer func() {
		if err := r.reconcileCetiAdminAccess(ctx, instance.Name); err != nil {
			logger.Error(err, "CetiAdmin namespace access needs reconciliation")
			reconcileErr = stderrors.Join(reconcileErr, err)
		}
	}()

	if ns := instanceNamespace(instance.Name); ns == r.registryNamespace() || ns == registryauth.DefaultNamespace {
		return r.rejectReservedRegistryInstance(ctx, instance)
	}

	// 2. Handle deletion.
	if !instance.DeletionTimestamp.IsZero() {
		if allowed, result, err := r.reconcileMigration(ctx, instance, false); !allowed {
			return result, err
		}
		recorded, readErr := r.recordedReconciledSandbox(ctx, instance)
		if readErr == nil && recorded {
			return r.handleDelete(ctx, instance)
		}
		if controllerutil.ContainsFinalizer(instance, sandbox.PreparationFinalizer) ||
			controllerutil.ContainsFinalizer(instance, sandbox.BootstrapFinalizer) {
			return r.deleteSandboxBuiltin(ctx, instance)
		}
		if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom {
			return r.deleteCustom(ctx, instance)
		}
		if sandboxRequested(instance, nil) {
			return r.deleteSandboxBuiltin(ctx, instance)
		}
		if err := r.cleanupRegistryCredentials(ctx, instance, true); err != nil {
			return r.registryWaiting(ctx, instance, "RegistryCleanupPending")
		}
		logger.V(1).Info("DeletionTimestamp set, entering delete handler", "alias", instance.Spec.User.Alias)
		return r.handleDelete(ctx, instance)
	}

	// "ui" is the top-level namespace for native agent routes (/ui/<instance>/).
	// It must never remain an externally routed instance name: otherwise a legacy
	// /ui/... VirtualService can overlap native routes and route an authorized
	// request for one instance to the "ui" workload. This is intentionally a
	// breaking change; no migration path exists for the reserved legacy name.
	if instance.Name == "ui" {
		return r.rejectReservedUIInstance(ctx, instance)
	}
	if r.CetiAdmin != nil || instance.Spec.Helm.Maintenance != nil || instance.Status.HelmMaintenance != nil {
		result, handled, err := r.reconcileHelmMaintenance(ctx, instance)
		if handled {
			return result, err
		}
	}
	if r.CetiAdmin != nil {
		_, managed, bindingErr := r.CetiAdmin.BindingForAgent(ctx, instance)
		if managed && (bindingErr != nil || instance.Spec.Suspended) {
			result, stopErr := r.suspendRegistryBuiltin(ctx, instance)
			if bindingErr != nil {
				return result, stderrors.Join(bindingErr, stopErr)
			}
			return result, stopErr
		}
	}

	// A recorded source must be protected before a lifetime-mode lookup can
	// fail into a hold that stops compute and removes its observations.
	if state := instance.Status.PlannedMigration; state != nil && !state.CanStart() {
		_, result, err := r.reconcileMigration(ctx, instance, false)
		return result, err
	}
	reconciledSandbox, err := r.usesReconciledSandbox(ctx, instance)
	if err != nil {
		result, holdErr := r.holdReconciledSandbox(ctx, instance)
		return result, stderrors.Join(err, holdErr)
	}
	commonCustom := reconciledSandbox && instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom
	if reconciledSandbox {
		if handled, result, err := r.suspendUnbootstrappedReconciledSandbox(ctx, instance); handled || err != nil {
			return result, err
		}
	}
	if allowed, result, err := r.reconcileMigration(ctx, instance, reconciledSandbox); !allowed {
		return result, err
	}
	if reconciledSandbox && instance.Spec.Suspended {
		if err := r.stopReconciledSandboxForSuspension(ctx, instance); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
	}

	// Opted-in Agents remain policy-bound; never-governed Agents retain the
	// legacy runtime path while Sandbox execution is being completed. Resolve its
	// immutable owner/Team/policy binding before any Sandbox or workload
	// recovery can make compute available.
	policy, err := r.reconcilePolicy(ctx, instance)
	if policy.dependencyUnavailable {
		if reconciledSandbox && instance.Spec.Suspended && policy.identityPending {
			// Shutdown and revocation were verified above. A new identity is
			// bootstrapped only after resume, not by changing the stopped state.
			instance.Status.Phase = cetirunv1alpha1.PhaseSuspended
			instance.Status.AccessURL = ""
			instance.Status.ObservedGeneration = instance.Generation
			for _, condition := range []string{cetirunv1alpha1.ConditionReady, cetirunv1alpha1.ConditionPodReady,
				cetirunv1alpha1.ConditionSandboxReady, cetirunv1alpha1.ConditionIngressReady} {
				setCondition(&instance.Status.Conditions, condition, metav1.ConditionFalse,
					"Suspended", "Suspension is confirmed; identity preparation waits for resume")
			}
			return ctrl.Result{RequeueAfter: 15 * time.Second}, stderrors.Join(err, r.updateStatus(ctx, instance))
		}
		// A transient Team, policy, or Gateway catalog failure cannot prove a
		// denial. Preserve the key identity and spend record, but block it and
		// stop compute until fresh authorization evidence is available.
		bootstrapErr := r.bootstrapPendingIngressMTLS(ctx, instance, policy.ingressMTLSPending)
		bootstrapErr = stderrors.Join(bootstrapErr, r.bootstrapPendingIdentity(ctx, instance, policy.identityPending))
		result, holdErr := r.holdPolicyDependencyUnavailable(ctx, instance)
		return result, stderrors.Join(err, bootstrapErr, holdErr)
	} else if err != nil {
		// Status persistence is not part of the authorization boundary. If it
		// fails, revoke access and stop compute before returning the joined
		// error; an old workload must never survive because status storage was
		// temporarily unavailable.
		result, holdErr := r.holdPolicyDenied(ctx, instance)
		return result, stderrors.Join(err, holdErr)
	} else if !policy.authorized {
		return r.holdPolicyDenied(ctx, instance)
	} else if policy.reprojectionHold {
		if !reconciledSandbox {
			return r.holdPolicyReprojection(ctx, instance)
		}
		if err := r.holdReconciledPolicyChange(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	if sandboxRequested(instance, nil) && !reconciledSandbox {
		return r.reconcileRecordedNativeSandbox(ctx, instance)
	}
	if reconciledSandbox && r.SandboxNativeCoordination != nil && !instance.Spec.Suspended &&
		!controllerutil.ContainsFinalizer(instance, sandbox.HoldFinalizer) {
		ticket, found, err := r.SandboxNativeCoordination.ReadNative(ctx, instance.UID)
		if err != nil {
			return ctrl.Result{}, err
		}
		// A newer resume intent must not strand or supersede a recorded stop.
		if found && ticket.Reservation.Phase == sandboxcoordination.Stopping {
			return r.holdReconciledSandbox(ctx, instance)
		}
	}
	if reconciledSandbox && r.SandboxNativeCoordination != nil && controllerutil.ContainsFinalizer(instance, sandbox.HoldFinalizer) {
		if err := stderrors.Join(r.stopReconciledAuxiliaries(ctx, instance), r.stopSandboxBuiltin(ctx, instance)); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
	}

	// Inspect Team policy before runtime recovery can retain a Ready state.
	// Lookup errors are handled below so Custom suspension/recovery stays available.
	var team *cetirunv1alpha1.Team
	var teamErr error
	if instance.Spec.Team != "" {
		team = &cetirunv1alpha1.Team{}
		teamErr = r.Get(ctx, types.NamespacedName{Name: instance.Spec.Team}, team)
	}
	if teamErr == nil && sandboxRequested(instance, team) && !reconciledSandbox {
		return r.holdSandbox(ctx, instance)
	}

	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom && instance.Spec.Suspended && !commonCustom {
		return r.suspendCustom(ctx, instance)
	}
	if instance.Spec.Agent.Type != cetirunv1alpha1.AgentTypeCustom && instance.Spec.Suspended && !reconciledSandbox {
		return r.suspendRegistryBuiltin(ctx, instance)
	}
	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom && !commonCustom {
		if err := r.retryCustomCompletion(ctx, instance); err != nil {
			return r.customWaiting(ctx, instance, "CustomCompletionPending", "Waiting to acknowledge the previous apply outcome")
		}
	}
	if err := r.removeRegistryCredentials(ctx, instance); err != nil {
		return r.registryWaiting(ctx, instance, "RegistryCleanupPending")
	}

	if err := validateAgentRuntime(instance.Spec.Agent); err != nil {
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionAgentConfigReady,
			metav1.ConditionFalse, "AgentRuntimeNotImplemented", err.Error())
		return r.failWith(ctx, instance, "AgentRuntimeNotImplemented", err.Error())
	}
	if condition := meta.FindStatusCondition(instance.Status.Conditions, cetirunv1alpha1.ConditionAgentConfigReady); condition != nil &&
		condition.Reason == "AgentRuntimeNotImplemented" {
		meta.RemoveStatusCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionAgentConfigReady)
	}

	// 3. Ensure finalizer.
	if !controllerutil.ContainsFinalizer(instance, cetirunv1alpha1.Finalizer) {
		logger.V(1).Info("Adding finalizer", "alias", instance.Spec.User.Alias)
		before := instance.DeepCopy()
		controllerutil.AddFinalizer(instance, cetirunv1alpha1.Finalizer)
		if err := r.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 3a. Backfill spec.helm.version if empty.
	// This pins the chart version on the CR so that future operator upgrades
	// do not automatically roll out a new chart (avoiding pod restarts).
	// Policy-bound Agents stop at holdSandbox above. Before enabling that path,
	// authorize version/image/port pinning narrowly; admission rejects controller
	// spec writes and must not gain a blanket controller exemption.
	if instance.Spec.Helm.Version == "" {
		before := instance.DeepCopy()
		instance.Spec.Helm.Version = r.ChartVersion
		if err := r.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, fmt.Errorf("backfill helm version: %w", err)
		}
		// A spec patch bumps Generation; re-queue to pick up the new value.
		return ctrl.Result{Requeue: true}, nil
	}

	// 3b. Require the referenced Team before workload preparation.
	if teamErr != nil {
		if errors.IsNotFound(teamErr) {
			return r.failWith(ctx, instance,
				"TeamNotFound",
				fmt.Sprintf("referenced Team %q not found", instance.Spec.Team))
		}
		return ctrl.Result{}, fmt.Errorf("get Team %s: %w", instance.Spec.Team, teamErr)
	}

	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom && !commonCustom {
		return r.reconcileCustom(ctx, instance, team)
	}

	if !r.AIGateway.Enabled {
		if reconciledSandbox {
			return r.holdPolicyDependencyUnavailable(ctx, instance)
		}
		return r.gatewayRuntimeWaiting(ctx, instance, "AIGatewayRequired")
	}

	// Persist the Agent Identity provisioning intent before any workload
	// preparation. This preserves a canonical binding and create checkpoint
	// across generation changes and transient downstream failures.
	if agentIdentityRequested(instance) && (instance.Status.AgentIdentity == nil || instance.Status.AgentIdentity.ObservedGeneration != instance.Generation) {
		status := ensureAgentIdentityStatus(instance)
		status.Phase = cetirunv1alpha1.AgentIdentityPhaseProvisioning
		status.CredentialPolicyRevision = ""
		status.ObservedGeneration = instance.Generation
		status.Reason = ""
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionAgentIdentityReady,
			metav1.ConditionFalse, "Provisioning", "Agent Identity provisioning intent is recorded")
		if err := r.updateStatus(ctx, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("persist Agent Identity provisioning intent: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if instance.Spec.Agent.RegistrySecretRef != nil && instance.Spec.Agent.Image != nil && !instance.Spec.Suspended &&
		(!reconciledSandbox || instance.Spec.Agent.Port != nil || commonCustom) {
		// Owned bootstrap needs the runtime pins defaulted below. Registry sync
		// must not create a namespace that protected bootstrap cannot adopt.
		if reconciledSandbox {
			if err := r.bootstrapReconciledSandbox(ctx, instance); err != nil {
				result, holdErr := r.holdPolicyDependencyUnavailable(ctx, instance)
				return result, stderrors.Join(err, holdErr)
			}
		}
		// An unmatched Registry operation must not prevent repair of its exec gate.
		// Terminal permissions are still installed only during workload preparation.
		if commonCustom {
			if err := r.ensureCommonTerminalExecPolicy(ctx, instance); err != nil {
				result, waitErr := r.registryWaiting(ctx, instance, "RegistryIsolationUnverified")
				return result, stderrors.Join(err, waitErr)
			}
		}
		// Credential rotation must not pull the runtime chart or enter Helm.
		sa := "default"
		if !commonCustom {
			values, err := r.builtinHelmValues(instance, team, AgentIdentityHelmConfig{})
			if err != nil {
				return r.registryWaiting(ctx, instance, "RegistryRuntimeConfigRejected")
			}
			sa, err = registryServiceAccount(values, instanceServiceName(instance.Name))
			if err != nil {
				return r.registryWaiting(ctx, instance, "RegistryIsolationUnverified")
			}
		}
		if _, err := r.syncRegistryCredentials(ctx, instance, sa); err != nil {
			return r.registryWaiting(ctx, instance, err.Error())
		}
	}

	if changed, err := r.upgradeDefaultImage(ctx, instance); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{Requeue: true}, nil
	}

	var prepared *helm.PreparedRelease
	if !commonCustom && (instance.Spec.Agent.Image == nil || instance.Spec.Agent.Port == nil) {
		var err error
		prepared, err = r.prepareRuntimeAgent(ctx, instance, team)
		if err != nil {
			return r.preparationFailed(ctx, instance, err)
		}
		before := instance.DeepCopy()
		if pinAgentDefaults(&instance.Spec.Agent, prepared.Agent) {
			if err := r.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, fmt.Errorf("pin agent defaults: %w", err)
			}
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// 4. If already Ready and nothing has changed, skip the full reconcile.
	// This prevents a status-update loop: LastReconcileTime always differs,
	// which triggers a new watch event, which triggers another reconcile.
	// We read the gateway Secret content hash here so that external Secret
	// modifications (token rotation, tailscale key update) invalidate the
	// skip cache and trigger a full reconcile with Helm upgrade.
	connectionsMCPEnabled, err := r.connectionsMCPEnabledFor(instance)
	if err != nil {
		return r.rejectAgentConfig(ctx, instance, err)
	}
	// A suspended workload renders zero replicas and does not consume MCP trust.
	// Keep CA reads and distribution out of its stop path; resume verifies trust
	// again before Helm can start any workload.
	connectionsMCPTrustRequired := connectionsMCPEnabled && !instance.Spec.Suspended
	connectionsCAHash := ""
	if connectionsMCPTrustRequired {
		connectionsCAHash, err = r.connectionsMCPCAHash(ctx)
		if err != nil {
			return r.failWith(ctx, instance, "ProvisioningFailed", "Connections MCP CA trust is unavailable")
		}
	}
	gwSecretHash := ""
	gatewaySecretResourceVersion := ""
	if !reconciledSandbox && (instance.Status.Phase == cetirunv1alpha1.PhaseReady || instance.Status.Phase == cetirunv1alpha1.PhaseSuspended) {
		brokerRBACErr := r.reconcileConnectionsMCPBrokerRBAC(ctx, instance, !sandboxRequested(instance, team) || r.nativeConnectionsRequested(instance))
		namespaceMissing := stderrors.Is(brokerRBACErr, errConnectionsMCPBrokerNamespaceMissing)
		if brokerRBACErr != nil && !namespaceMissing {
			logger.Error(brokerRBACErr, "Broker storage access setup failed", "instance", instance.Name)
			r.EventRecorder.Eventf(instance, corev1.EventTypeWarning, "BrokerStorageAccessFailed", "%v", brokerRBACErr)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		if connectionsMCPTrustRequired && !namespaceMissing {
			if err := r.ensureConnectionsMCPCA(ctx, instance); err != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}
		gwSecret := &corev1.Secret{}
		gwSecretKey := types.NamespacedName{
			Namespace: instanceNamespace(instance.Name),
			Name:      instance.Spec.GatewayTokenSecretRef,
		}
		if err := r.Get(ctx, gwSecretKey, gwSecret); err != nil {
			if !errors.IsNotFound(err) {
				// Transient or RBAC error — requeue instead of silently falling back
				// to an empty hash which would spuriously invalidate the skip cache.
				logger.Error(err, "failed to read gateway Secret for skip-cache hash", "alias", instance.Spec.User.Alias)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			// NotFound: Secret doesn't exist yet, leave gwSecretHash empty.
		} else {
			// Do this before evaluating the skip-cache so existing instances created
			// before CETIRUN_AUTH_KEY was introduced are upgraded even when their
			// previously observed Secret hash still matches.
			if err := r.ensureCetiRunAuthKey(ctx, logger, gwSecret); err != nil {
				logger.Error(err, "failed to backfill CetiRun auth key before skip-cache", "alias", instance.Spec.User.Alias)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			gwSecretHash = secretContentHash(gwSecret.Data) + connectionsCAHash
		}

		identityVerificationDue := agentIdentityVerificationDue(instance, time.Now())
		aiGatewayDue := r.aiGatewayReconcileDue(instance, time.Now())
		// An Agent whose chart could not be downloaded takes the full path,
		// which retries the download, until a download succeeds.
		// A missing namespace also requires the full path, even if the cached
		// hash matches. Establish Broker RBAC after ensureNamespace recreates it.
		if !namespaceMissing && instance.Status.ObservedGeneration == instance.Generation &&
			!identityVerificationDue && !aiGatewayDue && !chartUnavailable(instance) &&
			instance.Status.ObservedOperatorHash == r.configHash(instance, team, gwSecretHash, teamRuntimeInputFingerprint(team)) {
			// Lightweight: ensure VirtualService is up to date (may change after operator upgrade).
			// Update ConditionIngressReady so CR status reflects the actual VS state.
			// When suspended, ensure VirtualService is absent (retries deletion if
			// a previous reconcile partially failed). When not suspended, ensure
			// VirtualService is up to date (may change after operator upgrade).
			bypassSkipCache := false
			statusChanged := false
			if instance.Status.Phase == cetirunv1alpha1.PhaseSuspended {
				vsName := virtualServiceName(instance.Name)
				nsCheck := instanceNamespace(instance.Name)
				vs := &istiov1beta1.VirtualService{}
				vsKey := types.NamespacedName{Name: vsName, Namespace: nsCheck}
				if err := r.Get(ctx, vsKey, vs); err == nil {
					// VS still exists — bypass skip-cache so the full suspend path re-runs.
					logger.Info("VirtualService still exists for suspended instance, forcing full reconcile", "alias", instance.Spec.User.Alias, "vs", vsName)
					bypassSkipCache = true
				} else if !errors.IsNotFound(err) {
					logger.Error(err, "failed to check VirtualService for suspended instance", "alias", instance.Spec.User.Alias)
				}
			} else {
				prevIngressReady := isConditionTrue(instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady)
				if _, err := r.ensureVirtualService(ctx, instance); err != nil {
					logger.Error(err, "VirtualService update failed (lightweight reconcile)", "alias", instance.Spec.User.Alias)
					setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady,
						metav1.ConditionFalse, "VirtualServiceFailed", err.Error())
					statusChanged = statusChanged || prevIngressReady
				} else {
					canonicalAccessURL := accessURLPathBased(instance.Name, r.DomainSuffix)
					if instance.Status.AccessURL != canonicalAccessURL {
						instance.Status.AccessURL = canonicalAccessURL
						statusChanged = true
					}
					setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady,
						metav1.ConditionTrue, "VirtualServiceCreated", "Istio ingress configured")
					statusChanged = statusChanged || !prevIngressReady
				}
			}
			if !bypassSkipCache {
				if statusChanged {
					if err := r.updateStatus(ctx, instance); err != nil {
						return ctrl.Result{}, fmt.Errorf("update steady-state status: %w", err)
					}
				}
				logger.V(1).Info("Instance already at steady state with no changes, skipping reconcile", "alias", instance.Spec.User.Alias, "phase", instance.Status.Phase)
				return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
			}
		}
	}

	if prepared == nil && !commonCustom {
		var err error
		prepared, err = r.prepareRuntimeAgent(ctx, instance, team)
		if err != nil {
			return r.preparationFailed(ctx, instance, err)
		}
	}

	// 5. Start provisioning. An Agent without a HelmDeployed condition is
	// being created: its chart was never applied. Record the creation once,
	// together with a HelmDeployed condition that stays until the chart is
	// installed, so that a retry after a later failure does not report it
	// again. This includes an Agent that failed before its first
	// provisioning pass, for example while its chart could not be prepared.
	// A deployed Agent can return to Pending, for example while it waits for
	// registry credentials; that is not a creation.
	creating := meta.FindStatusCondition(instance.Status.Conditions, cetirunv1alpha1.ConditionHelmDeployed) == nil
	if creating {
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionHelmDeployed,
			metav1.ConditionFalse, "ChartNotInstalled", "Helm chart not installed yet")
	}
	if instance.Status.Phase == "" || instance.Status.Phase == cetirunv1alpha1.PhasePending {
		logger.V(1).Info("Setting phase to Provisioning", "alias", instance.Spec.User.Alias, "currentPhase", instance.Status.Phase)
		if reconciledSandbox && !creating {
			// Bootstrap may still fail before it creates any lifetime evidence.
			// Keep Provisioning in memory until a later write can make progress;
			// persisting it here otherwise creates a Pending -> Provisioning ->
			// Pending loop through an unchanged dependency hold.
			instance.Status.Phase = cetirunv1alpha1.PhaseProvisioning
		} else {
			if err := r.setPhase(ctx, instance, cetirunv1alpha1.PhaseProvisioning); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else if creating {
		if err := r.updateStatus(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	if creating {
		r.EventRecorder.Eventf(instance, corev1.EventTypeNormal, "InstanceCreating",
			"Creating agent for user %s", instance.Spec.User.Alias)
	}

	// 5a. Transition Ready/Suspended → Provisioning when a full reconcile is needed.
	// If we reach this point while Ready or Suspended, the skip-cache (step 4)
	// determined that generation or operator config has changed, so an upgrade
	// will happen. We only set the phase in-memory here; it will be persisted
	// by the next updateStatus call (after Helm install or on error), avoiding
	// an extra status write that would trigger a redundant reconcile. Sandbox
	// source/drift checks also run at steady state, so wait for an actual apply.
	if !reconciledSandbox && (instance.Status.Phase == cetirunv1alpha1.PhaseReady || instance.Status.Phase == cetirunv1alpha1.PhaseSuspended) {
		instance.Status.Phase = cetirunv1alpha1.PhaseProvisioning
	}

	alias := instance.Spec.User.Alias
	ns := instanceNamespace(instance.Name)
	releaseName := helmReleaseName(instance.Name)

	logger.Info("Reconciling Agent", "alias", alias, "generation", instance.Generation)

	// 6. Ensure namespace.
	// Keep completed evidence stable while rechecking prerequisites. Invalidate
	// it only on an early exit, including errors that do not persist status.
	// Successful workload-only waits must not publish False then True: each
	// status change enqueues another reconcile independently of RequeueAfter.
	prerequisitesComplete := false
	defer func() {
		if prerequisitesComplete || meta.IsStatusConditionFalse(instance.Status.Conditions, builtInPrerequisitesReady) {
			return
		}
		setCondition(&instance.Status.Conditions, builtInPrerequisitesReady,
			metav1.ConditionFalse, "PrerequisitesPending", "Agent prerequisites did not complete")
		reconcileErr = stderrors.Join(reconcileErr, r.updateStatus(ctx, instance))
	}()
	logger.V(1).Info("Ensuring namespace", "namespace", ns)
	var namespaceErr error
	if reconciledSandbox {
		namespaceErr = r.bootstrapReconciledSandbox(ctx, instance)
	} else {
		namespaceErr = r.ensureNamespace(ctx, ns, alias, instance)
	}
	if namespaceErr != nil {
		if reconciledSandbox {
			result, holdErr := r.holdPolicyDependencyUnavailable(ctx, instance)
			return result, stderrors.Join(namespaceErr, holdErr)
		}
		return r.failWith(ctx, instance, "ProvisioningFailed", fmt.Sprintf("ensure namespace: %v", namespaceErr))
	}
	if commonCustom {
		if err := r.ensureCommonCustomRuntimeAuthentication(ctx, instance, logger); err != nil {
			return r.failWith(ctx, instance, "ProvisioningFailed", fmt.Sprintf("ensure Custom runtime authentication: %v", err))
		}
	}

	// 6a. Ensure namespaced Role + RoleBinding so the operator's access to Secrets,
	// ConfigMaps, Services, StatefulSets, PVCs, and ServiceAccounts is scoped to
	// this namespace only, rather than cluster-wide.
	logger.V(1).Info("Ensuring namespaced RBAC", "namespace", ns)
	if err := r.ensureNamespacedRBAC(ctx, ns); err != nil {
		return r.failWith(ctx, instance, "ProvisioningFailed", fmt.Sprintf("ensure namespaced RBAC: %v", err))
	}
	if connectionsMCPTrustRequired {
		if err := r.ensureConnectionsMCPCA(ctx, instance); err != nil {
			return r.failWith(ctx, instance, "ProvisioningFailed", "Connections MCP CA trust distribution failed")
		}
	}

	if err := r.reconcileConnectionsMCPBrokerRBAC(ctx, instance, !sandboxRequested(instance, team) || r.nativeConnectionsRequested(instance)); err != nil {
		return r.failWith(ctx, instance, "BrokerStorageAccessFailed", fmt.Sprintf("Broker storage access setup failed: %v", err))
	}
	if !commonCustom {
		// 7. Ensure gateway token secret.
		logger.V(1).Info("Ensuring gateway token secret", "alias", alias)
		if err := r.ensureGatewayToken(ctx, instance); err != nil {
			logger.Error(err, "failed to ensure gateway token", "alias", alias)
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
				metav1.ConditionFalse, "GatewayTokenError", err.Error())
			_ = r.updateStatus(ctx, instance)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// 8. Ensure Tailscale auth key is in the gateway token Secret (if configured).
		// The operator uses secrets.existingSecret, so the chart's Secret template is skipped.
		// We must patch the auth key directly into the gateway token Secret for the StatefulSet
		// to find TAILSCALE_AUTH_KEY via its secretKeyRef.
		if err := r.ensureTailscaleAuthKey(ctx, instance); err != nil {
			logger.Error(err, "failed to ensure Tailscale auth key", "alias", alias)
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionTailscaleReady,
				metav1.ConditionFalse, "TailscaleAuthKeyFailed", err.Error())
			_ = r.updateStatus(ctx, instance)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if instance.Spec.Tailscale != nil {
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionTailscaleReady,
				metav1.ConditionTrue, "TailscaleAuthKeyReady", "Tailscale auth key written to gateway Secret")
		} else {
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionTailscaleReady,
				metav1.ConditionFalse, "TailscaleDisabled", "Tailscale is not configured")
		}
	}

	// Reconcile the per-instance Agent Identity before rendering the workload.
	// The Broker reads only the canonical binding persisted in CR status.
	var agentIdentityRequeueAfter time.Duration
	if instance.Spec.AgentIdentity != nil || instance.Status.AgentIdentity != nil {
		logger.Info("Reconciling Agent Identity", "instance", instance.Name)
		if err := r.reconcileAgentIdentity(ctx, instance); err != nil {
			logger.Error(err, "Agent Identity reconciliation failed", "instance", instance.Name)
			reason := "ReconcileFailed"
			if instance.Status.AgentIdentity != nil && instance.Status.AgentIdentity.Reason != "" {
				reason = instance.Status.AgentIdentity.Reason
			}
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionAgentIdentityReady,
				metav1.ConditionFalse, reason, "Agent Identity is not ready")
			_ = r.updateStatus(ctx, instance)
			if !instance.Spec.Suspended {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			// Suspension is a safety boundary and must not depend on directory
			// availability. Continue to scale the workload to zero and remove
			// ingress, while retaining the identity error for a separate retry.
			agentIdentityRequeueAfter = 30 * time.Second
		}
		if agentIdentityRequested(instance) && instance.Status.AgentIdentity != nil && instance.Status.AgentIdentity.Phase == cetirunv1alpha1.AgentIdentityPhaseReady {
			now := metav1.Now()
			instance.Status.AgentIdentity.LastVerifiedTime = &now
		}
		if instance.Status.Policy != nil {
			// Broker readback uses the persisted canonical identity, never the
			// in-memory result of a directory request.
			if err := r.updateStatus(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Reconcile the Agent's AI Gateway virtual key before rendering the
	// workload: an Agent routed through the Gateway needs its key at startup.
	var aiGatewayRequeueAfter time.Duration
	chartSupported := commonCustom || aiGatewayChartSupports(prepared, instance.Spec.Agent.Type)
	if !r.AIGateway.Enabled || !chartSupported {
		if reconciledSandbox {
			return r.holdPolicyDependencyUnavailable(ctx, instance)
		}
		return r.gatewayRuntimeWaiting(ctx, instance, "AIGatewayRequired")
	}
	if err := r.reconcileAIGateway(ctx, instance, chartSupported); err != nil {
		logger.Error(err, "AI Gateway virtual key reconciliation failed", "instance", instance.Name)
		_ = r.updateStatus(ctx, instance)
		if !r.aiGatewayRouted(instance) && !instance.Spec.Suspended {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Keep an already routed workload through a transient management outage.
		aiGatewayRequeueAfter = 30 * time.Second
	} else if copilot := instance.Status.AIGateway.Copilot; copilot != nil &&
		(copilot.Phase == cetirunv1alpha1.AIGatewayCopilotPhaseDegraded || copilot.Phase == cetirunv1alpha1.AIGatewayCopilotPhaseStarting) {
		aiGatewayRequeueAfter = 15 * time.Second
	}
	if reconciledSandbox {
		if err := r.updateStatus(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The initial projection must be allowed to execute, but it is not a
	// workload authorization. Re-evaluate after the Gateway projection has had
	// a chance to update its evidence and stop here until every current
	// projection is verified. Any workload that was already exposed is fenced
	// by the early reprojection hold above.
	policy, err = r.reconcilePolicy(ctx, instance)
	if policy.dependencyUnavailable {
		result, holdErr := r.holdPolicyDependencyUnavailable(ctx, instance)
		return result, stderrors.Join(err, holdErr)
	} else if err != nil {
		result, holdErr := r.holdPolicyDenied(ctx, instance)
		return result, stderrors.Join(err, holdErr)
	}
	if !policy.authorized {
		return r.holdPolicyDenied(ctx, instance)
	}
	if !policy.projectionsVerified && !reconciledSandbox {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// 9. Install / upgrade Helm chart (skip if already deployed at this generation).
	// We check HelmDeployed condition + generation + operator hash rather than Phase,
	// because Phase may be Provisioning while waiting for the StatefulSet pod to become
	// ready (step 11). In that case Helm has already succeeded and we must not re-run it.
	// Re-read the gateway Secret hash now that ensureGatewayToken / ensureTailscaleAuthKey
	// have guaranteed the Secret exists and may have modified its content.
	{
		gwSecret := &corev1.Secret{}
		gwSecretKey := types.NamespacedName{Namespace: ns, Name: sandboxRuntimeSecretName(instance)}
		if err := r.Get(ctx, gwSecretKey, gwSecret); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("re-read gateway Secret for operator hash: %w", err)
			}
		} else {
			gwSecretHash = secretContentHash(gwSecret.Data) + connectionsCAHash
			gatewaySecretResourceVersion = gwSecret.ResourceVersion
		}
	}
	alreadyDeployed := isConditionTrue(instance.Status.Conditions, cetirunv1alpha1.ConditionHelmDeployed) &&
		instance.Status.ObservedGeneration == instance.Generation &&
		instance.Status.ObservedOperatorHash == r.configHash(instance, team, gwSecretHash, teamRuntimeInputFingerprint(team))
	var helmValues map[string]any
	if commonCustom {
		if !sandboxGatewayProjectionReadyForLifecycle(instance) {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		prepared, err = r.prepareReconciledSandbox(ctx, instance, prepared, nil)
		if err != nil {
			if stderrors.Is(err, errSkillsClaimPending) {
				// The Skill Marketplace has not created the skills claim yet.
				// Nothing is installed, held or stopped while waiting for it.
				return r.waitForSkillsClaim(ctx, instance, err)
			}
			if stderrors.Is(err, helm.ErrCustomSkillMarketplaceUnsupported) {
				// An earlier Agent chart cannot give the Agent its Team's
				// skills. Refuse the configuration with this reason, as for a
				// built-in Agent, instead of a hold that blocks its key. The
				// refusal itself changes no workload.
				return r.rejectAgentConfig(ctx, instance, err)
			}
			return r.reconciledSandboxFailure(ctx, instance, err)
		}
		endSkillsClaimWait(instance)
		helmValues = prepared.Values
	} else {
		// Rebuild from current bindings without reloading the versioned chart.
		helmValues, err = r.builtinHelmValues(instance, team, AgentIdentityHelmConfig{
			Enabled:                    r.AgentIdentityEnabled,
			BrokerURL:                  r.AgentIdentityBrokerURL,
			ConnectionsMCPURL:          r.ConnectionsMCPURL,
			ConnectionsMCPAudience:     r.ConnectionsMCPAudience,
			ConnectionsMCPCASecretName: r.ConnectionsMCPCASecretName,
			ConnectionsMCPRuntimeTypes: r.ConnectionsMCPRuntimeTypes,
		})
		if err != nil {
			return r.rejectAgentConfig(ctx, instance, err)
		}
		if err := r.applyAIGatewayValues(helmValues, instance); err != nil {
			return r.rejectAgentConfig(ctx, instance, err)
		}
		helmValues["memoryOptions"] = r.memoryOptions()
		if err := r.applyCetiAdminValues(ctx, instance, helmValues); err != nil {
			return r.rejectAgentConfig(ctx, instance, err)
		}
		r.applyCopilotChartValues(helmValues, instance, prepared)
		r.applyAIGatewayVisionModels(helmValues, instance, prepared)
		{
			secret := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: instance.Spec.GatewayTokenSecretRef}, secret); err != nil {
				return ctrl.Result{}, fmt.Errorf("read native integration bindings: %w", err)
			}
			helmValues["agentRequest"].(map[string]any)["tailscaleEnabled"] = len(secret.Data["TAILSCALE_AUTH_KEY"]) > 0
		}
		if gatewaySecretResourceVersion != "" {
			helmValues["podAnnotations"] = deepMerge(helmValues["podAnnotations"], map[string]interface{}{
				gatewaySecretResourceVersionAnnotation: gatewaySecretResourceVersion,
			})
		}
		if connectionsCAHash != "" && reconciledSandbox {
			helmValues["podAnnotations"] = deepMerge(helmValues["podAnnotations"], map[string]interface{}{"cetirun.microsoft.com/connections-ca-hash": connectionsCAHash})
		}
		if reconciledSandbox {
			prepared, err = r.prepareReconciledSandbox(ctx, instance, prepared, helmValues)
		} else {
			prepared, err = r.HelmClient.Refresh(prepared, helmValues)
		}
		if err != nil {
			if reconciledSandbox && (stderrors.Is(err, errMigrationIntentChanged) || stderrors.Is(err, errSandboxIntentChanged)) {
				return r.reconciledSandboxFailure(ctx, instance, err)
			}
			if reconciledSandbox && stderrors.Is(err, errSandboxIngressMTLS) {
				result, holdErr := r.holdReconciledSandbox(ctx, instance)
				return result, stderrors.Join(err, holdErr)
			}
			if reconciledSandbox && stderrors.Is(err, errSandboxSourceChanged) {
				return r.reconciledSandboxFailure(ctx, instance, err)
			}
			if reconciledSandbox && stderrors.Is(err, errSkillsClaimPending) {
				// The Skill Marketplace has not created the skills claim yet.
				// Nothing is installed, held or stopped while waiting for it.
				return r.waitForSkillsClaim(ctx, instance, err)
			}
			return r.rejectAgentConfig(ctx, instance, err)
		}
		if reconciledSandbox {
			endSkillsClaimWait(instance)
		}
	}
	if instance.Spec.Suspended {
		if err := validateSuspendedHome(prepared.Values); err != nil {
			return r.rejectAgentConfig(ctx, instance, err)
		}
	}
	if sandboxRequested(instance, team) && !prepared.SupportsValuePath(aiGatewayChartValue, aiGatewayChartModelsValue) {
		policyErr := fmt.Errorf("Agent chart %q does not support mandatory AI Gateway routing", instance.Spec.Helm.Version)
		if err := r.recordPolicy(ctx, instance, nil, policyErr); err != nil {
			return ctrl.Result{}, err
		}
		return r.holdPolicyDenied(ctx, instance)
	}
	setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionAgentConfigReady,
		metav1.ConditionTrue, "AgentConfigResolved", "Agent Harness configuration resolved")
	helmValues = prepared.Values
	if connectionsCAHash != "" && !reconciledSandbox {
		helmValues["podAnnotations"] = deepMerge(helmValues["podAnnotations"], map[string]interface{}{"cetirun.microsoft.com/connections-ca-hash": connectionsCAHash})
	}
	if reconciledSandbox {
		alreadyDeployed = isConditionTrue(instance.Status.Conditions, cetirunv1alpha1.ConditionHelmDeployed) &&
			instance.Status.ObservedGeneration == instance.Generation &&
			instance.Status.ObservedOperatorHash == r.configHash(instance, team, gwSecretHash, teamRuntimeInputFingerprint(team))+":"+prepared.WorkloadHash
		if alreadyDeployed {
			alreadyDeployed, err = r.reconciledSandboxApplied(ctx, instance, prepared)
			if err != nil {
				return r.reconciledSandboxFailure(ctx, instance, err)
			}
		}
		held := !instance.Spec.Suspended && controllerutil.ContainsFinalizer(instance, sandbox.HoldFinalizer)
		if err := r.releaseReconciledHold(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		if held {
			return ctrl.Result{Requeue: true}, nil
		}
	}
	if !alreadyDeployed && instance.Spec.Agent.RegistrySecretRef != nil {
		sa, err := registryServiceAccount(helmValues, instanceServiceName(instance.Name))
		if err != nil {
			return r.registryWaiting(ctx, instance, "RegistryIsolationUnverified")
		}
		if err := registryauth.CheckIsolation(ctx, r.registryClient(), r.registryNamespace(), ns, sa, instance.Spec.Agent.RegistrySecretRef.Name); err != nil {
			return r.registryWaiting(ctx, instance, "RegistryIsolationUnverified")
		}
	}
	if !alreadyDeployed {
		if err := r.checkRegistryIntent(ctx, instance); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		if current, err := r.agentIsCurrent(ctx, instance); err != nil {
			return ctrl.Result{}, err
		} else if !current {
			logger.Info("Agent changed since it was read; not installing from the older copy", "release", releaseName)
			// Nothing is recorded from this copy either: its status patch
			// would be refused, and the next pass reads the latest Agent.
			prerequisitesComplete = true
			return ctrl.Result{Requeue: true}, nil
		}
		if instance.Spec.RestartRequestID != "" && !instance.Spec.Suspended {
			if err := r.checkAgentGeneration(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
		}
		if reconciledSandbox {
			instance.Status.Phase = cetirunv1alpha1.PhaseProvisioning
		}
		logger.Info("Installing/upgrading Helm chart", "release", releaseName, "namespace", ns)
		if err := r.HelmClient.InstallOrUpgrade(ns, releaseName, prepared); err != nil {
			logger.Error(err, "Helm install/upgrade failed", "release", releaseName)
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionHelmDeployed,
				metav1.ConditionFalse, "HelmInstallFailed", err.Error())
			_ = r.updateStatus(ctx, instance)
			if reconciledSandbox {
				expected, expectationErr := prepared.ReconcileExpectation()
				if expectationErr != nil || slices.ContainsFunc(expected.Controls, sandboxworkload.KubernetesBindingResource) {
					result, holdErr := r.holdReconciledSandbox(ctx, instance)
					return result, stderrors.Join(err, expectationErr, holdErr)
				}
			}
			return ctrl.Result{RequeueAfter: 60 * time.Second}, err
		}
		logger.Info("Helm chart installed/upgraded successfully", "release", releaseName)
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionHelmDeployed,
			metav1.ConditionTrue, "ChartInstalled", "Helm chart successfully installed")
		// Record generation and operator hash immediately so that subsequent reconciles
		// (e.g. while polling for pod readiness) can skip the Helm install.
		instance.Status.ObservedGeneration = instance.Generation
		instance.Status.ObservedOperatorHash = r.configHash(instance, team, gwSecretHash, teamRuntimeInputFingerprint(team))
		if reconciledSandbox {
			instance.Status.ObservedOperatorHash += ":" + prepared.WorkloadHash
		}
		r.EventRecorder.Eventf(instance, corev1.EventTypeNormal, "HelmInstalled",
			"Helm chart openclaw installed in namespace %s", ns)
	} else {
		logger.V(1).Info("Skipping Helm install, already deployed at current generation", "release", releaseName)
	}
	if reconciledSandbox {
		if err := r.confirmReconciledKubernetesGrants(ctx, instance, prepared); err != nil {
			result, holdErr := r.holdReconciledSandbox(ctx, instance)
			return result, stderrors.Join(err, holdErr)
		}
	}
	if registryManaged(instance) {
		if err := r.cleanupRegistryCredentials(ctx, instance, false); err != nil {
			return r.registryWaiting(ctx, instance, "RegistryCleanupPending")
		}
	}
	if err := r.recoverRegistryPull(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	// Preserve the heartbeat between periodic passes; its own status event must
	// not schedule an endless series of otherwise unchanged Sandbox reconciles.
	reconciledAt := metav1.Now()
	if reconciledSandbox && alreadyDeployed && startPhase == instance.Status.Phase &&
		(startPhase == cetirunv1alpha1.PhaseReady || startPhase == cetirunv1alpha1.PhaseSuspended) &&
		instance.Status.LastReconcileTime != nil {
		elapsed := reconciledAt.Sub(instance.Status.LastReconcileTime.Time)
		if elapsed >= 0 && elapsed < 10*time.Minute {
			reconciledAt = *instance.Status.LastReconcileTime
		}
	}

	// 10. Handle suspended instances.
	// When suspended, delete the VirtualService (remove ingress routing) and
	// skip StatefulSet readiness checks — the pod was scaled to zero by the
	// Helm upgrade above (replicaCount=0). PVCs are preserved by the
	// StatefulSet's whenScaled: Retain retention policy.
	if instance.Spec.Suspended {
		logger.Info("Instance is suspended, removing ingress and marking Suspended", "alias", alias)
		if reconciledSandbox {
			if err := r.stopNativeSandboxReservation(ctx, instance); err != nil {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
		// Delete VirtualService so users see connection refused instead of 503.
		vsName := virtualServiceName(instance.Name)
		vs := &istiov1beta1.VirtualService{}
		vsKey := types.NamespacedName{Name: vsName, Namespace: ns}
		var vsDeleteErr error
		if err := r.Get(ctx, vsKey, vs); err != nil {
			if !errors.IsNotFound(err) {
				vsDeleteErr = err
				logger.Error(err, "failed to get VirtualService for suspend deletion", "name", vsName)
			}
		} else {
			if err := r.Delete(ctx, vs); err != nil && !errors.IsNotFound(err) {
				vsDeleteErr = err
				logger.Error(err, "failed to delete VirtualService on suspend", "name", vsName)
			} else {
				logger.Info("VirtualService deleted (instance suspended)", "name", vsName)
			}
		}
		if vsDeleteErr != nil {
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady,
				metav1.ConditionFalse, "SuspendIngressRemovalFailed",
				fmt.Sprintf("Failed to remove VirtualService %q: %v", vsName, vsDeleteErr))
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
				metav1.ConditionFalse, "SuspendIngressRemovalFailed",
				"Instance suspend waiting for ingress removal to succeed")
			now := metav1.Now()
			instance.Status.Namespace = ns
			instance.Status.HelmReleaseName = releaseName
			instance.Status.LastReconcileTime = &now
			if err := r.updateStatus(ctx, instance); err != nil {
				logger.Error(err, "Failed to update status after VirtualService suspend deletion failure", "alias", alias)
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, vsDeleteErr
		}
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady,
			metav1.ConditionFalse, "Suspended", "Ingress removed — instance is suspended")
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPodReady,
			metav1.ConditionFalse, "Suspended", "Pod scaled to zero — instance is suspended")
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
			metav1.ConditionFalse, "InstanceSuspended", "Instance is suspended")
		instance.Status.Phase = cetirunv1alpha1.PhaseSuspended
		instance.Status.Namespace = ns
		instance.Status.HelmReleaseName = releaseName
		instance.Status.LastReconcileTime = &reconciledAt
		if err := r.updateStatus(ctx, instance); err != nil {
			logger.Error(err, "Failed to update status to Suspended", "alias", alias)
			return ctrl.Result{}, err
		}
		if startPhase != cetirunv1alpha1.PhaseSuspended {
			r.EventRecorder.Event(instance, corev1.EventTypeNormal, "InstanceSuspended", "Instance suspended — scaled to zero and ingress removed")
		}
		requeueAfter := 10 * time.Minute
		if agentIdentityRequeueAfter > 0 && agentIdentityRequeueAfter < requeueAfter {
			requeueAfter = agentIdentityRequeueAfter
		}
		if aiGatewayRequeueAfter > 0 && aiGatewayRequeueAfter < requeueAfter {
			requeueAfter = aiGatewayRequeueAfter
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if reconciledSandbox {
		workloadReady, workloadResult, err := r.observeWorkload(ctx, instance)
		if !workloadReady {
			setWorkloadReadiness(instance, false)
			return workloadResult, stderrors.Join(err, r.updateStatus(ctx, instance))
		}
		if err := r.observeReconciledSandbox(ctx, instance, prepared); err != nil {
			return r.reconciledSandboxFailure(ctx, instance, err)
		}
	}

	if commonCustom {
		if err := r.configureReconciledCustomRuntime(ctx, instance, prepared); err != nil {
			logger.Error(err, "Common Custom access configuration failed", "alias", alias)
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
				metav1.ConditionFalse, "CustomAccessUnavailable", err.Error())
			instance.Status.Phase = cetirunv1alpha1.PhaseProvisioning
			return ctrl.Result{RequeueAfter: 15 * time.Second}, stderrors.Join(err, r.updateStatus(ctx, instance))
		}
	} else {
		// 10a. Ensure VirtualService (non-suspended path).
		// TODO: VirtualService failure should block Ready (return error, set phase=Failed).
		// Currently best-effort because Istio is not installed on all test clusters.
		// When Istio is guaranteed to be present, change this block to:
		//   if err != nil { return r.failWith(ctx, instance, "VirtualServiceFailed", err.Error()) }
		logger.V(1).Info("Ensuring VirtualService", "alias", alias)
		if result, err := r.ensureVirtualService(ctx, instance); err != nil {
			logger.Error(err, "VirtualService creation/update failed (best-effort)", "alias", alias)
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady,
				metav1.ConditionFalse, "VirtualServiceFailed", err.Error())
			if reconciledSandbox {
				instance.Status.AccessURL = ""
				instance.Status.Phase = cetirunv1alpha1.PhaseProvisioning
				setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
					metav1.ConditionFalse, "VirtualServiceFailed", "Current Sandbox ingress is not established")
				return ctrl.Result{RequeueAfter: 15 * time.Second}, stderrors.Join(err, r.updateStatus(ctx, instance))
			}
		} else {
			logger.V(1).Info("VirtualService ensured", "alias", alias, "result", result)
			ingress := meta.FindStatusCondition(instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady)
			if !reconciledSandbox || ingress == nil || ingress.Reason != "IngressDisabled" {
				instance.Status.AccessURL = accessURLPathBased(instance.Name, r.DomainSuffix)
				setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady,
					metav1.ConditionTrue, "VirtualServiceCreated", "Istio ingress configured")
			}
			if result != controllerutil.OperationResultNone {
				r.EventRecorder.Eventf(instance, corev1.EventTypeNormal, "IngressConfigured",
					"Istio VirtualService %s, instance accessible at %s", result, accessURLPathBased(instance.Name, r.DomainSuffix))
			}
		}

	}
	// 11. Verify StatefulSet pod readiness before marking Ready.
	prerequisitesComplete = true
	setCondition(&instance.Status.Conditions, builtInPrerequisitesReady,
		metav1.ConditionTrue, "PrerequisitesResolved", "Blocking Agent prerequisites reconciled")
	workloadReady, workloadResult, err := r.observeWorkload(ctx, instance)
	setWorkloadReadiness(instance, workloadReady)
	if !workloadReady {
		_ = r.updateStatus(ctx, instance)
		return workloadResult, err
	}

	// 12. Persist Ready after prerequisites and workload observation.
	instance.Status.Namespace = ns
	instance.Status.HelmReleaseName = releaseName
	instance.Status.LastReconcileTime = &reconciledAt
	if err := r.updateStatus(ctx, instance); err != nil {
		logger.Error(err, "Failed to update status to Ready", "alias", alias)
		return ctrl.Result{}, err
	}

	if startPhase != cetirunv1alpha1.PhaseReady {
		r.EventRecorder.Event(instance, corev1.EventTypeNormal, "InstanceReady", "Agent fully operational")
	}
	logger.Info("Reconciliation complete", "alias", alias, "accessURL", accessURLPathBased(instance.Name, r.DomainSuffix))
	requeueAfter := 10 * time.Minute
	if controls, ok := prepared.Values["sandboxWorkload"].(map[string]any); ok && controls["ingressMTLS"] == true {
		requeueAfter = 5 * time.Second
	}
	if aiGatewayRequeueAfter > 0 && aiGatewayRequeueAfter < requeueAfter {
		requeueAfter = aiGatewayRequeueAfter
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// handleDelete performs cleanup and removes the finalizer.
func (r *AgentReconciler) handleDelete(ctx context.Context, instance *cetirunv1alpha1.Agent) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	alias := instance.Spec.User.Alias
	reconciledSandbox, err := r.recordedReconciledSandbox(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(instance, cetirunv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}

	logger.Info("Deleting Agent", "alias", alias)
	r.EventRecorder.Eventf(instance, corev1.EventTypeNormal, "InstanceDeleting",
		"Starting cleanup for instance %s", alias)

	if err := r.setPhase(ctx, instance, cetirunv1alpha1.PhaseDeleting); err != nil {
		logger.Error(err, "failed to set phase Deleting")
	}

	ns := instanceNamespace(instance.Name)
	releaseName := helmReleaseName(instance.Name)
	if reconciledSandbox {
		if err := stderrors.Join(r.revokeSandboxPreparationGrants(ctx, instance), r.deleteAIGatewayKey(ctx, instance),
			r.stopSandboxBuiltin(ctx, instance), r.stopReconciledAuxiliaries(ctx, instance)); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Agent Identity cleanup is blocking. Removing the finalizer before the
	// authorization record and directory principal are gone would orphan them.
	if err := r.ensureConnectionsMCPBrokerCleanupRBAC(ctx, instance); err != nil {
		logger.Error(err, "Broker cleanup access setup failed; retaining finalizer", "instance", instance.Name)
		r.EventRecorder.Eventf(instance, corev1.EventTypeWarning, "BrokerStorageAccessFailed", "%v", err)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if agentIdentityRequested(instance) || instance.Status.AgentIdentity != nil {
		if err := r.deleteAgentIdentity(ctx, instance); err != nil {
			logger.Error(err, "Agent Identity cleanup failed; retaining finalizer", "instance", instance.Name)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	} else if r.ConnectionsMCPBrokerNamespace != "" {
		// Clean remaining bindings even if Agent Identity was previously disabled.
		if err := r.deleteAgentAuthorization(ctx, string(instance.UID)); err != nil {
			logger.Error(err, "Agent connection cleanup failed; retaining finalizer", "instance", instance.Name)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	// AI Gateway key revocation is blocking for the same reason, and runs
	// before the workload is removed so the key cannot outlive the Agent.
	if err := r.deleteAIGatewayKey(ctx, instance); err != nil {
		logger.Error(err, "AI Gateway key revocation failed; retaining finalizer", "instance", instance.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Helm uninstall.
	logger.Info("Uninstalling Helm release", "release", releaseName, "namespace", ns)
	if err := r.HelmClient.Uninstall(ns, releaseName); err != nil {
		logger.Error(err, "helm uninstall failed, continuing cleanup", "release", releaseName)
	} else {
		logger.V(1).Info("Helm release uninstalled", "release", releaseName)
	}

	// Delete VirtualService.
	vsName := virtualServiceName(instance.Name)
	vs := &istiov1beta1.VirtualService{}
	vsKey := types.NamespacedName{Name: vsName, Namespace: instanceNamespace(instance.Name)}
	logger.V(1).Info("Deleting VirtualService", "name", vsName, "namespace", vsKey.Namespace)
	if err := r.Get(ctx, vsKey, vs); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("VirtualService not found, skipping delete", "name", vsName, "namespace", vsKey.Namespace)
		} else {
			logger.Error(err, "failed to get VirtualService for deletion", "name", vsName, "namespace", vsKey.Namespace)
			return ctrl.Result{}, fmt.Errorf("get VirtualService %s/%s for deletion: %w", vsKey.Namespace, vsName, err)
		}
	} else {
		if err := r.Delete(ctx, vs); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "delete VirtualService failed", "name", vsName, "namespace", vsKey.Namespace)
		} else {
			logger.V(1).Info("VirtualService deleted", "name", vsName, "namespace", vsKey.Namespace)
		}
	}

	// Delete namespace (cascades all resources).
	logger.Info("Deleting namespace", "namespace", ns)
	nsObj := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: ns}, nsObj); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Namespace not found, skipping delete", "namespace", ns)
		} else {
			logger.Error(err, "failed to get namespace for deletion", "namespace", ns)
			return ctrl.Result{}, fmt.Errorf("get namespace %s for deletion: %w", ns, err)
		}
	} else {
		if reconciledSandbox {
			if err := r.checkSandboxBootstrapCleanup(ctx, instance); err != nil {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
			if err := r.releaseSandboxBootstrapResources(ctx, instance, nsObj); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.Delete(ctx, nsObj); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "delete namespace failed", "namespace", ns)
		} else {
			logger.V(1).Info("Namespace deletion initiated", "namespace", ns)
		}
		if reconciledSandbox {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if reconciledSandbox {
		if err := r.finishSandboxPreparation(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.finishSandboxBootstrap(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Remove finalizer.
	before := instance.DeepCopy()
	controllerutil.RemoveFinalizer(instance, cetirunv1alpha1.Finalizer)
	if reconciledSandbox {
		controllerutil.RemoveFinalizer(instance, sandbox.HoldFinalizer)
	}
	logger.V(1).Info("Removing finalizer", "alias", alias)
	if err := r.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("CR already deleted, finalizer removal unnecessary", "alias", alias)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to remove finalizer", "alias", alias)
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	logger.Info("Finalizer removed", "alias", alias)

	r.EventRecorder.Eventf(instance, corev1.EventTypeNormal, "InstanceDeleted",
		"Instance cleanup completed for %s", alias)
	return ctrl.Result{}, nil
}

func (r *AgentReconciler) operatorNamespaceRBAC(ns string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	const roleName = "cetirun-operator-ns-role"
	const bindingName = "cetirun-operator-ns-rolebinding"

	desiredRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cetirun-operator",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"secrets", "configmaps", "services", "persistentvolumeclaims", "serviceaccounts"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"statefulsets", "deployments"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"networking.k8s.io"},
				Resources: []string{"networkpolicies"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"rbac.authorization.k8s.io"},
				Resources: []string{"roles", "rolebindings"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"batch"},
				Resources: []string{"jobs"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"networking.istio.io"},
				Resources: []string{"virtualservices", "sidecars", "destinationrules"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"security.istio.io"},
				Resources: []string{"authorizationpolicies", "peerauthentications"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
		},
	}

	// The operator's own ServiceAccount is in the namespace where the operator pod runs.
	// We bind the Role to the operator's ServiceAccount so that it can act within ns.
	desiredBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bindingName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cetirun-operator",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      r.OperatorSAName,
				Namespace: r.OperatorNamespace,
			},
		},
	}
	return desiredRole, desiredBinding
}

func (r *AgentReconciler) ensureNamespacedRBAC(ctx context.Context, ns string) error {
	logger := log.FromContext(ctx)
	desiredRole, desiredBinding := r.operatorNamespaceRBAC(ns)
	roleName, bindingName := desiredRole.Name, desiredBinding.Name
	existingRole := &rbacv1.Role{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: roleName}, existingRole)
	if errors.IsNotFound(err) {
		logger.V(1).Info("Creating namespaced Role", "role", roleName, "namespace", ns)
		if err := r.Create(ctx, desiredRole); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create namespaced Role in %s: %w", ns, err)
		}
	} else if err != nil {
		return fmt.Errorf("get namespaced Role in %s: %w", ns, err)
	} else {
		logger.V(1).Info("Updating namespaced Role", "role", roleName, "namespace", ns)
		existingRole.Rules = desiredRole.Rules
		if err := r.Update(ctx, existingRole); err != nil {
			return fmt.Errorf("update namespaced Role in %s: %w", ns, err)
		}
	}
	existingBinding := &rbacv1.RoleBinding{}
	err = r.Get(ctx, types.NamespacedName{Namespace: ns, Name: bindingName}, existingBinding)
	if errors.IsNotFound(err) {
		logger.V(1).Info("Creating namespaced RoleBinding", "binding", bindingName, "namespace", ns)
		if err := r.Create(ctx, desiredBinding); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create namespaced RoleBinding in %s: %w", ns, err)
		}
	} else if err != nil {
		return fmt.Errorf("get namespaced RoleBinding in %s: %w", ns, err)
	}
	// RoleBinding.RoleRef is immutable after creation; nothing to update.
	return nil
}

// resolveIstioRevision returns the Istio revision to use for sidecar injection.
// If IstioRevision is explicitly set, it is returned directly. Otherwise, the
// revision is auto-detected once from the AKS managed Istio control plane by
// listing ConfigMaps in aks-istio-system with the istio.io/rev label.
func (r *AgentReconciler) resolveIstioRevision(ctx context.Context) string {
	if r.IstioRevision != "" {
		return r.IstioRevision
	}

	r.istioMu.Lock()
	defer r.istioMu.Unlock()

	// Fast path: if we've already successfully resolved a revision, reuse it.
	if r.istioRevResolved != "" {
		return r.istioRevResolved
	}

	logger := log.FromContext(ctx).WithName("istio-detection")
	cmList := &corev1.ConfigMapList{}
	labelReq, err := labels.Parse("istio.io/rev")
	if err != nil {
		logger.Info("Failed to parse istio.io/rev label selector", "error", err)
		return ""
	}
	err = r.List(ctx, cmList,
		client.InNamespace("aks-istio-system"),
		client.MatchingLabelsSelector{Selector: labelReq},
	)
	if err != nil {
		logger.Info("Could not list configmaps in aks-istio-system (not AKS Istio?)", "error", err)
		return ""
	}
	if len(cmList.Items) == 0 {
		logger.Info("No configmaps with istio.io/rev label found in aks-istio-system")
		return ""
	}

	// Collect distinct non-empty istio.io/rev values from all matching ConfigMaps.
	revisions := make(map[string]struct{})
	for _, cm := range cmList.Items {
		if cm.Labels == nil {
			continue
		}
		if rev, ok := cm.Labels["istio.io/rev"]; ok && rev != "" {
			revisions[rev] = struct{}{}
		}
	}

	if len(revisions) == 0 {
		logger.Info("Configmaps with istio.io/rev label found but no non-empty istio.io/rev values detected in aks-istio-system")
		return ""
	}

	if len(revisions) > 1 {
		distinct := make([]string, 0, len(revisions))
		for rev := range revisions {
			distinct = append(distinct, rev)
		}
		logger.Info("Multiple distinct istio.io/rev values detected in aks-istio-system; auto-detection is ambiguous", "revisions", distinct)
		return ""
	}

	var rev string
	for rVal := range revisions {
		rev = rVal
		break
	}
	r.istioRevResolved = rev
	logger.Info("Auto-detected AKS Istio revision", "revision", rev)
	return r.istioRevResolved
}

// ensureNamespace creates the instance namespace if it does not already exist.
func (r *AgentReconciler) ensureNamespace(ctx context.Context, ns, alias string, owner ...*cetirunv1alpha1.Agent) error {
	logger := log.FromContext(ctx)
	desiredLabels := map[string]string{
		"app.kubernetes.io/name":       "cetirun",
		"app.kubernetes.io/instance":   alias,
		"app.kubernetes.io/managed-by": "cetirun-operator",
	}
	if rev := r.resolveIstioRevision(ctx); rev != "" {
		desiredLabels["istio.io/rev"] = rev
	}

	nsObj := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: ns}, nsObj)
	if err == nil {
		// Namespace exists — ensure labels are up to date.
		needUpdate := false
		if nsObj.Labels == nil {
			nsObj.Labels = make(map[string]string)
		}
		for k, v := range desiredLabels {
			if nsObj.Labels[k] != v {
				nsObj.Labels[k] = v
				needUpdate = true
			}
		}
		if needUpdate {
			if err := r.Update(ctx, nsObj); err != nil {
				if errors.IsForbidden(err) {
					logger.Info("Missing RBAC permissions to update namespace labels", "namespace", ns, "error", err.Error())
					return fmt.Errorf("forbidden updating namespace labels %s (check RBAC permissions): %w", ns, err)
				}
				return fmt.Errorf("update namespace labels %s: %w", ns, err)
			}
		}
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("get namespace %s: %w", ns, err)
	}

	logger.Info("Creating namespace", "namespace", ns, "alias", alias)
	nsObj = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: desiredLabels,
		},
	}
	if len(owner) > 0 && owner[0] != nil && owner[0].UID != "" {
		nsObj.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(owner[0])}
	}
	if err := r.Create(ctx, nsObj); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}
	return nil
}

// ensureGatewayToken creates the gateway token Secret if it doesn't exist,
// and prepares native runtime credentials when required.
func (r *AgentReconciler) ensureGatewayToken(ctx context.Context, instance *cetirunv1alpha1.Agent) error {
	logger := log.FromContext(ctx)
	ns := instanceNamespace(instance.Name)
	secretName := instance.Spec.GatewayTokenSecretRef

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, secret)
	if err == nil {
		logger.V(1).Info("Gateway token secret already exists", "secret", secretName, "namespace", ns)
		return r.ensureExistingGatewayToken(ctx, logger, instance, secret)
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("get gateway token secret: %w", err)
	}

	logger.Info("Creating gateway token secret", "secret", secretName, "namespace", ns)

	secret, err = r.newGatewayTokenSecret(instance)
	if err != nil {
		return err
	}
	if err := r.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create gateway token secret: %w", err)
		}
		// A concurrent reconcile may have created a legacy-form Secret after our
		// initial Get. Re-read and enrich it rather than starting Helm with a
		// Secret that is missing a required key.
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, secret); err != nil {
			return fmt.Errorf("re-read concurrently created gateway token secret: %w", err)
		}
		return r.ensureExistingGatewayToken(ctx, logger, instance, secret)
	}
	return nil
}

func (r *AgentReconciler) newGatewayTokenSecret(instance *cetirunv1alpha1.Agent) (*corev1.Secret, error) {
	token, err := generateRandomToken(32)
	if err != nil {
		return nil, err
	}
	authKey, err := generateRandomToken(32)
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Spec.GatewayTokenSecretRef,
			Namespace: instanceNamespace(instance.Name),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cetirun-operator",
				"app.kubernetes.io/instance":   instance.Spec.User.Alias,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte(token),
			"CETIRUN_AUTH_KEY":       []byte(authKey),
		},
	}
	h, err := builtin.NewRegistry().Lookup(string(instance.Spec.Agent.Type))
	if err != nil {
		return nil, err
	}
	descriptor := h.Describe()
	if descriptor.EnvironmentFile != "" {
		apiKey, err := generateRandomToken(32)
		if err != nil {
			return nil, err
		}
		secret.Data[descriptor.NativeSecretKey] = []byte(apiKey)
		secret.Data[descriptor.EnvironmentFile] = []byte(renderHermesEnv(secret.Data, apiKey, hermesCORSOrigins(r.DomainSuffix)))
	}
	return secret, nil
}

func (r *AgentReconciler) ensureExistingGatewayToken(ctx context.Context, logger logr.Logger, instance *cetirunv1alpha1.Agent, secret *corev1.Secret) error {
	if err := r.ensureCetiRunAuthKey(ctx, logger, secret); err != nil {
		return err
	}
	return r.ensureHermesSecretKeys(ctx, logger, secret, instance)
}

// ensureCetiRunAuthKey preserves the runtime key used by Hermes Webapp
// sessions.
func (r *AgentReconciler) ensureCetiRunAuthKey(ctx context.Context, logger logr.Logger, secret *corev1.Secret) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if len(secret.Data["CETIRUN_AUTH_KEY"]) != 0 {
		return nil
	}
	key, err := generateRandomToken(32)
	if err != nil {
		return err
	}
	secret.Data["CETIRUN_AUTH_KEY"] = []byte(key)
	logger.Info("Adding CetiRun auth key to gateway token secret", "secret", secret.Name)
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("update gateway token secret with CetiRun auth key: %w", err)
	}
	return nil
}

// ensureHermesSecretKeys patches the gateway-token Secret with API_SERVER_KEY and hermes.env
// when a Hermes instance uses an operator-managed existingSecret.
func (r *AgentReconciler) ensureHermesSecretKeys(ctx context.Context, logger logr.Logger, secret *corev1.Secret, instance *cetirunv1alpha1.Agent) error {
	h, err := builtin.NewRegistry().Lookup(string(instance.Spec.Agent.Type))
	if err != nil {
		return err
	}
	descriptor := h.Describe()
	if descriptor.EnvironmentFile == "" {
		return nil
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	needsUpdate := false
	apiKey := string(secret.Data[descriptor.NativeSecretKey])
	if apiKey == "" {
		var err error
		apiKey, err = generateRandomToken(32)
		if err != nil {
			return err
		}
		secret.Data[descriptor.NativeSecretKey] = []byte(apiKey)
		needsUpdate = true
	}
	hermesEnv := renderHermesEnv(secret.Data, apiKey, hermesCORSOrigins(r.DomainSuffix))
	if string(secret.Data[descriptor.EnvironmentFile]) != hermesEnv {
		secret.Data[descriptor.EnvironmentFile] = []byte(hermesEnv)
		needsUpdate = true
	}
	if !needsUpdate {
		return nil
	}
	logger.Info("Updating gateway token secret with Hermes environment", "secret", secret.Name)
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("update gateway token secret with hermes environment: %w", err)
	}
	return nil
}

func renderHermesEnv(data map[string][]byte, apiKey string, corsOrigins string) string {
	var buf strings.Builder
	buf.WriteString("API_SERVER_KEY=" + apiKey + "\n")
	if corsOrigins != "" {
		buf.WriteString("API_SERVER_CORS_ORIGINS=" + corsOrigins + "\n")
	}
	if v := secretEnvValue(data, "BRAVE_API_KEY"); v != "" {
		buf.WriteString("BRAVE_API_KEY=" + v + "\n")
	}
	if v := secretEnvValue(data, "PERPLEXITY_API_KEY"); v != "" {
		buf.WriteString("PERPLEXITY_API_KEY=" + v + "\n")
	}
	return buf.String()
}

// hermesCORSOrigins returns the Origin allow-list for the Hermes API server,
// derived from the operator's configured domain suffix. Without this, the
// portal chat UI is rejected with 403 by Hermes' CORS middleware because the
// API server denies unknown browser origins.
func hermesCORSOrigins(domainSuffix string) string {
	if domainSuffix == "" {
		return ""
	}
	return "https://" + domainSuffix
}

func secretEnvValue(data map[string][]byte, key string) string {
	if len(data[key]) > 0 {
		return string(data[key])
	}
	for _, line := range strings.Split(string(data["hermes.env"]), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

// ensureVirtualService creates or updates the Istio VirtualService for this
// instance and reports whether it created, updated or left it unchanged.
func (r *AgentReconciler) ensureVirtualService(ctx context.Context, instance *cetirunv1alpha1.Agent, access ...customAccess) (controllerutil.OperationResult, error) {
	requireMTLS := false
	reconciled, err := r.usesReconciledSandbox(ctx, instance)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}
	if reconciled {
		resolution, err := r.resolvePolicy(ctx, instance)
		if err != nil {
			return controllerutil.OperationResultNone, err
		}
		if resolution.policy.Spec.Network.Ingress.Mode == cetirunv1alpha1.AgentPolicyIngressDisabled {
			err := r.withdrawKubernetesSandboxRoute(ctx, instance)
			instance.Status.AccessURL = ""
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionIngressReady,
				metav1.ConditionTrue, "IngressDisabled", "Sandbox ingress is disabled")
			return controllerutil.OperationResultNone, err
		}
		requireMTLS = resolution.policy.Spec.Network.Ingress.RequireMTLS
	}
	logger := log.FromContext(ctx)
	desired := buildVirtualService(instance, r.DomainSuffix, r.IstioGateway, access...)
	if desired == nil {
		return controllerutil.OperationResultNone, fmt.Errorf("unsupported Agent native access")
	}
	if requireMTLS {
		if err := r.restrictMTLSRoute(ctx, instance, desired); err != nil {
			withdrawErr := r.withdrawKubernetesSandboxRoute(ctx, instance)
			return controllerutil.OperationResultNone, stderrors.Join(err, withdrawErr)
		}
	}
	if reconciled {
		if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
			return controllerutil.OperationResultNone, err
		}
		if desired.Annotations == nil {
			desired.Annotations = map[string]string{}
		}
		desired.Annotations[customAgentUID] = string(instance.UID)
	}
	vsName := virtualServiceName(instance.Name)
	vsNS := instanceNamespace(instance.Name)

	existing := &istiov1beta1.VirtualService{}
	err = r.Get(ctx, types.NamespacedName{Namespace: vsNS, Name: vsName}, existing)
	if errors.IsNotFound(err) {
		logger.Info("Creating VirtualService", "name", vsName, "namespace", vsNS)
		if err := r.Create(ctx, desired); err != nil {
			return controllerutil.OperationResultNone, fmt.Errorf("create VirtualService: %w", err)
		}
		return controllerutil.OperationResultCreated, nil
	}
	if err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("get VirtualService: %w", err)
	}
	if reconciled && !sandboxBuiltinOwned(existing, instance) {
		return controllerutil.OperationResultNone, fmt.Errorf("Sandbox route ownership is unconfirmed; existing routes are not adopted")
	}

	// Update existing VirtualService by patching it with the desired spec fields.
	// We avoid direct struct assignment because the proto message contains a sync.Mutex.
	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom && !hasAgentOwner(existing, instance) {
		return controllerutil.OperationResultNone, fmt.Errorf("Custom platform route belongs to another owner")
	}
	ownedAnnotationsMatch := (!reconciled && instance.Spec.Agent.Type != cetirunv1alpha1.AgentTypeCustom) ||
		existing.Annotations[customAgentUID] == string(instance.UID)
	if proto.Equal(&existing.Spec, &desired.Spec) && maps.Equal(existing.Labels, desired.Labels) && ownedAnnotationsMatch {
		return controllerutil.OperationResultNone, nil
	}
	logger.V(1).Info("Updating VirtualService", "name", vsName, "namespace", vsNS)
	existing.Spec.Hosts = desired.Spec.Hosts
	existing.Spec.Gateways = desired.Spec.Gateways
	existing.Spec.Http = desired.Spec.Http
	existing.Labels = desired.Labels
	if reconciled || instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[customAgentUID] = string(instance.UID)
		existing.Spec.ExportTo = desired.Spec.ExportTo
	}
	if err := r.Update(ctx, existing); err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("update VirtualService: %w", err)
	}
	return controllerutil.OperationResultUpdated, nil
}

// rejectReservedUIInstance removes the externally routable VirtualService for
// an instance whose name conflicts with the native /ui/<instance>/ namespace.
func (r *AgentReconciler) rejectReservedUIInstance(ctx context.Context, instance *cetirunv1alpha1.Agent) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	vsName := virtualServiceName(instance.Name)
	vs := &istiov1beta1.VirtualService{}
	vsKey := types.NamespacedName{Name: vsName, Namespace: instanceNamespace(instance.Name)}

	if err := r.Get(ctx, vsKey, vs); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get VirtualService for reserved instance %s/%s: %w", vsKey.Namespace, vsName, err)
		}
	} else if err := r.Delete(ctx, vs); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete VirtualService for reserved instance %s/%s: %w", vsKey.Namespace, vsName, err)
	} else {
		logger.Info("Removed VirtualService for reserved instance name", "name", instance.Name, "virtualService", vsName)
	}

	return r.failWith(ctx, instance, "ReservedInstanceName", "instance name 'ui' is reserved for native agent routes")
}

// ensureTailscaleAuthKey reads the Tailscale auth key from the user-supplied Secret
// and patches it into the gateway token Secret so the StatefulSet can read it.
// The chart's Secret template is skipped when secrets.existingSecret is set, so
// we must write TAILSCALE_AUTH_KEY directly into the gateway token Secret.
// No-op if spec.Tailscale is nil.
func (r *AgentReconciler) ensureTailscaleAuthKey(ctx context.Context, instance *cetirunv1alpha1.Agent) error {
	ns := instanceNamespace(instance.Name)
	gwSecretName := instance.Spec.GatewayTokenSecretRef

	if instance.Spec.Tailscale == nil {
		// Tailscale disabled — remove the key from the gateway Secret if present.
		gwSecret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwSecretName}, gwSecret); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("get gateway secret %s/%s: %w", ns, gwSecretName, err)
		}
		if _, exists := gwSecret.Data[cetirunv1alpha1.TailscaleAuthKeyField]; exists {
			delete(gwSecret.Data, cetirunv1alpha1.TailscaleAuthKeyField)
			if err := r.Update(ctx, gwSecret); err != nil {
				return fmt.Errorf("remove %s from gateway secret: %w", cetirunv1alpha1.TailscaleAuthKeyField, err)
			}
		}
		return nil
	}

	// Read auth key from the user-supplied Secret.
	tsSecretName := instance.Spec.Tailscale.AuthKeySecretRef
	tsSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: tsSecretName}, tsSecret); err != nil {
		return fmt.Errorf("get Tailscale auth key secret %s/%s: %w", ns, tsSecretName, err)
	}
	authKey, ok := tsSecret.Data[cetirunv1alpha1.TailscaleAuthKeyField]
	if !ok || len(authKey) == 0 {
		return fmt.Errorf("secret %s/%s missing or empty key %q", ns, tsSecretName, cetirunv1alpha1.TailscaleAuthKeyField)
	}

	// Patch the auth key into the gateway token Secret.
	gwSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwSecretName}, gwSecret); err != nil {
		return fmt.Errorf("get gateway secret %s/%s: %w", ns, gwSecretName, err)
	}
	if gwSecret.Data == nil {
		gwSecret.Data = make(map[string][]byte)
	}
	if string(gwSecret.Data[cetirunv1alpha1.TailscaleAuthKeyField]) != string(authKey) {
		gwSecret.Data[cetirunv1alpha1.TailscaleAuthKeyField] = authKey
		if err := r.Update(ctx, gwSecret); err != nil {
			return fmt.Errorf("patch %s into gateway secret: %w", cetirunv1alpha1.TailscaleAuthKeyField, err)
		}
	}
	return nil
}

// setPhase updates only the Phase field in the CR status.
func (r *AgentReconciler) setPhase(ctx context.Context, instance *cetirunv1alpha1.Agent, phase string) error {
	instance.Status.Phase = phase
	return r.updateStatus(ctx, instance)
}

// agentIsCurrent reports whether instance is the Agent's latest revision.
//
// A pass can start from the informer's copy before the status of the pass
// just before it arrives. Measured on the e2e cluster after a Copilot sign-in:
// such a pass rendered from the older status (8 instead of 23 models), its
// Helm upgrade went through, and its status patch was then refused. The
// recorded configuration hash still described the newer render, so every
// later pass skipped the install and the Agent kept the older configuration.
// Installing only from the latest revision keeps the release and the
// recorded hash in step.
func (r *AgentReconciler) agentIsCurrent(ctx context.Context, instance *cetirunv1alpha1.Agent) (bool, error) {
	if r.APIReader == nil || instance.ResourceVersion == "" {
		return true, nil
	}
	latest := &cetirunv1alpha1.Agent{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(instance), latest); err != nil {
		return false, fmt.Errorf("read Agent before Helm install: %w", err)
	}
	return latest.UID == instance.UID && latest.ResourceVersion == instance.ResourceVersion, nil
}

// updateStatus patches the CR status sub-resource.
func (r *AgentReconciler) updateStatus(ctx context.Context, instance *cetirunv1alpha1.Agent) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &cetirunv1alpha1.Agent{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
		return fmt.Errorf("read Agent before status patch: %w", err)
	}
	// Callers already changed their typed status. Read the same revision to
	// construct a delta without deleting fields introduced by a newer binary.
	if instance.ResourceVersion == "" || current.ResourceVersion != instance.ResourceVersion || current.UID != instance.UID {
		return errors.NewConflict(cetirunv1alpha1.GroupVersion.WithResource("agents").GroupResource(), instance.Name,
			fmt.Errorf("Agent changed before its status could be patched"))
	}
	// An unchanged status must not issue a patch or generate a watch event.
	if equality.Semantic.DeepEqual(current.Status, instance.Status) {
		return nil
	}
	before := current.DeepCopy()
	current.Status = *instance.Status.DeepCopy()
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	instance.Status = current.Status
	instance.ResourceVersion = current.ResourceVersion
	emitSandboxAgentTransition(ctx, before, current)
	return nil
}

// failWith sets status to Failed with the given reason/message and returns a requeue result.
// TODO: failWith returns a non-nil error, which causes controller-runtime to ignore RequeueAfter
// and instead use exponential back-off. For now this is acceptable (back-off is still retried).
// If precise retry intervals are needed, change the return to (ctrl.Result{RequeueAfter: ...}, nil)
// and log the error explicitly.
func (r *AgentReconciler) failWith(ctx context.Context, instance *cetirunv1alpha1.Agent, reason, message string) (ctrl.Result, error) {
	instance.Status.Phase = cetirunv1alpha1.PhaseFailed
	setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
		metav1.ConditionFalse, reason, message)
	_ = r.updateStatus(ctx, instance)
	r.EventRecorder.Event(instance, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{RequeueAfter: 60 * time.Second}, fmt.Errorf("%s: %s", reason, message)
}

// enqueueInstancesForTeam maps an Team event to reconcile requests for
// all Agents that reference the team via spec.team.
func (r *AgentReconciler) enqueueInstancesForTeam(ctx context.Context, obj client.Object) []reconcile.Request {
	team, ok := obj.(*cetirunv1alpha1.Team)
	if !ok {
		return nil
	}
	list := &cetirunv1alpha1.AgentList{}
	if err := r.List(ctx, list, client.MatchingFields{instanceTeamIndex: team.Name}); err != nil {
		log.FromContext(ctx).Error(err, "failed to list instances for team watch", "team", team.Name)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, inst := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: inst.Name},
		})
	}
	return reqs
}
