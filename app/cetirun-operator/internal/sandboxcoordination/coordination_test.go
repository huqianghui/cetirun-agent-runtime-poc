// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func source(version, kind, namespace, name, uid string) w.SourceReference {
	return w.SourceReference{APIVersion: version, Kind: kind, Namespace: namespace, Name: name,
		UID: types.UID(uid), ResourceVersion: "1"}
}

func testIntent(suffix string) Intent {
	agent := w.ObjectIdentity{Name: "agent-" + suffix, UID: types.UID("agent-uid-" + suffix)}
	namespace := w.ObjectIdentity{Name: "cetirun-agent-" + suffix, UID: types.UID("namespace-uid-" + suffix)}
	home := w.ObjectIdentity{Name: "home", UID: types.UID("home-uid-" + suffix)}
	workload := w.ObjectIdentity{Name: "agent-" + suffix, UID: types.UID("workload-uid-" + suffix)}
	preparation := source("v1", "ConfigMap", "operator-state", "preparation-"+suffix, "preparation-uid-"+suffix)
	intent := Intent{
		Agent: agent, AgentGeneration: 1, Namespace: namespace, Home: home, Workload: workload,
		WorkloadDigest:   strings.Repeat("d", 64),
		TeamSharedDigest: strings.Repeat("f", 64),
		Preparation:      preparation, IngressMode: api.AgentPolicyIngressAuthenticated,
		Expected: sandbox.ExecutionIdentity{
			Binding: sandbox.Binding{AgentUID: agent.UID, TeamUID: "team-uid", SandboxUID: "sandbox-uid",
				AgentPolicyUID: "policy-uid"},
			Revision: "compiled-revision", PodUID: types.UID("pod-uid-" + suffix),
			NodeUID: types.UID("node-uid-" + suffix), EnforcementEpoch: "epoch-" + suffix,
		},
		Placement: &w.PlacementBinding{
			Agent: agent, Namespace: namespace, Home: home, Workload: workload,
			Registry: w.ObjectIdentity{Name: "placement", UID: "placement-uid"},
			Nodes:    []w.PlacementNode{{Name: "node-" + suffix, UID: types.UID("node-uid-" + suffix), ProviderID: "example://node-" + suffix}},
		},
	}
	intent.Writer = sandboxwriter.Writer{
		Workload: workload, Pod: w.ObjectIdentity{Name: workload.Name + "-0", UID: intent.Expected.PodUID},
		Node: intent.Placement.Nodes[0], MachineID: "synthetic-machine-incarnation-" + suffix,
		Epoch: intent.Expected.EnforcementEpoch,
		Storage: sandboxwriter.StorageIdentity{Volume: w.ObjectIdentity{Name: "pv-" + suffix, UID: types.UID("pv-uid-" + suffix)},
			Driver: "csi.example.com", Handle: "synthetic-volume-" + suffix},
	}
	intent.WriterJournal = w.ObjectIdentity{
		Name: sandboxwriter.JournalName(sandboxwriter.Lifetime{Agent: agent, Namespace: namespace, Home: home}),
		UID:  types.UID("journal-uid-" + suffix),
	}
	intent.Sources = []w.SourceReference{
		{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID, Generation: 1, IntentDigest: strings.Repeat("a", 64)},
		preparation,
		source(api.GroupVersion.String(), "Team", "", "team", "team-uid"),
		source(api.GroupVersion.String(), "Sandbox", "", "sandbox", "sandbox-uid"),
		source(api.GroupVersion.String(), "AgentPolicy", "", "policy", "policy-uid"),
		source("v1", "Namespace", "", namespace.Name, string(namespace.UID)),
		source("v1", "PersistentVolumeClaim", namespace.Name, home.Name, string(home.UID)),
		source("apps/v1", "StatefulSet", namespace.Name, workload.Name, string(workload.UID)),
		source("v1", "ConfigMap", "operator-state", "placement", "placement-uid"),
	}
	intent.SourceDigests = map[string]string{}
	for _, source := range intent.Sources {
		if w.EffectiveSource(source) {
			intent.SourceDigests[w.SourceKey(source)] = strings.Repeat("e", 64)
		}
	}
	return intent
}

