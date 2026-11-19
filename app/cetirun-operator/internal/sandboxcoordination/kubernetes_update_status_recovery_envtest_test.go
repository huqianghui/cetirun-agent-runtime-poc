// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxcoordination

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestKubernetesUpdateStatusRecoveryAPIServer uses an isolated API server to
// prove that a Team status write invalidates an in-flight full-object update,
// and that the durable update record can safely be released without replaying
// that stale request.
func TestKubernetesUpdateStatusRecoveryAPIServer(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("status recovery test refuses an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("explicit preinstalled Kubernetes 1.37 API-server/etcd binaries are required")
		}
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		UseExistingCluster:    ptr.To(false),
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
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
	if err := storage.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "status-recovery-state"}}); err != nil {
		t.Fatal(err)
	}

	for _, boundary := range []string{"before-authoritative-read", "before-source-update"} {
		t.Run(boundary, func(t *testing.T) {
			t.Helper()
			authority, err := NewState("status-recovery-state", "team-"+boundary)
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Create(ctx, authority); err != nil {
				t.Fatal(err)
			}
			before := configurationObject("Team").(*api.Team)
			before.Name = "team-" + boundary
			before.Annotations = map[string]string{"example.com/change": "original"}
			if err := storage.Create(ctx, before); err != nil {
				t.Fatal(err)
			}
			if before.UID == "" || before.ResourceVersion == "" {
				t.Fatal("API did not assign a persisted Team identity")
			}
			after := desiredConfiguration(before).(*api.Team)
			store := Store{Client: storage, APIReader: storage, Namespace: authority.Namespace,
				Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID}}

			statusWrites, teamUpdateAttempts := 0, 0
			statusRefresh := func(c client.WithWatch) error {
				current := &api.Team{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
					return err
				}
				current.Status.ObservedGeneration = current.Generation
				current.Status.Compatibility = &api.TeamCompatibilityStatus{
					State:      api.CompatibilityCompatible,
					Team:       api.CompatibilityReference{Name: current.Name, UID: string(current.UID), Generation: current.Generation},
					CheckedAt:  metav1.NewTime(time.Now().UTC()),
					ValidUntil: metav1.NewTime(time.Now().UTC().Add(time.Minute)),
				}
				statusWrites++
				return c.Status().Update(ctx, current)
			}
			refreshed := false
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if configMap, ok := object.(*corev1.ConfigMap); ok && boundary == "before-authoritative-read" && !refreshed {
						var state struct {
							Changes []struct {
								Phase Phase `json:"phase"`
							} `json:"changes"`
						}
						if err := json.Unmarshal([]byte(configMap.Data[DataKey]), &state); err != nil {
							t.Fatal(err)
						}
						if len(state.Changes) == 1 && state.Changes[0].Phase == Changing {
							if err := c.Update(ctx, object, opts...); err != nil {
								return err
							}
							refreshed = true
							return statusRefresh(c)
						}
					}
					if _, ok := object.(*api.Team); ok {
						teamUpdateAttempts++
						if boundary == "before-source-update" && !refreshed {
							refreshed = true
							if err := statusRefresh(c); err != nil {
								return err
							}
						}
					}
					return c.Update(ctx, object, opts...)
				},
			})
			store.Client = writer
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err = store.ReconcileChange(ctx, ticket, configurationAdapter(t, writer, storage, before, after))
			if err == nil || ticket.Change.Phase != Changing || !refreshed || statusWrites != 1 {
				t.Fatalf("initial conditional update was not fenced by one status refresh: ticket=%+v refreshed=%v statusWrites=%d err=%v", ticket, refreshed, statusWrites, err)
			}
			current := &api.Team{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
				t.Fatal(err)
			}
			if current.ResourceVersion == before.ResourceVersion || current.Spec.DefaultSandboxRef != before.Spec.DefaultSandboxRef || current.Status.Compatibility == nil {
				t.Fatalf("status update did not preserve the original writable Team: %+v", current)
			}

			restartedClient, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
			if err != nil {
				t.Fatal(err)
			}
			restarted := Store{Client: restartedClient, APIReader: restartedClient, Namespace: store.Namespace, Identity: store.Identity}
			recovered, found, err := restarted.ReadConfigurationUpdate(ctx, current)
			if err != nil || !found || recovered.Change.ID != ticket.Change.ID || recovered.Change.Phase != Changing {
				t.Fatalf("restarted Store did not recover the exact Changing update: ticket=%+v found=%v err=%v", recovered, found, err)
			}
			observer, err := NewKubernetesUpdateRecoveryAdapter(restartedClient, recovered, current)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := observer.ObserveChange(ctx, recovered)
			if err != nil || !outcome.Aborted || !outcome.Superseded || outcome.Applied || !outcome.NoPendingSubmissions {
				t.Fatalf("status-only refresh was not identified as a safe supersession: outcome=%+v err=%v", outcome, err)
			}
			if _, err := restarted.ReconcileChange(ctx, recovered, observer); err != nil {
				t.Fatal("safe supersession did not release its reservation:", err)
			}
			if err := restarted.CheckChange(ctx, recovered); err == nil {
				t.Fatal("released superseded update remained authorized")
			}
			if err := storage.Update(ctx, after.DeepCopy()); !apierrors.IsConflict(err) {
				t.Fatalf("delayed stale Team update was not rejected by Kubernetes CAS: %v", err)
			}

			freshBefore := &api.Team{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), freshBefore); err != nil {
				t.Fatal(err)
			}
			freshAfter := desiredConfiguration(freshBefore).(*api.Team)
			freshTicket, err := restarted.AcquireConfigurationChange(ctx, freshBefore, freshAfter)
			if err != nil {
				t.Fatal("fresh update could not reserve after supersession recovery:", err)
			}
			if _, err := restarted.ReconcileChange(ctx, freshTicket, configurationAdapter(t, restartedClient, restartedClient, freshBefore, freshAfter)); err != nil {
				t.Fatal("fresh update could not commit after supersession recovery:", err)
			}
			updated := &api.Team{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), updated); err != nil {
				t.Fatal(err)
			}
			if updated.Spec.DefaultSandboxRef != freshAfter.Spec.DefaultSandboxRef || updated.Status.Compatibility == nil ||
				updated.Status.Compatibility.State != api.CompatibilityCompatible {
				t.Fatalf("fresh update did not apply its spec while preserving the status observation: %+v", updated)
			}
			expectedAttempts := 0
			if boundary == "before-source-update" {
				expectedAttempts = 1
			}
			if teamUpdateAttempts != expectedAttempts {
				t.Fatalf("got %d Team update attempts, want %d", teamUpdateAttempts, expectedAttempts)
			}
		})
	}
}
