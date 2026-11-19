// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const credentialPolicyReady = "CredentialPolicyReady"

func credentialPolicyProjection(agent *api.Agent, team *api.Team, policy *api.AgentPolicy) agenttools.CredentialPolicy {
	projection := agenttools.CredentialPolicy{
		Version: agenttools.CredentialPolicyVersion, AgentUID: string(agent.UID), TeamUID: string(team.UID), PolicyUID: string(policy.UID),
		OwnerTenantID: agent.Annotations[api.AnnotationOwnerTenantID], OwnerObjectID: agent.Annotations[api.AnnotationOwnerObjectID],
		Enabled: agentIdentityRequested(agent),
		Identity: agenttools.CredentialPolicyIdentity{
			Allowed: policy.Spec.AgentIdentity.Allowed, Required: policy.Spec.AgentIdentity.Required,
			CredentialServices: policy.Spec.AgentIdentity.CredentialServices,
		},
	}
	if projection.Enabled && agent.Status.AgentIdentity != nil {
		projection.ClientID = agent.Status.AgentIdentity.ClientID
		projection.PrincipalID = agent.Status.AgentIdentity.ObjectID
	}
	return projection
}

func credentialPolicyVerified(agent *api.Agent, revision string) bool {
	status := agent.Status.AgentIdentity
	if !agentIdentityRequested(agent) {
		return status == nil || (status.Phase == api.AgentIdentityPhaseDisabled &&
			status.ClientID == "" && status.ObjectID == "" && !status.CreationAttempted && status.CredentialPolicyRevision == "")
	}
	if agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() {
		return false
	}
	condition := meta.FindStatusCondition(agent.Status.Conditions, credentialPolicyReady)
	return hasCanonicalAgentIdentityBinding(status) && status.Phase == api.AgentIdentityPhaseReady &&
		status.ObservedGeneration == agent.Generation && agenttools.ValidCredentialPolicyRevision(revision) &&
		status.CredentialPolicyRevision == revision && condition != nil && condition.Status == metav1.ConditionTrue &&
		condition.ObservedGeneration == agent.Generation &&
		meta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionAgentIdentityReady)
}

func setCredentialPolicyCondition(agent *api.Agent, status metav1.ConditionStatus, reason string) {
	meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: credentialPolicyReady, Status: status, Reason: reason, ObservedGeneration: agent.Generation,
		Message: "Credential service enforcement requires a current, owned Broker readback",
	})
}

// reconcileCredentialPolicy applies only the Broker authorization projection.
// It cannot provision an identity, approve a native runtime, or start a Pod.
func (r *AgentReconciler) reconcileCredentialPolicy(ctx context.Context, agent *api.Agent, resolution *policyResolution) error {
	before := agent.Status.DeepCopy()
	status := agent.Status.AgentIdentity
	finish := func(err error) error {
		if !reflect.DeepEqual(*before, agent.Status) {
			if statusErr := r.updateStatus(ctx, agent); statusErr != nil {
				if agent.Status.AgentIdentity != nil {
					agent.Status.AgentIdentity.CredentialPolicyRevision = ""
				}
				setCredentialPolicyCondition(agent, metav1.ConditionFalse, "CredentialStatusUnconfirmed")
				err = errors.Join(err, statusErr)
			}
		}
		return err
	}
	clear := func(reason string) {
		if status != nil {
			status.CredentialPolicyRevision = ""
		}
		setCredentialPolicyCondition(agent, metav1.ConditionFalse, reason)
	}
	if !agentIdentityRequested(agent) {
		if credentialPolicyVerified(agent, "") {
			meta.RemoveStatusCondition(&agent.Status.Conditions, credentialPolicyReady)
		} else {
			clear("CredentialCleanupPending")
		}
		return finish(nil)
	}
	if !r.AgentIdentityEnabled {
		clear("AgentIdentityDisabled")
		return finish(nil)
	}
	projection := credentialPolicyProjection(agent, resolution.team, resolution.policy)
	if !projection.ValidApplied() || status == nil || status.Phase != api.AgentIdentityPhaseReady ||
		status.ObservedGeneration != agent.Generation ||
		!meta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionAgentIdentityReady) {
		clear("IdentityPending")
		return finish(nil)
	}
	if agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() {
		clear("AgentInactive")
		return finish(nil)
	}
	pending, pendingErr := r.nativeConnectionsPodPending(ctx, agent)
	if pendingErr != nil {
		clear("NativeProjectionUnavailable")
		return finish(pendingErr)
	}
	if pending {
		clear("NativeProjectionPending")
		return finish(nil)
	}
	// Read first: a previous PUT may have succeeded even if its reply was lost.
	readback, code, err := r.credentialPolicyRequest(ctx, agent, http.MethodGet, nil)
	if err == nil && (code == http.StatusNotFound || code == http.StatusConflict) {
		_, code, err = r.credentialPolicyRequest(ctx, agent, http.MethodPut, &projection)
		if err == nil && code == http.StatusNoContent {
			readback, code, err = r.credentialPolicyRequest(ctx, agent, http.MethodGet, nil)
		}
	}
	if err != nil || code != http.StatusOK || !readback.Matches(projection) || readback.Revision != resolution.credentialRevision {
		clear("CredentialReadbackUnavailable")
		if r.nativeConnectionsRequested(agent) {
			// Keep evidence absent while common reconciliation repairs or
			// publishes its current projection. Active old workloads are fenced
			// by policyReprojectionMustHold; this is not applied readiness.
			return finish(nil)
		}
		return finish(fmt.Errorf("credential policy readback is unavailable or changed (HTTP %d)", code))
	}
	status.CredentialPolicyRevision = readback.Revision
	setCredentialPolicyCondition(agent, metav1.ConditionTrue, "CredentialEnforcementVerified")
	return finish(nil)
}

func (r *AgentReconciler) credentialPolicyRequest(ctx context.Context, agent *api.Agent, method string, policy *agenttools.CredentialPolicy) (agenttools.CredentialPolicyReadback, int, error) {
	var result agenttools.CredentialPolicyReadback
	base, err := url.Parse(r.AgentIdentityBrokerURL)
	if err != nil || !canonicalUUID.MatchString(string(agent.UID)) || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.ForceQuery {
		return result, 0, fmt.Errorf("credential policy Broker endpoint is invalid")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/operator/v1/instances/" + url.PathEscape(string(agent.UID)) + "/credential-policy"
	var body io.Reader
	if policy != nil {
		data, err := json.Marshal(policy)
		if err != nil {
			return result, 0, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, base.String(), body)
	if err != nil {
		return result, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := r.AgentIdentityHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(request)
	if err != nil {
		return result, 0, fmt.Errorf("credential policy Broker request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil || len(data) > 16<<10 {
		return result, response.StatusCode, fmt.Errorf("credential policy Broker response is unavailable")
	}
	if response.StatusCode == http.StatusOK && method == http.MethodGet {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
			return result, response.StatusCode, fmt.Errorf("credential policy Broker readback is invalid")
		}
	}
	return result, response.StatusCode, nil
}
