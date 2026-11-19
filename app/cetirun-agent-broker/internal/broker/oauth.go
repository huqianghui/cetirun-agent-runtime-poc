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
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type oauthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type oauthTokenFailure string

const (
	oauthTokenAssertionUnavailable oauthTokenFailure = "client_assertion_unavailable"
	oauthTokenPlatformUnavailable  oauthTokenFailure = "identity_platform_unavailable"
	oauthTokenInvalidRequest       oauthTokenFailure = "invalid_request"
	oauthTokenInvalidClient        oauthTokenFailure = "invalid_client"
	oauthTokenInvalidGrant         oauthTokenFailure = "invalid_grant"
	oauthTokenUnauthorizedClient   oauthTokenFailure = "unauthorized_client"
	oauthTokenInvalidScope         oauthTokenFailure = "invalid_scope"
	oauthTokenEndpointRejected     oauthTokenFailure = "endpoint_rejected"
	oauthTokenInvalidResponse      oauthTokenFailure = "invalid_response"
)

type oauthTokenError struct {
	failure oauthTokenFailure
}

func (e *oauthTokenError) Error() string {
	return "authorization token exchange failed"
}

func newOAuthTokenError(failure oauthTokenFailure) error {
	return &oauthTokenError{failure: failure}
}

func oauthTokenFailureCategory(err error) (string, bool) {
	var tokenError *oauthTokenError
	if !errors.As(err, &tokenError) {
		return "", false
	}
	return string(tokenError.failure), true
}

type oauthClient struct {
	cfg      Config
	client   *http.Client
	verifier *jwtVerifier
}

func newOAuthClient(cfg Config) *oauthClient {
	return &oauthClient{
		cfg:      cfg,
		client:   &http.Client{Timeout: cfg.RequestTimeout},
		verifier: newJWTVerifier(cfg.TenantID, cfg.AuthorizationClientID, cfg.RequestTimeout),
	}
}

