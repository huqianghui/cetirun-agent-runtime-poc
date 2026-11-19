// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type journalExclusionReader struct {
	client.Reader
	gets, forbidden int
	after           func()
}

func (r *journalExclusionReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets++
	if _, ok := object.(*corev1.ConfigMap); !ok || key.Namespace != "operator-state" ||
		!strings.HasPrefix(key.Name, "sandbox-writers-") {
		r.forbidden++
		return errors.New("TEST: historical cleanup must only read exact writer journals")
	}
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	if r.after != nil {
		r.after()
	}
	return nil
}

func (r *journalExclusionReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	r.forbidden++
	return errors.New("TEST: listing current resources cannot reconstruct historical writers")
}

// Synthetic assertions exercise adapter orchestration only, not physical
// revocation, a production provider or qualification of runtime/storage behavior.
type journalExclusionProvider struct {
	revokes, observes int
	now               time.Time
	requests          []sandboxwriter.Request
	observe           func(context.Context, sandboxwriter.Request) (sandboxwriter.Observation, error)
}

func (p *journalExclusionProvider) Revoke(_ context.Context, request sandboxwriter.Request) error {
	p.revokes++
	p.requests = append(p.requests, request.DeepCopy())
	return nil
}

func (p *journalExclusionProvider) Observe(ctx context.Context, request sandboxwriter.Request) (sandboxwriter.Observation, error) {
	p.observes++
	p.requests = append(p.requests, request.DeepCopy())
	if p.observe != nil {
		return p.observe(ctx, request)
	}
	return journalExclusionProof(request, p.now), nil
}

func journalExclusionProof(request sandboxwriter.Request, now time.Time) sandboxwriter.Observation {
	result := sandboxwriter.Observation{
		Request: request.DeepCopy(), ObservedAt: now, Coverage: sandboxwriter.CompleteHistory,
		Barrier: sandboxwriter.BarrierEvidence{ID: "synthetic-start-barrier", Method: sandboxwriter.DurableStartBarrier},
		Home: sandboxwriter.HomeRelease{
			Home: request.Lifetime.Home, State: sandboxwriter.Released, EvidenceID: "synthetic-home-release",
		},
	}
	seen := map[sandboxwriter.StorageIdentity]bool{}
	for _, writer := range request.Writers {
		result.Writers = append(result.Writers, sandboxwriter.WriterEvidence{
			Writer: writer, Method: sandboxwriter.InfrastructureDestruction, EvidenceID: "synthetic-writer-" + writer.Epoch,
		})
		if !seen[writer.Storage] {
			result.Home.Volumes = append(result.Home.Volumes, sandboxwriter.StorageRelease{
				Storage: writer.Storage, State: sandboxwriter.Released, EvidenceID: "synthetic-storage-" + writer.Epoch,
			})
			seen[writer.Storage] = true
		}
	}
	return result
}

type journalExclusionFixture struct {
	raw              client.WithWatch
	reader           *journalExclusionReader
	creates, updates int
	store            sandboxwriter.Store
	manager          *sandboxwriter.Manager
	provider         *journalExclusionProvider
	source           *controller.JournalSandboxWriterExclusion
	agent            *api.Agent
	prerequisites    controller.SandboxPrerequisites
	lifetime         sandboxwriter.Lifetime
}

func newJournalExclusionFixture(t *testing.T) *journalExclusionFixture {
	t.Helper()
	f := &journalExclusionFixture{
		raw: fake.NewClientBuilder().WithScheme(testScheme()).Build(),
		agent: &api.Agent{ObjectMeta: metav1.ObjectMeta{
			Name: "demo", UID: "agent-uid", Generation: 3,
		}},
	}
	f.lifetime = sandboxwriter.Lifetime{
		Agent:     w.ObjectIdentity{Name: f.agent.Name, UID: f.agent.UID},
		Namespace: w.ObjectIdentity{Name: "cetirun-demo", UID: "namespace-uid"},
		Home:      w.ObjectIdentity{Name: "home", UID: "home-uid"},
	}
	f.reader = &journalExclusionReader{Reader: f.raw}
	writer := interceptor.NewClient(f.raw, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("TEST: cached journal reads are forbidden")
		},
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			f.creates++
			object.SetUID(types.UID(fmt.Sprintf("synthetic-journal-%d", f.creates)))
			return c.Create(ctx, object, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			f.updates++
			return c.Update(ctx, object, opts...)
		},
	})
	f.store = sandboxwriter.Store{Client: writer, APIReader: f.reader, Namespace: "operator-state"}
	f.provider = &journalExclusionProvider{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	f.manager = &sandboxwriter.Manager{
		Store: f.store, Provider: f.provider, Now: func() time.Time { return f.provider.now }, MaxAge: time.Minute,
	}
	f.source = &controller.JournalSandboxWriterExclusion{Manager: f.manager}
	f.prerequisites = controller.SandboxPrerequisites{
		AgentUID: f.agent.UID, AgentGeneration: 1, NamespaceUID: f.lifetime.Namespace.UID, Home: f.lifetime.Home,
		Revision: "old-revision", ObservedAt: f.provider.now.Add(-24 * time.Hour), ChartVersion: "0.0.1",
		Placement: &w.PlacementBinding{
			Registry: w.ObjectIdentity{Name: "historical-placement", UID: "registry-uid"},
			Agent:    f.lifetime.Agent, Namespace: f.lifetime.Namespace, Home: f.lifetime.Home,
			Workload: f.writer(1).Workload, Nodes: []w.PlacementNode{f.writer(1).Node, f.writer(2).Node},
		},
	}
	return f
}

