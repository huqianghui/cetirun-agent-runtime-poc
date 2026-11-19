// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestTokenRequestUsesFixedSafeFailureCategories(t *testing.T) {
	assertionFile := filepath.Join(t.TempDir(), "federated-token")
	if err := os.WriteFile(assertionFile, []byte("synthetic-assertion"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		status   int
		body     string
		expected oauthTokenFailure
	}{
		{name: "invalid request", status: http.StatusBadRequest, body: `{"error":"invalid_request"}`, expected: oauthTokenInvalidRequest},
		{name: "invalid client", status: http.StatusUnauthorized, body: `{"error":"invalid_client"}`, expected: oauthTokenInvalidClient},
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"must not be logged"}`, expected: oauthTokenInvalidGrant},
		{name: "invalid scope", status: http.StatusBadRequest, body: `{"error":"invalid_scope"}`, expected: oauthTokenInvalidScope},
		{name: "unauthorized client", status: http.StatusBadRequest, body: `{"error":"unauthorized_client"}`, expected: oauthTokenUnauthorizedClient},
		{name: "unknown rejection", status: http.StatusTeapot, body: `{"error":"future_error"}`, expected: oauthTokenEndpointRejected},
		{name: "invalid success response", status: http.StatusOK, body: `{}`, expected: oauthTokenInvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newOAuthClient(Config{TenantID: "11111111-1111-1111-1111-111111111111", FederatedTokenFile: assertionFile})
			client.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != "login.microsoftonline.com" {
					t.Fatalf("unexpected token endpoint host: %s", request.URL.Host)
				}
				return &http.Response{
					StatusCode: test.status,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})}
			_, err := client.tokenRequest(context.Background(), make(url.Values))
			category, ok := oauthTokenFailureCategory(err)
			if !ok || category != string(test.expected) {
				t.Fatalf("failure category = %q, %v; want %q", category, err, test.expected)
			}
			if strings.Contains(err.Error(), "must not be logged") {
				t.Fatal("identity platform response description escaped into the error")
			}
		})
	}
}

func TestTokenRequestClassifiesLocalDependencyFailures(t *testing.T) {
	t.Run("assertion unavailable", func(t *testing.T) {
		client := newOAuthClient(Config{FederatedTokenFile: filepath.Join(t.TempDir(), "missing")})
		_, err := client.tokenRequest(context.Background(), make(url.Values))
		category, ok := oauthTokenFailureCategory(err)
		if !ok || category != string(oauthTokenAssertionUnavailable) {
			t.Fatalf("failure category = %q, %v; want %q", category, err, oauthTokenAssertionUnavailable)
		}
	})

	t.Run("identity platform unavailable", func(t *testing.T) {
		assertionFile := filepath.Join(t.TempDir(), "federated-token")
		if err := os.WriteFile(assertionFile, []byte("synthetic-assertion"), 0o600); err != nil {
			t.Fatal(err)
		}
		client := newOAuthClient(Config{TenantID: "11111111-1111-1111-1111-111111111111", FederatedTokenFile: assertionFile})
		client.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic transport detail")
		})}
		_, err := client.tokenRequest(context.Background(), make(url.Values))
		category, ok := oauthTokenFailureCategory(err)
		if !ok || category != string(oauthTokenPlatformUnavailable) {
			t.Fatalf("failure category = %q, %v; want %q", category, err, oauthTokenPlatformUnavailable)
		}
		if strings.Contains(err.Error(), "synthetic transport detail") {
			t.Fatal("transport error detail escaped into the error")
		}
	})
}

func TestTokenGrantsUseScopesBoundToAuthorizationCode(t *testing.T) {
	assertionFile := filepath.Join(t.TempDir(), "federated-token")
	if err := os.WriteFile(assertionFile, []byte("synthetic-assertion"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newOAuthClient(Config{
		TenantID:              "11111111-1111-1111-1111-111111111111",
		AuthorizationClientID: "33333333-3333-3333-3333-333333333333",
		RedirectURI:           "https://portal.example.com/api/v1/connections/callback",
		FederatedTokenFile:    assertionFile,
	})
	wantGrants := []string{"authorization_code", "refresh_token"}
	requestIndex := 0
	client.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if requestIndex >= len(wantGrants) || request.Form.Get("grant_type") != wantGrants[requestIndex] {
			t.Fatalf("grant type = %q at request %d", request.Form.Get("grant_type"), requestIndex)
		}
		if request.Form.Has("scope") {
			t.Fatal("token grant must use the scopes already bound to its code or refresh token")
		}
		requestIndex++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"synthetic-access","expires_in":3600}`)),
		}, nil
	})}
	if _, err := client.exchangeCode(context.Background(), "synthetic-code", "synthetic-verifier"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.refresh(context.Background(), "synthetic-refresh"); err != nil {
		t.Fatal(err)
	}
	if requestIndex != len(wantGrants) {
		t.Fatalf("token requests = %d, want %d", requestIndex, len(wantGrants))
	}
}

