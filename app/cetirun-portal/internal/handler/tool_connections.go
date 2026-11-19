// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const (
	connectionBindingsReadOnly = "X-CetiRun-Connection-Bindings-Read-Only"
	maxToolBrokerResponseBytes = 1 << 20
	maxToolRequestBytes        = 64 << 10
	connectionBindingLifetime  = 10 * time.Minute
	// Preserve the cookie prefix for in-flight GitHub authorizations during upgrades.
	connectionBindingCookiePrefix = "__Host-cetirun-github-oauth-"
	connectionBindingHeader       = "X-CetiRun-Connection-Binding"
)

// ToolConnectionsHandler exposes owner-scoped connection and binding management.
// It never receives or returns provider credentials; it only proxies bounded,
// validated control-plane requests to the protected Broker boundary.
type ToolConnectionsHandler struct {
	Kube          *kube.Client
	BrokerBaseURL string
	PortalOrigin  string
	HTTPClient    *http.Client
	Now           func() time.Time
	RuntimeTypes  []string
}

func NewToolConnectionsHandler(kubeClient *kube.Client, brokerBaseURL, portalOrigin string) *ToolConnectionsHandler {
	return &ToolConnectionsHandler{
		Kube:          kubeClient,
		BrokerBaseURL: strings.TrimRight(brokerBaseURL, "/"),
		PortalOrigin:  strings.TrimRight(portalOrigin, "/"),
		HTTPClient:    &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Now:           time.Now,
	}
}

func (h *ToolConnectionsHandler) ListConnections(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodGet, "/v1/connections", headers, nil)
}

func (h *ToolConnectionsHandler) ConnectionCallback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxToolRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid connection callback")
		return
	}
	form := url.Values{}
	for _, field := range []string{"state", "code", "error"} {
		if values := r.Form[field]; len(values) == 1 && len(values[0]) <= 8192 {
			form.Set(field, values[0])
		}
	}
	state := form.Get("state")
	cookieName, validState := connectionBindingCookieName(state)
	binding := ""
	if validState {
		if cookie, err := r.Cookie(cookieName); err == nil && validConnectionBinding(cookie.Value) {
			binding = cookie.Value
		}
		clearConnectionBindingCookie(w, cookieName)
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.BrokerBaseURL+"/api/v1/connections/callback", strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection callback unavailable")
		return
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if binding != "" {
		request.Header.Set(connectionBindingHeader, binding)
	}
	response, err := h.HTTPClient.Do(request)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection callback unavailable")
		return
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxToolBrokerResponseBytes+1))
	if err != nil || len(payload) > maxToolBrokerResponseBytes || response.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, "dependency_invalid", "Invalid connection callback response")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}

func (h *ToolConnectionsHandler) StartConnection(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.writeOwnerHeaders(w, r)
	if !ok {
		return
	}
	body, provider, ok := readConnectionStart(w, r)
	if !ok {
		return
	}
	binding := ""
	if provider == "github" || provider == "google" {
		var err error
		binding, err = newConnectionBinding()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection setup unavailable")
			return
		}
		headers[connectionBindingHeader] = binding
	}
	h.forwardConnectionAuthorization(w, r, "/v1/connections/start", headers, body, binding, true)
}

// GitHubAppConfig reports whether the shared GitHub App has been configured.
// It is owner-scoped because its install URL is used from an authenticated
// connection workflow.
func (h *ToolConnectionsHandler) GitHubAppConfig(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodGet, "/v1/connections/github-app/config", headers, nil)
}

func (h *ToolConnectionsHandler) ListGitHubInstallations(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodGet, "/v1/connections/"+url.PathEscape(r.PathValue("id"))+"/github/installations", headers, nil)
}

func (h *ToolConnectionsHandler) ListGitHubRepositories(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return
	}
	installationID, page, ok := readGitHubRepositoryPage(w, r)
	if !ok {
		return
	}
	path := fmt.Sprintf("/v1/connections/%s/github/repositories?installationId=%d&page=%d", url.PathEscape(r.PathValue("id")), installationID, page)
	h.forward(w, r, http.MethodGet, path, headers, nil)
}

