// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconcileConnectionsMCPBrokerRecoversMissingNamespaceFromSteadyState(t *testing.T) {
	// Suspension deliberately never recreates a missing namespace: it must be
	// able to stop a workload without chart preparation. Namespace recovery is
	// therefore a Ready-state contract.
	for _, phase := range []string{api.PhaseReady} {
		t.Run(string(phase), func(t *testing.T) {
			instance := connectionsMCPBrokerRecoveryAgent(phase)
			reconciler, kube := newConnectionsMCPBrokerRecoveryReconciler(t, instance)
			// The absent namespace also means the gateway Secret is absent. Seed
			// the exact empty hash so this verifies that recovery bypasses a
			// matching steady-state cache rather than merely handling a cache miss.
			setConnectionsMCPBrokerRecoveryHash(t, kube, instance, reconciler, "")

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if reconciler.HelmClient.(*chartHelm).installs == 0 {
				t.Fatal("missing namespace recovery stopped before the full reconcile applied Helm")
			}

			namespace := &corev1.Namespace{}
			if err := kube.Get(context.Background(), types.NamespacedName{Name: instanceNamespace(instance.Name)}, namespace); err != nil {
				t.Fatalf("get recovered namespace: %v", err)
			}
			if !connectionsMCPBrokerOwned(namespace, instance) {
				t.Fatalf("recovered namespace is not owned by the Agent: %#v", namespace.OwnerReferences)
			}
			for _, object := range []client.Object{
				&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: connectionsMCPBrokerRoleName, Namespace: namespace.Name}},
				&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: connectionsMCPBrokerRoleBindingName, Namespace: namespace.Name}},
			} {
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatalf("get recovered Broker RBAC %T: %v", object, err)
				}
				if !connectionsMCPBrokerOwned(object, instance) {
					t.Fatalf("recovered Broker RBAC %T is not owned by the Agent", object)
				}
			}
		})
	}
}

func TestReconcileBuiltinRegistryMigrationRequiresExplicitImagePin(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		for _, migration := range []struct{ old, target string }{
			{"cetirun.azurecr.io", "mcr.microsoft.com/aks/ai-runtime/cetirun"},
			{"registry.example.com", "registry.example.com/team/test"},
		} {
			for _, scenario := range []string{"retain coordinates", "registry changed", "explicit new pin"} {
				t.Run(string(kind)+"/"+migration.target+"/"+scenario, func(t *testing.T) {
					instance := connectionsMCPBrokerRecoveryAgent(api.PhaseReady)
					instance.Spec.Agent.Type = kind
					if kind == api.AgentTypeOpenClaw {
						instance.Spec.Agent.Port = ptr.To[int32](18789)
					}
					image := migration.old + "/" + string(kind) + "-gateway:0.1.1"
					instance.Spec.Agent.Image = ptr.To(image)
					reconciler, kube := newConnectionsMCPBrokerRecoveryReconciler(t, instance)
					reconciler.ChartRegistry = migration.old
					setConnectionsMCPBrokerRecoveryHash(t, kube, instance, reconciler, "")
					if scenario != "retain coordinates" {
						reconciler.ChartRegistry = migration.target
						if reconciler.configHash(instance, nil, "", "") == instance.Status.ObservedOperatorHash {
							t.Fatal("registry change must invalidate the cached configuration")
						}
					}
					if scenario == "explicit new pin" {
						current := &api.Agent{}
						if err := kube.Get(context.Background(), client.ObjectKeyFromObject(instance), current); err != nil {
							t.Fatal(err)
						}
						image = migration.target + "/" + string(kind) + "-gateway:0.1.1"
						current.Spec.Agent.Image = ptr.To(image)
						current.Generation++
						if err := kube.Update(context.Background(), current); err != nil {
							t.Fatal(err)
						}
					}
					_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
					if scenario == "registry changed" {
						if err == nil || !strings.Contains(err.Error(), "AgentConfigInvalid: spec.agent.image is not qualified") {
							t.Fatalf("expected an unqualified image rejection, got %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					current := &api.Agent{}
					if err := kube.Get(context.Background(), client.ObjectKeyFromObject(instance), current); err != nil {
						t.Fatal(err)
					}
					if *current.Spec.Agent.Image != image {
						t.Fatal("reconciliation silently changed the image pin")
					}
					installs := reconciler.HelmClient.(*chartHelm).installs
					if scenario == "registry changed" {
						if installs != 0 || current.Status.Phase != api.PhaseFailed {
							t.Fatalf("unmigrated pin: installs=%d, phase=%s", installs, current.Status.Phase)
						}
					} else if installs == 0 {
						t.Fatal("explicitly selected image and registry did not reach Helm apply")
					}
				})
			}
		}
	}
}

func TestReconcileConnectionsMCPBrokerSuspensionDoesNotRecreateMissingNamespace(t *testing.T) {
	instance := connectionsMCPBrokerRecoveryAgent(api.PhaseSuspended)
	reconciler, kube := newConnectionsMCPBrokerRecoveryReconciler(t, instance)
	setConnectionsMCPBrokerRecoveryHash(t, kube, instance, reconciler, "")

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if reconciler.HelmClient.(*chartHelm).installs != 0 {
		t.Fatal("suspension recreated a missing workload through Helm")
	}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: instanceNamespace(instance.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("suspension recreated a missing namespace: %v", err)
	}
	current := &api.Agent{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(instance), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != api.PhaseSuspended {
		t.Fatalf("phase = %s, want Suspended", current.Status.Phase)
	}
}

func TestReconcileConnectionsMCPBrokerRepairsOwnedNamespaceAtSteadyState(t *testing.T) {
	instance := connectionsMCPBrokerRecoveryAgent(api.PhaseReady)
	now := metav1.Now()
	instance.Status.AIGateway = &api.AIGatewayStatus{
		Phase: api.AIGatewayPhaseReady, Routed: true, KeyID: "existing-key", KeySecretName: aiGatewayKeySecretName,
		LastVerifiedTime: &now,
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: instanceNamespace(instance.Name), OwnerReferences: connectionsMCPBrokerOwner(instance),
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Spec.GatewayTokenSecretRef, Namespace: namespace.Name,
	}, Data: map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte("synthetic-token"), "CETIRUN_AUTH_KEY": []byte("synthetic-key")}}
	reconciler, kube := newConnectionsMCPBrokerRecoveryReconciler(t, instance, namespace, secret)
	setConnectionsMCPBrokerRecoveryHash(t, kube, instance, reconciler, secretContentHash(secret.Data))

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if reconciler.HelmClient.(*chartHelm).installs != 0 {
		t.Fatal("steady-state Broker RBAC repair unexpectedly applied Helm")
	}
	for _, object := range []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: connectionsMCPBrokerRoleName, Namespace: namespace.Name}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: connectionsMCPBrokerRoleBindingName, Namespace: namespace.Name}},
	} {
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatalf("get repaired Broker RBAC %T: %v", object, err)
		}
		if !connectionsMCPBrokerOwned(object, instance) {
			t.Fatalf("repaired Broker RBAC %T is not owned by the Agent", object)
		}
	}
}

