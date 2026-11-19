// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func reconciledSandboxInputs(t *testing.T) (*w.Bundle, map[string]any) {
	t.Helper()
	f := testfixture.New("openclaw")
	f.Input.Reconciled = true
	f.Input.AuthorizedSecrets = append(f.Input.AuthorizedSecrets, "model-key")
	f.Input.GatewayKey = &w.ObjectIdentity{Name: "model-key", UID: "model-key-uid"}
	bundle, err := w.Build(f.Input)
	if err != nil {
		t.Fatal(err)
	}
	values := sampleInput("openclaw", "openai", false)
	values["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	values["aiGateway"] = map[string]any{
		"enabled": true, "keySecretName": "model-key", "keySecretKey": "api-key",
		"models": []any{map[string]any{"id": "example-model", "name": "Example"}},
		"copilotProxy": map[string]any{
			"enabled": true, "image": "registry.example.com/copilot-proxy:example",
			"masterKeySecretName": "proxy-key", "tokenSecretName": "copilot-token",
		},
	}
	values["secrets"].(map[string]any)["model"] = "example-model"
	values["secrets"].(map[string]any)["apiBase"] = "http://gateway.example.svc.cluster.local:4000/v1"
	return bundle, values
}

func TestSandboxReusesCopilotResourceAssembly(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	prepared, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := prepared.SandboxManifest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"kind":"Deployment"`) ||
		!strings.Contains(string(manifest), "copilot-proxy") {
		t.Fatal("Sandbox discarded the existing Copilot resources")
	}
}

func TestReconciledSandboxHelperResources(t *testing.T) {
	for _, memory := range []string{"512Mi", "2Gi", "3Gi"} {
		t.Run(memory, func(t *testing.T) {
			bundle, values := reconciledSandboxInputs(t)
			input := bundle.Identity()
			input.Helpers = w.Helpers{}
			input.Resolution.Sandbox.Resources.Helpers.Limits[corev1.ResourceMemory] = resource.MustParse(memory)
			bundle, err := w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"doctor"} {
				if !slices.ContainsFunc(template.Spec.InitContainers, func(container corev1.Container) bool {
					return container.Name == name
				}) {
					t.Errorf("reconciled OpenClaw must retain the chart-default %s init container despite disabled legacy helper selection", name)
				}
			}
			helpers := input.Resolution.Sandbox.Resources.Helpers
			want := corev1.ResourceRequirements{Requests: helpers.Requests, Limits: helpers.Limits}
			for _, containers := range [][]corev1.Container{template.Spec.InitContainers, template.Spec.Containers[1:]} {
				for _, container := range containers {
					if !equality.Semantic.DeepEqual(container.Resources, want) {
						t.Errorf("%s resources = %v, want Sandbox helper resources %v", container.Name, container.Resources, want)
					}
				}
			}
			accounting := w.AccountResources(template.Spec, bundle.Overhead())
			if template.Spec.Resources == nil || !equality.Semantic.DeepEqual(*template.Spec.Resources, accounting.Pod) {
				t.Fatal("helper resources are missing from aggregate Pod resource accounting")
			}
		})
	}
}

func TestReconciledSandboxStartupWindowAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		user   map[string]any
		want   int32
		absent bool
	}{
		{name: "default", want: 60},
		{name: "explicit old threshold", user: map[string]any{"failureThreshold": 30}, want: 30},
		{name: "explicit threshold", user: map[string]any{"failureThreshold": 7}, want: 7},
		{name: "reset default", user: map[string]any{"failureThreshold": nil}, want: 60},
		{name: "disabled", user: map[string]any{"enabled": false}, absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, values := reconciledSandboxInputs(t)
			if tc.user != nil {
				if err := w.ValidateReconciledUserValues(map[string]any{"startupProbe": tc.user}, bundle.Identity().AuthorizedSecrets); err != nil {
					t.Fatal(err)
				}
				values["startupProbe"] = tc.user
			}
			base, err := PrepareChart(testChart(t), values)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := base.ReconcileSandbox(values, bundle, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			main := template.Spec.Containers[0]
			if (main.StartupProbe == nil) != tc.absent {
				t.Fatal("startup probe enablement changed")
			}
			if main.StartupProbe != nil &&
				(main.StartupProbe.FailureThreshold != tc.want || main.StartupProbe.PeriodSeconds != 10 ||
					main.StartupProbe.TimeoutSeconds != 5 || main.StartupProbe.Exec == nil) {
				t.Fatalf("common startup window or loopback probe differs: %+v", main.StartupProbe)
			}
			if main.LivenessProbe == nil || main.ReadinessProbe == nil ||
				main.LivenessProbe.Exec == nil || main.ReadinessProbe.Exec == nil {
				t.Fatal("startup defaults disabled runtime health checks")
			}
		})
	}
}

func TestReconciledSandboxPullSecretIsKubeletOnly(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			fixture := gatewayHeldFixture(kind)
			fixture.Input.Reconciled = true
			fixture.Input.ImagePullSecret = &w.ObjectIdentity{Name: "registry-pull-exampleuid", UID: "pull-secret-uid"}
			bundle, err := w.Build(fixture.Input)
			if err != nil {
				t.Fatal(err)
			}
			input := sampleInput(kind, "openai", false)
			input["agentRequest"].(map[string]any)["pullSecretName"] = fixture.Input.ImagePullSecret.Name
			input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
			prepared, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(template.Spec.ImagePullSecrets, []corev1.LocalObjectReference{{Name: fixture.Input.ImagePullSecret.Name}}) {
				t.Fatal("rendered workload lost the approved pull binding")
			}
			if slices.Contains(bundle.Identity().AuthorizedSecrets, fixture.Input.ImagePullSecret.Name) {
				t.Fatal("pull binding expanded container credential authority")
			}
			selector := &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: fixture.Input.ImagePullSecret.Name},
				Key:                  corev1.DockerConfigJsonKey,
			}
			for name, mutate := range map[string]func(*corev1.PodTemplateSpec){
				"missing pull": func(p *corev1.PodTemplateSpec) { p.Spec.ImagePullSecrets = nil },
				"foreign pull": func(p *corev1.PodTemplateSpec) {
					p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "foreign-secret"}}
				},
				"duplicate pull": func(p *corev1.PodTemplateSpec) {
					p.Spec.ImagePullSecrets = append(p.Spec.ImagePullSecrets, p.Spec.ImagePullSecrets[0])
				},
				"environment": func(p *corev1.PodTemplateSpec) {
					p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{
						Name: "EXAMPLE_REGISTRY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: selector},
					})
				},
				"initializer environment": func(p *corev1.PodTemplateSpec) {
					p.Spec.InitContainers[0].Env = append(p.Spec.InitContainers[0].Env, corev1.EnvVar{
						Name: "EXAMPLE_REGISTRY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: selector},
					})
				},
				"environment source": func(p *corev1.PodTemplateSpec) {
					p.Spec.Containers[0].EnvFrom = append(p.Spec.Containers[0].EnvFrom, corev1.EnvFromSource{
						SecretRef: &corev1.SecretEnvSource{LocalObjectReference: selector.LocalObjectReference},
					})
				},
				"volume": func(p *corev1.PodTemplateSpec) {
					p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "private-pull",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: selector.Name}},
					})
				},
			} {
				t.Run(name, func(t *testing.T) {
					changed := template.DeepCopy()
					mutate(changed)
					before := changed.DeepCopy()
					if err := bundle.ValidateReconciledTemplate(changed, prepared.Agent.Image, false, prepared.Agent.Port); err == nil {
						t.Fatal("pull credential escaped its approved kubelet-only use")
					}
					if !equality.Semantic.DeepEqual(changed, before) {
						t.Fatal("validation mutated the Pod template")
					}
				})
			}
			for name, mutate := range map[string]func(*w.Input){
				"missing UID":       func(i *w.Input) { i.ImagePullSecret.UID = "" },
				"invalid name":      func(i *w.Input) { i.ImagePullSecret.Name = "invalid/name" },
				"retained lifetime": func(i *w.Input) { i.Reconciled = false },
				"runtime alias":     func(i *w.Input) { i.ImagePullSecret.Name = "runtime-secret" },
			} {
				t.Run(name, func(t *testing.T) {
					input := bundle.Identity()
					mutate(&input)
					if _, err := w.Build(input); err == nil {
						t.Fatal("unapproved pull identity accepted")
					}
				})
			}
		})
	}
}

func TestReconciledHermesProbeCommands(t *testing.T) {
	fixture := gatewayHeldFixture("hermes")
	fixture.Input.Reconciled = true
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput("hermes", "openai", false)
	input["secrets"].(map[string]any)["existingSecret"] = "runtime-secret"
	prepared, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	legacy := template.DeepCopy()
	legacy.Spec.Containers[0].StartupProbe.Exec.Command = []string{"/usr/local/bin/hermes-python", "/etc/cetirun/hermes-runtime.py", "liveness", "--timeout", "5"}
	if err := bundle.ValidateReconciledTemplate(legacy, prepared.Agent.Image, false, prepared.Agent.Port); err != nil {
		t.Fatal("an existing Agent's pinned chart lost its supported startup contract:", err)
	}
	for name, mutate := range map[string]func(*corev1.Container){
		"missing startup": func(c *corev1.Container) { c.StartupProbe = nil },
		"unmanaged startup": func(c *corev1.Container) {
			c.StartupProbe.Exec.Command = []string{"node", "-e", "process.exit(0)"}
		},
		"lightweight readiness": func(c *corev1.Container) {
			c.ReadinessProbe.Exec.Command = []string{"node", "/etc/cetirun/hermes-health-probe.cjs", "--timeout", "10"}
		},
		"host HTTP startup": func(c *corev1.Container) {
			c.StartupProbe.Exec = nil
			c.StartupProbe.HTTPGet = &corev1.HTTPGetAction{Path: "/health"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := template.DeepCopy()
			mutate(&changed.Spec.Containers[0])
			if err := bundle.ValidateReconciledTemplate(changed, prepared.Agent.Image, false, prepared.Agent.Port); err == nil {
				t.Fatal("Hermes accepted a probe outside its managed health contract")
			}
		})
	}
}

func TestReconciledSandboxProxyCannotConsumeAgentPullCredentials(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	input := bundle.Identity()
	input.ImagePullSecret = &w.ObjectIdentity{Name: "registry-pull-exampleuid", UID: "pull-secret-uid"}
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	values["agentRequest"].(map[string]any)["pullSecretName"] = input.ImagePullSecret.Name
	prepared, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := prepared.ReconcileExpectation()
	if err != nil {
		t.Fatal(err)
	}
	for _, workload := range expected.Workloads {
		if workload.Kind != "Deployment" {
			continue
		}
		proxy := &appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Name: workload.Identity.Name},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(workload.Replicas), Template: workload.Template,
				Selector: &metav1.LabelSelector{MatchLabels: workload.Template.Labels},
			},
		}
		proxy.Spec.Template.Spec.Containers[0].Env = append(proxy.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
			Name: "EXAMPLE_REGISTRY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: input.ImagePullSecret.Name}, Key: corev1.DockerConfigJsonKey,
			}},
		})
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(proxy)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.validateReconciledObject(unstructured.Unstructured{Object: object}); err == nil ||
			!strings.Contains(err.Error(), "cannot receive Agent credentials") {
			t.Fatal("Copilot proxy consumed the Agent's pull credentials:", err)
		}
		return
	}
	t.Fatal("missing Copilot proxy workload")
}

func TestReconciledSandboxReusesAnApprovedChartComponent(t *testing.T) {
	for _, governed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "sandbox"}[governed], func(t *testing.T) {
			bundle, values := reconciledSandboxInputs(t)
			helperResources := bundle.Identity().Resolution.Sandbox.Resources.Helpers
			chrt := testChart(t)
			chrt.Templates = append(chrt.Templates, &chart.File{
				Name: "templates/example-observer.yaml",
				Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: example-observer\ndata:\n  mode: observe\n"),
			})
			values["sidecars"] = []corev1.Container{{
				Name: "example-observer", Image: "registry.example.com/observer:example",
				Command: []string{"sleep", "3600"}, SecurityContext: bundle.SecurityContext(),
				Resources:    corev1.ResourceRequirements{Requests: helperResources.Requests, Limits: helperResources.Limits},
				VolumeMounts: []corev1.VolumeMount{{Name: "observer-config", MountPath: "/etc/observer", ReadOnly: true}},
			}}
			values["extraVolumes"] = []corev1.Volume{{Name: "observer-config", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "example-observer"}},
			}}}
			raw, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &values); err != nil {
				t.Fatal(err)
			}
			var template corev1.PodTemplateSpec
			if governed {
				prepared, err := PrepareSandboxChart(chrt, values, nil, bundle)
				if err != nil {
					t.Fatal(err)
				}
				current, err := prepared.SandboxTemplate()
				if err != nil {
					t.Fatal(err)
				}
				template = *current
				expected, err := prepared.ReconcileExpectation()
				if err != nil || !slices.ContainsFunc(expected.Controls, func(control unstructured.Unstructured) bool {
					return control.GetKind() == "ConfigMap" && control.GetName() == "example-observer"
				}) {
					t.Fatal("the common component's configuration was not included in current admission state")
				}
				accounting := w.AccountResources(template.Spec, bundle.Overhead())
				if template.Spec.Resources == nil || !equality.Semantic.DeepEqual(*template.Spec.Resources, accounting.Pod) {
					t.Fatal("a new helper escaped aggregate Pod resource accounting")
				}
			} else {
				prepared, err := PrepareChart(chrt, values)
				if err != nil {
					t.Fatal(err)
				}
				template = registryRenderedTemplate(t, prepared)
			}
			if !slices.ContainsFunc(template.Spec.Containers, func(container corev1.Container) bool {
				return container.Name == "example-observer" && container.Image == "registry.example.com/observer:example"
			}) {
				t.Fatal("the once-defined chart component was dropped")
			}
		})
	}
}

func TestReconciledSandboxProxyPreparationIsIndependent(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	values["replicaCount"] = 0
	values["aiGateway"].(map[string]any)["copilotProxy"].(map[string]any)["replicaCount"] = 1
	prepared, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := prepared.ReconcileExpectation()
	if err != nil {
		t.Fatal(err)
	}
	if len(expected.Workloads) != 2 {
		t.Fatal("missing independently prepared Copilot proxy")
	}
	for _, workload := range expected.Workloads {
		want := int32(0)
		if workload.Kind == "Deployment" {
			want = 1
		}
		if workload.Replicas != want {
			t.Fatal("proxy authentication still depends on Agent replicas")
		}
	}
}

func TestReconciledSandboxExtraScratchLimit(t *testing.T) {
	for _, tc := range []struct {
		name, size string
		rejected   bool
	}{
		{name: "missing", rejected: true},
		{name: "zero", size: "0", rejected: true},
		{name: "negative", size: "-1", rejected: true},
		{name: "one byte over", size: "1073741825", rejected: true},
		{name: "below limit", size: "1073741823"},
		{name: "exact limit", size: "1Gi"},
		{name: "equivalent units", size: "1024Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, values := reconciledSandboxInputs(t)
			temporary := map[string]any{}
			if tc.size != "" {
				temporary["sizeLimit"] = tc.size
			}
			values["extraVolumes"] = []any{map[string]any{"name": "extension-scratch", "emptyDir": temporary}}
			_, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
			if (err != nil) != tc.rejected {
				t.Fatalf("temporary volume size %q: error=%v, want rejection=%t", tc.size, err, tc.rejected)
			}
		})
	}
}

func TestReconciledSandboxAuxiliaryRoleConstraints(t *testing.T) {
	bundle, _ := reconciledSandboxInputs(t)
	for name, change := range map[string]func(*corev1.PodSpec){
		"baseline": nil,
		"unbounded scratch": func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		},
		"oversized scratch": func(p *corev1.PodSpec) {
			size := bundle.Identity().Resolution.Sandbox.Storage.Scratch.SizeLimit.DeepCopy()
			size.Add(size)
			p.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size},
			}}}
		},
		"key environment": func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "model-key"}, Key: "api-key"},
			}}}
		},
		"key envFrom": func(p *corev1.PodSpec) {
			p.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "runtime-secret"}}}}
		},
		"key volume": func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "model-key"}}}}
		},
		"token projection": func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "identity", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"},
				}}},
			}}}
		},
		"HOME": func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "home", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: bundle.Identity().Home.Name},
			}}}
		},
		"capabilities": func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			deployment := &appsv1.Deployment{
				TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{Name: "approved-component"},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "approved-component"}},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						AutomountServiceAccountToken: ptr.To(false), SecurityContext: bundle.PodSecurityContext(),
						Containers: []corev1.Container{{Name: "helper", Image: "registry.example.com/helper:example",
							SecurityContext: bundle.SecurityContext()}},
					}},
				},
			}
			if change != nil {
				change(&deployment.Spec.Template.Spec)
			}
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
			if err != nil {
				t.Fatal(err)
			}
			prepared := &PreparedRelease{sandboxBundle: bundle, sandboxReconcile: &w.ReconcileExpectation{}}
			err = prepared.validateReconciledObject(unstructured.Unstructured{Object: object})
			if (err != nil) != (change != nil) {
				t.Fatalf("auxiliary constraint %s: %v", name, err)
			}
		})
	}
}

func TestReconciledSandboxApplyRequiresCurrentAuthority(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	base, err := PrepareChart(testChart(t), values)
	if err != nil {
		t.Fatal(err)
	}

	stale := errors.New("synthetic authority changed")
	prepared, err := base.ReconcileSandbox(values, bundle, func(context.Context) error { return stale })
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Client{}).InstallOrUpgrade(bundle.Identity().Namespace.Name, bundle.Identity().ReleaseName, prepared); !errors.Is(err, stale) {
		t.Fatalf("common apply did not recheck authority before Kubernetes access: %v", err)
	}
}

func TestReconciledSandboxHistoryFailureDoesNotSubmit(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	base, err := PrepareChart(testChart(t), values)
	if err != nil {
		t.Fatal(err)
	}
	submitted, checked, rendered := false, false, false
	prepared, err := base.ReconcileSandboxWithPreflight(values, bundle,
		func(context.Context) error { checked = true; return nil },
		func(context.Context) error { rendered = true; return nil },
		func(context.Context) error { submitted = true; return nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, kube, store := sandboxActionConfig()
	injected := errors.New("synthetic history read outage")
	store.queryError = injected
	err = (&Client{}).installOrUpgrade(bundle.Identity().Namespace.Name, bundle.Identity().ReleaseName, prepared,
		func(string) (*action.Configuration, error) { return cfg, nil })
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "helm history") {
		t.Fatal("fixture did not reach the history read:", err)
	}
	if !checked || submitted || len(kube.writes) != 0 || store.writes != 0 {
		t.Fatalf("read-only history failure changed submission state: checked=%t submitted=%t workloadWrites=%d releaseWrites=%d",
			checked, submitted, len(kube.writes), store.writes)
	}
	store.queryError = nil
	kube.afterWrite = func() {
		if !rendered {
			t.Fatal("Helm wrote a resource before manifest authorization")
		}
	}
	if err := (&Client{}).installOrUpgrade(bundle.Identity().Namespace.Name, bundle.Identity().ReleaseName, prepared,
		func(string) (*action.Configuration, error) { return cfg, nil }); err != nil {
		t.Fatal("restored history did not recover the same prepared release:", err)
	}
	if !submitted || len(kube.writes) == 0 {
		t.Fatal("recovered install did not pass the submission boundary")
	}
}

func TestReconciledSandboxPreflightRejectsStaleAuthority(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	base, err := PrepareChart(testChart(t), values)
	if err != nil {
		t.Fatal(err)
	}
	stale := errors.New("synthetic stale preflight authority")
	submitted, configured := false, false
	prepared, err := base.ReconcileSandboxWithPreflight(values, bundle,
		func(context.Context) error { return stale },
		func(context.Context) error { return nil },
		func(context.Context) error { submitted = true; return nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = (&Client{}).installOrUpgrade(bundle.Identity().Namespace.Name, bundle.Identity().ReleaseName, prepared,
		func(string) (*action.Configuration, error) {
			configured = true
			return nil, errors.New("unexpected configuration read")
		})
	if !errors.Is(err, stale) || submitted || configured {
		t.Fatal("preflight authority failure reached Helm or submission:", err)
	}
}

func TestReconciledSandboxConfigurationFailureDoesNotSubmit(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	base, err := PrepareChart(testChart(t), values)
	if err != nil {
		t.Fatal(err)
	}
	checked, submitted := false, false
	prepared, err := base.ReconcileSandboxWithPreflight(values, bundle,
		func(context.Context) error { checked = true; return nil },
		func(context.Context) error { return nil },
		func(context.Context) error { submitted = true; return nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("synthetic Helm configuration read outage")
	err = (&Client{}).installOrUpgrade(bundle.Identity().Namespace.Name, bundle.Identity().ReleaseName, prepared,
		func(string) (*action.Configuration, error) { return nil, injected })
	if !errors.Is(err, injected) || !checked || submitted {
		t.Fatal("read-only configuration failure submitted a workload:", err)
	}
}

func TestReconciledSandboxApplyGuardFailureDoesNotWrite(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	base, err := PrepareChart(testChart(t), values)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("synthetic final apply authority read failure")
	checked, submitted := false, false
	prepared, err := base.ReconcileSandboxWithPreflight(values, bundle,
		func(context.Context) error { checked = true; return nil },
		func(context.Context) error { submitted = true; return injected },
		func(context.Context) error { return nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, kube, store := sandboxActionConfig()
	err = (&Client{}).installOrUpgrade(bundle.Identity().Namespace.Name, bundle.Identity().ReleaseName, prepared,
		func(string) (*action.Configuration, error) { return cfg, nil })
	if !errors.Is(err, injected) || !checked || !submitted {
		t.Fatal("fixture did not reach the guarded Helm renderer:", err)
	}
	if len(kube.writes) != 0 || store.writes != 0 {
		t.Fatalf("failed final guard allowed Helm writes: workloadWrites=%d releaseWrites=%d", len(kube.writes), store.writes)
	}
}