func (h *ToolConnectionsHandler) SelectGitHubInstallation(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.writeOwnerHeaders(w, r)
	if !ok {
		return
	}
	body, ok := readGitHubInstallation(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodPost, "/v1/connections/"+url.PathEscape(r.PathValue("id"))+"/github/installation", headers, body)
}

// GitHubAppSetup is the optional GitHub App setup return page. Selection is
// always performed through authenticated owner-scoped APIs, so this endpoint
// intentionally ignores all query parameters and never contacts the Broker.
func (h *ToolConnectionsHandler) GitHubAppSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, "<!doctype html><title>GitHub App setup</title><p>You can close this window and return to CetiRun.</p><script>window.close()</script>")
}

func (h *ToolConnectionsHandler) GetConnection(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodGet, "/v1/connections/"+url.PathEscape(r.PathValue("id")), headers, nil)
}

func (h *ToolConnectionsHandler) GetConnectionAuthorization(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodGet, "/v1/connection-authorizations/"+url.PathEscape(r.PathValue("id")), headers, nil)
}

func (h *ToolConnectionsHandler) GetConnectionUsage(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodGet, "/v1/connections/"+url.PathEscape(r.PathValue("id"))+"/usage", headers, nil)
}

func (h *ToolConnectionsHandler) DeleteConnection(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.writeOwnerHeaders(w, r)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodDelete, "/v1/connections/"+url.PathEscape(r.PathValue("id")), headers, nil)
}

func (h *ToolConnectionsHandler) ReconnectConnection(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.writeOwnerHeaders(w, r)
	if !ok {
		return
	}
	binding, err := newConnectionBinding()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Connection setup unavailable")
		return
	}
	headers[connectionBindingHeader] = binding
	h.forwardConnectionAuthorization(w, r, "/v1/connections/"+url.PathEscape(r.PathValue("id"))+"/reconnect", headers, nil, binding, false)
}

func (h *ToolConnectionsHandler) ListConnectionBindings(w http.ResponseWriter, r *http.Request) {
	headers, readOnly, ok := h.instanceHeaders(w, r, false, true)
	if !ok {
		return
	}
	// Listing existing bindings remains available after a runtime change so
	// the owner can obtain each revision and revoke it. Mutations remain gated.
	if readOnly {
		w.Header().Set("X-CetiRun-Connection-Bindings-Read-Only", "true")
	}
	h.forward(w, r, http.MethodGet, "/v1/instances/"+url.PathEscape(r.PathValue("name"))+"/connection-bindings", headers, nil)
}

// CreateToolIdentity creates the provider binding for the immutable Agent UID.
// The provider principal is derived by the Broker from the Agent resource; the
// browser can only select the supported provider.
func (h *ToolConnectionsHandler) CreateToolIdentity(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.instanceOwnerHeaders(w, r, true, false)
	if !ok {
		return
	}
	body, ok := readToolIdentity(w, r)
	if !ok {
		return
	}
	path := "/v1/instances/" + url.PathEscape(r.PathValue("name")) + "/tool-identities"
	h.forward(w, r, http.MethodPost, path, headers, body)
}

func (h *ToolConnectionsHandler) CreateConnectionBinding(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.instanceOwnerHeaders(w, r, true, false)
	if !ok {
		return
	}
	body, _, ok := h.readBinding(w, r, false)
	if !ok {
		return
	}
	h.forward(w, r, http.MethodPost, "/v1/instances/"+url.PathEscape(r.PathValue("name"))+"/connection-bindings", headers, body)
}

func (h *ToolConnectionsHandler) UpdateConnectionBinding(w http.ResponseWriter, r *http.Request) {
	headers, ok := h.instanceOwnerHeaders(w, r, true, false)
	if !ok {
		return
	}
	body, revision, ok := h.readBinding(w, r, true)
	if !ok {
		return
	}
	headers["If-Match"] = revision
	path := fmt.Sprintf("/v1/instances/%s/connection-bindings/%s", url.PathEscape(r.PathValue("name")), url.PathEscape(r.PathValue("id")))
	h.forward(w, r, http.MethodPut, path, headers, body)
}

