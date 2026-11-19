// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	ValidationPath = "/validate-sandbox-configuration"
	DefaultingPath = cetiadmin.AgentDefaultingPath
)

type Handler struct {
	reader            client.Reader
	config            Config
	registrations     map[api.SandboxType]sandbox.Registration
	platformSource    sandboxplatform.Source
	platformSnapshot  *sandboxplatform.Snapshot
	nativeGrantChecks []nativePolicyGrantCheck
	coordination      *sandboxcoordination.Store
	requestTimeout    time.Duration
}

func NewHandler(reader client.Reader, config Config) (*Handler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("Sandbox admission requires an uncached resource reader")
	}
	config.AdminUsers = slices.Clone(config.AdminUsers)
	config.AdminGroups = slices.Clone(config.AdminGroups)
	config.TeamAssignments = slices.Clone(config.TeamAssignments)
	for i := range config.TeamAssignments {
		config.TeamAssignments[i].Users = slices.Clone(config.TeamAssignments[i].Users)
		config.TeamAssignments[i].Groups = slices.Clone(config.TeamAssignments[i].Groups)
	}
	handler := &Handler{reader: reader, config: config, requestTimeout: DefaultPodRequestTimeout}
	if config.PlatformConfigMapName != "" {
		handler.platformSource = &sandboxplatform.KubernetesSource{
			APIReader: reader, Namespace: config.PlatformNamespace, Name: config.PlatformConfigMapName,
		}
	}
	if config.CoordinationConfigMapName != "" {
		handler.coordination = &sandboxcoordination.Store{
			APIReader: reader, Namespace: strings.Split(config.ControllerUsername, ":")[2],
			Identity: w.ObjectIdentity{Name: config.CoordinationConfigMapName, UID: types.UID(config.CoordinationConfigMapUID)},
		}
	}
	return handler, nil
}

func (h *Handler) Validate(ctx context.Context, request admission.Request) admission.Response {
	return h.withPlatformSnapshot(ctx, request, (*Handler).validateCoordinated)
}

func (h *Handler) validate(ctx context.Context, request admission.Request) admission.Response {
	if request.SubResource == "status" {
		return admission.Allowed("status authorization belongs to Kubernetes RBAC")
	}
	if request.Operation == admissionv1.Delete && request.Resource.Resource == "agents" {
		return admission.Allowed("Agent deletion must remain available")
	}
	object, old, err := decodeRequest(request, h.config.ValidationMode)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if request.Operation != admissionv1.Delete && !h.config.administrator(request.UserInfo) && unknownSpecChanged(request, object) {
		return admission.Denied("changes to unrecognized Agent or Team spec fields require an administrator or a matching admission version")
	}
	if garbageCollectorFinalizerUpdate(request, object, old) {
		return admission.Allowed("Kubernetes garbage collector metadata only")
	}
	if agent, ok := object.(*api.Agent); ok {
		var previous *api.Agent
		if old != nil {
			previous = old.(*api.Agent)
		}
		return h.validateAgent(ctx, request, agent, previous)
	}

	if team, ok := object.(*api.Team); ok && request.Operation == admissionv1.Update {
		previous := old.(*api.Team)
		if sameTeamPolicy(team.Spec, previous.Spec) {
			if sandbox.TeamPolicyConfigured(team) && !slices.Equal(team.Spec.Skills, previous.Spec.Skills) {
				policy := &api.AgentPolicy{}
				if err := h.reader.Get(ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
					return dependencyError(ctx, err)
				}
				if err := teamSkillsAllowed(team, policy); err != nil {
					return admission.Denied(err.Error())
				}
			}
			return admission.Allowed("Team policy is unchanged")
		}
	}
	if team, ok := object.(*api.Team); ok &&
		(request.Operation == admissionv1.Create || request.Operation == admissionv1.Delete) && !sandbox.TeamPolicyConfigured(team) {
		return admission.Allowed("legacy Team")
	}
	if !h.config.administrator(request.UserInfo) {
		return admission.Denied("Sandbox configuration writes require an explicitly authorized administrator")
	}
	if request.Operation == admissionv1.Delete {
		return h.validateDeletion(ctx, object)
	}

	switch candidate := object.(type) {
	case *api.Team:
		if !sandbox.TeamPolicyConfigured(candidate) {
			if response := h.checkTeamConsumers(ctx, candidate, true); !response.Allowed {
				return response
			}
			return admission.Allowed("unreferenced Team policy removed")
		}
		resources, err := h.resources(ctx, candidate.Spec, sandbox.Resources{})
		if err != nil {
			return dependencyError(ctx, err)
		}
		registrations, err := h.platformRegistrations(ctx)
		if err != nil {
			return dependencyError(ctx, err)
		}
		if errs := sandbox.ValidateTeamConfiguration(candidate.Spec, resources, registrations); len(errs) != 0 {
			return compatibilityDenied(errs.ToAggregate().Error(), sandbox.TeamCompatibilityReport(candidate, resources, registrations, h.config.NativeExecution))
		}
		if err := h.checkTeamExecutionProfiles(ctx, candidate.Spec, resources, registrations); err != nil {
			return compatibilityDenied(err.Error(), h.executionCompatibilityReport(ctx, candidate, resources, registrations))
		}
		if err := teamSkillsAllowed(candidate, resources.AgentPolicies[candidate.Spec.AgentPolicyRef]); err != nil {
			return admission.Denied(err.Error())
		}
		return h.checkTeamConsumers(ctx, candidate, false)
	case *api.Sandbox:
		if _, errs := sandbox.NormalizeSandbox(candidate.Spec); len(errs) != 0 {
			return admission.Denied(errs.ToAggregate().Error())
		}
	case *api.AgentPolicy:
		if _, errs := sandbox.NormalizeAgentPolicy(candidate.Spec); len(errs) != 0 {
			return admission.Denied(errs.ToAggregate().Error())
		}
	}
	return h.checkReferencingTeams(ctx, object)
}

