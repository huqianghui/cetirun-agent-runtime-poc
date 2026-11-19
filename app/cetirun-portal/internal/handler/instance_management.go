// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sort"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	strictjson "sigs.k8s.io/json"
)

type managementError struct {
	status  int
	code    string
	message string
}

func (e *managementError) Error() string { return e.message }

func decodeManagementRequest(w http.ResponseWriter, r *http.Request, target any) (map[string]json.RawMessage, bool) {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "Management requests must use application/json")
			return nil, false
		}
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body is invalid or exceeds 64 KiB")
		return nil, false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be a JSON object")
		return nil, false
	}
	problems, err := strictjson.UnmarshalStrict(data, target)
	if err != nil || len(problems) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body, unknown field or duplicate field; use the nested agent object, not agentType")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return nil, false
	}
	return fields, true
}

func hasNativeCreateFields(fields map[string]json.RawMessage) bool {
	for _, key := range []string{"gatewayToken", "tailscaleAuthKey"} {
		if _, exists := fields[key]; exists {
			return true
		}
	}
	return false
}

func ownsInstance(instance *api.Agent, claims auth.Claims) bool {
	tenant := instance.Annotations[api.AnnotationOwnerTenantID]
	object := instance.Annotations[api.AnnotationOwnerObjectID]
	if tenant != "" || object != "" {
		return tenant != "" && object != "" && strings.EqualFold(tenant, claims.TenantID) && strings.EqualFold(object, claims.ObjectID)
	}
	return claims.Alias != "" && strings.EqualFold(instance.Spec.User.Alias, claims.Alias)
}

func (h *InstanceHandler) validateAgentType(kind api.AgentType) error {
	if !h.Config.AgentTypeAllowed(string(kind)) {
		return &managementError{http.StatusForbidden, "agent_type_not_allowed", "This agent type is not enabled for self-service"}
	}
	return nil
}

func (h *InstanceHandler) updateInstance(w http.ResponseWriter, r *http.Request, name string) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not_authenticated", "Authentication is required")
		return
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_name", "Agent name is required")
		return
	}
	if !h.validManagementOrigin(w, r) {
		return
	}
	var req UpdateInstanceRequest
	fields, ok := decodeManagementRequest(w, r, &req)
	if !ok {
		return
	}
	for key, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			writeError(w, http.StatusBadRequest, "invalid_body", key+" must not be null; clear only optional fields inside agent with null")
			return
		}
	}
	if (req.Agent == nil || req.Agent.Empty()) && req.DisplayName == nil && req.Team == nil && req.Model == nil &&
		req.GatewayToken == nil && req.TailscaleAuthKey == nil {
		writeError(w, http.StatusBadRequest, "no_fields", "At least one field must be provided for update")
		return
	}
	if req.GatewayToken != nil && *req.GatewayToken == "" {
		writeError(w, http.StatusBadRequest, "invalid_gateway_token", "gatewayToken must be non-empty when provided")
		return
	}
	if req.Model != nil && len(strings.TrimSpace(*req.Model)) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_model", "model must be 256 characters or fewer")
		return
	}
	if req.TailscaleAuthKey != nil && *req.TailscaleAuthKey != "" && !h.Config.TailscaleEnabled {
		writeError(w, http.StatusBadRequest, "feature_disabled", "Tailscale feature is not enabled")
		return
	}
	if req.DisplayName != nil {
		*req.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), name)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if !ownsInstance(instance, claims) {
		writeError(w, http.StatusForbidden, "forbidden", "Only the instance owner may change this Agent")
		return
	}
	if req.UID == "" || req.ResourceVersion == "" {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Agent are required")
		return
	}
	if req.UID != string(instance.UID) || req.ResourceVersion != instance.ResourceVersion {
		writeManagementError(w, kube.ErrInstanceChanged)
		return
	}
	opts := kube.UpdateInstanceOpts{
		DisplayName: req.DisplayName, Team: req.Team, Model: req.Model,
		GatewayToken: req.GatewayToken, TailscaleAuthKey: req.TailscaleAuthKey,
		ExpectedUID: types.UID(req.UID), ExpectedResourceVersion: req.ResourceVersion,
	}
	if req.Team != nil {
		team, sandboxRef, ok := h.resolveTeamBinding(w, r, *req.Team)
		if !ok {
			return
		}
		req.Team = &team
		opts.Team, opts.SandboxRef = &team, &sandboxRef
	}
	if req.Agent != nil {
		updated, err := req.Agent.Apply(instance.Spec.Agent)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_agent", err.Error())
			return
		}
		opts.Agent = &updated
	}
	opts.Authorize = func(current *api.Agent) error {
		if !ownsInstance(current, claims) {
			return &managementError{http.StatusForbidden, "forbidden", "Only the instance owner may change this Agent"}
		}
		if req.Agent != nil && current.Spec.Agent.RegistrySecretRef != nil && !registryOwner(current, claims) {
			return &managementError{http.StatusForbidden, "forbidden", "Only the instance owner may change a private image"}
		}
		teamChanged := req.Team != nil && *req.Team != instanceTeam(current)
		if teamChanged {
			if !claims.IsAdmin || !current.Spec.Suspended {
				return &managementError{http.StatusForbidden, "team_change_not_allowed", "Changing Team requires an administrator and a suspended Agent"}
			}
		}
		if req.Agent != nil || teamChanged || current.Spec.Agent.Type == api.AgentTypeCustom && req.Model != nil {
			agent := agentconfig.Defaults(current.Spec.Agent)
			if opts.Agent != nil {
				agent = *opts.Agent
			}
			return h.validateAgentType(agent.Type)
		}
		return nil
	}
	updated, err := h.Kube.UpdateInstanceByName(r.Context(), name, opts)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	response, err := h.toInstanceResponse(r.Context(), updated)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	changed := make([]string, 0, len(fields))
	for field := range fields {
		if field != "uid" && field != "resourceVersion" {
			changed = append(changed, field)
		}
	}
	sort.Strings(changed)
	slog.Info("Instance updated", "name", name, "fields", changed)
	writeJSON(w, http.StatusOK, response)
}