func (h *ToolConnectionsHandler) DeleteConnectionBinding(w http.ResponseWriter, r *http.Request) {
	// Keep stale bindings removable after an Agent moves to a Sandbox runtime.
	headers, ok := h.instanceOwnerHeaders(w, r, true, true)
	if !ok {
		return
	}
	revision := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if revision == "" {
		writeError(w, http.StatusPreconditionRequired, "revision_required", "Binding revision is required for revocation")
		return
	}
	headers["If-Match"] = revision
	path := fmt.Sprintf("/v1/instances/%s/connection-bindings/%s", url.PathEscape(r.PathValue("name")), url.PathEscape(r.PathValue("id")))
	h.forward(w, r, http.MethodDelete, path, headers, nil)
}

func (h *ToolConnectionsHandler) ownerHeaders(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok || claims.TenantID == "" || claims.ObjectID == "" {
		writeError(w, http.StatusForbidden, "verified_owner_required", "Verified Portal identity is required")
		return nil, false
	}
	return map[string]string{
		"X-CetiRun-Owner-Tenant-ID": claims.TenantID,
		"X-CetiRun-Owner-Object-ID": claims.ObjectID,
	}, true
}

func (h *ToolConnectionsHandler) writeOwnerHeaders(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	if !h.validOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid_origin", "Request origin is not allowed")
		return nil, false
	}
	return h.ownerHeaders(w, r)
}

func (h *ToolConnectionsHandler) instanceOwnerHeaders(w http.ResponseWriter, r *http.Request, write, allowReadOnly bool) (map[string]string, bool) {
	headers, _, ok := h.instanceHeaders(w, r, write, allowReadOnly)
	return headers, ok
}

func (h *ToolConnectionsHandler) instanceHeaders(w http.ResponseWriter, r *http.Request, write, allowReadOnly bool) (map[string]string, bool, bool) {
	if write && !h.validOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid_origin", "Request origin is not allowed")
		return nil, false, false
	}
	headers, ok := h.ownerHeaders(w, r)
	if !ok {
		return nil, false, false
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance_not_found", "Instance not found")
		return nil, false, false
	}
	if !strings.EqualFold(instance.Annotations[cetirunv1alpha1.AnnotationOwnerTenantID], headers["X-CetiRun-Owner-Tenant-ID"]) ||
		!strings.EqualFold(instance.Annotations[cetirunv1alpha1.AnnotationOwnerObjectID], headers["X-CetiRun-Owner-Object-ID"]) {
		// Keep ownership non-enumerable, including for Portal administrators.
		writeError(w, http.StatusNotFound, "instance_not_found", "Instance not found")
		return nil, false, false
	}
	readOnly := !h.runtimeSupported(instance)
	if readOnly && !allowReadOnly {
		if sandboxRequested(instance) {
			writeError(w, http.StatusBadRequest, "unsupported_sandbox", "This Sandbox runtime is not enabled for CetiRun Connections MCP in this deployment")
		} else {
			writeError(w, http.StatusBadRequest, "unsupported_agent_type", "CetiRun Connections MCP is not qualified for this runtime in this deployment")
		}
		return nil, false, false
	}
	headers["X-CetiRun-Instance-UID"] = string(instance.UID)
	headers["X-CetiRun-Instance-Name"] = instance.Name
	return headers, readOnly, true
}

func (h *ToolConnectionsHandler) runtimeSupported(instance *cetirunv1alpha1.Agent) bool {
	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom {
		if instance.Spec.SandboxRef == "" {
			return false
		}
		for _, runtimeType := range h.RuntimeTypes {
			if runtimeType == "custom" {
				return true
			}
		}
		return false
	}
	if instance.Spec.Agent.Type != cetirunv1alpha1.AgentTypeHermes && instance.Spec.Agent.Type != cetirunv1alpha1.AgentTypeOpenClaw {
		return false
	}
	// Policy-managed native workloads receive Connections only through an
	// explicit deployment allowlist. The Broker independently verifies their
	// native workload identity before each mutation. Legacy native workloads
	// retain the previous nil-allowlist compatibility behavior.
	if sandboxRequested(instance) && h.RuntimeTypes == nil {
		return false
	}
	if h.RuntimeTypes == nil {
		return true
	}
	for _, runtimeType := range h.RuntimeTypes {
		if runtimeType == string(instance.Spec.Agent.Type) && (runtimeType == "hermes" || runtimeType == "openclaw") {
			return true
		}
	}
	return false
}

