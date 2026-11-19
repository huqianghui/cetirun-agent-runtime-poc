// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/azure"
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const agentIdentityVerificationInterval = 10 * time.Minute

// agentIdentityDirectoryClient is the minimal directory contract needed by
// this reconciler. Keeping it narrow makes the identity lifecycle testable
// without a live Entra credential or Graph endpoint.
type agentIdentityDirectoryClient interface {
	CreateAgentIdentity(context.Context, string, string, string) (*azure.ServicePrincipal, error)
	DeleteServicePrincipal(context.Context, string) error
	GetAgentIdentity(context.Context, string) (*azure.ServicePrincipal, error)
	GetAgentIdentitiesByDisplayName(context.Context, string) ([]azure.ServicePrincipal, error)
}

func agentIdentityRequested(instance *cetirunv1alpha1.Agent) bool {
	return instance.Spec.AgentIdentity != nil && instance.Spec.AgentIdentity.Enabled
}

func agentIdentityVerificationDue(instance *cetirunv1alpha1.Agent, now time.Time) bool {
	status := instance.Status.AgentIdentity
	if !agentIdentityRequested(instance) || status == nil || status.Phase != cetirunv1alpha1.AgentIdentityPhaseReady {
		return agentIdentityRequested(instance)
	}
	return status.LastVerifiedTime == nil || now.Sub(status.LastVerifiedTime.Time) >= agentIdentityVerificationInterval
}

func (r *AgentReconciler) agentIdentityGraphClient() (agentIdentityDirectoryClient, error) {
	if r.agentIdentityGraphClientFactory != nil {
		return r.agentIdentityGraphClientFactory()
	}
	if r.AgentIdentityTenantID == "" || r.AgentIdentityBlueprintClientID == "" {
		return nil, fmt.Errorf("deployment Agent Identity configuration is incomplete")
	}
	tokenFile := os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
	if tokenFile == "" {
		return nil, fmt.Errorf("projected workload identity token is unavailable")
	}
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		TenantID:      r.AgentIdentityTenantID,
		ClientID:      r.AgentIdentityBlueprintClientID,
		TokenFilePath: tokenFile,
	})
	if err != nil {
		return nil, fmt.Errorf("create Blueprint workload credential: %w", err)
	}
	return azure.NewGraphClient(cred), nil
}

func (r *AgentReconciler) agentIdentityDisplayName(instance *cetirunv1alpha1.Agent) string {
	prefix := strings.Trim(r.AgentIdentityNamePrefix, "-")
	if prefix == "" {
		prefix = "cetirun-agent"
	}
	return fmt.Sprintf("%s-%s", prefix, instance.UID)
}

func ensureAgentIdentityStatus(instance *cetirunv1alpha1.Agent) *cetirunv1alpha1.AgentIdentityStatus {
	if instance.Status.AgentIdentity == nil {
		instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{}
	}
	return instance.Status.AgentIdentity
}

func hasCanonicalAgentIdentityBinding(status *cetirunv1alpha1.AgentIdentityStatus) bool {
	return status != nil && canonicalUUID.MatchString(status.ClientID) && canonicalUUID.MatchString(status.ObjectID)
}

func hasIncompleteAgentIdentityBinding(status *cetirunv1alpha1.AgentIdentityStatus) bool {
	if status == nil || hasCanonicalAgentIdentityBinding(status) {
		return false
	}
	if status.CreationAttempted && status.ClientID == "" && status.ObjectID == "" && status.DisplayName != "" {
		return false
	}
	return status.ClientID != "" || status.ObjectID != "" || status.DisplayName != ""
}

func setAgentIdentityState(instance *cetirunv1alpha1.Agent, phase, reason string) *cetirunv1alpha1.AgentIdentityStatus {
	status := ensureAgentIdentityStatus(instance)
	status.Phase = phase
	status.ObservedGeneration = instance.Generation
	status.Reason = reason
	if phase != cetirunv1alpha1.AgentIdentityPhaseReady {
		status.CredentialPolicyRevision = ""
		if instance.Status.Policy != nil {
			setCredentialPolicyCondition(instance, metav1.ConditionFalse, "IdentityPending")
		}
	}
	return status
}

func setAgentIdentityReady(instance *cetirunv1alpha1.Agent, identity *azure.ServicePrincipal, displayName string) {
	status := setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseReady, "")
	status.ClientID = identity.AppID
	status.ObjectID = identity.ID
	status.DisplayName = displayName
	status.CreationAttempted = false
	setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionAgentIdentityReady,
		metav1.ConditionTrue, "AgentIdentityReady", "Agent Identity is ready")
}

