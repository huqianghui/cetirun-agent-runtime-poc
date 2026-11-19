// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"net/http"
	"strings"
)

// AdminSet is the set of aliases authorized to access /api/v1/admin/* and
// the /admin SPA section. Aliases are stored lowercased; lookups are
// case-insensitive.
type AdminSet map[string]struct{}

// NewAdminSet constructs an AdminSet from a slice of aliases. Empty entries
// and surrounding whitespace are ignored; entries are lowercased; duplicates
// are deduplicated.
func NewAdminSet(aliases []string) AdminSet {
	s := AdminSet{}
	for _, a := range aliases {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		s[a] = struct{}{}
	}
	return s
}

// Contains reports whether alias is in the admin set.
func (s AdminSet) Contains(alias string) bool {
	if s == nil || alias == "" {
		return false
	}
	_, ok := s[strings.ToLower(alias)]
	return ok
}

// RequireAdmin returns middleware that rejects non-admin requests with 403.
// It assumes Middleware() ran first and populated Claims in context.
func RequireAdmin() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok || !claims.IsAdmin {
				writeError(w, http.StatusForbidden, "forbidden_admin",
					"Admin access required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