func fixture(t *testing.T) (Store, client.WithWatch) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, err := NewState("operator-state", "coordination")
	if err != nil {
		t.Fatal(err)
	}
	object.UID, object.ResourceVersion = "coordination-uid", "1"
	storage := fake.NewClientBuilder().WithScheme(scheme).WithObjects(object).Build()
	writer := interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("cached writer must not supply authority reads")
		},
	})
	return Store{Client: writer, APIReader: storage, Namespace: object.Namespace,
		Identity: w.ObjectIdentity{Name: object.Name, UID: object.UID}}, storage
}

func acquire(t *testing.T, store Store, intent Intent) Ticket {
	t.Helper()
	ticket, err := store.AcquireExecution(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestSharedSourcesExcludeChangesAcrossExecutionPhases(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	first := acquire(t, store, testIntent("one"))
	second := acquire(t, store, testIntent("two"))
	policy := first.Execution.Intent.Sources[4]
	for _, phase := range []Phase{Reserved, Starting, Running, Stopping} {
		var err error
		switch phase {
		case Starting:
			first, err = store.BeginStart(ctx, first)
		case Running:
			first, err = store.MarkRunning(ctx, first)
		case Stopping:
			first, err = store.BeginStop(ctx, first)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcquireChange(ctx, policy, strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s allowed a shared policy change: %v", phase, err)
		}
	}
	if err := store.ReleaseExecution(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireChange(ctx, policy, strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
		t.Fatal("second reader no longer protected the policy:", err)
	}
	second, err := store.BeginStop(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseExecution(ctx, second); err != nil {
		t.Fatal(err)
	}
	change, err := store.AcquireChange(ctx, policy, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireExecution(ctx, testIntent("one")); !errors.Is(err, ErrConflict) {
		t.Fatal("change did not exclude a new start:", err)
	}
	if err := store.ReleaseChange(ctx, change); !errors.Is(err, ErrConflict) {
		t.Fatal("unsubmitted change released without a terminal outcome:", err)
	}
	change, err = store.BeginChange(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseChange(ctx, change); err != nil {
		t.Fatal(err)
	}
	acquire(t, store, testIntent("one"))
}

func TestIdentityPinsAndNoAutomaticTakeover(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	intent := testIntent("one")
	ticket := acquire(t, store, intent)
	retry, err := store.AcquireExecution(ctx, intent)
	if err != nil || retry.Execution.ID != ticket.Execution.ID {
		t.Fatal("exact retry did not recover the durable operation:", err)
	}
	for name, mutate := range map[string]func(*Intent){
		"generation": func(i *Intent) { i.AgentGeneration++; i.Sources[0].Generation++ },
		"policy":     func(i *Intent) { i.Sources[4].ResourceVersion = "2" },
		"Pod":        func(i *Intent) { i.Expected.PodUID = "replacement-pod" },
		"epoch":      func(i *Intent) { i.Expected.EnforcementEpoch = "replacement-epoch" },
		"HOME":       func(i *Intent) { i.Home.UID = "replacement-home" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := cloneExecution(ticket.Execution).Intent
			mutate(&changed)
			if _, err := store.AcquireExecution(ctx, changed); err == nil {
				t.Fatal("changed execution stole an outstanding reservation")
			}
		})
	}
	starting, err := store.BeginStart(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginStart(ctx, ticket); !errors.Is(err, ErrConflict) {
		t.Fatal("old ticket authorized a duplicate start:", err)
	}
	restarted := Store{Client: store.Client, APIReader: store.APIReader, Namespace: store.Namespace, Identity: store.Identity}
	recovered, err := restarted.AcquireExecution(ctx, intent)
	if err != nil || recovered.Execution.ID != starting.Execution.ID || recovered.Execution.Phase != Starting {
		t.Fatal("restart forgot the uncertain start:", err)
	}
	if _, err := restarted.BeginStart(ctx, recovered); !errors.Is(err, ErrConflict) {
		t.Fatal("recovered intent authorized resubmission:", err)
	}
	// Returned state is detached from persistence and other holders.
	recovered.Execution.Intent.Placement.Nodes[0].ProviderID = "example://caller-only"
	recovered.Execution.Intent.Sources[0].IntentDigest = strings.Repeat("c", 64)
	if err := store.Check(ctx, starting); err != nil {
		t.Fatal("caller mutation changed persisted authority:", err)
	}
}

func TestDifferentAgentsCannotReserveTheSamePhysicalWriterOrVolume(t *testing.T) {
	for _, scenario := range []string{"journal", "machine", "PV UID", "volume handle"} {
		t.Run(scenario, func(t *testing.T) {
			store, _ := fixture(t)
			first := testIntent("one")
			acquire(t, store, first)
			second := testIntent("two")
			switch scenario {
			case "journal":
				second.WriterJournal.UID = first.WriterJournal.UID
			case "machine":
				second.Writer.MachineID = first.Writer.MachineID
			case "PV UID":
				second.Writer.Storage.Volume.UID = first.Writer.Storage.Volume.UID
			case "volume handle":
				second.Writer.Storage.Handle = first.Writer.Storage.Handle
			}
			if _, err := store.AcquireExecution(context.Background(), second); !errors.Is(err, ErrConflict) {
				t.Fatal("different logical HOME identities reused an exclusive physical identity:", err)
			}
		})
	}
}

func TestConcurrentStartAndChangeHaveOneCASWinner(t *testing.T) {
	ctx := context.Background()
	store, storage := fixture(t)
	var reads atomic.Int32
	barrier := make(chan struct{})
	store.APIReader = interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, object, opts...); err != nil {
				return err
			}
			switch reads.Add(1) {
			case 1:
				<-barrier
			case 2:
				close(barrier)
			}
			return nil
		},
	})
	var group sync.WaitGroup
	results := make(chan error, 2)
	group.Go(func() {
		_, err := store.AcquireExecution(ctx, testIntent("one"))
		results <- err
	})
	group.Go(func() {
		_, err := store.AcquireChange(ctx, testIntent("one").Sources[4], strings.Repeat("b", 64))
		results <- err
	})
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected one serialized winner, got %d", successes)
	}
}

func TestMissingCorruptOrReplacedAuthorityNeverRecoversFromNames(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.ConfigMap){
		"UID":            func(o *corev1.ConfigMap) { o.UID = "replacement-authority" },
		"protection":     func(o *corev1.ConfigMap) { o.Finalizers = nil },
		"null":           func(o *corev1.ConfigMap) { o.Data[DataKey] = "null" },
		"unsupported v2": func(object *corev1.ConfigMap) { object.Data[DataKey] = `{"version":2,"executions":[],"changes":[]}` },
		"unsupported v3": func(object *corev1.ConfigMap) { object.Data[DataKey] = `{"version":3,"executions":[],"changes":[]}` },
		"duplicate":      func(o *corev1.ConfigMap) { o.Data[DataKey] = `{"version":1,"version":1,"executions":[],"changes":[]}` },
		"unknown":        func(o *corev1.ConfigMap) { o.Data[DataKey] = `{"version":1,"executions":[],"changes":[],"allow":true}` },
		"inventory":      func(o *corev1.ConfigMap) { o.Data[DataKey] = `{"version":1,"executions":null,"changes":[]}` },
		"extra data":     func(o *corev1.ConfigMap) { o.Data["unexpected"] = "synthetic" },
		"binary data":    func(o *corev1.ConfigMap) { o.BinaryData = map[string][]byte{"unexpected": []byte("synthetic")} },
		"metadata bound": func(o *corev1.ConfigMap) {
			o.Annotations = map[string]string{"example.com/large": strings.Repeat("x", MaxBytes)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, storage := fixture(t)
			object := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			if err := storage.Get(context.Background(), key, object); err != nil {
				t.Fatal(err)
			}
			mutate(object)
			if err := storage.Update(context.Background(), object); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AcquireExecution(context.Background(), testIntent("one")); err == nil {
				t.Fatal("invalid authority was accepted")
			}
		})
	}
	store, _ := fixture(t)
	store.Identity.Name = "missing"
	if _, err := store.AcquireExecution(context.Background(), testIntent("one")); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing authority was treated as an empty record:", err)
	}
}

