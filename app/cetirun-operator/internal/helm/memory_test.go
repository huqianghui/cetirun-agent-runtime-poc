// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	corev1 "k8s.io/api/core/v1"
)

func TestMemoryOptionsPreserveLegacyCharts(t *testing.T) {
	legacy := &chart.Chart{Metadata: &chart.Metadata{Version: "1.0.0", AppVersion: "1.0.0"}, Values: map[string]any{}}
	for _, home := range []string{"/home/vibe", "/home/worker"} {
		for _, options := range []MemoryOptions{{Enabled: true}, {Enabled: true, Image: "registry.example.com/memory:2.0.0"}} {
			binding, configured, err := resolveMemory(map[string]any{"memoryOptions": options}, legacy, home)
			if err != nil || !configured || binding != nil {
				t.Fatalf("old pinned chart must retain its pre-Memory behavior: %+v %v", binding, err)
			}
		}
	}
	if _, _, err := resolveMemory(map[string]any{"memoryOptions": map[string]any{"Enabled": true}}, legacy, "/home/vibe"); err == nil {
		t.Fatal("JSON Helm overrides cannot impersonate platform Memory options")
	}
}

func TestMemoryOptionsResolveSelectedChartAppVersion(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes", "custom"} {
		for _, override := range []string{"", "registry.example.com/override:9.8.7"} {
			t.Run(typ+"/"+override, func(t *testing.T) {
				chrt := testChart(t)
				chrt.Metadata.Version, chrt.Metadata.AppVersion = "3.0.0", "2.0.0"
				input := sampleInput(typ, "openai", false)
				if typ == "custom" {
					input = customInput(8080)
				}
				input["memoryOptions"] = MemoryOptions{Enabled: true, Registry: "registry.example.com/staging", Image: override}
				prepared, err := PrepareChart(chrt, input)
				if err != nil {
					t.Fatal(err)
				}
				want := override
				if want == "" {
					want = "registry.example.com/staging/cetirun-memory-context:2.0.0"
				}
				objects := platformRender(t, prepared, chartutil.DefaultCapabilities)
				var pod corev1.PodSpec
				if err := decodeObject(object(object(object(objects["StatefulSet"]["spec"])["template"])["spec"]), &pod); err != nil {
					t.Fatal(err)
				}
				if got := pod.InitContainers[0].Image; got != want {
					t.Fatalf("selected Chart appVersion not honored: got %q want %q", got, want)
				}
				if _, leaked := prepared.Values["memoryOptions"]; leaked {
					t.Fatal("internal Memory options leaked into Helm values")
				}
				input["memoryOptions"] = MemoryOptions{Enabled: true, Image: "registry.example.com/memory:latest"}
				if _, err := PrepareChart(chrt, input); err == nil {
					t.Fatal("mutable Memory image accepted")
				}
			})
		}
	}
}

