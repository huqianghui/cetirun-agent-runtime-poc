// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func reconciledRegistryFixture(t *testing.T, kind string, setup ...func(*AgentReconciler, client.Client)) (*nativeSandboxFixture, *corev1.Secret) {
	t.Helper()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false, setup...)
	ctx := context.Background()
	agent := f.current(t)
	agent.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "registry-source-example"}
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[registryauth.Operation] = "synthetic-operation"
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	raw, err := registryauth.Encode(*agent.Spec.Agent.Image,
		registryauth.Credentials{Username: "example", Password: "synthetic-token"})
	if err != nil {
		t.Fatal(err)
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: agent.Spec.Agent.RegistrySecretRef.Name, Namespace: f.r.registryNamespace(),
			OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}, Labels: registryauth.Labels(agent.UID),
			Annotations: map[string]string{registryauth.Operation: agent.Annotations[registryauth.Operation]},
		},
		Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
	if err := f.kube.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	audit := &registryAuditClient{Client: f.kube}
	f.r.Client, f.r.APIReader = audit, audit
	return f, source
}

func TestReconciledSandboxRegistryColdStart(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, pinned := range []bool{false, true} {
			t.Run(kind+"/port-pinned="+map[bool]string{false: "false", true: "true"}[pinned], func(t *testing.T) {
				f, source := reconciledRegistryFixture(t, kind)
				ctx := context.Background()
				agent := f.current(t)
				if !pinned {
					agent.Spec.Agent.Port = nil
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					f.reconcileOK(t)
					if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
						t.Fatalf("runtime defaulting created an unbootstrapped namespace: %v", err)
					}
				}
				for range 3 {
					f.reconcileOK(t)
				}
				agent = f.current(t)
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
					t.Fatal(err)
				}
				if err := f.r.validateSandboxBootstrapNamespace(ctx, agent, namespace); err != nil {
					t.Fatal("private-image Agent lacks owned namespace bootstrap:", err)
				}
				name, err := registryauth.PullSecretName(agent.UID)
				if err != nil {
					t.Fatal(err)
				}
				target := &corev1.Secret{}
				key := client.ObjectKey{Namespace: namespace.Name, Name: name}
				if err := f.kube.Get(ctx, key, target); err != nil {
					t.Fatal(err)
				}
				if !registryauth.Owned(target, agent) || !reflect.DeepEqual(source.Data, target.Data) ||
					agent.Status.RegistryCredentials == nil || f.installer.stages != 1 {
					t.Fatal("cold private-image creation did not synchronize credentials and apply the common workload")
				}
				state := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace,
					Name: w.PodExpectationName(namespace.Name, namespace.UID)}, state); err != nil {
					t.Fatal(err)
				}
				expected, err := w.ReadReconcileExpectation(state)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(expected.Sources, func(reference w.SourceReference) bool {
					return reference.Kind == "Secret" && reference.Namespace == namespace.Name && reference.Name == target.Name &&
						reference.UID == target.UID && reference.ResourceVersion == target.ResourceVersion
				}) || !slices.Equal(f.workload(t).Spec.Template.Spec.ImagePullSecrets, []corev1.LocalObjectReference{{Name: target.Name}}) {
					t.Fatal("published workload did not pin the exact owned image-pull Secret")
				}
				home := &corev1.PersistentVolumeClaim{}
				homeKey := client.ObjectKey{Namespace: namespace.Name, Name: f.r.SandboxPreparation.bootstrap.HomeClaimName}
				if err := f.kube.Get(ctx, homeKey, home); err != nil {
					t.Fatal(err)
				}
				pullUID, homeUID := target.UID, home.UID
				workload := f.workload(t)
				source.Data[corev1.DockerConfigJsonKey], err = registryauth.Encode(*agent.Spec.Agent.Image,
					registryauth.Credentials{Username: "example", Password: "rotated-synthetic-token"})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.kube.Update(ctx, source); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if err := f.kube.Get(ctx, key, target); err != nil {
					t.Fatal(err)
				}
				if err := f.kube.Get(ctx, homeKey, home); err != nil {
					t.Fatal(err)
				}
				if target.UID != pullUID || home.UID != homeUID || !reflect.DeepEqual(target.Data, source.Data) {
					t.Fatal("registry rotation did not preserve the owned pull Secret and HOME")
				}
				if current := f.workload(t); current.UID != workload.UID || !reflect.DeepEqual(current.Spec, workload.Spec) {
					t.Fatal("registry credential rotation changed the workload or its Pod template")
				}
			})
		}
	}
}

func TestReconciledSandboxRegistryRequiresBootstrap(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, failure := range []string{"bootstrap-unavailable", "unreceipted-namespace"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				f, _ := reconciledRegistryFixture(t, kind)
				ctx := context.Background()
				agent := f.current(t)
				namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
					Name: instanceNamespace(agent.Name), OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)},
				}}
				if failure == "bootstrap-unavailable" {
					f.r.SandboxPreparation.bootstrap = nil
				} else if err := f.kube.Create(ctx, namespace); err != nil {
					t.Fatal(err)
				}
				before := namespace.DeepCopy()
				for range 2 {
					if _, err := f.reconcile(); err == nil {
						t.Fatal("missing owned bootstrap was accepted")
					}
				}
				name, err := registryauth.PullSecretName(agent.UID)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatalf("credentials reached an unbootstrapped namespace: %v", err)
				}
				err = f.kube.Get(ctx, client.ObjectKeyFromObject(namespace), namespace)
				if failure == "bootstrap-unavailable" {
					if !apierrors.IsNotFound(err) {
						t.Fatalf("registry synchronization created a namespace without bootstrap: %v", err)
					}
				} else if err != nil || !reflect.DeepEqual(before, namespace) {
					t.Fatal("unreceipted namespace was changed or adopted")
				}
				f.assertNotReady(t)
			})
		}
	}
}