func (h *Handler) DefaultAgent(ctx context.Context, request admission.Request) admission.Response {
	return h.withPlatformSnapshot(ctx, request, (*Handler).defaultAgent)
}

func (h *Handler) defaultAgent(ctx context.Context, request admission.Request) admission.Response {
	if (request.Operation != admissionv1.Create && request.Operation != admissionv1.Update) || request.Resource.Resource != "agents" {
		return admission.Denied("Sandbox selection defaulting applies to Agent creation or explicit update reauthorization")
	}
	object, old, err := decodeRequest(request, h.config.ValidationMode)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	agent := object.(*api.Agent)
	if old != nil {
		previous := old.(*api.Agent)
		return h.reauthorizeAgent(ctx, request, agent, previous)
	}
	return h.defaultAgentCreation(ctx, request, agent)
}

func (h *Handler) validateAgent(ctx context.Context, request admission.Request, agent, old *api.Agent) admission.Response {
	if response, handled := h.reconciledHoldRelease(ctx, request, agent, old); handled {
		return response
	}
	if old != nil && !slices.Equal(agent.Finalizers, old.Finalizers) {
		if h.config.NativeExecution && request.UserInfo.Username == h.config.ControllerUsername &&
			authenticated(request.UserInfo) && nativeHoldReleaseOnly(request, agent, old) {
			return h.validateNativeHoldRelease(ctx, request, old)
		}
		if request.UserInfo.Username == h.config.ControllerUsername && controllerFinalizerUpdate(agent, old) && rawMetadataOnly(request) {
			return admission.Allowed("controller lifecycle metadata only")
		}
		if changedLifecycleFinalizers(agent.Finalizers, old.Finalizers) {
			return admission.Denied("controller lifecycle finalizers may only change in a controller metadata-only update")
		}
	}
	if old == nil {
		for _, finalizer := range agent.Finalizers {
			if finalizer != api.Finalizer && lifecycleFinalizer(finalizer) {
				return admission.Denied("controller lifecycle markers cannot be supplied during Agent creation")
			}
		}
	}
	if err := h.protectedAuthorization(request, agent, old); err != nil {
		return admission.Denied(err.Error())
	}
	if err := h.protectedPolicyBudget(request, agent, old); err != nil {
		return admission.Denied(err.Error())
	}
	var previousMigration *api.PlannedMigrationRequest
	if old != nil {
		previousMigration = old.Spec.PlannedMigration
	}
	if !equality.Semantic.DeepEqual(agent.Spec.PlannedMigration, previousMigration) &&
		!h.config.administrator(request.UserInfo) {
		return admission.Denied("planned migration destinations require platform administrator authority")
	}
	assigned := ""
	if old == nil {
		name, assignment, err := h.creationTeam(request, agent)
		if err != nil {
			return admission.Denied(err.Error())
		}
		if name != agent.Spec.Team {
			return admission.Denied("the creation-defaulting webhook must persist the authenticated Team assignment")
		}
		assigned = assignment
	}
	team, err := h.team(ctx, agent.Spec.Team)
	if err != nil {
		return dependencyError(ctx, err)
	}
	if assigned != "" && team == nil {
		return admission.Denied("the administratively assigned Team does not exist")
	}
	if !sandbox.Requested(agent, team) && !sandbox.Requested(old, nil) &&
		!sandboxbinding.Present(agent) && !sandboxbinding.Present(old) {
		if old != nil && old.Spec.Team != agent.Spec.Team {
			previousTeam, err := h.team(ctx, old.Spec.Team)
			if err != nil {
				return dependencyError(ctx, err)
			}
			if sandbox.TeamPolicyConfigured(previousTeam) {
				return admission.Denied("policy-bound Agents cannot return to the legacy path by changing Team")
			}
		}
		return admission.Allowed("legacy Agent")
	}
	controllerDefaults := h.config.NativeExecution && request.UserInfo.Username == h.config.ControllerUsername &&
		authenticated(request.UserInfo) && controllerRuntimeDefaults(agent, old)
	if !h.config.administrator(request.UserInfo) && !controllerDefaults {
		if err := h.memberAuthority(ctx, request, agent); err != nil {
			return admission.Denied(err.Error())
		}
		if old != nil && !sameAgentSelection(agent, old) {
			return admission.Denied("members cannot change a policy-bound Agent's Team or Sandbox binding")
		}
	}
	if suspensionOnlyUpdate(request, agent, old) {
		return admission.Allowed("authorized suspension-only update; existing policy assignment is unchanged")
	}
	resources, response := h.agentAuthorizationResources(ctx, request.Operation, agent, team)
	if !response.Allowed {
		return response
	}
	return h.validateAgentAuthorization(request, agent, old, team, resources)
}

