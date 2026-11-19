// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// withSkillMarketplace enables the Skill Marketplace for the reconciler and its
// Kubernetes prerequisites, as CETIRUN_SKILL_MARKETPLACE does, and Memory,
// whose composed context the Team subtree of the skills claim requires.
func withSkillMarketplace(r *AgentReconciler, _ client.Client) {
	r.SkillMarketplace = true
	r.SandboxPreparation.Prerequisites.(*KubernetesSandboxPrerequisites).SkillMarketplace = true
	r.MemoryImage = "registry.example.com/memory:1.2.3"
}

// createMarketplaceSkillsClaim creates the Agent's skills claim the way the
// Skill Marketplace does: owned by, and annotated for, the Agent.
func createMarketplaceSkillsClaim(t *testing.T, f *nativeSandboxFixture, input testfixture.Fixture) *corev1.PersistentVolumeClaim {
	t.Helper()
	agent := f.current(t)
	claim := input.Input.Skills.DeepCopy()
	claim.UID, claim.ResourceVersion = "", ""
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}
	claim.Labels = map[string]string{api.SkillMarketplaceManagedLabel: "true"}
	claim.Annotations = map[string]string{
		w.AgentUIDAnnotation: string(agent.UID), w.AgentNameAnnotation: agent.Name,
		api.SkillMarketplaceTeamUIDAnnotation: string(input.Team.UID),
	}
	if err := f.kube.Create(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	return claim
}

// assertSkillsClaimWait checks a pass that waited for the skills claim: no
// error, no failure or hold, the wait recorded, and the key left unblocked.
func assertSkillsClaimWait(t *testing.T, f *nativeSandboxFixture, result ctrl.Result, err error) {
	t.Helper()
	if err != nil || result.RequeueAfter != skillsClaimPendingRetry {
		t.Fatalf("result %+v, %v; want a requeue after %s", result, err, skillsClaimPendingRetry)
	}
	agent := f.current(t)
	if agent.Status.Phase == api.PhaseFailed || slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) {
		t.Fatalf("waiting for the skills claim failed or held the Agent: phase %q, finalizers %v", agent.Status.Phase, agent.Finalizers)
	}
	condition := apimeta.FindStatusCondition(agent.Status.Conditions, builtInPrerequisitesReady)
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != skillsClaimPendingReason || condition.Message != skillsClaimPendingMessage {
		t.Fatalf("prerequisite readiness = %+v, want the Skill Marketplace wait", condition)
	}
	for _, key := range f.gateway.keys {
		if key.Blocked {
			t.Fatal("waiting for the skills claim blocked the Agent's key")
		}
	}
}

// A new native Agent of a marketplace Team waits for the skills claim that the
// Skill Marketplace creates, then starts with only its Team's subtree.
func TestSkillMarketplaceWaitsForTheTeamSkillsClaim(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, input := newUnreconciledSandboxRuntimeFixture(t, true, []string{"example-skill"}, true, kind, true, false, withSkillMarketplace)
			for range 2 {
				result, err := f.reconcile()
				assertSkillsClaimWait(t, f, result, err)
			}
			agent := f.current(t)
			ready := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionReady)
			if (agent.Status.Phase != api.PhasePending && agent.Status.Phase != api.PhaseProvisioning) ||
				ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != skillsClaimPendingReason {
				t.Fatalf("new Agent phase %q, Ready %+v; want a pending wait for the skills claim", agent.Status.Phase, ready)
			}
			if f.installer.stages != 0 {
				t.Fatal("the Agent was installed before its skills claim existed")
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name)},
				&appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatal("the Agent workload exists before its skills claim:", err)
			}

			claim := createMarketplaceSkillsClaim(t, f, input)
			for pass := 0; pass < 3 && f.installer.stages == 0; pass++ {
				f.reconcileOK(t)
			}
			if f.installer.stages != 1 {
				t.Fatalf("the Agent did not proceed once its skills claim existed: %d applies", f.installer.stages)
			}
			agent = f.current(t)
			for _, conditionType := range []string{builtInPrerequisitesReady, api.ConditionReady} {
				if condition := apimeta.FindStatusCondition(agent.Status.Conditions, conditionType); condition != nil && condition.Reason == skillsClaimPendingReason {
					t.Fatalf("%s still reports the finished wait: %+v", conditionType, condition)
				}
			}
			team := &api.Team{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: input.Team.Name}, team); err != nil {
				t.Fatal(err)
			}
			expected := reconciledExpectationForTest(t, f)
			if !slices.ContainsFunc(expected.Sources, func(source w.SourceReference) bool {
				return source.Kind == "PersistentVolumeClaim" && source.Name == claim.Name && source.UID != ""
			}) {
				t.Fatal("the marketplace claim is not a pinned source of the Agent")
			}
			mount := corev1.VolumeMount{Name: "shared-skills", MountPath: w.SkillMarketplaceMountPath,
				SubPath: api.SkillMarketplaceTeamPath(string(team.UID)), ReadOnly: true}
			selection := corev1.EnvVar{Name: w.SkillMarketplaceSelectionEnv, Value: api.SkillMarketplaceSelectionDigest(team.Spec.Skills)}
			index := slices.IndexFunc(expected.Workloads, func(workload w.RenderedWorkload) bool { return workload.Kind == "StatefulSet" })
			if index < 0 {
				t.Fatal("the expectation has no Agent workload")
			}
			spec := expected.Workloads[index].Template.Spec
			initializer := slices.IndexFunc(spec.InitContainers, func(container corev1.Container) bool { return container.Name == "init-home-data" })
			if initializer < 0 || !slices.ContainsFunc(spec.InitContainers[initializer].VolumeMounts, func(actual corev1.VolumeMount) bool {
				return reflect.DeepEqual(actual, mount)
			}) || !slices.Contains(spec.InitContainers[initializer].Env, selection) {
				t.Fatal("the HOME initializer does not read the Team's subtree and selection")
			}
			if slices.ContainsFunc(spec.Containers[0].VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == "shared-skills" }) {
				t.Fatal("the Agent container mounts the skills claim")
			}
		})
	}
}