func TestReconcileConnectionsMCPBrokerRejectsUnverifiableNamespaceWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name      string
		namespace *corev1.Namespace
		getErr    error
	}{
		{
			name: "foreign namespace",
			namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name:   instanceNamespace("broker-recovery"),
				Labels: map[string]string{"test": "foreign"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: "other", UID: "other-agent",
				}},
			}},
		},
		{
			name: "unowned legacy namespace",
			namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: instanceNamespace("broker-recovery"), Labels: map[string]string{"test": "legacy"},
			}},
		},
		{
			name:   "forbidden namespace read",
			getErr: apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "cetirun-broker-recovery", errors.New("synthetic denial")),
		},
		{
			name:   "namespace read failure",
			getErr: fmt.Errorf("synthetic read failure"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := connectionsMCPBrokerRecoveryAgent(api.PhaseReady)
			objects := []client.Object{instance}
			if test.namespace != nil {
				objects = append(objects, test.namespace)
			}
			reconciler, kube := newConnectionsMCPBrokerRecoveryReconciler(t, objects...)
			setConnectionsMCPBrokerRecoveryHash(t, kube, instance, reconciler, "")
			if test.getErr != nil {
				reconciler.Client = &connectionsMCPBrokerNamespaceGetErrorClient{Client: kube, err: test.getErr}

			}

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if reconciler.HelmClient.(*chartHelm).installs != 0 {
				t.Fatal("namespace ownership failure unexpectedly applied Helm")
			}
			recorder := reconciler.EventRecorder.(*record.FakeRecorder)
			select {
			case event := <-recorder.Events:
				if !strings.Contains(event, "Warning BrokerStorageAccessFailed") {
					t.Fatalf("unexpected namespace verification event: %q", event)
				}
			default:
				t.Fatal("namespace verification failure did not emit a warning event")
			}
			roleKey := types.NamespacedName{Namespace: instanceNamespace(instance.Name), Name: connectionsMCPBrokerRoleName}
			if err := kube.Get(context.Background(), roleKey, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
				t.Fatalf("Broker Role was created after refusing namespace: %v", err)
			}
			if test.namespace != nil {
				stored := &corev1.Namespace{}
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(test.namespace), stored); err != nil {
					t.Fatal(err)
				}
				if !equalBrokerRecoveryNamespace(test.namespace, stored) {
					t.Fatalf("namespace was adopted or changed: got %#v, want %#v", stored.ObjectMeta, test.namespace.ObjectMeta)
				}
			}
		})
	}
}

