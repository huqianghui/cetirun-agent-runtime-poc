// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import "testing"

func TestAgentIdentityBrokerNamespace(t *testing.T) {
	namespace, err := AgentIdentityBrokerNamespace("http://broker.credentials.svc.cluster.local:8080")
	if err != nil || namespace != "credentials" {
		t.Fatalf("namespace=%q err=%v", namespace, err)
	}
	for _, endpoint := range []string{
		"https://broker.example.invalid", "http://user@broker.credentials.svc.cluster.local:8080",
		"http://broker.credentials.svc.cluster.local:8080/path", "http://broker.credentials.svc.cluster.local:8080?",
		"http://broker.credentials.svc.cluster.local:8080#fragment", "http://broker.credentials.svc.cluster.local:8081",
		"http://broker.credentials.svc.cluster.local:8080?query=value", "http://broker.credentials.svc.cluster.local.evil:8080",
	} {
		if _, err := AgentIdentityBrokerNamespace(endpoint); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}
}
