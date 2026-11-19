// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	connectionsMCPTestOperatorNamespace = "cetirun-system"
	connectionsMCPTestSecretName        = "cetirun-connections-mcp-ca"
)

func TestConnectionsMCPRuntimeAllowlistDefaultsAndFailsClosed(t *testing.T) {
	defaults, err := ParseConnectionsMCPRuntimeTypes("")
	if err != nil || !reflect.DeepEqual(defaults, []string{"hermes", "openclaw"}) {
		t.Fatalf("default Connections MCP runtime types = %#v, %v", defaults, err)
	}
	parsed, err := ParseConnectionsMCPRuntimeTypes(" openclaw,hermes;openclaw ")
	if err != nil || !reflect.DeepEqual(parsed, []string{"hermes", "openclaw"}) {
		t.Fatalf("normalized Connections MCP runtime types = %#v, %v", parsed, err)
	}
	custom, err := ParseConnectionsMCPRuntimeTypes("custom")
	if err != nil || !reflect.DeepEqual(custom, []string{"custom"}) {
		t.Fatalf("explicit Custom Connections opt-in = %#v, %v", custom, err)
	}
	for _, raw := range []string{"unknown", "hermes,unknown", ",;"} {
		if _, err := ParseConnectionsMCPRuntimeTypes(raw); err == nil {
			t.Fatalf("invalid Connections MCP allowlist %q was accepted", raw)
		}
	}
}

func TestDisabledConnectionsMCPRuntimeStillReceivesBrokerCleanupRBAC(t *testing.T) {
	ctx := context.Background()
	instance := connectionsMCPTestAgent()
	instance.Spec.Agent.Type = api.AgentTypeOpenClaw
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: instanceNamespace(instance.Name), OwnerReferences: connectionsMCPBrokerOwner(instance),
	}}
	reconciler, kube := nativeMCPTestReconciler(t, namespace)
	reconciler.ConnectionsMCPRuntimeTypes = []string{"hermes"}
	reconciler.ConnectionsMCPBrokerNamespace = "cetirun-credentials"
	reconciler.ConnectionsMCPBrokerServiceAccount = "cetirun-agent-broker"

	enabled, err := reconciler.connectionsMCPEnabledFor(instance)
	if err != nil || enabled {
		t.Fatalf("OpenClaw Connections MCP runtime gate = %v, %v", enabled, err)
	}
	if err := reconciler.reconcileConnectionsMCPBrokerRBAC(ctx, instance, !sandboxRequested(instance, nil)); err != nil {
		t.Fatalf("Broker cleanup RBAC for disabled runtime: %v", err)
	}
	roleKey := types.NamespacedName{Namespace: namespace.Name, Name: connectionsMCPBrokerRoleName}
	role := &rbacv1.Role{}
	if err := kube.Get(ctx, roleKey, role); err != nil {
		t.Fatalf("get Broker cleanup Role: %v", err)
	}
	if len(role.Rules) != 1 || !reflect.DeepEqual(role.Rules[0], rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "create", "update", "delete"},
	}) {
		t.Fatalf("Broker cleanup Role grants = %#v", role.Rules)
	}
	binding := &rbacv1.RoleBinding{}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: connectionsMCPBrokerRoleBindingName}, binding); err != nil {
		t.Fatalf("get Broker cleanup RoleBinding: %v", err)
	}
	wantSubject := []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "cetirun-credentials", Name: "cetirun-agent-broker"}}
	if !reflect.DeepEqual(binding.Subjects, wantSubject) {
		t.Fatalf("Broker cleanup RoleBinding subjects = %#v, want %#v", binding.Subjects, wantSubject)
	}
}

