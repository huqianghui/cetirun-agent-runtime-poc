// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func fixtureAuthorization(t *testing.T, f fixture) string {
	t.Helper()
	record, err := sandboxbinding.Snapshot(f.agent, f.team, sandbox.Resources{
		Sandboxes: map[string]*api.Sandbox{f.box.Name: f.box}, AgentPolicies: map[string]*api.AgentPolicy{f.policy.Name: f.policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func stampFixtureAuthorization(t *testing.T, f fixture) {
	t.Helper()
	if f.agent.Annotations == nil {
		f.agent.Annotations = map[string]string{}
	}
	f.agent.Annotations[sandboxbinding.AuthorizationAnnotation] = fixtureAuthorization(t, f)
}

func assignedFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	f.policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	if err := f.client.Update(context.Background(), f.policy); err != nil {
		t.Fatal(err)
	}
	f.handler.config.TeamAssignments = []TeamAssignment{
		{Team: f.team.Name, Users: []string{"member"}, Groups: []string{"engineering-members"}},
	}
	return f
}

func applyAgentPatches(t *testing.T, request admission.Request, response admission.Response) (*api.Agent, admission.Request) {
	t.Helper()
	requireResponse(t, response, true, "")
	if len(response.Patches) != 0 {
		raw, err := json.Marshal(response.Patches)
		if err != nil {
			t.Fatal(err)
		}
		patch, err := jsonpatch.DecodePatch(raw)
		if err != nil {
			t.Fatal(err)
		}
		request.Object.Raw, err = patch.Apply(request.Object.Raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	agent := &api.Agent{}
	if err := json.Unmarshal(request.Object.Raw, agent); err != nil {
		t.Fatal(err)
	}
	return agent, request
}

func admitAgent(t *testing.T, h *Handler, request admission.Request) *api.Agent {
	t.Helper()
	agent, mutated := applyAgentPatches(t, request, h.DefaultAgent(context.Background(), request))
	requireResponse(t, h.Validate(context.Background(), mutated), true, "held")
	return agent
}

func requestReauthorization(t *testing.T, h *Handler, agent *api.Agent, id string) {
	t.Helper()
	team, err := h.team(context.Background(), agent.Spec.Team)
	if err != nil {
		t.Fatal(err)
	}
	resources, err := h.resources(context.Background(), team.Spec, sandbox.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := sandboxbinding.CurrentBinding(agent, team, resources)
	if err != nil {
		t.Fatal(err)
	}
	request, err := sandboxbinding.FormatReauthorization(id, binding)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[sandboxbinding.ReauthorizeAnnotation] = request
}

func TestTrustedTeamCreationContextIgnoresLabelsAndHumanRoles(t *testing.T) {
	for _, user := range []authenticationv1.UserInfo{
		{Username: "member"}, {Username: "group-member", Groups: []string{"engineering-members"}},
		{Username: "member", Groups: []string{"engineering-members", "unrelated"}},
	} {
		t.Run(user.Username+strings.Join(user.Groups, ","), func(t *testing.T) {
			f := assignedFixture(t)
			f.agent.Labels = map[string]string{"team": "caller-supplied-team", "role": "admin"}
			f.agent.Annotations["example.com/portal-role"] = "admin"
			req := requestFor(t, admissionv1.Create, f.agent, nil, user.Username)
			req.UserInfo = user
			agent := admitAgent(t, f.handler, req)
			if agent.Spec.Team != f.team.Name || agent.Spec.SandboxRef != f.box.Name {
				t.Fatalf("trusted creation context was not persisted: %#v", agent.Spec)
			}
			if agent.Annotations["example.com/portal-role"] != "admin" || f.agent.Spec.Team != f.team.Name || sandboxbinding.Present(f.agent) {
				t.Fatal("mutation changed unrelated metadata or the caller's object")
			}
			binding, err := sandboxbinding.AuthorizedBinding(agent)
			if err != nil || binding.TeamUID != f.team.UID || binding.AgentPolicyUID != f.policy.UID || agent.Status.Sandbox != nil {
				t.Fatalf("creation did not persist authorization independently of enforcement: %v, %v", binding, err)
			}
		})
	}
}

func TestMemberCreationContextCannotBeSelectedOrEscalatedByCaller(t *testing.T) {
	for _, scenario := range []string{
		"other Team", "legacy Team escape", "unmapped", "anonymous", "conflicting membership", "missing assignment Team",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := assignedFixture(t)
			agent := f.agent.DeepCopy()
			username, groups := "member", []string{}
			switch scenario {
			case "other Team":
				agent.Spec.Team = "other"
			case "legacy Team escape":
				agent.Spec.Team, agent.Spec.SandboxRef = "legacy", ""
			case "unmapped":
				username = "outsider"
				agent.Labels = map[string]string{"team": f.team.Name, "role": "admin"}
			case "anonymous":
				username, groups = "system:anonymous", []string{"engineering-members"}
			case "conflicting membership":
				f.handler.config.TeamAssignments = append(f.handler.config.TeamAssignments, TeamAssignment{Team: "other", Groups: []string{"other-members"}})
				groups = []string{"other-members"}
			case "missing assignment Team":
				if err := f.client.Delete(context.Background(), f.team); err != nil {
					t.Fatal(err)
				}
				agent.Spec.SandboxRef = ""
			}
			req := requestFor(t, admissionv1.Create, agent, nil, username)
			req.UserInfo.Groups = groups
			requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "")
			requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
		})
	}
}

func TestValidationCannotStandInForMissingCreationMutation(t *testing.T) {
	f := assignedFixture(t)
	f.agent.Spec.Team, f.agent.Spec.SandboxRef = "", ""
	req := requestFor(t, admissionv1.Create, f.agent, nil, "member")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "creation-defaulting")
	f.agent.Spec.Team, f.agent.Spec.SandboxRef = f.team.Name, f.box.Name
	req = requestFor(t, admissionv1.Create, f.agent, nil, "member")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "current UID authorization")
}

