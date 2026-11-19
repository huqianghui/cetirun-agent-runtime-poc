// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func testSkillMarketplace() *w.SkillMarketplaceInput {
	return &w.SkillMarketplaceInput{
		TeamPath:        api.SkillMarketplaceTeamPath("team-uid"),
		SelectionDigest: api.SkillMarketplaceSelectionDigest([]string{"example-skill"}),
	}
}

// renderSkillMarketplace renders a common-executor built-in Agent of the
// fixture Team, whose skills claim the Skill Marketplace provides.
func renderSkillMarketplace(t *testing.T, chrt *chart.Chart, kind string, marketplace *w.SkillMarketplaceInput, memory bool) (*w.Bundle, *PreparedRelease, error) {
	t.Helper()
	fixture := gatewayHeldFixture(kind)
	fixture.Input.Reconciled = true
	fixture.Input.SkillMarketplace = marketplace
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput(kind, "openai", false)
	input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	// The operator always supplies Memory options; disabled ones turn off the
	// chart's default composed context.
	input["memoryOptions"] = MemoryOptions{}
	if memory {
		input["memoryOptions"] = MemoryOptions{Enabled: true, Image: "registry.example.com/memory:1.2.3"}
	}
	prepared, err := PrepareSandboxChart(chrt, input, nil, bundle)
	return bundle, prepared, err
}

func skillMarketplaceMount(marketplace *w.SkillMarketplaceInput) corev1.VolumeMount {
	return corev1.VolumeMount{Name: "shared-skills", MountPath: w.SkillMarketplaceMountPath, SubPath: marketplace.TeamPath, ReadOnly: true}
}

func TestReconciledSkillMarketplaceMountsOnlyTheTeamSubtree(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			marketplace := testSkillMarketplace()
			_, prepared, err := renderSkillMarketplace(t, testChart(t), kind, marketplace, true)
			if err != nil {
				t.Fatal(err)
			}
			skills := object(prepared.Values["skills"])
			if skills["subPath"] != marketplace.TeamPath || skills["shared"] != true ||
				object(skills["marketplace"])["selectionDigest"] != marketplace.SelectionDigest {
				t.Fatalf("protected skills values = %#v", skills)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			selection := corev1.EnvVar{Name: w.SkillMarketplaceSelectionEnv, Value: marketplace.SelectionDigest}
			initializers := 0
			for group, containers := range [][]corev1.Container{template.Spec.Containers, template.Spec.InitContainers} {
				for _, container := range containers {
					initializer := group == 1 && container.Name == "init-home-data"
					mounted := slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == "shared-skills" })
					if mounted != initializer || slices.Contains(container.Env, selection) != initializer ||
						initializer && !slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
							return reflect.DeepEqual(mount, skillMarketplaceMount(marketplace))
						}) {
						t.Fatalf("%s mounts %+v with environment %+v", container.Name, container.VolumeMounts, container.Env)
					}
					if initializer {
						initializers++
					}
				}
			}
			if initializers != 1 {
				t.Fatal("the render has no HOME initializer")
			}
			assertSkillMarketplacePodAdmission(t, prepared)
		})
	}
}

// assertSkillMarketplacePodAdmission checks that Pod admission, which compares
// the Pod with the exact template, the subtree and selection included,
// accepts a Pod of the rendered template and refuses one that mounts the
// whole skills claim.
func assertSkillMarketplacePodAdmission(t *testing.T, prepared *PreparedRelease) {
	t.Helper()
	expected, err := prepared.ReconcileExpectation()
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(expected.Workloads, func(workload w.RenderedWorkload) bool { return workload.Kind == "StatefulSet" })
	if index < 0 {
		t.Fatal("the expectation has no Agent workload")
	}
	workload := expected.Workloads[index]
	owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: workload.Identity.Name,
		UID: "statefulset-uid", Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}
	pod := &corev1.Pod{ObjectMeta: *workload.Template.ObjectMeta.DeepCopy(), Spec: *workload.Template.Spec.DeepCopy()}
	pod.Name, pod.OwnerReferences = workload.Identity.Name+"-0", []metav1.OwnerReference{owner}
	pod.Labels["statefulset.kubernetes.io/pod-name"], pod.Labels["apps.kubernetes.io/pod-index"] = pod.Name, "0"
	pod.Labels["controller-revision-hash"] = "current-revision"
	pod.Spec.Hostname, pod.Spec.Subdomain, pod.Spec.Overhead = pod.Name, workload.Identity.Name, workload.Overhead.DeepCopy()
	if err := w.ValidateRenderedPod(pod, workload, owner, "current-revision"); err != nil {
		t.Fatal("admission refused the Pod of the rendered template:", err)
	}
	whole := pod.DeepCopy()
	for i := range whole.Spec.InitContainers {
		for j := range whole.Spec.InitContainers[i].VolumeMounts {
			if whole.Spec.InitContainers[i].VolumeMounts[j].Name == "shared-skills" {
				whole.Spec.InitContainers[i].VolumeMounts[j].SubPath = ""
			}
		}
	}
	if err := w.ValidateRenderedPod(whole, workload, owner, "current-revision"); err == nil {
		t.Fatal("admission accepted a Pod that mounts the whole skills claim")
	}
}

