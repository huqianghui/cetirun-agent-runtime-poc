// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"regexp"
	"strings"
)

// Claims represents the authenticated user extracted from OAuth2 Proxy headers.
type Claims struct {
	// Subject comes only from the gateway-authenticated opaque identity header.
	Subject string
	// Alias is the user's short name, extracted from the X-Auth-Request-Preferred-Username
	// header by stripping the @domain.com suffix. Used as the Agent name.
	Alias string

	// DisplayName is a human-readable identifier for the user, derived from
	// the preferred_username (UPN) header. X-Auth-Request-User is NOT used
	// because the entra-id provider populates it with a hashed subject ID.
	DisplayName string

	// Email is the user's email from the X-Auth-Request-Email header.
	Email string

	// LoginHint is preferably Entra's opaque optional login_hint claim from the
	// verified Portal ID token. Older app registrations fall back to the
	// tenant-facing preferred username. It only guides interactive sign-in and
	// is never accepted as proof of identity.
	LoginHint string

	// IsAdmin is true when the user's alias matches the configured admin
	// allowlist. Used by the RequireAdmin middleware and the /api/v1/me
	// endpoint to gate the admin portal.
	IsAdmin bool

	// TenantID and ObjectID are taken only from a cryptographically verified
	// Entra ID token. They are empty when Agent Identity is disabled.
	TenantID string
	ObjectID string
}

type claimsKey struct{}

// NewContext returns a new context with the given Claims.
func NewContext(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// ClaimsFromContext extracts Claims from the request context.
func ClaimsFromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(Claims)
	return c, ok
}

// DNS-1123 label pattern: start with letter, end with letter or digit,
// middle can contain letters, digits, or hyphens. Max 63 chars.
var aliasPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
var canonicalID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// aliasFromUPN extracts the alias from a UPN like "alice@example.com"
// and converts it to lowercase. Returns empty string if the result doesn't
// match the CRD pattern ^[a-z][a-z0-9-]{0,62}$.
func aliasFromUPN(upn string) string {
	local := upn
	if idx := strings.Index(upn, "@"); idx > 0 {
		local = upn[:idx]
	}
	alias := strings.ToLower(local)

	// Validate against CRD pattern
	if !aliasPattern.MatchString(alias) {
		return ""
	}

	return alias
}
