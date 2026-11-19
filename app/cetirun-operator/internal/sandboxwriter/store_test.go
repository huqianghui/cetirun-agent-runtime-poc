// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxwriter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type writerTestClient struct {
	client.Client
	creates, updates int
	beforeCreate     func(context.Context, client.Object) error
	afterCreate      func(context.Context, client.Object) error
	beforeUpdate     func(context.Context, client.Object) error
	afterUpdate      func(context.Context, client.Object) error
}

func (*writerTestClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("TEST: cached client must never read writer history")
}

func (*writerTestClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("TEST: journal discovery is forbidden")
}

func (c *writerTestClient) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	c.creates++
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.beforeCreate != nil {
		if err := c.beforeCreate(ctx, object); err != nil {
			return err
		}
	}
	object.SetUID(types.UID(fmt.Sprintf("synthetic-journal-%d", c.creates)))
	if err := c.Client.Create(ctx, object, opts...); err != nil {
		return err
	}
	if c.afterCreate != nil {
		return c.afterCreate(ctx, object)
	}
	return nil
}

func (c *writerTestClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	c.updates++
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.beforeUpdate != nil {
		if err := c.beforeUpdate(ctx, object); err != nil {
			return err
		}
	}
	current := &corev1.ConfigMap{}
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
		return err
	}
	// The fake API assigns RVs but does not enforce the API server's immutable
	// UID check. Model that check here and require both CAS inputs on every write.
	if object.GetUID() == "" || object.GetUID() != current.UID ||
		object.GetResourceVersion() == "" || object.GetResourceVersion() != current.ResourceVersion {
		return conflict(object.GetName(), "TEST: UID/resource-version conflict")
	}
	if err := c.Client.Update(ctx, object, opts...); err != nil {
		return err
	}
	if c.afterUpdate != nil {
		return c.afterUpdate(ctx, object)
	}
	return nil
}

type writerTestReader struct {
	client.Reader
	gets   int
	before func(context.Context, client.ObjectKey, client.Object) error
	after  func(context.Context, client.ObjectKey, client.Object) error
}

func (r *writerTestReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets++
	if _, ok := object.(*corev1.ConfigMap); !ok || key.Namespace != "operator-state" {
		return errors.New("TEST: only exact protected journal reads are permitted")
	}
	if r.before != nil {
		if err := r.before(ctx, key, object); err != nil {
			return err
		}
	}
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	if r.after != nil {
		return r.after(ctx, key, object)
	}
	return nil
}

func (*writerTestReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("TEST: history may not be reconstructed by listing")
}

// This provider records synthetic calls and assertions only. It does not
// implement or qualify a runtime, infrastructure fence or storage release.
type writerTestProvider struct {
	revokes, observes []Request
	revoke            func(context.Context, Request) error
	observe           func(context.Context, Request) (Observation, error)
}

func (p *writerTestProvider) Revoke(ctx context.Context, request Request) error {
	p.revokes = append(p.revokes, request.DeepCopy())
	if p.revoke != nil {
		return p.revoke(ctx, request)
	}
	return nil
}

func (p *writerTestProvider) Observe(ctx context.Context, request Request) (Observation, error) {
	p.observes = append(p.observes, request.DeepCopy())
	if p.observe == nil {
		return Observation{}, errors.New("TEST: no evidence configured")
	}
	return p.observe(ctx, request)
}

type writerFixture struct {
	raw      client.Client
	writes   *writerTestClient
	reads    *writerTestReader
	store    Store
	manager  *Manager
	provider *writerTestProvider
	lifetime Lifetime
	now      time.Time
}

