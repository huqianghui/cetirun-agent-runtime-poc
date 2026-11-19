// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cetirunv1alpha1.AddToScheme(scheme))
	return scheme
}

func newFakeClientWith(objs ...client.Object) client.Client {
	return newFakeClient(objs...)
}

func newFakeClient(objs ...client.Object) client.Client {
	scheme := newScheme()
	builder := fake.NewClientBuilder().WithScheme(scheme)
	hasTeam := false
	for _, obj := range objs {
		if _, ok := obj.(*cetirunv1alpha1.Team); ok {
			hasTeam = true
			break
		}
	}
	if !hasTeam {
		objs = append(objs, gatewayTeam("aks"))
	}
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	return builder.WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID(types.UID("test-" + obj.GetName()))
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
}

// newGatewayFakeClient adds the catalog required by Gateway-only creation
// tests. Tests of missing or malformed catalogs use newFakeClient directly.
func newGatewayFakeClient(objs ...client.Object) client.Client {
	objs = append(objs, catalogConfigMap("gpt-6", "gpt-6-astra"))
	return newFakeClient(objs...)
}

func gatewayTestConfig() config.Config {
	return gatewayConfig()
}

func replaceTestCatalog(t *testing.T, c client.Client, catalog *corev1.ConfigMap) {
	t.Helper()
	current := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(catalog), current); err != nil {
		t.Fatal(err)
	}
	current.Data = catalog.Data
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}
