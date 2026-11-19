// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func approvedRoleObjects(grants []ApprovedGrant) []client.Object {
	var objects []client.Object
	for _, grant := range grants {
		role := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: grant.RoleRef.Name, UID: grant.RoleUID, ResourceVersion: grant.RoleResourceVersion},
			Rules:      grant.Rules,
		}
		objects = append(objects, role.DeepCopy())
	}
	return objects
}

func roleReader(t *testing.T, objects []client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestVerifyPolicyGrantsRejectsLiveRoleDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ApprovedGrant, *rbacv1.ClusterRole)
		want   error
	}{
		{"approved observation", func(*ApprovedGrant, *rbacv1.ClusterRole) {}, nil},
		{"normalized contents", func(_ *ApprovedGrant, role *rbacv1.ClusterRole) {
			slices.Reverse(role.Rules)
			for i := range role.Rules {
				rule := &role.Rules[i]
				slices.Reverse(rule.APIGroups)
				slices.Reverse(rule.Resources)
				slices.Reverse(rule.ResourceNames)
				slices.Reverse(rule.NonResourceURLs)
				slices.Reverse(rule.Verbs)
				rule.Verbs = append(rule.Verbs, rule.Verbs[0])
			}
			role.Rules = append(role.Rules, role.Rules[0])
		}, nil},
		{"role contents with unchanged UID and RV", func(_ *ApprovedGrant, role *rbacv1.ClusterRole) {
			role.Rules[0].Resources = append(role.Rules[0].Resources, "secrets")
		}, ErrUnapprovedGrant},
		{"role UID with unchanged rules", func(_ *ApprovedGrant, role *rbacv1.ClusterRole) {
			role.UID = "replacement-role"
		}, ErrUnapprovedGrant},
		{"role RV with unchanged UID and rules", func(_ *ApprovedGrant, role *rbacv1.ClusterRole) {
			role.ResourceVersion = "99"
		}, ErrUnapprovedGrant},
		{"missing approved RV", func(grant *ApprovedGrant, _ *rbacv1.ClusterRole) {
			grant.RoleResourceVersion = ""
		}, ErrUnapprovedGrant},
		{"terminating role", func(_ *ApprovedGrant, role *rbacv1.ClusterRole) {
			stamp := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			role.DeletionTimestamp, role.Finalizers = &stamp, []string{"example.com/pending"}
		}, ErrUnapprovedGrant},
		{"explicit wildcard and secret approval", func(grant *ApprovedGrant, role *rbacv1.ClusterRole) {
			grant.Rules = []rbacv1.PolicyRule{{
				APIGroups: []string{"*"}, Resources: []string{"*", "secrets"},
				Verbs: []string{"*", "get", "bind", "escalate", "impersonate"},
			}}
			role.Rules = grant.Rules
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := adminFixture()
			input.Platform.Grants[0].Rules[0].Resources = []string{"pods", "configmaps"}
			input.Platform.Grants[0].Rules[0].ResourceNames = []string{"first", "second"}
			input.Platform.Grants[0].Rules = append(input.Platform.Grants[0].Rules, rbacv1.PolicyRule{
				NonResourceURLs: []string{"/healthz", "/version"}, Verbs: []string{"get"},
			})
			objects := approvedRoleObjects(input.Platform.Grants)
			tc.mutate(&input.Platform.Grants[0], objects[0].(*rbacv1.ClusterRole))
			policy := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{UID: input.Resolution.Binding.AgentPolicyUID},
				Spec:       input.Resolution.AgentPolicy,
			}
			before, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			err = VerifyPolicyGrants(context.Background(), roleReader(t, objects), policy, input.Platform.Grants)
			if !errors.Is(err, tc.want) {
				t.Fatalf("verification = %v, want %v", err, tc.want)
			}
			after, err := json.Marshal(input)
			if err != nil || string(before) != string(after) {
				t.Fatal("role observation changed immutable approval input", err)
			}
			if tc.want == nil {
				mustCompile(t, input)
			}
		})
	}
}

func TestVerifyPolicyGrantsPreservesUnavailableAndMissingCauses(t *testing.T) {
	input := adminFixture()
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{UID: input.Resolution.Binding.AgentPolicyUID}, Spec: input.Resolution.AgentPolicy}
	failure := errors.New("synthetic authoritative role read failure")
	reader := &grantObservationReader{Reader: roleReader(t, nil), failure: failure}
	err := VerifyPolicyGrants(context.Background(), reader, policy, input.Platform.Grants)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, failure) {
		t.Fatal("unavailable observation lost its underlying cause", err)
	}
	reader.failure = nil
	err = VerifyPolicyGrants(context.Background(), reader, policy, input.Platform.Grants)
	if !errors.Is(err, ErrUnapprovedGrant) || !apierrors.IsNotFound(err) {
		t.Fatal("missing live role was accepted or lost its read cause", err)
	}
	if err := VerifyPolicyGrants(context.Background(), nil, policy, input.Platform.Grants); !errors.Is(err, ErrUnavailable) {
		t.Fatal("requested grants require an authoritative reader", err)
	}
	policy.Spec.KubernetesAccess = nil
	if err := VerifyPolicyGrants(context.Background(), nil, policy, input.Platform.Grants); err != nil {
		t.Fatal("unrequested grants must not trigger role reads", err)
	}
}

func TestVerifyPolicyGrantsReadsSharedRoleOnceForAllScopes(t *testing.T) {
	input := adminFixture()
	objects := approvedRoleObjects(input.Platform.Grants)
	grant := input.Platform.Grants[0]
	grant.Namespace = "other-operations"
	input.Platform.Grants = append(input.Platform.Grants, grant)
	input.Resolution.AgentPolicy.KubernetesAccess.RoleBindings = append(input.Resolution.AgentPolicy.KubernetesAccess.RoleBindings,
		api.AgentPolicyRoleBinding{Namespace: grant.Namespace, ClusterRoleName: grant.RoleRef.Name})
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{UID: input.Resolution.Binding.AgentPolicyUID}, Spec: input.Resolution.AgentPolicy}
	reader := &grantObservationReader{Reader: roleReader(t, objects)}
	if err := VerifyPolicyGrants(context.Background(), reader, policy, input.Platform.Grants); err != nil {
		t.Fatal(err)
	}
	if reader.reads != 2 {
		t.Fatalf("shared roles were read inconsistently per scope: %d reads, want 2", reader.reads)
	}
}

type grantObservationReader struct {
	client.Reader
	failure error
	reads   int
}

func (r *grantObservationReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.reads++
	if r.failure != nil {
		return r.failure
	}
	return r.Reader.Get(ctx, key, object, opts...)
}