func newWriterFixture(t *testing.T) *writerFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	raw := fake.NewClientBuilder().WithScheme(scheme).Build()
	f := &writerFixture{
		raw: raw, writes: &writerTestClient{Client: raw}, reads: &writerTestReader{Reader: raw},
		lifetime: Lifetime{
			Agent:     sandboxworkload.ObjectIdentity{Name: "demo", UID: "agent-uid"},
			Namespace: sandboxworkload.ObjectIdentity{Name: "cetirun-demo", UID: "namespace-uid"},
			Home:      sandboxworkload.ObjectIdentity{Name: "home", UID: "home-uid"},
		},
		now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	f.store = Store{Client: f.writes, APIReader: f.reads, Namespace: "operator-state"}
	f.provider = &writerTestProvider{observe: func(_ context.Context, request Request) (Observation, error) {
		return syntheticObservation(request, f.now), nil
	}}
	f.manager = &Manager{Store: f.store, Provider: f.provider, Now: func() time.Time { return f.now }, MaxAge: time.Minute}
	return f
}

func syntheticWriter(index int) Writer {
	return Writer{
		Workload: sandboxworkload.ObjectIdentity{Name: "openclaw-demo-cetirun", UID: "workload-uid"},
		Pod:      sandboxworkload.ObjectIdentity{Name: "openclaw-demo-cetirun-0", UID: types.UID(fmt.Sprintf("pod-%d", index))},
		Node: sandboxworkload.PlacementNode{
			Name: fmt.Sprintf("worker-%03d", index), UID: types.UID(fmt.Sprintf("node-%d", index)),
			ProviderID: fmt.Sprintf("example://worker-resource-%d", index),
		},
		MachineID: fmt.Sprintf("physical-incarnation-%d", index), Epoch: fmt.Sprintf("epoch-%d", index),
		Storage: StorageIdentity{
			Volume: sandboxworkload.ObjectIdentity{Name: fmt.Sprintf("home-volume-%d", index), UID: types.UID(fmt.Sprintf("volume-%d", index))},
			Driver: "storage.example.com", Handle: fmt.Sprintf("volume-handle-%d", index),
		},
	}
}

func syntheticObservation(request Request, now time.Time) Observation {
	result := Observation{
		Request: request.DeepCopy(), ObservedAt: now, Coverage: CompleteHistory,
		Barrier: BarrierEvidence{ID: "synthetic-barrier-" + request.OperationID, Method: DurableStartBarrier},
		Writers: []WriterEvidence{},
		Home: HomeRelease{
			Home: request.Lifetime.Home, State: Released, EvidenceID: "synthetic-home-release", Volumes: []StorageRelease{},
		},
	}
	seen := map[StorageIdentity]bool{}
	for _, writer := range request.Writers {
		result.Writers = append(result.Writers, WriterEvidence{
			Writer: writer, Method: RuntimeRevocation, EvidenceID: "synthetic-exclusion-" + writer.Epoch,
		})
		if !seen[writer.Storage] {
			result.Home.Volumes = append(result.Home.Volumes, StorageRelease{
				Storage: writer.Storage, State: Released, EvidenceID: "synthetic-storage-" + string(writer.Storage.Volume.UID),
			})
			seen[writer.Storage] = true
		}
	}
	return result
}