func TestRequiredSourcesAndStoragePlacementPins(t *testing.T) {
	for index := range testIntent("one").Sources {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			store, _ := fixture(t)
			intent := testIntent("one")
			intent.Sources = append(intent.Sources[:index:index], intent.Sources[index+1:]...)
			if _, err := store.AcquireExecution(context.Background(), intent); err == nil {
				t.Fatal("missing required source accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*Intent){
		"no placement":       func(i *Intent) { i.Placement = nil },
		"node UID":           func(i *Intent) { i.Expected.NodeUID = "foreign-node" },
		"home UID":           func(i *Intent) { i.Placement.Home.UID = "foreign-home" },
		"binding":            func(i *Intent) { i.Expected.Binding.AgentPolicyUID = "foreign-policy" },
		"duplicate":          func(i *Intent) { i.Sources = append(i.Sources, i.Sources[0]) },
		"no ingress":         func(i *Intent) { i.IngressMode = "" },
		"no digest":          func(i *Intent) { i.WorkloadDigest = "" },
		"bad digest":         func(i *Intent) { i.WorkloadDigest = "not-a-digest" },
		"bad running digest": func(i *Intent) { i.RunningPreparationDigest = "not-a-digest" },
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := fixture(t)
			intent := testIntent("one")
			mutate(&intent)
			if _, err := store.AcquireExecution(context.Background(), intent); err == nil {
				t.Fatal("incomplete execution accepted")
			}
		})
	}
}

