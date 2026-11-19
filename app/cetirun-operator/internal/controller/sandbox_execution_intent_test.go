// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func executionIntentFixture(t *testing.T, typ string, configure ...func(*preparationFixture)) (*preparationFixture, sandboxcoordination.EnrolledTarget) {
	t.Helper()
	f, node, _ := placementPreparationFixture(t, typ)
	for _, configure := range configure {
		configure(f)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	preparedRevision(t, f)
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	expected := f.expectation(t)
	identity := sandbox.ExecutionIdentity{
		Binding: expected.Binding, Revision: expected.Revision,
		PodUID: "independently-enrolled-pod", NodeUID: node.UID, EnforcementEpoch: "synthetic-enrolled-epoch",
	}
	return f, sandboxcoordination.EnrolledTarget{
		ExecutionIdentity: identity,
		Writer: sandboxwriter.Writer{Workload: expected.Workload,
			Pod:  w.ObjectIdentity{Name: expected.Workload.Name + "-0", UID: identity.PodUID},
			Node: expected.Placement.Nodes[0], MachineID: "synthetic-machine-incarnation", Epoch: identity.EnforcementEpoch,
			Storage: sandboxwriter.StorageIdentity{Volume: w.ObjectIdentity{Name: "example-pv", UID: "pv-uid"},
				Driver: "csi.example.com", Handle: "synthetic-volume-incarnation"},
		},
		WriterJournal: w.ObjectIdentity{
			Name: sandboxwriter.JournalName(sandboxwriter.Lifetime{Agent: expected.Agent, Namespace: expected.Namespace, Home: expected.Home}),
			UID:  "synthetic-enrolled-journal-uid",
		},
	}
}

func TestSandboxExecutionIntentPinsHeldNativePreparation(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f, target := executionIntentFixture(t, typ)
			before, stages := f.current(t), f.installer.stages
			expected := f.expectation(t)
			intent, err := f.reconciler.SandboxExecutionIntent(context.Background(), before, target)
			if err != nil {
				t.Fatal(err)
			}
			if intent.Expected != target.ExecutionIdentity || intent.Writer != target.Writer || intent.WriterJournal != target.WriterJournal ||
				intent.Agent.UID != before.UID || intent.AgentGeneration != before.Generation ||
				intent.Namespace != expected.Namespace || intent.Home != expected.Home || intent.Workload != expected.Workload ||
				!reflect.DeepEqual(intent.Placement, expected.Placement) || len(intent.Sources) != len(expected.Sources)+1 ||
				!slices.Contains(intent.Sources, intent.Preparation) {
				t.Fatalf("intent did not preserve the prepared source and identity tuple: %+v", intent)
			}
			after := f.current(t)
			if !reflect.DeepEqual(before, after) || f.installer.stages != stages || !f.expectation(t).ExecutionHeld {
				t.Fatal("reserving an intent changed activation, readiness or preparation")
			}
			if err := f.reconciler.CheckSandboxExecutionSources(context.Background(), intent); err != nil {
				t.Fatal("current pinned sources were not verifiable:", err)
			}
			state, err := sandboxcoordination.NewState("cetirun-system", "execution-coordination")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.client.Create(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			store := sandboxcoordination.Store{Client: f.client, APIReader: f.client, Namespace: state.Namespace,
				Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID}}
			ticket, err := store.AcquireExecution(context.Background(), intent)
			if err != nil || ticket.Execution.Phase != sandboxcoordination.Reserved || ticket.Execution.Intent.Expected != target.ExecutionIdentity {
				t.Fatal("actual prepared intent could not enter serialized coordination:", err)
			}
			assertSandboxPrepared(t, f)
		})
	}
}

