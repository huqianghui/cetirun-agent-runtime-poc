// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/brokerclient"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/identity"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/tools/providers"
	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type Service struct {
	kubernetes interface {
		Authenticate(context.Context, string) (identity.Caller, error)
	}
	broker    brokerClient
	portalURL string
	providers providerClient
	logger    *slog.Logger
}
type brokerClient interface {
	List(context.Context, string) (brokerclient.ListResponse, error)
	Check(context.Context, brokerclient.Request) (brokerclient.CheckResponse, error)
	Issue(context.Context, brokerclient.Request) (brokerclient.IssueResponse, error)
}
type providerClient interface {
	Execute(context.Context, providers.Request, string, string) (providers.Result, error)
}
type ToolError struct {
	Code           string
	CorrelationID  string
	ProviderStatus int
	RequestIDs     []string
}

func (e *ToolError) Error() string { return e.Code }
func New(kubernetes *identity.Kubernetes, broker *brokerclient.Client, cfg config.Config) *Service {
	portal := ""
	if cfg.PortalBaseURL != nil {
		portal = cfg.PortalBaseURL.String()
	}
	return &Service{kubernetes: kubernetes, broker: broker, portalURL: portal, providers: providers.New(nil)}
}
func (s *Service) Authenticate(ctx context.Context, bearer string) (identity.Caller, error) {
	return s.kubernetes.Authenticate(ctx, bearer)
}
func (s *Service) List(ctx context.Context, caller identity.Caller) ([]Spec, error) {
	result, err := s.broker.List(ctx, caller.Proof)
	if err != nil {
		return nil, brokerError(err)
	}
	if result.InstanceUID != caller.InstanceUID || result.ContractVersion != agenttools.ContractVersion {
		return nil, &ToolError{Code: "authorization_changed"}
	}
	// The catalog is stable; changes to bindings are visible on the next call.
	return Catalog(), nil
}
func (s *Service) Call(ctx context.Context, caller identity.Caller, tool string, arguments map[string]any) (result map[string]any, err error) {
	correlation, randomErr := NewCorrelationID()
	if randomErr != nil {
		return nil, &ToolError{Code: "dependency_unavailable"}
	}
	ctx = agenttools.WithCorrelation(ctx, correlation)
	started := time.Now()
	var check brokerclient.CheckResponse
	service, method, targetPath := "", "", ""
	status := 0
	stage := "platform"
	requestIDs := []string{}
	logEvent := func(phase, outcome string) {
		attributes := []any{"agent_uid", caller.InstanceUID, "tool", tool, "service", service, "correlation_id", correlation, "stage", stage, "phase", phase, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds()}
		if method != "" {
			attributes = append(attributes, "method", method, "path", targetPath)
		}
		if check.Connection.ID != "" {
			attributes = append(attributes, "connection_id", agenttools.AuditText(check.Connection.ID, 128), "connection_revision", check.Connection.Revision, "binding_id", agenttools.AuditText(check.Binding.ID, 128), "binding_revision", check.Binding.Revision, "mode", check.Connection.Mode, "executor_account", agenttools.AuditText(check.Connection.AccountDisplay, 256), "executor_account_id", agenttools.AuditText(check.ExecutorAccountID, 256))
			if check.Connection.Identity != nil {
				attributes = append(attributes, "executor_principal", check.Connection.Identity.PrincipalID)
			}
			if check.CredentialPolicy != nil {
				attributes = append(attributes, "credential_policy_revision", agenttools.AuditText(check.CredentialPolicy.Revision, 80))
			}
		}
		if status != 0 {
			attributes = append(attributes, "provider_status", status, "provider_request_ids", requestIDs)
		}
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Info("agent service request", attributes...)
	}
	defer func() {
		outcome := "succeeded"
		if status >= 400 {
			outcome = "provider_error"
		}
		if err != nil {
			outcome = err.Error()
			if typed, ok := err.(*ToolError); ok {
				typed.CorrelationID = correlation
			}
		}
		logEvent("completed", outcome)
	}()
	if err = ValidateToolArguments(tool, arguments); err != nil {
		return nil, &ToolError{Code: "invalid_request"}
	}
	if tool == "connections_start_link" {
		if s.portalURL == "" {
			return nil, &ToolError{Code: "dependency_unavailable"}
		}
		return map[string]any{"actionUrl": s.portalURL}, nil
	}
	if tool == "connections_status" || tool == "services_list" {
		listed, e := s.broker.List(ctx, caller.Proof)
		if e != nil {
			return nil, brokerError(e)
		}
		if listed.InstanceUID != caller.InstanceUID || listed.ContractVersion != agenttools.ContractVersion {
			return nil, &ToolError{Code: "authorization_changed"}
		}
		if tool == "connections_status" {
			return map[string]any{"connections": listed.Connections, "bindings": listed.Bindings}, nil
		}
		services := []any{}
		for _, b := range listed.Bindings {
			if !b.Enabled || b.InstanceUID != caller.InstanceUID {
				continue
			}
			for _, c := range listed.Connections {
				if c.ID != b.ConnectionID {
					continue
				}
				for _, id := range b.Services {
					p, ok := agenttools.LookupService(id)
					if ok && p.Provider == c.Provider && contains(c.Services, id) {
						services = append(services, map[string]any{"connection_id": c.ID, "service": p.ID, "origin": p.Origin, "base_path": p.BasePath, "status": c.Status})
					}
				}
			}
		}
		return map[string]any{"services": services}, nil
	}
	input, e := providers.Decode(arguments)
	if e != nil {
		return nil, &ToolError{Code: "invalid_request"}
	}
	service, method = input.Service, input.Method
	target, _ := providers.Target(input)
	// Escaping can expand each validated path byte to three ASCII characters.
	targetPath = agenttools.AuditText(target.EscapedPath(), 32768)
	logEvent("started", "pending")
	check, e = s.broker.Check(ctx, brokerclient.NewRequest(caller.Proof, service, input.ConnectionID, nil, nil))
	if e != nil {
		return nil, brokerError(e)
	}
	if !validAuthorization(check, caller, service, input.ConnectionID) {
		return nil, &ToolError{Code: "authorization_changed"}
	}
	issueRequest := brokerclient.NewRequest(caller.Proof, service, input.ConnectionID, &check.Binding.Revision, &check.Connection.Revision)
	issueRequest.CredentialPolicy = check.CredentialPolicy
	issued, e := s.broker.Issue(ctx, issueRequest)
	if e != nil {
		return nil, brokerError(e)
	}
	if !validAuthorization(issued.CheckResponse, caller, service, input.ConnectionID) || !reflect.DeepEqual(check, issued.CheckResponse) || strings.TrimSpace(issued.AccessToken) == "" || expired(issued.ExpiresAt) {
		return nil, &ToolError{Code: "authorization_changed"}
	}
	stage = "provider"
	logEvent("started", "pending")
	response, e := s.providers.Execute(ctx, input, issued.AccessToken, correlation)
	status = response.Status
	requestIDs = response.RequestIDs
	if e != nil {
		if typed, ok := e.(*providers.Error); ok {
			return nil, &ToolError{Code: typed.Code, ProviderStatus: response.Status, RequestIDs: response.RequestIDs}
		}
		return nil, &ToolError{Code: "execution_unknown"}
	}
	// Upstream errors are returned as provider results, preserving HTTP status and
	// JSON details. They are never confused with platform binding rejections.
	return map[string]any{"status": response.Status, "body": response.Body, "headers": response.Headers, "request_ids": response.RequestIDs, "correlation_id": correlation}, nil
}
func brokerError(err error) error {
	if typed, ok := err.(*brokerclient.Error); ok {
		return &ToolError{Code: typed.Code}
	}
	return &ToolError{Code: "dependency_unavailable"}
}
func validAuthorization(check brokerclient.CheckResponse, caller identity.Caller, service, connectionID string) bool {
	c, b := check.Connection, check.Binding
	if agenttools.UsesAgentIdentity(c.Provider) && (caller.PolicyManaged || caller.CredentialPolicyRevision != "") ||
		check.CredentialPolicy != nil {
		if !caller.PolicyManaged || !agenttools.UsesAgentIdentity(c.Provider) || service != "azure_arm" ||
			!agenttools.ValidCredentialPolicyRevision(caller.CredentialPolicyRevision) ||
			!check.CredentialPolicy.Valid() || check.CredentialPolicy.Revision != caller.CredentialPolicyRevision {
			return false
		}
	}
	p, ok := agenttools.LookupService(service)
	if !ok || check.ContractVersion != agenttools.ContractVersion || check.InstanceUID != caller.InstanceUID || check.Service != service || c.ID != connectionID || c.Status != "connected" || c.Revision == 0 || c.Provider != p.Provider || !contains(c.Services, service) || b.ID == "" || b.Revision == 0 || !b.Enabled || b.InstanceUID != caller.InstanceUID || b.ConnectionID != c.ID || !contains(b.Services, service) {
		return false
	}
	switch c.Mode {
	case "delegated":
		return c.Identity == nil && c.GitHub == nil && b.GitHub == nil
	case "installation":
		return c.Provider == "github" && c.Identity == nil && c.GitHub != nil && c.GitHub.InstallationID > 0 && c.GitHub.AccountLogin != "" && c.GitHub.UserLogin != "" && b.GitHub.Valid()
	case "agent":
		return c.Provider == "azure" && c.GitHub == nil && b.GitHub == nil && c.Identity != nil && c.Identity.InstanceUID == caller.InstanceUID && c.Identity.TenantID != "" && c.Identity.PrincipalID != "" && c.Identity.ClientID != ""
	default:
		return false
	}
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func expired(value string) bool {
	expiry, err := time.Parse(time.RFC3339, value)
	return err != nil || !expiry.After(time.Now().Add(5*time.Second))
}

// NewCorrelationID creates a tracing ID. It does not deduplicate execution.
func NewCorrelationID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(data)
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:], nil
}