// sandboxRequested mirrors the persisted Agent signals the Broker uses to
// classify a workload as policy-managed. SandboxRef alone is insufficient:
// policy selection and recovery can leave durable status, finalizer, or
// authorization signals after the requested reference is no longer present.
func sandboxRequested(instance *cetirunv1alpha1.Agent) bool {
	for _, key := range []string{
		cetirunv1alpha1.AnnotationSandboxAuthorization,
		cetirunv1alpha1.AnnotationSandboxReauthorize,
	} {
		if _, present := instance.Annotations[key]; present {
			return true
		}
	}
	for _, finalizer := range instance.Finalizers {
		switch finalizer {
		case cetirunv1alpha1.SandboxHoldFinalizer,
			cetirunv1alpha1.SandboxPreparationFinalizer,
			cetirunv1alpha1.SandboxBootstrapFinalizer:
			return true
		}
	}
	if instance.Spec.SandboxRef != "" ||
		(instance.Spec.ModelAccess != nil && instance.Spec.ModelAccess.MonthlyBudgetUSD != "") ||
		instance.Status.Policy != nil || instance.Status.Sandbox != nil ||
		(instance.Status.AIGateway != nil && instance.Status.AIGateway.PolicyRevision != "") {
		return true
	}
	for _, condition := range instance.Status.Conditions {
		if condition.Type == cetirunv1alpha1.ConditionSandboxReady {
			return true
		}
	}
	return false
}

type connectionStartRequest struct {
	Provider string `json:"provider"`
	Mode     string `json:"mode,omitempty"`
}

type toolIdentityRequest struct {
	Provider string `json:"provider"`
}

func readToolIdentity(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var request toolIdentityRequest
	if !decodeJSON(w, r, &request) {
		return nil, false
	}
	if request.Provider != "azure" {
		writeError(w, http.StatusBadRequest, "unsupported_identity_provider", "Independent Agent identity currently supports only Azure")
		return nil, false
	}
	body, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Tool identity request is invalid")
		return nil, false
	}
	return body, true
}

func readConnectionStart(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	var request connectionStartRequest
	if !decodeJSON(w, r, &request) {
		return nil, "", false
	}
	if request.Provider != "azure" && request.Provider != "microsoft" && request.Provider != "github" && request.Provider != "google" {
		writeError(w, http.StatusBadRequest, "invalid_provider", "Provider must be azure, microsoft, github, or google")
		return nil, "", false
	}
	if request.Mode != "" && !(request.Provider == "github" && request.Mode == "installation") {
		writeError(w, http.StatusBadRequest, "invalid_mode", "GitHub installation mode is the only supported connection mode")
		return nil, "", false
	}
	body, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Connection request is invalid")
		return nil, "", false
	}
	return body, request.Provider, true
}

type connectionBindingRequest struct {
	ConnectionID string                `json:"connectionId"`
	Services     []string              `json:"services"`
	Enabled      bool                  `json:"enabled"`
	Revision     uint64                `json:"revision,omitempty"`
	GitHub       *githubBindingRequest `json:"github,omitempty"`
}

type githubBindingRequest struct {
	RepositoryIDs []int64                  `json:"repositoryIds"`
	Permissions   githubPermissionsRequest `json:"permissions"`
}

type githubPermissionsRequest struct {
	Contents     string `json:"contents,omitempty"`
	PullRequests string `json:"pull_requests,omitempty"`
	Issues       string `json:"issues,omitempty"`
}

type githubInstallationRequest struct {
	InstallationID int64 `json:"installationId"`
}

