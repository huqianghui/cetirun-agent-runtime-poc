// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func withNativeConnections(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		ctx := context.Background()
		r.ConnectionsMCPNativeEnabled = true
		r.ConnectionsMCPURL = "https://connections.cetirun-tools.svc.cluster.local/mcp"
		r.ConnectionsMCPAudience = "cetirun-connections-mcp"
		r.ConnectionsMCPCASecretName = "connections-ca"
		r.ConnectionsMCPBrokerNamespace = "cetirun-credentials"
		r.ConnectionsMCPBrokerServiceAccount = "broker"
		peer := sandboxpolicy.Peer{Name: "connections", Namespace: "cetirun-tools",
			UID: "connections-peer", IPs: []string{"10.96.2.40", "10.244.8.10"}}
		for _, object := range []client.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: peer.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: peer.Namespace, Name: peer.Name},
				Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: peer.IPs[0],
					Selector: map[string]string{sandboxpolicy.PeerUIDLabel: string(peer.UID)},
					Ports:    []corev1.ServicePort{{Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt32(8443)}}}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.OperatorNamespace, Name: r.ConnectionsMCPCASecretName},
				Data: map[string][]byte{"ca.crt": connectionsMCPTestCA(t, 101)}},
		} {
			if service, ok := object.(*corev1.Service); ok {
				current := &corev1.Service{}
				if err := kube.Get(ctx, client.ObjectKeyFromObject(service), current); err == nil {
					continue
				}
			}
			if err := kube.Create(ctx, object); client.IgnoreAlreadyExists(err) != nil {
				t.Fatal(err)
			}
		}
		actualService := &corev1.Service{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: peer.Namespace, Name: peer.Name}, actualService); err != nil {
			t.Fatal(err)
		}
		peer.IPs[0] = actualService.Spec.ClusterIP
		configuration := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, configuration); err != nil {
			t.Fatal(err)
		}
		var document SandboxPlatformConfiguration
		if json.Unmarshal([]byte(configuration.Data[SandboxPlatformConfigurationKey]), &document) != nil {
			t.Fatal("invalid test platform configuration")
		}
		platform := document.Platforms[api.SandboxKata]
		platform.Topology.Peers = append(platform.Topology.Peers, peer)
		document.Platforms[api.SandboxKata] = platform
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		configuration.Data[SandboxPlatformConfigurationKey] = string(data)
		if err := kube.Update(ctx, configuration); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeConnectionsDestinationPortBounds(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false, withNativeConnections(t))
	configuration := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, configuration); err != nil {
		t.Fatal(err)
	}
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(configuration.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		port        string
		servicePort int32
		targetPort  intstr.IntOrString
		want        []int32
	}{
		{name: "default HTTPS", servicePort: 443, targetPort: intstr.FromInt32(8443), want: []int32{443, 8443}},
		{name: "explicit HTTPS", port: "443", servicePort: 443, targetPort: intstr.FromString("https"), want: []int32{443, 8443}},
		{name: "leading zeroes", port: "00443", servicePort: 443, targetPort: intstr.FromInt32(8443), want: []int32{443, 8443}},
		{name: "minimum", port: "1", servicePort: 1, targetPort: intstr.FromInt32(1), want: []int32{1}},
		{name: "unsigned range", port: "32768", servicePort: 32768, targetPort: intstr.FromInt32(65535), want: []int32{32768, 65535}},
		{name: "maximum", port: "65535", servicePort: 65535, targetPort: intstr.FromInt32(65535), want: []int32{65535}},
		{name: "default target", port: "65535", servicePort: 65535, want: []int32{65535}},
		{name: "zero", port: "0", servicePort: 443},
		{name: "above maximum", port: "65536", servicePort: 443},
		{name: "int32 overflow", port: "2147483648", servicePort: 443},
		{name: "would wrap to HTTPS", port: "4294967739", servicePort: 443},
		{name: "uint64 overflow", port: "18446744073709551616", servicePort: 443},
		{name: "negative", port: "-1", servicePort: 443},
		{name: "nonnumeric", port: "https", servicePort: 443},
		{name: "different Service port", port: "8443", servicePort: 443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority := "connections.cetirun-tools.svc.cluster.local"
			if tc.port != "" {
				authority += ":" + tc.port
			}
			f.r.ConnectionsMCPURL = "https://" + authority + "/mcp"
			service := &corev1.Service{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: "cetirun-tools", Name: "connections"}, service); err != nil {
				t.Fatal(err)
			}
			service.Spec.Ports = []corev1.ServicePort{{
				Name: "https", Protocol: corev1.ProtocolTCP, Port: tc.servicePort, TargetPort: tc.targetPort,
			}}
			if err := f.kube.Update(ctx, service); err != nil {
				t.Fatal(err)
			}
			destination, observed, err := f.r.nativeConnectionsDestination(ctx, f.current(t), document.Platforms[api.SandboxKata])
			if tc.want == nil {
				if err == nil || destination != nil || observed != nil {
					t.Fatal("invalid port produced a managed destination")
				}
				return
			}
			if err != nil || destination == nil || observed == nil {
				t.Fatalf("valid port was rejected: %v", err)
			}
			if observed.UID != service.UID || len(destination.Ports) != len(tc.want) {
				t.Fatal("destination lost its Service identity or port set")
			}
			for i, port := range destination.Ports {
				if port.Protocol != corev1.ProtocolTCP || port.Port != tc.want[i] {
					t.Fatalf("unexpected port at %d: %+v", i, port)
				}
			}
		})
	}
}

