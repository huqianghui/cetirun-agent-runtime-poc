// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	customAgentUID = "cetirun.microsoft.com/agent-uid"
	customPlanHash = "cetirun.microsoft.com/custom-plan-hash"
)

// CustomBindingProvider is implemented by the platform capability owners, not
// by a Harness or a tenant. Resolve authorizes the exact Agent generation and
// image, namespace, model Secret, HOME name/claim template, immutable context and peers.
// ApproveApply performs old-writer shutdown and storage handoff for this exact
// plan: scale the supplied StatefulSet to zero without changing its UID, owner
// or protected annotations, and wait for Pods referencing HOME to terminate.
// Providers need workload patch/Pod read permissions and must honor context
// deadlines. The default operator has no provider and waits.
type CustomBindingProvider interface {
	Resolve(context.Context, *api.Agent, *api.Team) (runtimebindings.Custom, error)
	ApproveApply(context.Context, *api.Agent, *appsv1.StatefulSet, CustomApplyOperation) (CustomApplyLease, error)
	// ResumeApply returns a completion-capable handle for the same Agent UID
	// and operation ID, even after completion or a controller restart. It
	// never grants a new lease or repeats handoff. For an unallocated operation
	// it may only acknowledge not-started after confirming no lease was granted.
	ResumeApply(context.Context, *api.Agent, CustomApplyOperation) (CustomApplyLease, error)
}

type CustomApplyOutcome string

const (
	CustomApplyNotStarted CustomApplyOutcome = "not-started"
	CustomApplySucceeded  CustomApplyOutcome = "applied"
	CustomApplyUnknown    CustomApplyOutcome = "unknown"
)

// The provider holds exclusive lifecycle coordination through Complete. Check
// must fail if the approval has expired or been revoked. The approval key binds
// both input and final manifest digests. Complete must be idempotent for the
// operation ID and may be retried through ResumeApply after any failure.
type CustomApplyLease interface {
	Check(context.Context) error
	Complete(context.Context, CustomApplyOutcome) error
}

func (r *AgentReconciler) customWaiting(ctx context.Context, instance *api.Agent, reason, message string) (ctrl.Result, error) {
	previous := instance.DeepCopy().Status
	if !r.keepCustomObservation(ctx, instance) {
		instance.Status.AccessURL = ""
		instance.Status.Phase = api.PhasePending
		setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, reason, message)
	}
	setCondition(&instance.Status.Conditions, api.ConditionAgentConfigReady, metav1.ConditionFalse, reason, message)
	if err := r.updateCustomStatus(ctx, instance, previous); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *AgentReconciler) customOverrides(instance *api.Agent, team *api.Team) (map[string]any, error) {
	if agentIdentityRequested(instance) {
		return nil, fmt.Errorf("UnsupportedFeature: Custom does not support Agent Identity")
	}
	return r.customRuntimeValues(instance, team)
}

