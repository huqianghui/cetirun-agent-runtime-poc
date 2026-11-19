// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

// AdminTeamHandler implements CRUD for Team resources under
// /api/v1/admin/teams. Authorization is enforced by an upstream middleware
// (auth.RequireAdmin); this handler does not check the admin flag itself.
type AdminTeamHandler struct {
	Kube *kube.Client
	// Config enables the Skill Marketplace checks of Team skills.
	Config config.Config
}

// teamNamePattern enforces a DNS-1123 label for the cluster-scoped team
// name. Mirrors the alias pattern used elsewhere in the portal.
var teamNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Only expose known field paths, never arbitrary map keys or admission messages.
var teamValidationFieldPattern = regexp.MustCompile(`^(metadata\.name|spec(\.(agentPolicyRef|defaultSandboxRef|` +
	`sandboxRefs(\[[0-9]+\])?|skills(\[[0-9]+\])?|memberContext(\.(role|description(\[[0-9]+\])?))?|` +
	`skillStorage(\.(resourceGroup|storageAccount|containerName|storageClassName|storageSize|mountPath))?|` +
	`modelRouters(\[[0-9]+\](\.(name|classifier(\.(type|model))?|tiers(\.(simple|medium|complex|reasoning))?))?)?))?)$`)

func teamAdmissionRejected(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	message := status.Status().Message
	return strings.HasPrefix(message, `admission webhook "`) && strings.Contains(message, `" denied the request:`)
}

func teamValidationMessage(err error) string {
	message := "Team configuration was rejected by the cluster."
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return message + " Ask a platform administrator to check the Team requirements and Portal logs."
	}
	var fields []string
	for _, cause := range status.Status().Details.Causes {
		if !teamValidationFieldPattern.MatchString(cause.Field) {
			continue
		}
		// Kubernetes messages may include rejected values or webhook internals.
		// Use fixed descriptions for structured cause types instead.
		reason := "Invalid value"
		switch cause.Type {
		case metav1.CauseTypeFieldValueRequired:
			reason = "Required value"
		case metav1.CauseTypeFieldValueDuplicate:
			reason = "Duplicate value"
		case metav1.CauseTypeFieldValueNotSupported:
			reason = "Unsupported value"
		case metav1.CauseTypeFieldValueNotFound:
			reason = "Referenced value was not found"
		case metav1.CauseTypeTooLong:
			reason = "Value is too long"
		case metav1.CauseTypeTooMany:
			reason = "Too many items"
		case metav1.CauseTypeForbidden:
			reason = "Field is not allowed"
		}
		fields = append(fields, cause.Field+": "+reason)
	}
	if len(fields) > 0 {
		message += " " + strings.Join(fields, "; ") + "."
	}
	return message + " Check the Team fields; if the required setting is unavailable, ask a platform administrator to check the Team requirements and Portal logs."
}

// TeamSkillStorageDTO mirrors cetirunv1alpha1.SkillStorageSpec.
type TeamSkillStorageDTO struct {
	ResourceGroup    string `json:"resourceGroup"`
	StorageAccount   string `json:"storageAccount"`
	ContainerName    string `json:"containerName"`
	StorageClassName string `json:"storageClassName,omitempty"`
	StorageSize      string `json:"storageSize,omitempty"`
	MountPath        string `json:"mountPath,omitempty"`
}

