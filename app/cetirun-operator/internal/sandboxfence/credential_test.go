// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func TestAzureCLICredentialUsesScopedArgumentArray(t *testing.T) {
	expected := []string{"account", "get-access-token", "--subscription", fixtureSubscription,
		"--resource", "https://management.azure.com/", "--output", "json", "--only-show-errors"}
	calls := 0
	credential := &cliCredential{
		tenant: fixtureTenant, subscription: fixtureSubscription,
		execute: func(_ context.Context, args []string) ([]byte, error) {
			calls++
			if slices.Contains(args, "--subscription") && slices.Contains(args, "--tenant") {
				return nil, errors.New("Azure CLI rejects subscription and tenant selectors together")
			}
			if !slices.Equal(args, expected) {
				t.Fatalf("unexpected CLI argument array: %#v", args)
			}
			return json.Marshal(map[string]any{
				"accessToken": "synthetic-token", "expires_on": time.Now().Add(time.Hour).Unix(),
				"subscription": fixtureSubscription, "tenant": fixtureTenant, "tokenType": "Bearer",
			})
		},
	}
	token, err := credential.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
	if err != nil || token.Token != "synthetic-token" {
		t.Fatalf("scoped CLI credential failed: %v", err)
	}
	for _, options := range []policy.TokenRequestOptions{
		{Scopes: []string{"https://example.com/.default"}},
		{Scopes: []string{"https://management.azure.com/.default"}, Claims: "unexpected-challenge"},
		{Scopes: []string{"https://management.azure.com/.default"}, TenantID: fixtureReplacement},
	} {
		if _, err := credential.GetToken(context.Background(), options); err == nil {
			t.Fatal("unapproved token audience, challenge or tenant accepted")
		}
	}
	if calls != 1 {
		t.Fatalf("unapproved requests invoked credential acquisition or fallback: %d calls", calls)
	}
}

func TestAzureCLICredentialRejectsTokenMetadataMismatch(t *testing.T) {
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"wrong tenant", "tenant", fixtureReplacement},
		{"wrong subscription", "subscription", fixtureReplacement},
		{"missing tenant", "tenant", nil},
		{"missing subscription", "subscription", nil},
		{"wrong token type", "tokenType", "Other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := map[string]any{
				"accessToken": "synthetic-sensitive-token", "expires_on": time.Now().Add(time.Hour).Unix(),
				"subscription": fixtureSubscription, "tenant": fixtureTenant, "tokenType": "Bearer",
			}
			if test.value == nil {
				delete(document, test.field)
			} else {
				document[test.field] = test.value
			}
			calls := 0
			credential := &cliCredential{tenant: fixtureTenant, subscription: fixtureSubscription,
				execute: func(_ context.Context, args []string) ([]byte, error) {
					calls++
					if slices.Contains(args, "--tenant") || !slices.Contains(args, "--subscription") {
						t.Fatal("Azure CLI's mutually exclusive tenant/subscription constraint was violated")
					}
					return json.Marshal(document)
				}}
			token, err := credential.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
			if err == nil || token.Token != "" || !token.ExpiresOn.IsZero() || calls != 1 ||
				strings.Contains(err.Error(), "synthetic-sensitive-token") {
				t.Fatalf("metadata mismatch was accepted, leaked, or triggered fallback: error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestTokenMetadataMismatchPreventsARMRequests(t *testing.T) {
	for _, field := range []string{"tenant", "subscription"} {
		t.Run(field, func(t *testing.T) {
			f := newARMFixture(t)
			calls := 0
			credential := &cliCredential{tenant: fixtureTenant, subscription: fixtureSubscription,
				execute: func(context.Context, []string) ([]byte, error) {
					calls++
					document := map[string]any{
						"accessToken": "synthetic-sensitive-token", "expires_on": time.Now().Add(time.Hour).Unix(),
						"subscription": fixtureSubscription, "tenant": fixtureTenant, "tokenType": "Bearer",
					}
					document[field] = fixtureReplacement
					return json.Marshal(document)
				}}
			var err error
			f.client, err = makeARMClient(credential, f.server.URL, f.server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if journal, err := f.invoke(context.Background(), Reserve, false); err == nil || journal != nil ||
				calls != 1 || len(f.requests) != 0 {
				t.Fatalf("incorrect token metadata reached ARM: error=%v credentialCalls=%d requests=%d", err, calls, len(f.requests))
			}
		})
	}
}

func TestAzureCLICredentialRejectsMalformedAndExpiredTokens(t *testing.T) {
	for _, data := range []string{
		`not JSON`,
		`{"accessToken":"synthetic-sensitive-value","expires_on":1,"tokenType":"Bearer"}`,
		`{"accessToken":"synthetic-sensitive-value","expires_on":4102444800,"tokenType":"Bearer","tenant":"wrong"}`,
		strings.Repeat("x", 65537),
	} {
		credential := &cliCredential{tenant: fixtureTenant, subscription: fixtureSubscription,
			execute: func(context.Context, []string) ([]byte, error) { return []byte(data), nil }}
		_, err := credential.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
		if err == nil || strings.Contains(err.Error(), "synthetic-sensitive-value") {
			t.Fatal("invalid token was accepted or disclosed")
		}
	}
	credential := &cliCredential{tenant: fixtureTenant, subscription: fixtureSubscription,
		execute: func(context.Context, []string) ([]byte, error) { return nil, context.Canceled }}
	if _, err := credential.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
}

func TestScopeAndIdentityValidation(t *testing.T) {
	for _, id := range []string{
		fixturePool + "/machines/other",
		fixturePool + "?api-version=anything",
		strings.Replace(fixturePool, "/pilot", "/Pilot", 1),
		"/subscriptions/$(command)/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster/agentPools/pilot",
	} {
		if _, err := parsePoolID(id); err == nil {
			t.Errorf("unsafe pool ID accepted: %q", id)
		}
	}
	for _, etag := range []string{"", "*", `W/"weak"`, "line\r\nbreak", `"one","two"`} {
		if _, err := differentETag(etag); err == nil {
			t.Errorf("unusable ETag accepted: %q", etag)
		}
	}
	for _, etag := range []string{`"00000000-0000-4000-8000-000000000001"`, "opaqueValue", "123"} {
		other, err := differentETag(etag)
		if err != nil || other == etag || len(other) != len(etag) {
			t.Errorf("invalid nonmatching ETag: %q -> %q (%v)", etag, other, err)
		}
	}
}
