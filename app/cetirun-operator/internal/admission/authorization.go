// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"fmt"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (h *Handler) creationTeam(request admission.Request, agent *api.Agent) (string, string, error) {
	admin := h.config.administrator(request.UserInfo)
	if admin && agent.Spec.Team != "" {
		return agent.Spec.Team, "", nil
	}
	assigned, err := h.config.assignedTeam(request.UserInfo)
	if err != nil {
		return "", "", err
	}
	if assigned == "" {
		return agent.Spec.Team, "", nil
	}
	if agent.Spec.Team != "" && agent.Spec.Team != assigned {
		return "", "", fmt.Errorf("spec.team does not match the authenticated administrative Team assignment")
	}
	return assigned, assigned, nil
}

func (h *Handler) memberAuthority(ctx context.Context, request admission.Request, agent *api.Agent) error {
	if h.cetiAdminMember(ctx, request) {
		return nil
	}
	assigned, err := h.config.assignedTeam(request.UserInfo)
	if err != nil {
		return err
	}
	if assigned == "" || assigned != agent.Spec.Team {
		return fmt.Errorf("policy-bound Agent writes require an explicitly authorized administrator or an authenticated assigned Team member")
	}
	return nil
}

func (h *Handler) defaultAgentCreation(ctx context.Context, request admission.Request, agent *api.Agent) admission.Response {
	if sandboxbinding.Present(agent) {
		return admission.Denied("authorization records and reauthorization requests cannot be supplied during Agent creation")
	}
	name, assigned, err := h.creationTeam(request, agent)
	if err != nil {
		return admission.Denied(err.Error())
	}
	team, err := h.team(ctx, name)
	if err != nil {
		return dependencyError(ctx, err)
	}
	if assigned != "" && team == nil {
		return admission.Denied("the administratively assigned Team does not exist")
	}
	desired := agent.DeepCopy()
	desired.Spec.Team = name
	patches := selectionPatches(agent, desired)
	if !sandbox.Requested(desired, team) {
		return admission.Patched("legacy Agent; preserve any trusted Team assignment", patches...)
	}
	if !h.config.administrator(request.UserInfo) {
		if err := h.memberAuthority(ctx, request, desired); err != nil {
			return admission.Denied(err.Error())
		}
	}
	spec, err := sandbox.DefaultAgentSelection(desired.Spec, team)
	if err != nil {
		return admission.Denied(err.Error())
	}
	desired.Spec = spec
	resources, response := h.agentAuthorizationResources(ctx, admissionv1.Create, desired, team)
	if !response.Allowed {
		return response
	}
	record, err := sandboxbinding.Snapshot(desired, team, resources)
	if err != nil {
		return admission.Denied(err.Error())
	}
	patches = append(selectionPatches(agent, desired), authorizationPatch(agent, record))
	return admission.Patched("persist creation-time selection and UID authorization; execution remains held", patches...)
}

// Ordinary updates never regenerate either defaults or authorization. The
// mutator only changes a record in response to an exact, fresh admin request.
func (h *Handler) reauthorizeAgent(ctx context.Context, request admission.Request, agent, old *api.Agent) admission.Response {
	if !sameAnnotation(agent, old, sandboxbinding.AuthorizationAnnotation) {
		return admission.Denied("protected authorization records cannot be authored or rewritten by callers; use explicit administrator reauthorization")
	}
	if sameAnnotation(agent, old, sandboxbinding.ReauthorizeAnnotation) {
		return admission.Allowed("updates never re-default or implicitly reauthorize an Agent")
	}
	if !h.config.administrator(request.UserInfo) {
		return admission.Denied("explicit reauthorization requires an authorized administrator")
	}
	intent, err := reauthorizationIntent(agent, old)
	if err != nil {
		return admission.Denied(err.Error())
	}
	team, err := h.team(ctx, agent.Spec.Team)
	if err != nil {
		return dependencyError(ctx, err)
	}
	resources, response := h.agentAuthorizationResources(ctx, admissionv1.Update, agent, team)
	if !response.Allowed {
		return response
	}
	current, err := sandboxbinding.CurrentBinding(agent, team, resources)
	if err != nil {
		return admission.Denied(err.Error())
	}
	if intent.Binding != current {
		return admission.Denied("explicit reauthorization does not match the current Agent, Team, Sandbox and AgentPolicy UIDs")
	}
	record, err := sandboxbinding.Snapshot(agent, team, resources)
	if err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Patched("persist explicitly reauthorized UIDs; execution remains held", authorizationPatch(agent, record))
}