func (h *Handler) agentAuthorizationResources(ctx context.Context, operation admissionv1.Operation, agent *api.Agent, team *api.Team) (sandbox.Resources, admission.Response) {
	if !sandbox.TeamPolicyConfigured(team) {
		return sandbox.Resources{}, admission.Denied("policy-bound Agents require a policy-configured Team; clearing bindings does not release a hold")
	}
	if team.DeletionTimestamp != nil {
		return sandbox.Resources{}, admission.Denied("a terminating Team cannot authorize a policy-bound Agent")
	}
	spec, err := sandbox.DefaultAgentSelection(agent.Spec, team)
	if err != nil {
		return sandbox.Resources{}, admission.Denied(err.Error())
	}
	if spec.SandboxRef != agent.Spec.SandboxRef {
		if operation == admissionv1.Create {
			return sandbox.Resources{}, admission.Denied(fmt.Sprintf("Agent %q is missing its persisted selection; the creation-defaulting webhook must set Team %q's defaults before validation", agent.Name, team.Name))
		}
		return sandbox.Resources{}, admission.Denied("spec.sandboxRef must be persisted at creation; existing Agents are never re-defaulted")
	}
	resources, err := h.resources(ctx, team.Spec, sandbox.Resources{})
	if err != nil {
		return sandbox.Resources{}, dependencyError(ctx, err)
	}
	registrations, err := h.platformRegistrations(ctx)
	if err != nil {
		return sandbox.Resources{}, dependencyError(ctx, err)
	}
	if errs := sandbox.ValidateTeam(team, resources, registrations); len(errs) != 0 {
		return sandbox.Resources{}, admission.Denied(errs.ToAggregate().Error())
	}
	selected, errs := sandbox.NormalizeSandbox(resources.Sandboxes[spec.SandboxRef].Spec)
	if len(errs) != 0 {
		return sandbox.Resources{}, admission.Denied(errs.ToAggregate().Error())
	}
	if err := sandbox.CheckExecutionProfile(registrations[selected.Type].Capabilities, h.config.NativeExecution); err != nil {
		return sandbox.Resources{}, admission.Denied(err.Error())
	}
	if err := sandbox.CheckAgentProfile(registrations[selected.Type].Capabilities, spec.Agent.Type); err != nil {
		return sandbox.Resources{}, admission.Denied(err.Error())
	}
	if err := h.checkNativePolicyBinding(ctx, selected.Type, resources.AgentPolicies[team.Spec.AgentPolicyRef],
		registrations[selected.Type].Capabilities); err != nil {
		return sandbox.Resources{}, admission.Denied(err.Error())
	}
	return resources, admission.Allowed("")
}

