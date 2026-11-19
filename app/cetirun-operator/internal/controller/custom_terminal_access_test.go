// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"fmt"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func enableTerminal(t *testing.T, s *completionScenario, desktop bool) {
	t.Helper()
	s.provider.bindings.Network.APIServer = []runtimebindings.APIServerEndpoint{{IP: "192.0.2.10", Port: 443}}
	s.reconciler.DomainSuffix = "platform.example.com"
	current := s.current()
	current.Spec.Helm.Values = map[string]runtime.RawExtension{
		"ttyd":    {Raw: []byte(`{"enabled":true}`)},
		"desktop": {Raw: []byte(fmt.Sprintf(`{"enabled":%v}`, desktop))},
	}
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: terminalpolicy.WorkloadName("openclaw-" + s.agent.Name + "-cetirun"), Namespace: "cetirun-" + s.agent.Name,
		Generation:      1,
		Annotations:     map[string]string{"cetirun.microsoft.com/agent-uid": string(s.agent.UID)},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(s.agent, api.GroupVersion.WithKind("Agent"))},
	}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	if err := s.client.Create(context.Background(), deployment); err != nil {
		t.Fatal(err)
	}
}

func TestCustomTerminalUsesSeparateServiceAndStopsOnSuspension(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, desktop := range []bool{false, true} {
		t.Run(fmt.Sprintf("desktop=%v", desktop), func(t *testing.T) {
			s := newCompletionScenario(t)
			enableTerminal(t, s, desktop)
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			if !apimeta.IsStatusConditionTrue(s.current().Status.Conditions, api.ConditionTerminalConfigured) {
				t.Fatal("terminal not configured")
			}
			if s.current().Status.AccessURL != "" {
				t.Fatal("terminal became a native Agent UI URL")
			}
			key := types.NamespacedName{Namespace: "cetirun-completion", Name: "cetirun-completion"}
			route := &istiov1beta1.VirtualService{}
			if err := s.client.Get(context.Background(), key, route); err != nil {
				t.Fatal(err)
			}
			foundTerminal, foundDesktop := false, false
			for _, rule := range route.Spec.Http {
				for _, destination := range rule.Route {
					switch destination.Destination.Port.Number {
					case 7681:
						foundTerminal = true
						want := terminalpolicy.WorkloadName("openclaw-completion-cetirun") + ".cetirun-completion.svc.cluster.local"
						if destination.Destination.Host != want || rule.Match[0].Uri.GetPrefix() != "/completion/term/" || rule.Retries != nil {
							t.Fatal("terminal route target, prefix or retry policy changed")
						}
					case 6080:
						foundDesktop = true
					default:
						t.Fatal("unapproved Custom endpoint")
					}
				}
			}
			if !foundTerminal || foundDesktop != desktop {
				t.Fatal("routes do not match platform services")
			}
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name: terminalpolicy.WorkloadName("openclaw-completion-cetirun"), Namespace: key.Namespace,
				Annotations:     map[string]string{"cetirun.microsoft.com/agent-uid": string(s.agent.UID)},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(s.agent, api.GroupVersion.WithKind("Agent"))},
			}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}}
			if err := s.client.Get(context.Background(), types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Name}, deployment); err != nil {
				t.Fatal(err)
			}
			current := s.current()
			current.Spec.Suspended = true
			if err := s.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			if err := s.client.Get(context.Background(), key, route); !apierrors.IsNotFound(err) {
				t.Fatal("suspension retained route")
			}
			if err := s.client.Get(context.Background(), types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Name}, deployment); err != nil {
				t.Fatal(err)
			}
			if ptr.Deref(deployment.Spec.Replicas, 1) != 0 {
				t.Fatal("suspension retained terminal deployment")
			}
		})
	}
}

func TestCustomTerminalCanBeDisabled(t *testing.T) {
	requireLegacyCustomRuntime(t)
	s := newCompletionScenario(t)
	enableTerminal(t, s, true)
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	current := s.current()
	current.Spec.Helm.Values["ttyd"] = runtime.RawExtension{Raw: []byte(`{"enabled":false}`)}
	if err := s.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := s.step(); err != nil {
		t.Fatal(err)
	}
	if apimeta.IsStatusConditionTrue(s.current().Status.Conditions, api.ConditionTerminalConfigured) {
		t.Fatal("disabled terminal remains available")
	}
	route := &istiov1beta1.VirtualService{}
	if err := s.client.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-completion", Name: "cetirun-completion"}, route); err != nil {
		t.Fatal(err)
	}
	for _, rule := range route.Spec.Http {
		for _, destination := range rule.Route {
			if destination.Destination.Port.Number == 7681 {
				t.Fatal("disabled terminal route retained")
			}
		}
	}
}

func TestCustomTerminalStopsOnSandboxHold(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			s := newCompletionScenario(t)
			enableTerminal(t, s, true)
			if err := s.step(); err != nil {
				t.Fatal(err)
			}
			key := types.NamespacedName{Namespace: "cetirun-completion", Name: terminalpolicy.WorkloadName("openclaw-completion-cetirun")}
			deployment := &appsv1.Deployment{}
			if err := s.client.Get(context.Background(), key, deployment); err != nil {
				t.Fatal(err)
			}
			if foreign {
				deployment.OwnerReferences[0].UID = "foreign-agent"
				if err := s.client.Update(context.Background(), deployment); err != nil {
					t.Fatal(err)
				}
			}
			current := s.current()
			current.Spec.SandboxRef = "example-sandbox"
			if err := s.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if err := s.step(); (err != nil) != foreign {
				t.Fatalf("hold error=%v, foreign=%v", err, foreign)
			}
			if err := s.client.Get(context.Background(), key, deployment); err != nil {
				t.Fatal(err)
			}
			wantReplicas := int32(0)
			if foreign {
				wantReplicas = 1
			}
			if ptr.Deref(deployment.Spec.Replicas, -1) != wantReplicas {
				t.Fatalf("hold replicas=%v, want %d", deployment.Spec.Replicas, wantReplicas)
			}
			for _, condition := range []string{api.ConditionIngressReady, api.ConditionTerminalConfigured, api.ConditionDesktopConfigured} {
				if !apimeta.IsStatusConditionFalse(s.current().Status.Conditions, condition) {
					t.Fatalf("hold retained %s", condition)
				}
			}
			key.Name = "cetirun-completion"
			if err := s.client.Get(context.Background(), key, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
				t.Fatalf("hold retained route: %v", err)
			}
		})
	}
}
