// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
package tools

import (
	"bytes"
	"context"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/brokerclient"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/identity"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/tools/providers"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeBroker struct {
	check        brokerclient.CheckResponse
	issueChange  func(*brokerclient.IssueResponse)
	err          error
	issued       int
	correlations []string
}

func (f *fakeBroker) List(context.Context, string) (brokerclient.ListResponse, error) {
	return brokerclient.ListResponse{InstanceUID: f.check.InstanceUID, Connections: []agenttools.Connection{f.check.Connection}, Bindings: []agenttools.Binding{f.check.Binding}, ContractVersion: agenttools.ContractVersion}, f.err
}
func (f *fakeBroker) Check(ctx context.Context, _ brokerclient.Request) (brokerclient.CheckResponse, error) {
	f.correlations = append(f.correlations, agenttools.Correlation(ctx))
	return f.check, f.err
}
func (f *fakeBroker) Issue(ctx context.Context, r brokerclient.Request) (brokerclient.IssueResponse, error) {
	f.issued++
	f.correlations = append(f.correlations, agenttools.Correlation(ctx))
	if r.BindingRevision == nil || *r.BindingRevision != f.check.Binding.Revision || r.ConnectionRevision == nil || *r.ConnectionRevision != f.check.Connection.Revision {
		panic("revision not forwarded")
	}
	if !reflect.DeepEqual(r.CredentialPolicy, f.check.CredentialPolicy) {
		panic("credential policy revision not forwarded")
	}
	response := brokerclient.IssueResponse{CheckResponse: f.check, AccessToken: "synthetic-private-token", ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)}
	if f.check.CredentialPolicy != nil {
		policy := *f.check.CredentialPolicy
		response.CredentialPolicy = &policy
	}
	if f.issueChange != nil {
		f.issueChange(&response)
	}
	return response, f.err
}

type fakeProvider struct {
	calls int
	err   error
}

func (f *fakeProvider) Execute(_ context.Context, _ providers.Request, token, id string) (providers.Result, error) {
	f.calls++
	if token != "synthetic-private-token" || id == "" {
		panic("missing credential or correlation")
	}
	return providers.Result{Status: 403, Body: map[string]any{"error": "ProviderDenied"}, RequestIDs: []string{"provider-request-id"}}, f.err
}
func serviceFixture() (*Service, *fakeBroker, *fakeProvider, identity.Caller, *bytes.Buffer) {
	caller := identity.Caller{InstanceUID: "agent-uid", Proof: "private-workload-proof"}
	broker := &fakeBroker{check: brokerclient.CheckResponse{InstanceUID: caller.InstanceUID, Service: "azure_arm", ExecutorAccountID: "00000000-0000-0000-0000-000000000002", ContractVersion: agenttools.ContractVersion, Connection: agenttools.Connection{ID: "c", Mode: "agent", Provider: "azure", Status: "connected", AccountDisplay: "Synthetic Agent", Services: []string{"azure_arm"}, Revision: 3, Identity: &agenttools.IdentityBinding{InstanceUID: caller.InstanceUID, TenantID: "00000000-0000-0000-0000-000000000003", PrincipalID: "00000000-0000-0000-0000-000000000002", ClientID: "00000000-0000-0000-0000-000000000002"}}, Binding: agenttools.Binding{ID: "b", InstanceUID: caller.InstanceUID, ConnectionID: "c", Services: []string{"azure_arm"}, Enabled: true, Revision: 4}}}
	provider := &fakeProvider{}
	log := &bytes.Buffer{}
	return &Service{broker: broker, providers: provider, logger: slog.New(slog.NewJSONHandler(log, nil))}, broker, provider, caller, log
}
func arguments() map[string]any {
	return map[string]any{"connection_id": "c", "service": "azure_arm", "method": "POST", "path": "/providers/Microsoft.ResourceGraph/resources", "body": map[string]any{"query": "private-body"}}
}
func TestProviderPermissionsDecideWithoutOperationGrant(t *testing.T) {
	s, b, p, caller, log := serviceFixture()
	result, err := s.Call(context.Background(), caller, "service_request", arguments())
	if err != nil || result["status"] != 403 || p.calls != 1 || b.issued != 1 {
		t.Fatalf("provider request blocked: %v %#v", err, result)
	}
	for _, secret := range []string{"private-body", "private-workload-proof", "synthetic-private-token"} {
		if strings.Contains(log.String(), secret) {
			t.Fatal("audit leaked sensitive data")
		}
	}
	for _, expected := range []string{"agent-uid", "Synthetic Agent", "provider-request-id", "correlation_id", "403"} {
		if !strings.Contains(log.String(), expected) {
			t.Fatalf("audit missing %s", expected)
		}
	}
}
func TestBindingAndIssuanceChangesStopExecution(t *testing.T) {
	for _, change := range []func(*brokerclient.IssueResponse){func(r *brokerclient.IssueResponse) { r.Binding.Enabled = false }, func(r *brokerclient.IssueResponse) { r.Binding.Revision++ }, func(r *brokerclient.IssueResponse) { r.Connection.Revision++ }, func(r *brokerclient.IssueResponse) { r.InstanceUID = "other-agent" }, func(r *brokerclient.IssueResponse) { r.Binding.Services = []string{"other"} }, func(r *brokerclient.IssueResponse) { r.ExpiresAt = time.Now().Add(-time.Hour).Format(time.RFC3339) }} {
		s, b, p, caller, _ := serviceFixture()
		b.issueChange = change
		if _, err := s.Call(context.Background(), caller, "service_request", arguments()); err == nil || p.calls != 0 {
			t.Fatal("changed authorization reached provider")
		}
	}
}

