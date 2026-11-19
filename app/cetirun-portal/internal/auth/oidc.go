// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// VerifiedIdentity is the immutable tenant/user identity taken from a signed
// Entra ID token. It is intentionally separate from display headers.
type VerifiedIdentity struct {
	TenantID string
	ObjectID string
	// LoginHint is Entra's opaque optional claim for reliable SSO across
	// applications. It is a sign-in hint only and must never be used as an
	// authorization identifier.
	LoginHint string
}

type oidcClaims struct {
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	TenantID  string          `json:"tid"`
	ObjectID  string          `json:"oid"`
	LoginHint string          `json:"login_hint"`
	Expires   int64           `json:"exp"`
	NotBefore int64           `json:"nbf"`
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksRefresh struct {
	done chan struct{}
	err  error
}

const jwksRefreshTimeout = 10 * time.Second

// OIDCVerifier validates the ID token that ingress moves from the oauth2-proxy
// response into a Portal-only header. Keys are cached and refreshed on an
// unknown key ID.
type OIDCVerifier struct {
	tenantID string
	clientID string
	issuer   string
	jwksURL  string
	client   *http.Client
	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey
	fetched  time.Time
	// refreshMu serializes a JWKS refresh. Keeping it separate from mu means
	// cached-key verification never waits on network I/O.
	refreshMu         sync.Mutex
	refreshFlight     *jwksRefresh
	refreshGeneration uint64
}

func NewOIDCVerifier(tenantID, clientID string) (*OIDCVerifier, error) {
	if !canonicalID.MatchString(tenantID) || !canonicalID.MatchString(clientID) {
		return nil, errors.New("OIDC tenant and client IDs must be UUIDs")
	}
	issuer := "https://login.microsoftonline.com/" + tenantID + "/v2.0"
	return &OIDCVerifier{
		tenantID: tenantID,
		clientID: clientID,
		issuer:   issuer,
		jwksURL:  "https://login.microsoftonline.com/" + tenantID + "/discovery/v2.0/keys",
		client:   &http.Client{Timeout: 10 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
	}, nil
}

func (v *OIDCVerifier) Verify(ctx context.Context, compact string) (VerifiedIdentity, error) {
	if len(compact) == 0 || len(compact) > 32<<10 {
		return VerifiedIdentity{}, errors.New("identity token is malformed")
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return VerifiedIdentity{}, errors.New("identity token is malformed")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &header); err != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return VerifiedIdentity{}, errors.New("identity token header is invalid")
	}
	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return VerifiedIdentity{}, errors.New("identity token signature is invalid")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return VerifiedIdentity{}, errors.New("identity token signature verification failed")
	}
	var claims oidcClaims
	if err := decodeJWTPart(parts[1], &claims); err != nil {
		return VerifiedIdentity{}, errors.New("identity token claims are invalid")
	}
	now := time.Now().Unix()
	if claims.Issuer != v.issuer {
		return VerifiedIdentity{}, errors.New("identity token issuer is invalid")
	}
	if !strings.EqualFold(claims.TenantID, v.tenantID) {
		return VerifiedIdentity{}, errors.New("identity token tenant is invalid")
	}
	if !audienceContains(claims.Audience, v.clientID) {
		return VerifiedIdentity{}, errors.New("identity token audience is invalid")
	}
	if claims.ObjectID == "" {
		return VerifiedIdentity{}, errors.New("identity token object ID is missing")
	}
	if claims.Expires <= now {
		return VerifiedIdentity{}, errors.New("identity token is expired")
	}
	if claims.NotBefore > now+60 {
		return VerifiedIdentity{}, errors.New("identity token is not valid yet")
	}
	if !canonicalID.MatchString(claims.ObjectID) {
		return VerifiedIdentity{}, errors.New("identity token object ID is invalid")
	}
	return VerifiedIdentity{
		TenantID:  claims.TenantID,
		ObjectID:  claims.ObjectID,
		LoginHint: validEntraLoginHint(claims.LoginHint),
	}, nil
}

// validEntraLoginHint accepts the opaque, base64-encoded optional claim issued
// by Entra. The value is passed through unchanged as an OAuth login_hint; it is
// not decoded, displayed, logged, persisted, or used for authorization.
func validEntraLoginHint(value string) string {
	if len(value) == 0 || len(value) > 1024 {
		return ""
	}
	for _, character := range value {
		if character <= 0x20 || character > 0x7e {
			return ""
		}
	}
	return value
}

func decodeJWTPart(part string, target any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func audienceContains(raw json.RawMessage, expected string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == expected
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, audience := range many {
		if audience == expected {
			return true
		}
	}
	return false
}

func (v *OIDCVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	fresh := time.Since(v.fetched) < time.Hour
	generation := v.refreshGeneration
	v.mu.RUnlock()
	if ok && fresh {
		return key, nil
	}
	if err := v.refreshOnce(ctx, generation); err != nil {
		return nil, err
	}
	v.mu.RLock()
	key, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, errors.New("identity token signing key is unknown")
	}
	return key, nil
}

// refreshOnce coalesces concurrent cache refreshes. The shared fetch has a
// bounded lifetime independent of its callers, while every caller waits with
// its own request context. Therefore one canceled request cannot abort a
// refresh that other live requests are waiting for.
func (v *OIDCVerifier) refreshOnce(ctx context.Context, generation uint64) error {
	v.refreshMu.Lock()
	v.mu.RLock()
	refreshed := v.refreshGeneration != generation
	v.mu.RUnlock()
	if refreshed {
		v.refreshMu.Unlock()
		return nil
	}

	if flight := v.refreshFlight; flight != nil {
		v.refreshMu.Unlock()
		return waitForJWKSRefresh(ctx, flight)
	}

	flight := &jwksRefresh{done: make(chan struct{})}
	v.refreshFlight = flight
	v.refreshMu.Unlock()
	go v.runJWKSRefresh(flight)
	return waitForJWKSRefresh(ctx, flight)
}

func waitForJWKSRefresh(ctx context.Context, flight *jwksRefresh) error {
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *OIDCVerifier) runJWKSRefresh(flight *jwksRefresh) {
	ctx, cancel := context.WithTimeout(context.Background(), jwksRefreshTimeout)
	defer cancel()
	err := v.refresh(ctx)

	v.refreshMu.Lock()
	flight.err = err
	close(flight.done)
	if v.refreshFlight == flight {
		v.refreshFlight = nil
	}
	v.refreshMu.Unlock()
}

func (v *OIDCVerifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return errors.New("cannot create signing-key request")
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return errors.New("cannot retrieve identity signing keys")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("identity signing-key endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return errors.New("cannot read identity signing keys")
	}
	if len(body) > 1<<20 {
		return errors.New("identity signing-key document is too large")
	}
	var document struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return errors.New("identity signing-key document is invalid")
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, item := range document.Keys {
		if item.Kty != "RSA" || item.Kid == "" || item.N == "" || item.E == "" {
			continue
		}
		nBytes, nErr := base64.RawURLEncoding.DecodeString(item.N)
		eBytes, eErr := base64.RawURLEncoding.DecodeString(item.E)
		if nErr != nil || eErr != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			continue
		}
		var exponent uint32
		for _, b := range eBytes {
			exponent = exponent<<8 | uint32(b)
		}
		if exponent < 3 {
			continue
		}
		keys[item.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(exponent)}
	}
	if len(keys) == 0 {
		return errors.New("identity signing-key document contains no usable keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.refreshGeneration++
	v.mu.Unlock()
	return nil
}