func TestSandboxExecutionAcceptsMaximumPreparedSources(t *testing.T) {
	ctx := context.Background()
	f, target := executionIntentFixture(t, "hermes")
	expected := fillPreparedSourceInventory(t, f)
	target.Binding, target.Revision = expected.Binding, expected.Revision
	intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
	if err != nil || len(intent.Sources) != sandboxcoordination.MaxPreparedExecutionSources {
		t.Fatalf("valid maximum preparation failed execution capture: sources=%d err=%v", len(intent.Sources), err)
	}
	for len(intent.Sources) <= sandboxcoordination.MaxSources {
		source := w.SourceReference{
			APIVersion: "v1", Kind: "ConfigMap", Namespace: "cetirun-system",
			Name: fmt.Sprintf("extra-source-%d", len(intent.Sources)), UID: types.UID(fmt.Sprintf("extra-uid-%d", len(intent.Sources))),
			ResourceVersion: "1",
		}
		intent.Sources = append(intent.Sources, source)
		intent.SourceDigests[w.SourceKey(source)] = strings.Repeat("d", 64)
	}
	if err := intent.Validate(); err == nil {
		t.Fatal("execution inventory accepted more than its combined source budget")
	}
}

func fillPreparedSourceInventory(t *testing.T, f *preparationFixture) w.Expectation {
	t.Helper()
	ctx := context.Background()
	additional := w.MaxPreparationSources - len(f.expectation(t).Sources)
	if additional <= 0 {
		t.Fatal("fixture must leave room for approved role inputs")
	}
	platform := f.config.Platforms[api.SandboxKata]
	for i := range additional {
		role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("input-role-%03d", i)}}
		if err := f.client.Create(ctx, role); err != nil {
			t.Fatal(err)
		}
		platform.Grants = append(platform.Grants, sandboxpolicy.ApprovedGrant{
			PolicyUID: f.fixture.Policy.UID,
			RoleRef:   rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			RoleUID:   role.UID, RoleResourceVersion: role.ResourceVersion, Revision: "synthetic-approval",
			Rules: []rbacv1.PolicyRule{},
		})
	}
	f.config.Platforms[api.SandboxKata] = platform
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	expected := f.expectation(t)
	if len(expected.Sources) != w.MaxPreparationSources || len(f.current(t).Spec.Helm.Values) != 0 {
		t.Fatal("fixture did not produce exactly 128 prepared inputs")
	}
	return expected
}

func TestSandboxExecutionReservesOnlySelectedConfiguration(t *testing.T) {
	for _, policyName := range []string{"selected"} {
		t.Run(policyName, func(t *testing.T) {
			ctx := context.Background()
			var unselectedPolicy *api.AgentPolicy
			var unusedSandbox *api.Sandbox
			f, target := executionIntentFixture(t, "hermes", func(f *preparationFixture) {
				normal := &api.AgentPolicy{}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Policy), normal); err != nil {
					t.Fatal(err)
				}
				normal.Spec.SandboxSelection.AllowMultiple = true
				if err := f.client.Update(ctx, normal); err != nil {
					t.Fatal(err)
				}
				admin := normal.DeepCopy()
				admin.ObjectMeta = metav1.ObjectMeta{Name: "policy-admin"}
				unusedSandbox = f.fixture.Sandbox.DeepCopy()
				unusedSandbox.ObjectMeta = metav1.ObjectMeta{Name: "sandbox-unused"}
				for _, object := range []client.Object{admin, unusedSandbox} {
					if err := f.client.Create(ctx, object); err != nil {
						t.Fatal(err)
					}
				}
				team := &api.Team{}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), team); err != nil {
					t.Fatal(err)
				}
				team.Spec.SandboxRefs = append(team.Spec.SandboxRefs, unusedSandbox.Name)
				if err := f.client.Update(ctx, team); err != nil {
					t.Fatal(err)
				}
				agent := f.current(t)
				agent.Generation++
				authorization, err := sandboxbinding.Snapshot(agent, team, sandbox.Resources{
					Sandboxes:     map[string]*api.Sandbox{f.fixture.Sandbox.Name: f.fixture.Sandbox, unusedSandbox.Name: unusedSandbox},
					AgentPolicies: map[string]*api.AgentPolicy{normal.Name: normal, admin.Name: admin},
				})
				if err != nil {
					t.Fatal(err)
				}
				agent.Annotations[sandboxbinding.AuthorizationAnnotation] = authorization
				if err := f.client.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				f.source.inputs.AgentGeneration = agent.Generation
				unselectedPolicy = admin
			})
			expected := f.expectation(t)
			intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
			if err != nil {
				t.Fatal(err)
			}
			state, err := sandboxcoordination.NewState("cetirun-system", "execution-coordination")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.client.Create(ctx, state); err != nil {
				t.Fatal(err)
			}
			store := sandboxcoordination.Store{Client: f.client, APIReader: f.client, Namespace: state.Namespace,
				Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID}}
			ticket, err := store.AcquireExecution(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err = store.BeginStart(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err = store.MarkRunning(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			for _, object := range []client.Object{unselectedPolicy, unusedSandbox} {
				index := slices.IndexFunc(expected.Sources, func(source w.SourceReference) bool {
					return source.UID == object.GetUID()
				})
				if index < 0 {
					if object != unselectedPolicy {
						t.Fatal("preparation did not validate every Team Sandbox")
					}
				}
				before := w.SourceReference{
					APIVersion: api.GroupVersion.String(), Kind: "AgentPolicy", Name: object.GetName(),
					UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(),
				}
				if index >= 0 {
					if object == unselectedPolicy {
						t.Fatal("an unrelated policy became a Team preparation dependency")
					}
					before = expected.Sources[index]
				}
				if slices.Contains(intent.Sources, before) {
					t.Fatalf("execution reserved unselected %s %s", before.Kind, before.Name)
				}
				if _, err := store.AcquireChange(ctx, before, strings.Repeat("b", 64)); err != nil {
					t.Fatal("unselected configuration change required stopping this execution:", err)
				}
			}
			unselectedPolicy.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
			unusedSandbox.Spec.Storage.Scratch.SizeLimit = resource.MustParse("2Gi")
			for _, object := range []client.Object{unselectedPolicy, unusedSandbox} {
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
				t.Fatal("valid unselected configuration changes invalidated execution:", err)
			}
			for _, source := range intent.Sources {
				if source.Kind == "AgentPolicy" || source.Kind == "Sandbox" || source.Kind == "Team" {
					if _, err := store.AcquireChange(ctx, source, strings.Repeat("c", 64)); !errors.Is(err, sandboxcoordination.ErrConflict) {
						t.Fatal("selected configuration was not reserved:", err)
					}
				}
			}
			current, err := store.ReadExecution(ctx, intent.Agent.UID)
			if err != nil || !reflect.DeepEqual(current, ticket) {
				t.Fatal("unrelated changes modified the running execution ticket:", err)
			}
		})
	}
}