func (f *writerFixture) enroll(t *testing.T) Snapshot {
	t.Helper()
	snapshot, err := f.store.Enroll(context.Background(), f.lifetime, "synthetic-complete-birth-enrollment")
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *writerFixture) read(t *testing.T) Snapshot {
	t.Helper()
	snapshot, err := f.store.Read(context.Background(), f.lifetime)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *writerFixture) quiesce(t *testing.T, pinned Snapshot) Snapshot {
	t.Helper()
	snapshot, err := f.manager.Quiesce(context.Background(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *writerFixture) history(t *testing.T, writers ...Writer) Snapshot {
	t.Helper()
	snapshot := f.enroll(t)
	for _, writer := range writers {
		snapshot = f.quiesce(t, snapshot)
		var err error
		snapshot, err = f.store.Reserve(context.Background(), snapshot, writer)
		if err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func (f *writerFixture) object(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	object := &corev1.ConfigMap{}
	if err := f.raw.Get(context.Background(), client.ObjectKey{Namespace: f.store.Namespace, Name: JournalName(f.lifetime)}, object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestWriterExplicitEnrollmentAndMissingHistory(t *testing.T) {
	f := newWriterFixture(t)
	ctx := context.Background()
	if _, err := f.store.Read(ctx, f.lifetime); !apierrors.IsNotFound(err) {
		t.Fatalf("missing history became inventory: %v", err)
	}
	if err := f.manager.CheckQuiesced(ctx, f.lifetime); !apierrors.IsNotFound(err) {
		t.Fatalf("missing history did not block: %v", err)
	}
	if f.writes.creates != 0 || f.writes.updates != 0 || len(f.provider.revokes)+len(f.provider.observes) != 0 {
		t.Fatal("checking missing history enrolled or called the provider")
	}
	enrolled := f.enroll(t)
	if enrolled.UID == "" || enrolled.ResourceVersion == "" || enrolled.Phase != PhaseEnrolled ||
		enrolled.Version != JournalVersion || enrolled.Lifetime != f.lifetime || enrolled.EnrollmentID == "" ||
		enrolled.Writers == nil || len(enrolled.Writers) != 0 || enrolled.OperationID != "" {
		t.Fatalf("incomplete enrollment: %+v", enrolled)
	}
	if _, err := f.store.Reserve(ctx, enrolled, syntheticWriter(1)); err == nil {
		t.Fatal("enrollment was treated as verified quiescence")
	}
	if _, err := f.store.Enroll(ctx, f.lifetime, enrolled.EnrollmentID); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("enrollment adopted an existing journal: %v", err)
	}
	if _, err := f.store.Enroll(ctx, f.lifetime, "replacement-enrollment"); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("enrollment replaced history: %v", err)
	}
	if !reflect.DeepEqual(f.read(t), enrolled) {
		t.Fatal("enrollment retry changed the original history")
	}
	object := f.object(t)
	expectedOwner := sandboxworkload.AgentOwner("demo", "agent-uid")
	expectedOwner.BlockOwnerDeletion = ptr.To(false)
	if !reflect.DeepEqual(object.OwnerReferences, []metav1.OwnerReference{expectedOwner}) ||
		!reflect.DeepEqual(object.Finalizers, []string{JournalFinalizer}) || object.Namespace != "operator-state" {
		t.Fatal("enrollment must retain protected UID ownership without blocking foreground Agent deletion")
	}
	changed := f.lifetime
	changed.Home.UID = "replacement-home"
	if JournalName(changed) == JournalName(f.lifetime) {
		t.Fatal("journal name does not bind the HOME lifetime")
	}
	if _, err := f.store.Read(ctx, changed); !apierrors.IsNotFound(err) {
		t.Fatalf("HOME rebind rediscovered old inventory: %v", err)
	}
}

func TestWriterEnrollmentValidation(t *testing.T) {
	tests := map[string]func(*Store, *Lifetime, *string){
		"missing reader":       func(s *Store, _ *Lifetime, _ *string) { s.APIReader = nil },
		"missing writer":       func(s *Store, _ *Lifetime, _ *string) { s.Client = nil },
		"namespace absent":     func(s *Store, _ *Lifetime, _ *string) { s.Namespace = "" },
		"workload namespace":   func(s *Store, l *Lifetime, _ *string) { s.Namespace = l.Namespace.Name },
		"invalid namespace":    func(s *Store, _ *Lifetime, _ *string) { s.Namespace = "invalid/name" },
		"Agent absent":         func(_ *Store, l *Lifetime, _ *string) { l.Agent.UID = "" },
		"Agent name":           func(_ *Store, l *Lifetime, _ *string) { l.Agent.Name = "invalid/name" },
		"namespace UID":        func(_ *Store, l *Lifetime, _ *string) { l.Namespace.UID = "" },
		"namespace name":       func(_ *Store, l *Lifetime, _ *string) { l.Namespace.Name = "invalid.namespace" },
		"HOME name":            func(_ *Store, l *Lifetime, _ *string) { l.Home.Name = "" },
		"HOME UID":             func(_ *Store, l *Lifetime, _ *string) { l.Home.UID = "" },
		"oversized UID":        func(_ *Store, l *Lifetime, _ *string) { l.Home.UID = types.UID(strings.Repeat("u", 129)) },
		"empty enrollment":     func(_ *Store, _ *Lifetime, e *string) { *e = "" },
		"unbounded enrollment": func(_ *Store, _ *Lifetime, e *string) { *e = strings.Repeat("e", 257) },
		"ambiguous enrollment": func(_ *Store, _ *Lifetime, e *string) { *e = "enrollment\nother" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			store, lifetime, enrollment := f.store, f.lifetime, "birth-enrollment"
			change(&store, &lifetime, &enrollment)
			if _, err := store.Enroll(context.Background(), lifetime, enrollment); err == nil || f.writes.creates != 0 {
				t.Fatalf("invalid enrollment reached persistence: %v", err)
			}
		})
	}
}

func TestWriterJournalRejectsForeignReleasedAndCorruptHistory(t *testing.T) {
	tests := map[string]func(*corev1.ConfigMap){
		"name":                 func(o *corev1.ConfigMap) { o.Name = "foreign" },
		"namespace":            func(o *corev1.ConfigMap) { o.Namespace = "foreign" },
		"UID":                  func(o *corev1.ConfigMap) { o.UID = "" },
		"resource version":     func(o *corev1.ConfigMap) { o.ResourceVersion = "" },
		"kind":                 func(o *corev1.ConfigMap) { o.Kind = "Secret" },
		"api version":          func(o *corev1.ConfigMap) { o.APIVersion = "v2" },
		"owner UID":            func(o *corev1.ConfigMap) { o.OwnerReferences[0].UID = "foreign" },
		"owner name":           func(o *corev1.ConfigMap) { o.OwnerReferences[0].Name = "foreign" },
		"owner kind":           func(o *corev1.ConfigMap) { o.OwnerReferences[0].Kind = "Sandbox" },
		"owner absent":         func(o *corev1.ConfigMap) { o.OwnerReferences = nil },
		"multiple owners":      func(o *corev1.ConfigMap) { o.OwnerReferences = append(o.OwnerReferences, o.OwnerReferences[0]) },
		"controller absent":    func(o *corev1.ConfigMap) { o.OwnerReferences[0].Controller = nil },
		"block deletion true":  func(o *corev1.ConfigMap) { o.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true) },
		"block deletion nil":   func(o *corev1.ConfigMap) { o.OwnerReferences[0].BlockOwnerDeletion = nil },
		"Agent annotation":     func(o *corev1.ConfigMap) { o.Annotations[sandboxworkload.AgentUIDAnnotation] = "foreign" },
		"lifetime annotation":  func(o *corev1.ConfigMap) { o.Annotations[lifetimeAnnotation] = "foreign" },
		"missing finalizer":    func(o *corev1.ConfigMap) { o.Finalizers = nil },
		"duplicate finalizer":  func(o *corev1.ConfigMap) { o.Finalizers = append(o.Finalizers, JournalFinalizer) },
		"released receipt":     func(o *corev1.ConfigMap) { o.Annotations[JournalReleasedAnnotation] = string(o.UID) },
		"empty release marker": func(o *corev1.ConfigMap) { o.Annotations[JournalReleasedAnnotation] = "" },
		"immutable":            func(o *corev1.ConfigMap) { value := true; o.Immutable = &value },
		"binary data":          func(o *corev1.ConfigMap) { o.BinaryData = map[string][]byte{"unexpected": {1}} },
		"extra data":           func(o *corev1.ConfigMap) { o.Data["unexpected"] = "{}" },
		"missing data":         func(o *corev1.ConfigMap) { o.Data = nil },
		"wrong data key":       func(o *corev1.ConfigMap) { o.Data = map[string]string{"foreign": o.Data[JournalDataKey]} },
		"invalid JSON":         func(o *corev1.ConfigMap) { o.Data[JournalDataKey] = "{" },
		"multiple documents":   func(o *corev1.ConfigMap) { o.Data[JournalDataKey] += "{}" },
		"duplicate field": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"version":"v1"`, `"version":"v1","version":"v1"`, 1)
		},
		"unknown field": func(o *corev1.ConfigMap) { o.Data[JournalDataKey] = `{"unknown":true,` + o.Data[JournalDataKey][1:] },
		"case variant": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"version"`, `"Version"`, 1)
		},
		"null history": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"writers":[]`, `"writers":null`, 1)
		},
		"missing history": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"writers":[],`, "", 1)
		},
		"version drift": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"version":"v1"`, `"version":"v2"`, 1)
		},
		"released phase": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"phase":"enrolled"`, `"phase":"released"`, 1)
		},
		"HOME drift": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"home-uid"`, `"other-home-uid"`, 1)
		},
		"enrollment absent": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = strings.Replace(o.Data[JournalDataKey], `"synthetic-complete-birth-enrollment"`, `""`, 1)
		},
		"unbounded data":     func(o *corev1.ConfigMap) { o.Data[JournalDataKey] = strings.Repeat(" ", MaxJournalBytes+1) },
		"unbounded metadata": func(o *corev1.ConfigMap) { o.Annotations["example.com/padding"] = strings.Repeat("x", MaxJournalBytes) },
		"invalid UTF8":       func(o *corev1.ConfigMap) { o.Data[JournalDataKey] += string([]byte{0xff}) },
		"deep nesting":       func(o *corev1.ConfigMap) { o.Data[JournalDataKey] = `{"unknown":[[[[[[[]]]]]]]}` },
		"too many entries": func(o *corev1.ConfigMap) {
			o.Data[JournalDataKey] = `{"writers":[` + strings.Repeat("{},", MaxWriters) + `{}]}`
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			f.enroll(t)
			f.reads.after = func(_ context.Context, _ client.ObjectKey, object client.Object) error {
				mutate(object.(*corev1.ConfigMap))
				return nil
			}
			if _, err := f.store.Read(context.Background(), f.lifetime); !errors.Is(err, ErrInvalidHistory) {
				t.Fatalf("invalid journal was readable: %v", err)
			}
			if err := f.manager.CheckQuiesced(context.Background(), f.lifetime); err == nil ||
				len(f.provider.revokes)+len(f.provider.observes) != 0 || f.writes.updates != 0 {
				t.Fatalf("invalid journal reached the provider: %v", err)
			}
		})
	}
}

func TestWriterReservationPinsAllSnapshotFields(t *testing.T) {
	tests := map[string]func(*Snapshot){
		"UID":             func(s *Snapshot) { s.UID = "replacement-journal" },
		"RV":              func(s *Snapshot) { s.ResourceVersion = "older" },
		"version":         func(s *Snapshot) { s.Version = "v2" },
		"Agent":           func(s *Snapshot) { s.Lifetime.Agent.UID = "replacement-agent" },
		"namespace":       func(s *Snapshot) { s.Lifetime.Namespace.UID = "replacement-namespace" },
		"HOME":            func(s *Snapshot) { s.Lifetime.Home.UID = "replacement-home" },
		"enrollment":      func(s *Snapshot) { s.EnrollmentID = "replacement-enrollment" },
		"phase":           func(s *Snapshot) { s.Phase = PhaseRevoking },
		"operation":       func(s *Snapshot) { s.OperationID = strings.Repeat("a", 32) },
		"history erased":  func(s *Snapshot) { s.Writers = []Writer{} },
		"writer machine":  func(s *Snapshot) { s.Writers[0].MachineID = "replacement-machine" },
		"writer storage":  func(s *Snapshot) { s.Writers[0].Storage.Handle = "replacement-volume" },
		"writer Pod UID":  func(s *Snapshot) { s.Writers[0].Pod.UID = "replacement-pod" },
		"writer workload": func(s *Snapshot) { s.Writers[0].Workload.UID = "replacement-workload" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			original := f.quiesce(t, f.history(t, syntheticWriter(1)))
			changed := original.DeepCopy()
			mutate(&changed)
			updates, revokes, observes := f.writes.updates, len(f.provider.revokes), len(f.provider.observes)
			if _, err := f.store.Reserve(context.Background(), changed, syntheticWriter(2)); err == nil {
				t.Fatal("mutated snapshot reserved a writer")
			}
			if _, err := f.manager.Quiesce(context.Background(), changed); err == nil {
				t.Fatal("mutated snapshot reached quiescence")
			}
			if f.writes.updates != updates || len(f.provider.revokes) != revokes || len(f.provider.observes) != observes ||
				!reflect.DeepEqual(f.read(t), original) {
				t.Fatal("snapshot mutation caused a side effect or aliased durable history")
			}
		})
	}
}

func TestWriterReservationIdentityDriftAndReuse(t *testing.T) {
	first := syntheticWriter(1)
	tests := map[string]func(*Writer){
		"reused Pod UID":     func(w *Writer) { w.Pod.UID = first.Pod.UID },
		"reused epoch":       func(w *Writer) { w.Epoch = first.Epoch },
		"workload UID drift": func(w *Writer) { w.Workload.Name = "renamed-workload" },
		"Node UID drift":     func(w *Writer) { w.Node.UID = first.Node.UID },
		"machine ID drift":   func(w *Writer) { w.MachineID = first.MachineID },
		"Node provider drift": func(w *Writer) {
			w.Node = first.Node
			w.Node.ProviderID = "example://changed"
			w.MachineID = first.MachineID
		},
		"Node machine drift":   func(w *Writer) { w.Node = first.Node },
		"volume UID drift":     func(w *Writer) { w.Storage.Volume.UID = first.Storage.Volume.UID },
		"volume handle drift":  func(w *Writer) { w.Storage = first.Storage; w.Storage.Handle = "other-handle" },
		"volume driver drift":  func(w *Writer) { w.Storage = first.Storage; w.Storage.Driver = "other.example.com" },
		"rebound backend":      func(w *Writer) { w.Storage.Handle = first.Storage.Handle },
		"empty machine":        func(w *Writer) { w.MachineID = "" },
		"provider as machine":  func(w *Writer) { w.MachineID = w.Node.ProviderID },
		"unbounded machine":    func(w *Writer) { w.MachineID = strings.Repeat("m", 257) },
		"ambiguous machine":    func(w *Writer) { w.MachineID = "machine\nincarnation" },
		"empty epoch":          func(w *Writer) { w.Epoch = "" },
		"unbounded epoch":      func(w *Writer) { w.Epoch = strings.Repeat("e", 129) },
		"empty Workload":       func(w *Writer) { w.Workload.UID = "" },
		"invalid Pod":          func(w *Writer) { w.Pod.Name = "invalid/name" },
		"empty Pod UID":        func(w *Writer) { w.Pod.UID = "" },
		"invalid Node":         func(w *Writer) { w.Node.Name = "invalid/name" },
		"empty Node UID":       func(w *Writer) { w.Node.UID = "" },
		"empty provider":       func(w *Writer) { w.Node.ProviderID = "" },
		"unbounded provider":   func(w *Writer) { w.Node.ProviderID = strings.Repeat("p", 2049) },
		"empty volume":         func(w *Writer) { w.Storage.Volume.UID = "" },
		"empty storage driver": func(w *Writer) { w.Storage.Driver = "" },
		"empty storage handle": func(w *Writer) { w.Storage.Handle = "" },
		"unbounded storage":    func(w *Writer) { w.Storage.Handle = strings.Repeat("h", 2049) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			previous := f.quiesce(t, f.history(t, first))
			candidate := syntheticWriter(2)
			mutate(&candidate)
			updates := f.writes.updates
			if _, err := f.store.Reserve(context.Background(), previous, candidate); !errors.Is(err, ErrInvalidHistory) {
				t.Fatalf("reused or invalid identity accepted: %v", err)
			}
			if f.writes.updates != updates || !reflect.DeepEqual(f.read(t), previous) {
				t.Fatal("invalid reservation changed history")
			}
		})
	}
}

func TestWriterReservationSingleCurrentWriterAndCAS(t *testing.T) {
	f := newWriterFixture(t)
	ctx := context.Background()
	quiescent := f.quiesce(t, f.history(t, syntheticWriter(1)))
	second, err := f.store.Reserve(ctx, quiescent, syntheticWriter(2))
	if err != nil {
		t.Fatal(err)
	}
	if second.UID != quiescent.UID || second.ResourceVersion == quiescent.ResourceVersion ||
		second.OperationID != "" || second.Phase != PhaseReserved || len(second.Writers) != 2 ||
		second.Writers[0] != quiescent.Writers[0] {
		t.Fatal("reservation did not append or invalidate the prior proof")
	}
	updates := f.writes.updates
	for _, pinned := range []Snapshot{quiescent, second} {
		if _, err := f.store.Reserve(ctx, pinned, syntheticWriter(3)); !apierrors.IsConflict(err) {
			t.Fatalf("stale/concurrent reservation did not conflict: %v", err)
		}
	}
	if f.writes.updates != updates || !reflect.DeepEqual(f.read(t), second) {
		t.Fatal("concurrent reservation retried or changed inventory")
	}
}

func TestWriterAmbiguousReservationRetainsPossibleWriter(t *testing.T) {
	f := newWriterFixture(t)
	ctx := context.Background()
	previous := f.quiesce(t, f.enroll(t))
	writer := syntheticWriter(1)
	f.writes.afterUpdate = func(context.Context, client.Object) error { return context.DeadlineExceeded }
	result, err := f.store.Reserve(ctx, previous, writer)
	if !errors.Is(err, context.DeadlineExceeded) || result.UID != "" {
		t.Fatalf("ambiguous reservation returned a confirmed snapshot: %+v %v", result, err)
	}
	f.writes.afterUpdate = nil
	stored := f.read(t)
	if stored.Phase != PhaseReserved || !reflect.DeepEqual(stored.Writers, []Writer{writer}) {
		t.Fatal("lost reservation response discarded a possible writer")
	}
	updates := f.writes.updates
	if _, err := f.store.Reserve(ctx, previous, writer); !apierrors.IsConflict(err) || f.writes.updates != updates {
		t.Fatalf("ambiguous reservation was retried against new history: %v", err)
	}
	f.quiesce(t, stored)
	if request := f.provider.revokes[len(f.provider.revokes)-1]; !reflect.DeepEqual(request.Writers, []Writer{writer}) {
		t.Fatal("later exclusion omitted the ambiguously reserved writer")
	}
}

func TestWriterReservationConflictBetweenReadAndWrite(t *testing.T) {
	for _, identity := range []string{"resource version", "UID"} {
		t.Run(identity, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.quiesce(t, f.enroll(t))
			f.writes.beforeUpdate = func(ctx context.Context, object client.Object) error {
				if object.GetUID() != pinned.UID || object.GetResourceVersion() != pinned.ResourceVersion {
					t.Fatal("update omitted the pinned UID/RV")
				}
				if identity == "UID" {
					object.SetUID("replacement-journal")
					return nil
				}
				current := f.object(t)
				current.Annotations["example.com/concurrent"] = "update"
				return f.raw.Update(ctx, current)
			}
			updates := f.writes.updates
			if _, err := f.store.Reserve(context.Background(), pinned, syntheticWriter(1)); !apierrors.IsConflict(err) {
				t.Fatalf("CAS did not fail: %v", err)
			}
			if f.writes.updates != updates+1 || len(f.read(t).Writers) != 0 {
				t.Fatal("conflict was retried against newer inventory")
			}
		})
	}
}

type reservationBarrierReader struct {
	client.Reader
	reads   atomic.Int32
	ready   chan struct{}
	release chan struct{}
}

func (r *reservationBarrierReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	if r.reads.Add(1) <= 2 {
		select {
		case r.ready <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestWriterConcurrentReservationsHaveOneWinner(t *testing.T) {
	f := newWriterFixture(t)
	pinned := f.quiesce(t, f.enroll(t))
	reader := &reservationBarrierReader{
		Reader: f.raw, ready: make(chan struct{}, 2), release: make(chan struct{}),
	}
	store := Store{Client: f.raw, APIReader: reader, Namespace: f.store.Namespace}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for index := 1; index <= 2; index++ {
		go func() {
			_, err := store.Reserve(ctx, pinned, syntheticWriter(index))
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-reader.ready:
		case <-ctx.Done():
			t.Fatal("concurrent reservations did not capture the same revision")
		}
	}
	close(reader.release)
	successes, conflicts := 0, 0
	for range 2 {
		select {
		case err := <-results:
			switch {
			case err == nil:
				successes++
			case apierrors.IsConflict(err):
				conflicts++
			default:
				t.Fatalf("unexpected reservation result: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent reservations did not finish")
		}
	}
	stored := f.read(t)
	if successes != 1 || conflicts != 1 || len(stored.Writers) != 1 || stored.Phase != PhaseReserved {
		t.Fatalf("concurrent reservations: successes=%d conflicts=%d snapshot=%+v", successes, conflicts, stored)
	}
}

func TestWriterHistoryCountAndByteBoundsNeverTruncate(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		f := newWriterFixture(t)
		writers := make([]Writer, MaxWriters)
		for index := range writers {
			writers[index] = syntheticWriter(index + 1)
		}
		full := f.quiesce(t, f.history(t, writers...))
		updates := f.writes.updates
		if len(full.Writers) != MaxWriters || !reflect.DeepEqual(full.Writers, writers) {
			t.Fatal("maximum history was not retained exactly")
		}
		if _, err := f.store.Reserve(context.Background(), full, syntheticWriter(MaxWriters+1)); !errors.Is(err, ErrInvalidHistory) {
			t.Fatalf("full history did not fail closed: %v", err)
		}
		if f.writes.updates != updates || !reflect.DeepEqual(f.read(t), full) {
			t.Fatal("full history was truncated or rewritten")
		}
	})
	t.Run("complete serialized object", func(t *testing.T) {
		f := newWriterFixture(t)
		f.quiesce(t, f.history(t, syntheticWriter(1)))
		object := f.object(t)
		object.Annotations["example.com/padding"] = ""
		raw, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		object.Annotations["example.com/padding"] = strings.Repeat("x", MaxJournalBytes-len(raw)-256)
		if err := f.raw.Update(context.Background(), object); err != nil {
			t.Fatal(err)
		}
		pinned := f.read(t)
		updates := f.writes.updates
		if _, err := f.store.Reserve(context.Background(), pinned, syntheticWriter(2)); !errors.Is(err, ErrInvalidHistory) {
			t.Fatalf("serialized byte limit was not enforced before write: %v", err)
		}
		if f.writes.updates != updates || !reflect.DeepEqual(f.read(t), pinned) {
			t.Fatal("metadata or history was discarded to fit the byte limit")
		}
	})
}

func TestWriterTerminatingJournalCanQuiesceButCannotReserve(t *testing.T) {
	f := newWriterFixture(t)
	f.history(t, syntheticWriter(1))
	if err := f.raw.Delete(context.Background(), f.object(t)); err != nil {
		t.Fatal(err)
	}
	if f.object(t).DeletionTimestamp.IsZero() {
		t.Fatal("fixture did not retain a terminating journal")
	}
	quiescent := f.quiesce(t, f.read(t))
	if _, err := f.store.Reserve(context.Background(), quiescent, syntheticWriter(2)); !apierrors.IsConflict(err) {
		t.Fatalf("terminating journal accepted a new reservation: %v", err)
	}
	if !reflect.DeepEqual(f.object(t).Finalizers, []string{JournalFinalizer}) {
		t.Fatal("quiescence released the durable history finalizer")
	}
}

func TestWriterStorageAndNodeResourceReusePreservesPhysicalIncarnations(t *testing.T) {
	f := newWriterFixture(t)
	first, second := syntheticWriter(1), syntheticWriter(2)
	second.Node.Name, second.Node.ProviderID = first.Node.Name, first.Node.ProviderID
	third := syntheticWriter(3)
	third.Node, third.MachineID, third.Storage = second.Node, second.MachineID, second.Storage
	pinned := f.history(t, first, second, third)
	quiescent := f.quiesce(t, pinned)
	request := f.provider.observes[len(f.provider.observes)-1]
	if !reflect.DeepEqual(request.Writers, []Writer{first, second, third}) ||
		!reflect.DeepEqual(quiescent.Writers, request.Writers) {
		t.Fatal("resource-name reuse collapsed old physical or storage identities")
	}
}
