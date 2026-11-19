// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type Owner struct {
	TenantID string `json:"-"`
	ObjectID string `json:"-"`
}

const (
	modeDelegated    = "delegated"
	modeInstallation = "installation"
	modeAgent        = "agent"
)

// IdentityBinding is derived from platform status and verified provider data.
type IdentityBinding struct {
	InstanceUID string `json:"instanceUid"`
	TenantID    string `json:"tenantId,omitempty"`
	PrincipalID string `json:"principalId,omitempty"`
	ClientID    string `json:"clientId,omitempty"`
}

type Connection struct {
	CanonicalConnectionID string                         `json:"canonicalConnectionId,omitempty"`
	Mode                  string                         `json:"mode,omitempty"`
	Identity              *IdentityBinding               `json:"identity,omitempty"`
	GitHub                *agenttools.GitHubInstallation `json:"github,omitempty"`
	ID                    string                         `json:"id"`
	Provider              string                         `json:"provider"`
	AccountDisplay        string                         `json:"accountDisplay"`
	Services              []string                       `json:"services"`
	Scopes                []string                       `json:"scopes"`
	Revision              uint64                         `json:"revision"`
	Status                string                         `json:"status"`
	CreatedAt             time.Time                      `json:"createdAt"`
	UpdatedAt             time.Time                      `json:"-"`
	Owner                 Owner                          `json:"-"`
	AccountID             string                         `json:"-"`
}

// ConnectionBinding permits one Agent to use selected services of a Connection.
// Provider RBAC or OAuth scopes determine which resource operations succeed.
type ConnectionBinding struct {
	ID           string                  `json:"id"`
	InstanceUID  string                  `json:"instanceUid"`
	ConnectionID string                  `json:"connectionId"`
	Services     []string                `json:"services"`
	Enabled      bool                    `json:"enabled"`
	Revision     uint64                  `json:"revision"`
	Owner        Owner                   `json:"-"`
	Revoked      bool                    `json:"-"`
	CreatedAt    time.Time               `json:"-"`
	UpdatedAt    time.Time               `json:"-"`
	GitHub       *agenttools.GitHubGrant `json:"github,omitempty"`
}

type providerCredential struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
	// NonExpiring is set only for provider credential responses that explicitly
	// permit an access token without an expiry. It prevents a missing expiry
	// from becoming an implicit provider-agnostic exception.
	NonExpiring bool     `json:"nonExpiring,omitempty"`
	Scopes      []string `json:"scopes"`
}

type connectionTransaction struct {
	TargetConnectionID string `json:"targetConnectionId,omitempty"`
	ResultConnectionID string `json:"resultConnectionId,omitempty"`
	Status             string `json:"status,omitempty"`
	Provider           string `json:"provider"`
	Mode               string `json:"mode,omitempty"`
	AppSlug            string `json:"appSlug,omitempty"`
	StateHash          string `json:"stateHash"`
	// BrowserBindingHash is present only for GitHub OAuth transactions. The
	// Portal holds the corresponding browser secret in an HttpOnly cookie.
	BrowserBindingHash string    `json:"browserBindingHash,omitempty"`
	CodeVerifier       string    `json:"codeVerifier"`
	Nonce              string    `json:"nonce,omitempty"`
	ExpectedAccountID  string    `json:"expectedAccountId,omitempty"`
	ExpiresAt          time.Time `json:"expiresAt"`
	Consumed           bool      `json:"consumed"`
}

type connectionRecord struct {
	// A verified installation selection is locked before claiming its account
	// index. The connection remains unbindable until canonicalization completes.
	PendingInstallation *agenttools.GitHubInstallation `json:"pendingInstallation,omitempty"`
	References          map[string]bindingReservation  `json:"references,omitempty"`
	Connection          Connection                     `json:"connection"`
	Credential          *providerCredential            `json:"credential,omitempty"`
	RefreshLease        *credentialRefreshLease        `json:"refreshLease,omitempty"`
	Transaction         *connectionTransaction         `json:"transaction,omitempty"`
	Deleted             bool                           `json:"deleted"`
	// Display snapshots come from the trusted Agent resource. They are never
	// used to resolve ownership or authorize calls; instance UID remains binding.
	InstanceName      string `json:"instanceName,omitempty"`
	InstanceNamespace string `json:"instanceNamespace,omitempty"`
}

// Refresh leases are encrypted with the Connection record. They fence rotating
// GitHub App user refresh tokens across Broker replicas without exposing any
// credential material.
type credentialRefreshLease struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type bindingRecord struct {
	ReservationToken  string            `json:"reservationToken,omitempty"`
	Binding           ConnectionBinding `json:"binding"`
	InstanceName      string            `json:"instanceName,omitempty"`
	InstanceNamespace string            `json:"instanceNamespace,omitempty"`
}

type Agent struct {
	PolicyManaged         bool
	Deleting              bool
	RuntimeType           string
	Name                  string
	UID                   string
	Namespace             string
	ServiceAccount        string
	OwnerTenantID         string
	OwnerObjectID         string
	AgentIdentityObjectID string
	AgentIdentityClientID string
	WorkloadReady         bool
	AgentIdentityReady    bool
	UnsupportedRuntime    bool
}

type apiError struct {
	Error struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		CorrelationID string `json:"correlationId"`
	} `json:"error"`
}