func TestSandboxExecutionIntentRejectsUnpreparedOrMismatchedTarget(t *testing.T) {
	for _, scenario := range []string{"missing ControllerRevision", "wrong Pod", "wrong Node", "wrong epoch", "wrong policy", "wrong revision", "missing placement"} {
		t.Run(scenario, func(t *testing.T) {
			f, target := executionIntentFixture(t, "hermes")
			switch scenario {
			case "missing ControllerRevision":
				expected := f.expectation(t)
				workload := &appsv1.StatefulSet{}
				if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, workload); err != nil {
					t.Fatal(err)
				}
				workload.Status.UpdateRevision = ""
				if err := f.client.Status().Update(context.Background(), workload); err != nil {
					t.Fatal(err)
				}
			case "wrong Pod":
				target.PodUID = ""
			case "wrong Node":
				target.NodeUID = "foreign-node"
			case "wrong epoch":
				target.EnforcementEpoch = ""
			case "wrong policy":
				target.Binding.AgentPolicyUID = "foreign-policy"
			case "wrong revision":
				target.Revision = "foreign-revision"
			case "missing placement":
				f.reconciler.SandboxPreparation.Placement = nil
			}
			if intent, err := f.reconciler.SandboxExecutionIntent(context.Background(), f.current(t), target); err == nil ||
				!reflect.DeepEqual(intent, sandboxcoordination.Intent{}) {
				t.Fatalf("unprepared or unenrolled execution was accepted: %+v %v", intent, err)
			}
			if !f.expectation(t).ExecutionHeld {
				t.Fatal("rejection lost the activation hold")
			}
		})
	}
}

