// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const CetiAdminValidationPath = cetiadmin.AdmissionValidationPath

// CetiAdminConfig comes from the Operator Chart, not an Agent. The deployment
// allowlist excludes identities added only for Portal Agent authoring.
type CetiAdminConfig struct {
	Namespace       string   `json:"namespace"`
	ConfigMapName   string   `json:"configMapName"`
	RuntimeUsername string   `json:"runtimeUsername"`
	StopUsername    string   `json:"stopUsername"`
	AdminUsers      []string `json:"adminUsers"`
	AdminGroups     []string `json:"adminGroups"`
}

func (c CetiAdminConfig) validate(controllerNamespace string) error {
	if c.Namespace != controllerNamespace || len(validation.IsDNS1123Subdomain(c.ConfigMapName)) != 0 {
		return fmt.Errorf("CetiAdmin admission requires its exact configuration in the controller namespace")
	}
	parts := strings.Split(c.StopUsername, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] != c.Namespace || len(validation.IsDNS1123Subdomain(parts[3])) != 0 {
		return fmt.Errorf("CetiAdmin stop identity must name a dedicated ServiceAccount in the controller namespace")
	}
	parts = strings.Split(c.RuntimeUsername, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" ||
		len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[3])) != 0 || c.RuntimeUsername == c.StopUsername {
		return fmt.Errorf("CetiAdmin runtime identity must name its dedicated namespaced ServiceAccount")
	}
	if len(c.AdminUsers)+len(c.AdminGroups) == 0 {
		return fmt.Errorf("CetiAdmin source protection requires the deployment administrator allowlist")
	}
	for _, user := range c.AdminUsers {
		if !exactIdentity(user) {
			return fmt.Errorf("CetiAdmin deployment administrators must be exact identities")
		}
	}
	for _, group := range c.AdminGroups {
		if !exactIdentity(group) || blanketGroup(group) {
			return fmt.Errorf("CetiAdmin deployment groups cannot use blanket authentication groups")
		}
	}
	return nil
}

func (c CetiAdminConfig) administrator(user authenticationv1.UserInfo) bool {
	return (Config{AdminUsers: c.AdminUsers, AdminGroups: c.AdminGroups}).administrator(user)
}

type CetiAdminHandler struct {
	reader client.Reader
	config Config
}

func NewCetiAdminHandler(reader client.Reader, config Config) (*CetiAdminHandler, error) {
	if reader == nil || config.CetiAdmin == nil {
		return nil, fmt.Errorf("CetiAdmin admission requires a reader and a configuration source")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	copy := *config.CetiAdmin
	copy.AdminUsers, copy.AdminGroups = slices.Clone(copy.AdminUsers), slices.Clone(copy.AdminGroups)
	config.CetiAdmin = &copy
	return &CetiAdminHandler{reader: reader, config: config}, nil
}

type cetiAdminSnapshot struct {
	source *corev1.ConfigMap
	state  *cetiadmin.Installation
}

func (h *CetiAdminHandler) snapshot(ctx context.Context) (cetiAdminSnapshot, error) {
	var snapshot cetiAdminSnapshot
	config := h.config.CetiAdmin
	source := &corev1.ConfigMap{}
	err := h.reader.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: config.ConfigMapName}, source)
	if err == nil {
		snapshot.source = source
	} else if !apierrors.IsNotFound(err) {
		return snapshot, err
	}
	object := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: config.Namespace, Name: cetiadmin.InstallationName(config.Namespace, config.ConfigMapName)}
	if err := h.reader.Get(ctx, key, object); err != nil {
		return snapshot, client.IgnoreNotFound(err)
	}
	state, err := cetiadmin.ParseInstallation(object.Data[cetiadmin.BindingKey])
	if err != nil || state.Source.Namespace != config.Namespace || state.Source.Name != config.ConfigMapName || object.Annotations[cetiadmin.AnnotationSourceUID] != string(state.Source.UID) {
		return snapshot, fmt.Errorf("CetiAdmin installation record is invalid")
	}
	if state.CanRun() && (state.RecordUID == "" || state.RecordUID != object.UID) {
		return snapshot, fmt.Errorf("CetiAdmin installation record UID changed")
	}
	snapshot.state = &state
	return snapshot, nil
}

