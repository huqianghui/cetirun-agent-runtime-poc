// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type authSDKClient struct {
	baseURL string
	client  *http.Client
}

type authSDKValidationFailure string

const (
	authSDKValidationUnavailable       authSDKValidationFailure = "auth_sdk_unavailable"
	authSDKValidationInvalidResponse   authSDKValidationFailure = "auth_sdk_invalid_response"
	authSDKValidationRejected          authSDKValidationFailure = "auth_sdk_rejected"
	authSDKValidationRedirected        authSDKValidationFailure = "auth_sdk_redirected"
	authSDKValidationBadRequest        authSDKValidationFailure = "auth_sdk_bad_request"
	authSDKValidationNotFound          authSDKValidationFailure = "auth_sdk_not_found"
	authSDKValidationMethod            authSDKValidationFailure = "auth_sdk_method_not_allowed"
	authSDKValidationConflict          authSDKValidationFailure = "auth_sdk_conflict"
	authSDKValidationRateLimited       authSDKValidationFailure = "auth_sdk_rate_limited"
	authSDKValidationServerError       authSDKValidationFailure = "auth_sdk_server_error"
	authSDKValidationRejectedMalformed authSDKValidationFailure = "auth_sdk_rejected_malformed"
	authSDKValidationRejectedTenant    authSDKValidationFailure = "auth_sdk_rejected_tenant"
	authSDKValidationRejectedOwner     authSDKValidationFailure = "auth_sdk_rejected_owner"
	authSDKValidationRejectedAudience  authSDKValidationFailure = "auth_sdk_rejected_audience"
	authSDKValidationRejectedScope     authSDKValidationFailure = "auth_sdk_rejected_scope"
	authSDKValidationRejectedParty     authSDKValidationFailure = "auth_sdk_rejected_authorized_party"
	authSDKValidationRejectedApp       authSDKValidationFailure = "auth_sdk_rejected_application_token"
	authSDKValidationRejectedClaimsOK  authSDKValidationFailure = "auth_sdk_rejected_matching_claims"
	authSDKValidationClaimsMissing     authSDKValidationFailure = "auth_sdk_claims_missing"
	authSDKValidationTenant            authSDKValidationFailure = "auth_sdk_tenant"
	authSDKValidationOwner             authSDKValidationFailure = "auth_sdk_owner"
	authSDKValidationAudience          authSDKValidationFailure = "auth_sdk_audience"
	authSDKValidationScope             authSDKValidationFailure = "auth_sdk_scope"
	authSDKValidationParty             authSDKValidationFailure = "auth_sdk_authorized_party"
	authSDKValidationApp               authSDKValidationFailure = "auth_sdk_application_token"
)

type authSDKValidationError struct {
	failure authSDKValidationFailure
}

func (e *authSDKValidationError) Error() string { return "user assertion validation failed" }

func newAuthSDKValidationError(failure authSDKValidationFailure) error {
	return &authSDKValidationError{failure: failure}
}

func authSDKValidationFailureCategory(err error) (string, bool) {
	var validationError *authSDKValidationError
	if !errors.As(err, &validationError) {
		return "", false
	}
	return string(validationError.failure), true
}

func newAuthSDKClient(cfg Config) *authSDKClient {
	return &authSDKClient{baseURL: cfg.AuthSDKURL, client: &http.Client{Timeout: cfg.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *authSDKClient) ready(ctx context.Context) error {
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return errors.New("Auth SDK endpoint is invalid")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return errors.New("Auth SDK is unavailable")
	}
	return connection.Close()
}

func (c *authSDKClient) validate(ctx context.Context, userToken, tenantID, audience, authorizationClientID, ownerObjectID string) error {
	response := map[string]any{}
	status, err := c.get(ctx, "/Validate", userToken, nil, &response)
	if err != nil {
		if status == 0 {
			return newAuthSDKValidationError(authSDKValidationUnavailable)
		}
		return newAuthSDKValidationError(authSDKValidationInvalidResponse)
	}
	if status != http.StatusOK {
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			return newAuthSDKValidationError(authSDKStatusCategory(status))
		}
		return newAuthSDKValidationError(rejectedAssertionCategory(userToken, tenantID, audience, authorizationClientID, ownerObjectID))
	}
	claims := response
	if rawClaims, present := response["claims"]; present {
		nested, ok := rawClaims.(map[string]any)
		if !ok {
			return newAuthSDKValidationError(authSDKValidationClaimsMissing)
		}
		claims = nested
	}
	if len(claims) == 0 {
		return newAuthSDKValidationError(authSDKValidationClaimsMissing)
	}
	if !strings.EqualFold(stringClaim(claims, "tid"), tenantID) {
		return newAuthSDKValidationError(authSDKValidationTenant)
	}
	if !strings.EqualFold(stringClaim(claims, "oid"), ownerObjectID) {
		return newAuthSDKValidationError(authSDKValidationOwner)
	}
	if !claimContains(claims["aud"], audience) {
		return newAuthSDKValidationError(authSDKValidationAudience)
	}
	if !scopeContains(stringClaim(claims, "scp"), "access_agent") {
		return newAuthSDKValidationError(authSDKValidationScope)
	}
	if azp := stringClaim(claims, "azp"); !strings.EqualFold(azp, authorizationClientID) {
		return newAuthSDKValidationError(authSDKValidationParty)
	}
	if idtyp := stringClaim(claims, "idtyp"); strings.EqualFold(idtyp, "app") {
		return newAuthSDKValidationError(authSDKValidationApp)
	}
	return nil
}