func (r *AgentReconciler) customRuntimeValues(instance *api.Agent, team *api.Team) (map[string]any, error) {
	if instance.Spec.GatewayTokenSecretRef != "" || instance.Spec.Tailscale != nil {
		return nil, fmt.Errorf("UnsupportedFeature: Custom does not support native credentials or Tailscale")
	}
	shared := runtimebindings.SharedValues(r.CetiRunHelmValues)
	if err := runtimebindings.ValidateOverrides(shared); err != nil {
		return nil, fmt.Errorf("CETIRUN_HELM_VALUES platform configuration: %w", err)
	}
	values := platformRuntimeDefaults(instance.Name)
	for key, value := range shared {
		values[key] = deepMerge(values[key], value)
	}
	// With the Skill Marketplace, Team marketplace skills are dropped here, not
	// passed as ClawHub installs, which Custom refuses below. The HOME
	// initializer copies them from the Team's release instead.
	if err := skillsWithoutMarketplace(team, instance.Spec.SkillOverrides, r.SkillMarketplace); err != nil {
		return nil, err
	}
	if err := applySkillValues(values, team, instance.Spec.SkillOverrides, r.SkillsStorageAccount, r.SkillMarketplace); err != nil {
		return nil, err
	}
	if err := runtimebindings.ValidateCustomSkills(values["skills"]); err != nil {
		return nil, err
	}
	local := map[string]any{}
	for key, raw := range instance.Spec.Helm.Values {
		var value any
		if err := json.Unmarshal(raw.Raw, &value); err != nil {
			return nil, fmt.Errorf("invalid Custom Helm override")
		}
		local[key] = value
	}
	// RuntimeClass and scheduling are platform choices; instance overrides can
	// change resource requests/limits, but not select another isolation boundary.
	for key := range local {
		if key == "desktop" {
			desktop, ok := local[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid Custom desktop override")
			}
			for field := range desktop {
				switch field {
				case "enabled", "logging", "resources", "screenResolution":
				default:
					return nil, fmt.Errorf("UnsupportedFeature: Custom instance desktop.%s is platform-owned", field)
				}
			}
		} else if key == "ttyd" {
			settings, ok := local[key].(map[string]any)
			if !ok || len(settings) != 1 {
				return nil, fmt.Errorf("Custom instance ttyd accepts only enabled")
			}
			if _, ok := settings["enabled"].(bool); !ok {
				return nil, fmt.Errorf("Custom instance ttyd.enabled must be a boolean")
			}
		} else if key != "resources" {
			return nil, fmt.Errorf("UnsupportedFeature: Custom instance helm.values.%s is not supported", key)
		}
		values[key] = deepMerge(values[key], local[key])
	}
	return values, nil
}

