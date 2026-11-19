// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func gatewayCustomFixture(t *testing.T) (*KubernetesCustomBindings, *api.Agent, *fakeAIGateway, runtimebindings.Custom) {
	t.Helper()
	p, instance := readyProvider(t)
	r := p.reconciler
	r.CustomBindings = p
	r.EventRecorder = record.NewFakeRecorder(100)
	r.AIGateway = AIGatewayConfig{Enabled: true, URL: testAIGatewayURL, MasterKeySecretName: "gateway-auth", DefaultBudget: 10, CopilotProxyImage: testProxyImage}
	gateway := newFakeAIGateway()
	r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-ai-gateway", Namespace: "cetirun-system"}, Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.12", Selector: map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway"},
		Ports: []corev1.ServicePort{{Name: "http", Port: 4000, Protocol: corev1.ProtocolTCP}},
	}}
	if err := r.Create(context.Background(), service); err != nil {
		t.Fatal(err)
	}
	b, err := p.Resolve(context.Background(), instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, instance, gateway, b
}

func TestCustomGatewayRequiredForEveryApprovedImage(t *testing.T) {
	p, instance := readyProvider(t)
	p.reconciler.CustomBindings = p
	p.reconciler.AIGateway.Enabled = true
	if !p.reconciler.aiGatewayRequired(instance) || !p.UsesAIGateway(instance) {
		t.Fatal("all approved Custom images must require Gateway access")
	}
}

