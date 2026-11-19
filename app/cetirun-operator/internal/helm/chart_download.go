// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"
	"regexp"
)

// ChartDownloadError reports that a chart could not be downloaded from its
// registry. Registries redirect downloads to their storage, and the cause of
// a failed download quotes the storage URL; the message keeps the cause
// without the query strings of the URLs in it.
type ChartDownloadError struct {
	// Chart is the chart reference, repository and version.
	Chart string
	// Version is the chart version.
	Version string
	// Err is the cause, as the registry client returned it.
	Err error
}

func (e *ChartDownloadError) Error() string {
	return fmt.Sprintf("locate OCI chart %s: %s", e.Chart, withoutURLQueries(e.Err.Error()))
}

func (e *ChartDownloadError) Unwrap() error { return e.Err }

// urlQuery matches an HTTP or HTTPS URL that has a query string or fragment,
// up to the next space or quote.
var urlQuery = regexp.MustCompile(`(https?://[^\s"'?#]*)[?#][^\s"']*`)

// withoutURLQueries removes the query strings and fragments of the URLs in s.
func withoutURLQueries(s string) string {
	return urlQuery.ReplaceAllString(s, "$1")
}
