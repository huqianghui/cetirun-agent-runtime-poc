// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func containerRuntimeClassForTest() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "example-container"},
		Handler:    "runc",
	}
}

func updateContainerPlatformForTest(t *testing.T, r *AgentReconciler, kube client.Client, change func(*SandboxPlatformConfiguration)) {
	t.Helper()
	ctx := context.Background()
	object := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, object); err != nil {
		t.Fatal(err)
	}
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(object.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
		t.Fatal(err)
	}
	change(&document)
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	object.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := kube.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
}

func withContainerRuntime(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		ctx := context.Background()
		var boxes api.SandboxList
		if err := kube.List(ctx, &boxes); err != nil || len(boxes.Items) != 1 {
			t.Fatal("container fixture requires one Sandbox:", err)
		}
		box := &boxes.Items[0]
		box.Spec.Type = api.SandboxContainer
		if err := kube.Update(ctx, box); err != nil {
			t.Fatal(err)
		}
		runtimeClass := containerRuntimeClassForTest()
		if err := kube.Create(ctx, runtimeClass); err != nil {
			t.Fatal(err)
		}
		updateContainerPlatformForTest(t, r, kube, func(document *SandboxPlatformConfiguration) {
			registration := document.Registrations[api.SandboxKata]
			registration.Capabilities.RuntimeClassName = runtimeClass.Name
			registration.Capabilities.PerContainerResourceLimits = true
			registration.Capabilities.RequirePerContainerResourceLimits = true
			document.Registrations = map[api.SandboxType]sandbox.Registration{api.SandboxContainer: registration}
			document.Platforms[api.SandboxContainer] = document.Platforms[api.SandboxKata]
			delete(document.Platforms, api.SandboxKata)
			document.RuntimeClasses = map[string]SandboxRuntimePrerequisite{
				runtimeClass.Name: {UID: runtimeClass.UID, Handler: runtimeClass.Handler},
			}
		})
	}
}

func assertContainerWorkload(t *testing.T, f *nativeSandboxFixture, mode corev1.PersistentVolumeAccessMode) {
	t.Helper()
	expected := reconciledExpectationForTest(t, f)
	box := &api.Sandbox{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.current(t).Spec.SandboxRef}, box); err != nil {
		t.Fatal(err)
	}
	if box.Spec.Type != api.SandboxContainer || sandboxMetricRuntime(f.current(t), box) != "container" {
		t.Fatal("container execution or metrics mislabeled the selected backend")
	}
	if _, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t)); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	} else if err == nil {
		t.Fatal("container execution created a held/single-start receipt")
	}
	for _, workload := range expected.Workloads {
		spec := workload.Template.Spec
		if len(workload.Overhead) != 0 || len(spec.Overhead) != 0 || ptr.Deref(spec.AutomountServiceAccountToken, true) {
			t.Fatal("container workload inherited VM overhead or ambient Kubernetes credentials")
		}
		if workload.Kind != "StatefulSet" {
			continue
		}
		if ptr.Deref(spec.RuntimeClassName, "") != "example-container" {
			t.Fatal("Agent Pod did not select the explicitly registered ordinary runtime")
		}
		accounting := w.AccountResources(spec, nil)
		if spec.Resources == nil || !equality.Semantic.DeepEqual(*spec.Resources, accounting.Pod) {
			t.Fatal("container Pod resources do not match ordinary effective Kubernetes accounting")
		}
		agentResources := corev1.ResourceRequirements{Requests: box.Spec.Resources.Agent.Requests, Limits: box.Spec.Resources.Agent.Limits}
		helperResources := corev1.ResourceRequirements{Requests: box.Spec.Resources.Helpers.Requests, Limits: box.Spec.Resources.Helpers.Limits}
		if !equality.Semantic.DeepEqual(spec.Containers[0].Resources, agentResources) {
			t.Fatal("container Agent lost its requests and per-container limits")
		}
		for _, container := range append(spec.Containers[1:], spec.InitContainers...) {
			if !equality.Semantic.DeepEqual(container.Resources, helperResources) {
				t.Fatalf("helper %s lost its independent resource limits", container.Name)
			}
		}
		for _, volume := range spec.Volumes {
			if volume.EmptyDir != nil && volume.EmptyDir.Medium == "" &&
				(volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Cmp(box.Spec.Storage.Scratch.SizeLimit) != 0) {
				t.Fatalf("scratch volume %s is not bounded by the Sandbox", volume.Name)
			}
		}
	}
	assertNativeHomeAccessMode(t, f, mode)
	_ = reconciledNetworkPolicyForTest(t, f)
}

func TestContainerSandboxCommonRenderingAndAdmission(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
			t.Run(kind+"/"+string(mode), func(t *testing.T) {
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true,
					withContainerRuntime(t), withNativeHomeAccessMode(t, mode), withReconciledCoordination(t))
				assertContainerWorkload(t, f, mode)
				handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{
					StateNamespace:            f.r.OperatorNamespace,
					CoordinationConfigMapName: f.r.SandboxNativeCoordination.Identity.Name,
					CoordinationConfigMapUID:  string(f.r.SandboxNativeCoordination.Identity.UID),
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, workload := range reconciledExpectationForTest(t, f).Workloads {
					pod := reconciledPodForTest(t, f, workload)
					if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); !response.Allowed {
						t.Fatal("current container Pod was denied:", response.Result)
					}
					for name, mutate := range map[string]func(*corev1.Pod){
						"default runtime":   func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil },
						"Kata substitution": func(p *corev1.Pod) { p.Spec.RuntimeClassName = ptr.To("example-kata") },
						"resources":         func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Limits = nil },
						"privileged":        func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) },
					} {
						if name == "default runtime" && workload.Kind != "StatefulSet" {
							continue
						}
						t.Run(workload.Kind+"/"+name, func(t *testing.T) {
							changed := pod.DeepCopy()
							mutate(changed)
							if response := handler.Validate(context.Background(), reconciledPodRequest(t, changed)); response.Allowed {
								t.Fatal("container Pod bypassed common final admission")
							}
						})
					}
				}
			})
		}
	}
}

