// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type registryScenarioClient struct {
	client.Client
	reviews, inventories atomic.Int64
	allow                atomic.Bool
}

func (c *registryScenarioClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if review, ok := object.(*authorizationv1.SubjectAccessReview); ok {
		c.reviews.Add(1)
		review.Status.Allowed = c.allow.Load()
		review.Status.Denied = !review.Status.Allowed
		return nil
	}
	if object.GetUID() == "" {
		object.SetUID(types.UID("created-" + object.GetName()))
	}
	return c.Client.Create(ctx, object, options...)
}

func (c *registryScenarioClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	resolved := &client.ListOptions{}
	for _, option := range options {
		option.ApplyToList(resolved)
	}
	if resolved.Namespace == "" {
		switch list.(type) {
		case *api.AgentList, *corev1.PodList, *appsv1.StatefulSetList:
			c.inventories.Add(1)
		}
	}
	return c.Client.List(ctx, list, options...)
}

func attachCustomRegistry(t *testing.T, s *completionScenario) (*registryScenarioClient, *corev1.Secret) {
	t.Helper()
	ctx := context.Background()
	c := &registryScenarioClient{Client: s.client}
	s.reconciler.Client = c
	agent := s.current()
	agent.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "registry-source-example"}
	agent.Generation++
	if err := s.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	s.provider.bindings.Generation = agent.Generation
	raw, err := registryauth.Encode(*agent.Spec.Agent.Image, registryauth.Credentials{Username: "example", Password: "synthetic-token"})
	if err != nil {
		t.Fatal(err)
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Agent.RegistrySecretRef.Name, Namespace: registryauth.DefaultNamespace,
		UID: "source-uid", OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}, Labels: registryauth.Labels(agent.UID)},
		Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: raw}}
	if err := s.client.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	return c, source
}

func TestCustomRegistryChangesNeverStopHealthyWriter(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := runningCustomReview(t)
	ctx := context.Background()
	before := s.workload()
	approvals := s.provider.store.approvals
	c, source := attachCustomRegistry(t, s)
	agent := s.current()
	if s.current().Status.Phase != api.PhaseReady || s.current().Status.RegistryCredentials == nil {
		t.Fatalf("Custom sync did not complete: %+v", s.current().Status.Conditions)
	}
	if s.provider.store.approvals != approvals || !reflect.DeepEqual(before, s.workload()) {
		t.Fatal("adding a pull reference stopped or replaced the writer")
	}
	c.reviews.Store(0)
	c.inventories.Store(0)
	for range 10 {
		if err := s.step(); err != nil {
			t.Fatal(err)
		}
	}
	if c.reviews.Load() != 0 || c.inventories.Load() != 0 {
		t.Fatalf("steady Custom reconciliation repeated a registry audit: reviews=%d inventories=%d", c.reviews.Load(), c.inventories.Load())
	}
	hash := s.helm.lastValues["runtime"].(map[string]any)["workloadHash"]
	s.helm.installCalled = false
	_ = s.client.Get(ctx, client.ObjectKeyFromObject(source), source)
	source.Data[corev1.DockerConfigJsonKey], _ = registryauth.Encode(*agent.Spec.Agent.Image, registryauth.Credentials{Username: "example", Password: "rotated-token"})
	_ = s.client.Update(ctx, source)
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if s.helm.installCalled || s.provider.store.approvals != approvals || s.helm.lastValues["runtime"].(map[string]any)["workloadHash"] != hash {
		t.Fatal("content rotation redeployed Custom")
	}
	if c.reviews.Load() == 0 {
		t.Fatal("content rotation did not audit current permissions")
	}
	agent = s.current()
	agent.Spec.Agent.RegistrySecretRef = nil
	agent.Generation++
	_ = s.client.Update(ctx, agent)
	s.provider.bindings.Generation = agent.Generation
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if s.provider.store.approvals != approvals || s.current().Status.RegistryCredentials != nil || !reflect.DeepEqual(before, s.workload()) {
		t.Fatal("removal disturbed writer or retained credentials")
	}
}

func TestCustomPrivateRuntimeChangeRechecksAuthorizationBeforeHandoff(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := runningCustomReview(t)
	c, _ := attachCustomRegistry(t, s)
	s.provider.approve = func(sts *appsv1.StatefulSet) error {
		zero := int32(0)
		sts.Spec.Replicas = &zero
		return s.client.Update(context.Background(), sts)
	}
	before := s.workload()
	approvals := s.provider.store.approvals
	agent := s.current()
	port := int32(18080)
	agent.Spec.Agent.Port = &port
	agent.Generation++
	if err := s.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	s.provider.bindings.Generation = agent.Generation
	c.reviews.Store(0)
	c.allow.Store(true)
	s.helm.installCalled = false
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(s.current().Status.Conditions, api.ConditionRegistryCredentialsReady)
	if c.reviews.Load() == 0 || condition == nil || condition.Reason != "RegistryIsolationUnverified" {
		t.Fatal("workload change reused credential synchronization as authorization")
	}
	if s.helm.installCalled || s.provider.store.approvals != approvals || !reflect.DeepEqual(before, s.workload()) {
		t.Fatal("failed authorization reached workload application or writer handoff")
	}
	c.allow.Store(false)
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if !s.helm.installCalled || s.provider.store.approvals <= approvals {
		t.Fatalf("normal workload update did not recover after permissions were corrected: installed=%t approvals=%d conditions=%+v", s.helm.installCalled, s.provider.store.approvals, s.current().Status.Conditions)
	}
}