func TestEntryBoundRetainsOutstandingChanges(t *testing.T) {
	store, _ := fixture(t)
	for i := range MaxEntries {
		before := source("v1", "ConfigMap", "example", fmt.Sprintf("source-%d", i), fmt.Sprintf("uid-%d", i))
		if _, err := store.AcquireChange(context.Background(), before, strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AcquireChange(context.Background(), source("v1", "ConfigMap", "example", "overflow", "overflow-uid"), strings.Repeat("a", 64)); err == nil {
		t.Fatal("entry bound silently evicted outstanding operations")
	}
	_, state, err := store.read(context.Background())
	if err != nil || len(state.Changes) != MaxEntries {
		t.Fatal("failed acquisition damaged existing reservations:", err)
	}
}

func TestCanceledContextCannotAcquireOrRelease(t *testing.T) {
	store, _ := fixture(t)
	ticket := acquire(t, store, testIntent("one"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.BeginStart(ctx, ticket); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := store.AcquireChange(ctx, testIntent("two").Sources[4], strings.Repeat("a", 64)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := store.Check(context.Background(), ticket); err != nil {
		t.Fatal("cancellation changed authority:", err)
	}
}

func TestLostTransitionReplyCannotAuthorizeStart(t *testing.T) {
	store, storage := fixture(t)
	ticket := acquire(t, store, testIntent("one"))
	store.Client = interceptor.NewClient(storage, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if err := c.Update(ctx, object, opts...); err != nil {
				return err
			}
			return errors.New("synthetic lost write response")
		},
	})
	if _, err := store.BeginStart(context.Background(), ticket); err == nil {
		t.Fatal("lost reply authorized an external start")
	}
	recovered, err := store.ReadExecution(context.Background(), ticket.Execution.Intent.Agent.UID)
	if err != nil || recovered.Execution.Phase != Starting {
		t.Fatal("persisted intent was lost:", err)
	}
	if _, err := store.BeginStart(context.Background(), recovered); !errors.Is(err, ErrConflict) {
		t.Fatal("uncertain transition authorized resubmission:", err)
	}
}