func randomValue(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (c *oauthClient) authorizationURL(state, nonce, challenge, loginHint string) string {
	query := url.Values{
		"client_id":             {c.cfg.AuthorizationClientID},
		"response_type":         {"code"},
		"response_mode":         {"form_post"},
		"redirect_uri":          {c.cfg.RedirectURI},
		"scope":                 {c.authorizationScopes()},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if validLoginHint(loginHint) {
		query.Set("login_hint", loginHint)
	}
	return "https://login.microsoftonline.com/" + c.cfg.TenantID + "/oauth2/v2.0/authorize?" + query.Encode()
}

func (c *oauthClient) authorizationScopes() string {
	// profile is required for Entra to include the immutable user object ID in
	// the ID token. openid alone authenticates the user but does not guarantee
	// the oid claim used for the instance-owner binding.
	return "api://" + c.cfg.BlueprintClientID + "/access_agent offline_access openid profile"
}

func validLoginHint(value string) bool {
	// Entra's optional login_hint claim is an opaque base64-encoded value, not
	// necessarily a UPN or email address. It is URL-encoded below and is never
	// used as an authorization identifier.
	if len(value) == 0 || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}

func pkceChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (c *oauthClient) exchangeCode(ctx context.Context, code, verifier string) (oauthTokens, error) {
	form := url.Values{
		"client_id":     {c.cfg.AuthorizationClientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.cfg.RedirectURI},
		"code_verifier": {verifier},
	}
	return c.tokenRequest(ctx, form)
}

func (c *oauthClient) refresh(ctx context.Context, refreshToken string) (oauthTokens, error) {
	form := url.Values{
		"client_id":     {c.cfg.AuthorizationClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return c.tokenRequest(ctx, form)
}

func (c *oauthClient) tokenRequest(ctx context.Context, form url.Values) (oauthTokens, error) {
	assertion, err := os.ReadFile(c.cfg.FederatedTokenFile)
	if err != nil || len(assertion) == 0 {
		return oauthTokens{}, newOAuthTokenError(oauthTokenAssertionUnavailable)
	}
	form.Set("client_assertion_type", tokenAssertionType)
	form.Set("client_assertion", strings.TrimSpace(string(assertion)))
	endpoint := "https://login.microsoftonline.com/" + c.cfg.TenantID + "/oauth2/v2.0/token"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return oauthTokens{}, newOAuthTokenError(oauthTokenPlatformUnavailable)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var result oauthTokens
	if resp.StatusCode != http.StatusOK {
		var responseError struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &responseError)
		failure := oauthTokenEndpointRejected
		switch responseError.Error {
		case "invalid_request":
			failure = oauthTokenInvalidRequest
		case "invalid_client":
			failure = oauthTokenInvalidClient
		case "invalid_grant":
			failure = oauthTokenInvalidGrant
		case "unauthorized_client":
			failure = oauthTokenUnauthorizedClient
		case "invalid_scope":
			failure = oauthTokenInvalidScope
		}
		return oauthTokens{}, newOAuthTokenError(failure)
	}
	if json.Unmarshal(body, &result) != nil || result.AccessToken == "" || result.ExpiresIn <= 0 {
		return oauthTokens{}, newOAuthTokenError(oauthTokenInvalidResponse)
	}
	return result, nil
}

type jwtVerifier struct {
	tenantID string
	audience string
	issuer   string
	jwksURL  string
	client   *http.Client
	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey
	fetched  time.Time
}

type idTokenFailure string

const (
	idTokenMalformed        idTokenFailure = "malformed"
	idTokenInvalidHeader    idTokenFailure = "invalid_header"
	idTokenSigningKey       idTokenFailure = "signing_key"
	idTokenInvalidSignature idTokenFailure = "invalid_signature"
	idTokenInvalidClaims    idTokenFailure = "invalid_claims"
	idTokenIssuer           idTokenFailure = "issuer"
	idTokenAudience         idTokenFailure = "audience"
	idTokenTenant           idTokenFailure = "tenant"
	idTokenOwnerMissing     idTokenFailure = "owner_missing"
	idTokenOwner            idTokenFailure = "owner"
	idTokenNonce            idTokenFailure = "nonce"
	idTokenExpired          idTokenFailure = "expired"
	idTokenNotBefore        idTokenFailure = "not_before"
)

type idTokenValidationError struct {
	failure idTokenFailure
}

func (e *idTokenValidationError) Error() string {
	return "ID token validation failed"
}

func newIDTokenValidationError(failure idTokenFailure) error {
	return &idTokenValidationError{failure: failure}
}

func idTokenFailureCategory(err error) (string, bool) {
	var validationError *idTokenValidationError
	if !errors.As(err, &validationError) {
		return "", false
	}
	return string(validationError.failure), true
}

func newJWTVerifier(tenantID, audience string, timeout time.Duration) *jwtVerifier {
	return &jwtVerifier{
		tenantID: tenantID,
		audience: audience,
		issuer:   "https://login.microsoftonline.com/" + tenantID + "/v2.0",
		jwksURL:  "https://login.microsoftonline.com/" + tenantID + "/discovery/v2.0/keys",
		client:   &http.Client{Timeout: timeout},
		keys:     map[string]*rsa.PublicKey{},
	}
}

func (v *jwtVerifier) verifyIDToken(ctx context.Context, compact, nonce, ownerTenantID, ownerObjectID string) (string, error) {
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return "", newIDTokenValidationError(idTokenMalformed)
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if decodePart(parts[0], &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return "", newIDTokenValidationError(idTokenInvalidHeader)
	}
	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return "", newIDTokenValidationError(idTokenSigningKey)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", newIDTokenValidationError(idTokenInvalidSignature)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return "", newIDTokenValidationError(idTokenInvalidSignature)
	}
	var claims struct {
		Issuer            string          `json:"iss"`
		Audience          json.RawMessage `json:"aud"`
		TenantID          string          `json:"tid"`
		ObjectID          string          `json:"oid"`
		Nonce             string          `json:"nonce"`
		PreferredUsername string          `json:"preferred_username"`
		Expires           int64           `json:"exp"`
		NotBefore         int64           `json:"nbf"`
	}
	if decodePart(parts[1], &claims) != nil {
		return "", newIDTokenValidationError(idTokenInvalidClaims)
	}
	now := time.Now().Unix()
	if claims.Issuer != v.issuer {
		return "", newIDTokenValidationError(idTokenIssuer)
	}
	if !rawAudienceContains(claims.Audience, v.audience) {
		return "", newIDTokenValidationError(idTokenAudience)
	}
	if !strings.EqualFold(claims.TenantID, ownerTenantID) {
		return "", newIDTokenValidationError(idTokenTenant)
	}
	if claims.ObjectID == "" {
		return "", newIDTokenValidationError(idTokenOwnerMissing)
	}
	if !strings.EqualFold(claims.ObjectID, ownerObjectID) {
		return "", newIDTokenValidationError(idTokenOwner)
	}
	if claims.Nonce != nonce {
		return "", newIDTokenValidationError(idTokenNonce)
	}
	if claims.Expires <= now {
		return "", newIDTokenValidationError(idTokenExpired)
	}
	if claims.NotBefore > now+60 {
		return "", newIDTokenValidationError(idTokenNotBefore)
	}
	return claims.PreferredUsername, nil
}

func decodePart(part string, target any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func rawAudienceContains(raw json.RawMessage, expected string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == expected
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, item := range many {
		if item == expected {
			return true
		}
	}
	return false
}

func (v *jwtVerifier) key(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, found := v.keys[keyID]
	fresh := time.Since(v.fetched) < time.Hour
	v.mu.RUnlock()
	if found && fresh {
		return key, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	key, found = v.keys[keyID]
	if !found {
		return nil, errors.New("ID token signing key is unknown")
	}
	return key, nil
}

func (v *jwtVerifier) refresh(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	resp, err := v.client.Do(req)
	if err != nil {
		return errors.New("identity signing keys are unavailable")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("identity signing-key endpoint returned HTTP %d", resp.StatusCode)
	}
	var document struct {
		Keys []struct {
			KeyID string `json:"kid"`
			Type  string `json:"kty"`
			N     string `json:"n"`
			E     string `json:"e"`
		} `json:"keys"`
	}
	if json.Unmarshal(body, &document) != nil {
		return errors.New("identity signing-key document is invalid")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, item := range document.Keys {
		nBytes, nErr := base64.RawURLEncoding.DecodeString(item.N)
		eBytes, eErr := base64.RawURLEncoding.DecodeString(item.E)
		if item.Type != "RSA" || item.KeyID == "" || nErr != nil || eErr != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			continue
		}
		exponent := 0
		for _, value := range eBytes {
			exponent = exponent<<8 | int(value)
		}
		if exponent >= 3 {
			keys[item.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exponent}
		}
	}
	if len(keys) == 0 {
		return errors.New("identity signing-key document contains no usable keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}