func (h *CetiAdminHandler) currentBinding(ctx context.Context, snapshot cetiAdminSnapshot, user authenticationv1.UserInfo) (cetiadmin.Binding, error) {
	state := snapshot.state
	if state == nil || !state.CanRun() || snapshot.source == nil {
		return cetiadmin.Binding{}, cetiadmin.ErrUnbound
	}
	if !authenticated(user) || user.UID == "" || types.UID(user.UID) != state.ServiceAccount.UID ||
		user.Username != serviceAccountUsername(state.ServiceAccount) {
		return cetiadmin.Binding{}, fmt.Errorf("CetiAdmin caller does not match the current ServiceAccount UID")
	}
	config, err := cetiadmin.ParseConfig(snapshot.source.Data[cetiadmin.ConfigKey])
	if err != nil {
		return cetiadmin.Binding{}, err
	}
	observed := cetiadmin.ObservedInstallation{Source: snapshot.source, Agent: &api.Agent{}, Team: &api.Team{},
		Namespace: &corev1.Namespace{}, ServiceAccount: &corev1.ServiceAccount{}}
	for _, item := range []struct {
		identity cetiadmin.ObjectIdentity
		object   client.Object
	}{{state.Agent, observed.Agent}, {state.Team, observed.Team}, {state.Namespace, observed.Namespace}, {state.ServiceAccount, observed.ServiceAccount}} {
		if item.identity.Name == "" || item.identity.UID == "" {
			return cetiadmin.Binding{}, cetiadmin.ErrUnbound
		}
		if err := h.reader.Get(ctx, client.ObjectKey{Name: item.identity.Name, Namespace: item.identity.Namespace}, item.object); err != nil {
			return cetiadmin.Binding{}, err
		}
	}
	if observed.Agent.Spec.Suspended {
		return cetiadmin.Binding{}, fmt.Errorf("CetiAdmin is suspended")
	}
	return cetiadmin.ResolveBinding(config, &state.Record, observed)
}

func serviceAccountUsername(identity cetiadmin.ObjectIdentity) string {
	if identity.Namespace == "" || identity.Name == "" {
		return ""
	}
	return "system:serviceaccount:" + identity.Namespace + ":" + identity.Name
}

func (h *CetiAdminHandler) Validate(ctx context.Context, request admission.Request) admission.Response {
	ctx, cancel := context.WithTimeout(ctx, DefaultPodRequestTimeout)
	defer cancel()
	if response, handled := h.protectSource(request); handled {
		return response
	}
	snapshot, err := h.snapshot(ctx)
	if err != nil {
		return admission.Errored(http.StatusServiceUnavailable, fmt.Errorf("CetiAdmin authority cannot be read"))
	}
	isCetiAdmin := request.UserInfo.Username == h.config.CetiAdmin.RuntimeUsername ||
		(snapshot.state != nil && request.UserInfo.Username == serviceAccountUsername(snapshot.state.ServiceAccount))
	var binding cetiadmin.Binding
	if isCetiAdmin {
		binding, err = h.currentBinding(ctx, snapshot, request.UserInfo)
		if err != nil {
			return admission.Denied("CetiAdmin requires its current enabled installation and ServiceAccount UID")
		}
	}
	if response, handled := h.maintenanceGuard(ctx, request, snapshot); handled && !response.Allowed {
		return response
	}
	if response, handled := h.protectInstallation(request, snapshot); handled {
		return response
	}
	if response, handled := h.protectNativeAuthMarker(request, snapshot); handled {
		return response
	}
	if request.Resource.Group == api.GroupVersion.Group && request.Resource.Resource == "agents" {
		var next, old metav1.PartialObjectMetadata
		_ = decodeJSON(request.Object.Raw, &next, false)
		_ = decodeJSON(request.OldObject.Raw, &old, false)
		if next.Annotations[registryauth.ExecAdminBindingAnnotation] != old.Annotations[registryauth.ExecAdminBindingAnnotation] &&
			(!authenticated(request.UserInfo) || request.UserInfo.Username != h.config.ControllerUsername) {
			return admission.Denied("only the Operator may change a target's exec admin binding")
		}
	}
	if !isCetiAdmin {
		return admission.Allowed("request is not from the bound CetiAdmin")
	}
	response := h.validateCetiAdminOperation(ctx, request, binding, snapshot.state)
	if response.Allowed && request.Resource.Group == api.GroupVersion.Group && request.Resource.Resource == "agents" {
		governance, err := NewHandler(h.reader, h.config)
		if err != nil {
			return admission.Denied("Agent policy validation is unavailable")
		}
		return governance.Validate(ctx, request)
	}
	return response
}