// A running Agent whose claim is missing is not stopped: the wait changes no
// workload, key, route or readiness that the Agent already has.
func TestSkillMarketplaceClaimWaitKeepsARunningAgent(t *testing.T) {
	ctx := context.Background()
	f, input := newUnreconciledSandboxRuntimeFixture(t, true, []string{"example-skill"}, true, "openclaw", true, false, withSkillMarketplace)
	result, err := f.reconcile()
	assertSkillsClaimWait(t, f, result, err)
	claim := createMarketplaceSkillsClaim(t, f, input)
	for pass := 0; pass < 3 && f.installer.stages == 0; pass++ {
		f.reconcileOK(t)
	}
	workload := f.workload(t)
	workload.Status.ObservedGeneration = workload.Generation
	workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
	if err := f.kube.Status().Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3 && f.current(t).Status.Phase != api.PhaseReady; pass++ {
		f.reconcileOK(t)
	}
	before := f.current(t)
	if before.Status.Phase != api.PhaseReady || !apimeta.IsStatusConditionTrue(before.Status.Conditions, api.ConditionReady) {
		t.Fatalf("fixture Agent is not running: phase %q, conditions %v", before.Status.Phase, before.Status.Conditions)
	}
	workload, stages := f.workload(t), f.installer.stages
	state, _, err := f.r.readReconciledSandboxExpectation(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	result, err = f.reconcile()
	assertSkillsClaimWait(t, f, result, err)
	after := f.current(t)
	if after.Status.Phase != api.PhaseReady || !apimeta.IsStatusConditionTrue(after.Status.Conditions, api.ConditionReady) ||
		after.Status.AccessURL != before.Status.AccessURL || after.Status.AIGateway.KeyID != before.Status.AIGateway.KeyID {
		t.Fatalf("the wait changed the running Agent: phase %q, conditions %v", after.Status.Phase, after.Status.Conditions)
	}
	current := f.workload(t)
	if current.UID != workload.UID || current.ResourceVersion != workload.ResourceVersion ||
		ptr.Deref(current.Spec.Replicas, 0) != 1 || f.installer.stages != stages {
		t.Fatal("the wait scaled, replaced or re-applied the running workload")
	}
	currentState := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(state), currentState); err != nil ||
		currentState.ResourceVersion != state.ResourceVersion {
		t.Fatal("the wait changed the admitted expectation:", err)
	}
}

// reconcileUntilSkillsClaimWait reconciles a new Agent until a pass waits for
// its skills claim. A new Custom Agent first waits for its Gateway binding.
func reconcileUntilSkillsClaimWait(t *testing.T, f *nativeSandboxFixture) {
	t.Helper()
	var result ctrl.Result
	var err error
	for pass := 0; pass < 6; pass++ {
		result, err = f.reconcile()
		if condition := apimeta.FindStatusCondition(f.current(t).Status.Conditions, builtInPrerequisitesReady); condition != nil &&
			condition.Reason == skillsClaimPendingReason {
			break
		}
	}
	assertSkillsClaimWait(t, f, result, err)
}