func (h *Handler) protectedAuthorization(request admission.Request, agent, old *api.Agent) error {
	if old == nil {
		if _, supplied := agent.Annotations[sandboxbinding.ReauthorizeAnnotation]; supplied {
			return fmt.Errorf("reauthorization requests apply only to existing Agents")
		}
		return nil
	}
	recordChanged := !sameAnnotation(agent, old, sandboxbinding.AuthorizationAnnotation)
	requestChanged := !sameAnnotation(agent, old, sandboxbinding.ReauthorizeAnnotation)
	if !recordChanged && !requestChanged {
		return nil
	}
	if !h.config.administrator(request.UserInfo) {
		return fmt.Errorf("protected authorization metadata may only change through explicit administrator reauthorization")
	}
	if !requestChanged {
		return fmt.Errorf("protected authorization records require explicit administrator reauthorization")
	}
	_, err := reauthorizationIntent(agent, old)
	return err
}

func (h *Handler) validateAgentAuthorization(request admission.Request, agent, old *api.Agent, team *api.Team, resources sandbox.Resources) admission.Response {
	expected, err := sandboxbinding.Snapshot(agent, team, resources)
	if err != nil {
		return admission.Denied(err.Error())
	}
	record := agent.Annotations[sandboxbinding.AuthorizationAnnotation]
	if old == nil {
		if record != expected {
			return admission.Denied("the creation-defaulting webhook must persist the current UID authorization; caller-supplied records are not trusted")
		}
		return admission.Allowed("creation authorization is valid; Sandbox execution remains held")
	}
	reauthorizing := !sameAnnotation(agent, old, sandboxbinding.ReauthorizeAnnotation)
	if reauthorizing {
		if !h.config.administrator(request.UserInfo) {
			return admission.Denied("explicit reauthorization requires an authorized administrator")
		}
		intent, err := reauthorizationIntent(agent, old)
		if err != nil {
			return admission.Denied(err.Error())
		}
		current, err := sandboxbinding.CurrentBinding(agent, team, resources)
		if err != nil {
			return admission.Denied(err.Error())
		}
		if intent.Binding != current || record != expected {
			return admission.Denied("the explicit reauthorization request and protected record must match current reference UIDs; the reauthorization mutator must run")
		}
		return admission.Allowed("explicit administrator reauthorization is valid; Sandbox execution remains held")
	}
	if !sameAgentSelection(agent, old) {
		return admission.Denied("changing a policy-bound Agent's Team, policy class or Sandbox binding requires explicit administrator reauthorization")
	}
	if _, err := sandboxbinding.AuthorizedBinding(agent); err != nil {
		return admission.Denied(fmt.Sprintf("explicit administrator reauthorization is required: %v", err))
	}
	if record != expected {
		return admission.Denied("current reference UIDs differ from protected authorization; explicit administrator reauthorization is required")
	}
	return admission.Allowed("durable UID authorization is unchanged; Sandbox execution remains held")
}

func reauthorizationIntent(agent, old *api.Agent) (sandboxbinding.Reauthorization, error) {
	if old == nil || old.UID == "" || old.UID != agent.UID || old.Name != agent.Name || agent.ResourceVersion == "" {
		return sandboxbinding.Reauthorization{}, fmt.Errorf("explicit reauthorization requires an existing Agent UID and resourceVersion")
	}
	intent, err := sandboxbinding.ParseReauthorization(agent.Annotations[sandboxbinding.ReauthorizeAnnotation])
	if err != nil {
		return intent, err
	}
	if previous, err := sandboxbinding.ParseReauthorization(old.Annotations[sandboxbinding.ReauthorizeAnnotation]); err == nil &&
		previous.RequestID == intent.RequestID {
		return intent, fmt.Errorf("explicit reauthorization requires a fresh requestID")
	}
	return intent, nil
}

func sameAnnotation(a, b *api.Agent, key string) bool {
	first, firstFound := a.Annotations[key]
	second, secondFound := b.Annotations[key]
	return firstFound == secondFound && first == second
}

func sameAgentSelection(a, b *api.Agent) bool {
	return a.Spec.Team == b.Spec.Team && a.Spec.SandboxRef == b.Spec.SandboxRef
}

func selectionPatches(old, desired *api.Agent) []jsonpatch.JsonPatchOperation {
	var patches []jsonpatch.JsonPatchOperation
	for _, field := range []struct {
		path string
		old  any
		new  any
	}{
		{"/spec/team", old.Spec.Team, desired.Spec.Team},
		{"/spec/sandboxRef", old.Spec.SandboxRef, desired.Spec.SandboxRef},
	} {
		if field.old != field.new {
			patches = append(patches, jsonpatch.JsonPatchOperation{Operation: "add", Path: field.path, Value: field.new})
		}
	}
	return patches
}

func authorizationPatch(agent *api.Agent, record string) jsonpatch.JsonPatchOperation {
	if agent.Annotations == nil {
		return jsonpatch.JsonPatchOperation{Operation: "add", Path: "/metadata/annotations",
			Value: map[string]string{sandboxbinding.AuthorizationAnnotation: record}}
	}
	path := strings.ReplaceAll(strings.ReplaceAll(sandboxbinding.AuthorizationAnnotation, "~", "~0"), "/", "~1")
	return jsonpatch.JsonPatchOperation{Operation: "add", Path: "/metadata/annotations/" + path, Value: record}
}