func TestContainerSandboxUnavailableNeverStarts(t *testing.T) {
	for _, fault := range []string{"registration", "unavailable", "class pin", "handler pin", "UID pin", "container limits", "qualified profile", "missing class"} {
		t.Run(fault, func(t *testing.T) {
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false,
				withContainerRuntime(t), func(r *AgentReconciler, kube client.Client) {
					updateContainerPlatformForTest(t, r, kube, func(document *SandboxPlatformConfiguration) {
						registration := document.Registrations[api.SandboxContainer]
						pin := document.RuntimeClasses["example-container"]
						switch fault {
						case "registration":
							delete(document.Registrations, api.SandboxContainer)
							delete(document.Platforms, api.SandboxContainer)
							return
						case "unavailable":
							registration.Available = false
						case "class pin":
							delete(document.RuntimeClasses, "example-container")
						case "handler pin":
							pin.Handler = "unverified-handler"
							document.RuntimeClasses["example-container"] = pin
						case "UID pin":
							pin.UID = "unverified-runtime"
							document.RuntimeClasses["example-container"] = pin
						case "container limits":
							registration.Capabilities.PerContainerResourceLimits = false
						case "qualified profile":
							registration.Capabilities.Profile = ""
						case "missing class":
							if err := kube.Delete(context.Background(), containerRuntimeClassForTest()); err != nil {
								t.Fatal(err)
							}
						}
						document.Registrations[api.SandboxContainer] = registration
					})
				})
			_, _ = f.reconcile()
			if f.installer.stages != 0 || f.current(t).Status.Phase == api.PhaseReady {
				t.Fatal("an unverified container registration started a workload")
			}
			var workloads appsv1.StatefulSetList
			if err := f.kube.List(context.Background(), &workloads); err != nil || len(workloads.Items) != 0 {
				t.Fatal("unavailable runtime produced a StatefulSet:", err)
			}
		})
	}
}

func TestContainerSandboxRuntimeDriftRevokesAdmissionAndExecution(t *testing.T) {
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, false, withContainerRuntime(t))
	expected := reconciledExpectationForTest(t, f)
	pod := reconciledPodForTest(t, f, expected.Workloads[0])
	handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{StateNamespace: f.r.OperatorNamespace})
	if err != nil {
		t.Fatal(err)
	}
	if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); !response.Allowed {
		t.Fatal(response.Result)
	}
	runtimeClass := containerRuntimeClassForTest()
	if err := f.kube.Get(context.Background(), client.ObjectKeyFromObject(runtimeClass), runtimeClass); err != nil {
		t.Fatal(err)
	}
	runtimeClass.Handler = "unapproved-handler"
	if err := f.kube.Update(context.Background(), runtimeClass); err != nil {
		t.Fatal(err)
	}
	if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); response.Allowed {
		t.Fatal("handler drift retained Pod admission")
	}
	_, _ = f.reconcile()
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || !f.current(t).Status.AIGateway.Blocked {
		t.Fatal("handler drift did not stop common execution and Gateway access")
	}
}

func TestContainerSandboxRetainedRestart(t *testing.T) {
	testReconciledSandboxRetainedRestart(t, withContainerRuntime(t))
}

func TestContainerSandboxSuspendResume(t *testing.T) {
	testReconciledSandboxSuspension(t, withContainerRuntime(t))
}

func TestContainerSandboxHealthySourceMigration(t *testing.T) {
	testPlannedMigrationRetainsHOME(t, withContainerRuntime(t))
}

func TestContainerSandboxIngressMTLS(t *testing.T) {
	testReconciledSandboxIngressMTLS(t, api.SandboxContainer)
}

func TestContainerSandboxIngressMTLSRequiresApproval(t *testing.T) {
	for _, fault := range []string{"endpoint registration", "Agent binding", "server UID", "client version", "Gateway UID"} {
		t.Run(fault, func(t *testing.T) {
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false,
				withContainerRuntime(t), withIngressMTLS(t, "openclaw"), func(r *AgentReconciler, kube client.Client) {
					updateContainerPlatformForTest(t, r, kube, func(document *SandboxPlatformConfiguration) {
						platform := document.Platforms[api.SandboxContainer]
						switch fault {
						case "endpoint registration":
							platform.IngressMTLS = nil
						case "Agent binding":
							platform.IngressMTLS.Bindings = platform.IngressMTLS.Bindings[:0]
						case "server UID":
							platform.IngressMTLS.Bindings[0].ServerSecret.UID = "unapproved-server"
						case "client version":
							platform.IngressMTLS.Bindings[0].ClientSecret.ResourceVersion = "unapproved-version"
						case "Gateway UID":
							platform.IngressMTLS.Bindings[0].Gateway.UID = "unapproved-gateway"
						}
						document.Platforms[api.SandboxContainer] = platform
					})
				})
			for range 3 {
				_, _ = f.reconcile()
			}
			if f.installer.stages != 0 {
				t.Fatal("container mTLS bypassed missing or changed endpoint approvals")
			}
			f.assertNotReady(t)
			var workloads appsv1.StatefulSetList
			if err := f.kube.List(context.Background(), &workloads); err != nil || len(workloads.Items) != 0 {
				t.Fatal("unapproved mTLS created container compute:", err)
			}
		})
	}
}