func TestSandboxExecutionSourcesRecheckSharedChangesWithoutRestaging(t *testing.T) {
	for _, scenario := range []string{"policy", "Team", "Secret metadata", "HOME", "Agent intent", "placement", "platform", "preparation"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f, target := executionIntentFixture(t, "openclaw")
			intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
			if err != nil {
				t.Fatal(err)
			}
			stages := f.installer.stages
			if scenario == "platform" {
				platform := f.config.Platforms[api.SandboxKata]
				platform.Topology.Revision = "changed-topology"
				f.config.Platforms[api.SandboxKata] = platform
			} else {
				var key client.ObjectKey
				var object client.Object
				switch scenario {
				case "policy":
					object, key = &api.AgentPolicy{}, client.ObjectKey{Name: f.fixture.Policy.Name}
				case "Team":
					object, key = &api.Team{}, client.ObjectKey{Name: f.fixture.Team.Name}
				case "Secret metadata":
					object, key = &corev1.Secret{}, client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: f.fixture.Agent.Spec.GatewayTokenSecretRef}
				case "HOME":
					object, key = &corev1.PersistentVolumeClaim{}, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Home.Name}
				case "Agent intent":
					object, key = &api.Agent{}, client.ObjectKey{Name: intent.Agent.Name}
				case "placement":
					object, key = &corev1.ConfigMap{}, client.ObjectKey{Namespace: "cetirun-system", Name: intent.Placement.Registry.Name}
				case "preparation":
					object, key = &corev1.ConfigMap{}, client.ObjectKey{Namespace: intent.Preparation.Namespace, Name: intent.Preparation.Name}
				}
				if err := f.client.Get(ctx, key, object); err != nil {
					t.Fatal(err)
				}
				switch changed := object.(type) {
				case *api.Agent:
					agent := changed
					agent.Spec.Suspended = true
					agent.Generation++
				case *api.Team:
					changed.Spec.MemberContext.Role = "changed-role"
				case *api.AgentPolicy:
					changed.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
				case *corev1.PersistentVolumeClaim:
					changed.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("20Gi")
				case *corev1.ConfigMap:
					changed.Data["changed-input"] = "synthetic"
				default:
					object.SetLabels(map[string]string{"example.com/source-revision": "changed"})
				}
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err == nil {
				t.Fatal("stale execution sources remained current")
			}
			if f.installer.stages != stages {
				t.Fatal("source check restaged a workload instead of rejecting stale intent")
			}
		})
	}
}

