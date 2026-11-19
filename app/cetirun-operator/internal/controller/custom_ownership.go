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
	"reflect"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const customOwnershipFinalizer = "cetirun.microsoft.com/custom-runtime-ownership"

// Kept in the operator namespace, never in tenant-writable Agent annotations.
// This is an ownership receipt, not a claim that Helm or a Pod is ready.
type customOwnership struct {
	AgentName    string                     `json:"agentName"`
	AgentUID     types.UID                  `json:"agentUID"`
	Namespace    string                     `json:"namespace"`
	NamespaceUID types.UID                  `json:"namespaceUID"`
	Release      string                     `json:"release"`
	Home         string                     `json:"home"`
	HomeClaim    *runtimebindings.HomeClaim `json:"homeClaim"`
}

func (r *AgentReconciler) ownershipKey(instance *api.Agent) types.NamespacedName {
	return r.customRecordKey(instance, "ownership")
}

func (r *AgentReconciler) customRecordKey(instance *api.Agent, purpose string) types.NamespacedName {
	namespace := r.OperatorNamespace
	if namespace == "" {
		namespace = "cetirun-system"
	}
	if purpose == "ownership" {
		return types.NamespacedName{Namespace: namespace, Name: agenttools.CustomOwnershipRecordName(string(instance.UID))}
	}
	// Other records keep their established names; only the ownership receipt is
	// shared with the Broker.
	hash := sha256.Sum256([]byte(instance.UID))
	return types.NamespacedName{Namespace: namespace, Name: "custom-" + purpose + "-" + hex.EncodeToString(hash[:16])}
}

func (r *AgentReconciler) readCustomOwnership(ctx context.Context, instance *api.Agent) (*customOwnership, error) {
	record := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, r.ownershipKey(instance), record); err != nil {
		return nil, err
	}
	var receipt customOwnership
	if !hasCustomRecordOwner(record, instance) || record.Immutable == nil || !*record.Immutable ||
		json.Unmarshal([]byte(record.Data["ownership"]), &receipt) != nil ||
		receipt.AgentUID != instance.UID || receipt.AgentName != instance.Name ||
		receipt.Namespace != instanceNamespace(instance.Name) || receipt.Release != helmReleaseName(instance.Name) ||
		receipt.NamespaceUID == "" || receipt.Home == "" || receipt.HomeClaim == nil {
		return nil, fmt.Errorf("Custom ownership record is invalid")
	}
	if err := receipt.HomeClaim.Validate(); err != nil {
		return nil, fmt.Errorf("Custom ownership storage is invalid: %w", err)
	}
	return &receipt, nil
}

func (r *AgentReconciler) ensureCustomOwnership(ctx context.Context, instance *api.Agent, bindings runtimebindings.Custom, current *appsv1.StatefulSet) error {
	if err := r.checkAgentGeneration(ctx, instance); err != nil {
		return err
	}
	want := customOwnership{AgentName: instance.Name, AgentUID: instance.UID,
		Namespace: bindings.Namespace.Name, NamespaceUID: types.UID(bindings.Namespace.UID),
		Release: helmReleaseName(instance.Name), Home: bindings.Home.Name, HomeClaim: bindings.Home.Claim}
	if want.Namespace == r.ownershipKey(instance).Namespace {
		return fmt.Errorf("Custom runtime cannot use the operator namespace")
	}
	got, err := r.readCustomOwnership(ctx, instance)
	if apierrors.IsNotFound(err) {
		if current != nil || controllerutil.ContainsFinalizer(instance, customOwnershipFinalizer) {
			return fmt.Errorf("Custom ownership record is missing; existing resources cannot be adopted")
		}
		raw, err := json.Marshal(want)
		if err != nil {
			return err
		}
		key := r.ownershipKey(instance)
		record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace,
			Finalizers: []string{customOwnershipFinalizer},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent",
				Name: instance.Name, UID: instance.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}},
			Immutable: ptr.To(true), Data: map[string]string{"ownership": string(raw)}}
		if err := r.Create(ctx, record); err != nil {
			return fmt.Errorf("cannot persist Custom ownership record")
		}
	} else if err != nil {
		return err
	} else if !reflect.DeepEqual(*got, want) {
		return fmt.Errorf("Custom namespace or HOME ownership changed")
	}
	if !controllerutil.ContainsFinalizer(instance, customOwnershipFinalizer) {
		before := instance.DeepCopy()
		controllerutil.AddFinalizer(instance, customOwnershipFinalizer)
		if err := r.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("cannot persist Custom ownership finalizer: %w", err)
		}
	}
	return nil
}