func TestDelegatedServicesUseProviderPermissions(t *testing.T) {
	for _, service := range agenttools.Services() {
		t.Run(service.ID, func(t *testing.T) {
			s, broker, provider, caller, _ := serviceFixture()
			broker.check.Service = service.ID
			broker.check.Connection.Mode = "delegated"
			broker.check.Connection.Identity = nil
			broker.check.Connection.Provider = service.Provider
			broker.check.Connection.Services = []string{service.ID}
			broker.check.Binding.Services = []string{service.ID}
			args := arguments()
			args["service"] = service.ID
			args["path"] = "/example"
			if _, err := s.Call(context.Background(), caller, "service_request", args); err != nil || provider.calls != 1 || broker.issued != 1 {
				t.Fatalf("delegated execution: calls=%d issued=%d err=%v", provider.calls, broker.issued, err)
			}
		})
	}
}

func TestMismatchedProviderOrDelegatedIdentityNeverExecutes(t *testing.T) {
	for _, mutate := range []func(*brokerclient.CheckResponse){
		func(r *brokerclient.CheckResponse) { r.Connection.Mode = "delegated" },
		func(r *brokerclient.CheckResponse) { r.Connection.Provider = "github" },
		func(r *brokerclient.CheckResponse) { r.Connection.Mode = "unknown" },
	} {
		s, broker, provider, caller, _ := serviceFixture()
		mutate(&broker.check)
		if _, err := s.Call(context.Background(), caller, "service_request", arguments()); err == nil || provider.calls != 0 || broker.issued != 0 {
			t.Fatal("invalid executor reached provider")
		}
	}
}

func TestIsolationAndPlatformRejectionAudit(t *testing.T) {
	s, b, p, caller, log := serviceFixture()
	b.check.Binding.InstanceUID = "other-agent"
	_, err := s.Call(context.Background(), caller, "service_request", arguments())
	if err == nil || p.calls != 0 || b.issued != 0 || !strings.Contains(log.String(), "authorization_changed") || err.(*ToolError).CorrelationID == "" {
		t.Fatal("cross-Agent connection accepted or rejection unaudited")
	}
}
func TestStableCatalogAndDynamicServiceDiscovery(t *testing.T) {
	s, b, _, caller, _ := serviceFixture()
	before, err := s.List(context.Background(), caller)
	if err != nil || len(before) != 4 {
		t.Fatal("generic catalog unavailable")
	}
	b.check.Binding.Enabled = false
	after, err := s.List(context.Background(), caller)
	if err != nil || len(after) != len(before) {
		t.Fatal("binding changes altered catalog")
	}
	result, err := s.Call(context.Background(), caller, "services_list", map[string]any{})
	if err != nil || len(result["services"].([]any)) != 0 {
		t.Fatal("disabled binding remained discoverable")
	}
}

func TestRESTAuditLifecycleAndPrivacy(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		s, b, p, caller, log := serviceFixture()
		args := arguments()
		args["query"] = map[string]any{"q": "private-query"}
		args["headers"] = map[string]string{"If-Match": "private-header"}
		if unknown {
			p.err = &providers.Error{Code: "execution_unknown", Unknown: true}
		}
		result, err := s.Call(context.Background(), caller, "service_request", args)
		id := ""
		if unknown {
			if err == nil {
				t.Fatal("unknown outcome lost")
			}
			id = err.(*ToolError).CorrelationID
		} else {
			if err != nil {
				t.Fatal(err)
			}
			id = result["correlation_id"].(string)
		}
		if len(b.correlations) != 2 || b.correlations[0] != id || b.correlations[1] != id {
			t.Fatal("Broker calls did not share correlation")
		}
		for _, expected := range []string{`"method":"POST"`, `"path":"/providers/Microsoft.ResourceGraph/resources"`, `"duration_ms":`, `"phase":"started"`, `"phase":"completed"`, `"executor_account_id":"00000000-0000-0000-0000-000000000002"`} {
			if !strings.Contains(log.String(), expected) {
				t.Fatalf("audit missing %s", expected)
			}
		}
		for _, private := range []string{"private-query", "private-header", "private-body", "synthetic-private-token", "private-workload-proof"} {
			if strings.Contains(log.String(), private) {
				t.Fatalf("audit leaked %s", private)
			}
		}
		if unknown && !strings.Contains(log.String(), `"outcome":"execution_unknown"`) {
			t.Fatal("unknown outcome unaudited")
		}
	}
}
