// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tools

import (
	"context"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/brokerclient"
	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func TestGitHubInstallationAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*brokerclient.CheckResponse)
		allowed bool
	}{
		{"explicit grant", func(*brokerclient.CheckResponse) {}, true},
		{"no installation", func(c *brokerclient.CheckResponse) { c.Connection.GitHub = nil }, false},
		{"no grant", func(c *brokerclient.CheckResponse) { c.Binding.GitHub = nil }, false},
		{"all repositories", func(c *brokerclient.CheckResponse) { c.Binding.GitHub.RepositoryIDs = nil }, false},
		{"unknown permission", func(c *brokerclient.CheckResponse) { c.Binding.GitHub.Permissions["administration"] = "write" }, false},
		{"duplicate repository", func(c *brokerclient.CheckResponse) { c.Binding.GitHub.RepositoryIDs = []int64{123, 123} }, false},
		{"delegated fallback", func(c *brokerclient.CheckResponse) { c.Connection.Mode = "delegated" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b, p, caller, _ := serviceFixture()
			b.check.Service = "github"
			b.check.Connection.Mode, b.check.Connection.Provider = "installation", "github"
			b.check.Connection.Identity = nil
			b.check.Connection.Services = []string{"github"}
			b.check.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 42, AccountLogin: "example", UserLogin: "octocat"}
			b.check.Binding.Services = []string{"github"}
			b.check.Binding.GitHub = &agenttools.GitHubGrant{RepositoryIDs: []int64{123}, Permissions: map[string]string{"contents": "read"}}
			tc.change(&b.check)
			_, err := s.Call(context.Background(), caller, "service_request", map[string]any{"connection_id": "c", "service": "github", "method": "GET", "path": "/repos/example/project/contents"})
			if (err == nil) != tc.allowed || (p.calls == 1) != tc.allowed {
				t.Fatalf("allowed=%t, provider calls=%d, err=%v", tc.allowed, p.calls, err)
			}
		})
	}
}

func TestGitHubGrantCannotChangeBetweenCheckAndIssue(t *testing.T) {
	s, b, p, caller, _ := serviceFixture()
	b.check.Service = "github"
	b.check.Connection.Mode, b.check.Connection.Provider = "installation", "github"
	b.check.Connection.Identity = nil
	b.check.Connection.Services = []string{"github"}
	b.check.Connection.GitHub = &agenttools.GitHubInstallation{InstallationID: 42, AccountLogin: "example", UserLogin: "octocat"}
	b.check.Binding.Services = []string{"github"}
	b.check.Binding.GitHub = &agenttools.GitHubGrant{RepositoryIDs: []int64{123}, Permissions: map[string]string{"contents": "read"}}
	b.issueChange = func(response *brokerclient.IssueResponse) {
		response.Binding.GitHub = &agenttools.GitHubGrant{RepositoryIDs: []int64{456}, Permissions: map[string]string{"contents": "write"}}
	}
	_, err := s.Call(context.Background(), caller, "service_request", map[string]any{"connection_id": "c", "service": "github", "method": "GET", "path": "/repos/example/project/contents"})
	if err == nil || p.calls != 0 {
		t.Fatal("a changed GitHub grant reached the provider")
	}
}
