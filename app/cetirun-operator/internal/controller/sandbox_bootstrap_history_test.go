// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"os"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	strictjson "sigs.k8s.io/json"
)

type historicalBootstrapRBAC struct {
	SourceCommit string             `json:"sourceCommit"`
	SourceBlob   string             `json:"sourceBlob"`
	SourcePath   string             `json:"sourcePath"`
	Role         rbacv1.Role        `json:"role"`
	RoleBinding  rbacv1.RoleBinding `json:"roleBinding"`
}

func readHistoricalBootstrapRBAC(t *testing.T) historicalBootstrapRBAC {
	t.Helper()
	raw, err := os.ReadFile("testdata/bootstrap/pre-340-rbac.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture historicalBootstrapRBAC
	problems, err := strictjson.UnmarshalStrict(raw, &fixture)
	if err != nil || len(problems) != 0 {
		t.Fatalf("invalid historical fixture: %v %v", err, problems)
	}
	if fixture.SourceCommit != "74a7aedb57f039878af4dcc7b73cf9124c1070ca" ||
		fixture.SourceBlob != "52ffa26d634296c9fc03fe01fed9da7ff82e5570" ||
		fixture.SourcePath != "app/cetirun-operator/internal/controller/agent_controller.go" {
		t.Fatal("historical fixture provenance changed")
	}
	return fixture
}

func TestHistoricalBootstrapRBACContract(t *testing.T) {
	fixture := readHistoricalBootstrapRBAC(t)
	if len(fixture.Role.Rules) != 7 || len(fixture.RoleBinding.Subjects) != 1 {
		t.Fatal("historical Role or binding shape changed")
	}
	expected := fixture.Role.DeepCopy()
	found := false
	for index := range expected.Rules {
		rule := &expected.Rules[index]
		if slices.Equal(rule.APIGroups, []string{"networking.istio.io"}) {
			if found || !slices.Equal(rule.Resources, []string{"virtualservices", "sidecars"}) {
				t.Fatal("fixture no longer describes the pre-mTLS Istio rule")
			}
			rule.Resources = append(rule.Resources, "destinationrules")
			found = true
		}
	}
	if !found {
		t.Fatal("historical Istio rule is missing")
	}
	subject := fixture.RoleBinding.Subjects[0]
	r := &AgentReconciler{OperatorNamespace: subject.Namespace, OperatorSAName: subject.Name}
	role, binding := r.operatorNamespaceRBAC(fixture.Role.Namespace)
	if !reflect.DeepEqual(role.Rules, expected.Rules) ||
		role.Name != fixture.Role.Name || !reflect.DeepEqual(role.Labels, fixture.Role.Labels) ||
		binding.Name != fixture.RoleBinding.Name || binding.Namespace != fixture.RoleBinding.Namespace ||
		!reflect.DeepEqual(binding.Labels, fixture.RoleBinding.Labels) ||
		binding.RoleRef != fixture.RoleBinding.RoleRef || !reflect.DeepEqual(binding.Subjects, fixture.RoleBinding.Subjects) {
		t.Fatal("the supported upgrade is no longer only the documented permission addition")
	}
	legacy, current := sandboxBootstrapMTLSRoleRules()
	if !reflect.DeepEqual(legacy, fixture.Role.Rules) || !reflect.DeepEqual(current, expected.Rules) {
		t.Fatal("production migration does not match the independently frozen contract")
	}
}

func TestHistoricalBootstrapUpgradeRechecksPostWriteIdentity(t *testing.T) {
	for _, dependency := range []string{"namespace", "binding"} {
		for _, drift := range []string{"replacement", "terminating"} {
			t.Run(dependency+"/"+drift, func(t *testing.T) {
				ctx := context.Background()
				f, receipt, role := bootstrapRoleUpgradeFixture(t, "hermes", "container")
				patched := false
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if err := c.Patch(ctx, object, patch, opts...); err != nil {
							return err
						}
						if _, ok := object.(*rbacv1.Role); ok && object.GetName() == role.Name {
							patched = true
						}
						return nil
					},
				})
				f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if err := c.Get(ctx, key, object, opts...); err != nil {
							return err
						}
						_, namespace := object.(*corev1.Namespace)
						_, binding := object.(*rbacv1.RoleBinding)
						target := dependency == "namespace" && namespace && key.Name == receipt.Namespace ||
							dependency == "binding" && binding && key.Name == receipt.Objects[2].Name && key.Namespace == receipt.Namespace
						if patched && target {
							if drift == "replacement" {
								object.SetUID("replacement-dependency-uid")
							} else {
								object.SetDeletionTimestamp(ptr.To(metav1.Now()))
							}
						}
						return nil
					},
				})
				if err := f.r.bootstrapReconciledSandbox(ctx, f.current(t)); err == nil || !patched {
					t.Fatalf("post-write dependency drift was accepted: patched=%t err=%v", patched, err)
				}
				_, after, err := f.r.readSandboxBootstrap(ctx, f.current(t))
				if err != nil || !reflect.DeepEqual(after, receipt) {
					t.Fatal("failed migration adopted changed dependency identities:", err)
				}
			})
		}
	}
}