func authSDKStatusCategory(status int) authSDKValidationFailure {
	switch status {
	case http.StatusBadRequest:
		return authSDKValidationBadRequest
	case http.StatusNotFound:
		return authSDKValidationNotFound
	case http.StatusMethodNotAllowed:
		return authSDKValidationMethod
	case http.StatusConflict:
		return authSDKValidationConflict
	case http.StatusTooManyRequests:
		return authSDKValidationRateLimited
	default:
		if status >= 300 && status < 400 {
			return authSDKValidationRedirected
		}
		if status >= 500 && status < 600 {
			return authSDKValidationServerError
		}
		return authSDKValidationRejected
	}
}

func rejectedAssertionCategory(userToken, tenantID, audience, authorizationClientID, ownerObjectID string) authSDKValidationFailure {
	parts := strings.Split(userToken, ".")
	if len(parts) != 3 {
		return authSDKValidationRejectedMalformed
	}
	claims := map[string]any{}
	if decodePart(parts[1], &claims) != nil {
		return authSDKValidationRejectedMalformed
	}
	if !strings.EqualFold(stringClaim(claims, "tid"), tenantID) {
		return authSDKValidationRejectedTenant
	}
	if !strings.EqualFold(stringClaim(claims, "oid"), ownerObjectID) {
		return authSDKValidationRejectedOwner
	}
	if !claimContains(claims["aud"], audience) {
		return authSDKValidationRejectedAudience
	}
	if !scopeContains(stringClaim(claims, "scp"), "access_agent") {
		return authSDKValidationRejectedScope
	}
	if !strings.EqualFold(stringClaim(claims, "azp"), authorizationClientID) {
		return authSDKValidationRejectedParty
	}
	if strings.EqualFold(stringClaim(claims, "idtyp"), "app") {
		return authSDKValidationRejectedApp
	}
	return authSDKValidationRejectedClaimsOK
}

func (c *authSDKClient) authorizationHeaderFor(ctx context.Context, userToken, agentIdentityClientID, downstream string) (string, error) {
	if downstream != "AzureResourceManager" && downstream != "MicrosoftGraph" {
		return "", errors.New("unsupported Auth SDK downstream service")
	}
	query := url.Values{"AgentIdentity": {agentIdentityClientID}}
	var response struct {
		AuthorizationHeader string `json:"authorizationHeader"`
	}
	status, err := c.get(ctx, "/AuthorizationHeader/"+downstream+"?"+query.Encode(), userToken, nil, &response)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", errDownstreamForbidden
	}
	if status != http.StatusOK || !strings.HasPrefix(response.AuthorizationHeader, "Bearer ") {
		return "", errors.New("Auth SDK token exchange failed")
	}
	return response.AuthorizationHeader, nil
}

func (c *authSDKClient) get(ctx context.Context, endpoint, userToken string, query url.Values, target any) (int, error) {
	u := c.baseURL + endpoint
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	// Keep the TCP destination pinned to 127.0.0.1 while satisfying the Auth
	// SDK's independent local Host header restriction. Some sidecar builds
	// reject an IP-literal Host even though they accept the loopback peer.
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, errors.New("Auth SDK is unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return resp.StatusCode, errors.New("Auth SDK returned an invalid response")
	}
	if target != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && len(body) > 0 && json.Unmarshal(body, target) != nil {
		return resp.StatusCode, errors.New("Auth SDK returned invalid JSON")
	}
	return resp.StatusCode, nil
}

func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return value
}

func claimContains(value any, expected string) bool {
	switch item := value.(type) {
	case string:
		return item == expected
	case []any:
		for _, entry := range item {
			if entry == expected {
				return true
			}
		}
	}
	return false
}

func scopeContains(scopes, expected string) bool {
	for _, scope := range strings.Fields(scopes) {
		if scope == expected {
			return true
		}
	}
	return false
}

var errDownstreamForbidden = fmt.Errorf("downstream request is forbidden")
