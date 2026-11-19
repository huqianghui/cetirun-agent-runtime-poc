// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSandboxEventsPreparationRoleFailures(t *testing.T) {
	for _, runtime := range []string{"openclaw", "hermes"} {
		for _, failure := range []string{"deleted", "UID changed", "version changed", "read failed"} {
			t.Run(runtime+"/"+failure, func(t *testing.T) {
				f := newPreparationFixture(t, runtime)
				role := f.makeAdmin(t)
				ctx, capture := capturePolicyEvents(t)
				request := ctrl.Request{NamespacedName: client.ObjectKey{Name: f.fixture.Agent.Name}}
				if _, err := f.reconciler.Reconcile(ctx, request); err != nil {
					t.Fatal(err)
				}
				if len(capture.events) != 0 {
					t.Fatal("ordinary held preparation emitted a policy failure or application")
				}
				stages := f.installer.stages
				agent := f.current(t)
				journal := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
				bindings, err := journal.Read(ctx, agent.Name, agent.UID)
				if err != nil || len(bindings) != 2 {
					t.Fatalf("missing prepared workload and source-read grants: %+v %v", bindings, err)
				}
				wantReason, wantStatus, wantError := "UnauthorizedBinding", metav1.ConditionFalse, sandboxpolicy.ErrUnapprovedGrant
				switch failure {
				case "deleted", "UID changed":
					if err := f.client.Delete(ctx, role); err != nil {
						t.Fatal(err)
					}
					if failure == "deleted" {
						wantReason = "ReferenceMissing"
					} else {
						role.UID, role.ResourceVersion = "synthetic-replacement-role", ""
						if err := f.client.Create(ctx, role); err != nil {
							t.Fatal(err)
						}
					}
				case "version changed":
					role.Labels = map[string]string{"example.com/approval-changed": "synthetic"}
					if err := f.client.Update(ctx, role); err != nil {
						t.Fatal(err)
					}
				case "read failed":
					wantReason, wantStatus, wantError = "ObservationUnavailable", metav1.ConditionUnknown, sandboxpolicy.ErrUnavailable
					f.reconciler.APIReader = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
							if _, ok := object.(*rbacv1.ClusterRole); ok {
								return errors.New("sk-dummy role reader details")
							}
							return c.Get(ctx, key, object, options...)
						},
					})
				}
				for range 3 {
					result, err := f.reconciler.Reconcile(ctx, request)
					if !errors.Is(err, wantError) || result.RequeueAfter != 0 {
						t.Fatalf("approval failure was hidden or converted to a clean hold: %+v %v", result, err)
					}
				}
				agent = f.current(t)
				condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
				if condition == nil || condition.Status != wantStatus || condition.Reason != wantReason ||
					condition.ObservedGeneration != agent.Generation || agent.Status.Sandbox != nil ||
					agent.Status.AccessURL != "" || f.installer.stages != stages ||
					!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionReady) ||
					!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionAgentConfigReady) ||
					!f.expectation(t).ExecutionHeld {
					t.Fatalf("failed preparation was not accurately classified and held: %+v", agent.Status)
				}
				if len(capture.events) != 1 {
					t.Fatalf("expected one committed preparation failure across retries, got %d", len(capture.events))
				}
				event := capture.events[0]
				if event["event"] != "sandbox.enforcement.failed" || event["reason"] != wantReason ||
					event["conditionStatus"] != string(wantStatus) || event["attemptedRevision"] != "unresolved" ||
					event["appliedRevision"] != "unresolved" || event["attemptedBindingSource"] != "authorization" {
					t.Fatalf("event disagreed with committed preparation status: %+v", event)
				}
				for _, sensitive := range []string{"sk-dummy", "role reader details", role.Name} {
					if strings.Contains(capture.raw[0], sensitive) {
						t.Fatalf("structured event exposed free-form preparation details: %s", sensitive)
					}
				}
				for _, binding := range bindings {
					if err := f.client.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Name}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
						t.Fatalf("approval failure retained a grant: %s: %v", binding.Name, err)
					}
				}
				remaining, err := journal.Read(ctx, agent.Name, agent.UID)
				if err != nil || len(remaining) != 0 {
					t.Fatalf("approval failure abandoned journal cleanup: %+v %v", remaining, err)
				}

				f.reconciler.APIReader = f.client
				if failure == "deleted" {
					role.UID, role.ResourceVersion = "synthetic-reapproved-role", ""
					if err := f.client.Create(ctx, role); err != nil {
						t.Fatal(err)
					}
				}
				platform := f.config.Platforms[api.SandboxKata]
				platform.Grants[0].RoleUID, platform.Grants[0].RoleResourceVersion = role.UID, role.ResourceVersion
				platform.Grants[0].Revision = "synthetic-refreshed-approval"
				f.config.Platforms[api.SandboxKata] = platform
				if _, err := f.reconciler.Reconcile(ctx, request); err != nil {
					t.Fatal("explicit reapproval did not recover held preparation:", err)
				}
				agent = f.current(t)
				if len(capture.events) != 1 || !apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionAgentConfigReady) ||
					agent.Status.Sandbox != nil || !f.expectation(t).ExecutionHeld {
					t.Fatal("recovered held preparation emitted an application or released execution")
				}
			})
		}
	}
}