func (h *CetiAdminHandler) protectNativeAuthMarker(request admission.Request, snapshot cetiAdminSnapshot) (admission.Response, bool) {
	if (request.Resource.Group != api.GroupVersion.Group || request.Resource.Resource != "agents") &&
		(request.Resource.Group != "" || request.Resource.Resource != "secrets") {
		return admission.Response{}, false
	}
	var next, old metav1.PartialObjectMetadata
	_ = decodeJSON(request.Object.Raw, &next, false)
	_ = decodeJSON(request.OldObject.Raw, &old, false)
	if next.Annotations[nativeAuthRotationAnnotation] == old.Annotations[nativeAuthRotationAnnotation] {
		return admission.Response{}, false
	}
	if authenticated(request.UserInfo) && (request.UserInfo.Username == h.config.ControllerUsername || h.config.CetiAdmin.administrator(request.UserInfo)) {
		return admission.Response{}, false
	}
	if snapshot.state != nil && request.UserInfo.Username == serviceAccountUsername(snapshot.state.ServiceAccount) {
		return admission.Response{}, false // The normal operation check validates its UID, target and fields.
	}
	return admission.Denied("native auth rotation intent and receipts require the current CetiAdmin or deployment authority"), true
}

func (h *Handler) cetiAdminMember(ctx context.Context, request admission.Request) bool {
	if h.config.CetiAdmin == nil || !strings.HasPrefix(request.UserInfo.Username, "system:serviceaccount:") {
		return false
	}
	guard := &CetiAdminHandler{reader: h.reader, config: h.config}
	snapshot, err := guard.snapshot(ctx)
	if err != nil || snapshot.state == nil {
		return false
	}
	binding, err := guard.currentBinding(ctx, snapshot, request.UserInfo)
	return err == nil && guard.validateCetiAdminAgent(ctx, request, binding, snapshot.state).Allowed
}

func (h *CetiAdminHandler) protectSource(request admission.Request) (admission.Response, bool) {
	config := h.config.CetiAdmin
	if request.Resource.Group != "" || request.Resource.Resource != "configmaps" || request.Namespace != config.Namespace {
		return admission.Response{}, false
	}
	stateName := cetiadmin.InstallationName(config.Namespace, config.ConfigMapName)
	maintenance := strings.HasPrefix(request.Name, "cetiadmin-maintenance-")
	if request.Name != config.ConfigMapName && request.Name != stateName && !maintenance {
		return admission.Response{}, false
	}
	if request.SubResource != "" {
		return admission.Denied("CetiAdmin configuration has no writable subresource"), true
	}
	controller := authenticated(request.UserInfo) && request.UserInfo.Username == h.config.ControllerUsername
	if request.Name == stateName || maintenance {
		if !controller {
			return admission.Denied("only the Operator writes the CetiAdmin installation record"), true
		}
		return admission.Allowed("Operator installation checkpoint"), true
	}
	if config.administrator(request.UserInfo) {
		if request.Operation == admissionv1.Delete {
			return admission.Denied("retain the CetiAdmin source; use the stop hook before uninstalling"), true
		}
		var object corev1.ConfigMap
		if decodeJSON(request.Object.Raw, &object, false) != nil {
			return admission.Denied("CetiAdmin configuration must be a ConfigMap"), true
		}
		if _, err := cetiadmin.ParseConfig(object.Data[cetiadmin.ConfigKey]); err != nil {
			return admission.Denied("CetiAdmin configuration is invalid: " + err.Error()), true
		}
		return admission.Allowed("deployment administrator configuration"), true
	}
	if authenticated(request.UserInfo) && request.UserInfo.Username == config.StopUsername && request.Operation == admissionv1.Update {
		var next, old corev1.ConfigMap
		if decodeJSON(request.Object.Raw, &next, false) != nil || decodeJSON(request.OldObject.Raw, &old, false) != nil ||
			next.UID == "" || next.UID != old.UID || next.ResourceVersion == "" || next.ResourceVersion != old.ResourceVersion {
			return admission.Denied("CetiAdmin stop requires the current source UID and resourceVersion"), true
		}
		var nextConfig, oldConfig map[string]any
		if decodeJSON([]byte(next.Data[cetiadmin.ConfigKey]), &nextConfig, false) != nil || decodeJSON([]byte(old.Data[cetiadmin.ConfigKey]), &oldConfig, false) != nil || nextConfig["enabled"] != false {
			return admission.Denied("CetiAdmin stop only changes enabled to false"), true
		}
		delete(nextConfig, "enabled")
		delete(oldConfig, "enabled")
		next.Data[cetiadmin.ConfigKey] = old.Data[cetiadmin.ConfigKey]
		next.ManagedFields = old.ManagedFields
		if !reflect.DeepEqual(nextConfig, oldConfig) || !reflect.DeepEqual(next, old) {
			return admission.Denied("CetiAdmin stop must preserve all other configuration and metadata"), true
		}
		return admission.Allowed("conditional CetiAdmin disable intent"), true
	}
	return admission.Denied("CetiAdmin source is restricted to deployment administrators and the stop hook"), true
}

