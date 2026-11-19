// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConfigureTestGateway gives external controller tests a complete in-memory
// Gateway projection without exposing the production client injection point.
// It is compiled only into the controller test package.
func ConfigureTestGateway(t testing.TB, r *AgentReconciler) {
	t.Helper()
	r.AIGateway = AIGatewayConfig{
		Enabled: true, URL: testAIGatewayURL, MasterKeySecretName: "gateway-auth",
		DefaultBudget: 10, DefaultModel: testSharedModelOne,
	}
	gateway := newFakeAIGateway()
	r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-ai-gateway", Namespace: "cetirun-system"}, Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.12",
		Selector: map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway"},
		Ports:    []corev1.ServicePort{{Name: "http", Port: 4000, Protocol: corev1.ProtocolTCP}},
	}}
	if err := r.Create(context.Background(), service); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create test AI Gateway Service: %v", err)
	}
}

// ReuseTestGateway copies the durable Gateway test double into a reconstructed
// reconciler. Production restarts reconnect to the same Gateway, so tests of
// recovery must not accidentally replace its virtual-key records.
func ReuseTestGateway(t testing.TB, source, target *AgentReconciler) {
	t.Helper()
	target.AIGateway = source.AIGateway
	target.aiGatewayClientFactory = source.aiGatewayClientFactory
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-ai-gateway", Namespace: "cetirun-system"}, Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.12",
		Selector: map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway"},
		Ports:    []corev1.ServicePort{{Name: "http", Port: 4000, Protocol: corev1.ProtocolTCP}},
	}}
	if err := target.Create(context.Background(), service); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create test AI Gateway Service: %v", err)
	}
}
