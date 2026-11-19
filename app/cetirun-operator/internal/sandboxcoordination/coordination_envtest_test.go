// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxcoordination

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Real API persistence/CAS only. There are no Pods, containers, model calls,
// runtime providers, CSI/CNI adapters or existing-cluster connections here.
func TestCoordinationAPIServer(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("coordination tests refuse an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if info, err := os.Stat(filepath.Join(assets, name)); assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("explicit preinstalled API-server/etcd binaries are required")
		}
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	storage, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := storage.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "operator-state"}}); err != nil {
		t.Fatal(err)
	}
	newStore := func(t *testing.T, name string) Store {
		t.Helper()
		object, err := NewState("operator-state", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
		if object.UID == "" || object.ResourceVersion == "" {
			t.Fatal("API did not supply an immutable UID and resource version")
		}
		return Store{Client: storage, APIReader: storage, Namespace: object.Namespace,
			Identity: w.ObjectIdentity{Name: object.Name, UID: object.UID}}
	}

	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run("formatted-managed-fields-"+kind, func(t *testing.T) {
			store := newStore(t, "fields-"+strings.ToLower(kind))
			before := configurationObject(kind)
			before.SetName("fields-" + strings.ToLower(kind))
			if err := storage.Create(ctx, before, client.FieldOwner("configuration-test")); err != nil {
				t.Fatal(err)
			}
			if len(before.GetManagedFields()) == 0 || before.GetManagedFields()[0].FieldsV1 == nil {
				t.Fatal("API fixture did not contain a server-issued field set")
			}
			after := desiredConfiguration(before)
			for _, object := range []client.Object{before, after} {
				raw, err := json.MarshalIndent(object, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, object); err != nil {
					t.Fatal(err)
				}
			}
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal("formatted API image could not reserve its exact update:", err)
			}
			adapter := configurationAdapter(t, storage, storage, before, after)
			if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
				t.Fatal("formatted API image could not complete its guarded update:", err)
			}
			_, state, err := store.read(ctx)
			if err != nil || len(state.Changes) != 0 {
				t.Fatal("completed API update retained its reservation:", err)
			}
		})
	}

	t.Run("native-start-versus-change-real-CAS", func(t *testing.T) {
		testNativeCASRace(t, newStore(t, "native-concurrent"), storage)
	})

	t.Run("committed-Team-mutation-recovers-after-lost-reply", func(t *testing.T) {
		store := newStore(t, "team-recovery")
		before := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "team"},
			Spec: api.TeamSpec{AgentPolicyRef: "policy",
				SandboxRefs: []string{"sandbox", "spare"}, DefaultSandboxRef: "sandbox"}}
		if err := storage.Create(ctx, before); err != nil {
			t.Fatal(err)
		}
		acquire(t, store, teamIntent(t, before, "normal"))
		after := before.DeepCopy()
		after.Spec.DefaultSandboxRef = "spare"
		ticket, err := store.AcquireTeamChange(ctx, before, after)
		if err != nil {
			t.Fatal(err)
		}
		var committed *api.Team
		adapter := &syntheticChangeAdapter{
			submit: func(ctx context.Context, _ ChangeTicket) error {
				committed = after.DeepCopy()
				if err := storage.Update(ctx, committed); err != nil {
					return err
				}
				return errors.New("synthetic lost reply after actual API write")
			},
			outcome: func(ticket ChangeTicket) *ChangeOutcome {
				outcome := completeChange(ticket)
				outcome.After.ResourceVersion = committed.ResourceVersion
				return outcome
			},
		}
		if _, err := store.ReconcileChange(ctx, ticket, adapter); err == nil ||
			committed == nil || committed.ResourceVersion == before.ResourceVersion {
			t.Fatal("fixture did not lose a reply after an actual committed write")
		}
		reader, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		restarted := Store{Client: reader, APIReader: reader, Namespace: store.Namespace, Identity: store.Identity}
		recovered, err := restarted.AcquireTeamChange(ctx, before, after)
		if err != nil || recovered.Change.ID != ticket.Change.ID || recovered.Change.Phase != Changing {
			t.Fatal("advanced Team resourceVersion prevented exact recovery:", err)
		}
		if _, err := restarted.ReconcileChange(ctx, recovered, adapter); err != nil || adapter.submissions != 1 {
			t.Fatal("actual API mutation was resubmitted:", err)
		}
	})

	t.Run("real-CAS-excludes-start-versus-change", func(t *testing.T) {
		store := newStore(t, "concurrent")
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
		results := make(chan error, 2)
		var group sync.WaitGroup
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
			t.Fatalf("real API allowed %d winners", successes)
		}
	})

	t.Run("restart-after-lost-intent-write-reply", func(t *testing.T) {
		store := newStore(t, "lost-reply")
		ticket := acquire(t, store, testIntent("one"))
		uncertain := store
		uncertain.Client = interceptor.NewClient(storage, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
				if err := c.Update(ctx, object, opts...); err != nil {
					return err
				}
				return errors.New("synthetic lost response after committed API write")
			},
		})
		if _, err := uncertain.BeginStart(ctx, ticket); err == nil {
			t.Fatal("lost response authorized Start")
		}
		recovered, err := store.ReadExecution(ctx, ticket.Execution.Intent.Agent.UID)
		if err != nil || recovered.Execution.Phase != Starting {
			t.Fatal("restart lost committed intent:", err)
		}
		if _, err := store.BeginStart(ctx, recovered); !errors.Is(err, ErrConflict) {
			t.Fatal("restart authorized resubmission:", err)
		}
	})

	t.Run("authority-name-reuse-does-not-recover", func(t *testing.T) {
		store := newStore(t, "replacement")
		acquire(t, store, testIntent("one"))
		object := &corev1.ConfigMap{}
		key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
		if err := storage.Get(ctx, key, object); err != nil {
			t.Fatal(err)
		}
		object.Finalizers = nil
		if err := storage.Update(ctx, object); err != nil {
			t.Fatal(err)
		}
		if err := storage.Delete(ctx, object, client.Preconditions{UID: &object.UID, ResourceVersion: &object.ResourceVersion}); err != nil {
			t.Fatal(err)
		}
		replacement, err := NewState(key.Namespace, key.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		if replacement.UID == store.Identity.UID {
			t.Fatal("test did not replace authority")
		}
		if _, err := store.AcquireExecution(ctx, testIntent("two")); !errors.Is(err, ErrUnavailable) {
			t.Fatal("same-name replacement was adopted:", err)
		}
	})
}
