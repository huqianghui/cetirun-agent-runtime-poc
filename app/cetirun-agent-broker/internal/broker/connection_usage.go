// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"
)

type bindingReservation struct {
	BindingID         string    `json:"bindingId"`
	InstanceUID       string    `json:"instanceUid"`
	InstanceName      string    `json:"instanceName"`
	InstanceNamespace string    `json:"instanceNamespace"`
	Revision          uint64    `json:"revision"`
	CreatedAt         time.Time `json:"createdAt"`
}
type connectionAgentUsage struct {
	InstanceUID       string `json:"instanceUid"`
	InstanceName      string `json:"instanceName"`
	InstanceNamespace string `json:"instanceNamespace"`
}
type connectionUsageResult struct {
	ConnectionID string                 `json:"connectionId"`
	Agents       []connectionAgentUsage `json:"agents"`
	CanRemove    bool                   `json:"canRemove"`
}

// Register before writing the namespace Binding. Connection deletion and this
// registration use the same resourceVersion CAS, including across replicas.
func (k *kubeClient) reserveBinding(ctx context.Context, connection Connection, agent Agent, binding ConnectionBinding, now time.Time) (string, error) {
	token := opaqueID()
	for attempt := 0; attempt < 8; attempt++ {
		record, version, err := k.getConnectionForAgent(ctx, agent, connection.ID)
		if err != nil {
			return "", err
		}
		if record.Deleted || !sameOwner(record.Connection.Owner, binding.Owner) || record.Connection.Revision != connection.Revision || record.Connection.Status != "connected" {
			return "", errConflict
		}
		if record.References == nil {
			record.References = map[string]bindingReservation{}
		}
		record.References[token] = bindingReservation{BindingID: binding.ID, InstanceUID: agent.UID, InstanceName: agent.Name, InstanceNamespace: agent.Namespace, Revision: binding.Revision, CreatedAt: now.UTC()}
		if err := k.putConnection(ctx, record, version); errors.Is(err, errConflict) {
			continue
		} else if err != nil {
			return "", err
		}
		return token, nil
	}
	return "", errConflict
}