func verifyAgentIdentityBinding(status *cetirunv1alpha1.AgentIdentityStatus, identity *azure.ServicePrincipal, blueprintClientID, displayName string) error {
	if identity == nil {
		return fmt.Errorf("bound Agent Identity no longer exists")
	}
	if !strings.EqualFold(identity.ID, status.ObjectID) || !strings.EqualFold(identity.AppID, status.ClientID) {
		return fmt.Errorf("bound Agent Identity does not match the canonical status binding")
	}
	if !strings.EqualFold(identity.AgentIdentityBlueprintID, blueprintClientID) {
		return fmt.Errorf("bound Agent Identity belongs to a different Blueprint")
	}
	if identity.DisplayName != displayName {
		return fmt.Errorf("bound Agent Identity display name does not match the immutable instance key")
	}
	return nil
}

func agentIdentityDisplayNameHasInstanceUID(displayName string, instanceUID string) bool {
	suffix := "-" + instanceUID
	return instanceUID != "" && len(displayName) > len(suffix) && strings.HasSuffix(displayName, suffix)
}

// canonicalAgentIdentityDisplayName returns the recorded immutable name. Older
// status objects did not contain it, so they may be migrated exactly once when
// Graph confirms an exact UID suffix. Later reconciles never recompute a name
// from the mutable deployment prefix.
func canonicalAgentIdentityDisplayName(instance *cetirunv1alpha1.Agent, status *cetirunv1alpha1.AgentIdentityStatus, identity *azure.ServicePrincipal) (string, bool, error) {
	if status.DisplayName != "" {
		if !agentIdentityDisplayNameHasInstanceUID(status.DisplayName, string(instance.UID)) {
			return "", false, fmt.Errorf("recorded Agent Identity display name does not match the immutable instance key")
		}
		return status.DisplayName, false, nil
	}
	if identity == nil || !agentIdentityDisplayNameHasInstanceUID(identity.DisplayName, string(instance.UID)) {
		return "", false, fmt.Errorf("legacy Agent Identity display name does not match the immutable instance key")
	}
	return identity.DisplayName, true, nil
}

func creationAttemptDisplayName(instance *cetirunv1alpha1.Agent, status *cetirunv1alpha1.AgentIdentityStatus) (string, error) {
	if status == nil || !status.CreationAttempted || status.DisplayName == "" || status.ClientID != "" || status.ObjectID != "" || !agentIdentityDisplayNameHasInstanceUID(status.DisplayName, string(instance.UID)) {
		return "", fmt.Errorf("Agent Identity create checkpoint is incomplete or invalid")
	}
	return status.DisplayName, nil
}

// agentIdentityCreateDefinitelyFailed reports Graph responses that prove no
// create operation was accepted. All transport failures, timeouts, conflicts,
// throttling, and 5xx responses remain uncertain and keep the checkpoint.
func agentIdentityCreateDefinitelyFailed(err error) bool {
	if azure.IsRequestNotSent(err) {
		return true
	}
	var graphErr *azure.GraphError
	if !errors.As(err, &graphErr) {
		return false
	}
	return graphErr.StatusCode >= http.StatusBadRequest && graphErr.StatusCode < http.StatusInternalServerError &&
		graphErr.StatusCode != http.StatusRequestTimeout && graphErr.StatusCode != http.StatusConflict && graphErr.StatusCode != http.StatusTooManyRequests
}

