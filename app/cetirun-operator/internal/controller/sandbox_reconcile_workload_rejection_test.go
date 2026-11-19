// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxWorkloadRejectionFences(t *testing.T) {
	for _, scenario := range []string{"rejected", "earlier-submission", "new-claim", "write-failure", "lost-ack", "timeout", "lost-response"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := firstInstallKubernetesFixture(t, "openclaw", withContainerRuntime(t))
			installer := f.r.HelmClient.(*sharedSandboxInstaller)
			injected := errors.New("synthetic outcome write failure")
			installer.beforeApply = func(prepared *helm.PreparedRelease) error {
				if scenario == "earlier-submission" {
					if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
						return err
					}
				}
				complete, err := prepared.AuthorizeSandboxWorkloadCreate(ctx)
				if err != nil {
					return err
				}
				if scenario == "new-claim" {
					if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
						return err
					}
				}
				if scenario == "write-failure" || scenario == "lost-ack" {
					f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
						Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
							raw, err := patch.Data(object)
							if err != nil {
								return err
							}
							var body struct{ Metadata metav1.ObjectMeta }
							if err := json.Unmarshal(raw, &body); err != nil {
								return err
							}
							if body.Metadata.Annotations[sandboxApplyStateAnnotation] == sandboxApplyCancelled {
								if scenario == "write-failure" {
									return injected
								}
								return errors.Join(c.Patch(ctx, object, patch, opts...), injected)
							}
							return c.Patch(ctx, object, patch, opts...)
						},
					})
				}
				createErr := error(apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "statefulsets"},
					"synthetic", errors.New("synthetic rejection")))
				if scenario == "timeout" {
					createErr = apierrors.NewTimeoutError("synthetic uncertain submission", 0)
				} else if scenario == "lost-response" {
					createErr = io.ErrUnexpectedEOF
				}
				result := complete(createErr)
				if scenario == "new-claim" && (result == nil || !strings.Contains(result.Error(), "object was modified")) {
					t.Fatal("old rejection did not lose the current-claim race:", result)
				}
				if (scenario == "write-failure" || scenario == "lost-ack") && !errors.Is(result, injected) {
					t.Fatal("rejection checkpoint failure was hidden:", result)
				}
				return result
			}
			for range 5 {
				if _, err := f.reconcile(); err != nil {
					break
				}
			}
			expected := reconciledExpectationForTest(t, f)
			state := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace,
				Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}, state); err != nil {
				t.Fatal(err)
			}
			want := sandboxApplySubmitted
			if scenario == "rejected" || scenario == "lost-ack" {
				want = sandboxApplyCancelled
			}
			if state.Annotations[sandboxApplyStateAnnotation] != want {
				t.Fatalf("rejection changed another or uncertain submission: got %s want %s",
					state.Annotations[sandboxApplyStateAnnotation], want)
			}
		})
	}
}

func TestReconciledSandboxFirstWorkloadRejectionRecovery(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		for _, operation := range []string{"retry", "delete"} {
			t.Run(string(backend)+"/"+operation, func(t *testing.T) {
				ctx := context.Background()
				var setup []func(*AgentReconciler, client.Client)
				if backend == api.SandboxContainer {
					setup = append(setup, withContainerRuntime(t))
				}
				f := firstInstallKubernetesFixture(t, "openclaw", setup...)
				installer := f.r.HelmClient.(*sharedSandboxInstaller)
				original := installer.kube
				var attempted *helm.PreparedRelease
				installer.beforeApply = func(prepared *helm.PreparedRelease) error {
					attempted = prepared
					return nil
				}
				grants, rejected := 0, false
				installer.kube = interceptor.NewClient(f.kube, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
						kind := object.GetObjectKind().GroupVersionKind().Kind
						if kind == "StatefulSet" {
							rejected = true
							return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "statefulsets"},
								object.GetName(), fmt.Errorf("synthetic temporary admission denial"))
						}
						err := c.Create(ctx, object, opts...)
						if err == nil && (kind == "RoleBinding" || kind == "ClusterRoleBinding") {
							grants++
						}
						return err
					},
				})
				var failed error
				for range 5 {
					_, failed = f.reconcile()
					if failed != nil {
						break
					}
				}
				if !rejected || !apierrors.IsForbidden(failed) || grants < 2 {
					t.Fatalf("first workload was not rejected after successful grants: grants=%d err=%v", grants, failed)
				}
				expected := reconciledExpectationForTest(t, f)
				state := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace,
					Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}, state); err != nil ||
					state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled {
					t.Fatal("definitive non-creation did not cancel the initial apply:", err)
				}
				if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID); err != nil || found {
					t.Fatal("definitive workload rejection stranded its reservation:", err)
				}
				if err := attempted.AuthorizeSandboxApply(ctx); err == nil {
					t.Fatal("cancelled writer retained apply authority")
				}
				installer.kube = original
				if operation == "delete" {
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
				}
				for range 5 {
					f.reconcileOK(t)
				}
				if operation == "delete" {
					if err := f.kube.Get(ctx, client.ObjectKey{Name: f.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("normal deletion remains blocked:", err)
					}
					if _, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID); !apierrors.IsNotFound(err) {
						t.Fatal("normal deletion retained grants:", err)
					}
					return
				}
				assertKubernetesFixtureGrants(t, f, true)
				current := reconciledExpectationForTest(t, f)
				if current.Agent != expected.Agent || current.Namespace != expected.Namespace || current.Home != expected.Home ||
					f.installer.stages != 1 {
					t.Fatal("retry replaced the owned lifetime or applied more than once")
				}
			})
		}
	}
}