func signedTestIDToken(t *testing.T, key *rsa.PrivateKey, keyID, algorithm string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": algorithm, "kid": keyID})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestVerifyIDTokenClassifiesValidationFailures(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const (
		tenant   = "11111111-1111-1111-1111-111111111111"
		audience = "22222222-2222-2222-2222-222222222222"
		owner    = "33333333-3333-3333-3333-333333333333"
		nonce    = "expected-nonce"
		keyID    = "test-key"
	)
	verifier := newJWTVerifier(tenant, audience, time.Second)
	verifier.keys[keyID] = &key.PublicKey
	verifier.fetched = time.Now()
	validClaims := func() map[string]any {
		return map[string]any{
			"iss": verifier.issuer, "aud": audience, "tid": tenant, "oid": owner,
			"nonce": nonce, "preferred_username": "user@example.invalid",
			"exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
		}
	}
	verifyFailure := func(t *testing.T, token string, expected idTokenFailure) {
		t.Helper()
		_, err := verifier.verifyIDToken(context.Background(), token, nonce, tenant, owner)
		category, ok := idTokenFailureCategory(err)
		if !ok || category != string(expected) {
			t.Fatalf("failure category = %q, %v; want %q", category, err, expected)
		}
	}

	t.Run("valid", func(t *testing.T) {
		account, err := verifier.verifyIDToken(context.Background(), signedTestIDToken(t, key, keyID, "RS256", validClaims()), nonce, tenant, owner)
		if err != nil || account != "user@example.invalid" {
			t.Fatalf("account = %q, error = %v", account, err)
		}
	})
	t.Run("case insensitive identity IDs", func(t *testing.T) {
		claims := validClaims()
		claims["tid"] = strings.ToUpper(tenant)
		claims["oid"] = strings.ToUpper(owner)
		account, err := verifier.verifyIDToken(context.Background(), signedTestIDToken(t, key, keyID, "RS256", claims), nonce, tenant, owner)
		if err != nil || account != "user@example.invalid" {
			t.Fatalf("case-only identity ID difference was rejected: account=%q error=%v", account, err)
		}
	})

	tests := []struct {
		name     string
		expected idTokenFailure
		mutate   func(map[string]any)
	}{
		{name: "issuer", expected: idTokenIssuer, mutate: func(claims map[string]any) { claims["iss"] = "https://issuer.example.invalid" }},
		{name: "audience", expected: idTokenAudience, mutate: func(claims map[string]any) { claims["aud"] = "other-audience" }},
		{name: "tenant", expected: idTokenTenant, mutate: func(claims map[string]any) { claims["tid"] = "other-tenant" }},
		{name: "owner_missing", expected: idTokenOwnerMissing, mutate: func(claims map[string]any) { claims["oid"] = "" }},
		{name: "owner", expected: idTokenOwner, mutate: func(claims map[string]any) { claims["oid"] = "other-owner" }},
		{name: "nonce", expected: idTokenNonce, mutate: func(claims map[string]any) { claims["nonce"] = "other-nonce" }},
		{name: "expired", expected: idTokenExpired, mutate: func(claims map[string]any) { claims["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{name: "not_before", expected: idTokenNotBefore, mutate: func(claims map[string]any) { claims["nbf"] = time.Now().Add(2 * time.Minute).Unix() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := validClaims()
			test.mutate(claims)
			verifyFailure(t, signedTestIDToken(t, key, keyID, "RS256", claims), test.expected)
		})
	}

	t.Run("malformed", func(t *testing.T) {
		verifyFailure(t, "not-a-jwt", idTokenMalformed)
	})
	t.Run("invalid_header", func(t *testing.T) {
		verifyFailure(t, signedTestIDToken(t, key, keyID, "HS256", validClaims()), idTokenInvalidHeader)
	})
	t.Run("invalid_signature", func(t *testing.T) {
		token := signedTestIDToken(t, key, keyID, "RS256", validClaims())
		parts := strings.Split(token, ".")
		parts[2] = base64.RawURLEncoding.EncodeToString([]byte("invalid-signature"))
		verifyFailure(t, strings.Join(parts, "."), idTokenInvalidSignature)
	})
	t.Run("invalid_claims", func(t *testing.T) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key"}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
		unsigned := header + "." + payload
		digest := sha256.Sum256([]byte(unsigned))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		verifyFailure(t, unsigned+"."+base64.RawURLEncoding.EncodeToString(signature), idTokenInvalidClaims)
	})
	t.Run("signing_key", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		unknownKeyVerifier := newJWTVerifier(tenant, audience, time.Second)
		unknownKeyVerifier.jwksURL = server.URL
		token := signedTestIDToken(t, key, "unknown-key", "RS256", validClaims())
		_, err := unknownKeyVerifier.verifyIDToken(context.Background(), token, nonce, tenant, owner)
		category, ok := idTokenFailureCategory(err)
		if !ok || category != string(idTokenSigningKey) {
			t.Fatalf("failure category = %q, %v; want %q", category, err, idTokenSigningKey)
		}
	})
}