func (r *AgentReconciler) deleteCustom(ctx context.Context, instance *api.Agent) (ctrl.Result, error) {
	revokeErr := r.revokeSandboxPreparationGrants(ctx, instance)
	r.customPlans.Delete(instance.Name)
	previous := instance.DeepCopy().Status
	stopErr := r.stopCustomWorkload(ctx, instance)
	terminalErr := r.stopCustomTerminal(ctx, instance)
	if err := r.stopCustomCopilot(ctx, instance); err != nil && terminalErr == nil {
		terminalErr = err
	}
	routeErr := r.removeCustomPlatformRoute(ctx, instance)
	instance.Status.Phase = api.PhaseDeleting
	instance.Status.AccessURL = ""
	setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, "InstanceDeleting", "Deletion requested")
	setCondition(&instance.Status.Conditions, api.ConditionPodReady, metav1.ConditionFalse, "InstanceDeleting", "Workload stop requested")
	setCondition(&instance.Status.Conditions, api.ConditionIngressReady, metav1.ConditionFalse, "InstanceDeleting", "Access removal requested")
	setCustomServiceConditions(instance, customAccess{}, metav1.ConditionFalse, "InstanceDeleting")
	if stopErr != nil {
		setCondition(&instance.Status.Conditions, api.ConditionPodReady, metav1.ConditionFalse, "CustomStopBlocked", stopErr.Error())
	}
	if routeErr != nil {
		setCondition(&instance.Status.Conditions, api.ConditionIngressReady, metav1.ConditionFalse, "CustomRouteRemovalBlocked", routeErr.Error())
	}
	statusErr := r.updateCustomStatus(ctx, instance, previous)
	if err := errors.Join(revokeErr, stopErr, terminalErr, routeErr, statusErr); err != nil {
		return ctrl.Result{}, err
	}
	if agentIdentityRequested(instance) || (instance.Status.AgentIdentity != nil && instance.Status.AgentIdentity.Phase != api.AgentIdentityPhaseDisabled) {
		if err := r.deleteAgentIdentity(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	if instance.Status.AIGateway != nil || r.customGatewaySelected(instance) {
		if err := r.deleteAIGatewayKey(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.cleanupRegistryCredentials(ctx, instance, true); err != nil {
		return r.registryWaiting(ctx, instance, "RegistryCleanupPending")
	}
	if err := r.checkSandboxPreparationCleanup(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.retryCustomCompletion(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	receipt, err := r.readCustomOwnership(ctx, instance)
	if apierrors.IsNotFound(err) && !controllerutil.ContainsFinalizer(instance, customOwnershipFinalizer) {
		// Before this receipt/finalizer pair exists, Custom cannot apply a
		// workload. Preserve any unrecorded or foreign resources.
		sts := &appsv1.StatefulSet{}
		key := types.NamespacedName{Namespace: instanceNamespace(instance.Name), Name: instanceServiceName(instance.Name)}
		if err := r.customReader().Get(ctx, key, sts); err == nil && hasAgentOwner(sts, instance) {
			return ctrl.Result{}, fmt.Errorf("Custom workload has no ownership receipt; cleanup requires recovery")
		} else if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.finishCustomDeletion(ctx, instance)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			ns := &corev1.Namespace{}
			if err := r.customReader().Get(ctx, types.NamespacedName{Name: instanceNamespace(instance.Name)}, ns); apierrors.IsNotFound(err) {
				return r.finishCustomDeletion(ctx, instance)
			}
		}
		return ctrl.Result{}, fmt.Errorf("Custom cleanup requires a valid ownership record")
	}
	ns := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, types.NamespacedName{Name: receipt.Namespace}, ns); apierrors.IsNotFound(err) {
		return r.finishCustomDeletion(ctx, instance)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if ns.UID != receipt.NamespaceUID {
		return ctrl.Result{}, fmt.Errorf("Custom cleanup namespace UID mismatch")
	}
	configured, configErr := r.connectionsMCPBrokerStorageEnabled()
	if configErr != nil {
		return ctrl.Result{}, configErr
	}
	if configured {
		// Custom does not use Agent Identity to establish this grant. The
		// immutable ownership receipt and the namespace UID fence are checked
		// before the Broker can remove its Agent-scoped records.
		if err := r.ensureConnectionsMCPBrokerCleanupRBAC(ctx, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("prepare Broker cleanup for Custom Agent: %w", err)
		}
		if !agentIdentityRequested(instance) && (instance.Status.AgentIdentity == nil || instance.Status.AgentIdentity.Phase == api.AgentIdentityPhaseDisabled) {
			if err := r.deleteAgentAuthorization(ctx, string(instance.UID)); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete Custom Agent Broker records: %w", err)
			}
		}
	}
	if ns.DeletionTimestamp.IsZero() {
		// Deleting this verified dedicated namespace also removes its Helm
		// records/resources, without executing uninstall hooks from a chart.
		if err := r.Delete(ctx, ns, client.Preconditions{UID: &receipt.NamespaceUID}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *AgentReconciler) finishCustomDeletion(ctx context.Context, instance *api.Agent) (ctrl.Result, error) {
	if err := r.finishSandboxPreparation(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.finishSandboxBootstrap(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	record := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, r.ownershipKey(instance), record); err == nil {
		if !hasCustomRecordOwner(record, instance) {
			return ctrl.Result{}, fmt.Errorf("Custom ownership record owner changed")
		}
		// Namespace cleanup has finished. Allow foreground Agent GC to remove
		// the receipt, but keep the Agent finalizer if this write fails.
		controllerutil.RemoveFinalizer(record, customOwnershipFinalizer)
		if err := r.Update(ctx, record); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	base := instance.DeepCopy()
	controllerutil.RemoveFinalizer(instance, api.Finalizer)
	controllerutil.RemoveFinalizer(instance, customOwnershipFinalizer)
	controllerutil.RemoveFinalizer(instance, sandboxHoldFinalizer)
	if err := r.Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	// Agent GC removes the operator-owned receipt only after finalization.
	return ctrl.Result{}, nil
}
