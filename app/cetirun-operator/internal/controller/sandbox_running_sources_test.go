// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (s *preparationSource) ResolveRunning(ctx context.Context, agent *api.Agent, team *api.Team) (controller.SandboxPrerequisites, error) {
	return s.Resolve(ctx, agent, team)
}

// Simulates the Kubernetes outputs of a trusted runtime handoff. No admission
// hold is bypassed in a cluster, and no guest-external evidence is produced.
func runningExecutionFixture(t *testing.T, typ string, configure ...func(*preparationFixture)) (*preparationFixture, sandboxcoordination.EnrolledTarget, sandboxcoordination.Intent) {
	t.Helper()
	ctx := context.Background()
	f, target := executionIntentFixture(t, typ, configure...)
	intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
	if err != nil {
		t.Fatal(err)
	}
	completeRunningFixture(t, f, target, intent)
	return f, target, intent
}

func completeRunningFixture(t *testing.T, f *preparationFixture, target sandboxcoordination.EnrolledTarget, intent sandboxcoordination.Intent) {
	t.Helper()
	ctx := context.Background()
	agent := f.current(t)
	controllerutil.RemoveFinalizer(agent, sandbox.HoldFinalizer)
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	workload := &appsv1.StatefulSet{}
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}, workload); err != nil {
		t.Fatal(err)
	}
	workload.Spec.Replicas = ptr.To(int32(1))
	if err := f.client.Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	expected := f.expectation(t)
	expected.ExecutionHeld = false
	currentAgent, err := w.AgentSourceReference(agent)
	if err != nil {
		t.Fatal(err)
	}
	for i := range expected.Sources {
		source := &expected.Sources[i]
		if source.IsAgent() {
			*source = currentAgent
		}
		if source.Kind == "StatefulSet" {
			source.ResourceVersion = workload.ResourceVersion
		}
	}
	writeRunningExpectation(t, f, intent, expected)
	pod := &corev1.Pod{
		ObjectMeta: *expected.Template.ObjectMeta.DeepCopy(),
		Spec:       *expected.Template.Spec.DeepCopy(),
	}
	pod.Name, pod.Namespace, pod.UID = target.Writer.Pod.Name, intent.Namespace.Name, target.PodUID
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
	pod.Labels["statefulset.kubernetes.io/pod-name"] = pod.Name
	pod.Labels["apps.kubernetes.io/pod-index"] = "0"
	pod.Labels["controller-revision-hash"] = expected.ControllerRevision
	pod.Spec.NodeName = target.Writer.Node.Name
	pod.Spec.Hostname, pod.Spec.Subdomain = pod.Name, workload.Name
	pod.Spec.Overhead = expected.Overhead.DeepCopy()
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func writeRunningExpectation(t *testing.T, f *preparationFixture, intent sandboxcoordination.Intent, expected w.Expectation) {
	t.Helper()
	state := &corev1.ConfigMap{}
	if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: intent.Preparation.Namespace, Name: intent.Preparation.Name}, state); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	state.Data[w.PodExpectationKey] = string(raw)
	if err := f.client.Update(context.Background(), state); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxRunningSourcesAreDistinctFromHeldPreparation(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			ctx := context.Background()
			held, target := executionIntentFixture(t, typ)
			heldIntent, err := held.reconciler.SandboxExecutionIntent(ctx, held.current(t), target)
			if err != nil || heldIntent.RunningPreparationDigest == "" {
				t.Fatal("held preparation did not capture running invariants:", err)
			}
			if _, err := held.reconciler.SandboxRunningStatusInput(ctx, heldIntent); err == nil {
				t.Fatal("held state was accepted as running")
			}
			f, target, intent := runningExecutionFixture(t, typ)
			before, stages := f.current(t), f.installer.stages
			input, err := f.reconciler.SandboxRunningStatusInput(ctx, intent)
			if err != nil {
				t.Fatal("complete simulated handoff failed source verification:", err)
			}
			if input.Expected != target.ExecutionIdentity || input.AgentGeneration != before.Generation ||
				input.Plan == nil || input.Resolution == nil || input.Plan.Binding != intent.Expected.Binding ||
				input.Plan.Revision != intent.Expected.Revision || input.Observation != nil || !input.Now.IsZero() || input.MaxAge != 0 {
				t.Fatal("running inputs invented evidence or differed from the enrolled binding")
			}
			if !reflect.DeepEqual(before, f.current(t)) || stages != f.installer.stages {
				t.Fatal("running verification changed status or restaged the workload")
			}
			if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err == nil {
				t.Fatal("the held-only check silently accepted unheld execution")
			}
			if _, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target); err == nil {
				t.Fatal("running state was recaptured through held preparation")
			}
		})
	}
}

