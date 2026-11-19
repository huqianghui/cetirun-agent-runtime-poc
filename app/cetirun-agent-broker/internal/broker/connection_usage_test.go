// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func accountUsageFixture(t *testing.T, bound bool) (*namespaceFixture, *connectionRecord, *Server) {
	t.Helper()
	f := newNamespaceFixture(t)
	ctx := context.Background()
	record := namespaceDelegatedConnection(time.Now())
	if err := f.store.putConnection(ctx, record, ""); err != nil {
		t.Fatal(err)
	}
	if bound {
		binding := namespaceBinding("binding", record.Connection.ID, time.Now())
		token, err := f.store.reserveBinding(ctx, record.Connection, agentFromDocument(f.agent), binding.Binding, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		binding.ReservationToken = token
		if err := f.store.putBinding(ctx, binding, ""); err != nil {
			t.Fatal(err)
		}
	}
	record, _, err := f.store.getConnection(ctx, record.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: Config{PortalNamespace: "portal", PortalServiceAccount: "portal"}, store: f.store, mux: http.NewServeMux(), tlsMux: http.NewServeMux()}
	server.routes()
	return f, record, server
}

func accountUsageRequest(server *Server, record *connectionRecord, method, suffix string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/v1/connections/"+record.Connection.ID+suffix, nil)
	setPortalRequestHeaders(r, record.Connection.Owner, "")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	return w
}

func TestAccountUsageSkipsUnrelatedIneligibleNamespaces(t *testing.T) {
	for _, runtime := range []string{"sandbox", "policy-held", "custom"} {
		for _, bound := range []bool{false, true} {
			name := runtime + map[bool]string{false: "/unused", true: "/bound-elsewhere"}[bound]
			t.Run(name, func(t *testing.T) {
				f, record, server := accountUsageFixture(t, bound)
				other := f.agent
				other.Metadata.Name, other.Metadata.UID = "unrelated", "66666666-6666-6666-6666-666666666666"
				other.Status.Namespace = "unrelated"
				switch runtime {
				case "sandbox":
					other.Spec.SandboxRef = "shared"
				case "policy-held":
					other.Metadata.Finalizers = []string{"cetirun.microsoft.com/sandbox-hold"}
				case "custom":
					other.Spec.Agent.Type = "custom"
				}
				reads := 0
				transport := f.store.client.Transport
				f.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/apis/cetirun.microsoft.com/v1alpha1/agents":
						return securityJSONResponse(t, 200, agentList{Items: []agentDocument{f.agent, other}}), nil
					case "/api/v1/namespaces/unrelated":
						return securityJSONResponse(t, 200, map[string]any{"metadata": secretMetadata{Name: "unrelated", OwnerReferences: []ownerReference{agentRecordOwner(agentFromDocument(other))}}}), nil
					}
					if strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/unrelated/secrets") {
						reads++
						return securityJSONResponse(t, 403, map[string]string{"message": "Broker has no Secret access in this namespace"}), nil
					}
					return transport.RoundTrip(r)
				})
				response := accountUsageRequest(server, record, http.MethodGet, "/usage")
				if response.Code != 200 {
					t.Fatalf("usage returned %d: %s", response.Code, response.Body.String())
				}
				var usage connectionUsageResult
				if err := json.Unmarshal(response.Body.Bytes(), &usage); err != nil {
					t.Fatal(err)
				}
				if usage.CanRemove == bound || len(usage.Agents) != map[bool]int{false: 0, true: 1}[bound] {
					t.Fatalf("unexpected usage: %#v", usage)
				}
				if bound && usage.Agents[0].InstanceUID != namespaceAgentUID {
					t.Fatalf("lost the actual binding: %#v", usage.Agents)
				}
				response = accountUsageRequest(server, record, http.MethodDelete, "")
				if expected := map[bool]int{false: 204, true: 409}[bound]; response.Code != expected {
					t.Fatalf("removal returned %d, want %d: %s", response.Code, expected, response.Body.String())
				}
				if reads != 0 {
					t.Fatalf("read unrelated namespace Secrets %d times", reads)
				}
			})
		}
	}
}

func TestAccountUsageStillFailsWhenRelevantBindingsCannotBeRead(t *testing.T) {
	for _, state := range []string{"eligible", "bound-sandbox", "pending-sandbox"} {
		t.Run(state, func(t *testing.T) {
			f, record, server := accountUsageFixture(t, state != "eligible")
			if state != "eligible" {
				f.agent.Spec.SandboxRef = "shared"
			}
			if state == "pending-sandbox" {
				delete(f.secrets["agents"], toolRecordName("binding", "binding"))
			}
			reads := 0
			transport := f.store.client.Transport
			f.store.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/agents/secrets") {
					reads++
					return securityJSONResponse(t, 403, map[string]string{"message": "synthetic authorization failure"}), nil
				}
				return transport.RoundTrip(r)
			})
			if _, err := f.store.usage(context.Background(), record); err == nil {
				t.Fatal("usage ignored an unreadable eligible or registered namespace")
			}
			if reads == 0 {
				t.Fatal("usage did not check the relevant namespace")
			}
			for _, operation := range [][2]string{{http.MethodGet, "/usage"}, {http.MethodDelete, ""}} {
				response := accountUsageRequest(server, record, operation[0], operation[1])
				if response.Code != 503 {
					t.Fatalf("%s returned %d, want 503", operation[0], response.Code)
				}
			}
			current, _, err := f.store.getConnection(context.Background(), record.Connection.ID)
			if err != nil || current.Deleted || current.Credential == nil {
				t.Fatal("unreadable binding allowed credential removal")
			}
		})
	}
}
