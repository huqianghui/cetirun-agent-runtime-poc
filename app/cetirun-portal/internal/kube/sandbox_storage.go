// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"

	"github.com/Azure/CetiRun/app/cetirun-operator/sandboxcatalog"
)

// GetSandboxStorageOptions reads approved choices through the same strict
// platform configuration parser as the Operator. c.client is a direct client.
func (c *Client) GetSandboxStorageOptions(ctx context.Context, namespace, name string) (sandboxcatalog.Options, error) {
	reader := sandboxcatalog.Reader{APIReader: c.client, Namespace: namespace, Name: name}
	return reader.StorageOptions(ctx)
}