func TestSandboxRunningSourcesRejectIncompleteOrChangedHandoff(t *testing.T) {
	for _, scenario := range []string{
		"Agent held", "expectation held", "missing hold", "null hold", "template changed",
		"Agent reference held", "source UID", "zero replicas", "two replicas", "Pod UID", "Pod Node",
		"Pod template", "Agent spec", "suspended", "Secret version", "missing running pin",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f, _, intent := runningExecutionFixture(t, "openclaw")
			expected := f.expectation(t)
			switch scenario {
			case "Agent held", "Agent spec", "suspended":
				agent := f.current(t)
				if scenario == "Agent held" {
					controllerutil.AddFinalizer(agent, sandbox.HoldFinalizer)
				} else if scenario == "suspended" {
					agent.Spec.Suspended = true
				} else {
					agent.Spec.Model = "different-model"
				}
				if err := f.client.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
			case "expectation held", "template changed", "Agent reference held", "source UID":
				switch scenario {
				case "expectation held":
					expected.ExecutionHeld = true
				case "template changed":
					expected.Template.Spec.Containers[0].Image = "registry.example.com/changed:latest"
				case "Agent reference held":
					for i := range expected.Sources {
						if expected.Sources[i].IsAgent() {
							for _, original := range intent.Sources {
								if original.IsAgent() {
									expected.Sources[i] = original
								}
							}
						}
					}
				case "source UID":
					for i := range expected.Sources {
						if expected.Sources[i].Kind == "PersistentVolumeClaim" {
							expected.Sources[i].UID = "replaced-claim"
							break
						}
					}
				}
				writeRunningExpectation(t, f, intent, expected)
			case "missing hold", "null hold":
				state := &corev1.ConfigMap{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Preparation.Namespace, Name: intent.Preparation.Name}, state); err != nil {
					t.Fatal(err)
				}
				replacement := ""
				if scenario == "null hold" {
					replacement = `"executionHeld":null,`
				}
				state.Data[w.PodExpectationKey] = strings.Replace(state.Data[w.PodExpectationKey], `"executionHeld":false,`, replacement, 1)
				if err := f.client.Update(ctx, state); err != nil {
					t.Fatal(err)
				}
			case "zero replicas", "two replicas":
				workload := &appsv1.StatefulSet{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}, workload); err != nil {
					t.Fatal(err)
				}
				replicas := int32(0)
				if scenario == "two replicas" {
					replicas = 2
				}
				workload.Spec.Replicas = &replicas
				if err := f.client.Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
			case "Pod UID", "Pod Node", "Pod template":
				pod := &corev1.Pod{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Writer.Pod.Name}, pod); err != nil {
					t.Fatal(err)
				}
				if scenario == "Pod UID" {
					if err := f.client.Delete(ctx, pod); err != nil {
						t.Fatal(err)
					}
					pod.UID, pod.ResourceVersion = "replacement-pod", ""
					if err := f.client.Create(ctx, pod); err != nil {
						t.Fatal(err)
					}
				} else {
					if scenario == "Pod Node" {
						pod.Spec.NodeName = "another-node"
					} else {
						pod.Spec.Containers[0].Image = "registry.example.com/changed:latest"
					}
					if err := f.client.Update(ctx, pod); err != nil {
						t.Fatal(err)
					}
				}
			case "Secret version":
				secret := &corev1.Secret{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: f.current(t).Spec.GatewayTokenSecretRef}, secret); err != nil {
					t.Fatal(err)
				}
				secret.Annotations = map[string]string{"example.com/change": "changed"}
				if err := f.client.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "missing running pin":
				intent.RunningPreparationDigest = ""
			}
			before := f.current(t)
			if _, err := f.reconciler.SandboxRunningStatusInput(ctx, intent); err == nil {
				t.Fatal("changed or incomplete handoff was accepted")
			}
			if !reflect.DeepEqual(before, f.current(t)) {
				t.Fatal("failed source verification modified the Agent")
			}
		})
	}
}

func TestSandboxRunningSourcesAllowCurrentObservationMetadata(t *testing.T) {
	ctx := context.Background()
	f, _, intent := runningExecutionFixture(t, "hermes")
	workload := &appsv1.StatefulSet{}
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}, workload); err != nil {
		t.Fatal(err)
	}

	workload.Status.ReadyReplicas = 1
	if err := f.client.Status().Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	policy := &api.AgentPolicy{}
	if err := f.client.Get(ctx, client.ObjectKey{Name: f.fixture.Policy.Name}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Annotations = map[string]string{"example.com/description": "metadata-only"}
	if err := f.client.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	expected := f.expectation(t)
	for i := range expected.Sources {
		switch expected.Sources[i].Kind {
		case "StatefulSet":
			expected.Sources[i].ResourceVersion = workload.ResourceVersion
		case "AgentPolicy":
			expected.Sources[i].ResourceVersion = policy.ResourceVersion
		}
	}
	writeRunningExpectation(t, f, intent, expected)
	if _, err := f.reconciler.SandboxRunningStatusInput(ctx, intent); err != nil {
		t.Fatal("equivalent current source versions invalidated running inputs:", err)
	}
}