func (h *ToolConnectionsHandler) readBinding(w http.ResponseWriter, r *http.Request, requireRevision bool) ([]byte, string, bool) {
	var request connectionBindingRequest
	if !decodeJSON(w, r, &request) {
		return nil, "", false
	}
	if !request.Enabled || request.ConnectionID == "" || len(request.ConnectionID) > 128 || len(request.Services) == 0 || len(request.Services) > 5 {
		writeError(w, 400, "invalid_request", "Connection and services are required")
		return nil, "", false
	}
	seen := map[string]bool{}
	for _, id := range request.Services {
		if _, ok := agenttools.LookupService(id); !ok || seen[id] {
			writeError(w, 400, "invalid_request", "Unknown or duplicate service")
			return nil, "", false
		}
		seen[id] = true
	}
	if request.GitHub != nil && !validGitHubBinding(request.GitHub, request.Services) {
		writeError(w, http.StatusBadRequest, "invalid_github_binding", "GitHub bindings require 1 to 500 unique repositories and at least one read or write permission")
		return nil, "", false
	}
	if requireRevision && request.Revision == 0 {
		writeError(w, 400, "revision_required", "Binding revision is required for updates")
		return nil, "", false
	}
	body, err := json.Marshal(request)
	if err != nil {
		writeError(w, 400, "invalid_request", "Invalid connection binding")
		return nil, "", false
	}
	return body, strconv.FormatUint(request.Revision, 10), true
}

func validGitHubBinding(binding *githubBindingRequest, services []string) bool {
	if len(services) != 1 || services[0] != "github" || len(binding.RepositoryIDs) == 0 || len(binding.RepositoryIDs) > 500 {
		return false
	}
	seen := make(map[int64]struct{}, len(binding.RepositoryIDs))
	for _, id := range binding.RepositoryIDs {
		if id <= 0 {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		seen[id] = struct{}{}
	}
	permissions := []string{binding.Permissions.Contents, binding.Permissions.PullRequests, binding.Permissions.Issues}
	granted := false
	for _, permission := range permissions {
		if permission == "" {
			continue
		}
		if permission != "read" && permission != "write" {
			return false
		}
		granted = true
	}
	return granted
}

func readGitHubInstallation(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var request githubInstallationRequest
	if !decodeJSON(w, r, &request) {
		return nil, false
	}
	if request.InstallationID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_installation", "GitHub installation is required")
		return nil, false
	}
	body, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "GitHub installation request is invalid")
		return nil, false
	}
	return body, true
}

func readGitHubRepositoryPage(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	query := r.URL.Query()
	if len(query["installationId"]) != 1 || len(query["page"]) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "GitHub installation and page are required")
		return 0, 0, false
	}
	installationID, err := strconv.ParseInt(query.Get("installationId"), 10, 64)
	if err != nil || installationID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_installation", "GitHub installation is invalid")
		return 0, 0, false
	}
	page, err := strconv.Atoi(query.Get("page"))
	if err != nil || page < 1 || page > 10000 {
		writeError(w, http.StatusBadRequest, "invalid_page", "GitHub repository page is invalid")
		return 0, 0, false
	}
	return installationID, page, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "Request content type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxToolRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Request body is invalid")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "Request body is invalid")
		return false
	}
	return true
}

type connectionAuthorizationResponse struct {
	AuthorizationID        string          `json:"authorizationId"`
	ID                     string          `json:"id"`
	ConnectionID           string          `json:"connectionId"`
	AuthorizationURL       string          `json:"authorizationUrl"`
	ExpiresAt              json.RawMessage `json:"expiresAt"`
	BrowserBindingRequired bool            `json:"browserBindingRequired"`
}

// forwardConnectionAuthorization keeps browser-bound personal OAuth proof out
// of JSON and binds it to the callback state using an HttpOnly host cookie.
func (h *ToolConnectionsHandler) forwardConnectionAuthorization(w http.ResponseWriter, incoming *http.Request, path string, headers map[string]string, body []byte, binding string, normalizeStart bool) {
	h.forwardWithConnectionAuthorization(w, incoming, http.MethodPost, path, headers, body, binding, normalizeStart)
}

func (h *ToolConnectionsHandler) forward(w http.ResponseWriter, incoming *http.Request, method, path string, headers map[string]string, body []byte) {
	h.forwardWithConnectionAuthorization(w, incoming, method, path, headers, body, "", false)
}

