// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const recordLabelPrefix = "agenttools.cetirun.microsoft.com/"

func agentRecordOwner(agent Agent) ownerReference {
	return ownerReference{APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: agent.Name, UID: agent.UID}
}

// agentRecordStore resolves authority from the live Agent and namespace, never
// from a caller-supplied namespace or a persisted display snapshot.
func (k *kubeClient) agentRecordStore(ctx context.Context, uid string, names ...string) (*kubeClient, error) {
	var agent Agent
	var err error
	if len(names) == 1 && names[0] != "" {
		// A stored name is only a lookup hint. Recreated objects must not gain access.
		agent, err = k.getAgent(ctx, names[0])
		if err == nil && agent.UID != uid {
			return nil, errNotFound
		}
	} else {
		agent, err = k.getAgentByUID(ctx, uid)
	}
	if err != nil {
		return nil, err
	}
	return k.recordStoreForAgent(ctx, agent)
}

func (k *kubeClient) recordStoreForAgent(ctx context.Context, agent Agent) (*kubeClient, error) {
	if !dnsLabel(agent.Namespace) || agent.Namespace == k.namespace || !uuidPattern.MatchString(agent.UID) {
		return nil, errors.New("Agent record namespace is invalid")
	}
	var namespace struct {
		Metadata secretMetadata `json:"metadata"`
	}
	if _, err := k.request(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(agent.Namespace), nil, &namespace); err != nil {
		return nil, err
	}
	if ordinaryCustomAgent(agent) {
		// Custom namespaces deliberately do not use an Agent OwnerReference, and
		// a foreign OwnerReference must not bypass the receipt proof.
		if err := k.customRecordNamespaceOwned(ctx, agent, namespace.Metadata); err != nil {
			return nil, err
		}
	} else {
		owned := false
		for _, owner := range namespace.Metadata.OwnerReferences {
			if owner == agentRecordOwner(agent) {
				owned = true
			}
		}
		if !owned {
			return nil, errors.New("Agent namespace ownership is invalid")
		}
	}
	copy := *k
	copy.namespace, copy.agentNamespaces, copy.recordAgent = agent.Namespace, false, &agent
	copy.recordNamespaceDeleting = namespace.Metadata.DeletionTimestamp != nil
	return &copy, nil
}

func ordinaryCustomAgent(agent Agent) bool {
	return !agent.PolicyManaged && agent.RuntimeType == "custom"
}

type configMapDocument struct {
	Metadata struct {
		Name            string           `json:"name"`
		Namespace       string           `json:"namespace"`
		OwnerReferences []ownerReference `json:"ownerReferences"`
	} `json:"metadata"`
	Immutable *bool             `json:"immutable"`
	Data      map[string]string `json:"data"`
}

// customRecordNamespaceOwned validates the immutable operator receipt in
// addition to the namespace annotation. The annotation alone is writable by a
// namespace administrator and is never authority for Broker record access.
func (k *kubeClient) customRecordNamespaceOwned(ctx context.Context, agent Agent, namespace secretMetadata) error {
	if namespace.Annotations["cetirun.microsoft.com/agent-uid"] != agent.UID {
		return errors.New("Custom Agent namespace ownership is invalid")
	}
	name := agenttools.CustomOwnershipRecordName(agent.UID)
	var receipt configMapDocument
	path := "/api/v1/namespaces/" + url.PathEscape(k.nativeOperatorNamespace) + "/configmaps/" + url.PathEscape(name)
	if _, err := k.request(ctx, http.MethodGet, path, nil, &receipt); err != nil {
		return err
	}
	ownerMatches := len(receipt.Metadata.OwnerReferences) == 1 && receipt.Metadata.OwnerReferences[0] == agentRecordOwner(agent)
	// During orphan deletion Kubernetes can remove the owner reference from
	// the receipt before the Broker's final cleanup call. That narrow case is
	// safe only for the deleting live Agent and only after all UID checks below.
	ownerlessDeleting := agent.Deleting && len(receipt.Metadata.OwnerReferences) == 0
	if receipt.Metadata.Name != name || receipt.Metadata.Namespace != k.nativeOperatorNamespace || receipt.Immutable == nil || !*receipt.Immutable ||
		(!ownerMatches && !ownerlessDeleting) {
		return errors.New("Custom ownership receipt is invalid")
	}
	raw, ok := receipt.Data["ownership"]
	if !ok {
		return errors.New("Custom ownership receipt is invalid")
	}
	proof, err := agenttools.ParseCustomOwnershipNamespace(raw)
	if err != nil || !proof.Matches(agent.Name, agent.UID, namespace.Name, namespace.UID) {
		return errors.New("Custom ownership receipt does not match the Agent namespace")
	}
	return nil
}

