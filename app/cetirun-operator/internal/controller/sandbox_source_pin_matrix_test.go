// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSandboxSourcePinMatrix(t *testing.T) {
	metadata := metav1.ObjectMeta{Name: "source", UID: "source-uid", ResourceVersion: "1", Generation: 1}
	for _, fixture := range []struct {
		object client.Object
		kind   schema.GroupVersionKind
	}{
		{&api.Team{ObjectMeta: metadata, Spec: api.TeamSpec{AgentPolicyRef: "policy", SandboxRefs: []string{"selected"}, DefaultSandboxRef: "selected"}}, api.GroupVersion.WithKind("Team")},
		{&api.Sandbox{ObjectMeta: metadata}, api.GroupVersion.WithKind("Sandbox")},
		{&api.AgentPolicy{ObjectMeta: metadata}, api.GroupVersion.WithKind("AgentPolicy")},
		{&corev1.ConfigMap{ObjectMeta: metadata, Data: map[string]string{"input": "synthetic"}}, corev1.SchemeGroupVersion.WithKind("ConfigMap")},
		{&rbacv1.Role{ObjectMeta: metadata}, rbacv1.SchemeGroupVersion.WithKind("Role")},
		{&rbacv1.ClusterRole{ObjectMeta: metadata}, rbacv1.SchemeGroupVersion.WithKind("ClusterRole")},
		{&corev1.Secret{ObjectMeta: metadata, Type: corev1.SecretTypeOpaque}, corev1.SchemeGroupVersion.WithKind("Secret")},
	} {
		for _, change := range []string{"observation", "replacement-uid", "missing-version", "terminating", "unavailable"} {
			t.Run(fixture.kind.Kind+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				scheme := runtime.NewScheme()
				for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, api.AddToScheme} {
					if err := add(scheme); err != nil {
						t.Fatal(err)
					}
				}
				object := fixture.object.DeepCopyObject().(client.Object)
				if fixture.kind.Kind == "ConfigMap" || fixture.kind.Kind == "Role" || fixture.kind.Kind == "Secret" {
					object.SetNamespace("example")
				}
				kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(object).Build()
				r := &AgentReconciler{Client: kube, APIReader: kube, Scheme: scheme}
				source := sandboxSource(object, fixture.kind.GroupVersion().String(), fixture.kind.Kind)
				read := func(capture bool) (string, error) {
					if fixture.kind.Kind == "Secret" {
						_, err := r.readSandboxSource(ctx, source)
						return "", err
					}
					return r.executionSourceDigest(ctx, source, "selected", capture)
				}
				baseline, err := read(false)
				if err != nil {
					t.Fatal(err)
				}
				if change == "observation" {
					object.SetAnnotations(map[string]string{"example.com/observation": "changed"})
				}
				failure := errors.New("synthetic source read failure")
				r.APIReader = interceptor.NewClient(kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, current client.Object, opts ...client.GetOption) error {
						if fixture.kind.Kind == "Secret" {
							if _, ok := current.(*metav1.PartialObjectMetadata); !ok {
								t.Fatal("Secret content was read instead of its approval identity")
							}
						}
						if change == "unavailable" {
							return failure
						}
						if err := c.Get(ctx, key, current, opts...); err != nil {
							return err
						}
						switch change {
						case "replacement-uid":
							current.SetUID("replacement-source-uid")
						case "missing-version":
							current.SetResourceVersion("")
						case "terminating":
							current.SetDeletionTimestamp(ptr.To(metav1.Now()))
						}
						return nil
					},
				})
				if change == "observation" {
					if err := kube.Update(ctx, object); err != nil {
						t.Fatal(err)
					}
					for _, capture := range []bool{false, true} {
						actual, err := read(capture)
						pinned := capture || source.IsClusterRole() || fixture.kind.Kind == "Secret"
						if pinned {
							if !errors.Is(err, errSandboxSourceChanged) {
								t.Fatalf("approval/capture version was ignored: capture=%t err=%v", capture, err)
							}
						} else if err != nil || actual != baseline {
							t.Fatalf("observation-only metadata changed effective execution: %v", err)
						}
					}
				} else {
					_, err := read(false)
					expected := errSandboxSourceChanged
					if change == "unavailable" {
						expected = failure
					}
					if !errors.Is(err, expected) {
						t.Fatalf("invalid source was accepted or its failure hidden: %v", err)
					}
				}
				if fixture.kind.Kind == "Secret" && w.EffectiveSource(source) {
					t.Fatal("Secret content became an effective digest input")
				}
			})
		}
	}
}