func TestCustomGatewayKeyBindingAndCopilot(t *testing.T) {
	p, instance, gateway, b := gatewayCustomFixture(t)
	ctx := context.Background()
	if err := p.reconciler.bindCustomGateway(ctx, instance, &b); err != nil {
		t.Fatal(err)
	}
	resolved, err := agents.ResolveCustom(agents.Request{Type: "custom", Image: *instance.Spec.Agent.Image})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(resolved); err != nil {
		t.Fatal(err)
	}
	if _, err := p.reconciler.validateCustomObjects(ctx, instance, b); err != nil {
		t.Fatal(err)
	}
	if b.Model.Name != aiGatewayKeySecretName || b.Model.BaseURL != testAIGatewayURL+"/v1" || b.Network.GatewayPort != 4000 || b.Copilot == nil {
		t.Fatalf("missing Custom Gateway binding: %#v", b)
	}
	if !instance.Status.AIGateway.Routed || instance.Status.AIGateway.Copilot.Phase != api.AIGatewayCopilotPhaseSignInRequired {
		t.Fatal("unauthed Copilot readiness was not kept separate")
	}
	if len(gateway.generated) != 1 {
		t.Fatal("unexpected key creation count")
	}
	serialized, _ := json.Marshal(b)
	if strings.Contains(string(serialized), gateway.generated[0].Key) {
		t.Fatal("binding contains a key value")
	}
	first := b.Model
	if err := p.reconciler.bindCustomGateway(ctx, instance, &b); err != nil {
		t.Fatal(err)
	}
	if first != b.Model || len(gateway.generated) != 1 {
		t.Fatal("steady reconciliation changed the key binding")
	}
	if err := p.reconciler.deleteAIGatewayKey(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if len(gateway.keys) != 0 || len(gateway.deletedAliases) != 1 {
		t.Fatal("Custom key was not revoked")
	}
}

func TestCustomGatewayNeverFallsBack(t *testing.T) {
	for _, scenario := range []string{"disabled", "outage", "foreign-secret", "foreign-service"} {
		t.Run(scenario, func(t *testing.T) {
			p, instance, gateway, b := gatewayCustomFixture(t)
			ctx := context.Background()
			switch scenario {
			case "disabled":
				p.reconciler.AIGateway.Enabled = false
			case "outage":
				gateway.getErr = fmt.Errorf("synthetic unavailable")
			case "foreign-secret":
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiGatewayKeySecretName, Namespace: b.Namespace.Name, Labels: map[string]string{aiGatewayAgentUIDLabel: "other-uid"}}, Data: map[string][]byte{aiGatewayKeySecretField: []byte("sk-dummy")}}
				if err := p.reconciler.Create(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "foreign-service":
				service := &corev1.Service{}
				if err := p.reconciler.Get(ctx, types.NamespacedName{Namespace: "cetirun-system", Name: "cetirun-ai-gateway"}, service); err != nil {
					t.Fatal(err)
				}
				service.Spec.Selector = map[string]string{"app": "other"}
				if err := p.reconciler.Update(ctx, service); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.reconciler.bindCustomGateway(ctx, instance, &b); err == nil {
				t.Fatal("invalid Gateway binding accepted")
			}
			if b.Model.Name != "" || len(gateway.generated) != 0 {
				t.Fatal("failure fell back or issued a usable binding")
			}
			stored := &api.Agent{}
			if err := p.reconciler.Get(ctx, client.ObjectKeyFromObject(instance), stored); err != nil {
				t.Fatal(err)
			}
			if scenario == "disabled" && (stored.Status.AIGateway == nil || stored.Status.AIGateway.Reason != aiGatewayReasonDisabled) {
				t.Fatal("Gateway failure was not persisted")
			}
		})
	}
}

func TestCustomGatewayPublishesWorkloadReadinessBeforeCopilotRetry(t *testing.T) {
	for _, tc := range []struct {
		name             string
		copilotPhase     string
		routeUnavailable bool
		requeue          time.Duration
	}{
		{"starting", api.AIGatewayCopilotPhaseStarting, false, 15 * time.Second},
		{"degraded", api.AIGatewayCopilotPhaseDegraded, false, 15 * time.Second},
		{"starting with unavailable Chat route", api.AIGatewayCopilotPhaseStarting, true, 15 * time.Second},
		{"degraded with unavailable Chat route", api.AIGatewayCopilotPhaseDegraded, true, 15 * time.Second},
		{"sign-in required", api.AIGatewayCopilotPhaseSignInRequired, false, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, instance, gateway, bindings := gatewayCustomFixture(t)
			r := p.reconciler
			if err := istiov1beta1.AddToScheme(r.Scheme); err != nil {
				t.Fatal(err)
			}
			chart := &chartHelm{}
			r.HelmClient = chart
			r.CetiRunHelmValues = map[string]any{
				"ttyd": map[string]any{"enabled": false}, "desktop": map[string]any{"enabled": false},
			}
			r.ChatAccess = chataccess.Config{Enabled: true, Callers: []chataccess.Caller{{
				Namespace: "cetirun-system", PodLabels: map[string]string{"app": "portal"},
			}}}
			if err := r.bindCustomGateway(ctx, instance, &bindings); err != nil {
				t.Fatal(err)
			}
			instance.Spec.Agent.Port = ptr.To[int32](8080)
			instance.Generation++
			if tc.copilotPhase != api.AIGatewayCopilotPhaseSignInRequired {
				token := &corev1.Secret{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: bindings.Namespace.Name, Name: api.CopilotTokenSecretName}, token); err != nil {
					t.Fatal(err)
				}
				token.Data = map[string][]byte{api.CopilotTokenSecretKey: []byte("gho_synthetic")}
				if err := r.Update(ctx, token); err != nil {
					t.Fatal(err)
				}
				if instance.Annotations == nil {
					instance.Annotations = map[string]string{}
				}
				instance.Annotations[api.AnnotationCopilotModels] = testCopilotModels
			}
			if err := r.Update(ctx, instance); err != nil {
				t.Fatal(err)
			}
			if tc.copilotPhase == api.AIGatewayCopilotPhaseDegraded {
				proxy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
					Name: copilotProxyServiceName(instance), Namespace: bindings.Namespace.Name,
				}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
				if err := r.Create(ctx, proxy); err != nil {
					t.Fatal(err)
				}
				gateway.addModelErr = fmt.Errorf("synthetic Copilot registration failure")
			}
			if tc.routeUnavailable {
				route := chataccess.PrivateRoute(instance.Name, "another-agent-uid", bindings.Namespace.Name, 8080, r.ChatAccess.Callers)
				if err := r.Create(ctx, route); err != nil {
					t.Fatal(err)
				}
			}
			reconcile := func() ctrl.Result {
				t.Helper()
				result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
					t.Fatal(err)
				}
				return result
			}
			reconcile()
			if chart.installs != 1 || instance.Status.Phase != api.PhaseProvisioning {
				t.Fatalf("Custom workload did not start provisioning: installs=%d status=%+v", chart.installs, instance.Status)
			}
			workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: instanceServiceName(instance.Name), Namespace: bindings.Namespace.Name, Generation: 1,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("Agent"))},
				Annotations: map[string]string{
					customAgentUID: string(instance.UID),
					customPlanHash: chart.lastValues["runtime"].(map[string]any)["workloadHash"].(string),
				},
			}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
				Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, UpdatedReplicas: 1}}
			if err := r.Create(ctx, workload); err != nil {
				t.Fatal(err)
			}
			if result := reconcile(); result.RequeueAfter != 30*time.Second || instance.Status.Phase != api.PhaseProvisioning ||
				meta.IsStatusConditionTrue(instance.Status.Conditions, api.ConditionReady) {
				t.Fatalf("unready workload did not remain provisioning: result=%+v status=%+v", result, instance.Status)
			}
			workload.Status.ReadyReplicas = 1
			if err := r.Status().Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			result := reconcile()
			status := instance.Status.AIGateway
			if status == nil || !status.Routed || status.Phase != api.AIGatewayPhaseReady ||
				!slices.Contains(status.Models, testSharedModelOne) || status.Copilot == nil || status.Copilot.Phase != tc.copilotPhase {
				t.Fatalf("shared models or expected optional Copilot state missing: %+v", status)
			}
			if instance.Status.Phase != api.PhaseReady || instance.Status.ObservedGeneration != instance.Generation ||
				!meta.IsStatusConditionTrue(instance.Status.Conditions, api.ConditionPodReady) ||
				!meta.IsStatusConditionTrue(instance.Status.Conditions, api.ConditionReady) ||
				!meta.IsStatusConditionTrue(instance.Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatalf("ready workload status was not published: %+v", instance.Status)
			}
			if got := meta.IsStatusConditionTrue(instance.Status.Conditions, chataccess.Condition); got == tc.routeUnavailable {
				t.Fatalf("unexpected Chat route readiness: %+v", instance.Status.Conditions)
			}
			if result.RequeueAfter != tc.requeue || chart.installs != 1 {
				t.Fatalf("ready workload must keep its shortest retry without reapplying: result=%+v installs=%d", result, chart.installs)
			}
		})
	}
}