// A new Custom Agent of a marketplace Team waits for its skills claim like a
// built-in Agent, instead of being held. Then its HOME initializer alone reads
// its Team's subtree, with the selection of the release to copy.
func TestSkillMarketplaceCustomWaitsForTheTeamSkillsClaim(t *testing.T) {
	ctx := context.Background()
	f, input := newUnreconciledSandboxRuntimeFixture(t, true, []string{"example-skill"}, true, "custom", true, false,
		withCommonCustom(t, "headless"), withSkillMarketplace)
	reconcileUntilSkillsClaimWait(t, f)
	result, err := f.reconcile()
	assertSkillsClaimWait(t, f, result, err)
	if f.installer.stages != 0 {
		t.Fatal("the Custom Agent was installed before its skills claim existed")
	}

	createMarketplaceSkillsClaim(t, f, input)
	for pass := 0; pass < 3 && f.installer.stages == 0; pass++ {
		f.reconcileOK(t)
	}
	if f.installer.stages != 1 {
		t.Fatalf("the Custom Agent did not proceed once its skills claim existed: %d applies", f.installer.stages)
	}
	agent := f.current(t)
	if condition := apimeta.FindStatusCondition(agent.Status.Conditions, builtInPrerequisitesReady); condition != nil &&
		condition.Reason == skillsClaimPendingReason {
		t.Fatalf("prerequisite readiness still reports the finished wait: %+v", condition)
	}
	team := &api.Team{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: input.Team.Name}, team); err != nil {
		t.Fatal(err)
	}
	expected := reconciledExpectationForTest(t, f)
	index := slices.IndexFunc(expected.Workloads, func(workload w.RenderedWorkload) bool { return workload.Kind == "StatefulSet" })
	if index < 0 {
		t.Fatal("the expectation has no Agent workload")
	}
	spec := expected.Workloads[index].Template.Spec
	mount := corev1.VolumeMount{Name: "shared-skills", MountPath: w.SkillMarketplaceMountPath,
		SubPath: api.SkillMarketplaceTeamPath(string(team.UID)), ReadOnly: true}
	selection := corev1.EnvVar{Name: w.SkillMarketplaceSelectionEnv, Value: api.SkillMarketplaceSelectionDigest(team.Spec.Skills)}
	initializer := slices.IndexFunc(spec.InitContainers, func(container corev1.Container) bool { return container.Name == "init-home-data" })
	if initializer < 0 || !slices.ContainsFunc(spec.InitContainers[initializer].VolumeMounts, func(actual corev1.VolumeMount) bool {
		return reflect.DeepEqual(actual, mount)
	}) || !slices.Contains(spec.InitContainers[initializer].Env, selection) {
		t.Fatal("the Custom HOME initializer does not read the Team's subtree and selection")
	}
	for _, container := range append(slices.Clone(spec.Containers), spec.InitContainers...) {
		if container.Name != "init-home-data" && slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
			return mount.Name == "shared-skills"
		}) {
			t.Fatalf("container %s mounts the skills claim", container.Name)
		}
	}
}

// A Custom Agent pinned to an earlier Agent chart, which cannot give it its
// Team's skills, gets a configuration error, as a built-in Agent does. It is
// neither installed nor held, and its key stays usable.
func TestSkillMarketplaceCustomRefusesAnEarlierAgentChart(t *testing.T) {
	f, input := newUnreconciledSandboxRuntimeFixture(t, true, []string{"example-skill"}, true, "custom", true, false,
		withCommonCustom(t, "headless"), withSkillMarketplace)
	delete(f.installer.chart.Metadata.Annotations, "cetirun.microsoft.com/custom-skill-marketplace")
	reconcileUntilSkillsClaimWait(t, f)
	createMarketplaceSkillsClaim(t, f, input)
	if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "does not deliver Skill Marketplace skills to Custom Agents") {
		t.Fatal("an earlier Agent chart was not refused:", err)
	}
	agent := f.current(t)
	condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionAgentConfigReady)
	if agent.Status.Phase != api.PhaseFailed || condition == nil || condition.Reason != "AgentConfigInvalid" ||
		!strings.Contains(condition.Message, "upgrade the Agent") {
		t.Fatalf("phase %q, AgentConfigReady %+v; want a configuration error that asks for an upgrade", agent.Status.Phase, condition)
	}
	if slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) || f.installer.stages != 0 {
		t.Fatal("the refused Custom Agent was held or installed")
	}
	for _, key := range f.gateway.keys {
		if key.Blocked {
			t.Fatal("the refusal blocked the Agent's key")
		}
	}
}