func TestTeamAssignmentConflictsHaveTheSameSafeAdmissionDiagnostic(t *testing.T) {
	f := assignedFixture(t)
	old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
	f.handler.config.TeamAssignments = append(f.handler.config.TeamAssignments,
		TeamAssignment{Team: "other-team", Groups: []string{"other-members"}})
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		req := requestFor(t, operation, f.agent, nil, "member")
		if operation == admissionv1.Update {
			req = requestFor(t, operation, old.DeepCopy(), old, "member")
		}
		req.UserInfo.Groups = []string{"engineering-members", "other-members"}
		if operation == admissionv1.Create {
			requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, errTeamAssignmentConflict.Error())
		}
		requireResponse(t, f.handler.Validate(context.Background(), req), false, errTeamAssignmentConflict.Error())
	}
}

func TestCallersCannotTransplantOrRewriteProtectedAuthorization(t *testing.T) {
	for _, user := range []string{"member", "administrator", testController} {
		for _, key := range []string{sandboxbinding.AuthorizationAnnotation, sandboxbinding.ReauthorizeAnnotation} {
			t.Run(user+"/"+key, func(t *testing.T) {
				f := assignedFixture(t)
				for _, value := range []string{"", fixtureAuthorization(t, f)} {
					agent := f.agent.DeepCopy()
					agent.Annotations = map[string]string{key: value}
					req := requestFor(t, admissionv1.Create, agent, nil, user)
					requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "cannot be supplied")
					agent.Spec.Team, agent.Spec.SandboxRef = "", ""
					req = requestFor(t, admissionv1.Create, agent, nil, user)
					requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "cannot be supplied")
				}
				old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
				for _, remove := range []bool{false, true} {
					agent := old.DeepCopy()
					if remove {
						delete(agent.Annotations, sandboxbinding.AuthorizationAnnotation)
					} else {
						agent.Annotations[sandboxbinding.AuthorizationAnnotation] = strings.Replace(
							agent.Annotations[sandboxbinding.AuthorizationAnnotation], string(f.team.UID), "forged-team-uid", 1)
					}
					req := requestFor(t, admissionv1.Update, agent, old, user)
					requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "protected authorization")
					requireResponse(t, f.handler.Validate(context.Background(), req), false, "authorization")
				}
			})
		}
	}
}

