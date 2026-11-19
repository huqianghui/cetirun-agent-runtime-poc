// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

// AdminInstanceDetail contains configuration and observations only. Building it
// must not read Secrets or invoke the owner's toInstanceResponse method.
type AdminInstanceDetail struct {
	AdminInstanceSummary
	ResourceVersion    string                 `json:"resourceVersion"`
	Generation         int64                  `json:"generation"`
	ObservedGeneration int64                  `json:"observedGeneration"`
	Namespace          string                 `json:"namespace,omitempty"`
	LastReconciled     string                 `json:"lastReconciled,omitempty"`
	DeletionTimestamp  string                 `json:"deletionTimestamp,omitempty"`
	Suspended          bool                   `json:"suspended"`
	SandboxRef         string                 `json:"sandboxRef,omitempty"`
	ChartVersion       string                 `json:"chartVersion,omitempty"`
	Model              string                 `json:"model,omitempty"`
	AgentIdentity      *AgentIdentityResponse `json:"agentIdentity,omitempty"`
	TailscaleEnabled   bool                   `json:"tailscaleEnabled"`
	// This means a credential reference is configured, not that it was verified.
	RegistryAuthConfigured bool              `json:"registryAuthConfigured"`
	Conditions             []ConditionStatus `json:"conditions"`
}

type AdminAgentUpdate struct {
	Image agentconfig.Field[string] `json:"image,omitzero"`
	Port  agentconfig.Field[int32]  `json:"port,omitzero"`
	WebUI agentconfig.Field[bool]   `json:"webUI,omitzero"`
}

// AdminUpdateInstanceRequest excludes ownership, credentials and lifecycle
// controls. Unknown fields are rejected by decodeManagementRequest.
type AdminUpdateInstanceRequest struct {
	kube.InstanceVersion
	Agent       *AdminAgentUpdate `json:"agent,omitempty"`
	DisplayName *string           `json:"displayName,omitempty"`
	Model       *string           `json:"model,omitempty"`
}

type AdminDiagnosticError struct {
	Section string `json:"section"`
	Message string `json:"message"`
}

type AdminInstanceStatus struct {
	StatusResponse
	UID    string                 `json:"uid"`
	Errors []AdminDiagnosticError `json:"errors,omitempty"`
}

func adminInstanceDetail(instance *api.Agent) AdminInstanceDetail {
	resp := AdminInstanceDetail{
		AdminInstanceSummary: adminInstanceSummary(instance),
		ResourceVersion:      instance.ResourceVersion,
		Generation:           instance.Generation, ObservedGeneration: instance.Status.ObservedGeneration,
		Namespace: instance.Status.Namespace, Suspended: instance.Spec.Suspended,
		SandboxRef: instance.Spec.SandboxRef, ChartVersion: instance.Spec.Helm.Version,
		Model: instance.Spec.Model, TailscaleEnabled: instance.Spec.Tailscale != nil,
		RegistryAuthConfigured: instance.Spec.Agent.RegistrySecretRef != nil,
		Conditions:             adminInstanceConditions(instance),
	}
	if instance.Status.LastReconcileTime != nil {
		resp.LastReconciled = instance.Status.LastReconcileTime.UTC().Format("2006-01-02T15:04:05Z")
	}
	if !instance.DeletionTimestamp.IsZero() {
		resp.DeletionTimestamp = instance.DeletionTimestamp.UTC().Format("2006-01-02T15:04:05Z")
	}
	if instance.Spec.AgentIdentity != nil || instance.Status.AgentIdentity != nil {
		resp.AgentIdentity = &AgentIdentityResponse{}
		if instance.Spec.AgentIdentity != nil {
			resp.AgentIdentity.Enabled = instance.Spec.AgentIdentity.Enabled
		}
		if instance.Status.AgentIdentity != nil {
			resp.AgentIdentity.Phase, resp.AgentIdentity.Reason = instance.Status.AgentIdentity.Phase, instance.Status.AgentIdentity.Reason
		}
	}
	return resp
}