func recordOwnedBy(secret secretDocument, agent Agent) bool {
	return len(secret.Metadata.OwnerReferences) == 1 && secret.Metadata.OwnerReferences[0] == agentRecordOwner(agent)
}

func (k *kubeClient) validateConnectionLocation(record *connectionRecord) error {
	if k.recordAgent == nil {
		return nil
	}
	c, a := record.Connection, k.recordAgent
	if connectionMode(c) != modeAgent || c.Identity == nil || c.Identity.InstanceUID != a.UID || !sameOwner(c.Owner, Owner{a.OwnerTenantID, a.OwnerObjectID}) {
		return errors.New("Connection does not belong to the Agent namespace")
	}
	return nil
}

func (k *kubeClient) validateBindingLocation(record *bindingRecord) error {
	if a := k.recordAgent; a != nil && (record.Binding.InstanceUID != a.UID || !sameOwner(record.Binding.Owner, Owner{a.OwnerTenantID, a.OwnerObjectID})) {
		return errors.New("Binding does not belong to the Agent namespace")
	}
	return nil
}

// Agent identities and bindings have no central-record fallback.
func (k *kubeClient) getConnectionForAgent(ctx context.Context, agent Agent, id string) (*connectionRecord, string, error) {
	if !k.agentNamespaces {
		return k.getConnection(ctx, id)
	}
	target, err := k.recordStoreForAgent(ctx, agent)
	if err != nil {
		return nil, "", err
	}
	secret, err := target.getToolSecret(ctx, "connection", id)
	if err == nil && secret.Metadata.Labels[recordLabelPrefix+"record-type"] == "connection" {
		return target.getConnection(ctx, id)
	}
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, "", err
	}
	if err == nil && !validConnectionReference(secret, *target.recordAgent, id) {
		return nil, "", errors.New("Connection reference is invalid")
	}
	record, version, err := k.getConnection(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if !isSharedConnectionMode(connectionMode(record.Connection)) || record.Connection.Identity != nil {
		return nil, "", errNotFound
	}
	return record, version, nil
}

func (k *kubeClient) ownerAgentStores(ctx context.Context, owner Owner) ([]*kubeClient, error) {
	agents, err := k.listAgents(ctx)
	if err != nil {
		return nil, err
	}
	result := []*kubeClient{}
	for _, document := range agents.Items {
		a := agentFromDocument(document)
		if (a.UnsupportedRuntime && !nativeAgentCandidate(a)) || a.Deleting || a.Namespace == "" || !sameOwner(owner, Owner{a.OwnerTenantID, a.OwnerObjectID}) {
			continue
		}
		if nativeAgentCandidate(a) {
			present, err := k.nativeStorageRecorded(ctx, a)
			if err != nil {
				return nil, err
			}
			if !present {
				continue
			}
		}
		target, err := k.recordStoreForAgent(ctx, a)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, target)
	}
	return result, nil
}

func (k *kubeClient) getOwnedConnection(ctx context.Context, owner Owner, id string) (*connectionRecord, string, error) {
	if !k.agentNamespaces {
		return k.getConnection(ctx, id)
	}
	record, version, err := k.getConnection(ctx, id)
	if err == nil && isSharedConnectionMode(connectionMode(record.Connection)) {
		if !sameOwner(owner, record.Connection.Owner) {
			return nil, "", errNotFound
		}
		return record, version, nil
	}
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, "", err
	}
	stores, err := k.ownerAgentStores(ctx, owner)
	if err != nil {
		return nil, "", err
	}
	var found *connectionRecord
	for _, target := range stores {
		secret, readErr := target.getToolSecret(ctx, "connection", id)
		if errors.Is(readErr, errNotFound) {
			continue
		}
		if readErr != nil {
			return nil, "", readErr
		}
		if secret.Metadata.Labels[recordLabelPrefix+"record-type"] != "connection" {
			continue
		}
		r, v, readErr := target.getConnection(ctx, id)
		if readErr != nil {
			return nil, "", readErr
		}
		if found != nil {
			return nil, "", errors.New("Connection has ambiguous ownership")
		}
		found, version = r, v
	}
	if found == nil {
		return nil, "", errNotFound
	}
	return found, version, nil
}

func validConnectionReference(secret secretDocument, agent Agent, id string) bool {
	l := secret.Metadata.Labels
	return l[recordLabelPrefix+"record-type"] == "connection-reference" &&
		l[recordLabelPrefix+"id"] == id && l[recordLabelPrefix+"connection-id"] == id &&
		l[recordLabelPrefix+"instance-uid"] == agent.UID &&
		sameOwner(Owner{l[recordLabelPrefix+"owner-tenant"], l[recordLabelPrefix+"owner-object"]}, Owner{agent.OwnerTenantID, agent.OwnerObjectID}) &&
		len(secret.Data) == 0 && reflect.DeepEqual(secret.Metadata.OwnerReferences, []ownerReference{agentRecordOwner(agent)})
}

