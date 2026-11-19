// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/copilot"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// validInstanceNameSuffix matches a valid user-supplied instance name suffix:
// starts with a lowercase letter, ends with a lowercase letter or digit, only
// lowercase letters, digits, and hyphens in between.
var validInstanceNameSuffix = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$`)

// maxInstanceNameLength is the longest instance name whose agent can start.
// The operator names the agent StatefulSet "openclaw-<instance>-cetirun", 17
// characters longer than the instance name, and Kubernetes labels each of its
// pods with controller-revision-hash=<StatefulSet name>-<hash of up to 10
// characters>. A label value holds at most 63 characters, so the StatefulSet
// name has to fit in 52 and the instance name in 35. Longer names produce an
// agent whose pod is never created, and from 45 characters Helm rejects the
// release name. The limit also keeps the namespace "cetirun-<instance>" within
// the 63 characters of a DNS label.
const maxInstanceNameLength = 35

// reservedInstanceNames are names that collide with one of the gateway-level
// path tokens. The collision surfaces are:
//
//   - EnvoyFilter gateway path tokens
//     (infra/istio/templates/ingress/envoy-filter-alias.yaml): exact Portal paths and
//     complete path-segment subtrees skip the ownership check. An instance using one
//     of the same bare names would collide with that Portal route (oauth2, api,
//     internal, healthz, readyz, assets, instances, admin, connections).
//   - Portal SPA top-level routes (app/cetirun-portal/web/src/App.tsx): the portal
//     serves these paths itself, so an instance with that name would be unreachable
//     via the workspace URL (instances, admin, connections).
//   - Native agent UI namespace (ui): native Agent UIs are served below
//     /ui/<instance>, so this token cannot be an instance name.
//   - Portal hostname/subdomain (portal): reserved to keep the portal's own DNS
//     name from being shadowed by an instance entry.
//
// Keep this list aligned with the three sources above when any of them change.
var reservedInstanceNames = map[string]struct{}{
	"oauth2":      {},
	"api":         {},
	"internal":    {},
	"healthz":     {},
	"readyz":      {},
	"assets":      {},
	"portal":      {},
	"instances":   {},
	"connections": {},
	"admin":       {},
	"ui":          {},
}

// CreateInstanceRequest is the JSON body for POST /api/v1/instance.
type CreateInstanceRequest struct {
	InstanceName         string                    `json:"instanceName,omitempty"`
	Agent                *agentconfig.Input        `json:"agent"`
	RegistryAuth         *registryauth.Credentials `json:"registryAuth,omitempty"`
	DisplayName          string                    `json:"displayName,omitempty"`
	Team                 string                    `json:"team"`
	GatewayToken         string                    `json:"gatewayToken,omitempty"`
	TailscaleAuthKey     string                    `json:"tailscaleAuthKey,omitempty"`
	Model                string                    `json:"model,omitempty"`
	AgentIdentityEnabled bool                      `json:"agentIdentityEnabled,omitempty"`
	// ModelAccess selects Gateway model sources for every Agent harness.
	ModelAccess *ModelAccessRequest `json:"modelAccess,omitempty"`
}

// InstanceResponse is the JSON response for GET /api/v1/instance.
type InstanceResponse struct {
	Alias                       string                      `json:"alias"`
	InstanceName                string                      `json:"instanceName"`
	Agent                       cetirunv1alpha1.HarnessSpec `json:"agent"`
	UID                         string                      `json:"uid"`
	ResourceVersion             string                      `json:"resourceVersion"`
	RegistryAuthConfigured      bool                        `json:"registryAuthConfigured"`
	Generation                  int64                       `json:"generation"`
	ObservedGeneration          int64                       `json:"observedGeneration"`
	Suspended                   bool                        `json:"suspended"`
	SuspendAvailable            bool                        `json:"suspendAvailable"`
	SuspensionUnavailableReason string                      `json:"suspensionUnavailableReason,omitempty"`
	ResumeAvailable             bool                        `json:"resumeAvailable"`
	RestartAvailable            bool                        `json:"restartAvailable"`
	RestartRequestID            string                      `json:"restartRequestId,omitempty"`
	ObservedRestartRequestID    string                      `json:"observedRestartRequestId,omitempty"`
	DisplayName                 string                      `json:"displayName"`
	Team                        string                      `json:"team"`
	Model                       string                      `json:"model,omitempty"`
	Phase                       string                      `json:"phase"`
	AccessURL                   string                      `json:"accessUrl,omitempty"` // Access URL (token is returned separately via GatewayToken)
	WebUIURL                    string                      `json:"webUIUrl,omitempty"`
	GatewayToken                string                      `json:"gatewayToken,omitempty"`     // Separate gateway token for display, when available
	APIServerKey                string                      `json:"apiServerKey,omitempty"`     // Hermes API server key for display, when available
	TailscaleAuthKey            string                      `json:"tailscaleAuthKey,omitempty"` // Tailscale auth key for display, when available
	Conditions                  []ConditionStatus           `json:"conditions,omitempty"`
	Governance                  *GovernanceStatus           `json:"governance,omitempty"`
	CreatedAt                   string                      `json:"createdAt"`
	LastReconciled              string                      `json:"lastReconciled,omitempty"`
	UpgradeAvailable            bool                        `json:"upgradeAvailable"`
	AgentIdentity               *AgentIdentityResponse      `json:"agentIdentity,omitempty"`
	AIGateway                   *AIGatewayResponse          `json:"aiGateway,omitempty"`
	// TeamSkills are the Skill Marketplace skills of the Agent's Team, when
	// the Team has a policy. Only the single-Agent read fills them in.
	TeamSkills []TeamSkill `json:"teamSkills,omitempty"`
	// TeamSkillsRestartNeeded reports that the Agent runs an earlier release
	// of its Team's skills than the current one. A restart copies it.
	TeamSkillsRestartNeeded bool `json:"teamSkillsRestartNeeded,omitempty"`
	// TeamSkillsPublicationFailed reports that the catalog has a publication
	// error for the Agent's Team: the Skill Marketplace could not publish the
	// Team's skills. Administrators see the error on the Skills page.
	TeamSkillsPublicationFailed bool `json:"teamSkillsPublicationFailed,omitempty"`
}

// AgentIdentityResponse contains only non-sensitive lifecycle information.
type AgentIdentityResponse struct {
	Enabled bool   `json:"enabled"`
	Phase   string `json:"phase,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ConditionStatus is a simplified view of a Kubernetes condition.
