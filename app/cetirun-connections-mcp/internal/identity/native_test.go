// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/internal/agenttools/testfixture"
)

type nativeIdentityTransport func(*http.Request) (*http.Response, error)

func (f nativeIdentityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeAuthenticationRequiresCurrentCommonAuthority(t *testing.T) {
	for _, scenario := range []string{"current", "long-pod", "dotted-pod", "invalid-pod", "oversized-pod",
		"dotted-sa", "long-sa", "max-sa", "empty-sa", "upper-sa", "empty-label-sa",
		"leading-hyphen-sa", "trailing-dot-sa", "slash-sa", "oversized-sa",
		"denied", "recreated-sa", "missing-projection", "entra-audience", "terminated", "not-ready",
		"missing-agent-resource-version", "missing-agent-generation", "stale-agent-identity-generation"} {
		t.Run(scenario, func(t *testing.T) {
			f := testfixture.NewNative(t)
			agent := f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)
			status := agent["status"].(map[string]any)
			status["policy"] = map[string]string{"phase": "Applied"}
			revision := "sha256:" + strings.Repeat("1", 64)
			status["agentIdentity"] = map[string]any{"credentialPolicyRevision": revision, "observedGeneration": 1}
			audience := "cetirun-connections-mcp"
			switch scenario {
			case "long-pod":
				f.SetPodName(strings.Repeat("a", 63) + "-0")
			case "dotted-pod":
				f.SetPodName("approved.workload-0")
			case "invalid-pod":
				f.SetPodName("a..b-0")
			case "oversized-pod":
				f.SetPodName(strings.Repeat("a", 254))
			case "dotted-sa":
				f.SetServiceAccountName("approved.identity")
			case "long-sa":
				f.SetServiceAccountName(strings.Repeat("a", 64))
			case "max-sa":
				f.SetServiceAccountName(strings.Repeat("a", 253))
			case "empty-sa":
				f.SetServiceAccountName("")
			case "upper-sa":
				f.SetServiceAccountName("Uppercase")
			case "empty-label-sa":
				f.SetServiceAccountName("a..b")
			case "leading-hyphen-sa":
				f.SetServiceAccountName("-a")
			case "trailing-dot-sa":
				f.SetServiceAccountName("a.")
			case "slash-sa":
				f.SetServiceAccountName("a/b")
			case "oversized-sa":
				f.SetServiceAccountName(strings.Repeat("a", 254))
			case "denied":
				f.Allowed = false
			case "recreated-sa":
				f.Objects["/api/v1/namespaces/agents/serviceaccounts/default"].(map[string]any)["metadata"].(map[string]any)["uid"] = "replacement"
			case "missing-projection":
				f.Objects["/api/v1/namespaces/agents/pods/"+f.Identity.PodName].(map[string]any)["spec"].(map[string]any)["volumes"] = []any{}
			case "entra-audience":
				audience = "api://AzureADTokenExchange"
			case "terminated":
				f.Objects["/api/v1/namespaces/agents/pods/"+f.Identity.PodName].(map[string]any)["status"].(map[string]any)["phase"] = "Failed"
			case "not-ready":
				f.Objects["/api/v1/namespaces/agents/pods/"+f.Identity.PodName].(map[string]any)["status"].(map[string]any)["conditions"] = []any{}
			case "missing-agent-resource-version":
				delete(agent["metadata"].(map[string]any), "resourceVersion")
			case "missing-agent-generation":
				delete(agent["metadata"].(map[string]any), "generation")
			case "stale-agent-identity-generation":
				status["agentIdentity"].(map[string]any)["observedGeneration"] = 2
			}
			token := filepath.Join(t.TempDir(), "synthetic-server-proof")
			if err := os.WriteFile(token, []byte("synthetic-server-proof"), 0600); err != nil {
				t.Fatal(err)
			}
			kubernetes := &Kubernetes{baseURL: "https://kubernetes.example.com", tokenFile: token,
				audience: "cetirun-connections-mcp", operatorNamespace: "operator", nativeAdmissionTransport: f.Transport}
			kubernetes.client = &http.Client{Transport: nativeIdentityTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews" {
					data, _ := json.Marshal(map[string]any{"status": map[string]any{
						"authenticated": true, "audiences": []string{audience},
						"user": map[string]any{"username": "system:serviceaccount:agents:" + f.Identity.ServiceAccountName, "uid": f.Identity.ServiceAccountUID,
							"extra": map[string][]string{"authentication.kubernetes.io/pod-name": {f.Identity.PodName},
								"authentication.kubernetes.io/pod-uid": {f.Identity.PodUID}}},
					}})
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
				}
				if response, ok := f.Response(r.URL.Path); ok {
					return response, nil
				}
				t.Fatalf("unexpected native source request %s", r.URL.Path)
				return nil, nil
			})}
			caller, err := kubernetes.Authenticate(context.Background(), "synthetic-native-proof")
			if scenario == "current" || scenario == "dotted-sa" || scenario == "long-sa" || scenario == "max-sa" ||
				scenario == "long-pod" || scenario == "dotted-pod" || scenario == "stale-agent-identity-generation" {
				if err != nil || !caller.PolicyManaged || caller.InstanceUID != f.Identity.AgentUID ||
					caller.CredentialPolicyRevision != revision || f.Calls != 1 {
					t.Fatalf("native proof was not verified: %#v %v", caller, err)
				}
			} else if err == nil || caller != (Caller{}) {
				t.Fatalf("unqualified native proof was accepted: %#v %v", caller, err)
			}
		})
	}
}

