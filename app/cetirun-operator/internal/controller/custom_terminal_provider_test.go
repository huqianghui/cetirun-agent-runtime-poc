// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/terminal"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTerminalAPIEgressUsesOnlyDiscoveredAddresses(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.1", ClusterIPs: []string{"10.96.0.1"},
			Ports: []corev1.ServicePort{{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP}}}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default",
		Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Port: ptr.To[int32](6443), Protocol: ptr.To(corev1.ProtocolTCP)}},
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.10"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service, slice).Build()
	r := &AgentReconciler{Client: c}
	got, err := r.terminalAPIEndpoints(context.Background())
	want := []runtimebindings.APIServerEndpoint{{IP: "10.96.0.1", Port: 443}, {IP: "192.0.2.10", Port: 6443}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
	slice.Endpoints[0].Addresses = []string{"not-an-address"}
	if err := c.Update(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	if _, err := r.terminalAPIEndpoints(context.Background()); err == nil {
		t.Fatal("missing backend addresses were silently accepted")
	}
}

func TestTerminalReadRoleBootstrapDoesNotGrantExec(t *testing.T) {
	p, agent := readyProvider(t)
	ctx := context.Background()
	if err := p.reconciler.ensureTerminalReadRoles(ctx, agent); err != nil {
		t.Fatal(err)
	}
	name := terminalpolicy.WorkloadName(instanceServiceName(agent.Name))
	role := &rbacv1.Role{}
	if err := p.reconciler.Get(ctx, types.NamespacedName{Namespace: instanceNamespace(agent.Name), Name: name}, role); err != nil {
		t.Fatal(err)
	}
	for _, rule := range role.Rules {
		if !reflect.DeepEqual(rule.Verbs, []string{"get"}) || len(rule.ResourceNames) != 1 {
			t.Fatal("read role widened permissions")
		}
	}
	reader := &rbacv1.ClusterRole{}
	if err := p.reconciler.Get(ctx, types.NamespacedName{Name: terminalpolicy.AgentReaderName(string(agent.UID))}, reader); err != nil {
		t.Fatal(err)
	}
	if len(reader.Rules) != 1 || !reflect.DeepEqual(reader.Rules[0].ResourceNames, []string{agent.Name}) ||
		!reflect.DeepEqual(reader.Rules[0].Verbs, []string{"get"}) {
		t.Fatal("Agent reader is not instance-scoped")
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := p.reconciler.List(ctx, bindings, client.InNamespace(instanceNamespace(agent.Name))); err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings.Items {
		if binding.RoleRef.Name == terminal.ExecRole {
			t.Fatal("bootstrap activated exec before Chart validation")
		}
	}
	role.OwnerReferences[0].UID = "foreign-owner"
	if err := p.reconciler.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := p.reconciler.ensureTerminalReadRoles(ctx, agent); err == nil {
		t.Fatal("foreign role was adopted")
	}
}
