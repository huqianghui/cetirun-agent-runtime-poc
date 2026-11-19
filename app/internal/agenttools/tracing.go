// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools

import "context"

type correlationKey struct{}

func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}
func Correlation(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// AuditText bounds structured log values and removes control characters.
func AuditText(value string, limit int) string {
	result := make([]rune, 0, len(value))
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			r = '\uFFFD'
		}
		if len(result) == limit {
			break
		}
		result = append(result, r)
	}
	return string(result)
}