func TestMemoryChartDefaultsAndContext(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw", "custom"} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shared=%v", typ, shared), func(t *testing.T) {
				chrt := testChart(t)
				home := "/home/vibe"
				image := runtimebindings.DefaultMemoryImage("registry.example.com", "2.3.4")
				input := sampleInput(typ, "openai", false)
				input["global"] = map[string]any{"registry": "registry.example.com", "version": "2.3.4"}
				if typ == "custom" {
					input, home = customInput(8080), "/home/worker"
					bindings := input["customBindings"].(runtimebindings.Custom)
					bindings.Memory = &runtimebindings.Memory{Image: image, Home: home}
					input["customBindings"] = bindings
				}
				if shared {
					input["skills"] = sharedSkillValues()
				}
				prepared, err := PrepareChart(chrt, input)
				if err != nil {
					t.Fatal(err)
				}
				objects := platformRender(t, prepared, chartutil.DefaultCapabilities)
				var pod corev1.PodSpec
				if err := decodeObject(object(object(object(objects["StatefulSet"]["spec"])["template"])["spec"]), &pod); err != nil {
					t.Fatal(err)
				}
				if len(pod.InitContainers) < 2 || pod.InitContainers[0].Name != "memory-assets" || pod.InitContainers[0].Image != image || pod.InitContainers[1].Name != "init-home-data" {
					t.Fatal("Memory default image or initialization order differs")
				}
				assets := pod.InitContainers[0]
				if _, limited := assets.Resources.Limits[corev1.ResourceEphemeralStorage]; limited {
					t.Fatal("Memory asset init limit must not cap the whole Pod's ephemeral storage")
				}
				storage := assets.Resources.Requests[corev1.ResourceEphemeralStorage]
				if storage.String() != "40Mi" || assets.Resources.Limits.Cpu().String() != "100m" || assets.Resources.Limits.Memory().String() != "64Mi" {
					t.Fatal("Memory asset reservation or CPU/memory limits changed")
				}
				boundedAssets := false
				for _, volume := range pod.Volumes {
					if volume.Name == "memory-assets" {
						boundedAssets = volume.EmptyDir != nil && volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.String() == "40Mi"
					}
				}
				if !boundedAssets {
					t.Fatal("Memory assets must retain their volume-local storage bound")
				}
				main, init := pod.Containers[0], pod.InitContainers[1]
				expectedContext := []corev1.VolumeMount{runtimebindings.ContextMount(home)}
				if typ != "custom" {
					expectedContext = []corev1.VolumeMount{
						{Name: "context-view", MountPath: home + "/.agents/AGENTS.md", SubPath: "current/AGENTS.md", ReadOnly: true},
					}
					if shared {
						expectedContext = append(expectedContext, corev1.VolumeMount{Name: "shared-skills", MountPath: home + "/.aks/skills", ReadOnly: true})
					}
				}
				found := false
				homeMounted := false
				for _, mount := range main.VolumeMounts {
					homeMounted = homeMounted || mount.Name == "data" && mount.MountPath == home
					if typ == "custom" && (mount.Name == "shared-skills" || strings.HasPrefix(mount.MountPath, home+"/.agents/")) {
						t.Fatal("a nested source mount can hide the composed Memory skill")
					}
					if mount.MountPath == home+"/.agents" {
						if typ != "custom" || !homeMounted || found || !reflect.DeepEqual(mount, runtimebindings.ContextMount(home)) {
							t.Fatal("Agent context is not one read-only composed mount")
						}
						found = true
					}
				}
				if typ == "custom" && !found {
					t.Fatal("Agent context is missing")
				}
				for _, wanted := range expectedContext {
					if !slices.ContainsFunc(main.VolumeMounts, func(mount corev1.VolumeMount) bool { return reflect.DeepEqual(mount, wanted) }) {
						t.Fatalf("missing runtime context mount: %+v", wanted)
					}
				}
				for _, helper := range pod.InitContainers {
					if helper.Name != "doctor" {
						continue
					}
					assets := false
					for _, mount := range helper.VolumeMounts {
						assets = assets || reflect.DeepEqual(mount, runtimebindings.MemoryMounts()[0])
					}
					if !assets {
						t.Fatal("doctor cannot validate the configured Memory plugin and skills")
					}
					for _, wanted := range expectedContext {
						if !slices.ContainsFunc(helper.VolumeMounts, func(mount corev1.VolumeMount) bool { return reflect.DeepEqual(mount, wanted) }) {
							t.Fatalf("doctor is missing runtime context: %+v", wanted)
						}
					}
				}
				for _, wanted := range runtimebindings.ContextInitMounts(shared && typ == "custom", true) {
					matches := 0
					for _, actual := range init.VolumeMounts {
						if reflect.DeepEqual(actual, wanted) {
							matches++
						}
					}
					if matches != 1 {
						t.Fatalf("missing or duplicate context initializer mount: %+v", wanted)
					}
				}
				for _, wanted := range runtimebindings.ContextInitEnvironment(shared && typ == "custom", true) {
					found := false
					for _, actual := range init.Env {
						found = found || reflect.DeepEqual(actual, wanted)
					}
					if !found {
						t.Fatalf("missing context initializer environment: %s", wanted.Name)
					}
				}
			})
		}
	}
}

