// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Only authority and Agent status are real API objects. The captured source and
// runtime identities remain synthetic; this does not qualify any running adapter.
func TestSandboxExecutionStatusWriterAPIServer(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("status CAS tests refuse an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if info, err := os.Stat(filepath.Join(assets, name)); assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("explicit preinstalled local API-server and etcd binaries are required")
		}
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 30 * time.Second,
	}
	config, err := environment.Start()
	if err != nil {
		if stopErr := environment.Stop(); stopErr != nil {
			t.Log("stop incomplete local API-server startup:", stopErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := storage.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-system"}}); err != nil {
		t.Fatal(err)
	}
	read := func(t *testing.T, name string) *api.Agent {
		t.Helper()
		agent := &api.Agent{}
		if err := storage.Get(ctx, client.ObjectKey{Name: name}, agent); err != nil {
			t.Fatal(err)
		}
		return agent
	}

	t.Run("clear invalidates delayed positive status and then becomes a no-op", func(t *testing.T) {
		writer, ticket, before := executionStatusAPIFixture(t, ctx, storage, "status-delayed-positive")
		counts := &preparationRequestCounts{Client: storage}
		writer.Reconciler.Client = counts
		positive := before.DeepCopy()
		expected := ticket.Execution.Intent.Expected
		_, err := controller.UpdateSandboxAgentStatus(positive, controller.SandboxStatusInput{
			AgentGeneration: before.Generation,
			Resolution:      &sandbox.Resolution{Binding: expected.Binding, Revision: "synthetic-foundation"},
			Plan: &sandboxpolicy.Plan{
				AgentName: before.Name, Namespace: ticket.Execution.Intent.Namespace.Name, Binding: expected.Binding,
				ResolutionRevision: "synthetic-foundation", Revision: expected.Revision,
				Network: sandboxpolicy.NetworkPlan{IngressMode: api.AgentPolicyIngressDisabled},
			},
			Expected: expected, Observation: &sandbox.Observation{
				ExecutionIdentity: expected, ObservedAt: writer.Now(), RuntimeVerified: true, PodVerified: true, ControlsVerified: true,
			},
			Now: writer.Now(), MaxAge: writer.MaxAge,
		})
		if err != nil {
			t.Fatal(err)
		}
		pending, err := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}).Data(positive)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
			t.Fatal(err)
		}
		cleared := read(t, before.Name)
		if cleared.ResourceVersion == before.ResourceVersion || counts.writes != 1 ||
			!reflect.DeepEqual(cleared.Status.Sandbox, before.Status.Sandbox) ||
			!apimeta.IsStatusConditionFalse(cleared.Status.Conditions, api.ConditionSandboxReady) ||
			!apimeta.IsStatusConditionFalse(cleared.Status.Conditions, api.ConditionReady) || cleared.Status.AccessURL != "" {
			t.Fatal("real API clear did not force the first status CAS on an already nonready Agent")
		}
		if err := storage.Status().Patch(ctx, positive, client.RawPatch(types.MergePatchType, pending)); !apierrors.IsConflict(err) {
			t.Fatal("real API accepted a delayed positive status computed before clear:", err)
		}
		if err := writer.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
			t.Fatal(err)
		}
		if counts.writes != 1 || !reflect.DeepEqual(cleared, read(t, before.Name)) {
			t.Fatal("a repeated clear wrote status or the delayed positive CAS changed it")
		}
	})

	t.Run("clear follows the current suspended and deleting generation without sources", func(t *testing.T) {
		writer, ticket, agent := executionStatusAPIFixture(t, ctx, storage, "status-generation")
		agent.Spec.Suspended = true
		if err := storage.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if agent.Generation <= ticket.Execution.Intent.AgentGeneration {
			t.Fatal("the API did not advance generation for suspension")
		}
		if err := writer.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
			t.Fatal("real cleanup required old generation or unavailable policy/source objects:", err)
		}
		current := read(t, agent.Name)
		if current.Status.Phase != api.PhaseSuspended || current.Status.ObservedGeneration != current.Generation {
			t.Fatal("cleanup did not reflect the current suspended generation")
		}
		if err := storage.Delete(ctx, current); err != nil {
			t.Fatal(err)
		}
		if err := writer.ClearReadiness(ctx, ticket, statusTestGuard); err != nil {
			t.Fatal(err)
		}
		current = read(t, agent.Name)
		if current.DeletionTimestamp.IsZero() || current.Status.Phase != api.PhaseDeleting ||
			apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionSandboxReady).Reason != "Deleting" ||
			!reflect.DeepEqual(agent.Status.Sandbox, current.Status.Sandbox) {
			t.Fatal("deleting Agent cleanup lost applied history or claimed a suspended-only state")
		}
	})

	t.Run("a concurrent metadata update conflicts without rebasing", func(t *testing.T) {
		writer, ticket, before := executionStatusAPIFixture(t, ctx, storage, "status-metadata-conflict")
		writes := 0
		writer.Reconciler.Client = interceptor.NewClient(storage, interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, _ string, object client.Object, patch client.Patch,
				opts ...client.SubResourcePatchOption,
			) error {
				writes++
				current := read(t, before.Name)
				current.Labels = map[string]string{"example.com/status-race": "synthetic"}
				if err := c.Update(ctx, current); err != nil {
					return err
				}
				return c.Status().Patch(ctx, object, patch, opts...)
			},
		})
		if err := writer.ClearReadiness(ctx, ticket, statusTestGuard); !apierrors.IsConflict(err) {
			t.Fatal("real metadata RV race was rebased or swallowed:", err)
		}
		if writes != 1 || !reflect.DeepEqual(before.Status, read(t, before.Name).Status) {
			t.Fatal("conflicting status write retried or changed history/readiness")
		}
	})

	t.Run("same-name replacement is not modified", func(t *testing.T) {
		writer, ticket, original := executionStatusAPIFixture(t, ctx, storage, "status-replacement")
		original.Finalizers = nil
		if err := storage.Update(ctx, original); err != nil {
			t.Fatal(err)
		}
		if err := storage.Delete(ctx, original); err != nil {
			t.Fatal(err)
		}
		replacement := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: original.Name}, Spec: *original.Spec.DeepCopy()}
		if err := storage.Create(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		if replacement.UID == original.UID {
			t.Fatal("API fixture did not create a new Agent lifetime")
		}
		if err := writer.ClearReadiness(ctx, ticket, statusTestGuard); err == nil {
			t.Fatal("old stopping ticket accepted a same-name replacement")
		}
		if !reflect.DeepEqual(replacement, read(t, replacement.Name)) {
			t.Fatal("old stopping operation modified the replacement")
		}
	})
}