func TestMemberWritesRequireCurrentMembershipAndImmutableBinding(t *testing.T) {
	f := assignedFixture(t)
	old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
	updated := old.DeepCopy()
	updated.Spec.Model = "application-model"
	req := requestFor(t, admissionv1.Update, updated, old, "member")
	response := f.handler.DefaultAgent(context.Background(), req)
	requireResponse(t, response, true, "never re-default")
	if len(response.Patches) != 0 {
		t.Fatal("ordinary member update rewrote authorization")
	}
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "unchanged")
	for _, edit := range []func(*api.Agent){
		func(a *api.Agent) { a.Spec.Team = "other" },
		func(a *api.Agent) { a.Spec.SandboxRef = "other" },
		func(a *api.Agent) { requestReauthorization(t, f.handler, a, "member-escalation") },
	} {
		updated := old.DeepCopy()
		edit(updated)
		req := requestFor(t, admissionv1.Update, updated, old, "member")
		requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
	}
	f.handler.config.TeamAssignments = nil
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "administrator")
	req.UserInfo.Username = "administrator"
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "unchanged")
}

func TestUIDReplacementBeforeFirstReconcileRequiresExplicitReauthorization(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			f := assignedFixture(t)
			old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
			binding, err := sandboxbinding.AuthorizedBinding(old)
			if err != nil || old.Status.Sandbox != nil {
				t.Fatal("test must begin before any successful apply")
			}
			objects := map[string]client.Object{"Team": f.team, "Sandbox": f.box, "AgentPolicy": f.policy}
			object := objects[kind]
			if err := f.client.Delete(context.Background(), object); err != nil {
				t.Fatal(err)
			}
			object.SetUID("replacement-uid")
			object.SetResourceVersion("")
			if err := f.client.Create(context.Background(), object); err != nil {
				t.Fatal(err)
			}
			for _, user := range []string{"member", "administrator"} {
				updated := old.DeepCopy()
				updated.Spec.Model = "changed"
				req := requestFor(t, admissionv1.Update, updated, old, user)
				response := f.handler.DefaultAgent(context.Background(), req)
				requireResponse(t, response, true, "")
				if len(response.Patches) != 0 {
					t.Fatal("ordinary update silently authorized a replacement UID")
				}
				requireResponse(t, f.handler.Validate(context.Background(), req), false, "reauthorization")
			}
			resources, err := f.handler.resources(context.Background(), f.team.Spec, sandbox.Resources{})
			if err != nil {
				t.Fatal(err)
			}
			input := sandbox.ResolveInput{Agent: old, Team: f.team, Resources: resources, Registrations: f.handler.registrations, AuthorizedBinding: binding}
			if _, err := sandbox.Resolve(input); !errors.Is(err, sandbox.ErrUnauthorizedBinding) {
				t.Fatalf("resolver accepted an unapproved replacement: %v", err)
			}
			updated := old.DeepCopy()
			requestReauthorization(t, f.handler, updated, "approve-replacement")
			req := requestFor(t, admissionv1.Update, updated, old, "member")
			requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "administrator")
			req.UserInfo.Username = "administrator"
			renewed := admitAgent(t, f.handler, req)
			input.Agent, input.AuthorizedBinding = renewed, sandbox.Binding{}
			input.AuthorizedBinding, err = sandboxbinding.AuthorizedBinding(renewed)
			if err != nil || input.AuthorizedBinding == binding || renewed.Status.Sandbox != nil {
				t.Fatal("explicit authorization was not durable and separate from successful apply")
			}
			if _, err := sandbox.Resolve(input); err != nil {
				t.Fatalf("explicit current-UID authorization did not feed the resolver: %v", err)
			}
		})
	}
}

func TestReplacementBetweenMutationAndValidationIsNotAuthorized(t *testing.T) {
	f := assignedFixture(t)
	req := requestFor(t, admissionv1.Create, f.agent, nil, "member")
	_, mutated := applyAgentPatches(t, req, f.handler.DefaultAgent(context.Background(), req))
	if err := f.client.Delete(context.Background(), f.policy); err != nil {
		t.Fatal(err)
	}
	f.policy.UID, f.policy.ResourceVersion = "replacement-policy-uid", ""
	if err := f.client.Create(context.Background(), f.policy); err != nil {
		t.Fatal(err)
	}
	requireResponse(t, f.handler.Validate(context.Background(), mutated), false, "current UID authorization")
}

