// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateRequiresExactAuthorizationClient(t *testing.T) {
	const (
		tenant    = "11111111-1111-1111-1111-111111111111"
		blueprint = "22222222-2222-2222-2222-222222222222"
		client    = "33333333-3333-3333-3333-333333333333"
		owner     = "44444444-4444-4444-4444-444444444444"
	)
	for _, test := range []struct {
		name     string
		tenant   string
		owner    string
		azp      string
		expected authSDKValidationFailure
	}{
		{name: "exact", tenant: tenant, owner: owner, azp: client},
		{name: "case insensitive IDs", tenant: strings.ToUpper(tenant), owner: strings.ToUpper(owner), azp: strings.ToUpper(client)},
		{name: "missing authorized party", tenant: tenant, owner: owner, azp: "", expected: authSDKValidationParty},
		{name: "different authorized party", tenant: tenant, owner: owner, azp: "55555555-5555-5555-5555-555555555555", expected: authSDKValidationParty},
		{name: "different tenant", tenant: "55555555-5555-5555-5555-555555555555", owner: owner, azp: client, expected: authSDKValidationTenant},
		{name: "different owner", tenant: tenant, owner: "55555555-5555-5555-5555-555555555555", azp: client, expected: authSDKValidationOwner},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"tid":%q,"oid":%q,"aud":%q,"scp":"access_agent","azp":%q}`, test.tenant, test.owner, blueprint, test.azp)
			}))
			defer server.Close()
			service := &authSDKClient{baseURL: server.URL, client: server.Client()}
			err := service.validate(context.Background(), "synthetic-user-token", tenant, blueprint, client, owner)
			if test.expected == "" {
				if err != nil {
					t.Fatalf("validate() error = %v, want success", err)
				}
				return
			}
			category, ok := authSDKValidationFailureCategory(err)
			if !ok || category != string(test.expected) {
				t.Fatalf("failure category = %q, %v; want %q", category, err, test.expected)
			}
		})
	}
}

func syntheticAssertion(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".synthetic-signature"
}

func TestValidateClassifiesAuthSDKRejectionWithoutLoggingClaims(t *testing.T) {
	const (
		tenant    = "11111111-1111-1111-1111-111111111111"
		blueprint = "22222222-2222-2222-2222-222222222222"
		client    = "33333333-3333-3333-3333-333333333333"
		owner     = "44444444-4444-4444-4444-444444444444"
	)
	validClaims := func() map[string]any {
		return map[string]any{"tid": tenant, "oid": owner, "aud": blueprint, "scp": "access_agent", "azp": client}
	}
	for _, test := range []struct {
		name     string
		token    func(*testing.T) string
		expected authSDKValidationFailure
	}{
		{name: "malformed", token: func(*testing.T) string { return "opaque-token" }, expected: authSDKValidationRejectedMalformed},
		{name: "tenant", token: func(t *testing.T) string {
			claims := validClaims()
			claims["tid"] = "other"
			return syntheticAssertion(t, claims)
		}, expected: authSDKValidationRejectedTenant},
		{name: "owner", token: func(t *testing.T) string {
			claims := validClaims()
			claims["oid"] = "other"
			return syntheticAssertion(t, claims)
		}, expected: authSDKValidationRejectedOwner},
		{name: "audience", token: func(t *testing.T) string {
			claims := validClaims()
			claims["aud"] = "other"
			return syntheticAssertion(t, claims)
		}, expected: authSDKValidationRejectedAudience},
		{name: "scope", token: func(t *testing.T) string {
			claims := validClaims()
			claims["scp"] = "other"
			return syntheticAssertion(t, claims)
		}, expected: authSDKValidationRejectedScope},
		{name: "authorized party", token: func(t *testing.T) string {
			claims := validClaims()
			claims["azp"] = "other"
			return syntheticAssertion(t, claims)
		}, expected: authSDKValidationRejectedParty},
		{name: "application token", token: func(t *testing.T) string {
			claims := validClaims()
			claims["idtyp"] = "app"
			return syntheticAssertion(t, claims)
		}, expected: authSDKValidationRejectedApp},
		{name: "matching claims", token: func(t *testing.T) string { return syntheticAssertion(t, validClaims()) }, expected: authSDKValidationRejectedClaimsOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid_token","detail":"must not be logged"}`))
			}))
			defer server.Close()
			service := &authSDKClient{baseURL: server.URL, client: server.Client()}
			err := service.validate(context.Background(), test.token(t), tenant, blueprint, client, owner)
			category, ok := authSDKValidationFailureCategory(err)
			if !ok || category != string(test.expected) {
				t.Fatalf("failure category = %q, %v; want %q", category, err, test.expected)
			}
			if strings.Contains(err.Error(), "must not be logged") {
				t.Fatal("Auth SDK response detail escaped into the error")
			}
		})
	}
}

