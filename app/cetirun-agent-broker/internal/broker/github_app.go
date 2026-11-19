// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const githubAPI = "https://api.github.com"

var errGitHubUserUnauthorized = errors.New("GitHub user authorization was rejected")

type githubInstallationSummary struct {
	ID          int64             `json:"id"`
	AppID       int64             `json:"app_id"`
	Account     githubAccount     `json:"account"`
	Permissions map[string]string `json:"permissions"`
	SuspendedAt *time.Time        `json:"suspended_at"`
}

type githubAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubRepository struct {
	ID          int64         `json:"id"`
	FullName    string        `json:"full_name"`
	Owner       githubAccount `json:"owner"`
	Permissions struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

type githubInstallationToken struct {
	Token        string             `json:"token"`
	ExpiresAt    time.Time          `json:"expires_at"`
	Permissions  map[string]string  `json:"permissions"`
	Repositories []githubRepository `json:"repositories"`
}

func (p *providerClient) githubAppAuthorizationURL(state, verifier string) (string, error) {
	if err := p.cfg.validateGitHubApp(); err != nil {
		return "", err
	}
	values := url.Values{"state": {state}, "client_id": {p.cfg.GitHubAppClientID}, "redirect_uri": {p.cfg.RedirectURI}, "code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"}}
	return "https://github.com/login/oauth/authorize?" + values.Encode(), nil
}

func (p *providerClient) exchangeGitHubAppUserCode(ctx context.Context, code, verifier string) (providerCredential, string, string, error) {
	if err := p.cfg.validateGitHubApp(); err != nil {
		return providerCredential{}, "", "", err
	}
	values := url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {p.cfg.GitHubAppClientID}, "client_secret": {p.cfg.GitHubAppClientSecret}, "grant_type": {"authorization_code"}}
	credential, err := p.tokenRequest(ctx, "github-app", "https://github.com/login/oauth/access_token", values)
	if err != nil || credential.NonExpiring || credential.ExpiresAt.IsZero() || credential.RefreshToken == "" {
		if err == nil {
			err = errors.New("GitHub App user authorization must include an expiring refreshable token")
		}
		return providerCredential{}, "", "", err
	}
	account, err := p.githubUser(ctx, credential.AccessToken)
	if err != nil {
		return providerCredential{}, "", "", err
	}
	return credential, account.Login, strconv.FormatInt(account.ID, 10), nil
}

func (p *providerClient) refreshGitHubAppUser(ctx context.Context, current providerCredential) (providerCredential, error) {
	if current.RefreshToken == "" {
		return providerCredential{}, newOAuthTokenError(oauthTokenInvalidGrant)
	}
	values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {current.RefreshToken}, "client_id": {p.cfg.GitHubAppClientID}, "client_secret": {p.cfg.GitHubAppClientSecret}}
	next, err := p.tokenRequest(ctx, "github-app", "https://github.com/login/oauth/access_token", values)
	if err != nil {
		return providerCredential{}, err
	}
	if next.NonExpiring || next.ExpiresAt.IsZero() {
		return providerCredential{}, errors.New("GitHub App user token expiry is missing")
	}
	if next.RefreshToken == "" {
		next.RefreshToken = current.RefreshToken
	}
	return next, nil
}

func (p *providerClient) githubUser(ctx context.Context, token string) (githubAccount, error) {
	var result githubAccount
	if err := p.githubJSON(ctx, http.MethodGet, githubAPI+"/user", token, nil, &result); err != nil || result.ID <= 0 || result.Login == "" {
		if err == nil {
			err = errors.New("GitHub user response is incomplete")
		}
		return githubAccount{}, err
	}
	return result, nil
}

