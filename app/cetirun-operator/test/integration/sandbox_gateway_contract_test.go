// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Gateway-only Agents must not keep the former zero-replica,
// direct-provider held lifetime. The native-start and common-executor
// API-server suites exercise the valid projected-key start paths.
func TestGatewayOnlyRejectsUnsupportedHeldProfiles(t *testing.T) {
	for _, tc := range []struct {
		agentType string
		reason    string
	}{
		{agentType: "openclaw", reason: "native Gateway requires a typed authorized key projection"},
		{agentType: "hermes", reason: "native config does not match the selected profile"},
	} {
		t.Run(tc.agentType, func(t *testing.T) {
			assertGatewayOnlySandboxRejection(t, tc.agentType, tc.reason)
		})
	}
}

func assertGatewayOnlySandboxRejection(t *testing.T, agentType, reason string) {
	t.Helper()
	h := newPreparationHarness(t)
	f := h.newCase(t, agentType)
	f.reconcileConfiguration(t)
	assertGatewayOnlySandboxRejectionPass(t, f, agentType, reason)

	// A fresh controller must preserve the same fail-closed boundary rather
	// than discovering the removed direct-provider fallback after restart.
	f.restart()
	assertGatewayOnlySandboxRejectionPass(t, f, agentType+"-restart", reason)
}

func assertGatewayOnlySandboxRejectionPass(t *testing.T, f *preparationCase, name, reason string) {
	t.Helper()
	beforeNamespace, beforeHome := &corev1.Namespace{}, &corev1.PersistentVolumeClaim{}
	f.h.get(t, client.ObjectKeyFromObject(f.fixture.Namespace), beforeNamespace)
	f.h.get(t, client.ObjectKeyFromObject(f.fixture.Input.Home), beforeHome)
	if _, err := f.reconciler.Reconcile(f.h.ctx, f.request()); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("%s: unsupported held profile did not fail closed with %q: %v", name, reason, err)
	}
	agent := f.agent(t)
	if agent.Status.Phase == api.PhaseReady || agent.Status.AccessURL != "" {
		t.Fatalf("%s: unsupported held profile exposed an Agent", name)
	}
	if agent.Status.Sandbox != nil || apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionReady) ||
		apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionSandboxReady) {
		t.Fatalf("%s: unsupported held profile recorded an applied Sandbox binding", name)
	}
	if !slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) || slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) {
		t.Fatalf("%s: unsupported held profile did not preserve the expected hold finalizer boundary: %v", name, agent.Finalizers)
	}
	for _, kind := range []string{api.ConditionReady, api.ConditionSandboxReady, api.ConditionAgentConfigReady} {
		condition := apimeta.FindStatusCondition(agent.Status.Conditions, kind)
		if condition == nil || condition.Status != "False" || condition.ObservedGeneration != agent.Generation {
			t.Fatalf("%s: unsupported held profile did not record current-generation %s=False: %+v", name, kind, condition)
		}
	}
	statefulSet := &appsv1.StatefulSet{}
	err := f.h.client.Get(f.h.ctx, client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: f.workloadName()}, statefulSet)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("%s: unsupported held profile created a StatefulSet: %v", name, err)
	}
	deployments := &appsv1.DeploymentList{}
	if err := f.h.client.List(f.h.ctx, deployments, client.InNamespace(f.fixture.Namespace.Name)); err != nil || len(deployments.Items) != 0 {
		t.Fatalf("%s: unsupported held profile created Deployments: count=%d err=%v", name, len(deployments.Items), err)
	}
	workload := &corev1.PodList{}
	if err := f.h.client.List(f.h.ctx, workload, client.InNamespace(f.fixture.Namespace.Name)); err != nil {
		t.Fatalf("%s: read possible workload: %v", name, err)
	}
	if len(workload.Items) != 0 {
		t.Fatalf("%s: unsupported held profile created Pods", name)
	}
	var routes istiov1beta1.VirtualServiceList
	if err := f.h.client.List(f.h.ctx, &routes, client.InNamespace(f.fixture.Namespace.Name)); err != nil || len(routes.Items) != 0 {
		t.Fatalf("%s: unsupported held profile created routes: count=%d err=%v", name, len(routes.Items), err)
	}
	afterNamespace, afterHome := &corev1.Namespace{}, &corev1.PersistentVolumeClaim{}
	f.h.get(t, client.ObjectKeyFromObject(f.fixture.Namespace), afterNamespace)
	f.h.get(t, client.ObjectKeyFromObject(f.fixture.Input.Home), afterHome)
	if afterNamespace.UID != beforeNamespace.UID || afterHome.UID != beforeHome.UID {
		t.Fatalf("%s: unsupported held profile replaced protected namespace or HOME", name)
	}
}
