// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/config"
)

func TestAuthenticateValidatesCurrentWorkloadBinding(t *testing.T) {
	for _, test := range []struct {
		name             string
		namespaceVariant string
		mutate           func(map[string]any, map[string]any, map[string]any, map[string]any)
		allowed          bool
		wantErr          error
		policyManaged    bool
		policyRevision   string
	}{
		{name: "pod bound without Entra identity", allowed: true},
		{name: "status policy alone cannot qualify native identity", allowed: false,
			mutate: func(review, pod, account, agent map[string]any) {
				status := agent["status"].(map[string]any)
				status["policy"] = map[string]any{"phase": "Applied"}
				status["agentIdentity"] = map[string]any{"credentialPolicyRevision": "sha256:" + strings.Repeat("1", 64)}
			}},
		{name: "missing namespace owner", namespaceVariant: "missing"},
		{name: "duplicate namespace owner", namespaceVariant: "duplicate"},
		{name: "deleting namespace", namespaceVariant: "deleting"},
		{name: "unavailable namespace", namespaceVariant: "unavailable", wantErr: ErrDependencyUnavailable},
		{name: "rejected token", mutate: func(review, pod, account, agent map[string]any) { review["authenticated"] = false }},
		{name: "rejected expired token", mutate: func(review, pod, account, agent map[string]any) {
			review["authenticated"] = false
			review["error"] = "synthetic expired token diagnostic"
		}},
		{name: "wrong audience", mutate: func(review, pod, account, agent map[string]any) { review["audiences"] = []string{"other"} }},
		{name: "previous component audience", mutate: func(review, pod, account, agent map[string]any) { review["audiences"] = []string{"agent-tools-mcp"} }},
		{name: "no audiences", mutate: func(review, pod, account, agent map[string]any) { delete(review, "audiences") }},
		{name: "recreated Agent", mutate: func(review, pod, account, agent map[string]any) {
			agent["metadata"].(map[string]any)["uid"] = "new-instance"
		}},
		{name: "wrong Agent namespace", mutate: func(review, pod, account, agent map[string]any) {
			agent["status"].(map[string]any)["namespace"] = "another-namespace"
		}},
		{name: "recreated pod", mutate: func(review, pod, account, agent map[string]any) { pod["metadata"].(map[string]any)["uid"] = "new-pod" }},
		{name: "recreated account", mutate: func(review, pod, account, agent map[string]any) {
			account["metadata"].(map[string]any)["uid"] = "new-account"
		}},
		{name: "deleting pod", mutate: func(review, pod, account, agent map[string]any) {
			pod["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-10T00:00:00Z"
		}},
		{name: "suspended agent", mutate: func(review, pod, account, agent map[string]any) { agent["spec"] = map[string]any{"suspended": true} }},
		{name: "deleting agent", mutate: func(review, pod, account, agent map[string]any) {
			agent["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-10T00:00:00Z"
		}},
		{name: "other instance account", mutate: func(review, pod, account, agent map[string]any) { agent["metadata"].(map[string]any)["name"] = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			review := map[string]any{"authenticated": true, "audiences": []string{"cetirun-connections-mcp"}, "user": map[string]any{
				"username": "system:serviceaccount:cetirun-agent-a:openclaw-agent-a-cetirun", "uid": "account-uid",
				"extra": map[string]any{"authentication.kubernetes.io/pod-name": []string{"agent-pod"}, "authentication.kubernetes.io/pod-uid": []string{"pod-uid"}},
			}}
			pod := map[string]any{"metadata": map[string]any{"uid": "pod-uid", "namespace": "cetirun-agent-a"}, "spec": map[string]any{"serviceAccountName": "openclaw-agent-a-cetirun"}}
			account := map[string]any{"metadata": map[string]any{"uid": "account-uid"}}
			agent := map[string]any{"metadata": map[string]any{"uid": "instance-uid", "name": "agent-a"}, "status": map[string]any{"phase": "Ready", "namespace": "cetirun-agent-a"}}
			if test.mutate != nil {
				test.mutate(review, pod, account, agent)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				var result any
				switch request.URL.Path {
				case "/apis/authentication.k8s.io/v1/tokenreviews":
					var input tokenReview
					if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Spec.Token != "caller-proof" || !audienceMatches(input.Spec.Audiences, "cetirun-connections-mcp") {
						t.Error("invalid TokenReview request")
					}
					result = map[string]any{"status": review}
				case "/api/v1/namespaces/cetirun-agent-a/pods/agent-pod":
					result = pod
				case "/api/v1/namespaces/cetirun-agent-a/serviceaccounts/openclaw-agent-a-cetirun":
					result = account
				case "/api/v1/namespaces/cetirun-agent-a":
					owner := map[string]string{"apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Agent", "name": "agent-a", "uid": "instance-uid"}
					meta := map[string]any{"name": "cetirun-agent-a", "ownerReferences": []any{owner}}
					switch test.namespaceVariant {
					case "missing":
						delete(meta, "ownerReferences")
					case "duplicate":
						meta["ownerReferences"] = []any{owner, owner}
					case "deleting":
						meta["deletionTimestamp"] = "2026-10-10T00:00:00Z"
					case "unavailable":
						http.Error(writer, "unavailable", http.StatusServiceUnavailable)
						return
					}
					result = map[string]any{"metadata": meta}
				case "/apis/cetirun.microsoft.com/v1alpha1/agents/agent-a":
					result = agent
				default:
					t.Errorf("unexpected path %s", request.URL.Path)
					http.NotFound(writer, request)
					return
				}
				_ = json.NewEncoder(writer).Encode(result)
			}))
			defer server.Close()
			tokenPath := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenPath, []byte("server-proof"), 0600); err != nil {
				t.Fatal(err)
			}
			client := &Kubernetes{baseURL: server.URL, tokenFile: tokenPath, client: server.Client(), audience: config.DefaultAudience}
			caller, err := client.Authenticate(context.Background(), "caller-proof")
			if test.allowed && (err != nil || caller.InstanceUID != "instance-uid") {
				t.Fatalf("valid workload rejected: %v", err)
			}
			if test.allowed && (caller.PolicyManaged != test.policyManaged || caller.CredentialPolicyRevision != test.policyRevision) {
				t.Fatal("credential policy was not bound to the live Agent status")
			}
			if !test.allowed {
				wantErr := test.wantErr
				if wantErr == nil {
					wantErr = ErrUnauthenticated
				}
				if !errors.Is(err, wantErr) || caller != (Caller{}) {
					t.Fatalf("authentication = %#v, %v; want empty caller, %v", caller, err, wantErr)
				}
			}
		})
	}
}
