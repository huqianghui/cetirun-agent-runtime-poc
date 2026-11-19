// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestEnsureGatewayTokenHermesCreatesHermesEnv(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := cetirunv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cetirun scheme: %v", err)
	}

	instance := hermesSecretTestInstance("bob")
	r := &AgentReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).Build(),
		DomainSuffix: "claws.example.com",
	}
	if err := r.ensureGatewayToken(ctx, instance); err != nil {
		t.Fatalf("ensureGatewayToken: %v", err)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: instanceNamespace("bob"), Name: "gateway-token"}
	if err := r.Get(ctx, key, secret); err != nil {
		t.Fatalf("get gateway token secret: %v", err)
	}
	apiKey := string(secret.Data["API_SERVER_KEY"])
	if apiKey == "" {
		t.Fatal("API_SERVER_KEY was not populated")
	}
	if string(secret.Data["CETIRUN_AUTH_KEY"]) == "" || bytes.Equal(secret.Data["CETIRUN_AUTH_KEY"], secret.Data["API_SERVER_KEY"]) {
		t.Fatal("independent CETIRUN_AUTH_KEY was not populated")
	}
	hermesEnv := string(secret.Data["hermes.env"])
	if hermesEnv == "" {
		t.Fatal("hermes.env was not populated")
	}
	if !strings.Contains(hermesEnv, "API_SERVER_KEY="+apiKey+"\n") {
		t.Fatal("hermes.env does not contain generated API_SERVER_KEY")
	}
	if !strings.Contains(hermesEnv, "API_SERVER_CORS_ORIGINS=https://claws.example.com\n") {
		t.Fatal("hermes.env does not contain API_SERVER_CORS_ORIGINS from DomainSuffix")
	}
}

func TestEnsureGatewayTokenOpenClawCreatesCetiRunAuthKey(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := cetirunv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cetirun scheme: %v", err)
	}

	instance := hermesSecretTestInstance("alice")
	instance.Spec.Agent.Type = cetirunv1alpha1.AgentTypeOpenClaw
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	if err := r.ensureGatewayToken(ctx, instance); err != nil {
		t.Fatalf("ensureGatewayToken: %v", err)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: instanceNamespace("alice"), Name: "gateway-token"}
	if err := r.Get(ctx, key, secret); err != nil {
		t.Fatalf("get gateway token secret: %v", err)
	}
	if string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]) == "" {
		t.Fatal("OPENCLAW_GATEWAY_TOKEN was not populated")
	}
	if string(secret.Data["CETIRUN_AUTH_KEY"]) == "" {
		t.Fatal("CETIRUN_AUTH_KEY was not populated")
	}
}

func TestEnsureGatewayTokenHermesPreservesServerAndToolCredentials(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := cetirunv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cetirun scheme: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-token",
			Namespace: instanceNamespace("bob"),
		},
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte("gateway-token"),
			"CETIRUN_AUTH_KEY":       []byte("existing-auth-key"),
			"API_SERVER_KEY":         []byte("api-key"),
			"hermes.env":             []byte("BRAVE_API_KEY=tool-key\n"),
		},
	}
	instance := hermesSecretTestInstance("bob")
	r := &AgentReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		DomainSuffix: "claws.example.com",
	}
	if err := r.ensureGatewayToken(ctx, instance); err != nil {
		t.Fatalf("ensureGatewayToken: %v", err)
	}

	updated := &corev1.Secret{}
	key := types.NamespacedName{Namespace: instanceNamespace("bob"), Name: "gateway-token"}
	if err := r.Get(ctx, key, updated); err != nil {
		t.Fatalf("get gateway token secret: %v", err)
	}
	if got := string(updated.Data["CETIRUN_AUTH_KEY"]); got != "existing-auth-key" {
		t.Fatal("CETIRUN_AUTH_KEY was changed")
	}
	hermesEnv := string(updated.Data["hermes.env"])
	if !strings.Contains(hermesEnv, "API_SERVER_KEY=api-key\n") {
		t.Fatal("hermes.env does not preserve API_SERVER_KEY")
	}
	if !strings.Contains(hermesEnv, "BRAVE_API_KEY=tool-key\n") {
		t.Fatal("Hermes tool credential was lost")
	}
	if !strings.Contains(hermesEnv, "API_SERVER_CORS_ORIGINS=https://claws.example.com\n") {
		t.Fatal("hermes.env does not contain API_SERVER_CORS_ORIGINS")
	}
}

func TestEnsureGatewayTokenConcurrentCreateBackfillsAuthKey(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := cetirunv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cetirun scheme: %v", err)
	}

	instance := hermesSecretTestInstance("race")
	key := types.NamespacedName{Namespace: instanceNamespace("race"), Name: "gateway-token"}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	raceClient := &gatewaySecretCreateRaceClient{
		Client: baseClient,
		key:    key,
		secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Data:       map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte("concurrent-token")},
		},
	}
	r := &AgentReconciler{Client: raceClient, DomainSuffix: "claws.example.com"}
	if err := r.ensureGatewayToken(ctx, instance); err != nil {
		t.Fatalf("ensureGatewayToken: %v", err)
	}

	updated := &corev1.Secret{}
	if err := baseClient.Get(ctx, key, updated); err != nil {
		t.Fatalf("get concurrently created gateway token secret: %v", err)
	}
	if got := string(updated.Data["OPENCLAW_GATEWAY_TOKEN"]); got != "concurrent-token" {
		t.Fatalf("gateway token was replaced: got %q", got)
	}
	if got := string(updated.Data["CETIRUN_AUTH_KEY"]); got == "" {
		t.Fatal("CETIRUN_AUTH_KEY was not backfilled after concurrent creation")
	}
}

// gatewaySecretCreateRaceClient simulates another reconcile creating the Secret
// after the initial Get but before Create.
type gatewaySecretCreateRaceClient struct {
	client.Client
	key      types.NamespacedName
	secret   *corev1.Secret
	firstGet bool
}

func (c *gatewaySecretCreateRaceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == c.key && !c.firstGet {
		c.firstGet = true
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *gatewaySecretCreateRaceClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if secret, ok := obj.(*corev1.Secret); ok && (types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name} == c.key) {
		if err := c.Client.Create(ctx, c.secret.DeepCopy()); err != nil {
			return err
		}
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, secret.Name)
	}
	return c.Client.Create(ctx, obj, opts...)
}

func hermesSecretTestInstance(alias string) *cetirunv1alpha1.Agent {
	return &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: alias},
		Spec: cetirunv1alpha1.AgentSpec{
			Agent:                 cetirunv1alpha1.HarnessSpec{Type: cetirunv1alpha1.AgentTypeHermes},
			GatewayTokenSecretRef: "gateway-token",
			User: cetirunv1alpha1.UserSpec{
				Alias:       alias,
				DisplayName: "Bob",
			},
		},
	}
}