func adminInstanceConditions(instance *api.Agent) []ConditionStatus {
	out := make([]ConditionStatus, 0, len(instance.Status.Conditions))
	for _, condition := range instance.Status.Conditions {
		out = append(out, ConditionStatus{Type: condition.Type, Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message, ObservedGeneration: condition.ObservedGeneration})
	}
	return out
}

func adminInstanceAccess(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok || !claims.IsAdmin {
		writeError(w, http.StatusForbidden, "forbidden_admin", "Admin access required")
		return false
	}
	if r.PathValue("name") == "" {
		writeError(w, http.StatusBadRequest, "invalid_name", "Agent name is required")
		return false
	}
	return true
}

// GetInstance handles GET /api/v1/admin/instances/{name}.
func (h *AdminInstanceHandler) GetInstance(w http.ResponseWriter, r *http.Request) {
	if !adminInstanceAccess(w, r) {
		return
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminInstanceDetail(instance))
}

// GetInstanceStatus handles GET /api/v1/admin/instances/{name}/status.
func (h *AdminInstanceHandler) GetInstanceStatus(w http.ResponseWriter, r *http.Request) {
	if !adminInstanceAccess(w, r) {
		return
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	resp := AdminInstanceStatus{UID: string(instance.UID), StatusResponse: StatusResponse{
		Conditions: adminInstanceConditions(instance), Events: []EventItem{}, NsEvents: []EventItem{}, Pods: []PodStatus{},
	}}
	events, err := h.Kube.GetInstanceEvents(r.Context(), instance)
	if err != nil {
		resp.Errors = append(resp.Errors, AdminDiagnosticError{"events", "Agent events could not be loaded"})
	} else {
		resp.Events = toEventItems(events)
	}
	namespace, err := h.Kube.GetInstanceDiagnosticNamespace(r.Context(), instance)
	if err != nil || namespace == nil {
		message := "Agent namespace is not available yet"
		if err != nil {
			message = "Agent namespace could not be verified"
		}
		markNamespaceDiagnosticsUnavailable(&resp, message)
	} else {
		events, err := h.Kube.GetNamespaceEvents(r.Context(), namespace.Name)
		if err != nil {
			resp.Errors = append(resp.Errors, AdminDiagnosticError{"nsEvents", "Namespace events could not be loaded"})
		} else {
			resp.NsEvents = toEventItems(events)
		}
		pods, err := h.Kube.GetPods(r.Context(), namespace.Name)
		if err != nil {
			resp.Errors = append(resp.Errors, AdminDiagnosticError{"pods", "Pods could not be loaded"})
		} else {
			resp.Pods = toPodStatuses(pods)
		}
		if err := h.Kube.VerifyInstanceDiagnosticNamespace(r.Context(), instance, namespace); err != nil {
			markNamespaceDiagnosticsUnavailable(&resp, "Agent namespace changed while diagnostics were loading")
		}
	}
	// Never associate observations with a same-named replacement Agent.
	current, err := h.Kube.GetInstanceByName(r.Context(), instance.Name)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if current.UID != instance.UID {
		writeManagementError(w, kube.ErrInstanceChanged)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func markNamespaceDiagnosticsUnavailable(resp *AdminInstanceStatus, message string) {
	resp.NsEvents = []EventItem{}
	resp.Pods = []PodStatus{}
	filtered := resp.Errors[:0]
	for _, diagnostic := range resp.Errors {
		if diagnostic.Section != "pods" && diagnostic.Section != "nsEvents" {
			filtered = append(filtered, diagnostic)
		}
	}
	resp.Errors = append(filtered,
		AdminDiagnosticError{"pods", message},
		AdminDiagnosticError{"nsEvents", message},
	)
}

// UpdateInstance handles PATCH /api/v1/admin/instances/{name}.
func (h *AdminInstanceHandler) UpdateInstance(w http.ResponseWriter, r *http.Request) {
	audit := &adminInstanceAudit{ResponseWriter: w, status: http.StatusOK}
	w = audit
	defer audit.log(r, "update")
	if !adminInstanceAccess(w, r) {
		return
	}
	management := &InstanceHandler{Config: h.Config}
	if !management.validManagementOrigin(w, r) {
		return
	}
	var req AdminUpdateInstanceRequest
	fields, ok := decodeManagementRequest(w, r, &req)
	if !ok {
		return
	}
	audit.uid = string(req.UID)
	for field, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			writeError(w, http.StatusBadRequest, "invalid_body", field+" must not be null")
			return
		}
		if field != "uid" && field != "resourceVersion" && field != "agent" {
			audit.fields = append(audit.fields, field)
		}
	}
	if req.UID == "" || req.ResourceVersion == "" {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Agent are required")
		return
	}
	input := agentconfig.Input{}
	if req.Agent != nil {
		input.Image, input.Port, input.WebUI = req.Agent.Image, req.Agent.Port, req.Agent.WebUI
		if input.Image.Set {
			audit.fields = append(audit.fields, "agent.image")
		}
		if input.Port.Set {
			audit.fields = append(audit.fields, "agent.port")
		}
		if input.WebUI.Set {
			audit.fields = append(audit.fields, "agent.webUI")
		}
	}
	if input.Empty() && req.DisplayName == nil && req.Model == nil {
		writeError(w, http.StatusBadRequest, "no_fields", "At least one field must be provided for update")
		return
	}
	if req.Model != nil && len(strings.TrimSpace(*req.Model)) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_model", "model must be 256 characters or fewer")
		return
	}
	if req.DisplayName != nil {
		*req.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if req.UID != instance.UID || req.ResourceVersion != instance.ResourceVersion {
		writeManagementError(w, kube.ErrInstanceChanged)
		return
	}
	if input.Port.Set && agentconfig.Defaults(instance.Spec.Agent).Type != api.AgentTypeCustom {
		writeError(w, http.StatusBadRequest, "invalid_agent", "agent.port is configurable only for custom agents")
		return
	}
	if input.WebUI.Set && agentconfig.Defaults(instance.Spec.Agent).Type != api.AgentTypeCustom {
		writeError(w, http.StatusBadRequest, "invalid_agent", "agent.webUI is configurable only for custom agents")
		return
	}
	opts := kube.UpdateInstanceOpts{
		ExpectedUID: req.UID, ExpectedResourceVersion: req.ResourceVersion,
		DisplayName: req.DisplayName, Model: req.Model,
	}
	if !input.Empty() {
		agent, err := input.Apply(instance.Spec.Agent)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_agent", err.Error())
			return
		}
		opts.Agent = &agent
	}
	opts.Authorize = func(current *api.Agent) error {
		// Admin authority replaces ownership only on these routes; platform
		// type restrictions and admission still apply to runtime changes.
		if !input.Empty() || current.Spec.Agent.Type == api.AgentTypeCustom && req.Model != nil {
			return management.validateAgentType(agentconfig.Defaults(current.Spec.Agent).Type)
		}
		return nil
	}
	updated, err := h.Kube.UpdateInstanceByName(r.Context(), instance.Name, opts)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminInstanceDetail(updated))
}