func TestNativeAuthenticationDoesNotRequireCurrentOptionalAgentIdentity(t *testing.T) {
	f := testfixture.NewNative(t)
	agent := f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)
	status := agent["status"].(map[string]any)
	status["policy"] = map[string]string{"phase": "Applied"}
	status["agentIdentity"] = map[string]any{
		"credentialPolicyRevision": "sha256:independent-provider", "observedGeneration": 2,
	}

	token := filepath.Join(t.TempDir(), "synthetic-server-proof")
	if err := os.WriteFile(token, []byte("synthetic-server-proof"), 0600); err != nil {
		t.Fatal(err)
	}
	kubernetes := &Kubernetes{baseURL: "https://kubernetes.example.com", tokenFile: token,
		audience: "cetirun-connections-mcp", operatorNamespace: "operator", nativeAdmissionTransport: f.Transport}
	kubernetes.client = &http.Client{Transport: nativeIdentityTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews" {
			data, _ := json.Marshal(map[string]any{"status": map[string]any{
				"authenticated": true, "audiences": []string{"cetirun-connections-mcp"},
				"user": map[string]any{"username": "system:serviceaccount:agents:" + f.Identity.ServiceAccountName, "uid": f.Identity.ServiceAccountUID,
					"extra": map[string][]string{"authentication.kubernetes.io/pod-name": {f.Identity.PodName},
						"authentication.kubernetes.io/pod-uid": {f.Identity.PodUID}}},
			}})
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
		}
		if response, ok := f.Response(r.URL.Path); ok {
			return response, nil
		}
		t.Fatalf("unexpected native source request %s", r.URL.Path)
		return nil, nil
	})}

	caller, err := kubernetes.Authenticate(context.Background(), "synthetic-native-proof")
	if err != nil || caller.InstanceUID != f.Identity.AgentUID || !caller.PolicyManaged ||
		caller.CredentialPolicyRevision != "sha256:independent-provider" || f.Calls != 1 {
		t.Fatalf("independent provider was rejected by optional AgentIdentity staleness: %#v %v", caller, err)
	}
}

