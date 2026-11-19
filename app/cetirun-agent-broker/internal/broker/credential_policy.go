// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const credentialPolicyRecord = "credential-policy"

var credentialConfigurationName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func validCredentialConfigurationName(name string) bool {
	return len(name) <= 253 && credentialConfigurationName.MatchString(name)
}

type credentialObjectMeta struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid"`
	Generation        int64             `json:"generation"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	Annotations       map[string]string `json:"annotations"`
}

type credentialCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

type credentialAgentDocument struct {
	Metadata credentialObjectMeta `json:"metadata"`
	Spec     struct {
		Team          string `json:"team"`
		Suspended     bool   `json:"suspended"`
		AgentIdentity *struct {
			Enabled bool `json:"enabled"`
		} `json:"agentIdentity"`
	} `json:"spec"`
	Status struct {
		Namespace     string `json:"namespace"`
		AgentIdentity *struct {
			Phase              string `json:"phase"`
			ClientID           string `json:"clientId"`
			ObjectID           string `json:"objectId"`
			ObservedGeneration int64  `json:"observedGeneration"`
		} `json:"agentIdentity"`
	} `json:"status"`
}

func credentialConditionCurrent(conditions []credentialCondition, kind string, generation int64) bool {
	for _, condition := range conditions {
		if condition.Type == kind {
			return generation > 0 && condition.ObservedGeneration == generation && condition.Status == "True"
		}
	}
	return false
}

// currentCredentialPolicy independently resolves the authoritative assignment.
// Neither the operator's desired revision nor a previously reported applied
// revision is an authorization input to the Broker.
func (s *Server) currentCredentialPolicy(ctx context.Context, agent Agent) (agenttools.CredentialPolicy, error) {
	fail := func(code string, status int) (agenttools.CredentialPolicy, error) {
		return agenttools.CredentialPolicy{}, toolAuthorizationFailure(code, status)
	}
	if !agent.PolicyManaged {
		return fail("permission_denied", 403)
	}
	var source credentialAgentDocument
	if _, err := s.store.request(ctx, http.MethodGet, "/apis/cetirun.microsoft.com/v1alpha1/agents/"+url.PathEscape(agent.Name), nil, &source); err != nil {
		return fail("dependency_unavailable", 503)
	}
	if source.Metadata.Name != agent.Name || source.Metadata.UID != agent.UID ||
		source.Status.Namespace != agent.Namespace || source.Metadata.DeletionTimestamp != nil ||
		source.Spec.Suspended || source.Spec.AgentIdentity == nil || !source.Spec.AgentIdentity.Enabled {
		return fail("permission_denied", 403)
	}
	status := source.Status.AgentIdentity
	if status == nil || status.Phase != "Ready" || source.Metadata.Generation <= 0 ||
		status.ObservedGeneration != source.Metadata.Generation {
		return fail("authorization_changed", 409)
	}
	owner := Owner{
		source.Metadata.Annotations["agentidentity.cetirun.microsoft.com/owner-tenant-id"],
		source.Metadata.Annotations["agentidentity.cetirun.microsoft.com/owner-object-id"],
	}
	if !sameOwner(owner, Owner{agent.OwnerTenantID, agent.OwnerObjectID}) ||
		!sameIdentityID(owner.TenantID, s.cfg.TenantID) ||
		!sameIdentityID(status.ClientID, agent.AgentIdentityClientID) ||
		!sameIdentityID(status.ObjectID, agent.AgentIdentityObjectID) {
		return fail("authorization_changed", 409)
	}
	if !validCredentialConfigurationName(source.Spec.Team) {
		return fail("permission_denied", 403)
	}
	var team struct {
		Metadata credentialObjectMeta `json:"metadata"`
		Spec     struct {
			AgentPolicyRef string `json:"agentPolicyRef"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64                 `json:"observedGeneration"`
			Conditions         []credentialCondition `json:"conditions"`
		} `json:"status"`
	}
	if _, err := s.store.request(ctx, http.MethodGet, "/apis/cetirun.microsoft.com/v1alpha1/teams/"+url.PathEscape(source.Spec.Team), nil, &team); err != nil {
		return fail("dependency_unavailable", 503)
	}
	if team.Metadata.Name != source.Spec.Team || team.Metadata.UID == "" || team.Metadata.DeletionTimestamp != nil ||
		team.Status.ObservedGeneration != team.Metadata.Generation ||
		!credentialConditionCurrent(team.Status.Conditions, "Accepted", team.Metadata.Generation) ||
		!validCredentialConfigurationName(team.Spec.AgentPolicyRef) {
		return fail("authorization_changed", 409)
	}
	var policy struct {
		Metadata credentialObjectMeta `json:"metadata"`
		Spec     struct {
			AgentIdentity agenttools.CredentialPolicyIdentity `json:"agentIdentity"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64                 `json:"observedGeneration"`
			Conditions         []credentialCondition `json:"conditions"`
		} `json:"status"`
	}
	if _, err := s.store.request(ctx, http.MethodGet, "/apis/cetirun.microsoft.com/v1alpha1/agentpolicies/"+url.PathEscape(team.Spec.AgentPolicyRef), nil, &policy); err != nil {
		return fail("dependency_unavailable", 503)
	}
	if policy.Metadata.Name != team.Spec.AgentPolicyRef || policy.Metadata.UID == "" || policy.Metadata.DeletionTimestamp != nil ||
		policy.Status.ObservedGeneration != policy.Metadata.Generation ||
		!credentialConditionCurrent(policy.Status.Conditions, "Accepted", policy.Metadata.Generation) {
		return fail("authorization_changed", 409)
	}
	projection := agenttools.CredentialPolicy{
		Version: agenttools.CredentialPolicyVersion, AgentUID: agent.UID, TeamUID: team.Metadata.UID, PolicyUID: policy.Metadata.UID,
		OwnerTenantID: owner.TenantID, OwnerObjectID: owner.ObjectID, ClientID: status.ClientID, PrincipalID: status.ObjectID,
		Enabled: true, Identity: policy.Spec.AgentIdentity,
	}
	if !projection.ValidApplied() {
		return fail("permission_denied", 403)
	}
	armAvailable := s.cfg.connectionModeEnabled(Connection{Provider: "azure", Mode: modeAgent}) ||
		s.cfg.connectionModeEnabled(Connection{Provider: "azure", Mode: modeDelegated})
	if !armAvailable ||
		(agent.RuntimeType != "hermes" && agent.RuntimeType != "openclaw" && !(agent.RuntimeType == "custom" && agent.PolicyManaged)) ||
		((s.cfg.ToolRuntimeTypes != nil || agent.RuntimeType == "custom") && !contains(s.cfg.ToolRuntimeTypes, agent.RuntimeType)) {
		return fail("dependency_unavailable", 503)
	}
	return projection, nil
}

// Policy records contain no credential and use the Broker's existing record
// namespace. They must not require Secret access in an otherwise unsupported
// runtime namespace, especially during identity-only cleanup.
func (k *kubeClient) credentialPolicyStore(agent Agent) (*kubeClient, error) {
	if !dnsLabel(k.namespace) || !dnsLabel(agent.Name) || !uuidPattern.MatchString(agent.UID) {
		return nil, errors.New("credential policy record identity is invalid")
	}
	copy := *k
	copy.agentNamespaces, copy.recordAgent = false, &agent
	return &copy, nil
}

func (k *kubeClient) credentialRecordOwned(secret secretDocument, agent Agent) bool {
	l := secret.Metadata.Labels
	return secret.APIVersion == "v1" && secret.Kind == "Secret" &&
		secret.Metadata.Name == toolRecordName(credentialPolicyRecord, agent.UID) &&
		secret.Metadata.Namespace == k.namespace && secret.Metadata.UID != "" && secret.Metadata.ResourceVersion != "" &&
		secret.Metadata.DeletionTimestamp == nil && recordOwnedBy(secret, agent) &&
		l["app.kubernetes.io/managed-by"] == "cetirun-agent-broker" &&
		l[recordLabelPrefix+"record-type"] == credentialPolicyRecord && l[recordLabelPrefix+"instance-uid"] == agent.UID
}

func (k *kubeClient) credentialRecordMetadataValid(secret secretDocument, agent Agent) bool {
	l := secret.Metadata.Labels
	return k.credentialRecordOwned(secret, agent) &&
		sameOwner(Owner{l[recordLabelPrefix+"owner-tenant"], l[recordLabelPrefix+"owner-object"]}, Owner{agent.OwnerTenantID, agent.OwnerObjectID})
}

func (k *kubeClient) readCredentialPolicy(ctx context.Context, agent Agent) (agenttools.CredentialPolicyReadback, error) {
	secret, err := k.getToolSecret(ctx, credentialPolicyRecord, agent.UID)
	if err != nil {
		return agenttools.CredentialPolicyReadback{}, err
	}
	if !k.credentialRecordMetadataValid(secret, agent) || len(secret.Data) != 1 {
		return agenttools.CredentialPolicyReadback{}, errors.New("credential policy record ownership is invalid")
	}
	raw, err := base64.StdEncoding.DecodeString(secret.Data["policy.json"])
	if err != nil || len(raw) > 16<<10 {
		return agenttools.CredentialPolicyReadback{}, errors.New("credential policy record encoding is invalid")
	}
	var policy agenttools.CredentialPolicy
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&policy) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) ||
		!policy.ValidApplied() || policy.AgentUID != agent.UID ||
		!sameOwner(Owner{policy.OwnerTenantID, policy.OwnerObjectID}, Owner{agent.OwnerTenantID, agent.OwnerObjectID}) {
		return agenttools.CredentialPolicyReadback{}, errors.New("credential policy record is invalid")
	}
	return agenttools.CredentialPolicyReadback{
		Policy: policy, Revision: policy.Revision(), RecordUID: secret.Metadata.UID, ResourceVersion: secret.Metadata.ResourceVersion,
	}, nil
}

func (s *Server) credentialPolicyReadback(ctx context.Context, agent Agent) (agenttools.CredentialPolicyReadback, error) {
	current, err := s.currentCredentialPolicy(ctx, agent)
	if err != nil {
		return agenttools.CredentialPolicyReadback{}, err
	}
	target, err := s.store.credentialPolicyStore(agent)
	if err != nil || target.recordNamespaceDeleting {
		return agenttools.CredentialPolicyReadback{}, toolAuthorizationFailure("dependency_unavailable", 503)
	}
	readback, err := target.readCredentialPolicy(ctx, agent)
	if errors.Is(err, errNotFound) {
		return readback, toolAuthorizationFailure("credential_policy_missing", 404)
	}
	if err != nil {
		return readback, toolAuthorizationFailure("dependency_unavailable", 503)
	}
	if !readback.Matches(current) {
		return readback, toolAuthorizationFailure("authorization_changed", 409)
	}
	latest, err := s.currentCredentialPolicy(ctx, agent)
	if err != nil {
		return agenttools.CredentialPolicyReadback{}, err
	}
	if !readback.Matches(latest) {
		return agenttools.CredentialPolicyReadback{}, toolAuthorizationFailure("authorization_changed", 409)
	}
	// Installing a restriction is not proof that the runtime can use this
	// executor. Native Sandbox remains unavailable until its workload identity
	// integration is qualified; never certify the record as executable here.
	if agent.UnsupportedRuntime {
		if _, err := s.store.qualifyNativeAgent(ctx, agent, nil); err != nil {
			return agenttools.CredentialPolicyReadback{}, toolAuthorizationFailure("dependency_unavailable", 503)
		}
	}
	return readback, nil
}

func (s *Server) operatorCredentialPolicy(w http.ResponseWriter, r *http.Request) {
	if !operatorPrincipalAllowed(r.Header.Get("X-Forwarded-Client-Cert"), s.cfg) {
		writeAPIError(w, 403, "forbidden", "Verified operator workload identity is required", correlationID(r))
		return
	}
	agent, err := s.store.getAgentByUID(r.Context(), r.PathValue("uid"))
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, errNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, "dependency_unavailable", "Agent is unavailable", correlationID(r))
		return
	}
	auditTarget(r, "", agent.UID, "")
	if r.Method == http.MethodDelete {
		auditMutation(r)
		if err := s.store.deleteCredentialPolicy(r.Context(), agent); err != nil {
			writeAPIError(w, 503, "dependency_unavailable", "Credential policy cleanup is pending", correlationID(r))
			return
		}
		auditCommit(r)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPut {
		var input agenttools.CredentialPolicy
		if !decodeJSON(w, r, &input) {
			return
		}
		current, err := s.currentCredentialPolicy(r.Context(), agent)
		if err != nil {
			writeToolAuthorizationError(w, r, err)
			return
		}
		if !input.ValidApplied() || input.Revision() != current.Revision() {
			writeAPIError(w, 409, "authorization_changed", "Current credential policy does not match", correlationID(r))
			return
		}
		target, err := s.store.credentialPolicyStore(agent)
		if err != nil || target.recordNamespaceDeleting {
			writeAPIError(w, 503, "dependency_unavailable", "Credential policy storage is unavailable", correlationID(r))
			return
		}
		existing, err := target.readCredentialPolicy(r.Context(), agent)
		if err != nil && !errors.Is(err, errNotFound) {
			writeAPIError(w, 503, "dependency_unavailable", "Credential policy storage is unavailable", correlationID(r))
			return
		}
		if !existing.Matches(current) {
			raw, _ := json.Marshal(current)
			labels := map[string]string{
				recordLabelPrefix + "instance-uid": agent.UID,
				recordLabelPrefix + "owner-tenant": agent.OwnerTenantID,
				recordLabelPrefix + "owner-object": agent.OwnerObjectID,
			}
			auditMutation(r)
			if err := target.putToolSecret(r.Context(), credentialPolicyRecord, agent.UID, labels, nil,
				map[string]string{"policy.json": base64.StdEncoding.EncodeToString(raw)}, existing.ResourceVersion); err != nil {
				writeAPIError(w, 503, "dependency_unavailable", "Credential policy application must be read back before retry", correlationID(r))
				return
			}
			auditCommit(r)
		}
		// A write acknowledgment is deliberately not applied evidence.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	readback, err := s.credentialPolicyReadback(r.Context(), agent)
	if err != nil {
		writeToolAuthorizationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, readback)
}

func (k *kubeClient) deleteCredentialPolicy(ctx context.Context, agent Agent) error {
	target, err := k.credentialPolicyStore(agent)
	if err != nil {
		return err
	}
	secret, err := target.getToolSecret(ctx, credentialPolicyRecord, agent.UID)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !target.credentialRecordOwned(secret, agent) {
		return errors.New("credential policy cleanup ownership is invalid")
	}
	if err := target.deleteToolSecret(ctx, secret); err != nil {
		return err
	}
	if _, err := target.getToolSecret(ctx, credentialPolicyRecord, agent.UID); !errors.Is(err, errNotFound) {
		return errors.New("credential policy removal has not been observed")
	}
	return nil
}

func (s *Server) authorizeCredentialPolicy(ctx context.Context, identity *workloadIdentity, service, provider string) error {
	if !identity.Agent.PolicyManaged || !agenttools.UsesAgentIdentity(provider) {
		return nil
	}
	readback, err := s.credentialPolicyReadback(ctx, identity.Agent)
	if err != nil {
		var authorization toolAuthorizationError
		if errors.As(err, &authorization) && authorization.status == 404 {
			return toolAuthorizationFailure("permission_denied", 403)
		}
		return err
	}
	// The public policy name maps to exactly one compiled service profile.
	if service != "azure_arm" {
		return toolAuthorizationFailure("permission_denied", 403)
	}
	identity.CredentialPolicy = readback.Reference()
	return nil
}
