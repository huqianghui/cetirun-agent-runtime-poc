// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"testing"
)

func TestAliasFromUPN(t *testing.T) {
	tests := []struct {
		name string
		upn  string
		want string
	}{
		{
			name: "standard email",
			upn:  "alice@example.com",
			want: "alice",
		},
		{
			name: "uppercase email",
			upn:  "Alice@Example.com",
			want: "alice",
		},
		{
			name: "no domain part",
			upn:  "alice",
			want: "alice",
		},
		{
			name: "already valid alias",
			upn:  "alice-bob@example.com",
			want: "alice-bob",
		},
		{
			name: "valid with numbers",
			upn:  "alice123@example.com",
			want: "alice123",
		},
		// Invalid cases - should return empty string
		{
			name: "empty string",
			upn:  "",
			want: "",
		},
		{
			name: "starts with number",
			upn:  "123bob@example.com",
			want: "",
		},
		{
			name: "contains dots",
			upn:  "alice.bob@example.com",
			want: "",
		},
		{
			name: "contains underscores",
			upn:  "alice_bob@example.com",
			want: "",
		},
		{
			name: "leading special characters",
			upn:  ".alice@example.com",
			want: "",
		},
		{
			name: "trailing special characters",
			upn:  "alice.@example.com",
			want: "",
		},
		{
			name: "starts with dash",
			upn:  "-alice@example.com",
			want: "",
		},
		{
			name: "ends with dash",
			upn:  "alice-@example.com",
			want: "",
		},
		{
			name: "too long (over 63 chars)",
			upn:  "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnop@example.com",
			want: "",
		},
		{
			name: "exactly 63 chars",
			upn:  "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk@example.com",
			want: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk",
		},
		{
			name: "mixed special characters",
			upn:  "alice.bob_charlie@example.com",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aliasFromUPN(tt.upn)
			if got != tt.want {
				t.Errorf("aliasFromUPN(%q) = %q, want %q", tt.upn, got, tt.want)
			}
		})
	}
}

func TestNewContextAndClaimsFromContext(t *testing.T) {
	claims := Claims{
		Alias:       "alice",
		DisplayName: "Alice Smith",
		Email:       "alice@example.com",
	}

	ctx := NewContext(context.Background(), claims)
	got, ok := ClaimsFromContext(ctx)
	if !ok {
		t.Fatal("ClaimsFromContext returned ok=false")
	}
	if got.Alias != claims.Alias {
		t.Errorf("Alias = %q, want %q", got.Alias, claims.Alias)
	}
	if got.DisplayName != claims.DisplayName {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, claims.DisplayName)
	}
	if got.Email != claims.Email {
		t.Errorf("Email = %q, want %q", got.Email, claims.Email)
	}
}

func TestClaimsFromContext_Empty(t *testing.T) {
	_, ok := ClaimsFromContext(context.Background())
	if ok {
		t.Error("ClaimsFromContext on empty context should return ok=false")
	}
}