func TestSandboxExecutionEffectiveInputsIgnoreObservationMetadata(t *testing.T) {
	for _, kubernetesSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("kubernetes-prerequisites=%t", kubernetesSource), func(t *testing.T) {
			ctx := context.Background()
			f, target := executionIntentFixture(t, "hermes", func(f *preparationFixture) {
				if !kubernetesSource {
					return
				}
				secret := &corev1.Secret{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: f.fixture.Namespace.Name,
					Name: f.fixture.Agent.Spec.GatewayTokenSecretRef}, secret); err != nil {
					t.Fatal(err)
				}
				secret.OwnerReferences = []metav1.OwnerReference{w.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}
				if err := f.client.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
				f.reconciler.SandboxPreparation.Prerequisites = &controller.KubernetesSandboxPrerequisites{
					APIReader: f.client, HomeClaimName: f.fixture.Input.Home.Name, SkillsClaimName: f.fixture.Input.Skills.Name,
					ChartVersion: f.source.inputs.ChartVersion, Helpers: f.source.inputs.Helpers, Now: func() time.Time { return f.now },
					WriterExclusion: &testSandboxWriterExclusion{},
				}
			})
			intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
			if err != nil {
				t.Fatal(err)
			}
			before := intent
			renders, stages := f.installer.renders, f.installer.stages
			objects := []client.Object{
				f.fixture.Team.DeepCopy(), f.fixture.Sandbox.DeepCopy(), f.fixture.Policy.DeepCopy(),
				f.fixture.Namespace.DeepCopy(), f.fixture.Input.Home.DeepCopy(), f.fixture.Input.Skills.DeepCopy(),
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: intent.Preparation.Name, Namespace: intent.Preparation.Namespace}},
			}
			for _, object := range objects {
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatal(err)
				}
				oldRV := object.GetResourceVersion()
				labels, annotations := object.GetLabels(), object.GetAnnotations()
				if labels == nil {
					labels = map[string]string{}
				}
				if annotations == nil {
					annotations = map[string]string{}
				}
				labels["example.com/unrelated"], annotations["example.com/unrelated"] = "changed", "changed"
				object.SetLabels(labels)
				object.SetAnnotations(annotations)
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
				if object.GetResourceVersion() == oldRV {
					t.Fatal("fixture did not change resource version")
				}
				if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
					t.Fatalf("unrelated %T metadata invalidated effective execution inputs: %v", object, err)
				}
			}
			if !reflect.DeepEqual(before, intent) || f.installer.renders != renders || f.installer.stages != stages {
				t.Fatal("observation-only changes rewrote or restaged execution")
			}
			for _, object := range objects {
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatal(err)
				}
				original := object.GetOwnerReferences()
				object.SetOwnerReferences([]metav1.OwnerReference{w.AgentOwner("foreign", "foreign-uid")})
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
				if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err == nil {
					t.Fatalf("foreign %T ownership was accepted", object)
				}
				object.SetOwnerReferences(original)
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSandboxExecutionTeamMutationPreservesUnaffectedSelection(t *testing.T) {
	ctx := context.Background()
	var spare *api.Sandbox
	var otherPolicy *api.AgentPolicy
	f, target := executionIntentFixture(t, "hermes", func(f *preparationFixture) {
		policy := &api.AgentPolicy{}
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Policy), policy); err != nil {
			t.Fatal(err)
		}
		policy.Spec.SandboxSelection.AllowMultiple = true
		if err := f.client.Update(ctx, policy); err != nil {
			t.Fatal(err)
		}
		spare = f.fixture.Sandbox.DeepCopy()
		spare.ObjectMeta = metav1.ObjectMeta{Name: "spare-sandbox"}
		otherPolicy = policy.DeepCopy()
		otherPolicy.ObjectMeta = metav1.ObjectMeta{Name: "other-policy"}
		for _, object := range []client.Object{spare, otherPolicy} {
			if err := f.client.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
		team := &api.Team{}
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), team); err != nil {
			t.Fatal(err)
		}
		team.Spec.SandboxRefs = append(team.Spec.SandboxRefs, spare.Name)
		if err := f.client.Update(ctx, team); err != nil {
			t.Fatal(err)
		}
	})
	intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
	if err != nil {
		t.Fatal(err)
	}
	state, err := sandboxcoordination.NewState("cetirun-system", "team-coordination")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.Create(ctx, state); err != nil {
		t.Fatal(err)
	}
	store := sandboxcoordination.Store{Client: f.client, APIReader: f.client, Namespace: state.Namespace,
		Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID}}
	execution, err := store.AcquireExecution(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	for range []string{"default"} {
		before := &api.Team{}
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), before); err != nil {
			t.Fatal(err)
		}
		after := before.DeepCopy()
		after.Spec.DefaultSandboxRef = spare.Name
		change, err := store.AcquireTeamChange(ctx, before, after)
		if err != nil {
			t.Fatal("unrelated Team change required stopping normal execution:", err)
		}
		change, err = store.BeginChange(ctx, change)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.client.Update(ctx, after); err != nil {
			t.Fatal(err)
		}
		if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
			t.Fatal("unchanged selected Team binding was invalidated:", err)
		}
		if err := store.ReleaseChange(ctx, change); err != nil {
			t.Fatal(err)
		}
		if err := store.Check(ctx, execution); err != nil {
			t.Fatal("unrelated Team update replaced execution authority:", err)
		}
	}
	before := &api.Team{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.AgentPolicyRef = otherPolicy.Name
	if _, err := store.AcquireTeamChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("selected policy rebind was not excluded:", err)
	}
}

func TestSandboxExecutionSourcesPermitWorkloadStatusAndControlledScale(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			ctx := context.Background()
			f, target := executionIntentFixture(t, typ)
			intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
			if err != nil {
				t.Fatal(err)
			}
			stages, fences := f.installer.stages, f.source.fences
			workload := &appsv1.StatefulSet{}
			key := client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}
			for phase, replicas := range []*int32{ptr.To[int32](0), ptr.To[int32](1), nil} {
				if err := f.client.Get(ctx, key, workload); err != nil {
					t.Fatal(err)
				}
				if phase > 0 {
					workload.Spec.Replicas = replicas
					workload.Generation++
					if err := f.client.Update(ctx, workload); err != nil {
						t.Fatal(err)
					}
				}
				for observation := range 3 {
					if err := f.client.Get(ctx, key, workload); err != nil {
						t.Fatal(err)
					}
					previousRV := workload.ResourceVersion
					workload.Status.Replicas = int32(observation)
					workload.Status.ReadyReplicas = int32(observation)
					workload.Status.ObservedGeneration = workload.Generation
					workload.Status.CurrentRevision = fmt.Sprintf("synthetic-status-%d", observation)
					if err := f.client.Status().Update(ctx, workload); err != nil {
						t.Fatal(err)
					}
					if workload.ResourceVersion == previousRV {
						t.Fatal("fixture did not change the workload observation version")
					}
					if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
						t.Fatal("status or controlled zero/one scale invalidated workload inputs:", err)
					}
				}
			}
			if f.installer.stages != stages || f.source.fences != fences {
				t.Fatal("running-input checks restaged or attempted to quiesce the writer")
			}
		})
	}
}

