// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBindingCreateUsesOneActiveRecordPerInstanceConnection(t *testing.T) {
	fixture := newAuthorizationFixture(t, "github", "github")
	// An old random-ID record must not prevent recreation after its revocation.
	fixture.mutateBinding(t, func(grant *ConnectionBinding) {
		grant.Revoked = true
		grant.Revision++
	})

	base := fixture.server.store.client.Transport
	var storageMu sync.Mutex
	reads := make(chan struct{}, 2)
	release := make(chan struct{})
	fixture.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		targetRead := request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/"+toolRecordName("binding", fixture.grant.ID))
		storageMu.Lock()
		response, err := base.RoundTrip(request)
		storageMu.Unlock()
		if targetRead {
			reads <- struct{}{}
			<-release
		}
		return response, err
	})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { responses <- bindingCreateRequest(t, fixture) }()
	}
	<-reads
	<-reads
	close(release)
	first, second := <-responses, <-responses
	fixture.server.store.client.Transport = base

	statuses := map[int]int{first.Code: 1, second.Code: 1}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent create statuses = %d, %d; want one 200 and one 409", first.Code, second.Code)
	}
	grants, err := fixture.server.store.listBindings(context.Background(), fixture.grant.Owner, fixture.grant.InstanceUID)
	if err != nil {
		t.Fatal(err)
	}
	active := []ConnectionBinding{}
	for _, grant := range grants {
		if grant.ConnectionID == fixture.connection.Connection.ID && activeBinding(grant, time.Now()) {
			active = append(active, grant)
		}
	}
	if len(active) != 1 || active[0].ID != bindingKey(fixture.grant.InstanceUID, fixture.connection.Connection.ID) {
		t.Fatalf("active grants after concurrent create = %#v", active)
	}

	// A newly deterministic record can also be revoked and recreated; its
	// resource-version update must not leave two active grants behind.
	record, version, err := fixture.server.store.getBinding(context.Background(), active[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Binding.Revoked = true
	record.Binding.Revision++
	if err := fixture.server.store.putBinding(context.Background(), record, version); err != nil {
		t.Fatal(err)
	}
	response := bindingCreateRequest(t, fixture)
	if response.Code != http.StatusOK {
		t.Fatalf("recreate after revoke = %d: %s", response.Code, response.Body.String())
	}
	var recreated ConnectionBinding
	if err := json.NewDecoder(response.Result().Body).Decode(&recreated); err != nil {
		t.Fatal(err)
	}
	if recreated.ID != active[0].ID || recreated.Revision <= record.Binding.Revision {
		t.Fatalf("recreated grant = %#v; revoked record = %#v", recreated, record.Binding)
	}
	grants, err = fixture.server.store.listBindings(context.Background(), fixture.grant.Owner, fixture.grant.InstanceUID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, grant := range grants {
		if grant.ConnectionID == fixture.connection.Connection.ID && activeBinding(grant, time.Now()) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("active grants after revoke/recreate = %d, want 1", count)
	}
}

func bindingCreateRequest(t *testing.T, fixture *authorizationFixture) *httptest.ResponseRecorder {
	t.Helper()
	input := bindingRequest{
		ConnectionID: fixture.connection.Connection.ID,
		Services:     fixture.grant.Services,
		Enabled:      true,
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/instances/agent/connection-bindings", bytes.NewReader(body))
	request.SetPathValue("name", fixture.agent.Metadata.Name)
	request.Header.Set("X-CetiRun-Owner-Tenant-ID", fixture.grant.Owner.TenantID)
	request.Header.Set("X-CetiRun-Owner-Object-ID", fixture.grant.Owner.ObjectID)
	request.Header.Set("X-CetiRun-Instance-UID", fixture.grant.InstanceUID)
	response := httptest.NewRecorder()
	fixture.server.bindingCreate(response, request)
	return response
}