// renderCustomSkillMarketplace renders a common-executor Custom Agent of the
// fixture Team, whose skills claim the Skill Marketplace provides.
func renderCustomSkillMarketplace(t *testing.T, chrt *chart.Chart, marketplace *w.SkillMarketplaceInput) (*w.Bundle, *PreparedRelease, error) {
	t.Helper()
	input, values := customSandboxRenderInput(nil, false)
	input.Skills, input.SkillsMode = testfixture.New("openclaw").Input.Skills, ""
	input.SkillMarketplace = marketplace
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareSandboxChart(chrt, values, nil, bundle)
	return bundle, prepared, err
}

// A Custom Agent of a Skill Marketplace Team receives its Team's release like
// a built-in Agent: only the HOME initializer mounts the Team's subtree and
// gets the selection, and the Agent reads the copy in its context view.
func TestReconciledSkillMarketplaceCustomReadsOnlyTheTeamSubtree(t *testing.T) {
	marketplace := testSkillMarketplace()
	bundle, prepared, err := renderCustomSkillMarketplace(t, testChart(t), marketplace)
	if err != nil {
		t.Fatal(err)
	}
	skills := object(prepared.Values["skills"])
	if skills["subPath"] != marketplace.TeamPath || skills["shared"] != true ||
		object(skills["marketplace"])["selectionDigest"] != marketplace.SelectionDigest {
		t.Fatalf("protected skills values = %#v", skills)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	selection := corev1.EnvVar{Name: w.SkillMarketplaceSelectionEnv, Value: marketplace.SelectionDigest}
	for group, containers := range [][]corev1.Container{template.Spec.Containers, template.Spec.InitContainers} {
		for _, container := range containers {
			initializer := group == 1 && container.Name == "init-home-data"
			mounted := slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == "shared-skills" })
			if mounted != initializer || slices.Contains(container.Env, selection) != initializer ||
				initializer && !slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
					return reflect.DeepEqual(mount, skillMarketplaceMount(marketplace))
				}) {
				t.Fatalf("%s mounts %+v with environment %+v", container.Name, container.VolumeMounts, container.Env)
			}
		}
	}
	if !slices.ContainsFunc(template.Spec.Containers[0].VolumeMounts, func(mount corev1.VolumeMount) bool {
		return mount.Name == "context-view" && mount.MountPath == "/home/worker/.agents" && mount.SubPath == "current" && mount.ReadOnly
	}) {
		t.Fatal("the Custom Agent does not read the context view, where its initializer copies the release")
	}
	// The operator validates with the Memory setting of the render.
	memory := object(prepared.Values["memoryContext"])["enabled"] == true
	if err := bundle.ValidateReconciledTemplate(template.DeepCopy(), prepared.Agent.Image, memory, prepared.Agent.Port); err != nil {
		t.Fatal("the rendered Custom template was refused:", err)
	}
	assertSkillMarketplacePodAdmission(t, prepared)
	initializer := func(spec *corev1.PodSpec) *corev1.Container {
		index := slices.IndexFunc(spec.InitContainers, func(container corev1.Container) bool { return container.Name == "init-home-data" })
		return &spec.InitContainers[index]
	}
	initializerMount := func(spec *corev1.PodSpec) *corev1.VolumeMount {
		container := initializer(spec)
		index := slices.IndexFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == "shared-skills" })
		return &container.VolumeMounts[index]
	}
	withoutSelection := func(env []corev1.EnvVar) []corev1.EnvVar {
		return slices.DeleteFunc(slices.Clone(env), func(env corev1.EnvVar) bool { return env.Name == w.SkillMarketplaceSelectionEnv })
	}
	for name, mutate := range map[string]func(*corev1.PodSpec){
		"whole claim":       func(s *corev1.PodSpec) { initializerMount(s).SubPath = "" },
		"another Team":      func(s *corev1.PodSpec) { initializerMount(s).SubPath = "teams/another-team" },
		"missing selection": func(s *corev1.PodSpec) { initializer(s).Env = withoutSelection(initializer(s).Env) },
		"runtime mount": func(s *corev1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, skillMarketplaceMount(marketplace))
		},
		// The Custom initializer contract fixes the order of its environment.
		"selection moved": func(s *corev1.PodSpec) {
			container := initializer(s)
			container.Env = append([]corev1.EnvVar{selection}, withoutSelection(container.Env)...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := template.DeepCopy()
			mutate(&changed.Spec)
			if err := bundle.ValidateReconciledTemplate(changed, prepared.Agent.Image, memory, prepared.Agent.Port); err == nil {
				t.Fatal("an unsafe Custom Skill Marketplace template was accepted")
			}
		})
	}
}