func TestSandboxExecutionSourcesRejectWorkloadInputOrIdentityDrift(t *testing.T) {
	for name, mutate := range map[string]func(*appsv1.StatefulSet){
		"image": func(w *appsv1.StatefulSet) {
			w.Spec.Template.Spec.Containers[0].Image = "registry.example.com/changed:1"
		},
		"service":           func(w *appsv1.StatefulSet) { w.Spec.ServiceName = "changed-service" },
		"selector":          func(w *appsv1.StatefulSet) { w.Spec.Selector.MatchLabels["example.com/changed"] = "true" },
		"Pod annotations":   func(w *appsv1.StatefulSet) { w.Spec.Template.Annotations["example.com/changed"] = "true" },
		"owner":             func(w *appsv1.StatefulSet) { w.OwnerReferences[0].UID = "different-agent" },
		"revision":          func(w *appsv1.StatefulSet) { w.Annotations["cetirun.microsoft.com/sandbox-revision"] = "changed" },
		"labels":            func(w *appsv1.StatefulSet) { w.Labels["cetirun.microsoft.com/changed"] = "true" },
		"multiple replicas": func(w *appsv1.StatefulSet) { w.Spec.Replicas = ptr.To[int32](2) },
		"negative replicas": func(w *appsv1.StatefulSet) { w.Spec.Replicas = ptr.To[int32](-1) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, target := executionIntentFixture(t, "hermes")
			intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
			if err != nil {
				t.Fatal(err)
			}
			workload := &appsv1.StatefulSet{}
			if err := f.client.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}, workload); err != nil {
				t.Fatal(err)
			}
			mutate(workload)
			if err := f.client.Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err == nil {
				t.Fatal("changed workload input was accepted")
			}
		})
	}
	t.Run("same-name replacement", func(t *testing.T) {
		ctx := context.Background()
		f, target := executionIntentFixture(t, "hermes")
		intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
		if err != nil {
			t.Fatal(err)
		}
		workload := &appsv1.StatefulSet{}
		key := client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}
		if err := f.client.Get(ctx, key, workload); err != nil {
			t.Fatal(err)
		}
		if err := f.client.Delete(ctx, workload); err != nil {
			t.Fatal(err)
		}
		workload.UID, workload.ResourceVersion = "", ""
		if err := f.client.Create(ctx, workload); err != nil {
			t.Fatal(err)
		}
		if workload.UID == intent.Workload.UID {
			t.Fatal("fixture did not replace workload identity")
		}
		if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err == nil {
			t.Fatal("same-name workload replacement was accepted")
		}
	})
}

