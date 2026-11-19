// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func signedToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key"})
	payload, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCVerifierBindsTenantAudienceAndObject(t *testing.T) {
	tenant := "11111111-1111-1111-1111-111111111111"
	client := "22222222-2222-2222-2222-222222222222"
	object := "33333333-3333-3333-3333-333333333333"
	verifier, err := NewOIDCVerifier(tenant, client)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier.keys["test-key"] = &key.PublicKey
	verifier.fetched = time.Now()
	claims := map[string]any{
		"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"aud": client, "tid": tenant, "oid": object,
		"login_hint": "opaque-base64-login-hint==",
		"nbf":        time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	}
	identity, err := verifier.Verify(context.Background(), signedToken(t, key, claims))
	if err != nil || identity.TenantID != tenant || identity.ObjectID != object || identity.LoginHint != "opaque-base64-login-hint==" {
		t.Fatalf("valid identity rejected: %#v %v", identity, err)
	}
	claims["aud"] = "44444444-4444-4444-4444-444444444444"
	if _, err := verifier.Verify(context.Background(), signedToken(t, key, claims)); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("token for another audience returned %v", err)
	}
}

func TestOIDCVerifierRejectsUnsafeOptionalLoginHintWithoutRejectingIdentity(t *testing.T) {
	tenant := "11111111-1111-1111-1111-111111111111"
	client := "22222222-2222-2222-2222-222222222222"
	verifier, err := NewOIDCVerifier(tenant, client)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier.keys["test-key"] = &key.PublicKey
	verifier.fetched = time.Now()
	identity, err := verifier.Verify(context.Background(), signedToken(t, key, map[string]any{
		"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"aud": client, "tid": tenant, "oid": "33333333-3333-3333-3333-333333333333",
		"login_hint": "unsafe\nheader",
		"nbf":        time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	}))
	if err != nil {
		t.Fatalf("optional hint should not invalidate the signed identity: %v", err)
	}
	if identity.LoginHint != "" {
		t.Fatalf("unsafe login hint was retained: %q", identity.LoginHint)
	}
}

func TestOIDCVerifierReportsSafeClaimFailureCategory(t *testing.T) {
	tenant := "11111111-1111-1111-1111-111111111111"
	client := "22222222-2222-2222-2222-222222222222"
	object := "33333333-3333-3333-3333-333333333333"
	verifier, err := NewOIDCVerifier(tenant, client)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier.keys["test-key"] = &key.PublicKey
	verifier.fetched = time.Now()
	now := time.Now()
	valid := map[string]any{
		"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"aud": client, "tid": tenant, "oid": object,
		"nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix(),
	}
	tests := []struct {
		name  string
		claim string
		value any
		want  string
	}{
		{name: "issuer", claim: "iss", value: "https://issuer.example.invalid", want: "issuer"},
		{name: "tenant", claim: "tid", value: "44444444-4444-4444-4444-444444444444", want: "tenant"},
		{name: "audience", claim: "aud", value: "55555555-5555-5555-5555-555555555555", want: "audience"},
		{name: "object", claim: "oid", value: "", want: "object ID is missing"},
		{name: "expired", claim: "exp", value: now.Add(-time.Minute).Unix(), want: "expired"},
		{name: "not before", claim: "nbf", value: now.Add(2 * time.Minute).Unix(), want: "not valid yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := make(map[string]any, len(valid))
			for key, value := range valid {
				claims[key] = value
			}
			claims[tt.claim] = tt.value
			_, err := verifier.Verify(context.Background(), signedToken(t, key, claims))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Verify() error = %v, want category %q", err, tt.want)
			}
			if strings.Contains(err.Error(), tenant) || strings.Contains(err.Error(), client) || strings.Contains(err.Error(), object) {
				t.Fatalf("Verify() exposed a claim value: %v", err)
			}
		})
	}
}