func (h *ToolConnectionsHandler) forwardWithConnectionAuthorization(w http.ResponseWriter, incoming *http.Request, method, path string, headers map[string]string, body []byte, binding string, normalizeStart bool) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(incoming.Context(), method, h.BrokerBaseURL+path, reader)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Agent Tools service is unavailable")
		return
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		slog.Warn("Agent Tools Broker request failed", "path", path, "error", err)
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", "Agent Tools service is unavailable")
		return
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxToolBrokerResponseBytes+1))
	if err != nil || len(payload) > maxToolBrokerResponseBytes {
		writeError(w, http.StatusBadGateway, "dependency_invalid", "Agent Tools service returned an invalid response")
		return
	}
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		writeError(w, http.StatusBadGateway, "dependency_invalid", "Agent Tools service returned an invalid response")
		return
	}
	if method == http.MethodPost && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && (normalizeStart || binding != "") {
		var authorization connectionAuthorizationResponse
		if err := json.Unmarshal(payload, &authorization); err != nil || authorization.AuthorizationURL == "" || len(authorization.ExpiresAt) == 0 || bytes.Equal(authorization.ExpiresAt, []byte("null")) {
			writeError(w, http.StatusBadGateway, "dependency_invalid", "Agent Tools service returned an invalid response")
			return
		}
		if authorization.ConnectionID == "" {
			authorization.ConnectionID = authorization.ID
		}
		if authorization.AuthorizationID == "" && authorization.ConnectionID == "" {
			writeError(w, http.StatusBadGateway, "dependency_invalid", "Agent Tools service returned an invalid response")
			return
		}
		if authorization.BrowserBindingRequired {
			if !validConnectionBinding(binding) || !h.setConnectionBindingCookie(w, authorization.AuthorizationURL, authorization.ExpiresAt, binding) {
				writeError(w, http.StatusBadGateway, "dependency_invalid", "Agent Tools service returned an invalid response")
				return
			}
		}
		// The initiating window must verify this owner-scoped transaction.
		payload, err = json.Marshal(struct {
			AuthorizationID  string          `json:"authorizationId,omitempty"`
			ConnectionID     string          `json:"connectionId,omitempty"`
			AuthorizationURL string          `json:"authorizationUrl"`
			ExpiresAt        json.RawMessage `json:"expiresAt"`
		}{authorization.AuthorizationID, authorization.ConnectionID, authorization.AuthorizationURL, authorization.ExpiresAt})
		if err != nil {
			writeError(w, 502, "dependency_invalid", "Invalid authorization response")
			return
		}
	}
	for _, key := range []string{"Content-Type", "Cache-Control", "Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options"} {
		if value := resp.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(payload)
}

func newConnectionBinding() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func validConnectionBinding(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func connectionBindingCookieName(state string) (string, bool) {
	if !validConnectionBinding(state) {
		return "", false
	}
	sum := sha256.Sum256([]byte(state))
	return connectionBindingCookiePrefix + base64.RawURLEncoding.EncodeToString(sum[:]), true
}

func connectionAuthorizationState(authorizationURL string) (string, bool) {
	parsed, err := url.ParseRequestURI(authorizationURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query["state"]) != 1 || !validConnectionBinding(query.Get("state")) {
		return "", false
	}
	return query.Get("state"), true
}

func (h *ToolConnectionsHandler) setConnectionBindingCookie(w http.ResponseWriter, authorizationURL string, expiresAt json.RawMessage, binding string) bool {
	state, ok := connectionAuthorizationState(authorizationURL)
	if !ok {
		return false
	}
	var expiryText string
	if json.Unmarshal(expiresAt, &expiryText) != nil {
		return false
	}
	expiresAtTime, err := time.Parse(time.RFC3339, expiryText)
	if err != nil {
		return false
	}
	lifetime := expiresAtTime.Sub(h.now())
	if lifetime <= 0 || lifetime > connectionBindingLifetime {
		return false
	}
	seconds := int((lifetime + time.Second - 1) / time.Second)
	if seconds < 1 || seconds > int(connectionBindingLifetime/time.Second) {
		return false
	}
	cookieName, _ := connectionBindingCookieName(state)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    binding,
		Path:     "/",
		Expires:  expiresAtTime.UTC(),
		MaxAge:   seconds,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return true
}

func clearConnectionBindingCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *ToolConnectionsHandler) validOrigin(r *http.Request) bool {
	origin := strings.TrimRight(r.Header.Get("Origin"), "/")
	return h.PortalOrigin != "" && origin == h.PortalOrigin
}

func (h *ToolConnectionsHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