func TestSandboxExecutionSourcesRecheckPrerequisiteApproval(t *testing.T) {
	for name, mutate := range map[string]func(*preparationFixture){
		"unavailable source": func(f *preparationFixture) { f.source.resolveErr = errors.New("synthetic approval unavailable") },
		"missing source":     func(f *preparationFixture) { f.reconciler.SandboxPreparation.Prerequisites = nil },
		"withdrawn approval": func(f *preparationFixture) { f.source.inputs.Revision = "withdrawn-approval" },
		"Agent UID":          func(f *preparationFixture) { f.source.inputs.AgentUID = "other-agent" },
		"generation":         func(f *preparationFixture) { f.source.inputs.AgentGeneration++ },
		"namespace":          func(f *preparationFixture) { f.source.inputs.NamespaceUID = "other-namespace" },
		"HOME":               func(f *preparationFixture) { f.source.inputs.Home.UID = "other-home" },
		"skills":             func(f *preparationFixture) { f.source.inputs.Skills.UID = "other-skills" },
		"runtime approval":   func(f *preparationFixture) { f.source.inputs.RuntimeSecret.UID = "other-secret-identity" },
		"chart":              func(f *preparationFixture) { f.source.inputs.ChartVersion = "99.0.0" },
		"helpers":            func(f *preparationFixture) { f.source.inputs.Helpers.Logger = !f.source.inputs.Helpers.Logger },
		"publication": func(f *preparationFixture) {
			f.source.inputs.PublicationSources = []w.SourceReference{{
				APIVersion: "v1", Kind: "ConfigMap", Namespace: "cetirun-system",
				Name: "changed-publication", UID: "publication-uid", ResourceVersion: "2",
			}}
		},
		"placement": func(f *preparationFixture) {
			f.source.inputs.Placement = f.expectation(t).Placement.DeepCopy()
			f.source.inputs.Placement.Workload.UID = "other-workload"
		},
		"stale":  func(f *preparationFixture) { f.source.inputs.ObservedAt = f.now.Add(-time.Minute) },
		"future": func(f *preparationFixture) { f.source.inputs.ObservedAt = f.now.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, target := executionIntentFixture(t, "hermes")
			intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
			if err != nil {
				t.Fatal(err)
			}
			renders, stages, reads, fences := f.installer.renders, f.installer.stages, f.installer.digestReads, f.source.fences
			mutate(f)
			if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err == nil {
				t.Fatal("changed or unavailable whole-generation prerequisite approval was ignored")
			}
			if f.installer.renders != renders || f.installer.stages != stages || f.installer.digestReads != reads ||
				f.source.fences != fences {
				t.Fatal("reauthorization attempted Helm work or quiescence")
			}
		})
	}
}

func TestSandboxExecutionPrerequisiteRefreshDoesNotRewriteOrQuiesce(t *testing.T) {
	ctx := context.Background()
	f, target := executionIntentFixture(t, "openclaw")
	intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _, _ := preparationPlacementReceipt(t, f)
	resolves, fences, stages, reads := f.source.resolves, f.source.fences, f.installer.stages, f.installer.digestReads
	f.source.fenceErr = errors.New("synthetic writer is still running")
	for range 3 {
		f.now = f.now.Add(time.Second)
		f.source.inputs.ObservedAt = f.now
		if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
			t.Fatal("unchanged prerequisite approval with a fresh observation was rejected:", err)
		}
	}
	if f.source.resolves != resolves+3 || f.source.fences != fences || f.installer.stages != stages || f.installer.digestReads != reads {
		t.Fatal("execution recheck must refresh only approval, without Helm or running-writer quiescence")
	}
	current, _, _ := preparationPlacementReceipt(t, f)
	if !reflect.DeepEqual(current, receipt) {
		t.Fatal("current approval refresh rewrote its pinned historical receipt")
	}
}

func TestSandboxExecutionSourcesRequirePinnedPrerequisiteReceipt(t *testing.T) {
	f, target := executionIntentFixture(t, "hermes")
	intent, err := f.reconciler.SandboxExecutionIntent(context.Background(), f.current(t), target)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _, _ := preparationPlacementReceipt(t, f)
	intent.Sources = slices.DeleteFunc(intent.Sources, func(source w.SourceReference) bool {
		return source.APIVersion == "v1" && source.Kind == "ConfigMap" &&
			source.Namespace == receipt.Namespace && source.Name == receipt.Name
	})
	if err := f.reconciler.CheckSandboxExecutionSources(context.Background(), intent); err == nil {
		t.Fatal("an unpinned prerequisite receipt authorized execution")
	}
}

func TestSandboxExecutionSourcesPermitStatusOnlyAgentUpdates(t *testing.T) {
	ctx := context.Background()
	f, target := executionIntentFixture(t, "hermes")
	intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
	if err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	agent.Status.Conditions = append(agent.Status.Conditions, metav1.Condition{
		Type: "SyntheticStatus", Status: metav1.ConditionTrue, Reason: "StatusOnly",
		Message: "Synthetic non-authoritative test status", LastTransitionTime: metav1.Now(),
	})
	if err := f.client.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := f.reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
		t.Fatal("status churn invalidated authorization-relevant Agent intent:", err)
	}
}