// An Agent chart without the Custom Skill Marketplace annotation is refused
// before it renders a Custom Agent that receives its Team's skills.
func TestCustomSkillMarketplaceRequiresTheChartAnnotation(t *testing.T) {
	earlier := testChart(t)
	delete(earlier.Metadata.Annotations, customSkillMarketplaceAnnotation)
	_, _, err := renderCustomSkillMarketplace(t, earlier, testSkillMarketplace())
	if !errors.Is(err, ErrCustomSkillMarketplaceUnsupported) || !strings.Contains(err.Error(), "Agent chart "+earlier.Metadata.Version+" ") ||
		!strings.Contains(err.Error(), "upgrade the Agent") {
		t.Fatal("an earlier chart rendered Skill Marketplace values for a Custom Agent:", err)
	}
	// Without Team skills, and for a built-in Agent, the annotation does not
	// decide the render.
	if _, _, err := renderCustomSkillMarketplace(t, earlier, nil); err != nil {
		t.Fatal("a Custom Agent without Team skills needed the annotation:", err)
	}
	if _, _, err := renderSkillMarketplace(t, earlier, "openclaw", testSkillMarketplace(), true); err != nil {
		t.Fatal("a built-in Agent needed the Custom annotation:", err)
	}
}

func TestReconciledSkillMarketplaceRejectsUnsafeTemplates(t *testing.T) {
	marketplace := testSkillMarketplace()
	bundle, prepared, err := renderSkillMarketplace(t, testChart(t), "openclaw", marketplace, true)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.ValidateReconciledTemplate(template.DeepCopy(), prepared.Agent.Image, true, prepared.Agent.Port); err != nil {
		t.Fatal("the rendered template was refused:", err)
	}
	initializer := func(spec *corev1.PodSpec) *corev1.Container {
		index := slices.IndexFunc(spec.InitContainers, func(container corev1.Container) bool { return container.Name == "init-home-data" })
		return &spec.InitContainers[index]
	}
	initializerMount := func(spec *corev1.PodSpec) *corev1.VolumeMount {
		container := initializer(spec)
		index := slices.IndexFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == "shared-skills" })
		return &container.VolumeMounts[index]
	}
	initializerSelection := func(spec *corev1.PodSpec) *corev1.EnvVar {
		container := initializer(spec)
		index := slices.IndexFunc(container.Env, func(env corev1.EnvVar) bool { return env.Name == w.SkillMarketplaceSelectionEnv })
		return &container.Env[index]
	}
	for name, mutate := range map[string]func(*corev1.PodSpec){
		"whole claim":  func(s *corev1.PodSpec) { initializerMount(s).SubPath = "" },
		"another Team": func(s *corev1.PodSpec) { initializerMount(s).SubPath = "teams/another-team" },
		"parent path":  func(s *corev1.PodSpec) { initializerMount(s).SubPath = "teams" },
		"writable":     func(s *corev1.PodSpec) { initializerMount(s).ReadOnly = false },
		"other path":   func(s *corev1.PodSpec) { initializerMount(s).MountPath = "/etc/cetirun-other-skills" },
		"two subtrees": func(s *corev1.PodSpec) {
			mount := *initializerMount(s)
			mount.MountPath, mount.SubPath = "/etc/cetirun-other-skills", "teams/another-team"
			initializer(s).VolumeMounts = append(initializer(s).VolumeMounts, mount)
		},
		"runtime mount": func(s *corev1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, skillMarketplaceMount(marketplace))
		},
		"helper mount": func(s *corev1.PodSpec) {
			index := slices.IndexFunc(s.InitContainers, func(container corev1.Container) bool { return container.Name != "init-home-data" })
			s.InitContainers[index].VolumeMounts = append(s.InitContainers[index].VolumeMounts, skillMarketplaceMount(marketplace))
		},
		"missing mount": func(s *corev1.PodSpec) {
			container := initializer(s)
			container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == "shared-skills" })
		},
		"writable claim": func(s *corev1.PodSpec) {
			for i := range s.Volumes {
				if s.Volumes[i].Name == "shared-skills" {
					s.Volumes[i].PersistentVolumeClaim.ReadOnly = false
				}
			}
		},
		"missing selection": func(s *corev1.PodSpec) {
			container := initializer(s)
			container.Env = slices.DeleteFunc(container.Env, func(env corev1.EnvVar) bool { return env.Name == w.SkillMarketplaceSelectionEnv })
		},
		"other selection": func(s *corev1.PodSpec) {
			initializerSelection(s).Value = api.SkillMarketplaceSelectionDigest([]string{"another-skill"})
		},
		"duplicate selection": func(s *corev1.PodSpec) {
			initializer(s).Env = append(initializer(s).Env, *initializerSelection(s))
		},
		"runtime selection": func(s *corev1.PodSpec) {
			s.Containers[0].Env = append(s.Containers[0].Env, *initializerSelection(s))
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := template.DeepCopy()
			mutate(&changed.Spec)
			if err := bundle.ValidateReconciledTemplate(changed, prepared.Agent.Image, true, prepared.Agent.Port); err == nil ||
				!strings.Contains(err.Error(), "Skill Marketplace") {
				t.Fatal("an unsafe Skill Marketplace template was accepted or misclassified:", err)
			}
		})
	}
}

