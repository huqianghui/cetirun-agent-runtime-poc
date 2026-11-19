// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/brokerclient"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/identity"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/tools/providers"
	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func TestCredentialPolicyRevisionIsRequiredAndPinnedThroughIssuance(t *testing.T) {
	revision := "sha256:" + strings.Repeat("1", 64)
	for _, mode := range []string{"agent", "delegated"} {
		for _, state := range []string{"current", "missing-status", "missing-policy", "missing-check", "stale-check", "changed-issue", "removed-issue", "recreated-record", "rewritten-record"} {
			t.Run(mode+"/"+state, func(t *testing.T) {
				service, broker, provider, caller, _ := serviceFixture()
				caller.PolicyManaged, caller.CredentialPolicyRevision = true, revision
				broker.check.CredentialPolicy = &agenttools.CredentialPolicyReference{Revision: revision, RecordUID: "record-uid", ResourceVersion: "12"}
				broker.check.Connection.Mode = mode
				if mode == "delegated" {
					broker.check.Connection.Identity = nil
				}
				switch state {
				case "missing-status":
					caller.CredentialPolicyRevision = ""
				case "missing-policy":
					caller.PolicyManaged = false
				case "missing-check":
					broker.check.CredentialPolicy = nil
				case "stale-check":
					broker.check.CredentialPolicy.Revision = "sha256:" + strings.Repeat("2", 64)
				case "changed-issue":
					broker.issueChange = func(r *brokerclient.IssueResponse) { r.CredentialPolicy.Revision = "sha256:" + strings.Repeat("2", 64) }
				case "removed-issue":
					broker.issueChange = func(r *brokerclient.IssueResponse) { r.CredentialPolicy = nil }
				case "recreated-record":
					broker.issueChange = func(r *brokerclient.IssueResponse) { r.CredentialPolicy.RecordUID = "replacement-uid" }
				case "rewritten-record":
					broker.issueChange = func(r *brokerclient.IssueResponse) { r.CredentialPolicy.ResourceVersion = "13" }
				}
				result, err := service.Call(context.Background(), caller, "service_request", arguments())
				if state == "current" {
					if err != nil || result["status"] != 403 || provider.calls != 1 {
						t.Fatalf("current policy did not reach the synthetic provider: %v", err)
					}
				} else if err == nil || provider.calls != 0 {
					t.Fatal("missing or stale policy reached the provider")
				}
			})
		}
	}
}

func TestCredentialPolicyFixedServiceDoesNotBroadenToGraph(t *testing.T) {
	service, broker, provider, caller, _ := serviceFixture()
	caller.PolicyManaged, caller.CredentialPolicyRevision = true, "sha256:"+strings.Repeat("1", 64)
	broker.check.CredentialPolicy = &agenttools.CredentialPolicyReference{Revision: caller.CredentialPolicyRevision, RecordUID: "record-uid", ResourceVersion: "12"}
	broker.check.Connection.Mode, broker.check.Connection.Provider, broker.check.Connection.Identity = "delegated", "microsoft", nil
	broker.check.Service = "microsoft_graph"
	broker.check.Connection.Services, broker.check.Binding.Services = []string{"microsoft_graph"}, []string{"microsoft_graph"}
	args := arguments()
	args["service"] = "microsoft_graph"
	if _, err := service.Call(context.Background(), caller, "service_request", args); err == nil || broker.issued != 0 || provider.calls != 0 {
		t.Fatal("ARM-only credential policy authorized Graph OBO")
	}
}

func TestCredentialPolicyDoesNotRetryUnknownProviderExecution(t *testing.T) {
	service, broker, provider, caller, _ := serviceFixture()
	caller.PolicyManaged, caller.CredentialPolicyRevision = true, "sha256:"+strings.Repeat("1", 64)
	broker.check.CredentialPolicy = &agenttools.CredentialPolicyReference{Revision: caller.CredentialPolicyRevision, RecordUID: "record-uid", ResourceVersion: "12"}
	provider.err = &providers.Error{Code: "execution_unknown", Unknown: true}
	_, err := service.Call(context.Background(), caller, "service_request", arguments())
	if err == nil || err.Error() != "execution_unknown" || broker.issued != 1 || provider.calls != 1 {
		t.Fatal("policy readback retried or hid an unknown provider outcome")
	}
}