type ConditionStatus struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

// TeamSummary is the JSON response element for GET /api/v1/teams.
type TeamSummary struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// ErrorResponse is the standard error envelope.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail holds error code and message.
type ErrorDetail struct {
	Code    string                               `json:"code"`
	Message string                               `json:"message"`
	Details []cetirunv1alpha1.CompatibilityIssue `json:"details,omitempty"`
}

// StatusResponse is the JSON response for GET /api/v1/instance/status.
type StatusResponse struct {
	Conditions []ConditionStatus `json:"conditions"`
	Events     []EventItem       `json:"events"`
	NsEvents   []EventItem       `json:"nsEvents"`
	Pods       []PodStatus       `json:"pods"`
}

// EventItem is a simplified view of a Kubernetes Event.
type EventItem struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Source    string `json:"source"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	Count     int32  `json:"count"`
}

// PodStatus is a simplified view of a Pod's status.
type PodStatus struct {
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
}

// InstanceHandler handles /api/v1/instance requests.
type InstanceHandler struct {
	Kube   *kube.Client
	Config config.Config
	// VerifyInternalIdentity verifies the Portal-only ID token that the
	// gateway forwards to the internal ownership endpoint. It is intentionally
	// a function, rather than an interface containing a typed nil pointer, so
	// an unset verifier reliably fails closed for bound Agents.
	VerifyInternalIdentity func(context.Context, string) (auth.VerifiedIdentity, error)
	// Copilot runs the GitHub Copilot sign-in; nil uses the public GitHub.
	Copilot *copilot.Client
	// Now overrides the clock in tests.
	Now func() time.Time
}

// ListInstances handles GET /api/v1/instances
// Returns all instances owned by the authenticated user.
func (h *InstanceHandler) ListInstances(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	instances, err := h.Kube.ListAllInstances(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list instances: %v", err), http.StatusInternalServerError)
		return
	}
	var responses []InstanceResponse
	for i := range instances {
		inst := &instances[i]
		if !ownsInstance(inst, claims) {
			continue
		}
		resp, err := h.toInstanceResponse(r.Context(), inst)
		if err != nil {
			continue
		}
		responses = append(responses, resp)
	}
	if responses == nil {
		responses = []InstanceResponse{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responses)
}

// GetInstanceByName handles GET /api/v1/instances/{name}
// Returns 403 if the authenticated user does not own the instance.
func (h *InstanceHandler) GetInstanceByName(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing instance name", http.StatusBadRequest)
		return
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), name)
	if err != nil {
		http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusNotFound)
		return
	}
	if !ownsInstance(instance, claims) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	resp, err := h.toInstanceResponse(r.Context(), instance)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to build response: %v", err), http.StatusInternalServerError)
		return
	}
	if resp.AIGateway != nil {
		resp.AIGateway.RateLimits = h.policyRateLimits(r.Context(), instance)
	}
	resp.TeamSkills, resp.TeamSkillsRestartNeeded, resp.TeamSkillsPublicationFailed = h.teamSkills(r.Context(), instance)
	w.Header().Set("ETag", kube.RegistryETag(instance))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// UpdateInstanceByName handles PATCH /api/v1/instances/{name}.
// Both route shapes share the same ownership and version checks.
func (h *InstanceHandler) UpdateInstanceByName(w http.ResponseWriter, r *http.Request) {
	h.updateInstance(w, r, r.PathValue("name"))
}

// DeleteInstanceByName handles DELETE /api/v1/instances/{name}.
func (h *InstanceHandler) DeleteInstanceByName(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing instance name", http.StatusBadRequest)
		return
	}

	instance, err := h.Kube.GetInstanceByName(r.Context(), name)
	if err != nil {
		http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusNotFound)
		return
	}
	if !ownsInstance(instance, claims) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	version, ok := h.readInstanceVersion(w, r)
	if !ok {
		return
	}
	if err := h.Kube.DeleteInstanceByName(r.Context(), name, version); err != nil {
		writeManagementError(w, err)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// UpgradeInstanceByName handles POST /api/v1/instances/{name}/upgrade.
func (h *InstanceHandler) UpgradeInstanceByName(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing instance name", http.StatusBadRequest)
		return
	}

	existing, err := h.Kube.GetInstanceByName(r.Context(), name)
	if err != nil {
		http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusNotFound)
		return
	}
	if !ownsInstance(existing, claims) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if existing.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom {
		if err := h.validateAgentType(existing.Spec.Agent.Type); err != nil {
			writeManagementError(w, err)
			return
		}
	}

	version, ok := h.readInstanceVersion(w, r)
	if !ok {
		return
	}
	instance, err := h.Kube.UpgradeInstanceByName(r.Context(), name, version)
	if err != nil {
		writeManagementError(w, err)
		return
	}

	resp, err := h.toInstanceResponse(r.Context(), instance)
	if err != nil {
		slog.Error("Failed to build instance response", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to build response")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetInstanceStatusByName handles GET /api/v1/instances/{name}/status.
func (h *InstanceHandler) GetInstanceStatusByName(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing instance name", http.StatusBadRequest)
		return
	}

	instance, err := h.Kube.GetInstanceByName(r.Context(), name)
	if err != nil {
		http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusNotFound)
		return
	}
	if !ownsInstance(instance, claims) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ns := fmt.Sprintf("cetirun-%s", instance.Name)
	resp := StatusResponse{
		Conditions: []ConditionStatus{},
		Events:     []EventItem{},
		NsEvents:   []EventItem{},
		Pods:       []PodStatus{},
	}

	_, synchronized := h.Kube.RegistryAuthState(r.Context(), instance)
	for _, c := range registryResponseConditions(instance, synchronized) {
		resp.Conditions = append(resp.Conditions, ConditionStatus{
			Type:               c.Type,
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			ObservedGeneration: c.ObservedGeneration,
		})
	}

	crEvents, err := h.Kube.GetEvents(r.Context(), instance.Name)
	if err != nil {
		slog.Warn("Failed to get CR events", "name", name, "error", err)
	} else {
		resp.Events = toEventItems(crEvents)
	}

	nsEvents, err := h.Kube.GetNamespaceEvents(r.Context(), ns)
	if err != nil {
		slog.Warn("Failed to get namespace events", "namespace", ns, "error", err)
	} else {
		resp.NsEvents = toEventItems(nsEvents)
	}

	pods, err := h.Kube.GetPods(r.Context(), ns)
	if err != nil {
		slog.Warn("Failed to get pods", "namespace", ns, "error", err)
	} else {
		resp.Pods = toPodStatuses(pods)
	}

	writeJSON(w, http.StatusOK, resp)
}

// InternalAuth handles GET /internal/auth
// Called by the Istio EnvoyFilter Lua script to verify instance ownership.
// Returns 200 OK if the caller owns the instance, 403 Forbidden otherwise.
// Not behind auth middleware: the gateway forwards the verified OAuth2 Proxy
// ID token only to this internal endpoint. Older unbound Agents retain the
// alias/email comparison used before owner identity annotations existed.
func (h *InstanceHandler) InternalAuth(w http.ResponseWriter, r *http.Request) {
	instanceName := r.Header.Get("X-Internal-Instance")
	alias := r.Header.Get("X-Internal-Alias")
	email := r.Header.Get("X-Internal-Email")
	if instanceName == "" {
		http.Error(w, "missing instance", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	instance, err := h.Kube.GetInstanceByName(ctx, instanceName)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ownerTenant := instance.Annotations[cetirunv1alpha1.AnnotationOwnerTenantID]
	ownerObject := instance.Annotations[cetirunv1alpha1.AnnotationOwnerObjectID]
	if ownerTenant != "" || ownerObject != "" {
		const portalIdentityTokenHeader = "X-CetiRun-Portal-ID-Token"
		const bearerPrefix = "Bearer "
		authorization := r.Header.Get(portalIdentityTokenHeader)
		if h.VerifyInternalIdentity == nil || !strings.HasPrefix(authorization, bearerPrefix) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		identity, err := h.VerifyInternalIdentity(ctx, strings.TrimPrefix(authorization, bearerPrefix))
		if err != nil || !ownsInstance(instance, auth.Claims{TenantID: identity.TenantID, ObjectID: identity.ObjectID}) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if alias == "" && email == "" {
		http.Error(w, "missing identity", http.StatusBadRequest)
		return
	}
	ownerAlias := instance.Spec.User.Alias
	ownerEmail := instance.Spec.User.Email
	aliasMatch := alias != "" && ownerAlias != "" && strings.EqualFold(ownerAlias, alias)
	emailMatch := email != "" && ownerEmail != "" && strings.EqualFold(ownerEmail, email)
	if !aliasMatch && !emailMatch {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetInstance handles GET /api/v1/instance.
func (h *InstanceHandler) GetInstance(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	r.SetPathValue("name", claims.Alias)
	h.GetInstanceByName(w, r)
}

// CreateInstance handles POST /api/v1/instance.
func (h *InstanceHandler) CreateInstance(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not_authenticated", "Authentication is required")
		return
	}
	if !h.validManagementOrigin(w, r) {
		return
	}

	var req CreateInstanceRequest
	fields, ok := decodeManagementRequest(w, r, &req)
	if !ok {
		return
	}

	displayName := req.DisplayName
	if displayName == "" {
		displayName = claims.DisplayName
	}

	if req.Agent == nil {
		writeError(w, http.StatusBadRequest, "invalid_agent", "agent is required; use an object with type, image, username and optional port")
		return
	}
	agent, err := req.Agent.Create()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_agent", err.Error())
		return
	}
	if agent.Type == cetirunv1alpha1.AgentTypeCustom && hasNativeCreateFields(fields) {
		writeError(w, http.StatusBadRequest, "unsupported_custom_feature", "Custom does not accept Gateway Token or Tailscale settings")
		return
	}
	if agent.Type == cetirunv1alpha1.AgentTypeCustom && req.AgentIdentityEnabled {
		options, err := h.resolveCreationOptions(r, req.Team)
		if err != nil {
			writeManagementError(w, err)
			return
		}
		if !options.AgentIdentity.CustomAllowed {
			writeError(w, http.StatusBadRequest, "unsupported_custom_feature", "Custom Agent Identity requires a Sandbox, permitted policy and enabled governed Connections")
			return
		}
	}
	if err := h.validateAgentType(agent.Type); err != nil {
		writeManagementError(w, err)
		return
	}

	if req.TailscaleAuthKey != "" && !h.Config.TailscaleEnabled {
		writeError(w, http.StatusBadRequest, "feature_disabled", "Tailscale feature is not enabled")
		return
	}
	if req.AgentIdentityEnabled && !h.Config.AgentIdentityEnabled {
		writeError(w, http.StatusBadRequest, "feature_disabled", "Agent Identity is not enabled for this deployment")
		return
	}
	if req.AgentIdentityEnabled && (claims.TenantID == "" || claims.ObjectID == "") {
		writeError(w, http.StatusForbidden, "verified_owner_required", "A verified Microsoft Entra owner identity is required")
		return
	}
	if len(strings.TrimSpace(req.Model)) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_model", "model must be 256 characters or fewer")
		return
	}

	instanceName := claims.Alias
	if req.InstanceName != "" {
		// Validate the user-supplied suffix before combining.
		// Must match k8s DNS label rules: start and end with alphanumeric, hyphens allowed in between.
		if !validInstanceNameSuffix.MatchString(req.InstanceName) {
			writeError(w, http.StatusBadRequest, "invalid_name", "Instance name must start with a lowercase letter and contain only lowercase letters, digits, and hyphens")
			return
		}
		instanceName = claims.Alias + "-" + req.InstanceName
	}
	// See maxInstanceNameLength for where the limit comes from.
	if len(instanceName) > maxInstanceNameLength {
		writeError(w, http.StatusBadRequest, "name_too_long", fmt.Sprintf("Instance name too long: '%s' has %d characters, the limit is %d", instanceName, len(instanceName), maxInstanceNameLength))
		return
	}
	// Reject reserved instance names that collide with gateway-level routing or
	// EnvoyFilter ownership-check exemptions. See reservedInstanceNames.
	if _, ok := reservedInstanceNames[instanceName]; ok {
		writeError(w, http.StatusBadRequest, "reserved_name", "Instance name '"+instanceName+"' is reserved")
		return
	}
	if "cetirun-"+instanceName == h.Config.RegistryCredentialNamespace || "cetirun-"+instanceName == registryauth.DefaultNamespace {
		writeError(w, http.StatusBadRequest, "reserved_name", "Instance name is reserved for platform credentials")
		return
	}
	if req.RegistryAuth != nil {
		w.Header().Set("Cache-Control", "no-store")
		if !h.registryOriginAllowed(r) || registryCaller(claims) == "" {
			writeError(w, http.StatusForbidden, "forbidden", "Registry credentials require an authenticated owner and an allowed request origin")
			return
		}
		if req.RegistryAuth.Validate() != nil || agent.Image == nil || !validCreateRequestID.MatchString(r.Header.Get("Idempotency-Key")) {
			writeError(w, http.StatusBadRequest, "invalid_registry_auth", "Provide a target image, complete credentials and an Idempotency-Key for creation")
			return
		}
		if h.resumeRegistryCreate(w, r, instanceName, req, claims) {
			return
		}
	}

	team, sandboxRef, modelAccess, ok := h.validateAIGatewayCreate(w, r, req, agent.Type)
	if !ok {
		return
	}

	if h.Config.MaxInstancesPerUser > 0 {
		all, err := h.Kube.ListAllInstances(r.Context())
		if err != nil {
			slog.Error("Failed to list instances for limit check", "alias", claims.Alias, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check instance limit")
			return
		}
		count := 0
		for _, inst := range all {
			if ownsInstance(&inst, claims) {
				count++
			}
		}
		if count >= h.Config.MaxInstancesPerUser {
			writeError(w, http.StatusForbidden, "limit_exceeded",
				fmt.Sprintf("Maximum instances per user reached (%d)", h.Config.MaxInstancesPerUser))
			return
		}
	}

	options := kube.CreateInstanceOptions{
		AgentIdentityEnabled: req.AgentIdentityEnabled,
		OwnerTenantID:        claims.TenantID,
		OwnerObjectID:        claims.ObjectID,
		SandboxRef:           sandboxRef,
		ModelAccess:          modelAccess,
	}
	if sandboxRef != "" && agent.Type != cetirunv1alpha1.AgentTypeCustom {
		runtime, available := h.Config.SandboxRuntimeFor(agent.Type)
		_, configured := h.Config.SandboxRuntimes[string(agent.Type)]
		if !available && (configured || !h.Config.SandboxControllerDefaults) {
			writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "The selected agent runtime is not configured for Sandbox creation; ask an administrator to configure its approved runtime profile")
			return
		}
		// Preserve explicit requests so admission can validate their qualification.
		// Missing defaults may be delegated only to an explicitly enabled
		// common executor; the retained executor requires pins at creation.
		if available && agent.Image == nil {
			agent.Image = &runtime.Image
		}
		if available && agent.Port == nil {
			agent.Port = &runtime.Port
		}
		if available {
			options.ChartVersion = runtime.ChartVersion
		}
	}
	if req.RegistryAuth != nil {
		id, err := kube.RegistryID()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "registry_unavailable", "Unable to prepare registry credentials")
			return
		}
		agent.RegistrySecretRef = &cetirunv1alpha1.RegistrySecretRef{Name: "registry-source-" + id}
		options.Annotations = map[string]string{
			kube.AnnotationCreateRequest: r.Header.Get("Idempotency-Key"), kube.AnnotationCreateCaller: registryCaller(claims),
			kube.AnnotationCreateConfig: registryCreateConfig(req), kube.AnnotationCreateRegistrySource: agent.RegistrySecretRef.Name,
			registryauth.Operation: id,
		}
	}
	instance, err := h.Kube.CreateInstance(r.Context(), claims.Alias, displayName, team, claims.Email, instanceName, agent, req.GatewayToken, req.TailscaleAuthKey, strings.TrimSpace(req.Model), options)
	if err != nil {
		if kube.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, "instance_exists",
				"Instance already exists for user "+claims.Alias)
			return
		}
		writeManagementError(w, err)
		return
	}
	if req.RegistryAuth != nil {
		if err := h.Kube.SaveRegistryCredentials(r.Context(), instance, *req.RegistryAuth, true); err != nil {
			writeRegistryError(w, err, instance)
			return
		}
	}

	resp, err := h.toInstanceResponse(r.Context(), instance)
	if err != nil {
		slog.Error("Failed to build instance response", "alias", claims.Alias, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to build response")
		return
	}

	slog.Info("Instance created", "alias", claims.Alias, "team", team, "displayName", displayName)
	writeJSON(w, http.StatusCreated, resp)
}

// UpdateInstanceRequest is the JSON body for PATCH /api/v1/instance.
type UpdateInstanceRequest struct {
	UID              string             `json:"uid"`
	ResourceVersion  string             `json:"resourceVersion"`
	Agent            *agentconfig.Input `json:"agent,omitempty"`
	DisplayName      *string            `json:"displayName,omitempty"`
	Team             *string            `json:"team,omitempty"`
	GatewayToken     *string            `json:"gatewayToken,omitempty"`
	TailscaleAuthKey *string            `json:"tailscaleAuthKey,omitempty"`
	Model            *string            `json:"model,omitempty"`
}

// UpdateInstance handles PATCH /api/v1/instance.
func (h *InstanceHandler) UpdateInstance(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	h.updateInstance(w, r, claims.Alias)
}

// DeleteInstance handles DELETE /api/v1/instance.
func (h *InstanceHandler) DeleteInstance(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	r.SetPathValue("name", claims.Alias)
	h.DeleteInstanceByName(w, r)
}

// UpgradeInstance handles POST /api/v1/instance/upgrade.
func (h *InstanceHandler) UpgradeInstance(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	r.SetPathValue("name", claims.Alias)
	h.UpgradeInstanceByName(w, r)
}

func (h *InstanceHandler) toInstanceResponse(ctx context.Context, instance *cetirunv1alpha1.Agent) (InstanceResponse, error) {
	configured, synchronized := h.Kube.RegistryAuthState(ctx, instance)
	resp := InstanceResponse{
		Alias:                    instance.Spec.User.Alias,
		InstanceName:             instance.Name,
		Agent:                    agentconfig.Defaults(instance.Spec.Agent),
		UID:                      string(instance.UID),
		ResourceVersion:          instance.ResourceVersion,
		RegistryAuthConfigured:   configured,
		Generation:               instance.Generation,
		ObservedGeneration:       instance.Status.ObservedGeneration,
		Governance:               governanceStatus(instance),
		Suspended:                instance.Spec.Suspended,
		SuspendAvailable:         kube.CanSuspendInstance(instance),
		ResumeAvailable:          kube.CanResumeInstance(instance),
		RestartAvailable:         kube.CanRestartInstance(instance),
		RestartRequestID:         instance.Spec.RestartRequestID,
		ObservedRestartRequestID: instance.Status.ObservedRestartRequestID,
		DisplayName:              instance.Spec.User.DisplayName,
		Team:                     instanceTeam(instance),
		Model:                    instance.Spec.Model,
		Phase:                    instance.Status.Phase,
		CreatedAt:                instance.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	if resp.SuspendAvailable {
		if err := h.Kube.CheckSuspension(ctx, instance); err != nil {
			slog.Warn("Retained suspension storage check failed", "name", instance.Name, "error", err)
			resp.SuspendAvailable = false
			resp.SuspensionUnavailableReason = kube.ErrSuspensionHomeUnverified.Error()
			if errors.Is(err, kube.ErrSuspensionHomeRequired) {
				resp.SuspensionUnavailableReason = kube.ErrSuspensionHomeRequired.Error()
			}
		}
	}
	if instance.Status.LastReconcileTime != nil {
		resp.LastReconciled = instance.Status.LastReconcileTime.UTC().Format("2006-01-02T15:04:05Z")
	}

	for _, c := range registryResponseConditions(instance, synchronized) {
		resp.Conditions = append(resp.Conditions, ConditionStatus{
			Type:               c.Type,
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			ObservedGeneration: c.ObservedGeneration,
		})
	}

	// Determine if an upgrade is available by comparing instance version with operator latest.
	// Empty instance version means the operator hasn't backfilled it yet — not upgradeable.
	latestVersion, err := h.Kube.GetOperatorChartVersion(ctx)
	if err != nil {
		slog.Warn("Failed to get operator chart version", "error", err)
	} else if latestVersion != "" && instance.Spec.Helm.Version != "" && instance.Spec.Helm.Version != latestVersion {
		resp.UpgradeAvailable = true
	}

	// Get gateway token and access URL for Ready or Provisioning instances.
	// The operator records the Agent's namespace once it has created it; until
	// then there are no Secrets to read.
	if instance.Spec.Agent.Type != cetirunv1alpha1.AgentTypeCustom &&
		(instance.Status.Phase == "Ready" || instance.Status.Phase == "Provisioning") &&
		instance.Status.Namespace != "" {
		ns := instance.Status.Namespace
		token, err := h.Kube.GetGatewayToken(ctx, ns, instance.Spec.GatewayTokenSecretRef)
		if err != nil {
			// Log error but don't block the response
			slog.Warn("Failed to get gateway token", "alias", instance.Spec.User.Alias, "error", err)
			// Return original URL without token if token fetch fails
			resp.AccessURL = instance.Status.AccessURL
		} else {
			resp.GatewayToken = token
			resp.AccessURL = instance.Status.AccessURL
		}
		if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeHermes {
			apiServerKey, err := h.Kube.GetAPIServerKey(ctx, ns, instance.Spec.GatewayTokenSecretRef)
			if err != nil {
				slog.Warn("Failed to get Hermes API server key", "alias", instance.Spec.User.Alias, "error", err)
			} else {
				resp.APIServerKey = apiServerKey
			}
		}
		// Get tailscale auth key if Tailscale is enabled
		if instance.Spec.Tailscale != nil {
			tsKey, err := h.Kube.GetTailscaleAuthKey(ctx, ns, instance.Spec.Tailscale.AuthKeySecretRef)
			if err != nil {
				reason := "secret_read_failed"
				switch {
				case kube.IsNotFound(err):
					reason = "secret_not_found"
				case kube.IsForbidden(err):
					reason = "secret_access_denied"
				}
				slog.Warn("Failed to get tailscale auth key", "alias", instance.Spec.User.Alias, "reason", reason)
			} else {
				resp.TailscaleAuthKey = tsKey
			}
		}
	} else if instance.Spec.Agent.Type != cetirunv1alpha1.AgentTypeCustom {
		// For other states, and before the namespace is recorded, return the
		// original URL (without token)
		resp.AccessURL = instance.Status.AccessURL
	}
	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom &&
		(instance.Status.Phase == "Ready" || instance.Status.Phase == "Provisioning") {
		for _, condition := range instance.Status.Conditions {
			if condition.Type == cetirunv1alpha1.ConditionIngressReady && condition.Status == metav1.ConditionTrue {
				if instance.Spec.Agent.WebUI {
					resp.WebUIURL = instance.Status.AccessURL
				}
				service := customAccessService(instance.Status.Conditions, !instance.Spec.Agent.WebUI)
				if domain, err := h.Kube.GetOperatorDomainSuffix(ctx); err == nil && service != "" {
					resp.AccessURL = fmt.Sprintf("https://%s/%s/%s/", domain, instance.Name, service)
				}
				break
			}
		}
	}

	if instance.Spec.AgentIdentity != nil {
		resp.AgentIdentity = &AgentIdentityResponse{Enabled: instance.Spec.AgentIdentity.Enabled}
		if instance.Status.AgentIdentity != nil {
			resp.AgentIdentity.Phase = instance.Status.AgentIdentity.Phase
			resp.AgentIdentity.Reason = instance.Status.AgentIdentity.Reason
		}
	}
	resp.AIGateway = aiGatewayResponse(instance)

	return resp, nil
}

func customAccessService(conditions []metav1.Condition, legacyDesktop bool) string {
	desktop, terminal := legacyDesktop, false
	for _, condition := range conditions {
		switch condition.Type {
		case cetirunv1alpha1.ConditionDesktopConfigured:
			desktop = condition.Status == metav1.ConditionTrue
		case cetirunv1alpha1.ConditionTerminalConfigured:
			terminal = condition.Status == metav1.ConditionTrue
		}
	}
	if desktop {
		// Older operators report only IngressReady for their desktop route.
		return "desktop"
	}
	if terminal {
		return "term"
	}
	return ""
}

// GetInstanceStatus handles GET /api/v1/instance/status.
func (h *InstanceHandler) GetInstanceStatus(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	r.SetPathValue("name", claims.Alias)
	h.GetInstanceStatusByName(w, r)
}

func toEventItems(events []corev1.Event) []EventItem {
	items := make([]EventItem, 0, len(events))
	for _, e := range events {
		source := e.Source.Component
		if e.Source.Host != "" {
			if source != "" {
				source += "/" + e.Source.Host
			} else {
				source = e.Source.Host
			}
		}
		item := EventItem{
			Type:    e.Type,
			Reason:  e.Reason,
			Message: e.Message,
			Source:  source,
			Count:   e.Count,
		}
		if !e.FirstTimestamp.IsZero() {
			item.FirstSeen = e.FirstTimestamp.UTC().Format("2006-01-02T15:04:05Z")
		}
		if !e.LastTimestamp.IsZero() {
			item.LastSeen = e.LastTimestamp.UTC().Format("2006-01-02T15:04:05Z")
		}
		items = append(items, item)
	}
	// Sort by lastSeen descending. Events with empty LastSeen (no timestamp)
	// sort to the end since "" < any non-empty string.
	sort.Slice(items, func(i, j int) bool {
		return items[i].LastSeen > items[j].LastSeen
	})
	return items
}

func toPodStatuses(pods []corev1.Pod) []PodStatus {
	statuses := make([]PodStatus, 0, len(pods))
	for _, p := range pods {
		var totalRestarts int32
		for _, cs := range p.Status.ContainerStatuses {
			totalRestarts += cs.RestartCount
		}
		statuses = append(statuses, PodStatus{
			Name:         p.Name,
			Phase:        string(p.Status.Phase),
			Ready:        isPodReady(&p),
			RestartCount: totalRestarts,
		})
	}
	return statuses
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// ListTeams handles GET /api/v1/teams.
func (h *InstanceHandler) ListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := h.Kube.ListTeams(r.Context())
	if err != nil {
		slog.Error("failed to list teams", "err", err)
		http.Error(w, "failed to list teams", http.StatusInternalServerError)
		return
	}
	result := make([]TeamSummary, len(teams))
	for i, t := range teams {
		result[i] = TeamSummary{
			Name: t.Name,
			Role: t.Spec.MemberContext.Role,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Error("failed to encode teams response", "err", err)
	}
}
