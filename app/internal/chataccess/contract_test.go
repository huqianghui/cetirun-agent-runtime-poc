// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chataccess

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestConfigurationAndScope(t *testing.T) {
	for _, raw := range []string{`{"enabled":true}`, `{"enabled":true,"enabled":false}`, `{"enabled":false,"unknown":1}`, `{"enabled":true,"callers":[{"namespace":"system","podLabels":{}}]}`} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("accepted invalid config: %s", raw)
		}
	}
	if _, err := Parse(`{"enabled":true,"callers":[{"namespace":"system","podLabels":{"app":"portal"}}]}`); err != nil {
		t.Fatal(err)
	}
	if StorageScope("realm", "subject") == StorageScope("other", "subject") || StorageScope("realm", "subject") == StorageScope("realm", "other") || StorageScope("", "subject") != "" {
		t.Fatal("identity partition collision or fallback")
	}
	if StorageScope("ab", "c") == StorageScope("a", "bc") {
		t.Fatal("ambiguous scope encoding")
	}
}

func TestCallerRequiresNamespaceAndPodSelector(t *testing.T) {
	caller := Caller{Namespace: "system", PodLabels: map[string]string{"app": "portal"}}
	policy := &networkv1.NetworkPolicy{Spec: networkv1.NetworkPolicySpec{Ingress: []networkv1.NetworkPolicyIngressRule{{
		From:  []networkv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "system"}}, PodSelector: &metav1.LabelSelector{MatchLabels: caller.PodLabels}}},
		Ports: []networkv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(8080))}},
	}}}}
	if !Allows(policy, caller, 8080) {
		t.Fatal("exact caller denied")
	}
	if Allows(policy, caller, 8081) {
		t.Fatal("wrong port allowed")
	}
	policy.Spec.Ingress[0].From[0].PodSelector = nil
	if Allows(policy, caller, 8080) {
		t.Fatal("namespace-wide allowance treated as exact caller")
	}
}