func (f *journalExclusionFixture) writer(index int) sandboxwriter.Writer {
	return sandboxwriter.Writer{
		Workload: w.ObjectIdentity{Name: "openclaw-demo-cetirun", UID: "workload-uid"},
		Pod:      w.ObjectIdentity{Name: "openclaw-demo-cetirun-0", UID: types.UID(fmt.Sprintf("pod-%d", index))},
		Node: w.PlacementNode{
			Name: fmt.Sprintf("worker-%02d", index), UID: types.UID(fmt.Sprintf("node-%d", index)),
			ProviderID: fmt.Sprintf("example://worker-resource-%d", index),
		},
		MachineID: fmt.Sprintf("physical-incarnation-%d", index), Epoch: fmt.Sprintf("epoch-%d", index),
		Storage: sandboxwriter.StorageIdentity{
			Volume: w.ObjectIdentity{Name: fmt.Sprintf("volume-%d", index), UID: types.UID(fmt.Sprintf("volume-uid-%d", index))},
			Driver: "storage.example.com", Handle: fmt.Sprintf("storage-handle-%d", index),
		},
	}
}

func (f *journalExclusionFixture) history(t *testing.T, writers ...sandboxwriter.Writer) sandboxwriter.Snapshot {
	t.Helper()
	ctx := context.Background()
	snapshot, err := f.store.Enroll(ctx, f.lifetime, "synthetic-complete-birth-enrollment")
	if err != nil {
		t.Fatal(err)
	}
	for _, writer := range writers {
		snapshot, err = f.manager.Quiesce(ctx, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err = f.store.Reserve(ctx, snapshot, writer)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.resetCounts()
	return snapshot
}

func (f *journalExclusionFixture) resetCounts() {
	f.creates, f.updates, f.reader.gets, f.reader.forbidden = 0, 0, 0, 0
	f.provider.revokes, f.provider.observes, f.provider.requests = 0, 0, nil
}

func TestJournalSandboxWriterExclusionHistoricalCleanupWithoutCurrentLookups(t *testing.T) {
	f := newJournalExclusionFixture(t)
	pinned := f.history(t, f.writer(1), f.writer(2))
	ctx := context.Background()
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "removed-policy", UID: "policy-uid"}}
	if err := f.raw.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := f.raw.Delete(ctx, policy); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.lifetime.Namespace.Name, UID: "new-namespace-uid"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: f.lifetime.Home.Name, Namespace: f.lifetime.Namespace.Name, UID: "new-home-uid",
		}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: f.prerequisites.Placement.Registry.Name, Namespace: "operator-state", UID: "new-placement-uid",
		}, Data: map[string]string{"current": "unrelated-rebind"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: f.writer(1).Node.Name, UID: "new-node-uid",
		}, Spec: corev1.NodeSpec{ProviderID: f.writer(1).Node.ProviderID}},
	} {
		if err := f.raw.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	f.agent.Generation = 9
	f.agent.Spec.Suspended, f.agent.Spec.Agent.Type = true, api.AgentTypeCustom
	f.agent.Spec.Team, f.agent.Spec.SandboxRef = "rebound-team", "rebound-sandbox"
	f.agent.Spec.Helm.Version = "9.9.9"
	deleting := metav1.NewTime(f.provider.now)
	f.agent.DeletionTimestamp = &deleting
	agentBefore := f.agent.DeepCopy()
	placementBefore := f.prerequisites.Placement.DeepCopy()
	source := &controller.KubernetesSandboxPrerequisites{WriterExclusion: f.source}
	if err := source.CheckQuiesced(ctx, f.agent, f.prerequisites); err != nil {
		t.Fatal(err)
	}
	if f.provider.revokes != 1 || f.provider.observes != 1 || f.creates != 0 || f.updates != 2 ||
		f.reader.forbidden != 0 {
		t.Fatal("historical cleanup made unexpected calls or looked up current configuration")
	}
	for _, request := range f.provider.requests {
		if request.Lifetime != f.lifetime || request.JournalUID != pinned.UID || request.EnrollmentID != pinned.EnrollmentID ||
			request.OperationID == "" || !reflect.DeepEqual(request.Writers, pinned.Writers) {
			t.Fatal("adapter retargeted historical identities to current objects")
		}
	}
	if !reflect.DeepEqual(f.agent, agentBefore) || !reflect.DeepEqual(f.prerequisites.Placement, placementBefore) {
		t.Fatal("adapter mutated the supplied Agent or historical placement")
	}
	stored, err := f.store.Read(ctx, f.lifetime)
	if err != nil || stored.Phase != sandboxwriter.PhaseQuiescent || !reflect.DeepEqual(stored.Writers, pinned.Writers) {
		t.Fatalf("historical quiescence was not durably retained: %v", err)
	}
}

