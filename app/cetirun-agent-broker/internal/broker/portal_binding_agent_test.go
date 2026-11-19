// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const (
	portalBindingAgentUID    = "11111111-1111-1111-1111-111111111111"
	portalBindingTenantID    = "22222222-2222-2222-2222-222222222222"
	portalBindingObjectID    = "33333333-3333-3333-3333-333333333333"
	portalBindingOtherObject = "44444444-4444-4444-4444-444444444444"
)

func portalBindingDocument() agentDocument {
	document := agentDocument{}
	document.Metadata.Name = "pending-agent"
	document.Metadata.UID = portalBindingAgentUID
	document.Metadata.Annotations = map[string]string{
		"agentidentity.cetirun.microsoft.com/owner-tenant-id": portalBindingTenantID,
		"agentidentity.cetirun.microsoft.com/owner-object-id": portalBindingObjectID,
	}
	document.Spec.Agent.Type = "openclaw"
	return document
}

func newPortalBindingServer(t *testing.T, status int, document agentDocument) *Server {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/cetirun.microsoft.com/v1alpha1/agents/pending-agent" {
			t.Fatalf("Kubernetes request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			if err := json.NewEncoder(w).Encode(document); err != nil {
				t.Fatal(err)
			}
		}
	}))
	t.Cleanup(api.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Server{store: &kubeClient{baseURL: api.URL, tokenFile: tokenFile, client: api.Client()}}
}

func portalBindingRequest(owner Owner, uid string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/v1/instances/pending-agent/connection-bindings", nil)
	request.SetPathValue("name", "pending-agent")
	request.Header.Set("X-CetiRun-Owner-Tenant-ID", owner.TenantID)
	request.Header.Set("X-CetiRun-Owner-Object-ID", owner.ObjectID)
	request.Header.Set("X-CetiRun-Instance-UID", uid)
	return request
}

func TestPortalBindingAgentSeparatesRuntimeReadinessFromOwnership(t *testing.T) {
	owner := Owner{TenantID: portalBindingTenantID, ObjectID: portalBindingObjectID}
	pending := portalBindingDocument()
	server := newPortalBindingServer(t, http.StatusOK, pending)

	response := httptest.NewRecorder()
	_, _, ok := server.bindingAgent(response, portalBindingRequest(owner, portalBindingAgentUID))
	var payload apiError
	if err := json.NewDecoder(response.Result().Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if ok || response.Code != http.StatusConflict || payload.Error.Code != "runtime_not_ready" {
		t.Fatalf("pending binding = %d/%q, accepted=%t", response.Code, payload.Error.Code, ok)
	}

	for _, request := range []*http.Request{
		portalBindingRequest(Owner{TenantID: portalBindingTenantID, ObjectID: portalBindingOtherObject}, portalBindingAgentUID),
		portalBindingRequest(owner, "55555555-5555-5555-5555-555555555555"),
	} {
		response = httptest.NewRecorder()
		_, _, ok = server.bindingAgent(response, request)
		if ok || response.Code != http.StatusForbidden {
			t.Fatalf("untrusted pending binding = %d, accepted=%t", response.Code, ok)
		}
	}
}

func TestPortalBindingAgentReturnsDependencyFailureSeparately(t *testing.T) {
	server := newPortalBindingServer(t, http.StatusInternalServerError, agentDocument{})
	response := httptest.NewRecorder()
	_, _, ok := server.bindingAgent(response, portalBindingRequest(Owner{TenantID: portalBindingTenantID, ObjectID: portalBindingObjectID}, portalBindingAgentUID))
	var payload apiError
	if err := json.NewDecoder(response.Result().Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if ok || response.Code != http.StatusServiceUnavailable || payload.Error.Code != "dependency_unavailable" {
		t.Fatalf("dependency binding = %d/%q, accepted=%t", response.Code, payload.Error.Code, ok)
	}
}

func TestPortalBindingAgentAcceptsReadyOwnedAgent(t *testing.T) {
	document := portalBindingDocument()
	document.Status.Namespace, document.Status.Phase = "agents", "Ready"
	server := newPortalBindingServer(t, http.StatusOK, document)
	owner := Owner{TenantID: portalBindingTenantID, ObjectID: portalBindingObjectID}
	agent, err := server.portalBindingAgent(portalBindingRequest(owner, portalBindingAgentUID), owner)
	if err != nil || agent.UID != portalBindingAgentUID || agent.Namespace != "agents" {
		t.Fatalf("ready binding Agent = %#v, %v", agent, err)
	}
}

func TestPortalBindingAgentRetainsExistingSuspendedRuntimeBindings(t *testing.T) {
	document := portalBindingDocument()
	document.Status.Namespace, document.Status.Phase = "agents", "Provisioning"
	document.Spec.Suspended = true
	server := newPortalBindingServer(t, http.StatusOK, document)
	owner := Owner{TenantID: portalBindingTenantID, ObjectID: portalBindingObjectID}
	agent, err := server.portalBindingAgent(portalBindingRequest(owner, portalBindingAgentUID), owner)
	if err != nil || agent.UID != portalBindingAgentUID || agent.WorkloadReady {
		t.Fatalf("suspended binding Agent = %#v, %v", agent, err)
	}
}
