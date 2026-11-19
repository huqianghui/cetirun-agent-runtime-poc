// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type providerClient struct {
	cfg    Config
	client *http.Client
}

func newProviderClient(cfg Config) *providerClient {
	return &providerClient{cfg: cfg, client: &http.Client{Timeout: cfg.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func connectionServices(connection Connection) []string {
	return agenttools.ProviderServices(connection.Provider)
}

func projectedConnection(connection Connection) Connection {
	connection.Mode = connectionMode(connection)
	connection.Services = connectionServices(connection)
	if connection.Scopes == nil {
		connection.Scopes = []string{}
	}
	return connection
}

func (p *providerClient) authorizationURL(provider, state, verifier, nonce string) (string, error) {
	values := url.Values{"state": {state}, "code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"}}
	switch provider {
	case "azure", "microsoft":
		return newOAuthClient(p.cfg).authorizationURL(state, nonce, pkceChallenge(verifier), ""), nil
	case "github":
		if p.cfg.GitHubOAuthClientID == "" || p.cfg.GitHubOAuthClientSecret == "" {
			return "", errors.New("GitHub OAuth is not configured")
		}
		values.Set("client_id", p.cfg.GitHubOAuthClientID)
		values.Set("redirect_uri", p.cfg.RedirectURI)
		// The repo scope is intentionally coarse because GitHub OAuth Apps cannot
		// select individual repository permissions. GitHub enforces the account's
		// actual permissions when executing each API request.
		values.Set("scope", "repo")
		return "https://github.com/login/oauth/authorize?" + values.Encode(), nil
	case "google":
		if p.cfg.GoogleClientID == "" || p.cfg.GoogleClientSecret == "" {
			return "", errors.New("Google OAuth is not configured")
		}
		values.Set("client_id", p.cfg.GoogleClientID)
		values.Set("redirect_uri", p.cfg.RedirectURI)
		values.Set("response_type", "code")
		values.Set("access_type", "offline")
		// A new connection or reconnect must receive its own refresh token;
		// Google may omit it when reusing previously granted consent.
		values.Set("prompt", "consent")
		values.Set("include_granted_scopes", "false")
		values.Set("scope", "openid email https://www.googleapis.com/auth/gmail.readonly https://www.googleapis.com/auth/gmail.send https://www.googleapis.com/auth/calendar")
		return "https://accounts.google.com/o/oauth2/v2/auth?" + values.Encode(), nil
	default:
		return "", errors.New("unsupported provider")
	}
}

func (p *providerClient) exchangeCode(ctx context.Context, provider, code, verifier string) (providerCredential, string, string, error) {
	if provider == "azure" || provider == "microsoft" {
		return providerCredential{}, "", "", errors.New("Microsoft connections require owner-bound callback validation")
	}
	values := url.Values{"code": {code}, "code_verifier": {verifier}}
	endpoint, clientID, secret := "", "", ""
	switch provider {
	case "github":
		endpoint, clientID, secret = "https://github.com/login/oauth/access_token", p.cfg.GitHubOAuthClientID, p.cfg.GitHubOAuthClientSecret
	case "google":
		endpoint, clientID, secret = "https://oauth2.googleapis.com/token", p.cfg.GoogleClientID, p.cfg.GoogleClientSecret
		values.Set("redirect_uri", p.cfg.RedirectURI)
	default:
		return providerCredential{}, "", "", errors.New("unsupported provider")
	}
	values.Set("client_id", clientID)
	values.Set("client_secret", secret)
	values.Set("grant_type", "authorization_code")
	credential, err := p.tokenRequest(ctx, provider, endpoint, values)
	if err != nil {
		return providerCredential{}, "", "", err
	}
	if provider == "google" && credential.RefreshToken == "" {
		return providerCredential{}, "", "", errors.New("Google offline access was not granted")
	}
	accountID, display, err := p.account(ctx, provider, credential.AccessToken)
	if err != nil {
		return providerCredential{}, "", "", err
	}

	return credential, display, accountID, nil
}

func credentialFromOAuth(tokens oauthTokens) providerCredential {
	return providerCredential{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: time.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)}
}

func (p *providerClient) refresh(ctx context.Context, provider string, current providerCredential) (providerCredential, error) {
	if current.NonExpiring {
		return providerCredential{}, errors.New("provider credential does not support refresh")
	}
	if current.RefreshToken == "" {
		return providerCredential{}, newOAuthTokenError(oauthTokenInvalidGrant)
	}
	if provider == "azure" || provider == "microsoft" {
		tokens, err := newOAuthClient(p.cfg).refresh(ctx, current.RefreshToken)
		if err != nil {
			return providerCredential{}, err
		}
		next := credentialFromOAuth(tokens)
		if next.RefreshToken == "" {
			next.RefreshToken = current.RefreshToken
		}
		if len(next.Scopes) == 0 {
			next.Scopes = current.Scopes
		}
		return next, nil
	}
	values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {current.RefreshToken}}
	endpoint := ""
	switch provider {
	case "github":
		endpoint = "https://github.com/login/oauth/access_token"
		values.Set("client_id", p.cfg.GitHubOAuthClientID)
		values.Set("client_secret", p.cfg.GitHubOAuthClientSecret)
	case "google":
		endpoint = "https://oauth2.googleapis.com/token"
		values.Set("client_id", p.cfg.GoogleClientID)
		values.Set("client_secret", p.cfg.GoogleClientSecret)
	default:
		return providerCredential{}, errors.New("unsupported provider")
	}
	next, err := p.tokenRequest(ctx, provider, endpoint, values)
	if err != nil {
		return providerCredential{}, err
	}
	if next.RefreshToken == "" {
		next.RefreshToken = current.RefreshToken
	}
	if len(next.Scopes) == 0 {
		next.Scopes = current.Scopes
	}
	return next, nil
}

