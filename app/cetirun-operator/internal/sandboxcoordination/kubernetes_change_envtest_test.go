// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxcoordination

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// These tests exercise real API defaulting, Update, CAS and recovery only.
// No runtime, admission deployment, CSI/CNI provider or model service runs.
func TestKubernetesConfigurationAPIServer(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("configuration adapter tests refuse an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("explicit preinstalled Kubernetes 1.37 API-server/etcd binaries are required")
		}
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		if stopErr := environment.Stop(); stopErr != nil {
			t.Error("stop failed local API startup:", stopErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	endpoint, err := url.Parse(config.Host)
	if err != nil || endpoint.Scheme != "https" ||
		(endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost" && endpoint.Hostname() != "::1") {
		t.Fatal("envtest must provide its own HTTPS loopback endpoint")
	}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	storage, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := storage.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "configuration-state"}}); err != nil {
		t.Fatal(err)
	}
	newFixture := func(t *testing.T, kind, suffix string) (Store, client.Object, client.Object) {
		t.Helper()
		name := strings.ToLower(kind) + "-" + suffix
		authority, err := NewState("configuration-state", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, authority); err != nil {
			t.Fatal(err)
		}
		before := configurationObject(kind)
		before.SetName(name)
		before.SetAnnotations(map[string]string{"example.com/change": "original"})
		if err := storage.Create(ctx, before); err != nil {
			t.Fatal(err)
		}
		if before.GetUID() == "" || before.GetResourceVersion() == "" || before.GetGeneration() < 1 {
			t.Fatal("API did not issue a persisted source identity")
		}
		return Store{Client: storage, APIReader: storage, Namespace: authority.Namespace,
			Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID}}, before, desiredConfiguration(before)
	}

	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, scenario := range []string{"removed", "terminating", "lost-reply", "source-race"} {
			t.Run("delete/"+kind+"/"+scenario, func(t *testing.T) {
				store, before, _ := newFixture(t, kind, "delete-"+scenario)
				if scenario == "terminating" {
					before.SetFinalizers([]string{"example.com/retain"})
					if err := storage.Update(ctx, before); err != nil {
						t.Fatal(err)
					}
				}
				deletes := 0
				writer := interceptor.NewClient(storage, interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
						deletes++
						if scenario == "source-race" {
							raced := before.DeepCopyObject().(client.Object)
							raced.SetLabels(map[string]string{"example.com/race": "changed"})
							if err := c.Update(ctx, raced); err != nil {
								t.Fatal(err)
							}
						}
						if err := c.Delete(ctx, object, opts...); err != nil {
							return err
						}
						if scenario == "lost-reply" {
							return errors.New("synthetic lost Delete reply")
						}
						return nil
					},
				})
				ticket, err := store.AcquireConfigurationDeletion(ctx, before)
				if err != nil {
					t.Fatal(err)
				}
				ticket, err = store.ReconcileChange(ctx, ticket, deletionAdapter(t, writer, storage, before))
				switch scenario {
				case "source-race":
					if !apierrors.IsConflict(err) {
						t.Fatal("real Delete lost its resourceVersion precondition:", err)
					}
				case "lost-reply":
					if err == nil || ticket.Change.Phase != Changing {
						t.Fatal("lost reply did not retain intent")
					}
				default:
					if err != nil {
						t.Fatal("conditional deletion failed:", err)
					}
				}
				if scenario == "source-race" || scenario == "lost-reply" {
					recovered, err := store.ReadConfigurationDeletion(ctx, before)
					if err != nil {
						t.Fatal(err)
					}
					observer := deletionAdapter(t, nil, storage, before)
					outcome, err := observer.ObserveChange(ctx, recovered)
					if err != nil {
						t.Fatal("definitive deletion outcome was not observed:", err)
					}
					if scenario == "source-race" {
						if !outcome.Aborted || !outcome.Superseded || outcome.Applied {
							t.Fatal("superseded Delete was reported as accepted:", outcome)
						}
						current := before.DeepCopyObject().(client.Object)
						if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil ||
							current.GetUID() != before.GetUID() || !current.GetDeletionTimestamp().IsZero() {
							t.Fatal("superseded conditional Delete changed the live source:", err)
						}
					} else if !outcome.Applied || outcome.Aborted || outcome.Superseded {
						t.Fatal("accepted Delete was not distinguished from supersession:", outcome)
					}
					if err := store.CheckChange(ctx, recovered); err != nil {
						t.Fatal("read-only observation released its reservation:", err)
					}
					if _, err := store.ReconcileChange(ctx, recovered, observer); err != nil {
						t.Fatal("definitive observation could not release its reservation:", err)
					}
					if err := store.CheckChange(ctx, recovered); err == nil {
						t.Fatal("completed recovery retained its reservation")
					}
				}
				if deletes != 1 {
					t.Fatal("Delete was resubmitted")
				}
				if scenario == "terminating" {
					current := before.DeepCopyObject().(client.Object)
					if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil ||
						current.GetDeletionTimestamp().IsZero() || len(current.GetFinalizers()) != 1 {
						t.Fatal("Delete bypassed independent finalization:", err)
					}
					cleaned := current.DeepCopyObject().(client.Object)
					cleaned.SetFinalizers(nil)
					if err := store.AuthorizeConfigurationFinalization(ctx, current, cleaned); err != nil {
						t.Fatal("accepted deletion blocked finalization:", err)
					}
				}
			})
		}
	}

	t.Run("delete/large-replacement", func(t *testing.T) {
		store, before, _ := newFixture(t, "Team", "large-replacement")
		ticket, err := store.AcquireConfigurationDeletion(ctx, before)
		if err != nil {
			t.Fatal(err)
		}
		deletes := 0
		writer := interceptor.NewClient(storage, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
				deletes++
				if err := c.Delete(ctx, object, opts...); err != nil {
					return err
				}
				return errors.New("synthetic lost Delete reply")
			},
		})
		ticket, err = store.ReconcileChange(ctx, ticket, deletionAdapter(t, writer, storage, before))
		if err == nil || ticket.Change.Phase != Changing {
			t.Fatal("lost reply did not retain deletion intent")
		}
		replacement := &api.Team{
			ObjectMeta: metav1.ObjectMeta{Name: before.GetName()},
			Spec: api.TeamSpec{AgentPolicyRef: "policy", SandboxRefs: []string{"sandbox"}, DefaultSandboxRef: "sandbox", MemberContext: api.TeamContextSpec{
				Role: strings.Repeat("x", 270000),
			}},
		}
		if err := storage.Create(ctx, replacement); err != nil {
			t.Fatal("API rejected valid replacement larger than the configuration image bound:", err)
		}
		recovered, err := store.ReadConfigurationDeletion(ctx, before)
		if err != nil {
			t.Fatal(err)
		}
		observer := deletionAdapter(t, nil, storage, before)
		outcome, err := observer.ObserveChange(ctx, recovered)
		if err != nil || !outcome.Removed {
			t.Fatal("replacement contents prevented observing the original UID's removal:", err)
		}
		if err := store.CheckChange(ctx, recovered); err != nil {
			t.Fatal("read-only observation released the reservation:", err)
		}
		if _, err := store.ReconcileChange(ctx, recovered, observer); err != nil {
			t.Fatal("replacement blocked observation-only recovery:", err)
		}
		if _, err := store.ReadConfigurationDeletion(ctx, before); !errors.Is(err, ErrUnavailable) {
			t.Fatal("completed deletion retained its reservation:", err)
		}
		current := &api.Team{}
		if err := storage.Get(ctx, client.ObjectKeyFromObject(replacement), current); err != nil ||
			current.UID != replacement.UID || current.ResourceVersion != replacement.ResourceVersion ||
			current.Spec.MemberContext.Role != replacement.Spec.MemberContext.Role {
			t.Fatal("recovery modified the replacement:", err)
		}
		if deletes != 1 {
			t.Fatal("recovery resubmitted Delete")
		}
	})

	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, scenario := range []string{"success", "lost-reply", "failed-request", "never-submitted"} {
			for _, recovery := range []string{"original-images", "journal"} {
				t.Run(kind+"/"+scenario+"/"+recovery, func(t *testing.T) {
					store, before, after := newFixture(t, kind, scenario+"-"+recovery)
					updates := 0
					writer := interceptor.NewClient(storage, interceptor.Funcs{
						Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
							updates++
							if scenario == "failed-request" {
								return errors.New("synthetic request did not reach the API")
							}
							if object.GetUID() != before.GetUID() || object.GetResourceVersion() != before.GetResourceVersion() {
								t.Fatal("adapter changed its original API preconditions")
							}
							if err := c.Update(ctx, object, opts...); err != nil {
								return err
							}
							if scenario == "lost-reply" {
								return errors.New("synthetic lost response after real API commit")
							}
							return nil
						},
					})
					ticket, err := store.AcquireConfigurationChange(ctx, before, after)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "never-submitted" {
						ticket, err = store.BeginChange(ctx, ticket)
						if err != nil {
							t.Fatal(err)
						}
					}
					ticket, err = store.ReconcileChange(ctx, ticket, configurationAdapter(t, writer, storage, before, after))
					if scenario == "success" {
						if err != nil || updates != 1 {
							t.Fatalf("real Update failed: updates=%d error=%v", updates, err)
						}
					} else {
						if err == nil || ticket.Change.Phase != Changing {
							t.Fatal("uncertain operation lost its submission intent:", err)
						}
						restartedClient, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
						if err != nil {
							t.Fatal(err)
						}
						restarted := Store{Client: restartedClient, APIReader: restartedClient, Namespace: store.Namespace, Identity: store.Identity}
						var recovered ChangeTicket
						var adapter ChangeAdapter
						if recovery == "original-images" {
							recovered, err = restarted.AcquireConfigurationChange(ctx, before, after)
							adapter = configurationAdapter(t, writer, restartedClient, before, after)
						} else {
							current := configurationObject(kind)
							if err := restartedClient.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
								t.Fatal(err)
							}
							var found bool
							recovered, found, err = restarted.ReadConfigurationUpdate(ctx, current)
							if err != nil || !found {
								t.Fatal("journal recovery lookup failed:", err)
							}
							adapter, err = NewKubernetesUpdateRecoveryAdapter(restartedClient, recovered, current)
						}
						if err != nil || recovered.Change.ID != ticket.Change.ID || recovered.Change.Phase != Changing {
							t.Fatal("exact recovery failed:", err)
						}
						_, err = restarted.ReconcileChange(ctx, recovered, adapter)
						if scenario != "lost-reply" {
							if err == nil {
								t.Fatal("unchanged source was inferred aborted")
							}
							if err := store.CheckChange(ctx, ticket); err != nil {
								t.Fatal("unchanged source stopped excluding other writes:", err)
							}
							expected := 1
							if scenario == "never-submitted" {
								expected = 0
							}
							if updates != expected {
								t.Fatal("recovery retried an uncertain request")
							}
							return
						}
						if err != nil || updates != 1 {
							t.Fatal("real committed write was not recovered without resubmission:", err)
						}
					}
					current := before.DeepCopyObject().(client.Object)
					if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
						t.Fatal(err)
					}
					if current.GetUID() != before.GetUID() || current.GetResourceVersion() == before.GetResourceVersion() ||
						current.GetGeneration() != before.GetGeneration()+1 {
						t.Fatal("real API did not preserve UID and advance resourceVersion/generation")
					}
					if err := storage.Update(ctx, after.DeepCopyObject().(client.Object)); !apierrors.IsConflict(err) {
						t.Fatal("old-RV delayed request could mutate an observed committed version:", err)
					}
				})
			}
		}
	}

	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		t.Run(kind+"/schema-defaults", func(t *testing.T) {
			store, before, after := newFixture(t, kind, "defaults")
			switch object := after.(type) {
			case *api.Sandbox:
				object.Spec.Type = ""
			case *api.AgentPolicy:
				object.Spec.Security.ReadOnlyRootFilesystem = nil
				object.Spec.Network.Ingress.Mode = ""
				object.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8080}}
				object.Spec.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
					{CIDR: "203.0.113.0/24", Ports: []api.AgentPolicyPort{{Port: 8443}}},
				}
			}
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReconcileChange(ctx, ticket,
				configurationAdapter(t, storage, storage, before, after)); err != nil {
				t.Fatal("valid API-defaulted update could not complete:", err)
			}
			if _, err := store.ReadConfigurationChange(ctx, before, after); !errors.Is(err, ErrUnavailable) {
				t.Fatal("completed defaulted update retained its reservation:", err)
			}
		})
	}

	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		for _, order := range []string{"consumer-first", "change-first"} {
			t.Run(kind+"/effective-metadata/"+order, func(t *testing.T) {
				store, before, _ := newFixture(t, kind, "metadata-"+order)
				capture := func() (Intent, *unstructured.Unstructured) {
					t.Helper()
					current := &unstructured.Unstructured{}
					current.SetGroupVersionKind(api.GroupVersion.WithKind(kind))
					if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
						t.Fatal(err)
					}
					effective, err := w.ExecutionObjectDigest(current)
					if err != nil {
						t.Fatal(err)
					}
					if kind == "AgentPolicy" {
						for _, path := range [][]string{
							{"sandboxSelection", "allowMultiple"}, {"entraID", "enabled"}, {"network", "ingress", "requireMTLS"},
						} {
							if value, found, err := unstructured.NestedBool(current.Object, append([]string{"spec"}, path...)...); err != nil || !found || value {
								t.Fatal("unstructured GET must retain API-defaulted false values:", err)
							}
						}
						for _, field := range []string{"blockedCIDRs", "blockedFQDNs", "blockedPorts", "allowedInternalCIDRs"} {
							if values, found, err := unstructured.NestedSlice(current.Object, "spec", "network", field); err != nil || !found || len(values) != 0 {
								t.Fatal("unstructured GET must retain API-defaulted empty arrays:", err)
							}
						}
					}
					reference := w.SourceReference{APIVersion: current.GetAPIVersion(), Kind: current.GetKind(),
						Name: current.GetName(), UID: current.GetUID(), ResourceVersion: current.GetResourceVersion()}
					intent := testIntent("metadata")
					for i, source := range intent.Sources {
						if source.Kind == kind {
							delete(intent.SourceDigests, w.SourceKey(source))
							intent.Sources[i] = reference
							intent.SourceDigests[w.SourceKey(reference)] = effective
						}
					}
					if kind == "Sandbox" {
						intent.Expected.Binding.SandboxUID = current.GetUID()
					} else {
						intent.Expected.Binding.AgentPolicyUID = current.GetUID()
					}
					return intent, current
				}
				intent, _ := capture()
				var execution Ticket
				if order == "consumer-first" {
					execution = acquire(t, store, intent)
				}
				after := before.DeepCopyObject().(client.Object)
				after.SetLabels(map[string]string{"example.com/description": "updated"})
				after.SetAnnotations(map[string]string{"example.com/description": "updated"})
				ticket, err := store.AcquireConfigurationChange(ctx, before, after)
				if err != nil {
					t.Fatal("metadata-only update blocked the API-captured execution:", err)
				}
				if order == "change-first" {
					intent, _ = capture()
					execution = acquire(t, store, intent)
				}
				if _, err := store.ReconcileChange(ctx, ticket,
					configurationAdapter(t, storage, storage, before, after)); err != nil {
					t.Fatal("actual metadata Update did not complete:", err)
				}
				observed, current := capture()
				if current.GetUID() != before.GetUID() || current.GetResourceVersion() == before.GetResourceVersion() ||
					current.GetAnnotations()["example.com/description"] != "updated" {
					t.Fatal("metadata update was not persisted")
				}
				if observed.SourceDigests[w.SourceKey(ticket.Change.Before)] != ticket.Change.UnchangedDigest {
					t.Fatal("typed update scope diverged from the authoritative execution digest")
				}
				if err := store.Check(ctx, execution); err != nil {
					t.Fatal("actual metadata Update changed execution ownership:", err)
				}
				if kind == "AgentPolicy" {
					policy := &api.AgentPolicy{}
					if err := storage.Get(ctx, client.ObjectKeyFromObject(before), policy); err != nil {
						t.Fatal(err)
					}
					for _, control := range []string{"boolean", "array"} {
						changed := policy.DeepCopy()
						if control == "boolean" {
							changed.Spec.SandboxSelection.AllowMultiple = true
						} else {
							changed.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
						}
						if _, err := store.AcquireConfigurationChange(ctx, policy, changed); !errors.Is(err, ErrConflict) {
							t.Fatal("effective control change bypassed the API-captured execution:", err)
						}
					}
				}
			})
		}
	}

	for _, scenario := range []string{"source-race", "source-replaced", "metadata-mismatch", "status-advanced"} {
		t.Run(scenario, func(t *testing.T) {
			store, before, after := newFixture(t, "Team", scenario)
			ticket := changingConfiguration(t, store, before, after)
			updates := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					updates++
					if scenario == "source-race" {
						raced := before.DeepCopyObject().(client.Object)
						raced.SetLabels(map[string]string{"example.com/race": "changed"})
						if err := c.Update(ctx, raced); err != nil {
							t.Fatal(err)
						}
					}
					return c.Update(ctx, object, opts...)
				},
			})
			adapter := configurationAdapter(t, writer, storage, before, after)
			if scenario == "source-replaced" {
				uid, rv := before.GetUID(), before.GetResourceVersion()
				if err := storage.Delete(ctx, before, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
					t.Fatal(err)
				}
				replacement := configurationObject("Team")
				replacement.SetName(before.GetName())
				if err := storage.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }); !errors.Is(err, ErrConflict) || updates != 0 {
					t.Fatal("replacement source reached Update:", err)
				}
			} else {
				err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) })
				if scenario == "source-race" {
					if !apierrors.IsConflict(err) || updates != 1 {
						t.Fatal("API CAS conflict was retried or hidden:", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					current := &api.Team{}
					if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
						t.Fatal(err)
					}
					if scenario == "metadata-mismatch" {
						current.Annotations["example.com/unapproved"] = "different"
						err = storage.Update(ctx, current)
					} else {
						current.Status.ObservedGeneration = current.Generation
						err = storage.Status().Update(ctx, current)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := store.ReconcileChange(ctx, ticket, configurationAdapter(t, nil, storage, before, after))
			if scenario == "status-advanced" {
				if err != nil {
					t.Fatal("read-only status evolution hid matching desired content:", err)
				}
			} else {
				if err == nil {
					t.Fatal("replacement/mismatched source was considered committed")
				}
				if err := store.CheckChange(ctx, ticket); err != nil {
					t.Fatal("unknown result released its reservation:", err)
				}
			}
		})
	}
}