func TestValidateClassifiesUnexpectedAuthSDKStatus(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		expected authSDKValidationFailure
	}{
		{name: "redirect", status: http.StatusTemporaryRedirect, expected: authSDKValidationRedirected},
		{name: "bad request", status: http.StatusBadRequest, expected: authSDKValidationBadRequest},
		{name: "not found", status: http.StatusNotFound, expected: authSDKValidationNotFound},
		{name: "method not allowed", status: http.StatusMethodNotAllowed, expected: authSDKValidationMethod},
		{name: "conflict", status: http.StatusConflict, expected: authSDKValidationConflict},
		{name: "rate limited", status: http.StatusTooManyRequests, expected: authSDKValidationRateLimited},
		{name: "server error", status: http.StatusInternalServerError, expected: authSDKValidationServerError},
		{name: "unexpected", status: http.StatusTeapot, expected: authSDKValidationRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"detail":"must not be logged"}`))
			}))
			defer server.Close()
			service := &authSDKClient{baseURL: server.URL, client: server.Client()}
			err := service.validate(context.Background(), "synthetic-user-token", "tenant", "audience", "client", "owner")
			category, ok := authSDKValidationFailureCategory(err)
			if !ok || category != string(test.expected) {
				t.Fatalf("failure category = %q, %v; want %q", category, err, test.expected)
			}
			if strings.Contains(err.Error(), "must not be logged") {
				t.Fatal("Auth SDK response detail escaped into the error")
			}
		})
	}
}

func TestValidateIgnoresAuthSDKErrorBodyShape(t *testing.T) {
	const (
		tenant    = "11111111-1111-1111-1111-111111111111"
		blueprint = "22222222-2222-2222-2222-222222222222"
		client    = "33333333-3333-3333-3333-333333333333"
		owner     = "44444444-4444-4444-4444-444444444444"
	)
	token := syntheticAssertion(t, map[string]any{
		"tid": tenant, "oid": owner, "aud": blueprint, "scp": "access_agent", "azp": client,
	})
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized empty", status: http.StatusUnauthorized},
		{name: "unauthorized plain text", status: http.StatusUnauthorized, body: "private sidecar detail"},
		{name: "unauthorized JSON string", status: http.StatusUnauthorized, body: `"private sidecar detail"`},
		{name: "unauthorized malformed JSON", status: http.StatusUnauthorized, body: `{"error":`},
		{name: "forbidden empty", status: http.StatusForbidden},
		{name: "forbidden plain text", status: http.StatusForbidden, body: "private sidecar detail"},
		{name: "forbidden JSON string", status: http.StatusForbidden, body: `"private sidecar detail"`},
		{name: "forbidden malformed JSON", status: http.StatusForbidden, body: `{"error":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			service := &authSDKClient{baseURL: server.URL, client: server.Client()}
			err := service.validate(context.Background(), token, tenant, blueprint, client, owner)
			category, ok := authSDKValidationFailureCategory(err)
			if !ok || category != string(authSDKValidationRejectedClaimsOK) {
				t.Fatalf("failure category = %q, %v; want %q", category, err, authSDKValidationRejectedClaimsOK)
			}
			if strings.Contains(err.Error(), "private sidecar detail") {
				t.Fatal("Auth SDK response detail escaped into the error")
			}
		})
	}
}

func TestValidateClassifiesTransportAndSuccessResponseFailures(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		service := &authSDKClient{baseURL: "http://auth-sdk.invalid", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("private transport detail")
		})}}
		err := service.validate(context.Background(), "synthetic-user-token", "tenant", "audience", "client", "owner")
		category, ok := authSDKValidationFailureCategory(err)
		if !ok || category != string(authSDKValidationUnavailable) {
			t.Fatalf("failure category = %q, %v; want %q", category, err, authSDKValidationUnavailable)
		}
		if strings.Contains(err.Error(), "private transport detail") {
			t.Fatal("transport detail escaped into the error")
		}
	})

	for _, test := range []struct {
		name     string
		body     string
		expected authSDKValidationFailure
	}{
		{name: "empty response", expected: authSDKValidationClaimsMissing},
		{name: "claims is not an object", body: `{"claims":"private claim detail"}`, expected: authSDKValidationClaimsMissing},
		{name: "malformed JSON", body: `{"claims":`, expected: authSDKValidationInvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			service := &authSDKClient{baseURL: server.URL, client: server.Client()}
			err := service.validate(context.Background(), "synthetic-user-token", "tenant", "audience", "client", "owner")
			category, ok := authSDKValidationFailureCategory(err)
			if !ok || category != string(test.expected) {
				t.Fatalf("failure category = %q, %v; want %q", category, err, test.expected)
			}
			if strings.Contains(err.Error(), "private claim detail") {
				t.Fatal("Auth SDK response detail escaped into the error")
			}
		})
	}
}

func TestValidateAcceptsOfficialNestedClaimsResponse(t *testing.T) {
	const (
		tenant    = "11111111-1111-1111-1111-111111111111"
		blueprint = "22222222-2222-2222-2222-222222222222"
		client    = "33333333-3333-3333-3333-333333333333"
		owner     = "44444444-4444-4444-4444-444444444444"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"protocol":"Bearer","token":"redacted","claims":{"tid":%q,"oid":%q,"aud":%q,"scp":"access_agent","azp":%q}}`, tenant, owner, blueprint, client)
	}))
	defer server.Close()
	service := &authSDKClient{baseURL: server.URL, client: server.Client()}
	if err := service.validate(context.Background(), "synthetic-user-token", tenant, blueprint, client, owner); err != nil {
		t.Fatalf("validate() error = %v, want success", err)
	}
}

func TestAuthSDKRequestsUseAllowedLoopbackHostHeader(t *testing.T) {
	observedHost := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedHost <- r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	service := &authSDKClient{baseURL: server.URL, client: server.Client()}
	status, err := service.get(context.Background(), "/Validate", "synthetic-user-token", nil, nil)
	if err != nil || status != http.StatusNoContent {
		t.Fatalf("get() status = %d, error = %v; want 204", status, err)
	}
	if host := <-observedHost; host != "localhost" {
		t.Fatalf("Auth SDK Host header = %q; want localhost", host)
	}
}