// Reuse only within one admission request, never across requests. Reference
// checks must not accept a policy from a stale informer or TTL cache.
func (h *Handler) resources(ctx context.Context, spec api.TeamSpec, resources sandbox.Resources) (sandbox.Resources, error) {
	if resources.Sandboxes == nil {
		resources.Sandboxes = make(map[string]*api.Sandbox)
	}
	if resources.AgentPolicies == nil {
		resources.AgentPolicies = make(map[string]*api.AgentPolicy)
	}
	for _, name := range spec.SandboxRefs {
		if _, read := resources.Sandboxes[name]; name == "" || read {
			continue
		}
		var box api.Sandbox
		if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, &box); err != nil {
			if !apierrors.IsNotFound(err) {
				return sandbox.Resources{}, err
			}
			resources.Sandboxes[name] = nil
			continue
		}
		resources.Sandboxes[name] = &box
	}
	for _, name := range []string{spec.AgentPolicyRef} {
		if _, read := resources.AgentPolicies[name]; name == "" || read {
			continue
		}
		var policy api.AgentPolicy
		if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, &policy); err != nil {
			if !apierrors.IsNotFound(err) {
				return sandbox.Resources{}, err
			}
			resources.AgentPolicies[name] = nil
			continue
		}
		resources.AgentPolicies[name] = &policy
	}
	return resources, nil
}

func (h *Handler) team(ctx context.Context, name string) (*api.Team, error) {
	if name == "" {
		return nil, nil
	}
	var team api.Team
	if err := h.reader.Get(ctx, client.ObjectKey{Name: name}, &team); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &team, nil
}

func (h *Handler) checkTeamConsumers(ctx context.Context, team *api.Team, removing bool) admission.Response {
	var agents api.AgentList
	return h.checkPages(ctx, &agents, func() admission.Response {
		for _, agent := range agents.Items {
			if agent.Spec.Team != team.Name {
				continue
			}
			if removing {
				return admission.Denied(fmt.Sprintf("Team %q still has referencing Agent %q, including inherited policy; rebind it to another policy-configured Team or delete it before removing Team policy", team.Name, agent.Name))
			}
			if agent.Spec.SandboxRef != "" && !slices.Contains(team.Spec.SandboxRefs, agent.Spec.SandboxRef) {
				return admission.Denied(fmt.Sprintf("Team %q change would invalidate referencing Agent %q; rebind or delete its consumers first", team.Name, agent.Name))
			}
		}
		return admission.Allowed("Team bindings preserve existing consumers")
	})
}

func (h *Handler) checkReferencingTeams(ctx context.Context, candidate client.Object) admission.Response {
	var teams api.TeamList
	resources := sandbox.Resources{
		Sandboxes: make(map[string]*api.Sandbox), AgentPolicies: make(map[string]*api.AgentPolicy),
	}
	switch object := candidate.(type) {
	case *api.Sandbox:
		resources.Sandboxes[object.Name] = object
	case *api.AgentPolicy:
		resources.AgentPolicies[object.Name] = object
	}
	return h.checkPages(ctx, &teams, func() admission.Response {
		for i := range teams.Items {
			team := &teams.Items[i]
			if !references(team, candidate) {
				continue
			}
			var err error
			resources, err = h.resources(ctx, team.Spec, resources)
			if err != nil {
				return dependencyError(ctx, err)
			}
			registrations, err := h.platformRegistrations(ctx)
			if err != nil {
				return dependencyError(ctx, err)
			}
			if errs := sandbox.ValidateTeam(team, resources, registrations); len(errs) != 0 {
				return compatibilityDenied(fmt.Sprintf("configuration %q would invalidate referencing Team %q: %v", candidate.GetName(), team.Name, errs.ToAggregate()), sandbox.TeamCompatibilityReport(team, resources, registrations, h.config.NativeExecution))
			}
			if err := h.checkTeamExecutionProfiles(ctx, team.Spec, resources, registrations); err != nil {
				return compatibilityDenied(err.Error(), h.executionCompatibilityReport(ctx, team, resources, registrations))
			}
			if _, ok := candidate.(*api.AgentPolicy); ok {
				if err := teamSkillsAllowed(team, resources.AgentPolicies[team.Spec.AgentPolicyRef]); err != nil {
					return admission.Denied(fmt.Sprintf("configuration %q would invalidate referencing Team %q: %v", candidate.GetName(), team.Name, err))
				}
			}
		}
		return admission.Allowed("configuration is valid for its referencing Teams")
	})
}