func TestEnsureConnectionsMCPBrokerCleanupRBACHandlesUpgradeDeletion(t *testing.T) {
	ctx := context.Background()
	instance := connectionsMCPTestAgent()
	configure := func(reconciler *AgentReconciler) {
		reconciler.ConnectionsMCPBrokerNamespace = "cetirun-credentials"
		reconciler.ConnectionsMCPBrokerServiceAccount = "cetirun-agent-broker"
	}
	t.Run("live owned namespace creates grant", func(t *testing.T) {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: instanceNamespace(instance.Name), OwnerReferences: connectionsMCPBrokerOwner(instance),
		}}
		reconciler, kube := nativeMCPTestReconciler(t, namespace)
		configure(reconciler)
		if err := reconciler.ensureConnectionsMCPBrokerCleanupRBAC(ctx, instance); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: connectionsMCPBrokerRoleName}, &rbacv1.Role{}); err != nil {
			t.Fatalf("cleanup Role was not created before Broker cleanup: %v", err)
		}
	})
	t.Run("missing namespace is idempotent", func(t *testing.T) {
		reconciler, _ := connectionsMCPTestReconciler(t)
		configure(reconciler)
		if err := reconciler.ensureConnectionsMCPBrokerCleanupRBAC(ctx, instance); err != nil {
			t.Fatalf("missing namespace blocked cleanup: %v", err)
		}
	})
	t.Run("terminating owned namespace only verifies", func(t *testing.T) {
		now := metav1.Now()
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: instanceNamespace(instance.Name), OwnerReferences: connectionsMCPBrokerOwner(instance), DeletionTimestamp: &now,
			Finalizers: []string{"test.example.com/hold"},
		}}
		seed := &AgentReconciler{ConnectionsMCPBrokerNamespace: "cetirun-credentials", ConnectionsMCPBrokerServiceAccount: "cetirun-agent-broker"}
		role, binding := seed.desiredConnectionsMCPBrokerRBAC(instance)
		reconciler, kube := nativeMCPTestReconciler(t, namespace, role, binding)
		configure(reconciler)
		if err := reconciler.ensureConnectionsMCPBrokerCleanupRBAC(ctx, instance); err != nil {
			t.Fatal(err)
		}
		got := &rbacv1.Role{}
		if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: role.Name}, got); err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != role.ResourceVersion {
			t.Fatalf("terminating namespace Role was modified: %q -> %q", role.ResourceVersion, got.ResourceVersion)
		}
		gotBinding := &rbacv1.RoleBinding{}
		if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: binding.Name}, gotBinding); err != nil {
			t.Fatal(err)
		}
		if gotBinding.ResourceVersion != binding.ResourceVersion {
			t.Fatalf("terminating namespace RoleBinding was modified: %q -> %q", binding.ResourceVersion, gotBinding.ResourceVersion)
		}
	})
}

func TestConnectionsMCPRuntimeGateOnlyEnablesAllowedRuntime(t *testing.T) {
	config := AgentIdentityHelmConfig{
		ConnectionsMCPURL:          "https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp",
		ConnectionsMCPAudience:     "cetirun-connections-mcp",
		ConnectionsMCPCASecretName: "cetirun-connections-mcp-ca",
		ConnectionsMCPRuntimeTypes: []string{"hermes"},
	}
	for _, runtimeType := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		t.Run(string(runtimeType), func(t *testing.T) {
			instance := newTestInstance("tools", "Tools", "", "gateway-token")
			instance.Spec.Agent.Type = runtimeType
			values, err := buildHelmValuesWithAgentIdentity(instance, nil, "example.com", "", nil, "", "", config)
			if err != nil {
				t.Fatal(err)
			}
			connectionsMCP := values["connectionsMCP"].(map[string]interface{})
			enabled, _ := connectionsMCP["enabled"].(bool)
			if enabled != (runtimeType == api.AgentTypeHermes) {
				t.Fatalf("Connections MCP enabled for %s = %v", runtimeType, enabled)
			}
			if !enabled {
				if _, present := connectionsMCP["caSecretName"]; present {
					t.Fatalf("disabled runtime received a Connections MCP CA projection: %#v", connectionsMCP)
				}
				if connectionsMCP["url"] != "" {
					t.Fatalf("disabled runtime received a Connections MCP endpoint: %#v", connectionsMCP)
				}
			}
		})
	}
}

func TestConnectionsMCPRuntimeGateParticipatesInOperatorHash(t *testing.T) {
	instance := newTestInstance("tools", "Tools", "", "gateway-token")
	instance.Spec.Agent.Type = api.AgentTypeOpenClaw
	reconciler := &AgentReconciler{
		ConnectionsMCPURL:          "https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp",
		ConnectionsMCPRuntimeTypes: []string{"hermes"},
	}
	disabled := reconciler.configHash(instance, nil, "", "")
	reconciler.ConnectionsMCPRuntimeTypes = []string{"hermes", "openclaw"}
	enabled := reconciler.configHash(instance, nil, "", "")
	if disabled == enabled {
		t.Fatal("enabling Connections MCP for this runtime did not invalidate the operator hash")
	}
	reconciler.ConnectionsMCPRuntimeTypes = []string{"openclaw", "hermes"}
	if reordered := reconciler.configHash(instance, nil, "", ""); reordered != enabled {
		t.Fatalf("allowlist ordering changed operator hash: %q != %q", reordered, enabled)
	}
}

