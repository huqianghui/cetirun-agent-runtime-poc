// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package aigateway

import (
	"context"
	"net/http"
)

// UpdateKeyAliases replaces the model aliases of the key identified by token
// and leaves every other setting unchanged. An empty map removes them all.
// LiteLLM checks a request for an alias against the key's models by the
// alias's target, and serves it as the target.
func (c *Client) UpdateKeyAliases(ctx context.Context, token string, aliases map[string]string) error {
	if aliases == nil {
		aliases = map[string]string{}
	}
	_, err := c.do(ctx, http.MethodPost, "/key/update", map[string]any{"key": token, "aliases": aliases}, nil)
	return err
}

// keyAliases reads a key's aliases from /key/info, keeping the string-valued
// entries. LiteLLM returns an object, or null for a key without any.
func keyAliases(raw any) map[string]string {
	entries, ok := raw.(map[string]any)
	if !ok || len(entries) == 0 {
		return nil
	}
	aliases := make(map[string]string, len(entries))
	for name, target := range entries {
		if value, ok := target.(string); ok {
			aliases[name] = value
		}
	}
	if len(aliases) == 0 {
		return nil
	}
	return aliases
}