func TestMemoryDisabledPreservesLiveSharedSkillsMount(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw"} {
		input := sampleInput(typ, "openai", false)
		input["memoryContext"] = map[string]any{"enabled": false}
		input["skills"] = sharedSkillValues()
		prepared, err := PrepareChart(testChart(t), input)
		if err != nil {
			t.Fatal(err)
		}
		pod := registryRenderedTemplate(t, prepared).Spec
		if pod.InitContainers[0].Name != "init-home-data" {
			t.Fatal("disabled Memory still installed assets")
		}
		found := false
		for _, mount := range pod.Containers[0].VolumeMounts {
			if mount.Name == "memory-assets" || mount.Name == "context-view" {
				t.Fatal("disabled Memory changed native Skills delivery")
			}
			found = found || reflect.DeepEqual(mount, corev1.VolumeMount{Name: "shared-skills", MountPath: "/home/vibe/.aks/skills", ReadOnly: true})
		}
		if !found {
			t.Fatal("shared skills lost their original read-only PVC mount")
		}
		for _, container := range pod.InitContainers {
			for _, mount := range container.VolumeMounts {
				if mount.Name == "context-view" || mount.Name == "shared-skills" {
					t.Fatal("disabled Memory still snapshots native shared Skills")
				}
			}
		}
	}
}

func TestReconciledNativeMemoryHasComposedContext(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shared=%t", kind, shared), func(t *testing.T) {
				fixture := gatewayHeldFixture(kind)
				fixture.Input.Reconciled = true
				if !shared {
					fixture.Input.SkillsMode = w.SkillsModeEmpty
					fixture.Input.Skills = nil
				}
				bundle, err := w.Build(fixture.Input)
				if err != nil {
					t.Fatal(err)
				}
				input := sampleInput(kind, "openai", false)
				input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
				input["memoryOptions"] = MemoryOptions{Enabled: true, Image: "registry.example.com/memory:1.2.3"}
				prepared, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
				if err != nil {
					t.Fatal(err)
				}
				pod := registryRenderedTemplate(t, prepared).Spec
				if !slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(mount corev1.VolumeMount) bool {
					return reflect.DeepEqual(mount, corev1.VolumeMount{
						Name: "context-view", MountPath: "/home/vibe/.agents/AGENTS.md", SubPath: "current/AGENTS.md", ReadOnly: true,
					})
				}) {
					t.Fatal("native Memory cannot load the composed Agent context")
				}
				if slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(mount corev1.VolumeMount) bool {
					return mount.MountPath == "/home/vibe/.agents" || mount.MountPath == "/home/vibe/.agents/skills"
				}) {
					t.Fatal("native Memory hides the writable personal context")
				}
				foundInitializer := false
				for _, container := range pod.InitContainers {
					if container.Name != "init-home-data" {
						continue
					}
					foundInitializer = true
					for _, mount := range runtimebindings.ContextInitMounts(false, true) {
						if !slices.ContainsFunc(container.VolumeMounts, func(actual corev1.VolumeMount) bool {
							return reflect.DeepEqual(actual, mount)
						}) {
							t.Fatalf("context initializer is missing mount %+v", mount)
						}
					}
					for _, env := range runtimebindings.ContextInitEnvironment(false, true) {
						if !slices.ContainsFunc(container.Env, func(actual corev1.EnvVar) bool {
							return reflect.DeepEqual(actual, env)
						}) {
							t.Fatalf("context initializer is missing environment %s", env.Name)
						}
					}
					if slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == "shared-skills" }) {
						t.Fatal("native initialization still snapshots shared skills")
					}
				}
				if !foundInitializer {
					t.Fatal("native Memory has no context initializer")
				}
				if shared && !slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(mount corev1.VolumeMount) bool {
					return reflect.DeepEqual(mount, corev1.VolumeMount{
						Name: "shared-skills", MountPath: "/home/vibe/.aks/skills", ReadOnly: true,
					})
				}) {
					t.Fatal("native shared skills must retain the live read-only claim")
				}
				seen := map[string]bool{}
				for _, mount := range pod.Containers[0].VolumeMounts {
					if seen[mount.MountPath] {
						t.Fatalf("duplicate mount hides composed context: %s", mount.MountPath)
					}
					seen[mount.MountPath] = true
				}
				if !slices.ContainsFunc(pod.Volumes, func(volume corev1.Volume) bool {
					return volume.Name == "context-view" && volume.EmptyDir != nil &&
						volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.String() == fixture.Input.Resolution.Sandbox.Storage.Scratch.SizeLimit.String()
				}) {
					t.Fatal("composed context lost its Sandbox scratch accounting bound")
				}
			})
		}
	}
}

