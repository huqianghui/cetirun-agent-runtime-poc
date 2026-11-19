// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrInstanceVersionRequired  = errors.New("uid and resourceVersion from the current Agent are required")
	ErrSuspensionUnsupported    = errors.New("retained single-start Agents do not support owner suspend/resume")
	ErrSuspensionHomeRequired   = errors.New("retained suspension requires persistent HOME storage; ephemeral HOME would be lost")
	ErrSuspensionHomeUnverified = errors.New("retained suspension is unavailable until the current HOME storage can be verified")
)

// CheckSuspensionHome reads the effective workload, including chart and
// deployment defaults that are not present in the Agent's Helm overrides.
func (c *Client) CheckSuspensionHome(ctx context.Context, instance *api.Agent) error {
	workload := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: InstanceNamespace(instance.Name), Name: chataccess.ServiceName(instance.Name)}
	if err := c.client.Get(ctx, key, workload); err != nil {
		return fmt.Errorf("%w: %w", ErrSuspensionHomeUnverified, err)
	}
	if workload.UID == "" || !workload.DeletionTimestamp.IsZero() ||
		(workload.Annotations[chataccess.AgentUIDAnnotation] != "" &&
			workload.Annotations[chataccess.AgentUIDAnnotation] != string(instance.UID)) {
		return ErrSuspensionHomeUnverified
	}
	if owner := metav1.GetControllerOf(workload); owner != nil {
		if len(workload.OwnerReferences) != 1 || owner.APIVersion != api.GroupVersion.String() || owner.Kind != "Agent" ||
			owner.Name != instance.Name || owner.UID != instance.UID {
			return ErrSuspensionHomeUnverified
		}
	} else if instance.Status.Namespace != key.Namespace || instance.Status.HelmReleaseName != "openclaw-"+instance.Name ||
		!chatReleaseOwned(workload, instance) {
		return ErrSuspensionHomeUnverified
	}
	containers := workload.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return ErrSuspensionHomeUnverified
	}
	home := "/home/" + *agentconfig.Defaults(instance.Spec.Agent).Username
	volumeName := ""
	for _, mount := range containers[0].VolumeMounts {
		if mount.MountPath == home {
			volumeName = mount.Name
		}
	}
	if volumeName == "" {
		return ErrSuspensionHomeUnverified
	}
	for _, claim := range workload.Spec.VolumeClaimTemplates {
		if claim.Name == volumeName {
			return nil
		}
	}
	for _, volume := range workload.Spec.Template.Spec.Volumes {
		if volume.Name == volumeName && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			return nil
		}
	}
	return ErrSuspensionHomeRequired
}

// CheckSuspension verifies either the current persistent HOME, or the narrow
// first-start case where no HOME can exist yet. The latter is deliberately
// conservative: any durable bootstrap, preparation, grant, namespace or
// lifecycle evidence falls back to the normal retained-HOME requirement.
func (c *Client) CheckSuspension(ctx context.Context, instance *api.Agent) error {
	err := c.CheckSuspensionHome(ctx, instance)
	if !errors.Is(err, ErrSuspensionHomeUnverified) {
		return err
	}
	eligible, proofErr := c.preBootstrapSuspensionEligible(ctx, instance)
	if proofErr != nil || !eligible {
		return err
	}
	return nil
}

func (c *Client) preBootstrapSuspensionEligible(ctx context.Context, instance *api.Agent) (bool, error) {
	// This exception applies only to a governed Agent that has not started an
	// execution lifetime. Policy diagnostics are intentionally not inspected:
	// a failed policy prerequisite is the useful diagnosis the owner retains.
	if instance.Spec.SandboxRef == "" || instance.Spec.RestartRequestID != "" || instance.Status.ObservedRestartRequestID != "" ||
		instance.Spec.PlannedMigration != nil || instance.Status.PlannedMigration != nil ||
		slices.Contains(instance.Finalizers, api.SandboxPreparationFinalizer) ||
		slices.Contains(instance.Finalizers, api.SandboxBootstrapFinalizer) {
		return false, nil
	}
	if preBootstrapSuspensionHasHistory(instance) {
		return false, nil
	}
	namespace := &corev1.Namespace{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: InstanceNamespace(instance.Name)}, namespace); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	for _, key := range []client.ObjectKey{
		{Namespace: c.sandboxStateNamespace, Name: sandboxBootstrapReceiptName(instance.UID)},
		{Namespace: c.sandboxStateNamespace, Name: sandboxPreparationReceiptName(instance.UID)},
		{Namespace: c.sandboxStateNamespace, Name: sandboxGrantJournalName(instance.UID)},
	} {
		state := &corev1.ConfigMap{}
		if err := c.client.Get(ctx, key, state); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

// preBootstrapSuspensionHasHistory rejects status that proves an execution
// lifetime already progressed. Policy is intentionally excluded: a failed
// prerequisite is the diagnostic that this narrow pre-bootstrap pause keeps.
func preBootstrapSuspensionHasHistory(instance *api.Agent) bool {
	if instance.Status.Namespace != "" || instance.Status.HelmReleaseName != "" ||
		instance.Status.AIGateway != nil || instance.Status.Sandbox != nil ||
		instance.Status.HelmMaintenance != nil || instance.Status.RegistryCredentials != nil ||
		instance.Status.Phase == api.PhaseReady {
		return true
	}
	for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady, api.ConditionIngressReady, api.ConditionHelmDeployed} {
		if apimeta.IsStatusConditionTrue(instance.Status.Conditions, condition) {
			return true
		}
	}
	return false
}