// teamSkillsAllowed refuses a Team that selects a skill that its AgentPolicy
// does not allow. The operator denies the Agents of such a Team, which stops
// them and revokes their virtual keys, so the configuration is refused before
// it is stored, for a Team change and for a Policy change alike.
func teamSkillsAllowed(team *api.Team, policy *api.AgentPolicy) error {
	if policy == nil {
		return nil
	}
	var refused []string
	for _, skill := range team.Spec.Skills {
		if !slices.Contains(policy.Spec.Skills.Allowed, skill) {
			refused = append(refused, skill)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("Team %q selects skills that AgentPolicy %q does not allow in skills.allowed: %s; allow them in the Policy, or remove them from the Team",
		team.Name, policy.Name, strings.Join(refused, ", "))
}

func (h *Handler) validateDeletion(ctx context.Context, object client.Object) admission.Response {
	if team, ok := object.(*api.Team); ok {
		return h.checkTeamConsumers(ctx, team, true)
	}
	var teams api.TeamList
	response := h.checkPages(ctx, &teams, func() admission.Response {
		for i := range teams.Items {
			if references(&teams.Items[i], object) {
				return admission.Denied(fmt.Sprintf("configuration %q is still referenced by Team %q", object.GetName(), teams.Items[i].Name))
			}
		}
		return admission.Allowed("no referencing Teams")
	})
	if !response.Allowed {
		return response
	}
	// Check dangling direct Agent selections as well as Team references.
	if box, ok := object.(*api.Sandbox); ok {
		var agents api.AgentList
		return h.checkPages(ctx, &agents, func() admission.Response {
			for _, agent := range agents.Items {
				if agent.Spec.SandboxRef == box.Name {
					return admission.Denied(fmt.Sprintf("Sandbox %q is still referenced by Agent %q", box.Name, agent.Name))
				}
			}
			return admission.Allowed("Sandbox has no remaining references")
		})
	}
	return admission.Allowed("configuration has no remaining references")
}

const referencePageSize = 200

func (h *Handler) checkPages(ctx context.Context, list client.ObjectList, check func() admission.Response) admission.Response {
	for continuation := ""; ; continuation = list.GetContinue() {
		if err := ctx.Err(); err != nil {
			return dependencyError(ctx, err)
		}
		if err := h.reader.List(ctx, list, client.Limit(referencePageSize), client.Continue(continuation)); err != nil {
			return dependencyError(ctx, err)
		}
		response := check()
		if !response.Allowed || list.GetContinue() == "" {
			return response
		}
	}
}

func references(team *api.Team, object client.Object) bool {
	switch object.(type) {
	case *api.Sandbox:
		return slices.Contains(team.Spec.SandboxRefs, object.GetName())
	case *api.AgentPolicy:
		return team.Spec.AgentPolicyRef == object.GetName()
	default:
		return false
	}
}

func sameTeamPolicy(a, b api.TeamSpec) bool {
	return a.AgentPolicyRef == b.AgentPolicyRef &&
		a.DefaultSandboxRef == b.DefaultSandboxRef && slices.Equal(a.SandboxRefs, b.SandboxRefs)
}

func suspensionOnlyUpdate(request admission.Request, agent, old *api.Agent) bool {
	return request.Operation == admissionv1.Update && old != nil && !old.Spec.Suspended && agent.Spec.Suspended &&
		rawChangesOnly(request, []string{"suspended"}, []string{"generation", "managedFields"})
}

func lifecycleFinalizer(value string) bool {
	return slices.Contains([]string{api.Finalizer, sandbox.HoldFinalizer, sandbox.PreparationFinalizer, sandbox.BootstrapFinalizer,
		"cetirun.microsoft.com/custom-runtime-ownership", "cetirun.microsoft.com/custom-apply-completion"}, value)
}

func changedLifecycleFinalizers(a, b []string) bool {
	for _, values := range [][2][]string{{a, b}, {b, a}} {
		for _, value := range values[0] {
			if lifecycleFinalizer(value) && !slices.Contains(values[1], value) {
				return true
			}
		}
	}
	return false
}

func garbageCollectorFinalizerUpdate(request admission.Request, object, old client.Object) bool {
	if request.Operation != admissionv1.Update || old == nil || old.GetDeletionTimestamp().IsZero() ||
		!slices.Contains([]string{
			"system:kube-controller-manager",
			"system:serviceaccount:kube-system:generic-garbage-collector",
		}, request.UserInfo.Username) {
		return false
	}
	a := object.DeepCopyObject().(client.Object)
	b := old.DeepCopyObject().(client.Object)
	a.SetFinalizers(nil)
	b.SetFinalizers(nil)
	a.SetManagedFields(nil)
	b.SetManagedFields(nil)
	if !equality.Semantic.DeepEqual(a, b) || !rawMetadataOnly(request) {
		return false
	}
	expectedFinalizers := make([]string, 0, len(old.GetFinalizers()))
	foundForegroundDeletion := false
	for _, value := range old.GetFinalizers() {
		if value != metav1.FinalizerDeleteDependents {
			expectedFinalizers = append(expectedFinalizers, value)
			continue
		}
		if foundForegroundDeletion {
			return false
		}
		foundForegroundDeletion = true
	}
	return foundForegroundDeletion && slices.Equal(object.GetFinalizers(), expectedFinalizers)
}

func controllerFinalizerUpdate(agent, old *api.Agent) bool {
	a, b := agent.DeepCopy(), old.DeepCopy()
	a.Finalizers, b.Finalizers = nil, nil
	a.ManagedFields, b.ManagedFields = nil, nil
	if !equality.Semantic.DeepEqual(a, b) {
		return false
	}
	for _, values := range [][2][]string{{agent.Finalizers, old.Finalizers}, {old.Finalizers, agent.Finalizers}} {
		for _, value := range values[0] {
			if !slices.Contains(values[1], value) && !lifecycleFinalizer(value) {
				return false
			}
		}
	}
	if !old.DeletionTimestamp.IsZero() {
		for _, value := range agent.Finalizers {
			if !slices.Contains(old.Finalizers, value) {
				return false
			}
		}
	} else {
		for _, value := range old.Finalizers {
			if !slices.Contains(agent.Finalizers, value) {
				return false
			}
		}
	}
	return true
}

func decodeRequest(request admission.Request, mode string) (client.Object, client.Object, error) {
	kinds := map[string]string{"agents": "Agent", "teams": "Team", "sandboxes": "Sandbox", "agentpolicies": "AgentPolicy"}
	kind, known := kinds[request.Resource.Resource]
	if !known || request.Resource.Group != api.GroupVersion.Group || request.Resource.Version != api.GroupVersion.Version ||
		request.Kind.Group != api.GroupVersion.Group || request.Kind.Version != api.GroupVersion.Version || request.Kind.Kind != kind ||
		request.SubResource != "" || request.Namespace != "" {
		return nil, nil, fmt.Errorf("unsupported Sandbox admission resource or subresource")
	}
	if request.Operation != admissionv1.Create && request.Operation != admissionv1.Update && request.Operation != admissionv1.Delete {
		return nil, nil, fmt.Errorf("unsupported Sandbox admission operation")
	}
	makeObject := func(raw runtime.RawExtension) (client.Object, error) {
		var object client.Object
		switch kind {
		case "Agent":
			object = &api.Agent{}
		case "Team":
			object = &api.Team{}
		case "Sandbox":
			object = &api.Sandbox{}
		case "AgentPolicy":
			object = &api.AgentPolicy{}
		}
		if err := decodeJSON(raw.Raw, object, mode == ValidationStrict); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", kind, err)
		}
		assignedName := object.GetName() != "" && object.GetName() == request.Name
		generatedNameCreate := request.Operation == admissionv1.Create && request.Name == "" &&
			object.GetName() == "" && object.GetGenerateName() != ""
		if (!assignedName && !generatedNameCreate) || object.GetNamespace() != "" {
			return nil, fmt.Errorf("admitted object identity does not match the request")
		}
		return object, nil
	}
	var old client.Object
	if request.Operation != admissionv1.Create {
		var err error
		old, err = makeObject(request.OldObject)
		if err != nil {
			return nil, nil, err
		}
	}
	if request.Operation == admissionv1.Delete {
		return old, old, nil
	}
	object, err := makeObject(request.Object)
	return object, old, err
}

func dependencyError(ctx context.Context, err error) admission.Response {
	log.FromContext(ctx).Error(err, "Sandbox admission cannot verify current configuration")
	return admission.Errored(http.StatusServiceUnavailable, fmt.Errorf("Sandbox admission dependencies are unavailable; retry without weakening policy"))
}
