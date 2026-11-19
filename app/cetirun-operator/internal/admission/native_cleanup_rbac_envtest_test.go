// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"fmt"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func nativeDisabledCleanupClient(t *testing.T, ctx context.Context, admin client.Client, config *rest.Config, scheme *runtime.Scheme) client.Client {
	t.Helper()
	chrt, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValues(chrt, map[string]any{
		"cetiadmin":      map[string]any{"enabled": false},
		"sandboxSources": map[string]any{"enabled": false, "nativeExecution": false},
	}, chartutil.ReleaseOptions{Name: "cleanup", Namespace: "operator-state"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chrt, values)
	if err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal([]byte(rendered[chrt.Name()+"/templates/clusterrole.yaml"]), role); err != nil {
		t.Fatal(err)
	}
	if role.Name == "" {
		t.Fatal("disabled configuration did not render the manager role")
	}
	if err := admin.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.ClusterRoleBinding{}
	if err := yaml.Unmarshal([]byte(rendered[chrt.Name()+"/templates/clusterrolebinding.yaml"]), binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" {
		t.Fatal("disabled configuration did not render the manager binding")
	}
	if err := admin.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	scoped := rest.CopyConfig(config)
	subject := binding.Subjects[0]
	scoped.Impersonate = rest.ImpersonationConfig{
		UserName: fmt.Sprintf("system:serviceaccount:%s:%s", subject.Namespace, subject.Name),
	}
	cleanup, err := client.New(scoped, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return cleanup
}