func (r *AgentReconciler) reconcileCustom(ctx context.Context, instance *api.Agent, team *api.Team) (result ctrl.Result, returnedErr error) {
	previous := instance.DeepCopy().Status
	// Suspension is allowed even if a model/context provider is unavailable.
	if instance.Spec.Suspended {
		return r.suspendCustom(ctx, instance)
	}
	values, err := r.customOverrides(instance, team)
	if err != nil {
		return r.rejectCustomConfig(ctx, instance, err)
	}
	if r.CustomBindings == nil {
		return r.customWaiting(ctx, instance, "CustomBindingsPending",
			"Custom requires platform model, HOME, context, policy and lifecycle bindings")
	}
	resolveCtx, cancelResolve := context.WithTimeout(ctx, customProviderTimeout)
	bindings, err := r.CustomBindings.Resolve(resolveCtx, instance.DeepCopy(), team)
	cancelResolve()
	if err != nil {
		return r.customWaiting(ctx, instance, "CustomBindingsPending", "Platform binding resolution is not ready")
	}
	bindings.RestartRequestID = instance.Spec.RestartRequestID
	request := agents.Request{Type: "custom", Image: *instance.Spec.Agent.Image, Port: instance.Spec.Agent.Port, WebUI: instance.Spec.Agent.WebUI}
	if instance.Spec.Agent.Username != nil {
		request.Username = *instance.Spec.Agent.Username
	}
	resolved, err := agents.ResolveCustom(request)
	if err != nil {
		return r.rejectCustomConfig(ctx, instance, err)
	}
	if bindings.AgentName != instance.Name || bindings.AgentUID != string(instance.UID) || bindings.Generation != instance.Generation ||
		bindings.Namespace.Name != instanceNamespace(instance.Name) {
		return r.customWaiting(ctx, instance, "CustomBindingRejected", "Platform bindings do not belong to this Agent generation")
	}
	if ready, err := r.bindCustomAgentIdentity(ctx, instance, &bindings); err != nil {
		return r.rejectCustomConfig(ctx, instance, err)
	} else if !ready {
		return r.customWaiting(ctx, instance, "AgentIdentityBindingPending", "The instance Agent Identity binding is not ready")
	}
	if err := r.bindCustomGateway(ctx, instance, &bindings); err != nil {
		return r.customWaiting(ctx, instance, "AIGatewayBindingPending", "The AI Gateway binding is not ready")
	}
	if bindings.Memory != nil {
		return r.customWaiting(ctx, instance, "MemoryBindingRejected", "Only the Memory asset owner may supply a binding")
	}
	if err := bindings.Validate(resolved); err != nil {
		return r.customWaiting(ctx, instance, "CustomBindingRejected", err.Error())
	}
	previousCallers := append([]runtimebindings.Peer(nil), bindings.Network.Callers...)
	if r.ChatAccess.Enabled && resolved.Port != nil && !resolved.WebUI {
		for _, caller := range r.ChatAccess.Callers {
			if err := caller.Validate(); err != nil {
				return r.customWaiting(ctx, instance, "ChatAccessInvalid", err.Error())
			}
			bindings.Network.Callers = append(bindings.Network.Callers, runtimebindings.Peer{Namespace: caller.Namespace, PodLabels: caller.PodLabels})
		}
	}
	homeBound, err := r.validateCustomObjects(ctx, instance, bindings)
	if err != nil {
		return r.customWaiting(ctx, instance, "CustomBindingRejected", err.Error())
	}
	if customTerminalRequested(values) && len(bindings.Network.APIServer) == 0 {
		requestCtx, cancel := context.WithTimeout(ctx, customProviderTimeout)
		bindings.Network.APIServer, err = r.terminalAPIEndpoints(requestCtx)
		cancel()
		// A legacy Chart does not need this binding. New terminal Charts reject
		// an absent binding before any resources are applied.
		if err != nil {
			bindings.Network.APIServer = nil
		}
	}
	if customTerminalRequested(values) {
		if err := r.ensureTerminalReadRoles(ctx, instance); err != nil {
			return r.customWaiting(ctx, instance, "TerminalRBACBindingPending", "Terminal read roles are not ready or have foreign ownership")
		}
	}
	// A general binding provider cannot inject a pull Secret. Only the registry
	// synchronizer can supply it after source ownership and isolation checks.
	if bindings.PullSecretName != "" {
		return r.registryWaiting(ctx, instance, "RegistryBindingRejected")
	}
	if instance.Spec.Agent.RegistrySecretRef != nil {
		bindings.PullSecretName, err = r.syncRegistryCredentials(ctx, instance, instanceServiceName(instance.Name), bindings.Namespace.UID)
		if err != nil {
			return r.registryWaiting(ctx, instance, err.Error())
		}
	}
	values["agentType"] = "custom"
	desktop := map[string]any{"novncPathPrefix": instance.Name + "/desktop/"}
	if r.ChartRegistry != "" {
		desktop["image"] = map[string]any{"repository": r.ChartRegistry + "/cetirun-desktop"}
		values["ttyd"] = deepMerge(values["ttyd"], map[string]any{
			"image": map[string]any{"repository": r.ChartRegistry + "/cetirun-terminal"},
		})
	}
	values["desktop"] = deepMerge(values["desktop"], desktop)
	values["agentRequest"] = runtimeRequest(instance, "")
	values["agentRequest"].(map[string]any)["releaseName"] = helmReleaseName(instance.Name)
	values["customBindings"] = bindings
	values["memoryOptions"] = r.memoryOptions()
	values["fullnameOverride"] = instanceServiceName(instance.Name)
	values["replicaCount"] = 1
	current := &appsv1.StatefulSet{}
	err = r.customReader().Get(ctx, types.NamespacedName{Namespace: bindings.Namespace.Name, Name: instanceServiceName(instance.Name)}, current)
	if apierrors.IsNotFound(err) {
		current = nil
	} else if err != nil {
		return ctrl.Result{}, err
	}
	signature, err := customInputSignature(instance.Spec.Helm.Version, values)
	if err != nil {
		return r.rejectCustomConfig(ctx, instance, err)
	}
	prepared, cached := r.cachedCustomPlan(instance, signature, current)
	if !cached {
		prepared, err = r.HelmClient.Prepare(bindings.Namespace.Name, values, instance.Spec.Helm.Version)
		if err != nil {
			return r.customPreparationFailed(ctx, instance, err)
		}
	}
	// A replacement HelmInstaller must preserve the final-plan validation.
	if r.ChatAccess.Enabled && !prepared.SupportsPortalChat() {
		bindings.Network.Callers = previousCallers
		values["customBindings"] = bindings
		signature, err = customInputSignature(instance.Spec.Helm.Version, values)
		if err != nil {
			return r.rejectCustomConfig(ctx, instance, err)
		}
		prepared, cached = r.cachedCustomPlan(instance, signature, current)
		if !cached {
			prepared, err = r.HelmClient.Prepare(bindings.Namespace.Name, values, instance.Spec.Helm.Version)
			if err != nil {
				return r.customPreparationFailed(ctx, instance, err)
			}
		}
	}
	if err := prepared.ValidateCustom(bindings.Namespace.Name, helmReleaseName(instance.Name)); err != nil {
		return r.rejectCustomConfig(ctx, instance, err)
	}
	if err := r.customPodReadAccess(ctx, instance); err != nil {
		return r.customPreparationFailed(ctx, instance, err)
	}
	if current != nil && (!hasAgentOwner(current, instance) || current.Annotations[customAgentUID] != string(instance.UID) ||
		!current.DeletionTimestamp.IsZero()) {
		return r.customWaiting(ctx, instance, "CustomOwnershipConflict", "Existing workload or HOME ownership is not confirmed")
	}
	if err := r.ensureCustomOwnership(ctx, instance, bindings, current); err != nil {
		return r.customWaiting(ctx, instance, "CustomOwnershipConflict", err.Error())
	}
	// Ordinary Custom does not receive a Connections MCP runtime endpoint, but
	// the Broker still owns its per-Agent binding records. Reconcile this after
	// the immutable ownership receipt exists so the Broker can prove the
	// namespace without changing Custom namespace garbage collection semantics.
	if err := r.reconcileConnectionsMCPBrokerRBAC(ctx, instance, true); err != nil {
		return r.customWaiting(ctx, instance, "BrokerStorageAccessFailed", "Broker storage access setup failed")
	}
	if current != nil &&
		strings.HasPrefix(instance.Status.ObservedOperatorHash, prepared.PlanHash+":") &&
		instance.Status.ObservedOperatorHash != prepared.CacheKey() {
		return r.customWaiting(ctx, instance, "RendererDrift", "The same Custom inputs produced different manifests; explicit review is required")
	}
	applied := cached
	if !cached {
		applied, err = r.HelmClient.CustomApplied(bindings.Namespace.Name, helmReleaseName(instance.Name), prepared)
		if err != nil {
			return r.customWaiting(ctx, instance, "CustomApplicationUnknown", "Cannot verify the previous Custom application")
		}
	}
	workloadChanged := current == nil || current.Annotations[customPlanHash] != prepared.WorkloadHash ||
		current.Spec.Replicas == nil || *current.Spec.Replicas != 1
	unchanged := applied && !workloadChanged
	if !unchanged {
		if ref := instance.Spec.Agent.RegistrySecretRef; ref != nil {
			if err := registryauth.CheckIsolation(ctx, r.registryClient(), r.registryNamespace(), bindings.Namespace.Name, instanceServiceName(instance.Name), ref.Name); err != nil {
				return r.registryWaiting(ctx, instance, "RegistryIsolationUnverified")
			}
		}
		if registryManaged(instance) && r.checkRegistryIntent(ctx, instance) != nil {
			return r.registryWaiting(ctx, instance, "RegistryInputChanged")
		}
		if !workloadChanged {
			if err := r.checkAgentGeneration(ctx, instance); err != nil {
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.HelmClient.InstallOrUpgrade(bindings.Namespace.Name, helmReleaseName(instance.Name), prepared); err != nil {
				return r.customWaiting(ctx, instance, "CustomApplyFailed", "Platform control update failed; the Agent writer was not stopped")
			}
		} else {
			operation, err := r.beginCustomCompletion(ctx, instance, prepared.CacheKey())
			if err != nil {
				return r.customWaiting(ctx, instance, "CustomCompletionPending", "Cannot persist the next apply operation")
			}
			outcome := CustomApplyNotStarted
			var lease CustomApplyLease
			defer func() {
				completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if err := r.finishCustomCompletion(completionCtx, instance, operation, outcome, lease); err != nil && returnedErr == nil {
					returnedErr = fmt.Errorf("Custom apply completion is pending")
				}
			}()
			approveCtx, cancelApprove := context.WithTimeout(ctx, customProviderTimeout)
			lease, err = r.CustomBindings.ApproveApply(approveCtx, instance.DeepCopy(), current.DeepCopy(), operation)
			cancelApprove()
			if err != nil || lease == nil {
				return r.customWaiting(ctx, instance, "CustomMaintenancePending", "Waiting for approved old-writer shutdown and storage handoff")
			}
			if current != nil {
				stopped := &appsv1.StatefulSet{}
				if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(current), stopped); err != nil ||
					stopped.UID != current.UID || stopped.Spec.Replicas == nil || *stopped.Spec.Replicas != 0 ||
					!stopped.DeletionTimestamp.IsZero() || stopped.Annotations[customAgentUID] != string(instance.UID) ||
					!hasAgentOwner(stopped, instance) {
					return r.customWaiting(ctx, instance, "CustomMaintenancePending", "Existing workload must be stopped without changing its ownership")
				}
			}
			if err := r.noCustomWriters(ctx, bindings); err != nil {
				return r.customWaiting(ctx, instance, "CustomMaintenancePending", err.Error())
			}
			// Re-read credentials, namespace, context and HOME specification after handoff.
			if _, err := r.validateCustomObjects(ctx, instance, bindings); err != nil {
				return r.customWaiting(ctx, instance, "CustomBindingRejected", err.Error())
			}
			if err := r.checkAgentGeneration(ctx, instance); err != nil {
				return ctrl.Result{Requeue: true}, nil
			}
			// If the controller exits during Helm, recovery must not report that
			// no application took place. Persist uncertainty before the first write.
			if err := r.recordCustomOutcome(ctx, instance, operation, CustomApplyUnknown); err != nil {
				return ctrl.Result{}, err
			}
			checkCtx, cancelCheck := context.WithTimeout(ctx, customProviderTimeout)
			checkErr := lease.Check(checkCtx)
			cancelCheck()
			if checkErr != nil {
				return r.customWaiting(ctx, instance, "CustomMaintenancePending", "Custom apply approval expired or was revoked")
			}
			if err := r.checkAgentGeneration(ctx, instance); err != nil {
				return ctrl.Result{Requeue: true}, nil
			}
			outcome = CustomApplyUnknown
			if err := r.HelmClient.InstallOrUpgrade(bindings.Namespace.Name, helmReleaseName(instance.Name), prepared); err != nil {
				return r.customWaiting(ctx, instance, "CustomApplyFailed", "Custom Helm application failed; existing data was not reset")
			}
			outcome = CustomApplySucceeded
			// Observe the result in a later reconcile, not the pre-apply workload.
			current = nil
		}
	}
	if !unchanged {
		r.customEvent(instance, corev1.EventTypeNormal, "HelmInstalled", "Validated Custom resources applied")
	}
	if registryManaged(instance) {
		if err := r.cleanupRegistryCredentials(ctx, instance, false); err != nil {
			return r.registryWaiting(ctx, instance, "RegistryCleanupPending")
		}
	}
	if err := r.recoverRegistryPull(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	setCondition(&instance.Status.Conditions, api.ConditionHelmDeployed, metav1.ConditionTrue,
		"CustomChartApplied", "Custom runtime plan applied")
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.ObservedOperatorHash = prepared.CacheKey()
	instance.Status.AccessURL = ""
	instance.Status.Namespace = bindings.Namespace.Name
	instance.Status.HelmReleaseName = helmReleaseName(instance.Name)
	setCondition(&instance.Status.Conditions, api.ConditionAgentConfigReady, metav1.ConditionTrue,
		"CustomConfigResolved", "Custom runtime bindings and plan validated")
	chatStatus, chatReason := metav1.ConditionFalse, "ChatAccessDisabled"
	if r.ChatAccess.Enabled && len(r.ChatAccess.Callers) > 0 && resolved.Port != nil && !resolved.WebUI {
		chatReason = "ChatContractUnsupported"
		if prepared.SupportsPortalChat() {
			chatStatus, chatReason = metav1.ConditionTrue, "ChatAccessConfigured"
		}
	}
	if err := r.reconcileChatRoute(ctx, instance, chatStatus == metav1.ConditionTrue); err != nil {
		chatStatus, chatReason = metav1.ConditionFalse, "ChatRouteUnavailable"
	}
	setCondition(&instance.Status.Conditions, chataccess.Condition, chatStatus, chatReason, "Reports private Chat configuration, not model or network qualification")
	r.configureCustomAccess(ctx, instance, customAccess{
		desktop:      prepared.Values["desktop"].(map[string]any)["enabled"] == true,
		terminal:     prepared.Values["ttyd"].(map[string]any)["enabled"] == true,
		terminalPort: uint32(prepared.CustomTerminalPort()),
	})
	ready := homeBound && r.customPodReady(ctx, current)
	if ready {
		ready, err = r.restartPodReady(ctx, instance, current)
		if err != nil {
			return r.customWaiting(ctx, instance, "RestartObservationFailed", err.Error())
		}
	}
	instance.Status.Phase = api.PhaseProvisioning
	status, reason, message := metav1.ConditionFalse, "CustomPodPending", "Waiting for the applied Custom Pod"
	if ready {
		instance.Status.Phase = api.PhaseReady
		instance.Status.ObservedRestartRequestID = instance.Spec.RestartRequestID
		status, reason = metav1.ConditionTrue, "CustomPodReady"
		message = "Custom Pod is ready; model calls, context use and session recovery are separate qualifications"
		if resolved.Port == nil {
			message = "Headless Custom process is running; no HTTP or model readiness is asserted"
		}
	}
	setCondition(&instance.Status.Conditions, api.ConditionPodReady, status, reason, message)
	setCondition(&instance.Status.Conditions, api.ConditionReady, status, reason, message)
	if err := r.updateCustomStatus(ctx, instance, previous); err != nil {
		return ctrl.Result{}, err
	}
	if !cached {
		r.customPlans.Store(instance.Name, cachedCustomPlan{signature: signature, plan: prepared, expires: time.Now().Add(10 * time.Minute)})
	}
	if ready {
		if aiGatewayCopilotPending(instance, r.AIGateway.Enabled) {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		if chatReason == "ChatRouteUnavailable" {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func referenceMatches(obj client.Object, ref runtimebindings.ObjectReference) bool {
	return string(obj.GetUID()) == ref.UID && obj.GetResourceVersion() == ref.Revision &&
		obj.GetDeletionTimestamp().IsZero()
}

func (r *AgentReconciler) customReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *AgentReconciler) validateCustomObjects(ctx context.Context, instance *api.Agent, b runtimebindings.Custom) (bool, error) {
	if b.Home.Claim == nil {
		return false, fmt.Errorf("HomeBindingInvalid: approved HOME claim template is required")
	}
	ns := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, types.NamespacedName{Name: b.Namespace.Name}, ns); err != nil || !referenceMatches(ns, b.Namespace) {
		return false, fmt.Errorf("NamespaceBindingInvalid: approved namespace is unavailable or changed")
	}
	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: b.Namespace.Name, Name: name}
	}
	secret := &corev1.Secret{}
	if err := r.customReader().Get(ctx, key(b.Model.Name), secret); err != nil || !referenceMatches(secret, b.Model.ObjectReference) ||
		secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 1 || len(secret.Data[b.Model.Key]) == 0 {
		return false, fmt.Errorf("ModelBindingInvalid: dedicated AI Gateway key Secret is unavailable or changed")
	}
	if !hasAgentOwner(secret, instance) {
		return false, fmt.Errorf("ModelBindingInvalid: Secret owner does not match Agent UID")
	}
	home := &corev1.PersistentVolumeClaim{}
	homeBound := false
	if err := r.customReader().Get(ctx, key(b.Home.Name), home); apierrors.IsNotFound(err) {
		// StatefulSet volumeClaimTemplates creates HOME with the first Pod.
	} else if err != nil {
		return false, fmt.Errorf("HomeBindingInvalid: approved persistent HOME is unavailable")
	} else {
		if !home.DeletionTimestamp.IsZero() || home.Status.Phase == corev1.ClaimLost {
			return false, fmt.Errorf("HomeBindingInvalid: approved HOME is deleting or lost")
		}
		if home.Annotations[customAgentUID] != string(instance.UID) || !b.Home.Claim.Matches(home) {
			return false, fmt.Errorf("HomeBindingInvalid: HOME ownership or storage specification changed")
		}
		for _, owner := range home.OwnerReferences {
			if owner.Kind == "Agent" && owner.UID != instance.UID {
				return false, fmt.Errorf("HomeBindingInvalid: PVC belongs to another Agent")
			}
		}
		homeBound = home.Status.Phase == corev1.ClaimBound && home.Spec.VolumeName != ""
	}
	context := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, key(b.Context.Name), context); err != nil || !referenceMatches(context, b.Context.ObjectReference) ||
		context.Immutable == nil || !*context.Immutable {
		return false, fmt.Errorf("ContextBindingInvalid: approved immutable snapshot is unavailable or changed")
	}
	for key := range b.Context.Files {
		if context.Data[key] == "" {
			return false, fmt.Errorf("ContextBindingInvalid: approved context file is missing or empty")
		}
	}
	return homeBound, nil
}