func TestNativeConnectionsComposeWithEntraIdentityPin(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			f := nativeEntraFixture(t, kind, withNativeConnections(t))
			current := f.current(t)
			expected := reconciledExpectationForTest(t, f)
			workload := f.workload(t)
			if workload.Spec.Template.Spec.ServiceAccountName != "example-entra" {
				t.Fatal("Connections replaced the approved Entra ServiceAccount")
			}
			entra := false
			for _, volume := range workload.Spec.Template.Spec.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					if token := source.ServiceAccountToken; token != nil && token.Audience == "api://AzureADTokenExchange" {
						entra = true
					}
				}
			}
			if !entra {
				t.Fatal("Connections removed the independent Entra projection")
			}
			contract := &w.ConnectionsMCP{URL: f.r.ConnectionsMCPURL, Namespace: "cetirun-tools",
				Audience: f.r.ConnectionsMCPAudience, CASecretName: f.r.ConnectionsMCPCASecretName}
			if err := contract.ValidatePod(workload.Spec.Template.Spec); err != nil {
				t.Fatal("combined workload lost the Connections purpose proof:", err)
			}
			policy, err := f.r.resolvePolicy(context.Background(), current)
			if err != nil || policy.sandboxRevision != expected.Revision {
				t.Fatal("desired policy and applied compilation disagree on Entra/Connections inputs:", err)
			}
		})
	}
}

func TestNativeConnectionsUsesExistingCurrentSASource(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, withNativeConnections(t))
			expected := reconciledExpectationForTest(t, f)
			workload := f.workload(t)
			account := &corev1.ServiceAccount{}
			if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: workload.Namespace, Name: workload.Spec.Template.Spec.ServiceAccountName}, account); err != nil {
				t.Fatal(err)
			}

			if account.Name != "default" {
				t.Fatal("Connections introduced a dedicated-SA lifecycle instead of reusing the current source")
			}
			pinned := false
			for _, source := range expected.Sources {
				if source.Kind == "ServiceAccount" && source.Name == account.Name &&
					source.Namespace == account.Namespace && source.UID == account.UID && source.ResourceVersion != "" {
					pinned = true
				}
			}
			if !pinned {
				t.Fatal("Connections current SA UID is not in the common source authority")
			}
			contract := &w.ConnectionsMCP{URL: f.r.ConnectionsMCPURL, Namespace: "cetirun-tools",
				Audience: f.r.ConnectionsMCPAudience, CASecretName: f.r.ConnectionsMCPCASecretName}
			if err := contract.ValidatePod(workload.Spec.Template.Spec); err != nil {
				t.Fatal(err)
			}
			for _, object := range []client.Object{&rbacv1.Role{}, &rbacv1.RoleBinding{}} {
				if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: workload.Namespace, Name: connectionsMCPBrokerRoleName}, object); err != nil {
					t.Fatal("native Broker storage grant is unavailable:", err)
				}
				if !connectionsMCPBrokerOwned(object, f.current(t)) {
					t.Fatal("native Broker storage grant is not exactly Agent-owned")
				}
			}
			if expected.Workloads[0].Template.Spec.AutomountServiceAccountToken == nil ||
				*expected.Workloads[0].Template.Spec.AutomountServiceAccountToken {
				t.Fatal("unexpected automatic token mount")
			}
		})
	}
}