func TestSharedSkillAliasDoesNotReplaceCommonContext(t *testing.T) {
	for _, path := range []string{"", "/home/vibe/team-skills", "/home/vibe/.agents/skills"} {
		t.Run(fmt.Sprintf("alias=%s", path), func(t *testing.T) {
			input := sampleInput("hermes", "openai", false)
			skills := sharedSkillValues()
			aliasPath := path
			if path == "" {
				delete(skills, "mountPath")
				aliasPath = "/home/vibe/.aks/skills"
			} else {
				skills["mountPath"] = path
			}
			input["skills"] = skills
			prepared, err := PrepareChart(testChart(t), input)
			if err != nil {
				t.Fatal(err)
			}
			pod := registryRenderedTemplate(t, prepared).Spec
			common, alias := false, false
			for _, mount := range pod.Containers[0].VolumeMounts {
				common = common || reflect.DeepEqual(mount, corev1.VolumeMount{Name: "context-view", MountPath: "/home/vibe/.agents/AGENTS.md", SubPath: "current/AGENTS.md", ReadOnly: true})
				alias = alias || reflect.DeepEqual(mount, corev1.VolumeMount{Name: "shared-skills", MountPath: aliasPath, ReadOnly: true})
			}
			if !common || !alias {
				t.Fatal("the shared mount must stay live beside the reserved Memory skill")
			}
		})
	}
}

func TestCustomMemoryPreparedManifest(t *testing.T) {
	input := customInput(8080)
	bindings := input["customBindings"].(runtimebindings.Custom)
	bindings.Memory = &runtimebindings.Memory{Image: "registry.example.com/memory@sha256:" + strings.Repeat("a", 64), Home: "/home/worker"}
	input["customBindings"] = bindings
	prepared, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.ValidateCustom("cetirun-example", "openclaw-example"); err != nil {
		t.Fatal(err)
	}
	if object(prepared.Values["memoryContext"])["enabled"] != true {
		t.Fatal("memory not rendered")
	}
	bindings.Memory.Image = "registry.example.com/memory:latest"
	input["customBindings"] = bindings
	if _, err := PrepareChart(testChart(t), input); err == nil {
		t.Fatal("mutable asset image accepted")
	}
}