func TestAdminReauthorizationIsExplicitFreshAndExact(t *testing.T) {
	f := assignedFixture(t)
	old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
	updated := old.DeepCopy()
	requestReauthorization(t, f.handler, updated, "operation-1")
	renewed := admitAgent(t, f.handler, requestFor(t, admissionv1.Update, updated, old, "administrator"))
	for _, edit := range []func(*api.Agent){
		func(a *api.Agent) {
			requestReauthorization(t, f.handler, a, "operation-2")
			a.Annotations[sandboxbinding.ReauthorizeAnnotation] = strings.Replace(a.Annotations[sandboxbinding.ReauthorizeAnnotation], string(a.UID), "other-agent-uid", 1)
		},
		func(a *api.Agent) {
			requestReauthorization(t, f.handler, a, "operation-2")
			a.Annotations[sandboxbinding.ReauthorizeAnnotation] = strings.Replace(a.Annotations[sandboxbinding.ReauthorizeAnnotation], string(f.policy.UID), "stale-policy-uid", 1)
		},
		func(a *api.Agent) {
			a.Annotations[sandboxbinding.ReauthorizeAnnotation] = `{"requestID":"operation-2","binding":{}}`
		},
		func(a *api.Agent) { a.Annotations[sandboxbinding.ReauthorizeAnnotation] += " " },
		func(a *api.Agent) { delete(a.Annotations, sandboxbinding.ReauthorizeAnnotation) },
		func(a *api.Agent) {
			requestReauthorization(t, f.handler, a, "operation-2")
			a.UID = "replacement-agent"
		},
		func(a *api.Agent) { requestReauthorization(t, f.handler, a, "operation-2"); a.ResourceVersion = "" },
	} {
		candidate := renewed.DeepCopy()
		edit(candidate)
		req := requestFor(t, admissionv1.Update, candidate, renewed, "administrator")
		requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "")
		requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
	}
	updated = renewed.DeepCopy()
	requestReauthorization(t, f.handler, updated, "operation-2")
	updated.Status.Sandbox = &api.SandboxStatus{TeamUID: "previous-team", SandboxUID: "previous-box", AgentPolicyUID: "previous-policy", AppliedRevision: "previous-revision"}
	renewed = admitAgent(t, f.handler, requestFor(t, admissionv1.Update, updated, renewed, "administrator"))
	if renewed.Status.Sandbox == nil || renewed.Status.Sandbox.AppliedRevision != "previous-revision" {
		t.Fatal("authorization rewrote last-applied status")
	}
}

func TestAdminRebindingAndLegacyAdoptionNeverImplicitlyDefault(t *testing.T) {
	for _, selection := range []string{"Team", "Sandbox"} {
		t.Run(selection, func(t *testing.T) {
			f := assignedFixture(t)
			old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
			updated := old.DeepCopy()
			switch selection {
			case "Team":
				other := f.team.DeepCopy()
				other.Name, other.UID, other.ResourceVersion = "other-team", "other-team-uid", ""
				if err := f.client.Create(context.Background(), other); err != nil {
					t.Fatal(err)
				}
				updated.Spec.Team = other.Name
			case "Sandbox":
				box := f.box.DeepCopy()
				box.Name, box.UID, box.ResourceVersion = "second", "second-uid", ""
				if err := f.client.Create(context.Background(), box); err != nil {
					t.Fatal(err)
				}
				f.policy.Spec.SandboxSelection.AllowMultiple = true
				if err := f.client.Update(context.Background(), f.policy); err != nil {
					t.Fatal(err)
				}
				f.team.Spec.SandboxRefs = append(f.team.Spec.SandboxRefs, box.Name)
				if err := f.client.Update(context.Background(), f.team); err != nil {
					t.Fatal(err)
				}
				updated.Spec.SandboxRef = box.Name
			}
			req := requestFor(t, admissionv1.Update, updated, old, "administrator")
			requireResponse(t, f.handler.Validate(context.Background(), req), false, "reauthorization")
			requestReauthorization(t, f.handler, updated, "rebind")
			_ = admitAgent(t, f.handler, requestFor(t, admissionv1.Update, updated, old, "administrator"))
		})
	}
	f := assignedFixture(t)
	old := f.agent.DeepCopy()
	requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Update, old, old, "administrator")), false, "reauthorization")
	updated := old.DeepCopy()
	requestReauthorization(t, f.handler, updated, "adopt")
	_ = admitAgent(t, f.handler, requestFor(t, admissionv1.Update, updated, old, "administrator"))
	updated.Spec.SandboxRef = ""
	req := requestFor(t, admissionv1.Update, updated, old, "administrator")
	requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "never re-defaulted")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "never re-defaulted")
}