// persistDefiniteCreateFailure clears a create checkpoint with a fresh status
// read and conflict retry. The caller has proved that no Graph request was
// accepted, so leaving CreationAttempted set would permanently block a safe
// retry. This deliberately merges only AgentIdentity and its condition; other
// status fields may be updated independently by the reconcile loop.
func (r *AgentReconciler) persistDefiniteCreateFailure(ctx context.Context, instance *cetirunv1alpha1.Agent) error {
	desired := instance.Status.AgentIdentity.DeepCopy()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &cetirunv1alpha1.Agent{}
		if err := r.Get(ctx, types.NamespacedName{Name: instance.Name}, latest); err != nil {
			return err
		}
		before := latest.DeepCopy()
		latest.Status.AgentIdentity = desired.DeepCopy()
		setCondition(&latest.Status.Conditions, cetirunv1alpha1.ConditionAgentIdentityReady,
			metav1.ConditionFalse, "CreateFailed", "Agent Identity create request was not sent")
		if err := r.Status().Patch(ctx, latest, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		instance.Status = latest.Status
		instance.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

// reconcileAgentIdentity creates or adopts exactly one Agent Identity. The CR
// UID is embedded in the display name so a successful Graph create followed by
// a failed status write can be recovered without creating a duplicate.
func (r *AgentReconciler) reconcileAgentIdentity(ctx context.Context, instance *cetirunv1alpha1.Agent) error {
	if !agentIdentityRequested(instance) {
		if instance.Status.AgentIdentity != nil && instance.Status.AgentIdentity.Phase != cetirunv1alpha1.AgentIdentityPhaseDisabled {
			if err := r.deleteAgentIdentity(ctx, instance); err != nil {
				return err
			}
		}
		status := ensureAgentIdentityStatus(instance)
		status.Phase = cetirunv1alpha1.AgentIdentityPhaseDisabled
		status.ObservedGeneration = instance.Generation
		status.Reason = ""
		status.ClientID = ""
		status.ObjectID = ""
		status.DisplayName = ""
		status.CreationAttempted = false
		status.CredentialPolicyRevision = ""
		return nil
	}
	if !r.AgentIdentityEnabled {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseDegraded, "DeploymentDisabled")
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionAgentIdentityReady,
			metav1.ConditionFalse, "DeploymentDisabled", "Agent Identity is disabled for this deployment")
		return nil
	}

	ownerTenantID := instance.Annotations[cetirunv1alpha1.AnnotationOwnerTenantID]
	ownerObjectID := instance.Annotations[cetirunv1alpha1.AnnotationOwnerObjectID]
	if !canonicalUUID.MatchString(ownerTenantID) || !canonicalUUID.MatchString(ownerObjectID) {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "OwnerBindingMissing")
		return fmt.Errorf("trusted Agent Identity owner binding is missing or invalid")
	}
	if !strings.EqualFold(ownerTenantID, r.AgentIdentityTenantID) {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "OwnerTenantMismatch")
		return fmt.Errorf("Agent Identity owner tenant does not match the deployment tenant")
	}

	graph, err := r.agentIdentityGraphClient()
	if err != nil {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "CredentialUnavailable")
		return err
	}

	// A canonical binding is authoritative. Never fall back to display-name
	// adoption once it exists: a stale or replaced principal must not silently
	// select another instance's identity.
	if status := instance.Status.AgentIdentity; hasCanonicalAgentIdentityBinding(status) {
		identity, err := graph.GetAgentIdentity(ctx, status.ObjectID)
		if err != nil {
			setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseDegraded, "DirectoryLookupFailed")
			return err
		}
		displayName, migrated, err := canonicalAgentIdentityDisplayName(instance, status, identity)
		if err != nil {
			setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "BindingMismatch")
			return err
		}
		if err := verifyAgentIdentityBinding(status, identity, r.AgentIdentityBlueprintClientID, displayName); err != nil {
			reason := "BindingNotFound"
			if identity != nil {
				reason = "BindingMismatch"
			}
			setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, reason)
			return err
		}
		setAgentIdentityReady(instance, identity, displayName)
		if migrated {
			if err := r.updateStatus(ctx, instance); err != nil {
				return fmt.Errorf("persist migrated Agent Identity display name: %w", err)
			}
		}
		return nil
	}

	if hasIncompleteAgentIdentityBinding(instance.Status.AgentIdentity) {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "BindingIncomplete")
		return fmt.Errorf("Agent Identity status contains an incomplete canonical binding")
	}

	displayName := r.agentIdentityDisplayName(instance)
	if status := instance.Status.AgentIdentity; status != nil && status.CreationAttempted {
		var recoveryErr error
		displayName, recoveryErr = creationAttemptDisplayName(instance, status)
		if recoveryErr != nil {
			setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "BindingIncomplete")
			return recoveryErr
		}
	}
	matches, err := graph.GetAgentIdentitiesByDisplayName(ctx, displayName)
	if err != nil {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseDegraded, "DirectoryLookupFailed")
		return err
	}
	if len(matches) > 1 {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "AdoptionAmbiguous")
		return fmt.Errorf("multiple Agent Identities match the immutable instance key")
	}
	var identity *azure.ServicePrincipal
	if len(matches) == 1 {
		identity = &matches[0]
		if !strings.EqualFold(identity.AgentIdentityBlueprintID, r.AgentIdentityBlueprintClientID) {
			setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "BlueprintMismatch")
			return fmt.Errorf("existing Agent Identity belongs to a different Blueprint")
		}
		if !canonicalUUID.MatchString(identity.ID) || !canonicalUUID.MatchString(identity.AppID) {
			setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "DirectoryResponseInvalid")
			return fmt.Errorf("existing Agent Identity returned an invalid binding")
		}
		setAgentIdentityReady(instance, identity, displayName)
		if err := r.updateStatus(ctx, instance); err != nil {
			return fmt.Errorf("persist adopted Agent Identity binding: %w", err)
		}
		return nil
	} else {
		status := ensureAgentIdentityStatus(instance)
		if status.CreationAttempted {
			setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseDegraded, "CreateOutcomeUnknown")
			return fmt.Errorf("Agent Identity create outcome is unknown; waiting for directory lookup")
		}

		// Persist the create checkpoint before issuing the non-idempotent Graph
		// POST. A crash or unknown HTTP outcome can then be recovered by display
		// name lookup without issuing a second create.
		status.CreationAttempted = true
		status.DisplayName = displayName
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseProvisioning, "")
		if err := r.updateStatus(ctx, instance); err != nil {
			return fmt.Errorf("persist Agent Identity create checkpoint: %w", err)
		}
		// Status persistence may replace the in-memory status object (notably in the
		// fake client used by unit tests), so reacquire it before changing the
		// persisted checkpoint after a pre-request failure.
		status = ensureAgentIdentityStatus(instance)
		identity, err = graph.CreateAgentIdentity(ctx, displayName, r.AgentIdentityBlueprintClientID, ownerObjectID)
		if err != nil {
			if agentIdentityCreateDefinitelyFailed(err) {
				status.CreationAttempted = false
				status.DisplayName = ""
				setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseDegraded, "CreateFailed")
				if persistErr := r.persistDefiniteCreateFailure(ctx, instance); persistErr != nil {
					return fmt.Errorf("persist definite Agent Identity create failure: %w", persistErr)
				}
			} else {
				setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseDegraded, "CreateOutcomeUnknown")
			}
			return err
		}
	}

	if !canonicalUUID.MatchString(identity.ID) || !canonicalUUID.MatchString(identity.AppID) {
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseFailed, "DirectoryResponseInvalid")
		return fmt.Errorf("created Agent Identity returned an invalid binding")
	}
	setAgentIdentityReady(instance, identity, displayName)
	if err := r.updateStatus(ctx, instance); err != nil {
		// Keep the durable checkpoint rather than an unpersisted binding. The next
		// reconcile will adopt the unique matching directory identity.
		status := ensureAgentIdentityStatus(instance)
		status.ClientID = ""
		status.ObjectID = ""
		status.CreationAttempted = true
		setAgentIdentityState(instance, cetirunv1alpha1.AgentIdentityPhaseDegraded, "BindingPersistenceFailed")
		return fmt.Errorf("persist created Agent Identity binding: %w", err)
	}
	return nil
}