func executionStatusAPIFixture(t *testing.T, ctx context.Context, storage client.Client,
	name string,
) (controller.SandboxExecutionStatusWriter, sandboxcoordination.Ticket, *api.Agent) {
	t.Helper()
	f, _, intent := runningExecutionFixture(t, "hermes")
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{"example.com/retain-status-test"}},
		Spec: *f.current(t).Spec.DeepCopy()}
	if err := storage.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent.Status.Sandbox = &api.SandboxStatus{
		TeamUID: "historical-team", SandboxUID: "historical-sandbox", AgentPolicyUID: "historical-policy",
		AppliedRevision: "historical-revision",
	}
	agent.Status.Phase = api.PhasePending
	agent.Status.ObservedGeneration = agent.Generation
	for _, kind := range []string{api.ConditionReady, api.ConditionSandboxReady} {
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionFalse, Reason: "HistoricalHold", Message: "Synthetic prior hold.",
			ObservedGeneration: agent.Generation, LastTransitionTime: metav1.NewTime(f.now),
		})
	}
	if err := storage.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	// Only rebind the synthetic stopping inventory to the API-issued Agent UID.
	// There is deliberately no claim that these source objects authorize a start.
	intent.Agent = w.ObjectIdentity{Name: agent.Name, UID: agent.UID}
	intent.AgentGeneration = agent.Generation
	intent.Expected.Binding.AgentUID = agent.UID
	intent.Placement.Agent = intent.Agent
	intent.WriterJournal.Name = sandboxwriter.JournalName(sandboxwriter.Lifetime{
		Agent: intent.Agent, Namespace: intent.Namespace, Home: intent.Home,
	})
	for i := range intent.Sources {
		source := &intent.Sources[i]
		if source.IsAgent() {
			source.Name, source.UID, source.Generation = agent.Name, agent.UID, agent.Generation
		}
	}
	state, err := sandboxcoordination.NewState("cetirun-system", name)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, state); err != nil {
		t.Fatal(err)
	}
	store := sandboxcoordination.Store{Client: storage, APIReader: storage, Namespace: state.Namespace,
		Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID}}
	ticket, err := store.AcquireExecution(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = store.BeginStop(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	return controller.SandboxExecutionStatusWriter{
		Reconciler: &controller.AgentReconciler{Client: storage, APIReader: storage}, Coordination: store,
		Now: func() time.Time { return f.now }, MaxAge: 15 * time.Second,
	}, ticket, agent
}
