// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
)

type policyEventCapture struct {
	events []map[string]any
	raw    []string
}

func capturePolicyEvents(t *testing.T) (context.Context, *policyEventCapture) {
	t.Helper()
	capture := &policyEventCapture{}
	logger := funcr.NewJSON(func(raw string) {
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] != nil {
			capture.events = append(capture.events, event)
			capture.raw = append(capture.raw, raw)
		}
	}, funcr.Options{})
	return log.IntoContext(context.Background(), logger), capture
}

func TestSandboxEventsCommittedConfigurationTransitions(t *testing.T) {
	injected := errors.New("sk-dummy provider error at https://provider.example.com/synthetic-private")
	for _, tc := range []struct {
		name, reason string
		mutate       func(*api.AgentPolicy, *controller.SandboxConfigurationSnapshot)
		sourceError  error
		wantError    bool
		status       metav1.ConditionStatus
	}{
		{"accepted", "PolicyAccepted", func(*api.AgentPolicy, *controller.SandboxConfigurationSnapshot) {}, nil, false, metav1.ConditionTrue},
		{"invalid", "InvalidConfiguration", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.Network.BlockedFQDNs = []string{"sk-dummy invalid endpoint https://policy.example.com/"}
		}, nil, false, metav1.ConditionFalse},
		{"unsupported", "UnsupportedControl", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.Network.BlockedFQDNs = []string{"sk-dummy.policy.example.com"}
		}, nil, false, metav1.ConditionFalse},
		{"conflict", "PolicyConflict", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53}}
		}, nil, false, metav1.ConditionFalse},
		{"grant", "UnapprovedGrant", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "synthetic-admin", ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: "synthetic-admin"}},
			}
		}, nil, false, metav1.ConditionFalse},
		{"unavailable", "PrerequisitesUnavailable", func(_ *api.AgentPolicy, s *controller.SandboxConfigurationSnapshot) {
			s.Platforms = nil
		}, nil, true, metav1.ConditionUnknown},
		{"source error", "PrerequisitesUnavailable", func(*api.AgentPolicy, *controller.SandboxConfigurationSnapshot) {}, injected, true, metav1.ConditionUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, snapshot, _ := sandboxConfigurationFixture()
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{
				Name: "event-policy", UID: "00000000-0000-4000-8000-000000000004", Generation: 6,
			}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
			tc.mutate(policy, &snapshot)
			ctx, capture := capturePolicyEvents(t)
			writes := 0
			backing := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(policy).WithObjects(policy).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
						writes++
						if len(capture.events) != 0 {
							t.Fatal("configuration event preceded status persistence or unchanged status was rewritten")
						}
						err := c.SubResource(subresource).Patch(ctx, object, patch, options...)
						if len(capture.events) != 0 {
							t.Fatal("configuration event preceded writer success")
						}
						return err
					},
				}).Build()
			r := &controller.AgentPolicyReconciler{
				Client: backing, APIReader: backing, Now: func() time.Time { return snapshot.ObservedAt },
				Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
					return snapshot, tc.sourceError
				}),
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
			for range 3 {
				_, err := r.Reconcile(ctx, request)
				if (err != nil) != tc.wantError || tc.sourceError != nil && !errors.Is(err, tc.sourceError) {
					t.Fatalf("configuration event changed assessment error semantics: %v", err)
				}
			}
			if writes != 1 || len(capture.events) != 1 {
				t.Fatalf("unchanged retry emitted or wrote more than once: writes=%d events=%d", writes, len(capture.events))
			}
			if err := backing.Get(ctx, request.NamespacedName, policy); err != nil {
				t.Fatal(err)
			}
			condition := apimeta.FindStatusCondition(policy.Status.Conditions, api.ConditionAgentPolicyAccepted)
			if condition == nil || condition.Status != tc.status || condition.Reason != tc.reason {
				t.Fatalf("configuration event disagreed with committed status: %+v", condition)
			}
			event := capture.events[0]
			expectedType := "sandbox.policy.rejected"
			if tc.status == metav1.ConditionTrue {
				expectedType = "sandbox.policy.accepted"
			}
			unresolved := map[string]any{
				"agentUID": "unresolved", "teamUID": "unresolved", "sandboxUID": "unresolved",
				"agentPolicyUID": "unresolved",
			}
			attempted := map[string]any{
				"agentUID": "unresolved", "teamUID": "unresolved", "sandboxUID": "unresolved",
				"agentPolicyUID": string(policy.UID),
			}
			if event["event"] != expectedType || event["reason"] != tc.reason ||
				event["conditionStatus"] != string(tc.status) || event["condition"] != api.ConditionAgentPolicyAccepted ||
				event["resourceKind"] != "AgentPolicy" || event["resourceUID"] != string(policy.UID) ||
				event["generation"] != float64(policy.Generation) || event["attemptedBindingSource"] != "configuration" ||
				!reflect.DeepEqual(event["attemptedBinding"], attempted) || !reflect.DeepEqual(event["appliedBinding"], unresolved) ||
				event["attemptedRevision"] != "unresolved" || event["appliedRevision"] != "unresolved" {
				t.Fatalf("standalone policy event invented per-Agent enforcement or reference identity: %+v", event)
			}
			if _, err := time.Parse(time.RFC3339Nano, event["timestamp"].(string)); err != nil {
				t.Fatal("configuration event is missing a valid timestamp", err)
			}
			if len(event["transitionID"].(string)) != 64 {
				t.Fatal("configuration event has no stable bounded correlation")
			}
			for _, sensitive := range []string{"sk-dummy", "example.com", "synthetic-admin", "provider error"} {
				if strings.Contains(capture.raw[0], sensitive) {
					t.Fatalf("configuration event disclosed spec or provider content: %s", sensitive)
				}
			}
		})
	}
}