// TeamDTO is the JSON shape for Team admin endpoints.
type TeamDTO struct {
	Generation        int64                                    `json:"generation"`
	Deleting          bool                                     `json:"deleting"`
	Compatibility     *cetirunv1alpha1.TeamCompatibilityStatus `json:"compatibility,omitempty"`
	UID               string                                   `json:"uid,omitempty"`
	ResourceVersion   string                                   `json:"resourceVersion,omitempty"`
	AgentPolicyRef    string                                   `json:"agentPolicyRef"`
	SandboxRefs       []string                                 `json:"sandboxRefs"`
	DefaultSandboxRef string                                   `json:"defaultSandboxRef"`
	Name              string                                   `json:"name"`
	Role              string                                   `json:"role,omitempty"`
	Description       []string                                 `json:"description,omitempty"`
	Skills            []string                                 `json:"skills,omitempty"`
	SkillStorage      *TeamSkillStorageDTO                     `json:"skillStorage,omitempty"`
	CreatedAt         string                                   `json:"createdAt,omitempty"`
	// ModelRouters are the Team's smart-routing models and
	// ModelRouterStatus the operator's report on them.
	ModelRouters      []TeamModelRouterDTO       `json:"modelRouters,omitempty"`
	ModelRouterStatus []TeamModelRouterStatusDTO `json:"modelRouterStatus,omitempty"`
}

// TeamUpsertRequest is the JSON body for create/update.
// For create, Name is taken from the URL or body; for update, Name is the
// URL path parameter and the body's name (if set) must match.
type TeamUpsertRequest struct {
	UID               string               `json:"uid,omitempty"`
	ResourceVersion   string               `json:"resourceVersion,omitempty"`
	AgentPolicyRef    string               `json:"agentPolicyRef"`
	SandboxRefs       []string             `json:"sandboxRefs"`
	DefaultSandboxRef string               `json:"defaultSandboxRef"`
	Name              string               `json:"name,omitempty"`
	Role              string               `json:"role,omitempty"`
	Description       []string             `json:"description,omitempty"`
	Skills            []string             `json:"skills,omitempty"`
	SkillStorage      *TeamSkillStorageDTO `json:"skillStorage,omitempty"`
	ModelRouters      []TeamModelRouterDTO `json:"modelRouters,omitempty"`
}

func toTeamDTO(t *cetirunv1alpha1.Team) TeamDTO {
	dto := TeamDTO{
		Generation: t.Generation, Deleting: !t.DeletionTimestamp.IsZero(),
		Compatibility: teamCompatibilityObservation(t),
		UID:           string(t.UID), ResourceVersion: t.ResourceVersion,
		AgentPolicyRef: t.Spec.AgentPolicyRef, SandboxRefs: append([]string{}, t.Spec.SandboxRefs...), DefaultSandboxRef: t.Spec.DefaultSandboxRef,
		Name:        t.Name,
		Role:        t.Spec.MemberContext.Role,
		Description: t.Spec.MemberContext.Description,
		Skills:      t.Spec.Skills,
		CreatedAt:   t.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}
	dto.ModelRouters, dto.ModelRouterStatus = toTeamModelRouterDTOs(t)
	if t.Spec.SkillStorage != nil {
		dto.SkillStorage = &TeamSkillStorageDTO{
			ResourceGroup:    t.Spec.SkillStorage.ResourceGroup,
			StorageAccount:   t.Spec.SkillStorage.StorageAccount,
			ContainerName:    t.Spec.SkillStorage.ContainerName,
			StorageClassName: t.Spec.SkillStorage.StorageClassName,
			StorageSize:      t.Spec.SkillStorage.StorageSize,
			MountPath:        t.Spec.SkillStorage.MountPath,
		}
	}
	return dto
}

func toTeamSpec(req TeamUpsertRequest) cetirunv1alpha1.TeamSpec {
	spec := cetirunv1alpha1.TeamSpec{
		AgentPolicyRef: req.AgentPolicyRef, SandboxRefs: append([]string(nil), req.SandboxRefs...), DefaultSandboxRef: req.DefaultSandboxRef,
		MemberContext: cetirunv1alpha1.TeamContextSpec{
			Role:        req.Role,
			Description: req.Description,
		},
		Skills:       req.Skills,
		ModelRouters: toTeamModelRouters(req.ModelRouters),
	}
	if req.SkillStorage != nil {
		spec.SkillStorage = &cetirunv1alpha1.SkillStorageSpec{
			ResourceGroup:    req.SkillStorage.ResourceGroup,
			StorageAccount:   req.SkillStorage.StorageAccount,
			ContainerName:    req.SkillStorage.ContainerName,
			StorageClassName: req.SkillStorage.StorageClassName,
			StorageSize:      req.SkillStorage.StorageSize,
			MountPath:        req.SkillStorage.MountPath,
		}
	}
	return spec
}