func (h *InstanceHandler) readInstanceVersion(w http.ResponseWriter, r *http.Request) (kube.InstanceVersion, bool) {
	var version kube.InstanceVersion
	if !h.validManagementOrigin(w, r) {
		return version, false
	}
	if r.Body == nil || r.ContentLength == 0 {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Agent are required")
		return version, false
	}
	if _, ok := decodeManagementRequest(w, r, &version); !ok {
		return version, false
	}
	if version.UID == "" || version.ResourceVersion == "" {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Agent are required")
		return version, false
	}
	return version, true
}

func (h *InstanceHandler) validManagementOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" || origin == "null" ||
		h.Config.PortalOrigin != "" && origin != "" && origin != h.Config.PortalOrigin {
		writeError(w, http.StatusForbidden, "invalid_origin", "Request origin is not allowed")
		return false
	}
	return true
}

func writeManagementError(w http.ResponseWriter, err error) {
	var detail *managementError
	switch {
	case errors.As(err, &detail):
		writeError(w, detail.status, detail.code, detail.message)
	case errors.Is(err, kube.ErrInstanceChanged), apierrors.IsConflict(err):
		writeError(w, http.StatusConflict, "instance_changed", kube.ErrInstanceChanged.Error())
	case errors.Is(err, kube.ErrInstanceDeleting):
		writeError(w, http.StatusConflict, "instance_deleting", err.Error())
	case errors.Is(err, kube.ErrInstanceVersionRequired):
		writeError(w, http.StatusPreconditionRequired, "version_required", err.Error())
	case errors.Is(err, kube.ErrSuspensionUnsupported):
		writeError(w, http.StatusConflict, "suspension_unavailable", err.Error())
	case errors.Is(err, kube.ErrSuspensionHomeRequired):
		writeError(w, http.StatusConflict, "suspension_home_required", kube.ErrSuspensionHomeRequired.Error())
	case errors.Is(err, kube.ErrSuspensionHomeUnverified):
		writeError(w, http.StatusServiceUnavailable, "suspension_home_unverified", kube.ErrSuspensionHomeUnverified.Error())
	case errors.Is(err, kube.ErrInvalidRestartRequest):
		writeError(w, http.StatusBadRequest, "invalid_restart_request", err.Error())
	case errors.Is(err, kube.ErrInstanceSuspended), errors.Is(err, kube.ErrRestartUnsupported), errors.Is(err, kube.ErrRestartNotReady), errors.Is(err, kube.ErrMigrationPending):
		writeError(w, http.StatusConflict, "restart_unavailable", err.Error())
	case kube.IsUnsupportedCustomUpdate(err):
		writeError(w, http.StatusBadRequest, "unsupported_custom_feature", err.Error())
	case kube.IsNotFound(err):
		writeError(w, http.StatusNotFound, "instance_not_found", "Agent not found")
	case apierrors.IsForbidden(err):
		writeError(w, http.StatusForbidden, "admission_rejected", "Platform admission denied the change")
	case apierrors.IsInvalid(err):
		writeError(w, http.StatusBadRequest, "invalid_agent", "The Agent configuration was rejected by the API")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to apply Agent change")
	}
}
