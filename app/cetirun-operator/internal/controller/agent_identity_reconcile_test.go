// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/azure"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
)

type suspensionHelmClient struct {
	installCalled bool
	lastValues    map[string]any
}

func (f *suspensionHelmClient) ResolveImageUpgrade(_, _, _, _, image, _ string) (string, error) {
	return image, nil
}

func (f *suspensionHelmClient) Prepare(_ string, values map[string]any, chartVersion string) (*helm.PreparedRelease, error) {
	return &helm.PreparedRelease{Values: values, ChartVersion: chartVersion}, nil
}

func (f *suspensionHelmClient) Refresh(prepared *helm.PreparedRelease, values map[string]any) (*helm.PreparedRelease, error) {
	prepared.Values = values
	return prepared, nil
}

func (f *suspensionHelmClient) InstallOrUpgrade(_ string, _ string, prepared *helm.PreparedRelease) error {
	f.installCalled = true
	f.lastValues = prepared.Values
	return nil
}

func (*suspensionHelmClient) CustomApplied(string, string, *helm.PreparedRelease) (bool, error) {
	return false, nil
}

func (*suspensionHelmClient) Uninstall(string, string) error { return nil }

func TestPolicyDeniedSuspensionStopsOwnedRouteBeforeIdentityLookup(t *testing.T) {
	const name = "suspended-identity"
	instance := &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			UID:        types.UID(testAgentIdentityInstanceUID),
			Generation: 2,
			Finalizers: []string{cetirunv1alpha1.Finalizer},
			Annotations: map[string]string{
				cetirunv1alpha1.AnnotationOwnerTenantID: testAgentIdentityTenantID,
				cetirunv1alpha1.AnnotationOwnerObjectID: testAgentIdentityOwnerObject,
			},
		},
		Spec: cetirunv1alpha1.AgentSpec{
			Agent:      cetirunv1alpha1.HarnessSpec{Type: cetirunv1alpha1.AgentTypeOpenClaw},
			SandboxRef: "unavailable-sandbox",
			User: cetirunv1alpha1.UserSpec{
				Alias:       name,
				DisplayName: "Suspended Identity",
			},
			GatewayTokenSecretRef: "gateway-token",
			Helm: cetirunv1alpha1.HelmSpec{
				Version: "0.1.28",
				Values:  map[string]runtime.RawExtension{},
			},
			Suspended:     true,
			AgentIdentity: &cetirunv1alpha1.AgentIdentitySpec{Enabled: true},
		},
		Status: cetirunv1alpha1.AgentStatus{
			Phase:              cetirunv1alpha1.PhaseReady,
			ObservedGeneration: 1,
			AgentIdentity: &cetirunv1alpha1.AgentIdentityStatus{
				Phase:              cetirunv1alpha1.AgentIdentityPhaseReady,
				ClientID:           testAgentIdentityClientID,
				ObjectID:           testAgentIdentityObjectID,
				DisplayName:        testAgentIdentityDisplayName(),
				ObservedGeneration: 2,
			},
		},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(name)}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-token", Namespace: instanceNamespace(name)},
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte("synthetic-token"),
			"CETIRUN_AUTH_KEY":       []byte("synthetic-auth-key"),
		},
	}
	virtualService := &istiov1beta1.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name: virtualServiceName(name), Namespace: instanceNamespace(name), UID: "suspended-identity-route",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: cetirunv1alpha1.GroupVersion.String(), Kind: "Agent", Name: name, UID: instance.UID,
			}},
			Annotations: map[string]string{
				"cetirun.microsoft.com/agent-uid": string(instance.UID),
				"meta.helm.sh/release-name":       helmReleaseName(name),
				"meta.helm.sh/release-namespace":  instanceNamespace(name),
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cetirunv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := istiov1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(instance, namespace, secret, virtualService).
		WithStatusSubresource(instance).
		Build()
	directory := &fakeAgentIdentityDirectory{getObjectErr: errors.New("synthetic Graph outage")}
	helmClient := &suspensionHelmClient{}
	reconciler := &AgentReconciler{
		Client:                         client,
		Scheme:                         scheme,
		EventRecorder:                  record.NewFakeRecorder(10),
		HelmClient:                     helmClient,
		OperatorNamespace:              "cetirun-system",
		OperatorSAName:                 "cetirun-operator-controller-manager",
		ChartVersion:                   "0.1.28",
		DomainSuffix:                   "example.com",
		AgentIdentityEnabled:           true,
		AgentIdentityTenantID:          testAgentIdentityTenantID,
		AgentIdentityBlueprintClientID: testAgentIdentityBlueprintID,
		agentIdentityGraphClientFactory: func() (agentIdentityDirectoryClient, error) {
			return directory, nil
		},
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	if err != nil {
		t.Fatalf("suspension returned an identity dependency error: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Fatalf("policy hold retry interval = %v, want 1m", result.RequeueAfter)
	}
	if helmClient.installCalled {
		t.Fatal("denied policy binding reached Helm or Agent Identity reconciliation")
	}
	remaining := &istiov1beta1.VirtualService{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: virtualService.Name, Namespace: virtualService.Namespace}, remaining); !apierrors.IsNotFound(err) {
		t.Fatalf("suspension did not remove ingress: %v", err)
	}
	updated := &cetirunv1alpha1.Agent{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: name}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != cetirunv1alpha1.PhaseSuspended {
		t.Fatalf("phase = %q, want Suspended", updated.Status.Phase)
	}
	if updated.Status.Policy == nil || updated.Status.Policy.Phase != "Denied" {
		t.Fatalf("policy denial was not retained: %#v", updated.Status.Policy)
	}
}

func findStatusCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func TestSandboxHeldDeletionPreservesAgentIdentityCleanup(t *testing.T) {
	for _, failure := range []string{"none", "authorization", "directory", "spec-removed"} {
		for _, namespacePresent := range []bool{true, false} {
			name := failure + map[bool]string{true: "/namespace-present", false: "/namespace-absent"}[namespacePresent]
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				instance := newAgentIdentityTestInstance()
				instance.Spec.Agent.Type = cetirunv1alpha1.AgentTypeOpenClaw
				instance.Spec.SandboxRef = "standard"
				instance.Finalizers = []string{cetirunv1alpha1.Finalizer, sandboxHoldFinalizer}
				instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
					Phase: cetirunv1alpha1.AgentIdentityPhaseReady, ClientID: testAgentIdentityClientID,
					ObjectID: testAgentIdentityObjectID, DisplayName: testAgentIdentityDisplayName(),
				}
				if failure == "spec-removed" {
					instance.Spec.AgentIdentity = nil
				}
				directory := &fakeAgentIdentityDirectory{byObject: &azure.ServicePrincipal{
					ID: testAgentIdentityObjectID, AppID: testAgentIdentityClientID,
					DisplayName: testAgentIdentityDisplayName(), AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
				}}
				if failure == "directory" {
					directory.deleteErr = errors.New("synthetic directory failure")
				}
				reconciler := newAgentIdentityTestReconciler(t, instance, directory)
				if err := clientgoscheme.AddToScheme(reconciler.Client.Scheme()); err != nil {
					t.Fatal(err)
				}
				if err := istiov1beta1.AddToScheme(reconciler.Client.Scheme()); err != nil {
					t.Fatal(err)
				}
				namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
					Name: instanceNamespace(instance.Name), UID: "namespace-uid",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: cetirunv1alpha1.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: instance.UID,
					}},
				}}
				if namespacePresent {
					if err := reconciler.Create(ctx, namespace); err != nil {
						t.Fatal(err)
					}
				}
				var brokerStatus, brokerCalls atomic.Int32
				brokerStatus.Store(http.StatusNoContent)
				if failure == "authorization" {
					brokerStatus.Store(http.StatusServiceUnavailable)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					brokerCalls.Add(1)
					if req.Method != http.MethodDelete || req.URL.Path != "/operator/v1/instances/"+testAgentIdentityInstanceUID+"/authorization" {
						t.Errorf("unexpected authorization cleanup request: %s %s", req.Method, req.URL.Path)
					}
					w.WriteHeader(int(brokerStatus.Load()))
				}))
				t.Cleanup(server.Close)
				reconciler.AgentIdentityBrokerURL = server.URL
				reconciler.AgentIdentityHTTPClient = server.Client()
				directory.onDelete = func(objectID string) {
					if objectID != testAgentIdentityObjectID || brokerCalls.Load() == 0 || brokerStatus.Load() != http.StatusNoContent {
						t.Error("directory deletion bypassed canonical ownership or authorization cleanup")
					}
					if directory.deleteErr == nil {
						directory.byObject = nil
					}
				}
				if err := reconciler.Delete(ctx, instance); err != nil {
					t.Fatal(err)
				}
				req := ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}}
				_, err := reconciler.Reconcile(ctx, req)
				wantFailure := failure == "authorization" || failure == "directory"
				if (err != nil) != wantFailure {
					t.Fatalf("identity cleanup failure was bypassed: %v", err)
				}
				if wantFailure {
					stored := &cetirunv1alpha1.Agent{}
					if err := reconciler.Get(ctx, req.NamespacedName, stored); err != nil ||
						!slices.Contains(stored.Finalizers, sandboxHoldFinalizer) || !slices.Contains(stored.Finalizers, cetirunv1alpha1.Finalizer) {
						t.Fatalf("cleanup failure released finalizers: %v", err)
					}
					if namespacePresent {
						if err := reconciler.Get(ctx, types.NamespacedName{Name: namespace.Name}, namespace); err != nil {
							t.Fatalf("namespace removed before identity cleanup succeeded: %v", err)
						}
					}
					if failure == "authorization" && directory.deleteCalls != 0 {
						t.Fatal("directory deletion preceded authorization cleanup")
					}
				}
				brokerStatus.Store(http.StatusNoContent)
				directory.deleteErr = nil
				for range 3 {
					if _, err := reconciler.Reconcile(ctx, req); err != nil {
						t.Fatal(err)
					}
				}
				if err := reconciler.Get(ctx, req.NamespacedName, &cetirunv1alpha1.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatalf("Agent cleanup did not converge: %v", err)
				}
				if err := reconciler.Get(ctx, types.NamespacedName{Name: namespace.Name}, namespace); !apierrors.IsNotFound(err) {
					t.Fatalf("namespace cleanup did not converge: %v", err)
				}
				wantDeletes := 1
				if failure == "directory" {
					wantDeletes = 2
				}
				if brokerCalls.Load() == 0 || directory.deleteCalls != wantDeletes || directory.createCalls != 0 {
					t.Fatalf("identity cleanup skipped or provisioned: broker=%d deletes=%d creates=%d", brokerCalls.Load(), directory.deleteCalls, directory.createCalls)
				}
			})
		}
	}
}
