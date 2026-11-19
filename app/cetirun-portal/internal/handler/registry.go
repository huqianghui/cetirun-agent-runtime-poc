// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	strictjson "sigs.k8s.io/json"
)

var validCreateRequestID = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,128}$`)

func decodeBoundedJSON(w http.ResponseWriter, r *http.Request, target any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, registryauth.MaxBodyBytes))
	if err != nil {
		return errors.New("invalid body")
	}
	problems, err := strictjson.UnmarshalStrict(raw, target)
	if err != nil || len(problems) != 0 {
		return errors.New("invalid body")
	}
	return nil
}

// The gateway terminates TLS. Compare with its configured public origin, not a
// caller-controlled Forwarded header. JSON and same-origin checks reject CSRF.
func (h *InstanceHandler) registryOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin == "" {
		return true // non-browser clients still require gateway authentication
	}
	if h.Config.PortalOrigin != "" {
		return origin == strings.TrimRight(h.Config.PortalOrigin, "/")
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "https" && parsed.Host == r.Host && parsed.Path == "" && parsed.User == nil
}

func registryCaller(claims auth.Claims) string {
	if claims.TenantID != "" && claims.ObjectID != "" {
		return strings.ToLower(claims.TenantID + "/" + claims.ObjectID)
	}
	return strings.ToLower(claims.Email)
}

func registryOwner(instance *api.Agent, claims auth.Claims) bool {
	if !ownsInstance(instance, claims) {
		return false
	}
	if instance.Annotations[api.AnnotationOwnerObjectID] != "" {
		return true
	}
	if owner := instance.Annotations[kube.AnnotationCreateCaller]; owner != "" {
		return owner == registryCaller(claims) || (strings.Contains(owner, "@") && strings.EqualFold(owner, claims.Email))
	}
	return claims.Email != "" && strings.EqualFold(instance.Spec.User.Email, claims.Email)
}

func registryResponseConditions(instance *api.Agent, synchronized bool) []metav1.Condition {
	conditions := append([]metav1.Condition(nil), instance.Status.Conditions...)
	pending := instance.Spec.Agent.RegistrySecretRef != nil && !synchronized
	removing := instance.Spec.Agent.RegistrySecretRef == nil && instance.Status.RegistryCredentials != nil
	if pending || removing {
		current := meta.FindStatusCondition(conditions, api.ConditionRegistryCredentialsReady)
		if current == nil || current.Status == metav1.ConditionTrue {
			reason, message := "RegistrySyncPending", "Waiting for the current credentials to be synchronized"
			if removing {
				reason, message = "RegistryRemovalPending", "Waiting for the pull binding and credential copies to be removed"
			}
			meta.SetStatusCondition(&conditions, metav1.Condition{Type: api.ConditionRegistryCredentialsReady, Status: metav1.ConditionFalse, Reason: reason, Message: message})
		}
	}
	return conditions
}

func (h *InstanceHandler) registryInstance(w http.ResponseWriter, r *http.Request) *api.Agent {
	w.Header().Set("Cache-Control", "no-store")
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not_authenticated", "Authentication required")
		return nil
	}
	if !h.registryOriginAllowed(r) {
		writeError(w, http.StatusForbidden, "invalid_origin", "Request origin is not allowed")
		return nil
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance_not_found", "Instance not found")
		return nil
	}
	if !registryOwner(instance, claims) {
		writeError(w, http.StatusForbidden, "forbidden", "Only the instance owner may manage registry credentials")
		return nil
	}
	if instance.UID == "" || !instance.DeletionTimestamp.IsZero() || r.Header.Get("If-Match") != kube.RegistryETag(instance) {
		writeError(w, http.StatusConflict, "instance_changed", "Instance changed; refresh before changing registry credentials")
		return nil
	}
	return instance
}

func writeRegistryError(w http.ResponseWriter, err error, instance *api.Agent) {
	code, status, message := "registry_save_pending", http.StatusServiceUnavailable, "Registry credentials could not be saved. Retry for this existing instance."
	if errors.Is(err, kube.ErrRegistryConflict) {
		code, status, message = "instance_changed", http.StatusConflict, "Instance or credentials changed; refresh and try again."
	} else if errors.Is(err, registryauth.ErrIsolation) {
		code, message = "registry_isolation_unverified", "Registry credentials were not saved because workload permissions could not be verified."
	}
	writeJSON(w, status, struct {
		Error        ErrorDetail `json:"error"`
		InstanceName string      `json:"instanceName"`
		UID          string      `json:"uid"`
	}{Error: ErrorDetail{Code: code, Message: message}, InstanceName: instance.Name, UID: string(instance.UID)})
}

func (h *InstanceHandler) PutRegistryAuth(w http.ResponseWriter, r *http.Request) {
	instance := h.registryInstance(w, r)
	if instance == nil {
		return
	}
	var credentials registryauth.Credentials
	if decodeBoundedJSON(w, r, &credentials) != nil || credentials.Validate() != nil || instance.Spec.Agent.Image == nil {
		writeError(w, http.StatusBadRequest, "invalid_registry_auth", "Provide a complete username and password or token for an instance with a target image")
		return
	}
	if _, err := registryauth.Host(*instance.Spec.Agent.Image); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_image", "A valid target image is required")
		return
	}
	updated, err := h.Kube.BeginRegistryUpdate(r.Context(), instance, false)
	if err == nil {
		err = h.Kube.SaveRegistryCredentials(r.Context(), updated, credentials, false)
	}
	if err != nil {
		writeRegistryError(w, err, instance)
		return
	}
	h.writeRegistryResponse(w, r, updated)
}

func (h *InstanceHandler) DeleteRegistryAuth(w http.ResponseWriter, r *http.Request) {
	instance := h.registryInstance(w, r)
	if instance == nil {
		return
	}
	updated, err := h.Kube.BeginRegistryUpdate(r.Context(), instance, true)
	if err != nil {
		writeRegistryError(w, err, instance)
		return
	}
	h.writeRegistryResponse(w, r, updated)
}

func (h *InstanceHandler) writeRegistryResponse(w http.ResponseWriter, r *http.Request, instance *api.Agent) {
	response, err := h.toInstanceResponse(r.Context(), instance)
	if err != nil {
		writeRegistryError(w, kube.ErrRegistrySave, instance)
		return
	}
	w.Header().Set("ETag", kube.RegistryETag(instance))
	writeJSON(w, http.StatusOK, response)
}

// Only non-sensitive creation settings are fingerprinted. A retry cannot
// replace saved credentials, even if its password differs.
func registryCreateConfig(req CreateInstanceRequest) string {
	req.RegistryAuth = nil
	req.GatewayToken = ""
	req.TailscaleAuthKey = ""
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func registryCreateMatches(instance *api.Agent, req CreateInstanceRequest) bool {
	if req.Agent == nil {
		return false
	}
	wanted, err := req.Agent.Create()
	current := agentconfig.Defaults(instance.Spec.Agent)
	if err != nil || current.Type != wanted.Type || !reflect.DeepEqual(current.Image, wanted.Image) ||
		!reflect.DeepEqual(current.Username, wanted.Username) || instanceTeam(instance) != req.Team || instance.Spec.Model != strings.TrimSpace(req.Model) {
		return false
	}
	// Built-in default ports can be pinned by the Operator after creation.
	if (wanted.Type == api.AgentTypeCustom || wanted.Port != nil) && !reflect.DeepEqual(current.Port, wanted.Port) {
		return false
	}
	return (instance.Spec.AgentIdentity != nil && instance.Spec.AgentIdentity.Enabled) == req.AgentIdentityEnabled
}

func (h *InstanceHandler) resumeRegistryCreate(w http.ResponseWriter, r *http.Request, name string, req CreateInstanceRequest, claims auth.Claims) bool {
	existing, err := h.Kube.GetInstanceByName(r.Context(), name)
	if kube.IsNotFound(err) {
		instances, listErr := h.Kube.ListAllInstances(r.Context())
		if listErr != nil {
			writeError(w, http.StatusServiceUnavailable, "instance_unavailable", "Unable to inspect the creation request")
			return true
		}
		for i := range instances {
			candidate := &instances[i]
			if candidate.Annotations[kube.AnnotationCreateRequest] == r.Header.Get("Idempotency-Key") && registryOwner(candidate, claims) {
				existing, err = candidate, nil
				break
			}
		}
		if existing == nil {
			return false
		}
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "instance_unavailable", "Unable to inspect the existing instance")
		return true
	}
	if existing.Name != name || !registryOwner(existing, claims) || existing.UID == "" || !existing.DeletionTimestamp.IsZero() ||
		!registryCreateMatches(existing, req) ||
		existing.Annotations[kube.AnnotationCreateRequest] != r.Header.Get("Idempotency-Key") ||
		existing.Annotations[kube.AnnotationCreateConfig] != registryCreateConfig(req) ||
		existing.Spec.Agent.RegistrySecretRef == nil ||
		existing.Spec.Agent.RegistrySecretRef.Name != existing.Annotations[kube.AnnotationCreateRegistrySource] {
		writeError(w, http.StatusConflict, "instance_exists", "This name is already in use; manage credentials on the existing instance")
		return true
	}
	// A retry resumes the persisted Agent, whose explicit source switches can
	// differ from today's defaults for the original request's omitted fields.
	// Revalidate that actual configuration before writing its credentials.
	persisted := req
	if access := existing.Spec.ModelAccess; access != nil {
		persisted.ModelAccess = &ModelAccessRequest{SharedModels: access.SharedModels, GitHubCopilot: access.GitHubCopilot}
	}
	if _, _, _, ok := h.validateAIGatewaySelection(w, r, persisted, existing.Spec.Agent.Type, false); !ok {
		return true
	}
	if err := h.Kube.SaveRegistryCredentials(r.Context(), existing, *req.RegistryAuth, true); err != nil {
		writeRegistryError(w, err, existing)
		return true
	}
	h.writeRegistryResponse(w, r, existing)
	return true
}