func (p *providerClient) githubInstallations(ctx context.Context, token string) ([]githubInstallationSummary, error) {
	result := []githubInstallationSummary{}
	for page := 1; page <= 100; page++ {
		var response struct {
			Installations []githubInstallationSummary `json:"installations"`
		}
		endpoint := fmt.Sprintf("%s/user/installations?page=%d&per_page=100", githubAPI, page)
		if err := p.githubJSON(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
			return nil, err
		}
		for _, installation := range response.Installations {
			if installation.ID <= 0 || installation.AppID != p.cfg.GitHubAppID || installation.Account.Login == "" || installation.SuspendedAt != nil || (installation.Account.Type != "User" && installation.Account.Type != "Organization") {
				continue
			}
			result = append(result, installation)
		}
		if len(response.Installations) < 100 {
			break
		}
		if page == 100 {
			return nil, errors.New("GitHub installation list exceeds the supported limit")
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (p *providerClient) githubInstallation(ctx context.Context, token string, id int64) (githubInstallationSummary, error) {
	installations, err := p.githubInstallations(ctx, token)
	if err != nil {
		return githubInstallationSummary{}, err
	}
	for _, installation := range installations {
		if installation.ID == id {
			return installation, nil
		}
	}
	return githubInstallationSummary{}, errors.New("GitHub App installation is not available to this user")
}

func (p *providerClient) githubInstallationRepositories(ctx context.Context, token string, installationID int64, page int) ([]githubRepository, bool, error) {
	if installationID <= 0 || page < 1 {
		return nil, false, errors.New("GitHub installation repository request is invalid")
	}
	endpoint := fmt.Sprintf("%s/user/installations/%d/repositories?page=%d&per_page=100", githubAPI, installationID, page)
	var response struct {
		Repositories []githubRepository `json:"repositories"`
	}
	if err := p.githubJSON(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
		return nil, false, err
	}
	for _, repository := range response.Repositories {
		if repository.ID <= 0 || repository.FullName == "" || repository.Owner.ID <= 0 || repository.Owner.Login == "" || (repository.Owner.Type != "User" && repository.Owner.Type != "Organization") {
			return nil, false, errors.New("GitHub repository response is incomplete")
		}
	}
	return response.Repositories, len(response.Repositories) == 100, nil
}

func (p *providerClient) githubGrantRepositories(ctx context.Context, token string, installationID, userID int64, grant *agenttools.GitHubGrant) error {
	if !grant.Valid() || installationID <= 0 || userID <= 0 {
		return errors.New("GitHub App grant is invalid")
	}
	expected := make(map[int64]bool, len(grant.RepositoryIDs))
	for _, id := range grant.RepositoryIDs {
		expected[id] = false
	}
	for page := 1; page <= 100; page++ {
		repositories, more, err := p.githubInstallationRepositories(ctx, token, installationID, page)
		if err != nil {
			return err
		}
		for _, repository := range repositories {
			if _, present := expected[repository.ID]; !present {
				continue
			}
			if githubRepositoryAuthorized(repository, userID) {
				expected[repository.ID] = true
			}
		}
		complete := true
		for _, accepted := range expected {
			if !accepted {
				complete = false
				break
			}
		}
		if complete {
			return nil
		}
		if !more {
			break
		}
		if page == 100 {
			return errors.New("GitHub installation repository list exceeds the supported limit")
		}
	}
	for _, accepted := range expected {
		if !accepted {
			return errors.New("GitHub repository is unavailable or cannot be administered")
		}
	}
	return nil
}

func githubRepositoryAuthorized(repository githubRepository, userID int64) bool {
	return (repository.Owner.Type == "User" && repository.Owner.ID == userID) || (repository.Owner.Type == "Organization" && repository.Permissions.Admin)
}

func githubAppUserID(accountID string) (int64, bool) {
	value, ok := strings.CutPrefix(accountID, "github-app-user-")
	if !ok || value == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

func (p *providerClient) githubInstallationToken(ctx context.Context, installationID int64, grant *agenttools.GitHubGrant) (string, time.Time, error) {
	if installationID <= 0 || !grant.Valid() {
		return "", time.Time{}, errors.New("GitHub App grant is invalid")
	}
	jwt, err := p.githubAppJWT()
	if err != nil {
		return "", time.Time{}, err
	}
	payload := struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}{RepositoryIDs: grant.RepositoryIDs, Permissions: grant.Permissions}
	var response githubInstallationToken
	if err := p.githubJSON(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", githubAPI, installationID), jwt, payload, &response); err != nil {
		return "", time.Time{}, err
	}
	if response.Token == "" || !response.ExpiresAt.After(time.Now().UTC()) || !sameGitHubPermissions(response.Permissions, grant.Permissions) || !sameGitHubRepositories(response.Repositories, grant.RepositoryIDs) {
		return "", time.Time{}, errors.New("GitHub installation token authorization does not match the binding")
	}
	return response.Token, response.ExpiresAt.UTC(), nil
}

func sameGitHubPermissions(actual, requested map[string]string) bool {
	for permission, level := range actual {
		if permission == "metadata" && level == "read" {
			continue
		}
		if requested[permission] != level {
			return false
		}
	}
	for permission, level := range requested {
		if actual[permission] != level {
			return false
		}
	}
	return true
}

func sameGitHubRepositories(actual []githubRepository, requested []int64) bool {
	if len(actual) != len(requested) {
		return false
	}
	seen := make(map[int64]bool, len(actual))
	for _, repository := range actual {
		if repository.ID <= 0 || seen[repository.ID] {
			return false
		}
		seen[repository.ID] = true
	}
	for _, id := range requested {
		if !seen[id] {
			return false
		}
	}
	return true
}

func (p *providerClient) githubAppJWT() (string, error) {
	if err := p.cfg.validateGitHubApp(); err != nil {
		return "", err
	}
	pemData, err := os.ReadFile(p.cfg.GitHubAppPrivateKeyFile)
	if err != nil {
		return "", errors.New("GitHub App private key is unavailable")
	}
	block, _ := pem.Decode(pemData)
	if block == nil {
		return "", errors.New("GitHub App private key is invalid")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr != nil {
			return "", errors.New("GitHub App private key is invalid")
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("GitHub App private key is not RSA")
		}
	}
	now := time.Now().UTC()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(p.cfg.GitHubAppID, 10)})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("GitHub App JWT signing failed")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (p *providerClient) githubJSON(ctx context.Context, method, endpoint, token string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(req)
	if err != nil {
		return errors.New("GitHub API is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return errGitHubUserUnauthorized
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("GitHub API rejected the request")
	}
	if result != nil {
		data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
		if err != nil || len(data) > 8<<20 || json.Unmarshal(data, result) != nil {
			return errors.New("GitHub API returned invalid JSON")
		}
	}
	return nil
}