func TestSandboxRenderDoesNotInheritOrdinaryMemoryDefaults(t *testing.T) {
	for _, typ := range []string{"openclaw"} {
		t.Run(typ, func(t *testing.T) {
			fixture := gatewayHeldFixture(typ)
			bundle, err := w.Build(fixture.Input)
			if err != nil {
				t.Fatal(err)
			}
			input := sampleInput(typ, "openai", false)
			input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
			prepared, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			if template.Spec.InitContainers[0].Name != "init-home-data" || prepared.Values["replicaCount"] != 0 {
				t.Fatal("ordinary Memory defaults changed the protected workload")
			}
			for _, volume := range template.Spec.Volumes {
				if volume.Name == "memory-assets" || volume.Name == "context-view" {
					t.Fatal("ordinary Memory assets leaked into protected preparation")
				}
			}
			workload := testfixture.Workload(fixture.Input, *template)
			expected, err := bundle.Expect(*template, workload, nil)
			if err != nil {
				t.Fatal(err)
			}
			expected.ControllerRevision = workload.Status.UpdateRevision
			expected.SinglePod = true
			if err := w.ValidatePod(testfixture.Pod(expected), expected); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenClawMemoryRegistrationMatchesExecutionMode(t *testing.T) {
	for _, mode := range []string{"ordinary", "reconciled", "single-start"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%t", mode, enabled), func(t *testing.T) {
				chrt := testChart(t)
				input := sampleInput("openclaw", "openai", false)
				input["memoryOptions"] = MemoryOptions{Enabled: enabled, Image: "registry.example.com/memory:1.2.3"}
				var prepared *PreparedRelease
				var err error
				if mode == "ordinary" {
					prepared, err = PrepareChart(chrt, input)
				} else {
					fixture := gatewayHeldFixture("openclaw")
					fixture.Input.Reconciled = mode == "reconciled"
					bundle, buildErr := w.Build(fixture.Input)
					if buildErr != nil {
						t.Fatal(buildErr)
					}
					input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
					prepared, err = PrepareSandboxChart(chrt, input, nil, bundle)
				}
				if err != nil {
					t.Fatal(err)
				}
				native := object(renderSnapshot(t, chrt, prepared.Values).Native["openclaw.json"])
				plugins := object(native["plugins"])
				entry := object(object(plugins["entries"])["cetirun-memory-context"])
				registered := enabled && mode != "single-start"
				if (entry["enabled"] == true) != registered ||
					(object(entry["hooks"])["allowConversationAccess"] == true) != registered {
					t.Fatal("native Memory plugin registration differs from the selected execution mode")
				}
				for _, list := range []struct {
					values []any
					item   string
				}{
					{array(plugins["allow"]), "cetirun-memory-context"},
					{array(object(plugins["load"])["paths"]), "/opt/cetirun-memory/adapters/openclaw"},
					{array(object(object(native["skills"])["load"])["extraDirs"]), "/home/vibe/.agents/skills"},
				} {
					count := 0
					for _, value := range list.values {
						if text(value) == list.item {
							count++
						}
					}
					want := 0
					if registered {
						want = 1
					}
					if count != want {
						t.Fatalf("native configuration has %d copies of %q, want %d", count, list.item, want)
					}
				}
				pod := registryRenderedTemplate(t, prepared).Spec
				if assets := slices.ContainsFunc(pod.InitContainers, func(container corev1.Container) bool {
					return container.Name == "memory-assets"
				}); assets != registered {
					t.Fatal("Memory asset installation and native adapter registration disagree")
				}
			})
		}
	}
}

func TestHermesMemoryRegistrationMatchesExecutionMode(t *testing.T) {
	for _, mode := range []string{"ordinary", "reconciled", "single-start"} {
		for _, enabled := range []bool{false, true} {
			for _, shared := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/enabled=%t/shared=%t", mode, enabled, shared), func(t *testing.T) {
					chrt := testChart(t)
					input := sampleInput("hermes", "openai", false)
					input["skills"] = map[string]any{
						"shared": shared, "resourceGroup": "test", "storageAccount": "test", "containerName": "skills",
					}
					input["memoryOptions"] = MemoryOptions{Enabled: enabled, Image: "registry.example.com/memory:1.2.3"}
					input["hermes"].(map[string]any)["config"] = map[string]any{
						"plugins": map[string]any{"enabled": []any{"example-plugin"}},
						"skills":  map[string]any{"external_dirs": []any{"/home/vibe/example-skills"}},
					}
					var prepared *PreparedRelease
					var err error
					if mode == "ordinary" {
						prepared, err = PrepareChart(chrt, input)
					} else {
						fixture := gatewayHeldFixture("hermes")
						if mode == "single-start" {
							if _, err := w.Build(fixture.Input); err == nil {
								t.Fatal("held Hermes workload accepted a Gateway key")
							}
							return
						}
						fixture.Input.Reconciled = true
						if !shared {
							fixture.Input.SkillsMode = w.SkillsModeEmpty
							fixture.Input.Skills = nil
						}
						bundle, buildErr := w.Build(fixture.Input)
						if buildErr != nil {
							t.Fatal(buildErr)
						}
						input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
						prepared, err = PrepareSandboxChart(chrt, input, nil, bundle)
					}
					if err != nil {
						t.Fatal(err)
					}
					native := object(renderSnapshot(t, chrt, prepared.Values).Native["hermes-config.yaml"])
					registered := enabled && mode != "single-start"
					for _, list := range []struct {
						values  []any
						item    string
						present bool
					}{
						{array(object(native["plugins"])["enabled"]), "cetirun-memory-context", registered},
						{array(object(native["plugins"])["enabled"]), "example-plugin", true},
						{array(object(native["skills"])["external_dirs"]), "/home/vibe/.agents/skills", registered},
						{array(object(native["skills"])["external_dirs"]), "/opt/cetirun-memory/skills", registered},
						{array(object(native["skills"])["external_dirs"]), "/home/vibe/.aks/skills", shared},
						{array(object(native["skills"])["external_dirs"]), "/home/vibe/example-skills", true},
					} {
						count := 0
						for _, value := range list.values {
							if text(value) == list.item {
								count++
							}
						}
						want := 0
						if list.present {
							want = 1
						}
						if count != want {
							t.Fatalf("native configuration has %d copies of %q, want %d", count, list.item, want)
						}
					}
					pod := registryRenderedTemplate(t, prepared).Spec
					if assets := slices.ContainsFunc(pod.InitContainers, func(container corev1.Container) bool {
						return container.Name == "memory-assets"
					}); assets != registered {
						t.Fatal("Memory asset installation and Hermes plugin enablement disagree")
					}
				})
			}
		}
	}
}