func TestCreationAuthorizationDryRunAndFailuresHaveNoSideEffects(t *testing.T) {
	f := assignedFixture(t)
	req := requestFor(t, admissionv1.Create, f.agent, nil, "member")
	req.DryRun = ptr.To(true)
	agent := admitAgent(t, f.handler, req)
	var agents api.AgentList
	if err := f.client.List(context.Background(), &agents); err != nil || len(agents.Items) != 0 {
		t.Fatalf("admission created a side-effect authorization object: %v", err)
	}
	// Returned dry-run metadata cannot be replayed as trusted input to a create.
	requireResponse(t, f.handler.DefaultAgent(context.Background(), requestFor(t, admissionv1.Create, agent, nil, "member")), false, "cannot be supplied")
	f.handler.reader = unavailableReader{}
	for _, dryRun := range []bool{false, true} {
		req.DryRun = ptr.To(dryRun)
		response := f.handler.DefaultAgent(context.Background(), req)
		requireResponse(t, response, false, "dependencies are unavailable")
		if response.Result.Code != 503 || len(response.Patches) != 0 {
			t.Fatal("failed admission leaked partial authorization patches")
		}
	}
	if sandboxbinding.Present(f.agent) {
		t.Fatal("dry run or error mutated the caller object")
	}
}

func TestReauthorizationFailurePreservesThePreviousRecord(t *testing.T) {
	f := assignedFixture(t)
	old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
	updated := old.DeepCopy()
	requestReauthorization(t, f.handler, updated, "dependency-failure")
	f.handler.reader = unavailableReader{}
	req := requestFor(t, admissionv1.Update, updated, old, "administrator")
	response := f.handler.DefaultAgent(context.Background(), req)
	requireResponse(t, response, false, "dependencies are unavailable")
	if response.Result.Code != 503 || len(response.Patches) != 0 ||
		updated.Annotations[sandboxbinding.AuthorizationAnnotation] != old.Annotations[sandboxbinding.AuthorizationAnnotation] {
		t.Fatal("failed reauthorization returned partial or rewritten authority")
	}
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "dependencies are unavailable")
}

func TestAuthenticatedCleanupCannotSmuggleAuthorizationEdits(t *testing.T) {
	f := assignedFixture(t)
	old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
	reauthorized := old.DeepCopy()
	requestReauthorization(t, f.handler, reauthorized, "before-deletion")
	old = admitAgent(t, f.handler, requestFor(t, admissionv1.Update, reauthorized, old, "administrator"))
	now := metav1.Now()
	old.DeletionTimestamp = &now
	old.Finalizers = []string{api.Finalizer, sandbox.HoldFinalizer, metav1.FinalizerDeleteDependents}
	for _, user := range []string{testController, "system:kube-controller-manager"} {
		candidate := old.DeepCopy()
		if user == testController {
			candidate.Finalizers = []string{sandbox.HoldFinalizer, metav1.FinalizerDeleteDependents}
		} else {
			candidate.Finalizers = []string{api.Finalizer, sandbox.HoldFinalizer}
		}
		f.handler.reader = unavailableReader{}
		req := requestFor(t, admissionv1.Update, candidate, old, user)
		requireResponse(t, f.handler.DefaultAgent(context.Background(), req), true, "")
		requireResponse(t, f.handler.Validate(context.Background(), req), true, "")
		for _, edit := range []func(*api.Agent){
			func(a *api.Agent) { a.Annotations[sandboxbinding.AuthorizationAnnotation] = "forged" },
			func(a *api.Agent) { delete(a.Annotations, sandboxbinding.AuthorizationAnnotation) },
			func(a *api.Agent) { delete(a.Annotations, sandboxbinding.ReauthorizeAnnotation) },
			func(a *api.Agent) { a.Annotations[sandboxbinding.ReauthorizeAnnotation] += " " },
			func(a *api.Agent) { a.Annotations = nil },
		} {
			smuggled := candidate.DeepCopy()
			edit(smuggled)
			req = requestFor(t, admissionv1.Update, smuggled, old, user)
			requireResponse(t, f.handler.DefaultAgent(context.Background(), req), false, "")
			requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
		}
	}
}