// DeleteInstance delegates cleanup to the Operator, retaining its finalizers.
func (h *AdminInstanceHandler) DeleteInstance(w http.ResponseWriter, r *http.Request) {
	audit := &adminInstanceAudit{ResponseWriter: w, status: http.StatusOK}
	w = audit
	defer audit.log(r, "delete")
	if !adminInstanceAccess(w, r) {
		return
	}
	management := &InstanceHandler{Config: h.Config}
	version, ok := management.readInstanceVersion(w, r)
	if !ok {
		return
	}
	audit.uid = string(version.UID)
	if err := h.Kube.DeleteInstanceByName(r.Context(), r.PathValue("name"), version); err != nil {
		writeManagementError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// Audit records field names and status only, including rejected mutations.
type adminInstanceAudit struct {
	http.ResponseWriter
	status int
	uid    string
	fields []string
}

func (a *adminInstanceAudit) WriteHeader(status int) {
	a.status = status
	a.ResponseWriter.WriteHeader(status)
}

func (a *adminInstanceAudit) log(r *http.Request, action string) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	sort.Strings(a.fields)
	slog.Info("Admin Agent operation", "actor", claims.Alias, "subject", claims.Subject,
		"action", action, "name", r.PathValue("name"), "uid", a.uid,
		"fields", a.fields, "status", a.status)
}