func TestReconciledOpenClawMemoryRejectsDisabledPluginHooks(t *testing.T) {
	for name, plugins := range map[string]map[string]any{
		"plugins": {"enabled": false},
		"adapter": {"entries": map[string]any{"cetirun-memory-context": map[string]any{"enabled": false}}},
		"conversation hook": {"entries": map[string]any{"cetirun-memory-context": map[string]any{
			"hooks": map[string]any{"allowConversationAccess": false},
		}}},
		"prompt hook": {"entries": map[string]any{"cetirun-memory-context": map[string]any{
			"hooks": map[string]any{"allowPromptInjection": false},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			bundle, input := reconciledSandboxInputs(t)
			input["memoryOptions"] = MemoryOptions{Enabled: true, Image: "registry.example.com/memory:1.2.3"}
			input["openclaw"] = map[string]any{"config": map[string]any{"plugins": plugins}}
			if _, err := PrepareSandboxChart(testChart(t), input, nil, bundle); err == nil ||
				!strings.Contains(err.Error(), "native config does not match the selected profile") {
				t.Fatal("reconciled Memory accepted an explicitly disabled adapter or hook:", err)
			}
			input["memoryOptions"] = MemoryOptions{Enabled: false}
			if _, err := PrepareSandboxChart(testChart(t), input, nil, bundle); err != nil {
				t.Fatal("disabled Memory should not require its adapter or hook:", err)
			}
		})
	}
}

func TestNativeMemoryRenderingPreservesSharedSkills(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw"} {
		t.Run(typ, func(t *testing.T) {
			chrt := testChart(t)
			input := sampleInput(typ, "openai", true)
			input["memoryContext"] = (&runtimebindings.Memory{Image: "registry.example.com/memory@sha256:" + strings.Repeat("a", 64), Home: "/home/vibe"}).Values()
			prepared, err := PrepareChart(chrt, input)
			if err != nil {
				t.Fatal(err)
			}
			if object(prepared.Values["skills"])["shared"] != true {
				t.Fatal("Memory changed the shared Skills binding")
			}
			snapshot := renderSnapshot(t, chrt, prepared.Values)
			native, _ := json.Marshal(snapshot.Native)
			if !strings.Contains(string(native), "/home/vibe/.agents/skills") || !strings.Contains(string(native), "cetirun-memory-context") {
				t.Fatal("Memory is absent from native configuration")
			}
			pod := registryRenderedTemplate(t, prepared).Spec
			for _, volume := range pod.Volumes {
				if volume.Name == "memory-policy" {
					t.Fatal("retained a Memory policy volume")
				}
			}
			for _, env := range pod.Containers[0].Env {
				if env.Name == "CETIRUN_MEMORY_POLICY" {
					t.Fatal("retained a Memory policy environment binding")
				}
			}
			found := false
			for _, init := range pod.InitContainers {
				if init.Name == "memory-assets" {
					found = true
					if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].Name != "memory-assets" || !*init.SecurityContext.ReadOnlyRootFilesystem {
						t.Fatal("Memory initializer can access more than its asset volume")
					}
				}
			}
			if !found {
				t.Fatal("Memory initializer is missing")
			}
		})
	}
}

