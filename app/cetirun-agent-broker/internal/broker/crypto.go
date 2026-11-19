// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const tokenAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

type keyWrapper interface {
	Wrap(context.Context, []byte) ([]byte, string, error)
	Unwrap(context.Context, []byte, string) ([]byte, error)
}

type envelope struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	WrappedDEK string `json:"wrappedDek"`
	KeyID      string `json:"keyId"`
}

func seal(ctx context.Context, wrapper keyWrapper, plaintext, aad []byte) ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("generate data encryption key: %w", err)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate envelope nonce: %w", err)
	}
	wrapped, keyID, err := wrapper.Wrap(ctx, dek)
	for index := range dek {
		dek[index] = 0
	}
	if err != nil {
		return nil, err
	}
	value := envelope{
		Version:    1,
		Algorithm:  "AES-256-GCM+RSA-OAEP-256",
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, plaintext, aad)),
		WrappedDEK: base64.RawURLEncoding.EncodeToString(wrapped),
		KeyID:      keyID,
	}
	return json.Marshal(value)
}

func openEnvelope(ctx context.Context, wrapper keyWrapper, encoded, aad []byte) ([]byte, error) {
	var value envelope
	if err := json.Unmarshal(encoded, &value); err != nil || value.Version != 1 || value.Algorithm != "AES-256-GCM+RSA-OAEP-256" {
		return nil, errors.New("authorization envelope is invalid")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(value.Nonce)
	if err != nil {
		return nil, errors.New("authorization envelope nonce is invalid")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(value.Ciphertext)
	if err != nil {
		return nil, errors.New("authorization envelope ciphertext is invalid")
	}
	wrapped, err := base64.RawURLEncoding.DecodeString(value.WrappedDEK)
	if err != nil {
		return nil, errors.New("authorization envelope key is invalid")
	}
	dek, err := wrapper.Unwrap(ctx, wrapped, value.KeyID)
	if err != nil {
		return nil, err
	}
	defer func() {
		for index := range dek {
			dek[index] = 0
		}
	}()
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, errors.New("authorization envelope key is invalid")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != aead.NonceSize() {
		return nil, errors.New("authorization envelope nonce is invalid")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("authorization envelope authentication failed")
	}
	return plaintext, nil
}

type keyVaultWrapper struct {
	tenantID       string
	clientID       string
	tokenFile      string
	configuredKey  string
	httpClient     *http.Client
	mu             sync.Mutex
	accessToken    string
	tokenExpiresAt time.Time
}

func newKeyVaultWrapper(cfg Config) *keyVaultWrapper {
	return &keyVaultWrapper{
		tenantID:      cfg.TenantID,
		clientID:      cfg.EncryptionClientID,
		tokenFile:     cfg.FederatedTokenFile,
		configuredKey: cfg.KeyVaultKeyURL,
		httpClient:    &http.Client{Timeout: cfg.RequestTimeout},
	}
}

func (k *keyVaultWrapper) Wrap(ctx context.Context, plaintext []byte) ([]byte, string, error) {
	var result struct {
		KeyID string `json:"kid"`
		Value string `json:"value"`
	}
	if err := k.keyOperation(ctx, k.configuredKey, "wrapkey", plaintext, &result); err != nil {
		return nil, "", err
	}
	value, err := base64.RawURLEncoding.DecodeString(result.Value)
	if err != nil || result.KeyID == "" {
		return nil, "", errors.New("Key Vault returned an invalid wrap response")
	}
	return value, result.KeyID, nil
}

func (k *keyVaultWrapper) Unwrap(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	if !sameKey(k.configuredKey, keyID) {
		return nil, errors.New("authorization envelope references an unexpected Key Vault key")
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := k.keyOperation(ctx, keyID, "unwrapkey", wrapped, &result); err != nil {
		return nil, err
	}
	value, err := base64.RawURLEncoding.DecodeString(result.Value)
	if err != nil || len(value) != 32 {
		return nil, errors.New("Key Vault returned an invalid unwrap response")
	}
	return value, nil
}

func sameKey(configured, returned string) bool {
	wantHost, wantName, wantVersion, wantOK := parseKeyID(configured, false)
	gotHost, gotName, gotVersion, gotOK := parseKeyID(returned, true)
	if !wantOK || !gotOK || !strings.EqualFold(wantHost, gotHost) || wantName != gotName {
		return false
	}
	// The configured URL may be versionless. Key Vault returns and persists the
	// exact version used for wrapping so rotation does not make old records unreadable.
	return wantVersion == "" || wantVersion == gotVersion
}

func parseKeyID(raw string, requireVersion bool) (host, name, version string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Host != u.Hostname() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "keys" || parts[1] == "" || (requireVersion && len(parts) != 3) {
		return "", "", "", false
	}
	if len(parts) == 3 {
		version = parts[2]
		if version == "" {
			return "", "", "", false
		}
	}
	for _, part := range parts[1:] {
		if url.PathEscape(part) != part || part == "." || part == ".." {
			return "", "", "", false
		}
	}
	return u.Hostname(), parts[1], version, true
}

func (k *keyVaultWrapper) keyOperation(ctx context.Context, keyID, operation string, value []byte, result any) error {
	token, err := k.token(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"alg": "RSA-OAEP-256", "value": base64.RawURLEncoding.EncodeToString(value)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(keyID, "/")+"/"+operation+"?api-version=7.4", bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create Key Vault request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return errors.New("Key Vault is unavailable")
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Key Vault %s failed with HTTP %d", operation, resp.StatusCode)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("Key Vault returned an invalid response")
	}
	return nil
}

func (k *keyVaultWrapper) token(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.accessToken != "" && time.Until(k.tokenExpiresAt) > 2*time.Minute {
		return k.accessToken, nil
	}
	assertion, err := os.ReadFile(k.tokenFile)
	if err != nil || len(assertion) == 0 {
		return "", errors.New("workload identity assertion is unavailable")
	}
	form := url.Values{
		"client_id":             {k.clientID},
		"scope":                 {"https://vault.azure.net/.default"},
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {tokenAssertionType},
		"client_assertion":      {strings.TrimSpace(string(assertion))},
	}
	endpoint := "https://login.microsoftonline.com/" + k.tenantID + "/oauth2/v2.0/token"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return "", errors.New("Microsoft identity platform is unavailable")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &result) != nil || result.AccessToken == "" {
		return "", fmt.Errorf("Key Vault workload token exchange failed with HTTP %d", resp.StatusCode)
	}
	k.accessToken = result.AccessToken
	k.tokenExpiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	return k.accessToken, nil
}