func TestConnectionsMCPOnlyDeletionBlocksUntilGrantsAreRevoked(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			instance := connectionsMCPTestAgent()
			instance.UID = "11111111-1111-1111-1111-111111111111"
			instance.Finalizers = []string{api.Finalizer}
			reconciler, kube := nativeMCPTestReconciler(t, instance)
			if err := istiov1beta1.AddToScheme(reconciler.Scheme); err != nil {
				t.Fatal(err)
			}
			reconciler.EventRecorder = record.NewFakeRecorder(10)
			reconciler.HelmClient = &suspensionHelmClient{}
			calls := 0
			broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if request.Method != http.MethodDelete || request.URL.Path != "/operator/v1/instances/11111111-1111-1111-1111-111111111111/authorization" {
					t.Error("unexpected cleanup request")
				}
				writer.WriteHeader(status)
			}))
			defer broker.Close()
			reconciler.AgentIdentityBrokerURL = broker.URL
			reconciler.AgentIdentityHTTPClient = broker.Client()
			result, err := reconciler.handleDelete(context.Background(), instance)
			if err != nil || calls != 1 {
				t.Fatalf("cleanup request missing: %v, calls %d", err, calls)
			}
			var current api.Agent
			if err := kube.Get(context.Background(), types.NamespacedName{Name: instance.Name}, &current); err != nil {
				t.Fatal(err)
			}
			if status == http.StatusServiceUnavailable && (len(current.Finalizers) == 0 || result.RequeueAfter == 0) {
				t.Fatal("failed grant revocation removed finalizer")
			}
			if status == http.StatusNoContent && len(current.Finalizers) != 0 {
				t.Fatal("successful cleanup retained finalizer")
			}
		})
	}
}

func TestDisabledConnectionsMCPRuntimeStillCleansLegacyGrantsOnDelete(t *testing.T) {
	instance := connectionsMCPTestAgent()
	instance.UID = "11111111-1111-1111-1111-111111111111"
	instance.Spec.Agent.Type = api.AgentTypeOpenClaw
	instance.Finalizers = []string{api.Finalizer}
	reconciler, _ := nativeMCPTestReconciler(t, instance)
	reconciler.ConnectionsMCPRuntimeTypes = []string{"hermes"}
	if enabled, err := reconciler.connectionsMCPEnabledFor(instance); err != nil || enabled {
		t.Fatalf("OpenClaw runtime gate = %v, %v", enabled, err)
	}
	if err := istiov1beta1.AddToScheme(reconciler.Scheme); err != nil {
		t.Fatal(err)
	}
	reconciler.EventRecorder = record.NewFakeRecorder(10)
	reconciler.HelmClient = &suspensionHelmClient{}
	calls := 0
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer broker.Close()
	reconciler.AgentIdentityBrokerURL = broker.URL
	reconciler.AgentIdentityHTTPClient = broker.Client()
	if _, err := reconciler.handleDelete(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("disabled runtime skipped legacy grant cleanup; calls = %d", calls)
	}
}

func TestAgentGrantCleanupRejectsRedirects(t *testing.T) {
	redirectedCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirectedCalls++
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	broker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer broker.Close()
	reconciler := &AgentReconciler{AgentIdentityBrokerURL: broker.URL, AgentIdentityHTTPClient: broker.Client()}
	err := reconciler.deleteAgentAuthorization(context.Background(), "11111111-1111-1111-1111-111111111111")
	if err == nil || redirectedCalls != 0 {
		t.Fatalf("redirected cleanup = %v, target calls %d", err, redirectedCalls)
	}
}

func TestEnsureConnectionsMCPCACreatesOwnerBoundPublicCASecret(t *testing.T) {
	ctx := context.Background()
	ca := connectionsMCPTestCA(t, 1)
	source := connectionsMCPTestSource(ca)
	reconciler, kube := nativeMCPTestReconciler(t, source)
	instance := connectionsMCPTestAgent()

	if err := reconciler.ensureConnectionsMCPCA(ctx, instance); err != nil {
		t.Fatalf("ensureConnectionsMCPCA() error = %v", err)
	}
	target := &corev1.Secret{}
	key := types.NamespacedName{Namespace: instanceNamespace(instance.Name), Name: connectionsMCPTestSecretName}
	if err := kube.Get(ctx, key, target); err != nil {
		t.Fatalf("get copied CA Secret: %v", err)
	}
	if len(target.Data) != 1 || string(target.Data["ca.crt"]) != string(ca) {
		t.Fatalf("target data = %#v, want only ca.crt", target.Data)
	}
	if len(target.OwnerReferences) != 1 || target.OwnerReferences[0].UID != instance.UID || target.OwnerReferences[0].Kind != "Agent" || target.OwnerReferences[0].Name != instance.Name {
		t.Fatalf("target owner references = %#v", target.OwnerReferences)
	}
}