func TestCredentialPolicyDoesNotTreatAgentControlledFieldsAsAuthorization(t *testing.T) {
	service, broker, provider, _, _ := serviceFixture()
	caller := identity.Caller{InstanceUID: broker.check.InstanceUID, Proof: "synthetic-proof", PolicyManaged: true}
	args := arguments()
	args["credentialPolicyRevision"] = "sha256:" + strings.Repeat("1", 64)
	if _, err := service.Call(context.Background(), caller, "service_request", args); err == nil || broker.issued != 0 || provider.calls != 0 {
		t.Fatal("caller-supplied credential policy bypassed the live Agent status")
	}
}

func TestCredentialPolicyPreservesIndependentProviderConnections(t *testing.T) {
	for _, profile := range agenttools.Services() {
		if agenttools.UsesAgentIdentity(profile.Provider) {
			continue
		}
		t.Run(profile.ID, func(t *testing.T) {
			service, broker, provider, caller, _ := serviceFixture()
			caller.PolicyManaged, caller.CredentialPolicyRevision = true, "sha256:"+strings.Repeat("1", 64)
			broker.check.Connection.Mode, broker.check.Connection.Provider, broker.check.Connection.Identity = "delegated", profile.Provider, nil
			broker.check.Service = profile.ID
			broker.check.Connection.Services, broker.check.Binding.Services = []string{profile.ID}, []string{profile.ID}
			args := arguments()
			args["service"], args["path"] = profile.ID, "/example"
			if _, err := service.Call(context.Background(), caller, "service_request", args); err != nil || provider.calls != 1 {
				t.Fatalf("unrelated provider authorization changed: %v", err)
			}
		})
	}
}

type credentialPolicyMetadataBroker struct {
	*fakeBroker
	metadata agenttools.ListResponse
}

func (b *credentialPolicyMetadataBroker) List(context.Context, string) (agenttools.ListResponse, error) {
	return b.metadata, nil
}

func TestCredentialPolicyFailureDoesNotHideIndependentMetadata(t *testing.T) {
	for _, code := range []string{"permission_denied", "authorization_changed", "dependency_unavailable"} {
		t.Run(code, func(t *testing.T) {
			service, broker, provider, caller, _ := serviceFixture()
			caller.PolicyManaged = true
			caller.CredentialPolicyRevision = ""
			broker.err = &brokerclient.Error{Code: code}
			metadata := agenttools.ListResponse{InstanceUID: caller.InstanceUID, ContractVersion: agenttools.ContractVersion}
			for _, profile := range agenttools.Services() {
				if agenttools.UsesAgentIdentity(profile.Provider) {
					continue
				}
				metadata.Connections = append(metadata.Connections, agenttools.Connection{
					ID: profile.ID, Provider: profile.Provider, Mode: "delegated",
					Services: []string{profile.ID}, Status: "connected", Revision: 1,
				})
				metadata.Bindings = append(metadata.Bindings, agenttools.Binding{
					ID: "binding-" + profile.ID, InstanceUID: caller.InstanceUID, ConnectionID: profile.ID,
					Services: []string{profile.ID}, Enabled: true, Revision: 1,
				})
			}
			service.broker = &credentialPolicyMetadataBroker{fakeBroker: broker, metadata: metadata}
			if _, err := service.List(context.Background(), caller); err != nil {
				t.Fatalf("catalog listing failed: %v", err)
			}
			status, err := service.Call(context.Background(), caller, "connections_status", map[string]any{})
			if err != nil || len(status["connections"].([]agenttools.Connection)) != 3 {
				t.Fatalf("GitHub/Google metadata was hidden: %v", err)
			}
			services, err := service.Call(context.Background(), caller, "services_list", map[string]any{})
			if err != nil || len(services["services"].([]any)) != 3 {
				t.Fatalf("independent service metadata was hidden: %v", err)
			}
			if _, err := service.Call(context.Background(), caller, "service_request", arguments()); err == nil ||
				err.Error() != code || broker.issued != 0 || provider.calls != 0 {
				t.Fatal("metadata availability allowed ARM credential execution")
			}
		})
	}
}