// Release only this write's token, never another concurrent write's reference.
func (k *kubeClient) releaseBindingReservation(ctx context.Context, owner Owner, id, token string) error {
	if token == "" {
		return nil
	}
	for attempt := 0; attempt < 8; attempt++ {
		record, version, err := k.getOwnedConnection(ctx, owner, id)
		if errors.Is(err, errNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !sameOwner(record.Connection.Owner, owner) {
			return errNotFound
		}
		if _, ok := record.References[token]; !ok {
			return nil
		}
		delete(record.References, token)
		if err := k.putConnection(ctx, record, version); errors.Is(err, errConflict) {
			continue
		} else {
			return err
		}
	}
	return errConflict
}

// Every binding write first registers on its Connection. Keep checking registered
// Agents even after they become ineligible, but do not require Secret access in
// unrelated Sandbox/Custom namespaces. Read failures are never proof of no usage.
func (k *kubeClient) connectionBindingRecords(ctx context.Context, record *connectionRecord) ([]*bindingRecord, error) {
	owner, id := record.Connection.Owner, record.Connection.ID
	stores := []*kubeClient{k}
	if k.agentNamespaces {
		stores = nil
		referenced := make(map[string]bool, len(record.References))
		for _, ref := range record.References {
			referenced[ref.InstanceUID] = true
		}
		list, err := k.listAgents(ctx)
		if err != nil {
			return nil, err
		}
		for _, document := range list.Items {
			agent := agentFromDocument(document)
			if agent.Namespace == "" || !sameOwner(owner, Owner{agent.OwnerTenantID, agent.OwnerObjectID}) {
				continue
			}
			if agent.UnsupportedRuntime && !referenced[agent.UID] {
				continue
			}
			target, err := k.recordStoreForAgent(ctx, agent)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			stores = append(stores, target)
		}
	}
	records := []*bindingRecord{}
	for _, target := range stores {
		found, err := target.bindingRecords(ctx, recordLabelPrefix+"connection-id="+id)
		if err != nil {
			return nil, err
		}
		for _, item := range found {
			if item.record.Binding.ConnectionID != id || !sameOwner(item.record.Binding.Owner, owner) {
				return nil, errors.New("binding owner changed")
			}
			records = append(records, item.record)
		}
	}
	return records, nil
}

func (k *kubeClient) usage(ctx context.Context, record *connectionRecord) (connectionUsageResult, error) {
	result := connectionUsageResult{ConnectionID: record.Connection.ID, Agents: []connectionAgentUsage{}}
	found, err := k.connectionBindingRecords(ctx, record)
	if err != nil {
		return result, err
	}
	agents := map[string]connectionAgentUsage{}
	for _, binding := range found {
		if !binding.Binding.Revoked && binding.Binding.Enabled {
			agents[binding.Binding.InstanceUID] = connectionAgentUsage{binding.Binding.InstanceUID, binding.InstanceName, binding.InstanceNamespace}
		}
	}
	// Pending registrations also block removal. They are deliberately not treated
	// as a usable Binding by credential issuance or the Agent page.
	for _, ref := range record.References {
		agents[ref.InstanceUID] = connectionAgentUsage{ref.InstanceUID, ref.InstanceName, ref.InstanceNamespace}
	}
	for _, agent := range agents {
		result.Agents = append(result.Agents, agent)
	}
	sort.Slice(result.Agents, func(i, j int) bool { return result.Agents[i].InstanceUID < result.Agents[j].InstanceUID })
	result.CanRemove = len(result.Agents) == 0 && isSharedConnectionMode(connectionMode(record.Connection))
	return result, nil
}

// Recover interrupted reservations only after fencing their possible Binding
// write. Expiry alone is never enough: a delayed request could still commit.
func (k *kubeClient) reconcileReservations(ctx context.Context, record *connectionRecord, now time.Time) error {
	for token, ref := range record.References {
		agent := Agent{UID: ref.InstanceUID, Name: ref.InstanceName, Namespace: ref.InstanceNamespace, OwnerTenantID: record.Connection.Owner.TenantID, OwnerObjectID: record.Connection.Owner.ObjectID}
		if k.agentNamespaces {
			live, err := k.getAgentByUID(ctx, ref.InstanceUID)
			if errors.Is(err, errNotFound) {
				if err := k.releaseBindingReservation(ctx, record.Connection.Owner, record.Connection.ID, token); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			agent = live
		}
		binding, version, err := k.getBinding(ctx, ref.BindingID, agent)
		absent := errors.Is(err, errNotFound)
		if err != nil && !absent {
			return err
		}
		if !absent && binding.ReservationToken == token && !binding.Binding.Revoked && binding.Binding.Enabled {
			continue
		}
		fenced := !absent && binding.Binding.Revision >= ref.Revision
		if !fenced {
			if now.Before(ref.CreatedAt.Add(10 * time.Minute)) {
				continue
			}
			if !absent && !binding.Binding.Revoked && binding.Binding.Enabled {
				continue
			}
			// A tombstone forces any delayed POST/PUT using the old version to conflict.
			tombstone := &bindingRecord{Binding: ConnectionBinding{ID: ref.BindingID, InstanceUID: ref.InstanceUID, ConnectionID: record.Connection.ID, Owner: record.Connection.Owner, Revision: ref.Revision, Revoked: true, CreatedAt: now, UpdatedAt: now}, InstanceName: agent.Name, InstanceNamespace: agent.Namespace}
			if err := k.putBinding(ctx, tombstone, version); errors.Is(err, errConflict) {
				continue
			} else if err != nil {
				return err
			}
		}
		if err := k.releaseBindingReservation(ctx, record.Connection.Owner, record.Connection.ID, token); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) connectionUsage(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerFromPortal(r)
	if err != nil {
		writeAPIError(w, 403, "forbidden", "Verified owner identity is required", correlationID(r))
		return
	}
	record, _, err := s.store.getOwnedConnection(r.Context(), owner, r.PathValue("id"))
	if errors.Is(err, errNotFound) || (err == nil && (record.Deleted || !sameOwner(record.Connection.Owner, owner))) {
		writeAPIError(w, 404, "not_found", "Connection not found", correlationID(r))
		return
	}
	if err == nil {
		err = s.store.reconcileReservations(r.Context(), record, s.currentTime())
		if err == nil {
			record, _, err = s.store.getOwnedConnection(r.Context(), owner, record.Connection.ID)
		}
	}
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection usage is unavailable", correlationID(r))
		return
	}
	usage, err := s.store.usage(r.Context(), record)
	if err != nil {
		writeAPIError(w, 503, "dependency_unavailable", "Connection usage is unavailable", correlationID(r))
		return
	}
	writeJSON(w, 200, usage)
}