func TestSandboxEventsConfigurationFailureOrCASConflictDoesNotEmit(t *testing.T) {
	for _, assessment := range []string{"accepted", "rejected", "unavailable"} {
		for _, failure := range []string{"write error", "CAS conflict"} {
			t.Run(assessment+"/"+failure, func(t *testing.T) {
				_, snapshot, _ := sandboxConfigurationFixture()
				policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{
					Name: "event-policy", UID: "00000000-0000-4000-8000-000000000004", Generation: 1,
				}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
				if assessment == "rejected" {
					policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
				}
				if assessment == "unavailable" {
					snapshot.Platforms = nil
				}
				ctx, capture := capturePolicyEvents(t)
				injected := errors.New("sk-dummy synthetic status error")
				backing := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(policy).WithObjects(policy).Build()
				writer := interceptor.NewClient(backing, interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
						if failure == "write error" {
							return injected
						}
						current := &api.AgentPolicy{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
							t.Fatal(err)
						}
						current.Annotations = map[string]string{"example.com/concurrent": "synthetic"}
						if err := c.Update(ctx, current); err != nil {
							t.Fatal(err)
						}
						return c.SubResource(subresource).Patch(ctx, object, patch, options...)
					},
				})
				r := &controller.AgentPolicyReconciler{
					Client: writer, APIReader: backing, Now: func() time.Time { return snapshot.ObservedAt },
					Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
						return snapshot, nil
					}),
				}
				request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
				_, err := r.Reconcile(ctx, request)
				if failure == "write error" && !errors.Is(err, injected) || failure == "CAS conflict" && !apierrors.IsConflict(err) {
					t.Fatalf("configuration persistence error was hidden: %v", err)
				}
				if len(capture.events) != 0 {
					t.Fatal("uncommitted configuration decision emitted an event")
				}
				if err := backing.Get(ctx, request.NamespacedName, policy); err != nil {
					t.Fatal(err)
				}
				if len(policy.Status.Conditions) != 0 {
					t.Fatal("failed configuration status was committed")
				}
			})
		}
	}
}