// validateSkillStorage returns a non-empty error message when the optional
// skillStorage block is present but missing CRD-required fields. The fields
// resourceGroup, storageAccount, containerName are marked required in the
// Team CRD, so accepting empty values would persist an invalid spec.
func validateSkillStorage(s *TeamSkillStorageDTO) string {
	if s == nil {
		return ""
	}
	switch {
	case strings.TrimSpace(s.ResourceGroup) == "":
		return "skillStorage.resourceGroup is required"
	case strings.TrimSpace(s.StorageAccount) == "":
		return "skillStorage.storageAccount is required"
	case strings.TrimSpace(s.ContainerName) == "":
		return "skillStorage.containerName is required"
	}
	return ""
}

// ListTeams handles GET /api/v1/admin/teams.
func (h *AdminTeamHandler) ListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := h.Kube.ListTeams(r.Context())
	if err != nil {
		slog.Error("admin: list teams failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list teams")
		return
	}
	out := make([]TeamDTO, 0, len(teams))
	for i := range teams {
		out = append(out, toTeamDTO(&teams[i]))
	}
	h.refreshCompatibilityReferences(r.Context(), out)
	writeJSON(w, http.StatusOK, map[string]any{"teams": out})
}

// GetTeam handles GET /api/v1/admin/teams/{name}.
func (h *AdminTeamHandler) GetTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !teamNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid team name")
		return
	}
	team, err := h.Kube.GetTeam(r.Context(), name)
	if err != nil {
		if kube.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "team_not_found", "Team "+name+" not found")
			return
		}
		slog.Error("admin: get team failed", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to get team")
		return
	}
	dtos := []TeamDTO{toTeamDTO(team)}
	h.refreshCompatibilityReferences(r.Context(), dtos)
	writeJSON(w, http.StatusOK, dtos[0])
}

// CreateTeam handles POST /api/v1/admin/teams.
func (h *AdminTeamHandler) CreateTeam(w http.ResponseWriter, r *http.Request) {
	var req TeamUpsertRequest
	if !decodeConfigurationRequest(w, r, &req) {
		return
	}
	if !teamNamePattern.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_name",
			"name must be a DNS-1123 label (start with letter, lowercase, max 63 chars)")
		return
	}
	if msg := validateSkillStorage(req.SkillStorage); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_skill_storage", msg)
		return
	}
	if !h.validateTeamBindings(w, r, req, nil) {
		return
	}
	team := &cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       toTeamSpec(req),
	}
	created, err := h.Kube.CreateTeam(r.Context(), team)
	if err != nil {
		writeConfigurationError(w, "create", "Team", req.Name, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTeamDTO(created))
}

// UpdateTeam handles PUT /api/v1/admin/teams/{name}. The request body
// replaces the complete Team configuration at the version the administrator read.
func (h *AdminTeamHandler) UpdateTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !teamNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid team name")
		return
	}
	var req TeamUpsertRequest
	if !decodeConfigurationRequest(w, r, &req) {
		return
	}
	if req.Name != "" && req.Name != name {
		writeError(w, http.StatusBadRequest, "name_mismatch",
			"body.name does not match URL name")
		return
	}
	if msg := validateSkillStorage(req.SkillStorage); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_skill_storage", msg)
		return
	}
	version, valid := configurationVersion(req.UID, req.ResourceVersion)
	if !valid {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Team are required")
		return
	}
	var previous []string
	if h.Config.SkillMarketplaceEnabled && len(cetirunv1alpha1.SkillMarketplaceSelection(req.Skills)) > 0 {
		// Only marketplace skills the Team does not select yet must be in
		// the catalog.
		stored, err := h.Kube.GetTeam(r.Context(), name)
		if err != nil {
			writeConfigurationError(w, "read", "Team", name, err)
			return
		}
		previous = stored.Spec.Skills
	}
	if !h.validateTeamBindings(w, r, req, previous) {
		return
	}
	updated, err := h.Kube.UpdateTeamSpec(r.Context(), name, toTeamSpec(req), version)
	if err != nil {
		writeConfigurationError(w, "update", "Team", name, err)
		return
	}
	writeJSON(w, http.StatusOK, toTeamDTO(updated))
}

