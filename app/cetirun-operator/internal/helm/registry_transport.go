// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"sync/atomic"
)

// contextTransport sends every request with ctx, so a request ends when ctx
// is done. Helm's registry client builds its requests with a background
// context; this binds them to the caller's deadline and cancellation. It sits
// outside the retry transport, whose waits between attempts follow the
// request's context too.
type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(t.ctx))
}

// plainHTTPFallback retries a request over plain HTTP when an HTTPS request
// reaches a registry that answers plain HTTP, and keeps using HTTP afterwards.
// Helm v3's registry client does the same with its default transport, which a
// client given its own HTTP client does not get; this keeps such a client
// reaching the registries the release path reaches.
type plainHTTPFallback struct {
	base      http.RoundTripper
	forceHTTP atomic.Bool
}

func (t *plainHTTPFallback) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.forceHTTP.Load() {
		req.URL.Scheme = "http"
		return t.base.RoundTrip(req)
	}
	resp, err := t.base.RoundTrip(req)
	var header tls.RecordHeaderError
	if err != nil && req.URL.Scheme == "https" && errors.As(err, &header) && string(header.RecordHeader[:]) == "HTTP/" {
		t.forceHTTP.Store(true)
		req.URL.Scheme = "http"
		return t.base.RoundTrip(req)
	}
	return resp, err
}