// This credential-free reference is for inventory/lifecycle only. Binding and
// decrypted owner checks remain the authority for using the shared credential.
func (k *kubeClient) ensureConnectionReference(ctx context.Context, agent Agent, connection Connection) error {
	if !k.agentNamespaces || !isSharedConnectionMode(connectionMode(connection)) {
		return nil
	}
	target, err := k.recordStoreForAgent(ctx, agent)
	if err != nil {
		return err
	}
	agent = *target.recordAgent
	if agent.Deleting || target.recordNamespaceDeleting || !sameOwner(connection.Owner, Owner{agent.OwnerTenantID, agent.OwnerObjectID}) {
		return errors.New("Connection reference owner is invalid")
	}
	labels := map[string]string{
		recordLabelPrefix + "id": connection.ID, recordLabelPrefix + "connection-id": connection.ID,
		recordLabelPrefix + "instance-uid": agent.UID, recordLabelPrefix + "owner-tenant": connection.Owner.TenantID,
		recordLabelPrefix + "owner-object": connection.Owner.ObjectID, recordLabelPrefix + "provider": connection.Provider,
		recordLabelPrefix + "mode": connectionMode(connection),
	}
	annotations := recordInstanceMetadata(labels, agent.Name, agent.Namespace)
	annotations[recordLabelPrefix+"credential-namespace"] = k.namespace
	secret, err := target.getToolSecret(ctx, "connection", connection.ID)
	if err == nil {
		if !validConnectionReference(secret, agent, connection.ID) {
			return errors.New("Connection reference conflicts with an existing record")
		}
		return nil
	}
	if !errors.Is(err, errNotFound) {
		return err
	}
	// Keep the familiar Connection name while distinguishing the record type.
	labels["app.kubernetes.io/managed-by"] = "cetirun-agent-broker"
	labels[recordLabelPrefix+"record-type"] = "connection-reference"
	secret = secretDocument{APIVersion: "v1", Kind: "Secret", Metadata: secretMetadata{
		Name: toolRecordName("connection", connection.ID), Namespace: target.namespace, Labels: labels, Annotations: annotations,
		OwnerReferences: []ownerReference{agentRecordOwner(agent)},
	}}
	_, err = target.request(ctx, http.MethodPost, "/api/v1/namespaces/"+url.PathEscape(target.namespace)+"/secrets", secret, nil)
	if errors.Is(err, errConflict) {
		current, readErr := target.getToolSecret(ctx, "connection", connection.ID)
		if readErr == nil && validConnectionReference(current, agent, connection.ID) {
			return nil
		}
	}
	return err
}

func (k *kubeClient) listToolSecrets(ctx context.Context, kind, selector string) (secretList, error) {
	selector = recordLabelPrefix + "record-type=" + kind + func() string {
		if selector != "" {
			return "," + selector
		}
		return ""
	}()
	result := secretList{}
	continuation := ""
	for {
		query := url.Values{"labelSelector": {selector}, "limit": {"50"}}
		if continuation != "" {
			query.Set("continue", continuation)
		}
		var page secretList
		if _, err := k.request(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(k.namespace)+"/secrets?"+query.Encode(), nil, &page); err != nil {
			return secretList{}, err
		}
		if k.recordAgent != nil {
			for _, secret := range page.Items {
				if !recordOwnedBy(secret, *k.recordAgent) {
					return secretList{}, errors.New("Agent record ownership is invalid")
				}
			}
		}
		result.Items = append(result.Items, page.Items...)
		if page.Metadata.Continue == "" {
			return result, nil
		}
		if page.Metadata.Continue == continuation {
			return secretList{}, errors.New("Kubernetes pagination did not advance")
		}
		continuation = page.Metadata.Continue
	}
}

func (k *kubeClient) deleteToolSecret(ctx context.Context, secret secretDocument) error {
	if secret.Metadata.Namespace != k.namespace || secret.Metadata.UID == "" || secret.Metadata.ResourceVersion == "" {
		return errors.New("Secret deletion requires namespace, UID and resource version")
	}
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": secret.Metadata.UID, "resourceVersion": secret.Metadata.ResourceVersion}}
	_, err := k.request(ctx, http.MethodDelete, "/api/v1/namespaces/"+url.PathEscape(k.namespace)+"/secrets/"+url.PathEscape(secret.Metadata.Name), body, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}