// Without the marketplace, nothing delivers the skills that a policy-bound
// Team selects: the Agent gets a configuration error that says so, and it is
// not installed.
func TestMissingSkillsClaimWithoutMarketplaceFailsTheAgent(t *testing.T) {
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, []string{"example-skill"}, true, "openclaw", true, false)
	if _, err := f.reconcile(); err == nil {
		t.Fatal("the missing skills claim was not reported")
	}
	agent := f.current(t)
	condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionAgentConfigReady)
	if agent.Status.Phase != api.PhaseFailed || condition == nil || condition.Reason != "AgentConfigInvalid" ||
		!strings.Contains(condition.Message, "selects the skills example-skill, which Sandbox Agents receive only from the Skill Marketplace, and the Skill Marketplace is off") {
		t.Fatalf("phase %q, AgentConfigReady %+v; want a configuration error about the Skill Marketplace", agent.Status.Phase, condition)
	}
	if f.installer.stages != 0 {
		t.Fatal("an Agent without its skills claim was installed")
	}
}

func TestSkillsClaimPendingOnlyForMarketplaceClaims(t *testing.T) {
	missing := fmt.Errorf("read Sandbox prerequisite cetirun-example/skills-example: %w",
		apierrors.NewNotFound(corev1.Resource("persistentvolumeclaims"), "skills-example"))
	enabled := &KubernetesSandboxPrerequisites{SkillMarketplace: true}
	if !enabled.skillsClaimPending(missing, true) {
		t.Fatal("a missing marketplace claim is not a wait")
	}
	for name, test := range map[string]struct {
		source     *KubernetesSandboxPrerequisites
		err        error
		reconciled bool
	}{
		"marketplace disabled": {&KubernetesSandboxPrerequisites{}, missing, true},
		"held native lifetime": {enabled, missing, false},
		"read failure": {enabled, fmt.Errorf("read Sandbox prerequisite cetirun-example/skills-example: %w",
			apierrors.NewServiceUnavailable("synthetic outage")), true},
		"terminating claim": {enabled, fmt.Errorf("%w: Sandbox prerequisite cetirun-example/skills-example is not the persisted, non-terminating object",
			errSandboxSourceChanged), true},
	} {
		t.Run(name, func(t *testing.T) {
			if test.source.skillsClaimPending(test.err, test.reconciled) {
				t.Fatal("the existing skills claim error became a wait")
			}
		})
	}
	wrapped := fmt.Errorf("%w: %w", errSkillsClaimPending, missing)
	if !errors.Is(wrapped, errSkillsClaimPending) || !apierrors.IsNotFound(wrapped) {
		t.Fatal("the wait lost its sentinel or its API cause")
	}
}

// Marketplace skills select the skills claim for built-in and Custom Agents
// alike: the claim holds the subtree of every Team, and the HOME initializer
// copies only the release of the bound Team from it.
func TestSkillMarketplaceSkillsSelectTheSkillsClaim(t *testing.T) {
	team := &api.Team{}
	agent := func(kind api.AgentType) *api.Agent {
		return &api.Agent{Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: kind}}}
	}
	for _, test := range []struct {
		name   string
		kind   api.AgentType
		skills []string
		want   w.SkillsMode
	}{
		{"Custom", api.AgentTypeCustom, []string{"example-skill"}, ""},
		{"built-in", api.AgentTypeOpenClaw, []string{"example-skill"}, ""},
		{"Custom without a selection", api.AgentTypeCustom, nil, w.SkillsModeEmpty},
		{"built-in without a selection", api.AgentTypeHermes, nil, w.SkillsModeEmpty},
	} {
		t.Run(test.name, func(t *testing.T) {
			team.Spec.Skills = test.skills
			if got := sandboxSkillsMode(true, agent(test.kind), team); got != test.want {
				t.Fatalf("skills mode = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSkillMarketplaceReachesKubernetesPrerequisites(t *testing.T) {
	config, err := ParseSandboxSourcesConfig(nativeSandboxSourcesJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`"SkillMarketplace":true`, `"skillMarketplace":true`} {
		if _, err := ParseSandboxSourcesConfig(strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,`+raw, 1)); err == nil {
			t.Fatalf("CETIRUN_SANDBOX_SOURCES enabled the Skill Marketplace with %s", raw)
		}
	}
	r, kube, _ := aiGatewayTestReconciler(t)
	for _, enabled := range []bool{false, true} {
		config.SkillMarketplace = enabled
		preparation, err := NewKubernetesSandboxPreparation(config, kube, &nativeSandboxInstaller{}, r.OperatorNamespace, "0.1.1")
		if err != nil {
			t.Fatal(err)
		}
		if preparation.Prerequisites.(*KubernetesSandboxPrerequisites).SkillMarketplace != enabled {
			t.Fatalf("Skill Marketplace %t did not reach the Kubernetes prerequisites", enabled)
		}
	}
}