func TestSandboxRunningSourcesVerifyScheduledTopology(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(fmt.Sprintf("matching=%v", matching), func(t *testing.T) {
			ctx := context.Background()
			f, target, intent := runningExecutionFixture(t, "openclaw")
			node := &corev1.Node{}
			if err := f.client.Get(ctx, client.ObjectKey{Name: target.Writer.Node.Name}, node); err != nil {
				t.Fatal(err)
			}

			node.Labels = map[string]string{corev1.LabelTopologyRegion: "region-one", corev1.LabelTopologyZone: "zone-one"}
			if err := f.client.Update(ctx, node); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{}
			if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: target.Writer.Pod.Name}, pod); err != nil {
				t.Fatal(err)
			}
			pod.Labels[corev1.LabelTopologyRegion], pod.Labels[corev1.LabelTopologyZone] = "region-one", "zone-one"
			if !matching {
				pod.Labels[corev1.LabelTopologyZone] = "forged-zone"
			}
			if err := f.client.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			_, err := f.reconciler.SandboxRunningStatusInput(ctx, intent)
			if (err == nil) != matching {
				t.Fatalf("topology matching=%v: %v", matching, err)
			}
		})
	}
}

func TestSandboxRunningSourcesUseKubernetesPrerequisiteReader(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f, _, intent := runningExecutionFixture(t, typ, func(f *preparationFixture) {
				installKubernetesPrerequisites(t, f, &testSandboxWriterExclusion{})
			})
			if _, err := f.reconciler.SandboxRunningStatusInput(context.Background(), intent); err != nil {
				t.Fatal("concrete prerequisite reader rejected a stable running handoff:", err)
			}
			team := &api.Team{}
			if err := f.client.Get(context.Background(), client.ObjectKey{Name: f.fixture.Team.Name}, team); err != nil {
				t.Fatal(err)
			}
			if _, err := f.reconciler.SandboxPreparation.Prerequisites.Resolve(context.Background(), f.current(t), team); err == nil {
				t.Fatal("held prerequisite reader accepted running state")
			}
		})
	}
}

func TestSandboxRunningSourcesRecheckExpectationAgentDuringReadback(t *testing.T) {
	f, _, intent := runningExecutionFixture(t, "openclaw")
	changed := false
	f.reconciler.APIReader = &prerequisitesAPIReader{
		Reader: f.client, reads: map[client.ObjectKey]int{},
		afterGet: func(_ client.ObjectKey, object client.Object) error {
			if _, pod := object.(*corev1.Pod); pod && !changed {
				changed = true
				expected := f.expectation(t)
				for i := range expected.Sources {
					if expected.Sources[i].IsAgent() {
						for _, source := range intent.Sources {
							if source.IsAgent() {
								expected.Sources[i] = source
							}
						}
					}
				}
				writeRunningExpectation(t, f, intent, expected)
			}
			return nil
		},
	}
	if _, err := f.reconciler.SandboxRunningStatusInput(context.Background(), intent); err == nil || !changed {
		t.Fatal("late stale expectation Agent reference escaped final revalidation:", err)
	}
}

func TestSandboxRunningSourcesRecheckPodDuringReadback(t *testing.T) {
	for _, scenario := range []string{"deleted", "replaced", "template changed", "status only"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f, _, intent := runningExecutionFixture(t, "hermes")
			readPod, changed := false, false
			f.reconciler.APIReader = &prerequisitesAPIReader{
				Reader: f.client, reads: map[client.ObjectKey]int{},
				afterGet: func(_ client.ObjectKey, object client.Object) error {
					if _, pod := object.(*corev1.Pod); pod {
						readPod = true
					}
					if _, node := object.(*corev1.Node); !node || !readPod || changed {
						return nil
					}
					changed = true
					pod := &corev1.Pod{}
					if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Writer.Pod.Name}, pod); err != nil {
						return err
					}
					switch scenario {
					case "deleted", "replaced":
						if err := f.client.Delete(ctx, pod); err != nil {
							return err
						}
						if scenario == "replaced" {
							pod.UID, pod.ResourceVersion = "replacement-after-read", ""
							return f.client.Create(ctx, pod)
						}
					case "template changed":
						pod.Spec.Containers[0].Image = "registry.example.com/replaced:latest"
						return f.client.Update(ctx, pod)
					case "status only":
						pod.Status.Phase = corev1.PodPending
						return f.client.Status().Update(ctx, pod)
					}
					return nil
				},
			}
			_, err := f.reconciler.SandboxRunningStatusInput(ctx, intent)
			if !changed || (err == nil) != (scenario == "status only") {
				t.Fatalf("readback scenario %s: changed=%v err=%v", scenario, changed, err)
			}
		})
	}
}

func TestSandboxRunningSourcesExpireDuringPodReads(t *testing.T) {
	f, _, intent := runningExecutionFixture(t, "openclaw")
	advanced := false
	f.reconciler.APIReader = &prerequisitesAPIReader{
		Reader: f.client, reads: map[client.ObjectKey]int{},
		afterGet: func(_ client.ObjectKey, object client.Object) error {
			if _, pod := object.(*corev1.Pod); pod && !advanced {
				advanced = true
				f.now = f.now.Add(10 * time.Minute)
			}
			return nil
		},
	}
	if _, err := f.reconciler.SandboxRunningStatusInput(context.Background(), intent); err == nil || !advanced {
		t.Fatal("expired source snapshots survived final Pod reads:", err)
	}
}