// An Agent chart that ignores the marketplace values would mount the whole
// claim, with the subtree of every Team; such a render is refused.
func TestReconciledSkillMarketplaceRefusesAChartWithoutTheTeamSubtree(t *testing.T) {
	for name, block := range map[string]string{
		"subtree":   "{{- with .Values.skills.subPath }}",
		"selection": "{{- with (.Values.skills.marketplace | default dict).selectionDigest }}",
	} {
		t.Run(name, func(t *testing.T) {
			chrt := testChart(t)
			changed := false
			for _, template := range chrt.Templates {
				if template.Name == "templates/_context.tpl" && bytes.Count(template.Data, []byte(block)) == 1 {
					template.Data = bytes.Replace(template.Data, []byte(block), []byte("{{- with .Values.skills.ignoredByAnOlderChart }}"), 1)
					changed = true
				}
			}
			if !changed {
				t.Fatal("test setup: the chart's Skill Marketplace rendering moved")
			}
			if _, _, err := renderSkillMarketplace(t, chrt, "openclaw", testSkillMarketplace(), true); err == nil ||
				!strings.Contains(err.Error(), "Skill Marketplace") {
				t.Fatal("a chart without the Team subtree or selection was accepted:", err)
			}
		})
	}
}

func TestReconciledSkillMarketplaceRequiresTheComposedContext(t *testing.T) {
	if _, _, err := renderSkillMarketplace(t, testChart(t), "openclaw", testSkillMarketplace(), false); err == nil ||
		!strings.Contains(err.Error(), "composed Agent context") {
		t.Fatal("Skill Marketplace skills rendered without the composed Agent context:", err)
	}
}

func TestReconciledSkillsWithoutMarketplaceKeepTheirRender(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			bundle, prepared, err := renderSkillMarketplace(t, testChart(t), kind, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			skills := object(prepared.Values["skills"])
			if _, present := skills["subPath"]; present {
				t.Fatal("a render without the marketplace selected a subtree")
			}
			if _, present := skills["marketplace"]; present {
				t.Fatal("a render without the marketplace has a selection")
			}
			raw, err := json.Marshal(bundle.Identity())
			if err != nil || strings.Contains(string(raw), "SkillMarketplace") {
				t.Fatal("the protected input gained a serialized field:", err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			for _, container := range append(slices.Clone(template.Spec.Containers), template.Spec.InitContainers...) {
				for _, mount := range container.VolumeMounts {
					if mount.Name == "shared-skills" && mount.SubPath != "" {
						t.Fatalf("%s mounts a subtree of the skills claim", container.Name)
					}
				}
				if slices.ContainsFunc(container.Env, func(env corev1.EnvVar) bool { return env.Name == w.SkillMarketplaceSelectionEnv }) {
					t.Fatalf("%s received a Skill Marketplace selection", container.Name)
				}
			}
		})
	}
}
