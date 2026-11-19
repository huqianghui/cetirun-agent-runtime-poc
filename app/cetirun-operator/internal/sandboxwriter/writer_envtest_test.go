// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxwriter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// Unlike writerTestClient, this forwards UID/RV checks to the real API server.
type apiWriterClient struct {
	client.Client
	updates      int
	beforeUpdate func(context.Context, client.Object) error
}

func (*apiWriterClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("TEST: writes must use a separate authoritative reader")
}

func (c *apiWriterClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	c.updates++
	if c.beforeUpdate != nil {
		if err := c.beforeUpdate(ctx, object); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, object, opts...)
}

type writerReplyTransport func(*http.Request) (*http.Response, error)

func (f writerReplyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type apiWriterFixture struct {
	ctx      context.Context
	raw      client.Client
	writes   *apiWriterClient
	store    Store
	manager  *Manager
	provider *writerTestProvider
	lifetime Lifetime
	now      time.Time
}

func newAPIWriterFixture(t *testing.T, writer, reader client.Client, name string) *apiWriterFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	state := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "writer-state-" + name}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "writer-home-" + name}}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "writer-" + name},
		Spec: api.AgentSpec{
			Agent: api.HarnessSpec{
				Type: api.AgentTypeHermes, Image: ptr.To("registry.example.com/agent:1"), Port: ptr.To[int32](18789),
			},
			User: api.UserSpec{Alias: "writer-test", DisplayName: "Example User"},
			Team: "example-team", SandboxRef: "standard",
			Helm: api.HelmSpec{Version: "1.0.0"}, GatewayTokenSecretRef: "synthetic-runtime-secret",
		},
	}
	home := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: namespace.Name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: ptr.To(""),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	for _, object := range []client.Object{state, namespace, agent, home} {
		if err := writer.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
		if object.GetUID() == "" || object.GetResourceVersion() == "" {
			t.Fatal("API server did not assign a real UID/resource version")
		}
	}
	f := &apiWriterFixture{
		ctx: ctx, raw: writer, writes: &apiWriterClient{Client: writer},
		lifetime: Lifetime{
			Agent:     sandboxworkload.ObjectIdentity{Name: agent.Name, UID: agent.UID},
			Namespace: sandboxworkload.ObjectIdentity{Name: namespace.Name, UID: namespace.UID},
			Home:      sandboxworkload.ObjectIdentity{Name: home.Name, UID: home.UID},
		},
		now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	f.store = Store{Client: f.writes, APIReader: reader, Namespace: state.Name}
	f.provider = &writerTestProvider{observe: func(_ context.Context, request Request) (Observation, error) {
		return syntheticObservation(request, f.now), nil
	}}
	f.manager = &Manager{Store: f.store, Provider: f.provider, Now: func() time.Time { return f.now }, MaxAge: time.Minute}
	enrolled, err := f.store.Enroll(ctx, f.lifetime, "synthetic-complete-birth-"+name)
	if err != nil {
		t.Fatal(err)
	}
	object := f.journal(t)
	expectedOwner := sandboxworkload.AgentOwner(agent.Name, agent.UID)
	expectedOwner.BlockOwnerDeletion = ptr.To(false)
	if enrolled.UID != object.UID || enrolled.ResourceVersion != object.ResourceVersion ||
		!reflect.DeepEqual(object.OwnerReferences, []metav1.OwnerReference{expectedOwner}) {
		t.Fatal("enrollment did not bind the API-assigned journal and Agent identities")
	}
	return f
}

func (f *apiWriterFixture) journal(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	object := &corev1.ConfigMap{}
	if err := f.store.APIReader.Get(f.ctx, client.ObjectKey{Namespace: f.store.Namespace, Name: JournalName(f.lifetime)}, object); err != nil {
		t.Fatal(err)
	}
	return object
}