// DeleteTeam handles DELETE /api/v1/admin/teams/{name}.
func (h *AdminTeamHandler) DeleteTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !teamNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Invalid team name")
		return
	}
	var req struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	}
	if !decodeConfigurationRequest(w, r, &req) {
		return
	}
	version, valid := configurationVersion(req.UID, req.ResourceVersion)
	if !valid {
		writeError(w, http.StatusPreconditionRequired, "version_required", "uid and resourceVersion from the current Team are required")
		return
	}
	if err := h.Kube.DeleteTeam(r.Context(), name, version); err != nil {
		writeConfigurationError(w, "delete", "Team", name, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Validate Portal-managed bindings before asking admission to check runtime
// prerequisites and current consumers. References are resolved on each request.
// previous holds the stored Team's skills on update, nil on creation.
func (h *AdminTeamHandler) validateTeamBindings(w http.ResponseWriter, r *http.Request, req TeamUpsertRequest, previous []string) bool {
	invalid := func(message string) bool {
		writeError(w, http.StatusBadRequest, "invalid_team_binding", message)
		return false
	}
	if !validAgentPolicyResourceName(req.AgentPolicyRef) {
		return invalid("Choose an Agent Policy")
	}
	if len(req.SandboxRefs) == 0 {
		return invalid("Choose at least one allowed Sandbox")
	}
	if !slices.Contains(req.SandboxRefs, req.DefaultSandboxRef) {
		return invalid("The default Sandbox must be one of the allowed Sandboxes")
	}
	seen := map[string]bool{}
	for _, name := range req.SandboxRefs {
		if !validAgentPolicyResourceName(name) || seen[name] {
			return invalid("Allowed Sandboxes must contain unique, valid resource names")
		}
		seen[name] = true
	}
	policy, err := h.Kube.GetAgentPolicy(r.Context(), req.AgentPolicyRef)
	if err != nil {
		writeConfigurationError(w, "read", "AgentPolicy", req.AgentPolicyRef, err)
		return false
	}
	if !policy.DeletionTimestamp.IsZero() {
		return invalid("The selected Agent Policy is being deleted")
	}
	if len(req.SandboxRefs) > 1 && !policy.Spec.SandboxSelection.AllowMultiple {
		return invalid("The selected Agent Policy allows only one Sandbox")
	}
	if h.Config.SkillMarketplaceEnabled {
		if message := invalidMarketplaceSkillName(req.Skills); message != "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_team", message)
			return false
		}
	}
	for _, skill := range req.Skills {
		if !slices.Contains(policy.Spec.Skills.Allowed, skill) {
			return invalid("Team Skills must be allowed by the selected Agent Policy")
		}
	}
	if h.Config.SkillMarketplaceEnabled && !h.validateNewMarketplaceSkills(w, r, req.Skills, previous) {
		return false
	}
	if message := validateTeamModelRouters(req.ModelRouters); message != "" {
		return invalid(message)
	}
	for _, name := range req.SandboxRefs {
		box, err := h.Kube.GetSandbox(r.Context(), name)
		if err != nil {
			writeConfigurationError(w, "read", "Sandbox", name, err)
			return false
		}
		if !box.DeletionTimestamp.IsZero() {
			return invalid("A selected Sandbox is being deleted")
		}
	}
	return true
}
