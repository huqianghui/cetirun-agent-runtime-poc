// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type cliCredential struct {
	tenant, subscription string
	execute              func(context.Context, []string) ([]byte, error)
}

func (c *cliCredential) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if len(options.Scopes) != 1 || options.Scopes[0] != "https://management.azure.com/.default" ||
		options.Claims != "" || options.TenantID != "" && !strings.EqualFold(options.TenantID, c.tenant) {
		return azcore.AccessToken{}, errors.New("only the explicit tenant's ARM token is permitted; interactive challenges are unsupported")
	}
	// Azure CLI forbids --tenant with --subscription; validate the returned tenant below.
	args := []string{"account", "get-access-token", "--subscription", c.subscription,
		"--resource", "https://management.azure.com/", "--output", "json", "--only-show-errors"}
	execute := c.execute
	if execute == nil {
		execute = executeAzureCLI
	}
	data, err := execute(ctx, args)
	if err != nil {
		return azcore.AccessToken{}, err
	}
	var token struct {
		AccessToken  string `json:"accessToken"`
		ExpiresOn    int64  `json:"expires_on"`
		Subscription string `json:"subscription"`
		Tenant       string `json:"tenant"`
		TokenType    string `json:"tokenType"`
	}
	if len(data) > 64*1024 || json.Unmarshal(data, &token) != nil || token.AccessToken == "" ||
		!strings.EqualFold(token.Subscription, c.subscription) || !strings.EqualFold(token.Tenant, c.tenant) ||
		token.TokenType != "Bearer" || token.ExpiresOn <= time.Now().Unix() ||
		strings.ContainsAny(token.AccessToken, "\r\n") {
		return azcore.AccessToken{}, errors.New("Azure CLI returned an invalid, expired or incorrectly scoped ARM token")
	}
	return azcore.AccessToken{Token: token.AccessToken, ExpiresOn: time.Unix(token.ExpiresOn, 0)}, nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 64*1024 {
		return 0, errors.New("Azure CLI response exceeded the token response limit")
	}
	return b.Buffer.Write(data)
}

func executeAzureCLI(ctx context.Context, args []string) ([]byte, error) {
	command := exec.CommandContext(ctx, "az", args...)
	var output boundedOutput
	command.Stdout = &output
	// CLI diagnostics may contain authentication data; expose only the failure.
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("explicit Azure CLI ARM token request failed; no login or credential fallback was attempted")
	}
	return output.Bytes(), nil
}
