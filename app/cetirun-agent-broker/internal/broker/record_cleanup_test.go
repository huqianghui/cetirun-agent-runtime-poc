// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func TestAgentRecordCleanupPreservesSharedCredentials(t *testing.T) {
	f := newNamespaceFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	shared := namespaceDelegatedConnection(now)
	if err := f.store.putConnection(ctx, shared, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.putConnection(ctx, namespaceAgentConnection(now), ""); err != nil {
		t.Fatal(err)
	}
	binding := namespaceBinding("personal", shared.Connection.ID, now)
	if err := f.store.putBinding(ctx, binding, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.secrets["agents"]) != 3 {
		t.Fatalf("expected connection, reference and binding: %d", len(f.secrets["agents"]))
	}
	f.agent.Metadata.DeletionTimestamp = &now
	if err := f.store.deleteAgentRecords(ctx, agentFromDocument(f.agent)); err != nil {
		t.Fatal(err)
	}
	if len(f.secrets["agents"]) != 0 || len(f.secrets["credentials"]) != 1 {
		t.Fatal("incorrect cleanup scope")
	}
	if err := f.store.deleteAgentRecords(ctx, agentFromDocument(f.agent)); err != nil {
		t.Fatal(err)
	}
}

func TestCustomAgentRecordCleanupPreservesSharedCredentials(t *testing.T) {
	f := newNamespaceFixture(t)
	f.agent.Spec.Agent.Type = "custom"
	agent := agentFromDocument(f.agent)
	immutable := true
	f.customReceipt = &configMapDocument{Immutable: &immutable, Data: map[string]string{"ownership": `{"agentName":"agent","agentUID":"` + agent.UID + `","namespace":"agents","namespaceUID":"namespace-agent"}`}}
	f.customReceipt.Metadata.Name = agenttools.CustomOwnershipRecordName(agent.UID)
	f.customReceipt.Metadata.Namespace = "cetirun-system"
	f.customReceipt.Metadata.OwnerReferences = []ownerReference{agentRecordOwner(agent)}
	f.store.nativeOperatorNamespace = "cetirun-system"
	ctx, now := context.Background(), time.Now().UTC()
	shared := namespaceDelegatedConnection(now)
	if err := f.store.putConnection(ctx, shared, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.putConnection(ctx, namespaceAgentConnection(now), ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.putBinding(ctx, namespaceBinding("personal", shared.Connection.ID, now), ""); err != nil {
		t.Fatal(err)
	}
	f.agent.Metadata.DeletionTimestamp = &now
	if err := f.store.deleteAgentRecords(ctx, agentFromDocument(f.agent)); err != nil {
		t.Fatal(err)
	}
	if len(f.secrets["agents"]) != 0 || len(f.secrets["credentials"]) != 1 {
		t.Fatal("Custom cleanup deleted shared credentials or retained Agent records")
	}
}
func TestAgentRecordCleanupRetriesDeleteConflict(t *testing.T) {
	f := newNamespaceFixture(t)
	ctx := context.Background()
	if err := f.store.putConnection(ctx, namespaceAgentConnection(time.Now()), ""); err != nil {
		t.Fatal(err)
	}
	f.conflictNextDelete = true
	if err := f.store.deleteAgentRecords(ctx, agentFromDocument(f.agent)); !errors.Is(err, errConflict) {
		t.Fatalf("conflict = %v", err)
	}
	if len(f.secrets["agents"]) != 1 {
		t.Fatal("deleted conflicting record")
	}
	if err := f.store.deleteAgentRecords(ctx, agentFromDocument(f.agent)); err != nil {
		t.Fatal(err)
	}
}
func TestDeletingAgentCannotCreateRecords(t *testing.T) {
	f := newNamespaceFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	shared := namespaceDelegatedConnection(now)
	if err := f.store.putConnection(ctx, shared, ""); err != nil {
		t.Fatal(err)
	}
	f.agent.Metadata.DeletionTimestamp = &now
	if err := f.store.putConnection(ctx, namespaceAgentConnection(now), ""); err == nil {
		t.Fatal("deleting Agent accepted connection")
	}
	if err := f.store.putBinding(ctx, namespaceBinding("new", shared.Connection.ID, now), ""); err == nil {
		t.Fatal("deleting Agent accepted binding")
	}
	if err := f.store.ensureConnectionReference(ctx, agentFromDocument(f.agent), shared.Connection); err == nil {
		t.Fatal("deleting Agent accepted reference")
	}
}
func TestRecreatedAgentCannotReadOldUIDBinding(t *testing.T) {
	f := newNamespaceFixture(t)
	ctx := context.Background()
	now := time.Now()
	shared := namespaceDelegatedConnection(now)
	if err := f.store.putConnection(ctx, shared, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.putBinding(ctx, namespaceBinding("old", shared.Connection.ID, now), ""); err != nil {
		t.Fatal(err)
	}
	f.agent.Metadata.UID = "66666666-6666-6666-6666-666666666666"
	agent := agentFromDocument(f.agent)
	if _, _, err := f.store.getBinding(ctx, "old", agent); err == nil {
		t.Fatal("new UID inherited old binding")
	}
	secret := f.secrets["agents"][toolRecordName("binding", "old")]
	secret.Metadata.OwnerReferences = []ownerReference{agentRecordOwner(agent)}
	f.secrets["agents"][secret.Metadata.Name] = secret
	if _, _, err := f.store.getBinding(ctx, "old", agent); err == nil {
		t.Fatal("metadata bypassed encrypted UID binding")
	}
}
func TestSandboxCleanupDoesNotRequireLocalSecretAccess(t *testing.T) {
	f := newNamespaceFixture(t)
	f.agent.Spec.SandboxRef = "sandbox"
	base := f.store.client.Transport
	f.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/apis/rbac.authorization.k8s.io/v1/namespaces/agents/rolebindings/cetirun-agent-tools-broker" {
			return securityJSONResponse(t, 404, map[string]any{}), nil
		}
		if r.URL.Path == "/api/v1/namespaces/agents/secrets" {
			t.Fatal("Sandbox secret access")
		}
		return base.RoundTrip(r)
	})
	if err := f.store.deleteAgentRecords(context.Background(), agentFromDocument(f.agent)); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRecordCleanupHandlesNamespaceLifecycle(t *testing.T) {
	for _, phase := range []string{"missing", "terminating"} {
		t.Run(phase, func(t *testing.T) {
			f := newNamespaceFixture(t)
			ctx := context.Background()
			now := time.Now().UTC()
			if err := f.store.putConnection(ctx, namespaceAgentConnection(now), ""); err != nil {
				t.Fatal(err)
			}
			base := f.store.client.Transport
			secretCalls := 0
			f.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/namespaces/credentials/secrets") {
					t.Error("Agent cleanup accessed shared credentials")
				}
				if strings.Contains(r.URL.Path, "/namespaces/agents/secrets") {
					secretCalls++
				}
				if r.URL.Path == "/api/v1/namespaces/agents" {
					if phase == "missing" {
						return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
					}
					body, err := json.Marshal(map[string]any{"metadata": secretMetadata{OwnerReferences: []ownerReference{agentRecordOwner(agentFromDocument(f.agent))}, DeletionTimestamp: &now}})
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				}
				return base.RoundTrip(r)
			})
			if err := f.store.deleteAgentRecords(ctx, agentFromDocument(f.agent)); err != nil {
				t.Fatalf("cleanup blocked by %s namespace: %v", phase, err)
			}
			if phase == "missing" && secretCalls != 0 {
				t.Fatal("cleanup accessed missing namespace")
			}
			if phase == "terminating" && len(f.secrets["agents"]) != 0 {
				t.Fatal("terminating namespace records survived cleanup")
			}
		})
	}
}
