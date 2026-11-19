// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package observability holds the rules that the platform's log and metrics
// collection sets for the workloads it collects from.
package observability

import "strings"

// CollectorAnnotationPrefix is the prefix of the Pod annotations that ADX-Mon
// reads: they choose the database and table of a Pod's logs, and turn on
// metrics scraping of the Pod. Only the platform sets them; an Agent's values
// cannot.
const CollectorAnnotationPrefix = "adx-mon/"

// CollectorAnnotation reports whether a Pod annotation is reserved for the
// platform's log and metrics collection.
func CollectorAnnotation(key string) bool {
	return strings.HasPrefix(key, CollectorAnnotationPrefix)
}
