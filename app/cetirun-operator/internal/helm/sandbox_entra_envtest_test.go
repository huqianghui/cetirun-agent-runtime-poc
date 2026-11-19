// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package helm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEntraIdentityBindingAPIServer(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("Entra binding tests refuse an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("KUBEBUILDER_ASSETS must contain the pinned local envtest binaries")
		}
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false)}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		t.Fatal(errors.Join(err, environment.Stop()))
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bundle := entraRenderBundle(t, "hermes", true)
	namespace, account, _ := entraBindingObjects(t, bundle)
	namespace.UID, namespace.ResourceVersion = "", ""
	account.UID, account.ResourceVersion = "", ""
	if err := kube.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	input := bundle.Identity()
	input.Namespace.UID = namespace.UID
	bundle, err = w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, account); err != nil {
		t.Fatal(err)
	}
	pin := w.ObjectIdentity{Name: account.Name, UID: account.UID}
	readback := func() (*corev1.Namespace, *corev1.ServiceAccount) {
		t.Helper()
		ns, sa := &corev1.Namespace{}, &corev1.ServiceAccount{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(namespace), ns); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(account), sa); err != nil {
			t.Fatal(err)
		}
		return ns, sa
	}
	ns, sa := readback()
	if err := bundle.ValidateIdentityBinding(ns, sa, pin); err != nil {
		t.Fatal("served identity rejected:", err)
	}
	for _, permission := range []struct{ resource, subresource, verb string }{
		{"pods", "", "create"}, {"pods", "", "get"}, {"secrets", "", "get"}, {"serviceaccounts", "token", "create"},
	} {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   "system:serviceaccount:" + namespace.Name + ":" + account.Name,
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace.Name, "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace.Name, Resource: permission.resource, Subresource: permission.subresource, Verb: permission.verb,
			},
		}}
		if err := kube.Create(ctx, review); err != nil || review.Status.Allowed {
			t.Fatal("Entra-only ServiceAccount gained unrequested Kubernetes RBAC:", err)
		}
	}
	sa.Annotations["azure.workload.identity/client-id"] = "unapproved-client"
	if err := kube.Update(ctx, sa); err != nil {
		t.Fatal(err)
	}
	ns, sa = readback()
	if err := bundle.ValidateIdentityBinding(ns, sa, pin); err == nil {
		t.Fatal("served client drift accepted")
	}
	sa.Annotations = account.Annotations
	if err := kube.Update(ctx, sa); err != nil {
		t.Fatal(err)
	}
	ns, sa = readback()
	if err := bundle.ValidateIdentityBinding(ns, sa, pin); err != nil {
		t.Fatal("restored current identity rejected:", err)
	}
	if err := kube.Delete(ctx, sa, client.Preconditions{UID: &pin.UID}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(sa), &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatal("ServiceAccount deletion was not confirmed:", err)
	}
	replacement := account.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "", ""
	replacement.CreationTimestamp, replacement.ManagedFields = metav1.Time{}, nil
	if err := kube.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	ns, sa = readback()
	if sa.UID == pin.UID || bundle.ValidateIdentityBinding(ns, sa, pin) == nil {
		t.Fatal("same-name ServiceAccount replacement inherited the recorded identity pin")
	}
}