func (h *CetiAdminHandler) protectInstallation(request admission.Request, snapshot cetiAdminSnapshot) (admission.Response, bool) {
	var next, old metav1.PartialObjectMetadata
	_ = decodeJSON(request.Object.Raw, &next, false)
	_ = decodeJSON(request.OldObject.Raw, &old, false)
	marked := false
	for _, metadata := range []metav1.ObjectMeta{next.ObjectMeta, old.ObjectMeta} {
		for key := range metadata.Annotations {
			marked = marked || strings.HasPrefix(key, "cetirun.microsoft.com/cetiadmin-")
		}
	}
	protected := false
	if state := snapshot.state; state != nil {
		for _, item := range []struct {
			group, resource string
			identity        cetiadmin.ObjectIdentity
		}{{api.GroupVersion.Group, "agents", state.Agent}, {api.GroupVersion.Group, "teams", state.Team},
			{"", "namespaces", state.Namespace}, {"", "serviceaccounts", state.ServiceAccount}} {
			protected = protected || (request.Resource.Group == item.group && request.Resource.Resource == item.resource &&
				request.Name == item.identity.Name && request.Namespace == item.identity.Namespace && item.identity.Name != "")
		}
		if request.Namespace == "" && request.Resource.Group == api.GroupVersion.Group &&
			(request.Resource.Resource == "agents" || request.Resource.Resource == "teams") && request.Name == state.Configuration.InstanceName {
			protected = true
		}
	}
	if !marked && !protected {
		return admission.Response{}, false
	}
	if authenticated(request.UserInfo) && request.UserInfo.Username == h.config.ControllerUsername {
		return admission.Allowed("Operator maintains the installed CetiAdmin objects"), true
	}
	if request.Resource.Group == "" && request.Resource.Resource == "serviceaccounts" && request.SubResource == "token" &&
		request.Operation == admissionv1.Create && (snapshot.state == nil || request.UserInfo.Username != serviceAccountUsername(snapshot.state.ServiceAccount)) {
		return admission.Allowed("TokenRequest authorization and Pod binding remain Kubernetes-controlled; the ServiceAccount is unchanged"), true
	}
	if request.Resource.Group == "" && request.Resource.Resource == "namespaces" && request.SubResource == "status" {
		return admission.Allowed("namespace status remains Kubernetes-controlled"), true
	}
	if request.Resource.Group == api.GroupVersion.Group && request.Resource.Resource == "agents" && request.Operation == admissionv1.Update && request.SubResource == "" &&
		snapshot.state != nil && snapshot.state.Bootstrapped && !snapshot.state.BootstrapReleasePending &&
		request.UserInfo.Username != serviceAccountUsername(snapshot.state.ServiceAccount) {
		if rawChangesOnly(request, []string{"suspended", "restartRequestID"}, []string{"managedFields", "generation"}) {
			return admission.Allowed("retain owner suspension and restart controls without changing the installation"), true
		}
		installed := snapshot.state.Agent
		if h.config.administrator(request.UserInfo) && installed.UID != "" &&
			request.Name == installed.Name && request.Namespace == installed.Namespace &&
			next.UID == installed.UID && old.UID == installed.UID &&
			rawChangesOnly(request, nil, []string{"managedFields", "generation"},
				api.AnnotationCopilotModels, api.AnnotationCopilotSignInGeneration, api.AnnotationCopilotSignedInAt) {
			return admission.Allowed("Portal authoring may record Copilot sign-in metadata on the installed Agent"), true
		}
	}
	return admission.Denied("CetiAdmin installation objects and source markers are Operator-owned"), true
}