func TestJournalSandboxWriterExclusionValidatesBeforeSideEffects(t *testing.T) {
	tests := map[string]func(*api.Agent, *controller.SandboxPrerequisites){
		"namespaced Agent":    func(a *api.Agent, _ *controller.SandboxPrerequisites) { a.Namespace = "foreign" },
		"Agent name":          func(a *api.Agent, _ *controller.SandboxPrerequisites) { a.Name = "invalid/name" },
		"Agent UID":           func(a *api.Agent, _ *controller.SandboxPrerequisites) { a.UID = "" },
		"Agent generation":    func(a *api.Agent, _ *controller.SandboxPrerequisites) { a.Generation = 0 },
		"prerequisite UID":    func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.AgentUID = "foreign" },
		"zero generation":     func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.AgentGeneration = 0 },
		"future generation":   func(a *api.Agent, p *controller.SandboxPrerequisites) { p.AgentGeneration = a.Generation + 1 },
		"namespace UID":       func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.NamespaceUID = "" },
		"HOME UID":            func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Home.UID = "" },
		"HOME name":           func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Home.Name = "invalid/name" },
		"placement absent":    func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement = nil },
		"registry UID":        func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Registry.UID = "" },
		"placement Agent":     func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Agent.UID = "foreign" },
		"placement namespace": func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Namespace.UID = "foreign" },
		"placement HOME":      func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Home.UID = "foreign" },
		"workload name":       func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Workload.Name = "foreign" },
		"workload UID":        func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Workload.UID = "foreign" },
		"Node name":           func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Nodes[0].Name = "foreign" },
		"Node UID":            func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Nodes[0].UID = "foreign" },
		"Node provider": func(_ *api.Agent, p *controller.SandboxPrerequisites) {
			p.Placement.Nodes[0].ProviderID = "example://foreign"
		},
		"nodes absent":     func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Nodes = nil },
		"nodes duplicate":  func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Nodes[0] = p.Placement.Nodes[1] },
		"old Node omitted": func(_ *api.Agent, p *controller.SandboxPrerequisites) { p.Placement.Nodes = p.Placement.Nodes[1:] },
		"retargeted HOME": func(_ *api.Agent, p *controller.SandboxPrerequisites) {
			p.Home.UID = "new-home"
			p.Placement.Home = p.Home
		},
		"retargeted namespace": func(_ *api.Agent, p *controller.SandboxPrerequisites) {
			p.NamespaceUID = "new-namespace"
			p.Placement.Namespace.UID = p.NamespaceUID
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newJournalExclusionFixture(t)
			f.history(t, f.writer(1), f.writer(2))
			mutate(f.agent, &f.prerequisites)
			if err := f.source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatalf("invalid historical tuple was accepted: %v", err)
			}
			if f.provider.revokes+f.provider.observes+f.creates+f.updates != 0 {
				t.Fatal("historical validation occurred after a side effect")
			}
		})
	}
	t.Run("nil Agent", func(t *testing.T) {
		f := newJournalExclusionFixture(t)
		f.history(t, f.writer(1))
		if err := f.source.CheckQuiesced(context.Background(), nil, f.prerequisites); !errors.Is(err, sandbox.ErrObservationUnavailable) ||
			f.provider.revokes+f.provider.observes+f.creates+f.updates != 0 {
			t.Fatalf("nil Agent was not rejected before side effects: %v", err)
		}
	})
}