func (r *AgentReconciler) noCustomWriters(ctx context.Context, b runtimebindings.Custom) error {
	pods := &corev1.PodList{}
	if err := r.customReader().List(ctx, pods, client.InNamespace(b.Namespace.Name)); err != nil {
		return fmt.Errorf("Cannot confirm old-writer shutdown")
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == b.Home.Name {
				return fmt.Errorf("A Pod still references the approved HOME; replacement is blocked")
			}
		}
	}
	return nil
}

func (r *AgentReconciler) suspendCustom(ctx context.Context, instance *api.Agent) (ctrl.Result, error) {
	previous := instance.DeepCopy().Status
	keyErr := r.suspendGatewayKey(ctx, instance)
	stopErr := r.stopCustomWorkload(ctx, instance)
	terminalErr := r.stopCustomTerminal(ctx, instance)
	if err := r.stopCustomCopilot(ctx, instance); err != nil && terminalErr == nil {
		terminalErr = err
	}
	routeErr := r.removeCustomPlatformRoute(ctx, instance)
	if stopErr == nil {
		stopErr = r.removeRegistryCredentials(ctx, instance)
	}
	instance.Status.AccessURL = ""
	instance.Status.Phase = api.PhaseSuspended
	if stopErr != nil {
		instance.Status.Phase = api.PhasePending
	}
	if terminalErr != nil {
		instance.Status.Phase = api.PhasePending
	}
	setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, "InstanceSuspended", "Custom workload requested to scale to zero")
	setCondition(&instance.Status.Conditions, api.ConditionPodReady, metav1.ConditionFalse, "Suspended", "Suspension requested; writer shutdown requires separate observation")
	setCondition(&instance.Status.Conditions, api.ConditionIngressReady, metav1.ConditionFalse, "Suspended", "Platform access removed")
	setCustomServiceConditions(instance, customAccess{}, metav1.ConditionFalse, "Suspended")
	if routeErr != nil {
		setCondition(&instance.Status.Conditions, api.ConditionIngressReady, metav1.ConditionFalse,
			"SuspendIngressRemovalFailed", routeErr.Error())
	}
	if stopErr != nil {
		setCondition(&instance.Status.Conditions, api.ConditionPodReady, metav1.ConditionFalse,
			"CustomSuspensionBlocked", stopErr.Error())
	}
	if err := r.retryCustomCompletion(ctx, instance); err != nil {
		setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse,
			"CustomCompletionPending", "Suspension requested; previous apply completion is still pending")
	}
	if err := r.updateCustomStatus(ctx, instance, previous); err != nil {
		return ctrl.Result{}, err
	}
	if stopErr != nil {
		return ctrl.Result{}, stopErr
	}
	if terminalErr != nil {
		return ctrl.Result{}, terminalErr
	}
	if routeErr != nil {
		return ctrl.Result{}, routeErr
	}
	if keyErr != nil {
		return ctrl.Result{}, keyErr
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *AgentReconciler) removeCustomPlatformRoute(ctx context.Context, instance *api.Agent) error {
	route := &istiov1beta1.VirtualService{}
	key := types.NamespacedName{Namespace: instanceNamespace(instance.Name), Name: virtualServiceName(instance.Name)}
	if err := r.customReader().Get(ctx, key, route); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil
		}
		return client.IgnoreNotFound(err)
	}
	if !hasCustomWorkloadOwner(route, instance) {
		return fmt.Errorf("Custom platform route belongs to another owner")
	}
	return client.IgnoreNotFound(r.Delete(ctx, route, client.Preconditions{UID: &route.UID, ResourceVersion: &route.ResourceVersion}))
}