func (f *apiWriterFixture) read(t *testing.T) Snapshot {
	t.Helper()
	snapshot, err := f.store.Read(f.ctx, f.lifetime)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *apiWriterFixture) quiesce(t *testing.T) Snapshot {
	t.Helper()
	snapshot, err := f.manager.Quiesce(f.ctx, f.read(t))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *apiWriterFixture) reserve(t *testing.T, index int) Snapshot {
	t.Helper()
	snapshot, err := f.store.Reserve(f.ctx, f.read(t), syntheticWriter(index))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// Only API persistence is real here. No operator, scheduler, kubelet, CSI driver
// or physical fencing provider runs; provider assertions remain synthetic.
func TestWriterJournalAPIServerDurability(t *testing.T) {
	t.Setenv("USE_EXISTING_CLUSTER", "false")
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("set KUBEBUILDER_ASSETS to preinstalled Kubernetes 1.37 envtest binaries")
		}
	}
	raw, err := os.ReadFile("../../config/crd/bases/cetirun.microsoft.com_agents.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &definition); err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDs:                     []*apiextensionsv1.CustomResourceDefinition{&definition},
		ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 30 * time.Second,
	}
	environment.ControlPlane.GetAPIServer().Configure().Set("bind-address", "127.0.0.1")
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		if stopErr := environment.Stop(); stopErr != nil {
			t.Errorf("stop failed envtest startup: %v", stopErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop isolated writer API server: %v", err)
		}
	})
	endpoint, err := url.Parse(config.Host)
	if err != nil || endpoint.Scheme != "https" || !net.ParseIP(endpoint.Hostname()).IsLoopback() {
		t.Fatal("envtest must supply its own HTTPS loopback API endpoint")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	newClient := func(t *testing.T, config *rest.Config) client.Client {
		t.Helper()
		result, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	newFixture := func(t *testing.T, name string) *apiWriterFixture {
		t.Helper()
		return newAPIWriterFixture(t, newClient(t, config), newClient(t, config), name)
	}
	restart := func(t *testing.T, f *apiWriterFixture, intent Snapshot) (*Manager, *writerTestProvider) {
		t.Helper()
		provider := &writerTestProvider{
			revoke: func(context.Context, Request) error {
				t.Fatal("a fresh manager resubmitted a persisted operation")
				return nil
			},
			observe: func(_ context.Context, request Request) (Observation, error) {
				if !reflect.DeepEqual(request, requestOf(intent)) {
					t.Fatal("restart changed the journal UID, operation, enrollment or full historical request")
				}
				return syntheticObservation(request, f.now), nil
			},
		}
		return &Manager{
			Store:    Store{Client: newClient(t, config), APIReader: newClient(t, config), Namespace: f.store.Namespace},
			Provider: provider, Now: func() time.Time { return f.now }, MaxAge: time.Minute,
		}, provider
	}

	t.Run("retained history does not block foreground Agent finalization", func(t *testing.T) {
		f := newFixture(t, "foreground")
		f.quiesce(t)
		history := f.reserve(t, 1)
		agent := &api.Agent{}
		key := client.ObjectKey{Name: f.lifetime.Agent.Name}
		if err := f.raw.Get(f.ctx, key, agent); err != nil {
			t.Fatal(err)
		}
		if err := f.raw.Delete(f.ctx, agent, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
			t.Fatal(err)
		}
		if err := f.raw.Get(f.ctx, key, agent); err != nil ||
			!slices.Contains(agent.Finalizers, metav1.FinalizerDeleteDependents) {
			t.Fatal("API server did not start foreground deletion:", err)
		}
		journal := f.journal(t)
		if len(journal.OwnerReferences) != 1 || journal.OwnerReferences[0].UID != agent.UID ||
			ptr.Deref(journal.OwnerReferences[0].BlockOwnerDeletion, true) {
			t.Fatal("retained journal blocks foreground Agent deletion")
		}
		// envtest has no garbage collector. Simulate its dependent DELETE
		// and completion only after checking the non-blocking owner edge.
		if err := f.raw.Delete(f.ctx, journal, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			t.Fatal(err)
		}
		quiescent := f.quiesce(t)
		if quiescent.UID != history.UID || !reflect.DeepEqual(quiescent.Writers, history.Writers) {
			t.Fatal("dependent deletion discarded historical writers")
		}
		agent.Finalizers = slices.DeleteFunc(agent.Finalizers, func(value string) bool {
			return value == metav1.FinalizerDeleteDependents
		})
		if err := f.raw.Update(f.ctx, agent); err != nil {
			t.Fatal(err)
		}
		if err := f.raw.Get(f.ctx, key, &api.Agent{}); !apierrors.IsNotFound(err) {
			t.Fatal("Agent did not complete simulated foreground finalization:", err)
		}
		journal = f.journal(t)
		if journal.UID != history.UID || journal.DeletionTimestamp.IsZero() ||
			!slices.Contains(journal.Finalizers, JournalFinalizer) {
			t.Fatal("Agent deletion removed protected historical state")
		}
		if err := f.manager.CheckQuiesced(f.ctx, f.lifetime); err != nil {
			t.Fatal("historical cleanup required the deleted Agent:", err)
		}
		if _, err := f.store.Reserve(f.ctx, f.read(t), syntheticWriter(2)); !apierrors.IsConflict(err) {
			t.Fatal("terminating history allowed a replacement writer:", err)
		}
	})

	t.Run("real resource-version conflict does not retry or append", func(t *testing.T) {
		f := newFixture(t, "rv-cas")
		pinned := f.quiesce(t)
		f.writes.updates = 0
		f.writes.beforeUpdate = func(ctx context.Context, object client.Object) error {
			if object.GetUID() != pinned.UID || object.GetResourceVersion() != pinned.ResourceVersion {
				t.Fatal("Store did not send the pinned UID and resource version")
			}
			concurrent := f.journal(t)
			concurrent.Annotations["example.com/concurrent"] = "update"
			return f.raw.Update(ctx, concurrent)
		}
		if _, err := f.store.Reserve(f.ctx, pinned, syntheticWriter(1)); !apierrors.IsConflict(err) {
			t.Fatalf("real API server did not reject the stale resource version: %v", err)
		}
		current := f.read(t)
		if f.writes.updates != 1 || current.UID != pinned.UID || current.ResourceVersion == pinned.ResourceVersion ||
			!reflect.DeepEqual(recordOf(current), recordOf(pinned)) {
			t.Fatal("CAS conflict retried or changed the durable history")
		}
		f.writes.beforeUpdate = nil
		reserved, err := f.store.Reserve(f.ctx, current, syntheticWriter(1))
		if err != nil || reserved.Phase != PhaseReserved || reserved.UID != pinned.UID ||
			reserved.ResourceVersion == current.ResourceVersion || len(reserved.Writers) != 1 {
			t.Fatalf("fresh UID/RV reservation did not commit and read back: %+v %v", reserved, err)
		}
		if _, err := f.store.Reserve(f.ctx, current, syntheticWriter(2)); !apierrors.IsConflict(err) || f.writes.updates != 2 {
			t.Fatalf("another reservation reused the previously quiescent revision: %v", err)
		}
	})

	t.Run("real UID precondition rejects same-name journal replacement", func(t *testing.T) {
		f := newFixture(t, "uid-cas")
		pinned := f.quiesce(t)
		f.writes.updates = 0
		var replacement *corev1.ConfigMap
		f.writes.beforeUpdate = func(ctx context.Context, outgoing client.Object) error {
			if outgoing.GetUID() != pinned.UID || outgoing.GetResourceVersion() != pinned.ResourceVersion {
				t.Fatal("Store did not send both original CAS identities")
			}
			original := f.journal(t)
			replacement = original.DeepCopy()
			replacement.ObjectMeta = metav1.ObjectMeta{
				Name: original.Name, Namespace: original.Namespace, Annotations: original.Annotations,
				OwnerReferences: original.OwnerReferences, Finalizers: []string{JournalFinalizer},
			}
			// Privileged corruption is injected only in this disposable API.
			original.Finalizers = nil
			if err := f.raw.Update(ctx, original); err != nil {
				return err
			}
			if err := f.raw.Delete(ctx, original, client.Preconditions{UID: &original.UID}); err != nil {
				return err
			}
			if err := f.raw.Create(ctx, replacement); err != nil {
				return err
			}
			if replacement.UID == pinned.UID {
				t.Fatal("API server reused the deleted journal's UID")
			}
			// Isolate UID enforcement: even the replacement's current RV must
			// not make the outgoing, old-UID update acceptable to the API.
			outgoing.SetResourceVersion(replacement.ResourceVersion)
			return nil
		}
		_, err := f.store.Reserve(f.ctx, pinned, syntheticWriter(1))
		if (!apierrors.IsConflict(err) && !apierrors.IsInvalid(err)) ||
			!strings.Contains(strings.ToLower(err.Error()), "uid") {
			t.Fatalf("real API server did not enforce the journal UID: %v", err)
		}
		current := f.read(t)
		if f.writes.updates != 1 || replacement == nil || current.UID != replacement.UID ||
			current.ResourceVersion != replacement.ResourceVersion || !reflect.DeepEqual(recordOf(current), recordOf(pinned)) {
			t.Fatal("old-UID update changed or adopted the replacement")
		}
		f.writes.beforeUpdate = nil
		if _, err := f.store.Reserve(f.ctx, pinned, syntheticWriter(2)); !apierrors.IsConflict(err) || f.writes.updates != 1 {
			t.Fatalf("stale snapshot adopted a recreated real API object: %v", err)
		}
	})

	t.Run("revocation intent CAS conflict never reaches the provider", func(t *testing.T) {
		f := newFixture(t, "intent-cas")
		f.quiesce(t)
		pinned := f.reserve(t, 1)
		f.writes.updates = 0
		f.provider.revokes, f.provider.observes = nil, nil
		f.writes.beforeUpdate = func(ctx context.Context, object client.Object) error {
			if object.GetUID() != pinned.UID || object.GetResourceVersion() != pinned.ResourceVersion {
				t.Fatal("revocation intent did not pin both journal identities")
			}
			concurrent := f.journal(t)
			concurrent.Annotations["example.com/concurrent"] = "intent-race"
			return f.raw.Update(ctx, concurrent)
		}
		if _, err := f.manager.Quiesce(f.ctx, pinned); !apierrors.IsConflict(err) {
			t.Fatalf("real API revocation CAS conflict was hidden: %v", err)
		}
		current := f.read(t)
		if f.writes.updates != 1 || len(f.provider.revokes)+len(f.provider.observes) != 0 ||
			current.UID != pinned.UID || current.ResourceVersion == pinned.ResourceVersion ||
			!reflect.DeepEqual(recordOf(current), recordOf(pinned)) {
			t.Fatal("failed intent retried, reached the provider or changed writer history")
		}
	})

	t.Run("lost API intent reply survives fresh-client restart without submission", func(t *testing.T) {
		f := newFixture(t, "lost-intent")
		f.quiesce(t)
		f.reserve(t, 1)
		f.quiesce(t)
		pinned := f.reserve(t, 2)
		f.provider.revokes, f.provider.observes = nil, nil
		lostConfig := rest.CopyConfig(config)
		journalPath := "/api/v1/namespaces/" + f.store.Namespace + "/configmaps/" + JournalName(f.lifetime)
		var puts, dropped atomic.Int32
		lostConfig.Wrap(func(base http.RoundTripper) http.RoundTripper {
			return writerReplyTransport(func(request *http.Request) (*http.Response, error) {
				target := request.Method == http.MethodPut && request.URL.Path == journalPath
				if target {
					puts.Add(1)
				}
				response, err := base.RoundTrip(request)
				if err == nil && target && response.StatusCode >= 200 && response.StatusCode < 300 && dropped.CompareAndSwap(0, 1) {
					if err := response.Body.Close(); err != nil {
						return nil, err
					}
					// The API committed, but its successful reply never
					// reaches the Kubernetes client or Store.
					return nil, context.DeadlineExceeded
				}
				return response, err
			})
		})
		f.store.Client = newClient(t, lostConfig)
		f.manager.Store = f.store
		if _, err := f.manager.Quiesce(f.ctx, pinned); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost successful API reply was not reported as ambiguous: %v", err)
		}
		intent := f.read(t)
		if puts.Load() != 1 || dropped.Load() != 1 || len(f.provider.revokes)+len(f.provider.observes) != 0 ||
			intent.Phase != PhaseRevoking || intent.UID != pinned.UID || intent.ResourceVersion == pinned.ResourceVersion ||
			intent.OperationID == "" || !reflect.DeepEqual(intent.Writers, pinned.Writers) {
			t.Fatal("ambiguous API write did not preserve the exact observe-only intent")
		}
		restarted, provider := restart(t, f, intent)
		complete := provider.observe
		provider.observe = func(ctx context.Context, request Request) (Observation, error) {
			proof, err := complete(ctx, request)
			proof.Barrier = BarrierEvidence{}
			return proof, err
		}
		if err := restarted.CheckQuiesced(f.ctx, f.lifetime); !errors.Is(err, ErrUnproven) ||
			len(provider.revokes) != 0 || f.read(t).Phase != PhaseRevoking {
			t.Fatalf("persisted but unsubmitted intent became exclusion evidence: %v", err)
		}
		provider.observe = complete
		if err := restarted.CheckQuiesced(f.ctx, f.lifetime); err != nil {
			t.Fatal(err)
		}
		quiescent := f.read(t)
		if len(provider.revokes) != 0 || len(provider.observes) != 2 || quiescent.Phase != PhaseQuiescent ||
			quiescent.OperationID != intent.OperationID || !reflect.DeepEqual(quiescent.Writers, pinned.Writers) {
			t.Fatal("fresh-client recovery resubmitted or lost durable history")
		}
	})

	t.Run("lost Revoke reply survives fresh-manager restart and reobservation", func(t *testing.T) {
		f := newFixture(t, "lost-revoke")
		f.quiesce(t)
		f.reserve(t, 1)
		f.quiesce(t)
		pinned := f.reserve(t, 2)
		f.provider.revokes, f.provider.observes = nil, nil
		f.provider.revoke = func(ctx context.Context, request Request) error {
			persisted, err := f.store.Read(ctx, f.lifetime)
			if err != nil || persisted.Phase != PhaseRevoking || persisted.ResourceVersion == pinned.ResourceVersion ||
				!reflect.DeepEqual(requestOf(persisted), request) {
				t.Fatalf("Revoke preceded exact API intent persistence: %v", err)
			}
			return context.DeadlineExceeded
		}
		if _, err := f.manager.Quiesce(f.ctx, pinned); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost provider reply was hidden: %v", err)
		}
		intent := f.read(t)
		if intent.Phase != PhaseRevoking || len(f.provider.revokes) != 1 || len(f.provider.observes) != 0 {
			t.Fatal("lost Revoke reply was resubmitted or accepted as evidence")
		}
		restarted, provider := restart(t, f, intent)
		if err := restarted.CheckQuiesced(f.ctx, f.lifetime); err != nil {
			t.Fatal(err)
		}
		quiescent := f.read(t)
		if quiescent.Phase != PhaseQuiescent || quiescent.OperationID != intent.OperationID ||
			!reflect.DeepEqual(quiescent.Writers, pinned.Writers) || len(provider.revokes) != 0 || len(provider.observes) != 1 {
			t.Fatal("restart did not exclusively observe the persisted operation")
		}
		again, nextProvider := restart(t, f, intent)
		if err := again.CheckQuiesced(f.ctx, f.lifetime); err != nil || len(nextProvider.revokes) != 0 ||
			len(nextProvider.observes) != 1 || !reflect.DeepEqual(f.read(t), quiescent) {
			t.Fatalf("later restart treated a quiescent phase as cached proof: %v", err)
		}
	})
}