func TestJournalSandboxWriterExclusionChecksOldWorkloadNotJustLatest(t *testing.T) {
	f := newJournalExclusionFixture(t)
	first, second := f.writer(1), f.writer(2)
	second.Workload.UID = "new-workload-uid"
	f.history(t, first, second)
	f.prerequisites.Placement.Workload = second.Workload
	if err := f.source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !errors.Is(err, sandbox.ErrObservationUnavailable) ||
		f.provider.revokes+f.provider.observes+f.creates+f.updates != 0 {
		t.Fatalf("latest workload placement hid an incompatible historical workload: %v", err)
	}
}

func TestJournalSandboxWriterExclusionRequiresEnrollmentAndProvider(t *testing.T) {
	f := newJournalExclusionFixture(t)
	if err := f.source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !apierrors.IsNotFound(err) ||
		f.provider.revokes+f.provider.observes+f.creates+f.updates != 0 {
		t.Fatalf("adapter silently enrolled missing history: %v", err)
	}
	f.history(t)
	f.prerequisites.Placement = nil
	f.manager.Provider = nil
	if err := f.source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !errors.Is(err, sandboxwriter.ErrUnavailable) ||
		f.provider.revokes+f.provider.observes+f.creates+f.updates != 0 {
		t.Fatalf("empty history qualified without a provider: %v", err)
	}
	f.manager.Provider = f.provider
	f.provider.observe = func(_ context.Context, request sandboxwriter.Request) (sandboxwriter.Observation, error) {
		proof := journalExclusionProof(request, f.provider.now)
		proof.Home.State = "pending"
		return proof, nil
	}
	if err := f.source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !errors.Is(err, sandboxwriter.ErrUnproven) {
		t.Fatalf("empty history bypassed HOME release evidence: %v", err)
	}
	f.provider.observe = nil
	if err := f.source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.Read(context.Background(), f.lifetime)
	if err != nil || len(stored.Writers) != 0 || stored.Phase != sandboxwriter.PhaseQuiescent ||
		f.provider.revokes != 1 || f.provider.observes != 2 {
		t.Fatalf("adapter reserved a writer or resubmitted instead of observing: %v", err)
	}
}

func TestJournalSandboxWriterExclusionDoesNotRetargetRacedHistory(t *testing.T) {
	f := newJournalExclusionFixture(t)
	reserved := f.history(t, f.writer(1))
	quiescent, err := f.manager.Quiesce(context.Background(), reserved)
	if err != nil {
		t.Fatal(err)
	}
	f.resetCounts()
	f.reader.after = func() {
		f.reader.after = nil
		if _, err := f.store.Reserve(context.Background(), quiescent, f.writer(3)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !apierrors.IsConflict(err) ||
		f.provider.revokes+f.provider.observes != 0 || f.updates != 1 {
		t.Fatalf("adapter followed newly reserved history after historical placement validation: %v", err)
	}
}

func TestJournalSandboxWriterExclusionCancellationAndNilDefault(t *testing.T) {
	f := newJournalExclusionFixture(t)
	f.history(t, f.writer(1))
	for _, source := range []controller.SandboxWriterExclusionSource{
		(*controller.JournalSandboxWriterExclusion)(nil),
		&controller.JournalSandboxWriterExclusion{},
	} {
		if err := source.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !errors.Is(err, sandbox.ErrObservationUnavailable) {
			t.Fatalf("nil manager did not remain unavailable: %v", err)
		}
	}
	defaultSource := &controller.KubernetesSandboxPrerequisites{}
	if defaultSource.WriterExclusion != nil {
		t.Fatal("default source installed a provider")
	}
	if err := defaultSource.CheckQuiesced(context.Background(), f.agent, f.prerequisites); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatalf("unchanged nil default was not fail-closed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.source.CheckQuiesced(ctx, f.agent, f.prerequisites); !errors.Is(err, context.Canceled) ||
		f.provider.revokes+f.provider.observes+f.creates+f.updates != 0 {
		t.Fatalf("canceled adapter call had side effects: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	f.provider.observe = func(_ context.Context, request sandboxwriter.Request) (sandboxwriter.Observation, error) {
		cancel()
		return journalExclusionProof(request, f.provider.now), nil
	}
	if err := f.source.CheckQuiesced(ctx, f.agent, f.prerequisites); !errors.Is(err, context.Canceled) {
		t.Fatalf("provider cancellation was hidden: %v", err)
	}
	stored, err := f.store.Read(context.Background(), f.lifetime)
	if err != nil || stored.Phase != sandboxwriter.PhaseRevoking {
		t.Fatalf("canceled observation persisted quiescence: %v", err)
	}
}