func (p *providerClient) tokenRequest(ctx context.Context, provider, endpoint string, values url.Values) (providerCredential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return providerCredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := p.client.Do(req)
	if err != nil {
		return providerCredential{}, errors.New("provider token endpoint is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&failure)
		if (response.StatusCode == 400 || response.StatusCode == 401) && failure.Error == "invalid_grant" {
			return providerCredential{}, newOAuthTokenError(oauthTokenInvalidGrant)
		}
		return providerCredential{}, errors.New("provider token endpoint rejected the request")
	}
	var payload struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Scope        string          `json:"scope"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&payload) != nil || payload.AccessToken == "" {
		return providerCredential{}, errors.New("provider token endpoint returned an invalid response")
	}
	if len(payload.ExpiresIn) == 0 {
		if provider != "github" {
			return providerCredential{}, errors.New("provider token endpoint returned an invalid response")
		}
		return providerCredential{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, NonExpiring: true, Scopes: githubScopes(payload.Scope)}, nil
	}
	var expiresIn int64
	if bytes.Equal(bytes.TrimSpace(payload.ExpiresIn), []byte("null")) || json.Unmarshal(payload.ExpiresIn, &expiresIn) != nil || expiresIn <= 0 || expiresIn > int64(time.Duration(1<<63-1)/time.Second) {
		return providerCredential{}, errors.New("provider token endpoint returned an invalid response")
	}
	credential := providerCredential{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, ExpiresAt: time.Now().UTC().Add(time.Duration(expiresIn) * time.Second), Scopes: strings.Fields(payload.Scope)}
	if provider == "github" {
		credential.Scopes = githubScopes(payload.Scope)
	}
	return credential, nil
}

func (p *providerClient) account(ctx context.Context, provider, token string) (string, string, error) {
	endpoint := ""
	switch provider {
	case "github":
		endpoint = "https://api.github.com/user"
	case "google":
		endpoint = "https://openidconnect.googleapis.com/v1/userinfo"
	default:
		return "", "", errors.New("unsupported provider")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	response, err := p.client.Do(req)
	if err != nil {
		return "", "", errors.New("provider account endpoint is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", errors.New("provider account endpoint rejected the token")
	}
	var payload map[string]any
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		return "", "", errors.New("provider account endpoint returned invalid JSON")
	}
	if provider == "github" {
		id, _ := payload["id"].(json.Number)
		login, _ := payload["login"].(string)
		accountID, err := strconv.ParseUint(id.String(), 10, 64)
		if err != nil || accountID == 0 || login == "" {
			return "", "", errors.New("GitHub account response is incomplete")
		}
		return id.String(), login, nil
	}
	sub, _ := payload["sub"].(string)
	email, _ := payload["email"].(string)
	if sub == "" || email == "" {
		return "", "", errors.New("Google account response is incomplete")
	}
	return sub, email, nil
}

func githubScopes(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
	return uniqueStrings(parts)
}