func sandboxBootstrapReceiptName(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return "sandbox-bootstrap-" + hex.EncodeToString(sum[:20])
}

func sandboxPreparationReceiptName(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return "sandbox-preparation-" + hex.EncodeToString(sum[:20])
}

func sandboxGrantJournalName(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return "sandbox-grants-" + hex.EncodeToString(sum[:24])
}

func canChangeSuspension(instance *api.Agent) bool {
	return instance.UID != "" && instance.ResourceVersion != "" && instance.DeletionTimestamp.IsZero() &&
		!slices.Contains(instance.Finalizers, api.SandboxPreparationFinalizer)
}

func CanSuspendInstance(instance *api.Agent) bool {
	return canChangeSuspension(instance) && !instance.Spec.Suspended
}

func CanResumeInstance(instance *api.Agent) bool {
	return canChangeSuspension(instance) && instance.Spec.Suspended
}

func (c *Client) SetInstanceSuspendedByName(ctx context.Context, name string, version InstanceVersion,
	suspended bool, authorize func(*api.Agent) error,
) (*api.Agent, error) {
	if version.UID == "" || version.ResourceVersion == "" {
		return nil, ErrInstanceVersionRequired
	}
	if version.Generation < 0 {
		return nil, ErrInstanceVersionRequired
	}
	// A status write changes resourceVersion without changing the owner-visible
	// configuration. New clients send generation so that a held Agent remains
	// suspendable while the controller reports progress. There is deliberately
	// only one fresh read after a conditional-write conflict: it rechecks the
	// owner, lifecycle guards, and the complete specification without masking
	// sustained contention.
	for attempt := 0; attempt < 2; attempt++ {
		instance, err := c.GetInstanceByName(ctx, name)
		if err != nil {
			return nil, err
		}
		if authorize != nil {
			if err := authorize(instance); err != nil {
				return nil, err
			}
		}
		if err := validateInstanceUpdate(instance, UpdateInstanceOpts{ExpectedUID: version.UID}); err != nil {
			return nil, err
		}
		if slices.Contains(instance.Finalizers, api.SandboxPreparationFinalizer) {
			return nil, ErrSuspensionUnsupported
		}
		// A retry may observe the requested intent after a lost response or a
		// status write. Identity, ownership, and deletion are checked above,
		// so returning this existing intent cannot reverse a later operation.
		if instance.Spec.Suspended == suspended {
			return instance, nil
		}
		if version.Generation > 0 {
			if instance.Generation != version.Generation {
				return nil, ErrInstanceChanged
			}
		} else if instance.ResourceVersion != version.ResourceVersion {
			return nil, ErrInstanceChanged
		}
		if suspended {
			if err := c.CheckSuspension(ctx, instance); err != nil {
				return nil, err
			}
		}
		before := instance.DeepCopy()
		// Keep pending restart/migration requests: suspension takes precedence,
		// and resumption lets the existing controller continue those operations.
		instance.Spec.Suspended = suspended
		err = c.client.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
			client.FieldValidation(metav1.FieldValidationStrict))
		if err == nil {
			return instance, nil
		}
		// Older clients did not supply a generation and retain the original
		// resourceVersion-only contract, including surfacing the first
		// optimistic-lock conflict unchanged.
		if version.Generation == 0 || !apierrors.IsConflict(err) || attempt == 1 {
			return nil, fmt.Errorf("request Agent suspension change: %w", err)
		}
	}
	return nil, ErrInstanceChanged
}