func TestNativeMemoryRejectsReservedOverrides(t *testing.T) {
	for name, override := range map[string]map[string]any{
		"environment":           {"extraEnv": []any{map[string]any{"name": "CETIRUN_MEMORY_ROOT", "value": "/other"}}},
		"asset ancestor":        {"extraVolumeMounts": []any{map[string]any{"name": "extra", "mountPath": "/opt"}}},
		"asset child":           {"extraVolumeMounts": []any{map[string]any{"name": "extra", "mountPath": "/opt/cetirun-memory/bin"}}},
		"context parent":        {"extraVolumeMounts": []any{map[string]any{"name": "extra", "mountPath": "/home/vibe/.agents"}}},
		"context skill":         {"extraVolumeMounts": []any{map[string]any{"name": "extra", "mountPath": "/home/vibe/.agents/skills/cetirun-memory"}}},
		"context volume":        {"extraVolumes": []any{map[string]any{"name": "context-view", "emptyDir": map[string]any{}}}},
		"context sidecar":       {"sidecars": []any{map[string]any{"name": "extra", "image": "registry.example.com/fixture:1", "volumeMounts": []any{map[string]any{"name": "context-view", "mountPath": "/other"}}}}},
		"normalized asset":      {"extraVolumeMounts": []any{map[string]any{"name": "extra", "mountPath": "/opt/./cetirun-memory"}}},
		"filesystem root":       {"extraVolumeMounts": []any{map[string]any{"name": "extra", "mountPath": "/"}}},
		"private root ancestor": {"extraVolumeMounts": []any{map[string]any{"name": "extra", "mountPath": "/home/vibe/.cetirun"}}},
		"asset volume":          {"extraVolumes": []any{map[string]any{"name": "memory-assets", "emptyDir": map[string]any{}}}},
		"sidecar":               {"sidecars": []any{map[string]any{"name": "extra", "image": "registry.example.com/fixture:1", "volumeMounts": []any{map[string]any{"name": "memory-assets", "mountPath": "/other"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			input := sampleInput("hermes", "openai", false)
			input["memoryContext"] = (&runtimebindings.Memory{Image: "registry.example.com/memory@sha256:" + strings.Repeat("a", 64), Home: "/home/vibe"}).Values()
			for key, value := range override {
				input[key] = value
			}
			chrt := testChart(t)
			prepared, err := PrepareChart(chrt, input)
			if err != nil {
				return
			}
			values, err := chartutil.ToRenderValues(chrt, prepared.Values, chartutil.ReleaseOptions{Name: "fixture", Namespace: "fixture", IsInstall: true}, chartutil.DefaultCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Render(chrt, values); err == nil {
				t.Fatal("reserved Memory override accepted")
			}
		})
	}
}