func TestOIDCVerifierRejectsWrongSignature(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	const client = "22222222-2222-2222-2222-222222222222"
	verifier, err := NewOIDCVerifier(tenant, client)
	if err != nil {
		t.Fatal(err)
	}
	trustedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	untrustedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier.keys["test-key"] = &trustedKey.PublicKey
	verifier.fetched = time.Now()
	token := signedToken(t, untrustedKey, map[string]any{
		"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"aud": client,
		"tid": tenant,
		"oid": "33333333-3333-3333-3333-333333333333",
		"nbf": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(time.Minute).Unix(),
	})

	if _, err := verifier.Verify(context.Background(), token); err == nil || !strings.Contains(err.Error(), "signature verification") {
		t.Fatalf("Verify() error = %v, want signature verification failure", err)
	}
}

func TestOIDCVerifierRejectsOversizedToken(t *testing.T) {
	verifier, err := NewOIDCVerifier("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), strings.Repeat("a", (32<<10)+1)); err == nil {
		t.Fatal("oversized token was accepted")
	}
}

func TestOIDCVerifierRefreshesUnknownKeyOnlyOnce(t *testing.T) {
	verifier, err := NewOIDCVerifier("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	exponent := big.NewInt(int64(key.PublicKey.E)).Bytes()
	document, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": "different-key",
		"kty": "RSA",
		"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(exponent),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document)
	}))
	defer server.Close()
	verifier.jwksURL = server.URL
	verifier.client = server.Client()

	if _, err := verifier.key(context.Background(), "unknown-key"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("key() error = %v, want unknown signing key", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("JWKS requests = %d, want 1", got)
	}
}

func TestOIDCVerifierCoalescesConcurrentRefreshes(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": "test-key",
		"kty": "RSA",
		"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}

	for _, setup := range []struct {
		name  string
		setup func(*OIDCVerifier)
	}{
		{name: "cold cache", setup: func(*OIDCVerifier) {}},
		{name: "stale cache", setup: func(verifier *OIDCVerifier) {
			verifier.keys["old-key"] = &key.PublicKey
			verifier.fetched = time.Now().Add(-time.Hour)
		}},
		{name: "unknown key", setup: func(verifier *OIDCVerifier) {
			verifier.keys["old-key"] = &key.PublicKey
			verifier.fetched = time.Now()
		}},
	} {
		t.Run(setup.name, func(t *testing.T) {
			verifier, err := NewOIDCVerifier("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
			if err != nil {
				t.Fatal(err)
			}
			setup.setup(verifier)
			var requests atomic.Int32
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					close(started)
				}
				<-release
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(document)
			}))
			defer server.Close()
			verifier.jwksURL = server.URL
			verifier.client = server.Client()

			const callers = 24
			start := make(chan struct{})
			errs := make(chan error, callers)
			var callersWG sync.WaitGroup
			for range callers {
				callersWG.Add(1)
				go func() {
					defer callersWG.Done()
					<-start
					_, err := verifier.key(context.Background(), "test-key")
					errs <- err
				}()
			}
			close(start)
			<-started
			close(release)
			callersWG.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("key() error = %v", err)
				}
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("JWKS requests = %d, want 1", got)
			}
		})
	}
}

func TestOIDCVerifierRefreshWaiterHonorsContextDeadline(t *testing.T) {
	verifier, err := NewOIDCVerifier("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	verifier.jwksURL = server.URL
	verifier.client = server.Client()

	leaderDone := make(chan error, 1)
	go func() {
		_, err := verifier.key(context.Background(), "test-key")
		leaderDone <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := verifier.key(ctx, "test-key"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting key() error = %v, want context deadline exceeded", err)
	}
	close(release)
	if err := <-leaderDone; err == nil {
		t.Fatal("leader key() unexpectedly succeeded")
	}
}

func TestOIDCVerifierRefreshSurvivesLeaderCancellation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": "test-key",
		"kty": "RSA",
		"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}

	verifier, err := NewOIDCVerifier("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document)
	}))
	defer server.Close()
	verifier.jwksURL = server.URL
	verifier.client = server.Client()

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leaderDone := make(chan error, 1)
	go func() {
		_, err := verifier.key(leaderCtx, "test-key")
		leaderDone <- err
	}()
	<-started

	healthyDone := make(chan error, 1)
	go func() {
		_, err := verifier.key(context.Background(), "test-key")
		healthyDone <- err
	}()
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled leader error = %v, want context canceled", err)
	}
	close(release)
	if err := <-healthyDone; err != nil {
		t.Fatalf("healthy waiter error = %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("JWKS requests = %d, want 1", got)
	}
}