func TestEnsureConnectionsMCPCARotationUpdatesSecretAndHash(t *testing.T) {
	ctx := context.Background()
	firstCA, secondCA := connectionsMCPTestCA(t, 1), connectionsMCPTestCA(t, 2)
	reconciler, kube := nativeMCPTestReconciler(t, connectionsMCPTestSource(firstCA))
	instance := connectionsMCPTestAgent()
	if err := reconciler.ensureConnectionsMCPCA(ctx, instance); err != nil {
		t.Fatalf("initial ensureConnectionsMCPCA() error = %v", err)
	}
	firstHash, err := reconciler.connectionsMCPCAHash(ctx)
	if err != nil || firstHash == "" {
		t.Fatalf("initial connectionsMCPCAHash() = %q, %v", firstHash, err)
	}
	source := &corev1.Secret{}
	sourceKey := types.NamespacedName{Namespace: connectionsMCPTestOperatorNamespace, Name: connectionsMCPTestSecretName}
	if err := kube.Get(ctx, sourceKey, source); err != nil {
		t.Fatal(err)
	}
	source.Data = map[string][]byte{"ca.crt": secondCA, "tls.key": []byte("source-private-key")}
	if err := kube.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
	secondHash, err := reconciler.connectionsMCPCAHash(ctx)
	if err != nil || secondHash == "" || secondHash == firstHash {
		t.Fatalf("rotated connectionsMCPCAHash() = %q, %v; first = %q", secondHash, err, firstHash)
	}
	if err := reconciler.ensureConnectionsMCPCA(ctx, instance); err != nil {
		t.Fatalf("rotated ensureConnectionsMCPCA() error = %v", err)
	}
	target := &corev1.Secret{}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: instanceNamespace(instance.Name), Name: connectionsMCPTestSecretName}, target); err != nil {
		t.Fatal(err)
	}
	if len(target.Data) != 1 || string(target.Data["ca.crt"]) != string(secondCA) {
		t.Fatalf("rotated target data = %#v", target.Data)
	}
}

func TestEnsureConnectionsMCPCARejectsUnownedTarget(t *testing.T) {
	ctx := context.Background()
	instance := connectionsMCPTestAgent()
	target := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: connectionsMCPTestSecretName, Namespace: instanceNamespace(instance.Name), OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: "other", UID: "other-agent"}}},
		Data:       map[string][]byte{"ca.crt": []byte("unowned")},
	}
	reconciler, kube := nativeMCPTestReconciler(t, connectionsMCPTestSource(connectionsMCPTestCA(t, 1)), target)
	if err := reconciler.ensureConnectionsMCPCA(ctx, instance); err == nil {
		t.Fatal("ensureConnectionsMCPCA() accepted an unowned target")
	}
	stored := &corev1.Secret{}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: target.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Data["ca.crt"]) != "unowned" {
		t.Fatalf("unowned target was modified: %#v", stored.Data)
	}
}

func TestConnectionsMCPCARejectsMissingAndInvalidSources(t *testing.T) {
	tests := []struct {
		name   string
		source *corev1.Secret
	}{
		{name: "missing source"},
		{name: "missing CA", source: connectionsMCPTestSource(nil)},
		{name: "invalid PEM", source: connectionsMCPTestSource([]byte("not a certificate"))},
		{name: "private key material", source: connectionsMCPTestSource(append(connectionsMCPTestCA(t, 1), []byte("-----BEGIN PRIVATE KEY-----")...))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := []client.Object{}
			if test.source != nil {
				objects = append(objects, test.source)
			}
			reconciler, kube := nativeMCPTestReconciler(t, objects...)
			if _, err := reconciler.connectionsMCPCA(context.Background()); err == nil {
				t.Fatal("connectionsMCPCA() accepted an unavailable or invalid source")
			}
			if err := reconciler.ensureConnectionsMCPCA(context.Background(), connectionsMCPTestAgent()); err == nil {
				t.Fatal("ensureConnectionsMCPCA() accepted an unavailable or invalid source")
			}
			target := &corev1.Secret{}
			key := types.NamespacedName{Namespace: instanceNamespace(connectionsMCPTestAgent().Name), Name: connectionsMCPTestSecretName}
			if err := kube.Get(context.Background(), key, target); err == nil {
				t.Fatal("target Secret was created from an unavailable or invalid source")
			}
		})
	}
}

func connectionsMCPTestSource(ca []byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: connectionsMCPTestSecretName, Namespace: connectionsMCPTestOperatorNamespace}, Data: map[string][]byte{"ca.crt": ca, "tls.key": []byte("source-private-key")}}
}

func connectionsMCPTestCA(t *testing.T, serial int64) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "connections-mcp-test-ca"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func nativeMCPTestReconciler(t *testing.T, objects ...client.Object) (*AgentReconciler, client.Client) {
	t.Helper()
	reconciler, kube := connectionsMCPTestReconciler(t, objects...)
	reconciler.ConnectionsMCPURL = "https://cetirun-connections-mcp.example.com/mcp"
	reconciler.ConnectionsMCPCASecretName = connectionsMCPTestSecretName
	reconciler.ConnectionsMCPBrokerNamespace = "cetirun-credentials"
	reconciler.ConnectionsMCPBrokerServiceAccount = "cetirun-agent-broker"
	return reconciler, kube
}
