// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"text/template"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func TestPortalTeamUpdateRejectsConcurrentGovernance(t *testing.T) {
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "governed", UID: "team-uid"}, Spec: api.TeamSpec{
		AgentPolicyRef: "old-policy",
		SandboxRefs:    []string{"old"}, DefaultSandboxRef: "old",
		Skills: []string{"old-skill"},
	}}
	updates := 0
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(team).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			updates++
			if updates == 1 {
				current := &api.Team{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(team), current); err != nil {
					t.Fatal(err)
				}
				current.Spec.AgentPolicyRef = "new-policy"
				current.Spec.SandboxRefs, current.Spec.DefaultSandboxRef = []string{"new"}, "new"
				if err := c.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
				return apierrors.NewConflict(api.GroupVersion.WithResource("teams").GroupResource(), team.Name, fmt.Errorf("synthetic concurrent update"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		}}).Build()
	current := &api.Team{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(team), current); err != nil {
		t.Fatal(err)
	}
	_, err := NewClient(c).UpdateTeamSpec(context.Background(), team.Name, api.TeamSpec{
		MemberContext:  api.TeamContextSpec{Role: "new-role"},
		AgentPolicyRef: "requested-policy",
	}, ConfigurationVersion{UID: current.UID, ResourceVersion: current.ResourceVersion})
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
	got := &api.Team{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(team), got); err != nil {
		t.Fatal(err)
	}
	if updates != 1 || got.Spec.AgentPolicyRef != "new-policy" ||
		!slices.Equal(got.Spec.SandboxRefs, []string{"new"}) || got.Spec.MemberContext.Role != "" ||
		!slices.Equal(got.Spec.Skills, []string{"old-skill"}) {
		t.Fatalf("conflicting configuration was overwritten: %+v", got.Spec)
	}
}

func TestPortalCreatedNamespaceBelongsToNewAgent(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			var objects []client.Object
			if existing {
				objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-example", UID: "foreign-namespace"}})
			}
			c := newFakeClient(objects...)
			agent, err := NewClient(c).CreateInstance(context.Background(), "example", "Example", "", "", "", api.HarnessSpec{}, "dummy-token", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(agent.Finalizers, api.Finalizer) {
				t.Fatal("precreated resources have no cleanup finalizer")
			}
			ns := &corev1.Namespace{}
			if err := c.Get(context.Background(), client.ObjectKey{Name: "cetirun-example"}, ns); err != nil {
				t.Fatal(err)
			}
			if existing {
				if len(ns.OwnerReferences) != 0 {
					t.Fatal("Portal adopted a pre-existing namespace")
				}
			} else if len(ns.OwnerReferences) != 1 || ns.OwnerReferences[0].UID != agent.UID ||
				ns.OwnerReferences[0].Name != agent.Name || ns.OwnerReferences[0].Kind != "Agent" || agent.UID == "" {
				t.Fatal("new namespace is not bound to the actual Agent UID")
			}
		})
	}
}

func TestPortalNamespaceOwnershipRejectsReplacement(t *testing.T) {
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "agent-uid"}}
	created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-example", UID: "created-uid"}}
	replacement := created.DeepCopy()
	replacement.UID = "replacement-uid"
	c := newFakeClient(replacement)
	if err := NewClient(c).bindCreatedNamespace(context.Background(), created, agent); err == nil {
		t.Fatal("Portal adopted a replacement namespace")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(replacement), replacement); err != nil || len(replacement.OwnerReferences) != 0 {
		t.Fatalf("replacement namespace changed: %v", err)
	}
}

func TestPortalOwnershipWriteFailureKeepsAgentForRecovery(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetUID(types.UID("test-" + obj.GetName()))
				return c.Create(ctx, obj, opts...)
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return fmt.Errorf("synthetic ownership write failure")
			},
		}).Build()
	if _, err := NewClient(c).CreateInstance(context.Background(), "example", "Example", "", "", "", api.HarnessSpec{}, "dummy-token", "", ""); err == nil {
		t.Fatal("ownership error was hidden")
	}
	agent := &api.Agent{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "example"}, agent); err != nil ||
		!slices.Contains(agent.Finalizers, api.Finalizer) {
		t.Fatal("partially completed creation is no longer tracked for cleanup")
	}
}

func TestPortalNamespaceOwnershipRBAC(t *testing.T) {
	source, err := os.ReadFile("../../chart/templates/clusterrole.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, rules, ok := strings.Cut(string(source), "\nrules:\n")
	if !ok {
		t.Fatal("Portal role has no rules")
	}
	// The rules are a template with optional rules; check every variant.
	rulesTemplate, err := template.New("portal-rbac-rules").Parse(rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, marketplace := range []bool{false, true} {
		var rendered bytes.Buffer
		if err := rulesTemplate.Execute(&rendered, map[string]any{"Values": map[string]any{"skillMarketplace": map[string]any{"enabled": marketplace}}}); err != nil {
			t.Fatal(err)
		}
		var parsed []rbacv1.PolicyRule
		if err := yaml.Unmarshal(rendered.Bytes(), &parsed); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, rule := range parsed {
			if slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, "namespaces") {
				if !slices.Equal(rule.Verbs, []string{"get", "create", "patch"}) {
					t.Fatalf("namespace ownership needs get/create/patch, not deletion or update: %v", rule.Verbs)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Portal namespace ownership permissions are missing (Skill Marketplace enabled=%v)", marketplace)
		}
	}
}

func TestPortalNamespaceBindingRetriesWithoutTakingOverOwnership(t *testing.T) {
	for _, foreignOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign-owner=%v", foreignOwner), func(t *testing.T) {
			ctx := context.Background()
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-example", UID: "namespace-uid"}}
			agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "agent-uid"}}
			attempts := 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(namespace).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						attempts++
						if attempts == 1 {
							latest := &corev1.Namespace{}
							if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), latest); err != nil {
								return err
							}
							latest.Annotations = map[string]string{"example.com/concurrent-edit": "preserved"}
							if foreignOwner {
								latest.OwnerReferences = []metav1.OwnerReference{{
									APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: "other", UID: "other-agent",
								}}
							}
							if err := c.Update(ctx, latest); err != nil {
								return err
							}
							return apierrors.NewConflict(corev1.SchemeGroupVersion.WithResource("namespaces").GroupResource(), obj.GetName(), fmt.Errorf("concurrent update"))
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			err := NewClient(c).bindCreatedNamespace(ctx, namespace, agent)
			if foreignOwner && err == nil || !foreignOwner && err != nil {
				t.Fatalf("unexpected ownership result: %v", err)
			}
			stored := &corev1.Namespace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), stored); err != nil {
				t.Fatal(err)
			}
			expectedOwner := agent.UID
			expectedAttempts := 2
			if foreignOwner {
				expectedOwner = "other-agent"
				expectedAttempts = 1
			}
			if attempts != expectedAttempts || len(stored.OwnerReferences) != 1 || stored.OwnerReferences[0].UID != expectedOwner ||
				stored.Annotations["example.com/concurrent-edit"] != "preserved" {
				t.Fatalf("ownership retry lost concurrent metadata or took over a namespace: attempts=%d, metadata=%+v", attempts, stored.ObjectMeta)
			}
		})
	}
}
