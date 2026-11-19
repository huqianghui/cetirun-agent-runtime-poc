// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package agenttools defines the trusted service and Broker wire contract.
package agenttools

import "time"

const ContractVersion = "2026-10-28"

// Service fixes the token destination and audience. Workloads cannot override it.
// Request paths are relative to BasePath, with a leading slash.
type Service struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Origin   string `json:"origin"`
	BasePath string `json:"basePath"`
	Audience string `json:"audience,omitempty"`
}

func Services() []Service {
	return []Service{
		{ID: "azure_arm", Provider: "azure", Origin: "https://management.azure.com", BasePath: "/", Audience: "https://management.azure.com/"},
		{ID: "github", Provider: "github", Origin: "https://api.github.com", BasePath: "/"},
		{ID: "microsoft_graph", Provider: "microsoft", Origin: "https://graph.microsoft.com", BasePath: "/v1.0/", Audience: "https://graph.microsoft.com/"},
		{ID: "google_gmail", Provider: "google", Origin: "https://gmail.googleapis.com", BasePath: "/gmail/v1/"},
		{ID: "google_calendar", Provider: "google", Origin: "https://www.googleapis.com", BasePath: "/calendar/v3/"},
	}
}

func LookupService(id string) (Service, bool) {
	for _, service := range Services() {
		if service.ID == id {
			return service, true
		}
	}
	return Service{}, false
}

func ProviderServices(provider string) []string {
	result := []string{}
	for _, service := range Services() {
		if service.Provider == provider {
			result = append(result, service.ID)
		}
	}
	return result
}

type IdentityBinding struct {
	InstanceUID string `json:"instanceUid"`
	TenantID    string `json:"tenantId,omitempty"`
	PrincipalID string `json:"principalId,omitempty"`
	ClientID    string `json:"clientId,omitempty"`
}

// GitHubInstallation identifies an App installation without exposing credentials.
type GitHubInstallation struct {
	InstallationID int64  `json:"installationId,omitempty"`
	AppSlug        string `json:"appSlug,omitempty"`
	AccountLogin   string `json:"accountLogin,omitempty"`
	UserLogin      string `json:"userLogin"`
}

// GitHubGrant narrows an installation credential to explicit repositories and
// repository permissions. Metadata read access is always required by GitHub.
type GitHubGrant struct {
	RepositoryIDs []int64           `json:"repositoryIds"`
	Permissions   map[string]string `json:"permissions"`
}

func (g *GitHubGrant) Valid() bool {
	if g == nil || len(g.RepositoryIDs) == 0 || len(g.RepositoryIDs) > 500 || len(g.Permissions) == 0 {
		return false
	}
	seen := make(map[int64]bool, len(g.RepositoryIDs))
	for _, id := range g.RepositoryIDs {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	for permission, level := range g.Permissions {
		if permission != "contents" && permission != "pull_requests" && permission != "issues" {
			return false
		}
		if level != "read" && level != "write" {
			return false
		}
	}
	return true
}

// Connection exposes credential-free account and service metadata.
type Connection struct {
	ID             string              `json:"id"`
	Mode           string              `json:"mode"`
	Identity       *IdentityBinding    `json:"identity,omitempty"`
	GitHub         *GitHubInstallation `json:"github,omitempty"`
	Provider       string              `json:"provider"`
	AccountDisplay string              `json:"accountDisplay"`
	Services       []string            `json:"services"`
	Scopes         []string            `json:"scopes"`
	Revision       uint64              `json:"revision"`
	Status         string              `json:"status"`
	CreatedAt      time.Time           `json:"createdAt"`
}

// Binding associates exactly one trusted Agent with a Connection and services.
// GitHub App grants also narrow the installation credential at issuance.
type Binding struct {
	ID           string       `json:"id"`
	InstanceUID  string       `json:"instanceUid"`
	ConnectionID string       `json:"connectionId"`
	Services     []string     `json:"services"`
	Enabled      bool         `json:"enabled"`
	Revision     uint64       `json:"revision"`
	GitHub       *GitHubGrant `json:"github,omitempty"`
}

type Request struct {
	CredentialPolicy   *CredentialPolicyReference `json:"credentialPolicy,omitempty"`
	CallerProof        string                     `json:"callerProof"`
	ConnectionID       string                     `json:"connectionId"`
	Service            string                     `json:"service"`
	BindingRevision    *uint64                    `json:"bindingRevision,omitempty"`
	ConnectionRevision *uint64                    `json:"connectionRevision,omitempty"`
	ContractVersion    string                     `json:"contractVersion"`
}

type ListResponse struct {
	InstanceUID     string       `json:"instanceUid"`
	Connections     []Connection `json:"connections"`
	Bindings        []Binding    `json:"bindings"`
	ContractVersion string       `json:"contractVersion"`
}

type CheckResponse struct {
	CredentialPolicy *CredentialPolicyReference `json:"credentialPolicy,omitempty"`
	// Stable provider account ID, only included in internal check/issue responses.
	ExecutorAccountID string     `json:"executorAccountId,omitempty"`
	InstanceUID       string     `json:"instanceUid"`
	Connection        Connection `json:"connection"`
	Binding           Binding    `json:"binding"`
	Service           string     `json:"service"`
	ContractVersion   string     `json:"contractVersion"`
}

type IssueResponse struct {
	CheckResponse
	AccessToken string `json:"accessToken"`
	ExpiresAt   string `json:"expiresAt"`
}
