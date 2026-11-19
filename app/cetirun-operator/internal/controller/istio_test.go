// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return scheme
}

func TestResolveIstioRevision_AutoDetectsSingleRevision(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-rev",
			Namespace: "aks-istio-system",
			Labels: map[string]string{
				"istio.io/rev": "asm-1-20",
			},
		},
	}

	reconciler := &AgentReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build(),
	}

	if got := reconciler.resolveIstioRevision(ctx); got != "asm-1-20" {
		t.Fatalf("resolveIstioRevision() = %q, want %q", got, "asm-1-20")
	}
}

func TestEnsureNamespace_AppliesIstioRevisionLabel(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-rev",
			Namespace: "aks-istio-system",
			Labels: map[string]string{
				"istio.io/rev": "asm-1-20",
			},
		},
	}
	alias := "alice"
	nsName := instanceNamespace(alias)
	existingNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
			Labels: map[string]string{
				"owner": "test",
			},
		},
	}

	reconciler := &AgentReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, existingNS).Build(),
	}

	if err := reconciler.ensureNamespace(ctx, nsName, alias); err != nil {
		t.Fatalf("ensureNamespace() error: %v", err)
	}

	updated := &corev1.Namespace{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: nsName}, updated); err != nil {
		t.Fatalf("get namespace after ensureNamespace: %v", err)
	}

	if got := updated.Labels["istio.io/rev"]; got != "asm-1-20" {
		t.Fatalf("namespace istio.io/rev label = %q, want %q", got, "asm-1-20")
	}
	if got := updated.Labels["app.kubernetes.io/instance"]; got != alias {
		t.Fatalf("namespace app.kubernetes.io/instance label = %q, want %q", got, alias)
	}
}
