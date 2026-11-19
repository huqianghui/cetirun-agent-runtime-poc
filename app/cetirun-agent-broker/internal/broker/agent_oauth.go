// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// agentToken implements the Entra autonomous Agent OAuth flow. The exchange
// assertion is scoped to a single child identity and never persisted or exposed.
// Delegated credentials and the encryption identity are not used in this flow.
func (c *oauthClient) agentToken(ctx context.Context, agentClientID string) (oauthTokens, error) {
	if !uuidPattern.MatchString(agentClientID) || !uuidPattern.MatchString(c.cfg.BlueprintClientID) || !uuidPattern.MatchString(c.cfg.TenantID) || strings.EqualFold(agentClientID, c.cfg.BlueprintClientID) {
		return oauthTokens{}, newOAuthTokenError(oauthTokenInvalidRequest)
	}
	assertion, err := os.ReadFile(c.cfg.FederatedTokenFile)
	if err != nil || len(assertion) == 0 || len(assertion) > 64<<10 || strings.TrimSpace(string(assertion)) == "" {
		return oauthTokens{}, newOAuthTokenError(oauthTokenAssertionUnavailable)
	}
	exchange, err := c.agentTokenRequest(ctx, url.Values{
		"client_id": {c.cfg.BlueprintClientID}, "grant_type": {"client_credentials"},
		"scope": {"api://AzureADTokenExchange/.default"}, "fmi_path": {agentClientID},
		"client_assertion_type": {tokenAssertionType}, "client_assertion": {strings.TrimSpace(string(assertion))},
	})
	if err != nil {
		return oauthTokens{}, err
	}
	return c.agentTokenRequest(ctx, url.Values{
		"client_id": {agentClientID}, "grant_type": {"client_credentials"},
		"scope":                 {"https://management.azure.com/.default"},
		"client_assertion_type": {tokenAssertionType}, "client_assertion": {exchange.AccessToken},
	})
}

func (c *oauthClient) agentTokenRequest(ctx context.Context, form url.Values) (oauthTokens, error) {
	endpoint := "https://login.microsoftonline.com/" + c.cfg.TenantID + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokens{}, newOAuthTokenError(oauthTokenInvalidRequest)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Copy the client so injected test transports work without mutating shared
	// redirect policy or allowing assertions to be forwarded to another origin.
	client := *c.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return oauthTokens{}, newOAuthTokenError(oauthTokenPlatformUnavailable)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(body) > 256<<10 {
		return oauthTokens{}, newOAuthTokenError(oauthTokenInvalidResponse)
	}
	if resp.StatusCode != http.StatusOK {
		return oauthTokens{}, newOAuthTokenError(oauthTokenEndpointRejected)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &result) != nil || !strings.EqualFold(result.TokenType, "Bearer") || result.AccessToken == "" || len(result.AccessToken) > 32<<10 || strings.ContainsAny(result.AccessToken, " \t\r\n") || result.ExpiresIn <= 30 || result.ExpiresIn > 86400 {
		return oauthTokens{}, newOAuthTokenError(oauthTokenInvalidResponse)
	}
	return oauthTokens{AccessToken: result.AccessToken, ExpiresIn: result.ExpiresIn}, nil
}