func TestNativeAuthenticationRechecksAuthorizationInsteadOfStatusResourceVersion(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(map[string]any)
		allowed bool
	}{
		{name: "status resource version", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["resourceVersion"] = "status-heartbeat"
		}, allowed: true},
		{name: "agent identity verification heartbeat", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["resourceVersion"] = "identity-heartbeat"
			agent["status"].(map[string]any)["agentIdentity"].(map[string]any)["lastVerifiedTime"] = "2026-01-01T00:00:01Z"
		}, allowed: true},
		{name: "generation", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["generation"] = 2
		}},
		{name: "missing resource version", mutate: func(agent map[string]any) {
			delete(agent["metadata"].(map[string]any), "resourceVersion")
		}},
		{name: "missing generation", mutate: func(agent map[string]any) {
			delete(agent["metadata"].(map[string]any), "generation")
		}},
		{name: "agent uid", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["uid"] = "replacement-agent"
		}},
		{name: "deletion", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-06T00:00:00Z"
		}},
		{name: "owner metadata", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]string{"uid": "replacement-owner"}}
		}},
		{name: "labels", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["labels"] = map[string]string{"changed": "true"}
		}},
		{name: "annotations", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["annotations"] = map[string]string{"changed": "true"}
		}},
		{name: "suspension", mutate: func(agent map[string]any) {
			agent["spec"].(map[string]any)["suspended"] = true
		}},
		{name: "readiness", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["phase"] = "Pending"
		}},
		{name: "namespace", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["namespace"] = "replacement-namespace"
		}},
		{name: "policy", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["policy"] = map[string]string{"phase": "Changed"}
		}},
		{name: "credential policy revision", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["agentIdentity"] = map[string]string{"credentialPolicyRevision": "sha256:changed"}
		}},
		{name: "agent identity client", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["agentIdentity"] = map[string]any{
				"credentialPolicyRevision": "sha256:initial", "clientId": "replacement-client", "objectId": "initial-object", "observedGeneration": 1,
			}
		}},
		{name: "agent identity object", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["agentIdentity"] = map[string]any{
				"credentialPolicyRevision": "sha256:initial", "clientId": "initial-client", "objectId": "replacement-object", "observedGeneration": 1,
			}
		}},
		{name: "agent identity phase", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["agentIdentity"] = map[string]any{
				"credentialPolicyRevision": "sha256:initial", "clientId": "initial-client", "objectId": "initial-object", "phase": "Degraded", "observedGeneration": 1,
			}
		}},
		{name: "agent identity observed generation", mutate: func(agent map[string]any) {
			agent["status"].(map[string]any)["agentIdentity"] = map[string]any{
				"credentialPolicyRevision": "sha256:initial", "clientId": "initial-client", "objectId": "initial-object", "observedGeneration": 2,
			}
		}},
		{name: "missing agent identity", mutate: func(agent map[string]any) {
			delete(agent["status"].(map[string]any), "agentIdentity")
		}},
		{name: "finalizer", mutate: func(agent map[string]any) {
			agent["metadata"].(map[string]any)["finalizers"] = []string{"cetirun.microsoft.com/sandbox-hold"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := testfixture.NewNative(t)
			agent := f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)
			agent["metadata"].(map[string]any)["generation"] = 1
			status := agent["status"].(map[string]any)
			status["policy"] = map[string]string{"phase": "Applied"}
			status["agentIdentity"] = map[string]any{
				"credentialPolicyRevision": "sha256:initial", "phase": "Ready", "clientId": "initial-client", "objectId": "initial-object", "observedGeneration": 1,
				"lastVerifiedTime": "2026-01-01T00:00:00Z",
			}
			token := filepath.Join(t.TempDir(), "synthetic-server-proof")
			if err := os.WriteFile(token, []byte("synthetic-server-proof"), 0600); err != nil {
				t.Fatal(err)
			}
			kubernetes := &Kubernetes{baseURL: "https://kubernetes.example.com", tokenFile: token,
				audience: "cetirun-connections-mcp", operatorNamespace: "operator", nativeAdmissionTransport: f.Transport}
			agentReads := 0
			kubernetes.client = &http.Client{Transport: nativeIdentityTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews" {
					data, _ := json.Marshal(map[string]any{"status": map[string]any{
						"authenticated": true, "audiences": []string{"cetirun-connections-mcp"},
						"user": map[string]any{"username": "system:serviceaccount:agents:" + f.Identity.ServiceAccountName, "uid": f.Identity.ServiceAccountUID,
							"extra": map[string][]string{"authentication.kubernetes.io/pod-name": {f.Identity.PodName},
								"authentication.kubernetes.io/pod-uid": {f.Identity.PodUID}}},
					}})
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
				}
				if r.URL.Path == "/apis/cetirun.microsoft.com/v1alpha1/agents/agent" {
					agentReads++
					// The fourth read is the MCP-specific recheck after the common
					// admission verifier has already completed its own current-state read.
					if agentReads == 4 {
						test.mutate(agent)
					}
				}
				if response, ok := f.Response(r.URL.Path); ok {
					return response, nil
				}
				t.Fatalf("unexpected native source request %s", r.URL.Path)
				return nil, nil
			})}
			caller, err := kubernetes.Authenticate(context.Background(), "synthetic-native-proof")
			if agentReads != 4 {
				t.Fatalf("Agent reads = %d, want four", agentReads)
			}
			if test.allowed {
				if err != nil || caller.InstanceUID != f.Identity.AgentUID || caller.CredentialPolicyRevision != "sha256:initial" {
					t.Fatalf("status-only update rejected: %#v %v", caller, err)
				}
			} else if !errors.Is(err, ErrUnauthenticated) || caller != (Caller{}) {
				t.Fatalf("authorization change accepted: %#v %v", caller, err)
			}
		})
	}
}

func TestServiceAccountIdentityKeepsNamespaceLabelsStrict(t *testing.T) {
	for _, namespace := range []string{"agents", "agents.example", strings.Repeat("a", 64), "Uppercase", ""} {
		for _, name := range []string{"approved.identity", strings.Repeat("a", 64)} {
			actualNamespace, actualName, valid := serviceAccountIdentity("system:serviceaccount:" + namespace + ":" + name)
			if valid != (namespace == "agents") {
				t.Fatalf("namespace %q with valid ServiceAccount %q: accepted=%t", namespace, name, valid)
			}
			if valid && (actualNamespace != namespace || actualName != name) {
				t.Fatal("ServiceAccount identity was truncated or rewritten")
			}
		}
	}
}