func (r *AgentReconciler) deleteAgentIdentity(ctx context.Context, instance *cetirunv1alpha1.Agent) error {
	if instance.Status.AgentIdentity == nil {
		instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{}
	}
	if instance.Status.AgentIdentity.Phase != cetirunv1alpha1.AgentIdentityPhaseDeleting || instance.Status.AgentIdentity.CredentialPolicyRevision != "" {
		instance.Status.AgentIdentity.Phase = cetirunv1alpha1.AgentIdentityPhaseDeleting
		instance.Status.AgentIdentity.CredentialPolicyRevision = ""
		instance.Status.AgentIdentity.ObservedGeneration = instance.Generation
		if instance.Status.Policy != nil {
			setCredentialPolicyCondition(instance, metav1.ConditionFalse, "CredentialCleanupPending")
		}
		if err := r.updateStatus(ctx, instance); err != nil {
			return fmt.Errorf("persist Agent Identity deletion gate: %w", err)
		}
	}
	// Revoke the Agent's authorizations before deleting the delegated principal.
	// This is a blocking operation: retaining the CR finalizer keeps both
	// operations retryable while the Broker removes Agent-scoped records.
	if err := r.deleteAgentAuthorization(ctx, string(instance.UID)); err != nil {
		return err
	}
	graph, err := r.agentIdentityGraphClient()
	if err != nil {
		return err
	}
	status := instance.Status.AgentIdentity
	if hasCanonicalAgentIdentityBinding(status) {
		identity, err := graph.GetAgentIdentity(ctx, status.ObjectID)
		if err != nil {
			return err
		}
		if identity == nil {
			instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
				Phase:              cetirunv1alpha1.AgentIdentityPhaseDisabled,
				ObservedGeneration: instance.Generation,
			}
			return nil
		}
		displayName, _, err := canonicalAgentIdentityDisplayName(instance, status, identity)
		if err != nil {
			return fmt.Errorf("Agent Identity cleanup canonical display name is invalid: %w", err)
		}
		if err := verifyAgentIdentityBinding(status, identity, r.AgentIdentityBlueprintClientID, displayName); err != nil {
			return fmt.Errorf("Agent Identity cleanup canonical binding is invalid: %w", err)
		}
		if err := graph.DeleteServicePrincipal(ctx, identity.ID); err != nil {
			return err
		}
	} else {
		if hasIncompleteAgentIdentityBinding(status) {
			return fmt.Errorf("Agent Identity cleanup binding is incomplete")
		}
		displayName := r.agentIdentityDisplayName(instance)
		if status != nil && status.CreationAttempted {
			var recoveryErr error
			displayName, recoveryErr = creationAttemptDisplayName(instance, status)
			if recoveryErr != nil {
				return recoveryErr
			}
		}
		// Before a canonical binding exists, UID-derived display-name adoption is
		// only safe when Graph returns exactly one candidate. In particular, never
		// select or delete among multiple matching identities.
		matches, err := graph.GetAgentIdentitiesByDisplayName(ctx, displayName)
		if err != nil {
			return err
		}
		if len(matches) > 1 {
			return fmt.Errorf("Agent Identity cleanup is ambiguous")
		}
		if len(matches) == 0 && status != nil && status.CreationAttempted {
			return fmt.Errorf("Agent Identity cleanup cannot confirm an unknown create outcome")
		}
		for _, identity := range matches {
			if !strings.EqualFold(identity.AgentIdentityBlueprintID, r.AgentIdentityBlueprintClientID) {
				return fmt.Errorf("Agent Identity cleanup found an identity from another Blueprint")
			}
			if identity.DisplayName != displayName || !canonicalUUID.MatchString(identity.AppID) || !canonicalUUID.MatchString(identity.ID) {
				return fmt.Errorf("Agent Identity cleanup found an invalid canonical binding")
			}
			if status != nil && status.ClientID != "" && !strings.EqualFold(identity.AppID, status.ClientID) {
				return fmt.Errorf("Agent Identity cleanup found an unexpected identity")
			}
			if status != nil && status.ObjectID != "" && !strings.EqualFold(identity.ID, status.ObjectID) {
				return fmt.Errorf("Agent Identity cleanup object ID does not match status")
			}
			// Persist the recovered canonical binding before issuing DELETE. If the
			// directory accepts the delete but its response is lost, the next
			// reconcile can use the object-ID path and treat NotFound as completed.
			status.ClientID = identity.AppID
			status.ObjectID = identity.ID
			status.DisplayName = displayName
			status.CreationAttempted = false
			if err := r.updateStatus(ctx, instance); err != nil {
				return fmt.Errorf("persist recovered Agent Identity cleanup binding: %w", err)
			}
			if err := graph.DeleteServicePrincipal(ctx, identity.ID); err != nil {
				return err
			}
		}
	}
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		Phase:              cetirunv1alpha1.AgentIdentityPhaseDisabled,
		ObservedGeneration: instance.Generation,
	}
	return nil
}

func (r *AgentReconciler) deleteAgentAuthorization(ctx context.Context, instanceUID string) error {
	if !canonicalUUID.MatchString(instanceUID) {
		return fmt.Errorf("instance UID is invalid")
	}
	base, err := url.Parse(r.AgentIdentityBrokerURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("Agent Identity Broker URL is invalid")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/operator/v1/instances/" + url.PathEscape(instanceUID) + "/authorization"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base.String(), nil)
	if err != nil {
		return fmt.Errorf("create authorization cleanup request: %w", err)
	}
	client := r.AgentIdentityHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := requestClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete Agent authorization: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete Agent authorization returned HTTP %d", response.StatusCode)
	}
	return nil
}
