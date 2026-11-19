// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
)

// A Team may select only skills that its AgentPolicy allows: the operator
// denies the Agents of any other Team, which stops them and revokes their
// virtual keys. Admission refuses such a Team write, also one that changes
// only the skills, and a Policy write that would remove a skill that a Team
// of the Policy selects.
func TestTeamSkillsMustStayWithinTheirPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.policy.Spec.Skills.Allowed = []string{"pdf-tools", "zh-notes"}
	if err := f.client.Update(ctx, f.policy); err != nil {
		t.Fatal(err)
	}

	// A Team update that changes only the skills, by a user who is no
	// administrator: the authority of such writes stays as it was.
	previous := f.team.DeepCopy()
	allowed := f.team.DeepCopy()
	allowed.Spec.Skills = []string{"pdf-tools"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, allowed, previous, "member")), true, "")
	refused := f.team.DeepCopy()
	refused.Spec.Skills = []string{"pdf-tools", "web-research", "npm:@example/fetch"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, refused, previous, "member")), false,
		`Team "engineering" selects skills that AgentPolicy "normal" does not allow in skills.allowed: web-research, npm:@example/fetch`)

	// A Team creation or a change of its Policy.
	created := f.team.DeepCopy()
	created.Name, created.UID, created.ResourceVersion = "research", "", ""
	created.Spec.Skills = []string{"web-research"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Create, created, nil, "administrator")), false,
		"does not allow in skills.allowed: web-research")
	created.Spec.Skills = []string{"zh-notes"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Create, created, nil, "administrator")), true, "")

	// A Policy change that takes away a skill that the Team selects.
	f.team.Spec.Skills = []string{"pdf-tools", "zh-notes"}
	if err := f.client.Update(ctx, f.team); err != nil {
		t.Fatal(err)
	}
	narrowed := f.policy.DeepCopy()
	narrowed.Spec.Skills.Allowed = []string{"zh-notes"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, narrowed, f.policy, "administrator")), false,
		`configuration "normal" would invalidate referencing Team "engineering": Team "engineering" selects skills that AgentPolicy "normal" does not allow in skills.allowed: pdf-tools`)
	widened := f.policy.DeepCopy()
	widened.Spec.Skills.Allowed = []string{"pdf-tools", "zh-notes", "web-research"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, widened, f.policy, "administrator")), true, "")
}
