// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSandboxSDKRefreshesJournalBeforeWorkloadBindingCreation(t *testing.T) {
	for _, mode := range []string{"absent", "deleted during preflight", "same-owner replacement"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			prepared, _, target := sandboxSDKKubernetesAccessPlans(t)
			cfg, api, releaseStore := sandboxSDKConfig()
			input := prepared.sandboxBundle.Identity()
			desired := []sandboxpolicy.BindingReference{}
			for _, object := range input.Controls.Objects {
				if object.GetKind() != "ClusterRoleBinding" {
					continue
				}
				binding := &rbacv1.ClusterRoleBinding{}
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, binding); err != nil {
					t.Fatal(err)
				}
				desired = append(desired, sandboxpolicy.BindingReference{
					Kind: binding.Kind, Name: binding.Name, AgentName: input.AgentName, AgentUID: input.Resolution.Binding.AgentUID,
					RoleRef: binding.RoleRef, ServiceAccountName: binding.Subjects[0].Name,
					ServiceAccountNamespace: binding.Subjects[0].Namespace,
					GrantRevision:           binding.Annotations[sandboxpolicy.GrantRevisionAnnotation],
				})
			}
			if len(desired) != 2 {
				t.Fatal("SDK fixture must contain two independent workload grants")
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			tracking := false
			var sequence []string
			base := fake.NewClientBuilder().WithScheme(scheme).Build()
			grantClient := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if _, ok := object.(*rbacv1.ClusterRoleBinding); !ok {
						return c.Get(ctx, key, object, options...)
					}
					probe := &unstructured.Unstructured{}
					probe.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding"))
					probe.SetName(key.Name)
					api.mu.Lock()
					defer api.mu.Unlock()
					current := api.objects[sandboxTestPath(probe)]
					if current == nil {
						return apierrors.NewNotFound(rbacv1.Resource("clusterrolebindings"), key.Name)
					}
					return runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, object)
				},
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
					object.SetUID("synthetic-sdk-journal")
					return c.Create(ctx, object, options...)
				},
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
					if tracking {
						cm := object.(*corev1.ConfigMap)
						var record struct {
							Bindings []sandboxpolicy.BindingReference
						}
						if err := json.Unmarshal([]byte(cm.Data[sandboxpolicy.GrantJournalDataKey]), &record); err != nil {
							return err
						}
						index := slices.IndexFunc(record.Bindings, func(ref sandboxpolicy.BindingReference) bool { return ref.Name == target.GetName() })
						switch {
						case index == -1:
							sequence = append(sequence, "retire")
						case record.Bindings[index].UID == "":
							sequence = append(sequence, "intent")
						default:
							sequence = append(sequence, "confirm")
						}
					}
					return c.Update(ctx, object, options...)
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return fmt.Errorf("journal refresh must not delete a live grant")
				},
			})
			journal := sandboxpolicy.GrantJournalStore{Client: grantClient, APIReader: grantClient, Namespace: "operator-state"}
			record := func(refs []sandboxpolicy.BindingReference) {
				t.Helper()
				if err := journal.Record(ctx, input.AgentName, input.Resolution.Binding.AgentUID, refs); err != nil {
					t.Fatal(err)
				}
			}
			check := func(ctx context.Context) error {
				return journal.RefreshBindingIntents(ctx, input.AgentName, input.Resolution.Binding.AgentUID, desired)
			}
			confirmed := func() []sandboxpolicy.BindingReference {
				t.Helper()
				refs := slices.Clone(desired)
				for i := range refs {
					binding := &rbacv1.ClusterRoleBinding{}
					if err := grantClient.Get(ctx, client.ObjectKey{Name: refs[i].Name}, binding); err != nil {
						t.Fatal(err)
					}
					refs[i].UID = binding.UID
				}
				return refs
			}
			record(desired)
			if err := stageSandboxTest(prepared, cfg, check); err != nil {
				t.Fatal(err)
			}
			record(confirmed())
			path := sandboxTestPath(target)
			oldUID := api.objects[path].GetUID()
			tracking = true
			releaseWrites := releaseStore.writes
			requests := len(api.requests)
			if mode == "absent" {
				delete(api.objects, path)
			} else if mode == "same-owner replacement" {
				api.objects[path].SetUID("unrelated-same-owner-binding")
			}
			deleted := false
			freshIntent := false
			api.before = func(call sandboxSDKRequest) {
				if mode == "deleted during preflight" && !deleted && call.method == http.MethodGet && call.path == path {
					delete(api.objects, path)
					deleted = true
				}
				if call.method == http.MethodPost && call.path == strings.TrimSuffix(path, "/"+target.GetName()) {
					refs, err := journal.Read(ctx, input.AgentName, input.Resolution.Binding.AgentUID)
					if err != nil {
						t.Fatal(err)
					}
					for _, ref := range refs {
						if ref.Name == target.GetName() && ref.UID == "" {
							freshIntent = true
						}
					}
					sequence = append(sequence, "create")
				}
			}
			err := stageSandboxTest(prepared, cfg, check)
			if mode == "same-owner replacement" {
				if !errors.Is(err, sandboxpolicy.ErrOwnership) || len(sequence) != 0 || releaseStore.writes != releaseWrites {
					t.Fatalf("same-name replacement reached a journal or Helm write: %v %v", err, sequence)
				}
				for _, call := range api.requests[requests:] {
					if call.method != http.MethodGet {
						t.Fatal("replacement rejection reached an HTTP mutation")
					}
				}
				return
			}
			if err != nil || !freshIntent || api.objects[path].GetUID() == oldUID {
				t.Fatalf("real Helm recreation lacked fresh durable intent: %v fresh=%v", err, freshIntent)
			}
			if mode == "deleted during preflight" && !deleted {
				t.Fatal("late deletion was not exercised at the Helm HTTP boundary")
			}
			record(confirmed())
			if !slices.Equal(sequence, []string{"retire", "intent", "create", "confirm"}) {
				t.Fatalf("unsafe journal/SDK mutation sequence: %v", sequence)
			}
			for _, call := range api.requests[requests:] {
				if call.method == http.MethodDelete {
					t.Fatal("recovering a missing grant deleted a live Helm resource")
				}
			}
		})
	}
}
