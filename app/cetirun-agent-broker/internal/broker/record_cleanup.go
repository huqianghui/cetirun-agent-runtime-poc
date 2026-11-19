// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
)

// Called by the authenticated Operator before the Agent finalizer is removed.
// Shared personal credentials are never owned or deleted by an Agent.
func (k *kubeClient) deleteAgentRecords(ctx context.Context, agent Agent) error {
	stores := []*kubeClient{}
	if agent.UnsupportedRuntime && nativeAgentCandidate(agent) && agent.Namespace != "" {
		present, err := k.nativeStorageRecorded(ctx, agent)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
	}
	// Ordinary Custom namespaces use the immutable Custom ownership receipt.
	// They retain the unsupported-runtime boundary for credential issuance, but
	// their Broker-owned binding records must be removed before the namespace is
	// deleted. Other unsupported runtimes still rely on garbage collection.
	if k.agentNamespaces && agent.Namespace != "" && (!agent.UnsupportedRuntime || nativeAgentCandidate(agent) || ordinaryCustomAgent(agent)) {
		target, err := k.agentRecordStore(ctx, agent.UID)
		if err != nil && !errors.Is(err, errNotFound) {
			return err
		}
		if err == nil {
			stores = append(stores, target)
		}
	}
	if !k.agentNamespaces {
		stores = append(stores, k)
	}
	owner := Owner{agent.OwnerTenantID, agent.OwnerObjectID}
	for _, target := range stores {
		grants, err := target.listToolSecrets(ctx, "binding", recordLabelPrefix+"instance-uid="+agent.UID)
		if errors.Is(err, errNotFound) && target.recordAgent != nil {
			continue
		}
		if err != nil {
			return err
		}
		for _, secret := range grants.Items {
			record, err := target.decodeBinding(ctx, secret)
			if err != nil {
				return err
			}
			if record.Binding.InstanceUID != agent.UID || !sameOwner(record.Binding.Owner, owner) || secret.Metadata.Name != toolRecordName("binding", record.Binding.ID) {
				return errors.New("Binding cleanup ownership is invalid")
			}
			if err := target.deleteToolSecret(ctx, secret); err != nil {
				return err
			}
		}
		// Shared personal credentials are outside the Agent record store.
		connections, err := target.listToolSecrets(ctx, "connection", recordLabelPrefix+"owner-tenant="+owner.TenantID+","+recordLabelPrefix+"owner-object="+owner.ObjectID)
		if errors.Is(err, errNotFound) && target.recordAgent != nil {
			continue
		}
		if err != nil {
			return err
		}
		for _, secret := range connections.Items {
			record, err := target.decodeConnection(ctx, secret)
			if err != nil {
				return err
			}
			c := record.Connection
			if connectionMode(c) != modeAgent || c.Identity == nil || c.Identity.InstanceUID != agent.UID {
				continue
			}
			if !sameOwner(c.Owner, owner) || secret.Metadata.Name != toolRecordName("connection", c.ID) {
				return errors.New("Connection cleanup ownership is invalid")
			}
			if err := target.deleteToolSecret(ctx, secret); err != nil {
				return err
			}
		}
		if target.recordAgent == nil {
			continue
		}
		references, err := target.listToolSecrets(ctx, "connection-reference", recordLabelPrefix+"instance-uid="+agent.UID)
		if errors.Is(err, errNotFound) && target.recordAgent != nil {
			continue
		}
		if err != nil {
			return err
		}
		for _, secret := range references.Items {
			id := secret.Metadata.Labels[recordLabelPrefix+"connection-id"]
			if !validConnectionReference(secret, agent, id) || secret.Metadata.Name != toolRecordName("connection", id) {
				return errors.New("Connection reference cleanup ownership is invalid")
			}
			if err := target.deleteToolSecret(ctx, secret); err != nil {
				return err
			}
		}
	}
	return nil
}