func (r *AgentReconciler) updateCustomStatus(ctx context.Context, instance *api.Agent, previous api.AgentStatus) error {
	if equality.Semantic.DeepEqual(previous, instance.Status) {
		return nil
	}
	now := metav1.Now()
	instance.Status.LastReconcileTime = &now
	if previous.Phase != instance.Status.Phase {
		reason := "CustomStateChanged"
		switch instance.Status.Phase {
		case api.PhaseProvisioning:
			reason = "InstanceCreating"
		case api.PhaseReady:
			reason = "InstanceReady"
		case api.PhaseSuspended:
			reason = "InstanceSuspended"
		case api.PhaseDeleting:
			reason = "InstanceDeleting"
		}
		r.customEvent(instance, corev1.EventTypeNormal, reason, "Custom instance lifecycle state changed")
	}
	if condition := apimeta.FindStatusCondition(instance.Status.Conditions, api.ConditionAgentConfigReady); condition != nil && condition.Status == metav1.ConditionFalse {
		old := apimeta.FindStatusCondition(previous.Conditions, api.ConditionAgentConfigReady)
		if old == nil || old.Reason != condition.Reason || old.Message != condition.Message {
			r.customEvent(instance, corev1.EventTypeWarning, condition.Reason, condition.Message)
		}
	}
	return r.updateStatus(ctx, instance)
}

func hasAgentOwner(obj client.Object, instance *api.Agent) bool {
	for _, owner := range obj.GetOwnerReferences() {
		if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" &&
			owner.Name == instance.Name && owner.UID == instance.UID {
			return true
		}
	}
	return false
}