func TestReconcileConnectionsMCPBrokerRoleNotFoundDoesNotTriggerNamespaceRecovery(t *testing.T) {
	instance := connectionsMCPBrokerRecoveryAgent(api.PhaseReady)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: instanceNamespace(instance.Name), OwnerReferences: connectionsMCPBrokerOwner(instance),
	}}
	reconciler, kube := newConnectionsMCPBrokerRecoveryReconciler(t, instance, namespace)
	setConnectionsMCPBrokerRecoveryHash(t, kube, instance, reconciler, "")
	roleReads := 0
	reconciler.Client = interceptor.NewClient(kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, isRole := object.(*rbacv1.Role); isRole && key.Name == connectionsMCPBrokerRoleName {
				roleReads++
				// Simulate a cache that still cannot observe the newly created Role.
				return apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"}, key.Name)
			}
			return c.Get(ctx, key, object, options...)
		},
	})
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if roleReads != 2 || reconciler.HelmClient.(*chartHelm).installs != 0 {
		t.Fatalf("Role NotFound entered the full reconcile: Role reads = %d", roleReads)
	}
	stored := &corev1.Namespace{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(namespace), stored); err != nil {
		t.Fatal(err)
	}
	if !equalBrokerRecoveryNamespace(namespace, stored) || stored.ResourceVersion != namespace.ResourceVersion {
		t.Fatal("Role NotFound modified the namespace")
	}
}

type connectionsMCPBrokerNamespaceGetErrorClient struct {
	client.Client
	err error
}

func (c *connectionsMCPBrokerNamespaceGetErrorClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, isNamespace := object.(*corev1.Namespace); isNamespace && key.Name == "cetirun-broker-recovery" {
		return c.err
	}
	return c.Client.Get(ctx, key, object, options...)
}

func equalBrokerRecoveryNamespace(want, got *corev1.Namespace) bool {
	return string(want.UID) == string(got.UID) &&
		reflect.DeepEqual(want.Labels, got.Labels) &&
		reflect.DeepEqual(want.OwnerReferences, got.OwnerReferences)
}

func newConnectionsMCPBrokerRecoveryReconciler(t *testing.T, objects ...client.Object) (*AgentReconciler, client.WithWatch) {
	t.Helper()
	operatorScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(operatorScheme))
	utilruntime.Must(api.AddToScheme(operatorScheme))
	utilruntime.Must(istiov1beta1.AddToScheme(operatorScheme))
	kube := fake.NewClientBuilder().WithScheme(operatorScheme).WithObjects(objects...).WithStatusSubresource(&api.Agent{}).Build()
	reconciler := &AgentReconciler{
		Client:                             kube,
		Scheme:                             operatorScheme,
		EventRecorder:                      record.NewFakeRecorder(10),
		HelmClient:                         &chartHelm{},
		OperatorNamespace:                  "cetirun-system",
		OperatorSAName:                     "cetirun-operator-controller-manager",
		ConnectionsMCPBrokerNamespace:      "cetirun-credentials",
		ConnectionsMCPBrokerServiceAccount: "cetirun-agent-broker",
	}
	// These cases model a ready standalone runtime. Configure the Gateway
	// contract rather than relying on the retired direct-provider path to reach
	// the Broker-RBAC recovery behavior under test.
	ConfigureTestGateway(t, reconciler)
	return reconciler, kube
}

func connectionsMCPBrokerRecoveryAgent(phase string) *api.Agent {
	return &api.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "broker-recovery", UID: "broker-recovery-agent", Generation: 3,
			Finalizers: []string{api.Finalizer},
		},
		Spec: api.AgentSpec{
			Agent: api.HarnessSpec{
				Type: api.AgentTypeHermes, Image: ptr.To("mcr.microsoft.com/aks/ai-runtime/cetirun/hermes-gateway:0.1.1"), Port: ptr.To[int32](8642),
			},
			User:                  api.UserSpec{Alias: "broker-recovery", DisplayName: "Broker Recovery"},
			GatewayTokenSecretRef: "gateway-token",
			Helm:                  api.HelmSpec{Version: "0.1.28", Values: map[string]runtime.RawExtension{}},
			Suspended:             phase == api.PhaseSuspended,
		},
		Status: api.AgentStatus{Phase: phase, ObservedGeneration: 3},
	}
}

func setConnectionsMCPBrokerRecoveryHash(t *testing.T, kube client.Client, instance *api.Agent, reconciler *AgentReconciler, gatewaySecretHash string) {
	t.Helper()
	stored := &api.Agent{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(instance), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.ObservedOperatorHash = reconciler.configHash(stored, nil, gatewaySecretHash, "")
	if err := kube.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	instance.Status.ObservedOperatorHash = stored.Status.ObservedOperatorHash
}
