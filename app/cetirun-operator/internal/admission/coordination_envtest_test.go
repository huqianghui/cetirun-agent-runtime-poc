// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestConfigurationCoordinationAPIServer(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("configuration coordination tests refuse an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, name))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("explicit preinstalled API-server/etcd binaries are required")
		}
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
	}
	connection, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		api.AddToScheme, corev1.AddToScheme, admissionregistrationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := client.NewWithWatch(connection, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := storage.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "operator-test"}}); err != nil {
		t.Fatal(err)
	}
	state, err := sandboxcoordination.NewState("operator-test", "configuration-coordination")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, state); err != nil {
		t.Fatal(err)
	}
	before := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "coordinated-policy"}, Spec: api.AgentPolicySpec{
		Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}},
	}}
	if err := storage.Create(ctx, before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
	config := testConfig()
	config.AdminGroups = []string{"system:masters"}
	config.CoordinationConfigMapName, config.CoordinationConfigMapUID = state.Name, string(state.UID)
	handler, err := NewHandler(storage, config)
	if err != nil {
		t.Fatal(err)
	}
	handler.registrations = newFixture(t).handler.registrations
	mux := http.NewServeMux()
	mux.Handle(ValidationPath, &admission.Webhook{Handler: admission.HandlerFunc(handler.Validate)})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	validating, _ := renderedWebhooks(t, certificate)
	for i := range validating.Webhooks {
		hook := &validating.Webhooks[i]
		url := server.URL + *hook.ClientConfig.Service.Path
		hook.ClientConfig.Service, hook.ClientConfig.URL = nil, &url
	}
	if err := storage.Create(ctx, validating); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		err := storage.Update(ctx, after.DeepCopy(), client.DryRunAll)
		return err != nil && strings.Contains(err.Error(), "CoordinationRequired"), nil
	}); err != nil {
		t.Fatal("served configuration coordination gate did not become ready:", err)
	}
	if err := storage.Update(ctx, after.DeepCopy()); err == nil {
		t.Fatal("direct configuration update bypassed the served reservation gate")
	}
	store := sandboxcoordination.Store{
		Client: storage, APIReader: storage, Namespace: state.Namespace,
		Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID},
	}
	ticket, err := store.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Update(ctx, after.DeepCopy()); err == nil {
		t.Fatal("a reserved but unsubmitted change authorized a source write")
	}
	var updates atomic.Int32
	lostReply := interceptor.NewClient(storage, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			updates.Add(1)
			if err := c.Update(ctx, object, options...); err != nil {
				return err
			}
			return errors.New("synthetic lost reply after committed update")
		},
	})
	adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(lostReply, storage, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileChange(ctx, ticket, adapter); err == nil {
		t.Fatal("fixture did not lose the update response")
	}
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion == before.ResourceVersion || len(current.Spec.Network.BlockedCIDRs) != 1 ||
		current.Spec.Network.BlockedCIDRs[0] != "203.0.113.0/24" {
		t.Fatal("coordinated mutation was not committed through the real admission path")
	}
	recovered, err := store.ReadConfigurationChange(ctx, before, after)
	if err != nil || recovered.Change.Phase != sandboxcoordination.Changing {
		t.Fatal("durable submission intent did not survive the lost response:", err)
	}
	if _, err := store.ReconcileChange(ctx, recovered, adapter); err != nil {
		t.Fatal("committed update did not recover by observation:", err)
	}
	if updates.Load() != 1 {
		t.Fatal("recovery replayed an already submitted configuration write")
	}
	if err := store.CheckChange(ctx, recovered); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("observed completed mutation retained its reservation:", err)
	}

	t.Run("schema-defaulted protocol", func(t *testing.T) {
		original := current.DeepCopy()
		proposed := original.DeepCopy()
		proposed.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8080}}
		ticket, err := store.AcquireConfigurationChange(ctx, original, proposed)
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(storage, storage, original, proposed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
			t.Fatal("schema defaults invalidated the exact coordinated write:", err)
		}
		if err := storage.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			t.Fatal(err)
		}
		if len(current.Spec.Network.BlockedPorts) != 1 ||
			current.Spec.Network.BlockedPorts[0].Protocol != corev1.ProtocolTCP {
			t.Fatal("defaulted protocol did not persist through real admission")
		}
		if _, err := store.ReadConfigurationChange(ctx, original, proposed); !errors.Is(err, sandboxcoordination.ErrUnavailable) {
			t.Fatal("completed defaulted write retained a reservation:", err)
		}
	})

	testConfigurationDeletionThroughAdmission(t, ctx, storage, store)

	for _, scenario := range []string{"unreserved", "reserved", "changing"} {
		t.Run("finalization/"+scenario, func(t *testing.T) {
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{
				Name: "finalization-" + scenario, Finalizers: []string{"example.com/cleanup", "example.com/retain"},
			}, Spec: before.Spec}
			if err := storage.Create(ctx, policy); err != nil {
				t.Fatal(err)
			}
			var pending sandboxcoordination.ChangeTicket
			if scenario != "unreserved" {
				proposed := policy.DeepCopy()
				proposed.Spec.Network.BlockedCIDRs = []string{"198.51.100.0/24"}
				pending, err = store.AcquireConfigurationChange(ctx, policy, proposed)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "changing" {
					pending, err = store.BeginChange(ctx, pending)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario != "unreserved" {
				if err := storage.Delete(ctx, policy); err == nil {
					t.Fatal("deletion bypassed an outstanding update reservation")
				}
				if err := store.CheckChange(ctx, pending); err != nil {
					t.Fatal("rejected deletion changed the outstanding operation:", err)
				}
				return
			}
			deleting, err := store.AcquireConfigurationDeletion(ctx, policy)
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := sandboxcoordination.NewKubernetesDeleteAdapter(storage, storage, policy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReconcileChange(ctx, deleting, adapter); err != nil {
				t.Fatal("coordinated delete acceptance failed:", err)
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			cleaned := policy.DeepCopy()
			cleaned.Finalizers = []string{"example.com/retain"}
			tampered := cleaned.DeepCopy()
			tampered.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
			if err := storage.Update(ctx, tampered); err == nil {
				t.Fatal("finalization allowed a concurrent spec change")
			}
			originalState := &corev1.ConfigMap{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(state), originalState); err != nil {
				t.Fatal(err)
			}
			if err := storage.Update(ctx, cleaned.DeepCopy(), client.DryRunAll); err != nil {
				t.Fatal("authorized finalizer dry run failed:", err)
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil || len(policy.Finalizers) != 2 {
				t.Fatal("dry-run finalization changed the source:", err)
			}
			if err := storage.Update(ctx, cleaned); err != nil {
				t.Fatal("authorized finalizer removal failed:", err)
			}
			cleaned.Finalizers = nil
			if err := storage.Update(ctx, cleaned); err != nil {
				t.Fatal("last finalizer removal failed:", err)
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(cleaned), &api.AgentPolicy{}); !apierrors.IsNotFound(err) {
				t.Fatal("configuration deletion did not finish:", err)
			}
			currentState := &corev1.ConfigMap{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(state), currentState); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(originalState, currentState) {
				t.Fatal("finalization changed authority instead of checking it read-only")
			}
		})
	}

	state.Data[sandboxcoordination.DataKey] = "{}"
	if err := storage.Update(ctx, state); err == nil {
		t.Fatal("fixture unexpectedly updated a stale authority version")
	}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
		t.Fatal(err)
	}
	state.Data[sandboxcoordination.DataKey] = "{}"
	if err := storage.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	changed := current.DeepCopy()
	changed.Spec.Network.BlockedCIDRs = []string{"198.51.100.0/24"}
	if err := storage.Update(ctx, changed); err == nil {
		t.Fatal("corrupt authority allowed a direct configuration update")
	}
	current.Status.ObservedGeneration = current.Generation
	if err := storage.Status().Update(ctx, current); err != nil {
		t.Fatal("coordination source loss blocked an independent status update:", err)
	}
	if err := storage.Delete(ctx, current); err == nil {
		t.Fatal("corrupt coordination authority permitted a configuration deletion")
	}
}

func testConfigurationDeletionThroughAdmission(t *testing.T, ctx context.Context, storage client.WithWatch, store sandboxcoordination.Store) {
	t.Helper()
	f := newFixture(t)
	supportBox := f.box.DeepCopy()
	supportBox.ObjectMeta = metav1.ObjectMeta{Name: "deletion-support-sandbox"}
	supportPolicy := f.policy.DeepCopy()
	supportPolicy.ObjectMeta = metav1.ObjectMeta{Name: "deletion-support-policy"}
	for _, object := range []client.Object{supportBox, supportPolicy} {
		if err := storage.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	teamSpec := f.team.Spec
	teamSpec.AgentPolicyRef = supportPolicy.Name
	teamSpec.SandboxRefs, teamSpec.DefaultSandboxRef = []string{supportBox.Name}, supportBox.Name
	for _, before := range []client.Object{
		&api.Team{ObjectMeta: metav1.ObjectMeta{Name: "delete-team"}, Spec: teamSpec},
		&api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "delete-sandbox"}, Spec: f.box.Spec},
		&api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "delete-policy"}, Spec: f.policy.Spec},
	} {
		t.Run(before.GetName(), func(t *testing.T) {
			if err := storage.Create(ctx, before); err != nil {
				t.Fatal(err)
			}
			uid, rv := before.GetUID(), before.GetResourceVersion()
			opts := []client.DeleteOption{
				client.Preconditions{UID: &uid, ResourceVersion: &rv},
				client.PropagationPolicy(metav1.DeletePropagationBackground),
			}
			if err := storage.Delete(ctx, before, opts...); err == nil {
				t.Fatal("unreserved Delete bypassed served admission")
			}
			ticket, err := store.AcquireConfigurationDeletion(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Delete(ctx, before, opts...); err == nil {
				t.Fatal("Reserved phase authorized a Delete")
			}
			deletes := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					deletes++
					if err := c.Delete(ctx, object, options...); err != nil {
						return err
					}
					return errors.New("synthetic lost deletion response")
				},
			})
			adapter, err := sandboxcoordination.NewKubernetesDeleteAdapter(writer, storage, before)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err = store.ReconcileChange(ctx, ticket, adapter)
			if err == nil || ticket.Change.Phase != sandboxcoordination.Changing || deletes != 1 {
				t.Fatal("lost Delete reply did not retain submission intent:", err)
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), before.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
				t.Fatal("Delete was not accepted through the real webhook:", err)
			}
			replacement := before.DeepCopyObject().(client.Object)
			replacement.SetUID("")
			replacement.SetResourceVersion("")
			replacement.SetGeneration(0)
			replacement.SetCreationTimestamp(metav1.Time{})
			if err := storage.Create(ctx, replacement); err != nil {
				t.Fatal(err)
			}
			recovered, err := store.ReadConfigurationDeletion(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			observer, err := sandboxcoordination.NewKubernetesDeleteAdapter(nil, storage, before)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReconcileChange(ctx, recovered, observer); err != nil {
				t.Fatal("lost deletion response failed observation-only recovery:", err)
			}
			if err := storage.Delete(ctx, before, opts...); err == nil {
				t.Fatal("delayed old Delete was allowed to affect a replacement")
			}
			actual := replacement.DeepCopyObject().(client.Object)
			if err := storage.Get(ctx, client.ObjectKeyFromObject(replacement), actual); err != nil || actual.GetUID() != replacement.GetUID() {
				t.Fatal("replacement disappeared or changed:", err)
			}
			if deletes != 1 {
				t.Fatal("recovery resubmitted Delete")
			}
		})
	}
}
