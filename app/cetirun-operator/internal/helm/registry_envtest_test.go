// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package helm

import (
	"context"
	"os"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestRegistryIsolationAgainstAPIServerRBAC(t *testing.T) {
	environment := &envtest.Environment{BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	// This isolated server is used only by this test; avoid client-side throttling
	// while checking the deny matrix. No deployed client's limits are changed.
	cfg.QPS, cfg.Burst = 1000, 1000
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	const sourceNS, targetNS, otherNS = "registry-source-test", "cetirun-registry-test", "other-registry-test"
	for _, name := range []string{sourceNS, targetNS, otherNS} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: targetNS}}); err != nil {
		t.Fatal(err)
	}
	check := func() error {
		return registryauth.CheckIsolation(ctx, c, sourceNS, targetNS, "agent", "registry-source-example", "registry-pull-example")
	}
	if err := check(); err != nil {
		t.Fatalf("unprivileged service account: %v", err)
	}
	for _, test := range []struct {
		name, namespace string
		rules           []rbacv1.PolicyRule
	}{
		{"named-source-read", sourceNS, []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"registry-source-example"}, Verbs: []string{"get"}}}},
		{"local-secret-list", targetNS, []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"list"}}}},
		{"indirect-pod-mount", targetNS, []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create"}}}},
		{"indirect-job-mount", sourceNS, []rbacv1.PolicyRule{{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create"}}}},
		{"named-workload-patch", targetNS, []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"statefulsets"}, ResourceNames: []string{"one-agent"}, Verbs: []string{"patch"}}}},
		{"other-namespace-token", otherNS, []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, ResourceNames: []string{"privileged"}, Verbs: []string{"create"}}}},
		{"named-source-write", sourceNS, []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"registry-source-example"}, Verbs: []string{"patch"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: test.name, Namespace: test.namespace}, Rules: test.rules}
			binding := &rbacv1.RoleBinding{ObjectMeta: role.ObjectMeta, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: test.name},
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "agent", Namespace: targetNS}}}
			if err := c.Create(ctx, role); err != nil {
				t.Fatal(err)
			}
			if err := c.Create(ctx, binding); err != nil {
				t.Fatal(err)
			}
			if err := check(); err == nil {
				t.Fatal("effective RBAC escalation was accepted")
			}
			if err := c.Delete(ctx, binding); err != nil {
				t.Fatal(err)
			}
		})
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "registry-test-wide"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}}
	if err := c.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:serviceaccounts:" + targetNS, APIGroup: rbacv1.GroupName}}}
	if err := c.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("cluster-wide group grant bypassed credential isolation")
	}
}