func TestNativeConnectionsSuspensionKeepsHomeAndKey(t *testing.T) {
	testReconciledSandboxSuspension(t, withNativeConnections(t))
}

func TestNativeConnectionsRetainedRestartKeepsCurrentBindings(t *testing.T) {
	testReconciledSandboxRetainedRestart(t, withNativeConnections(t))
}

func TestNativeConnectionsPreservesRuntimeAllowlist(t *testing.T) {
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "hermes", true, false,
		withNativeConnections(t), func(r *AgentReconciler, _ client.Client) {
			r.ConnectionsMCPRuntimeTypes = []string{"openclaw"}
		})
	if f.r.nativeConnectionsRequested(f.current(t)) {
		t.Fatal("native integration ignored the existing runtime allowlist")
	}
	for _, volume := range f.workload(t).Spec.Template.Spec.Volumes {
		if volume.Name == "connections-identity" {
			t.Fatal("excluded runtime received a Connections proof")
		}
	}
	if len(f.gateway.generated) != 1 {
		t.Fatal("an excluded integration prevented ordinary native model preparation")
	}
}

func TestNativeConnectionsDisableRemovesProofButRetainsOwnedCleanupAccess(t *testing.T) {
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "hermes", true, false, withNativeConnections(t))
	ctx := context.Background()
	initial := reconciledExpectationForTest(t, f)
	role := &rbacv1.RoleBinding{}
	key := client.ObjectKey{Namespace: initial.Namespace.Name, Name: connectionsMCPBrokerRoleName}
	if err := f.kube.Get(ctx, key, role); err != nil {
		t.Fatal(err)
	}
	roleUID := role.UID
	keyID := f.current(t).Status.AIGateway.KeyID
	f.r.ConnectionsMCPNativeEnabled = false
	for range 3 {
		f.reconcileOK(t)
	}
	workload := f.workload(t)
	for _, volume := range workload.Spec.Template.Spec.Volumes {
		if volume.Name == "connections-identity" {
			t.Fatal("disabled native integration retained its token projection")
		}
	}
	if err := f.kube.Get(ctx, key, role); err != nil || role.UID != roleUID || !connectionsMCPBrokerOwned(role, f.current(t)) {
		t.Fatal("disable removed the existing Agent-owned metadata/cleanup grant:", err)
	}
	current := reconciledExpectationForTest(t, f)
	if current.Home != initial.Home || current.Namespace != initial.Namespace ||
		f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
		t.Fatal("native disable replaced retained storage or spending identity")
	}
	f.r.ConnectionsMCPNativeEnabled = true
	for range 3 {
		f.reconcileOK(t)
	}
	contract := &w.ConnectionsMCP{URL: f.r.ConnectionsMCPURL, Namespace: "cetirun-tools",
		Audience: f.r.ConnectionsMCPAudience, CASecretName: f.r.ConnectionsMCPCASecretName}
	if err := contract.ValidatePod(f.workload(t).Spec.Template.Spec); err != nil {
		t.Fatal("native re-enable did not restore the purpose-bound contract:", err)
	}
}
