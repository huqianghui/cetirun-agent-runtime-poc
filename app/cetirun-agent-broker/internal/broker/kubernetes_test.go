// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceAccountTokenIsReloaded(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &kubeClient{tokenFile: tokenFile}
	first, err := client.serviceAccountToken()
	if err != nil || first != "first-token" {
		t.Fatalf("first token = %q, %v", first, err)
	}
	if err := os.WriteFile(tokenFile, []byte("second-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := client.serviceAccountToken()
	if err != nil || second != "second-token" {
		t.Fatalf("rotated token = %q, %v", second, err)
	}
	if err := os.WriteFile(tokenFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.serviceAccountToken(); err == nil {
		t.Fatal("empty projected token must be rejected")
	}
}

func TestWorkloadServiceAccountUsesInstanceName(t *testing.T) {
	if got := workloadServiceAccount("agentid-hermes-v1"); got != "openclaw-agentid-hermes-v1-cetirun" {
		t.Fatalf("workload ServiceAccount = %q", got)
	}
	if got := workloadServiceAccount("a-very-long-instance-name-that-needs-to-be-truncated-at-the-dns-limit"); len(got) > 63 || got[len(got)-1] == '-' {
		t.Fatalf("workload ServiceAccount is not a valid bounded name: %q", got)
	}
	for length, want := range map[int]string{
		46: "openclaw-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-cetirun",
		47: "openclaw-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-cetiru",
	} {
		if got := workloadServiceAccount(strings.Repeat("a", length)); got != want {
			t.Fatalf("workloadServiceAccount(%d chars) = %q, want %q", length, got, want)
		}
	}
}

func TestWorkloadReadinessDoesNotRequireAgentIdentity(t *testing.T) {
	document := agentDocument{}
	document.Metadata.Name = "agent"
	document.Metadata.UID = "11111111-1111-1111-1111-111111111111"
	document.Metadata.Annotations = map[string]string{
		"agentidentity.cetirun.microsoft.com/owner-tenant-id": "22222222-2222-2222-2222-222222222222",
		"agentidentity.cetirun.microsoft.com/owner-object-id": "33333333-3333-3333-3333-333333333333",
	}
	document.Status.Namespace = "agents"
	document.Status.Phase = "Ready"
	agent := agentFromDocument(document)
	if !agent.WorkloadReady || agent.AgentIdentityReady {
		t.Fatalf("non-Entra ready workload = %#v", agent)
	}

	document.Spec.Suspended = true
	if agentFromDocument(document).WorkloadReady {
		t.Fatal("suspended workload was considered ready")
	}
	document.Spec.Suspended = false
	deleting := time.Now()
	document.Metadata.DeletionTimestamp = &deleting
	if agentFromDocument(document).WorkloadReady {
		t.Fatal("deleting workload was considered ready")
	}
}

func TestTokenReviewRequiresRequestedAudience(t *testing.T) {
	if !tokenReviewAudienceAccepted([]string{"cetirun-connections-mcp"}, "cetirun-connections-mcp") {
		t.Fatal("requested TokenReview audience was rejected")
	}
	for _, audiences := range [][]string{nil, {"another-audience"}, {"cetirun-connections-mcp", "another-audience"}} {
		if tokenReviewAudienceAccepted(audiences, "cetirun-connections-mcp") {
			t.Fatalf("unexpected TokenReview audiences accepted: %#v", audiences)
		}
	}
}
