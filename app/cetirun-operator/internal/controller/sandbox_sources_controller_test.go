// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSandboxSourcesHeldControllerWiring(t *testing.T) {
	for _, test := range []struct {
		agentType      string
		advancingClock bool
	}{
		{"openclaw", false}, {"hermes", false},
		{"openclaw", true}, {"hermes", true},
	} {
		name := test.agentType + "/system-clock"
		if test.advancingClock {
			name = test.agentType + "/clock-advances-during-capture"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newPreparationFixture(t, test.agentType)
			for _, object := range []client.Object{
				&corev1.PersistentVolumeClaim{ObjectMeta: f.fixture.Input.Home.ObjectMeta},
				&corev1.PersistentVolumeClaim{ObjectMeta: f.fixture.Input.Skills.ObjectMeta},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
					Name: f.fixture.Agent.Spec.GatewayTokenSecretRef, Namespace: f.fixture.Namespace.Name,
				}},
			} {
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatal(err)
				}
				object.SetOwnerReferences([]metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)})
				annotations := object.GetAnnotations()
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations[sandboxworkload.AgentUIDAnnotation] = string(f.fixture.Agent.UID)
				annotations[sandboxworkload.AgentNameAnnotation] = f.fixture.Agent.Name
				object.SetAnnotations(annotations)
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			config := controller.SandboxSourcesConfig{
				Enabled: true, PlatformConfigMapName: "sandbox-platform",
				HomeClaimName: f.fixture.Input.Home.Name, SkillsClaimName: f.fixture.Input.Skills.Name,
				AdmissionServiceAccount: "cetirun-sandbox-admission", Helpers: f.fixture.Input.Helpers,
			}
			declaration := sandboxSourceDeclaration(t, f.config)
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: config.PlatformConfigMapName, Namespace: "cetirun-system", UID: "platform-config-uid"},
				Data:       map[string]string{controller.SandboxPlatformConfigurationKey: declaration},
			}
			if err := f.client.Create(ctx, configMap); err != nil {
				t.Fatal(err)
			}
			preparation, err := controller.NewKubernetesSandboxPreparation(config, f.client, f.installer,
				"cetirun-system", f.installer.chart.Metadata.Version)
			if err != nil {
				t.Fatal(err)
			}
			source := preparation.Configuration.(*controller.KubernetesSandboxConfigurationSource)
			prerequisites := preparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites)
			if test.advancingClock {
				preparation.Now = func() time.Time { return f.now }
				prerequisites.Now = preparation.Now
				source.Now = func() time.Time {
					f.now = f.now.Add(time.Millisecond)
					return f.now
				}
			}
			f.reconciler.SandboxPreparation = preparation
			sandboxController := &controller.SandboxReconciler{
				Client: f.client, APIReader: f.client, Source: source, Now: preparation.Now,
			}
			if _, err := sandboxController.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.fixture.Sandbox)}); err != nil {
				t.Fatal("Sandbox status rejected a freshly captured source:", err)
			}
			policyController := &controller.AgentPolicyReconciler{
				Client: f.client, APIReader: f.client, Source: source, Now: preparation.Now,
			}
			if _, err := policyController.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.fixture.Policy)}); err != nil {
				t.Fatal("AgentPolicy status rejected a freshly captured source:", err)
			}
			box, policy := &api.Sandbox{}, &api.AgentPolicy{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Sandbox), box); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Policy), policy); err != nil {
				t.Fatal(err)
			}
			if !apimeta.IsStatusConditionTrue(box.Status.Conditions, api.ConditionReady) ||
				!apimeta.IsStatusConditionTrue(policy.Status.Conditions, api.ConditionAgentPolicyAccepted) {
				t.Fatal("shared configuration controllers did not accept the checked prerequisites")
			}

			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("stock source wiring did not stop at unavailable writer fencing:", err)
			}
			agent := f.current(t)
			condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
			if f.installer.stages != 0 || agent.Status.AccessURL != "" || agent.Status.Sandbox != nil ||
				!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) ||
				slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) ||
				!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionReady) ||
				condition == nil || condition.Status != metav1.ConditionUnknown || condition.Reason != "ObservationUnavailable" ||
				!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatal("Kubernetes sources were mistaken for execution or fencing evidence")
			}
			var state corev1.ConfigMapList
			if err := f.client.List(ctx, &state, client.InNamespace(configMap.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(state.Items) != 1 || state.Items[0].UID != configMap.UID {
				t.Fatal("stock source wiring persisted preparation state without a writer-exclusion adapter")
			}
			workload := &appsv1.StatefulSet{}
			if err := f.client.Get(ctx, client.ObjectKey{Namespace: f.fixture.Namespace.Name,
				Name: "openclaw-example-cetirun"}, workload); !apierrors.IsNotFound(err) {
				t.Fatal("unavailable fencing allowed workload staging:", err)
			}

			prerequisites.WriterExclusion = f.source
			if err := f.reconcile(); err != nil {
				t.Fatal("test-only fence could not drive real-source held preparation:", err)
			}
			assertSandboxPrepared(t, f)
			stages := f.installer.stages
			if err := f.reconcile(); err != nil || f.installer.stages != stages {
				t.Fatal("unchanged authoritative inputs did not reuse the held preparation:", err)
			}
			assertSandboxPrepared(t, f)

			if err := f.client.Delete(ctx, configMap); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); !apierrors.IsNotFound(err) {
				t.Fatal("configuration outage was hidden behind cached preparation:", err)
			}
			agent = f.current(t)
			if f.installer.stages != stages || agent.Status.AccessURL != "" || agent.Status.Sandbox != nil ||
				!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionAgentConfigReady) ||
				!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionReady) {
				t.Fatal("lost platform authority retained readiness or started a workload")
			}
		})
	}
}

func sandboxSourceDeclaration(t *testing.T, snapshot controller.SandboxConfigurationSnapshot) string {
	t.Helper()
	raw, err := json.Marshal(controller.SandboxPlatformConfiguration{
		Version: "v1", Registrations: snapshot.Registrations, Platforms: snapshot.Platforms,
		RuntimeClasses: snapshot.RuntimeClasses, StorageClasses: snapshot.StorageClasses,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
