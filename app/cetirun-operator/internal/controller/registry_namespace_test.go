// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type registryNamespaceHelm struct {
	HelmInstaller
	prepareCalls, uninstallCalls int
}

func (h *registryNamespaceHelm) Prepare(string, map[string]any, string) (*helm.PreparedRelease, error) {
	h.prepareCalls++
	return nil, fmt.Errorf("unexpected chart preparation")
}

func (h *registryNamespaceHelm) Uninstall(string, string) error {
	h.uninstallCalls++
	return nil
}

func TestRegistryNamespaceReservedBeforeEveryLifecyclePath(t *testing.T) {
	for _, namespace := range []struct{ name, configured string }{
		{registryauth.DefaultNamespace, ""},
		{"cetirun-private-sources", "cetirun-private-sources"},
		{registryauth.DefaultNamespace, "cetirun-private-sources"},
	} {
		for _, runtime := range []string{"hermes", "openclaw", "custom", "sandbox"} {
			for _, lifecycle := range []string{"create", "suspend", "delete"} {
				t.Run(namespace.name+"/"+namespace.configured+"/"+runtime+"/"+lifecycle, func(t *testing.T) {
					r, _, source := registryFixture(t)
					ctx := context.Background()
					r.RegistryCredentialNamespace = namespace.configured
					r.EventRecorder = record.NewFakeRecorder(10)
					r.DomainSuffix = "agents.example.com"
					h := &registryNamespaceHelm{}
					r.HelmClient = h
					agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{
						Name: strings.TrimPrefix(namespace.name, "cetirun-"), UID: "reserved-agent-uid", Generation: 1,
						Finalizers: []string{api.Finalizer, "example.com/other-finalizer"},
					}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentType(runtime), Image: ptr.To("registry.example.com/agent:1"), Port: ptr.To[int32](8080)},
						User: api.UserSpec{Alias: "example"}, Helm: api.HelmSpec{Version: "0.1.1"}},
						Status: api.AgentStatus{Phase: api.PhaseReady, AccessURL: "https://agents.example.com/old/"}}
					if runtime == "sandbox" {
						agent.Spec.Agent.Type = api.AgentTypeHermes
						agent.Spec.SandboxRef = "example-sandbox"
					}
					if lifecycle == "create" {
						agent.Finalizers = nil
					}
					if lifecycle == "suspend" {
						agent.Spec.Suspended = true
					}
					if err := r.Create(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if lifecycle == "delete" {
						if err := r.Delete(ctx, agent); err != nil {
							t.Fatal(err)
						}
						if err := r.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil || agent.DeletionTimestamp.IsZero() {
							t.Fatalf("deletion was not persisted: %v", err)
						}
					}
					protected := []client.Object{
						&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace.name, UID: "credentials-namespace-uid"}},
						&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: namespace.name}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)}},
						&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "retained-data", Namespace: namespace.name}},
						&istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: virtualServiceName(agent.Name), Namespace: namespace.name}},
					}
					for _, object := range protected {
						if err := r.Create(ctx, object); err != nil {
							t.Fatal(err)
						}
					}
					protected = append(protected, source)
					if namespace.name != source.Namespace {
						copy := source.DeepCopy()
						copy.Namespace, copy.ResourceVersion = namespace.name, ""
						if err := r.Create(ctx, copy); err != nil {
							t.Fatal(err)
						}
						protected = append(protected, copy)
					}
					for _, object := range protected {
						if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
							t.Fatal(err)
						}
					}
					_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
					if err == nil || !strings.Contains(err.Error(), "ReservedInstanceName") {
						t.Errorf("expected reserved namespace rejection, got %v", err)
					}
					if h.prepareCalls != 0 || h.uninstallCalls != 0 {
						t.Errorf("reserved namespace reached Helm: prepare=%d uninstall=%d", h.prepareCalls, h.uninstallCalls)
					}
					for _, before := range protected {
						after := before.DeepCopyObject().(client.Object)
						if err := r.Get(ctx, client.ObjectKeyFromObject(before), after); err != nil || !reflect.DeepEqual(before, after) {
							t.Errorf("protected %T %s changed: %v", before, client.ObjectKeyFromObject(before), err)
						}
					}
					current := &api.Agent{}
					if err := r.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
						t.Fatal(err)
					}
					condition := meta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentConfigReady)
					if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ReservedInstanceName" || current.Status.AccessURL != "" {
						t.Error("reserved Agent still appears configured or accessible")
					}
					phase := api.PhaseFailed
					if lifecycle == "delete" {
						phase = api.PhaseDeleting
					}
					if current.Status.Phase != phase {
						t.Errorf("phase = %q, want %q", current.Status.Phase, phase)
					}
					if !reflect.DeepEqual(current.Finalizers, agent.Finalizers) {
						t.Error("reserved Agent finalizers changed without safe recovery")
					}
				})
			}
		}
	}
}
