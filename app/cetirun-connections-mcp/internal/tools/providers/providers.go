// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package providers executes JSON REST requests using platform-owned profiles.
package providers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const MaxBodyBytes = 256 << 10
const MaxResponseBytes = 256 << 10

type Request struct {
	ConnectionID string            `json:"connection_id"`
	Service      string            `json:"service"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Query        map[string]any    `json:"query,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         json.RawMessage   `json:"body,omitempty"`
}
type Result struct {
	Status        int               `json:"status"`
	Body          any               `json:"body"`
	Headers       map[string]string `json:"headers"`
	RequestIDs    []string          `json:"request_ids"`
	CorrelationID string            `json:"correlation_id"`
}
type Error struct {
	Code    string
	Unknown bool
}

func (e *Error) Error() string { return e.Code }

type Client struct{ http *http.Client }

func New(transport http.RoundTripper) *Client {
	if transport == nil {
		// A fresh HTTP/1 connection per call prevents net/http's replay on a reused
		// connection or HTTP/2 REFUSED_STREAM. No proxy, redirects or retry loop.
		transport = &http.Transport{
			DialContext: publicDial, DisableKeepAlives: true, DisableCompression: true,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
			MaxResponseHeaderBytes: 32 << 10,
		}
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, raw := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, errors.New("invalid service address")
	}
	allowed := false
	for _, s := range agenttools.Services() {
		u, _ := url.Parse(s.Origin)
		if host == u.Hostname() {
			allowed = true
		}
	}
	if !allowed {
		return nil, errors.New("invalid service host")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("service DNS unavailable")
	}
	// Validate the whole answer and dial the checked IP, avoiding a second lookup.
	for _, ip := range addresses {
		if !publicIP(ip) {
			return nil, errors.New("service DNS address rejected")
		}
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
}

func Decode(arguments map[string]any) (Request, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil || len(encoded) > MaxBodyBytes+32768 {
		return Request{}, &Error{Code: "invalid_request"}
	}
	var input Request
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		return Request{}, &Error{Code: "invalid_request"}
	}
	_, err = Target(input)
	return input, err
}

func Target(input Request) (*url.URL, error) {
	invalid := func() (*url.URL, error) { return nil, &Error{Code: "invalid_request"} }
	profile, ok := agenttools.LookupService(input.Service)
	if !ok || len(input.ConnectionID) == 0 || len(input.ConnectionID) > 128 {
		return invalid()
	}
	switch input.Method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return invalid()
	}
	p := input.Path
	if len(p) == 0 || len(p) > 8192 || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || !utf8.ValidString(p) || strings.ContainsAny(p, "?#\\\r\n\x00") {
		return invalid()
	}
	// Decode once and reject encoded separators and nested escapes; neither a
	// gateway nor provider can reinterpret the path as traversal or an origin.
	decoded, err := url.PathUnescape(p)
	if err != nil || !utf8.ValidString(decoded) || strings.ContainsAny(decoded, "%\\?#\r\n\x00") || strings.Contains(decoded, "//") {
		return invalid()
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return invalid()
		}
	}
	if strings.Count(p, "/") != strings.Count(decoded, "/") {
		return invalid()
	}
	for _, r := range decoded {
		if r < 0x20 || r == 0x7f {
			return invalid()
		}
	}
	base, err := url.Parse(profile.Origin)
	if err != nil {
		return invalid()
	}
	base.Path = strings.TrimSuffix(profile.BasePath, "/") + decoded
	if path.Clean(base.Path) != strings.TrimSuffix(base.Path, "/") && base.Path != "/" {
		return invalid()
	}
	if !strings.HasPrefix(base.Path, profile.BasePath) {
		return invalid()
	}
	query := url.Values{}
	if len(input.Query) > 100 {
		return invalid()
	}
	for key, value := range input.Query {
		if len(key) == 0 || len(key) > 256 || strings.ContainsAny(key, "\r\n\x00") {
			return invalid()
		}
		switch strings.ToLower(key) {
		case "access_token", "oauth_token", "authorization", "api_key", "key":
			return invalid()
		}
		add := func(v string) bool {
			if len(v) > 8192 || strings.ContainsAny(v, "\r\n\x00") {
				return false
			}
			query.Add(key, v)
			return true
		}
		switch values := value.(type) {
		case string:
			if !add(values) {
				return invalid()
			}
		case []any:
			if len(values) > 100 {
				return invalid()
			}
			for _, item := range values {
				v, ok := item.(string)
				if !ok || !add(v) {
					return invalid()
				}
			}
		case []string:
			if len(values) > 100 {
				return invalid()
			}
			for _, v := range values {
				if !add(v) {
					return invalid()
				}
			}
		default:
			return invalid()
		}
	}
	base.RawQuery = query.Encode()
	if len(base.RawQuery) > 16384 {
		return invalid()
	}
	seen := map[string]bool{}
	for name, value := range input.Headers {
		lower := strings.ToLower(name)
		if seen[lower] || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
			return invalid()
		}
		seen[lower] = true
		switch lower {
		case "if-match", "if-none-match", "prefer", "consistencylevel", "x-github-api-version":
		default:
			return invalid()
		}
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return invalid()
			}
		}
	}
	if len(input.Body) > MaxBodyBytes || (len(input.Body) > 0 && !json.Valid(input.Body)) {
		return invalid()
	}
	if (input.Method == "GET" || input.Method == "HEAD") && len(input.Body) > 0 {
		return invalid()
	}
	// Validate the final origin, not only the input path.
	if base.Scheme != "https" || base.User != nil || base.Host != strings.TrimPrefix(profile.Origin, "https://") || base.Fragment != "" {
		return invalid()
	}
	return base, nil
}

func (c *Client) Execute(ctx context.Context, input Request, token, correlation string) (Result, error) {
	target, err := Target(input)
	if err != nil {
		return Result{}, err
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return Result{}, &Error{Code: "dependency_unavailable"}
	}
	request, err := http.NewRequestWithContext(ctx, input.Method, target.String(), io.NopCloser(bytes.NewReader(input.Body)))
	if err != nil {
		return Result{}, &Error{Code: "invalid_request"}
	}
	request.GetBody = nil // No implicit replay, including requests with JSON bodies.
	if len(input.Body) == 0 {
		request.Body = nil
	} else {
		request.ContentLength = int64(len(input.Body))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CetiRun-Agent-Services/1")
	for name, value := range input.Headers {
		request.Header.Set(name, value)
	}
	// A platform tracing header is sent for every service. Providers may ignore
	// it; their response request IDs remain the external lookup reference.
	request.Header.Set("X-CetiRun-Correlation-ID", correlation)
	profile, _ := agenttools.LookupService(input.Service)
	if profile.Provider == "azure" {
		request.Header.Set("x-ms-client-request-id", correlation)
	}
	if profile.Provider == "microsoft" {
		request.Header.Set("client-request-id", correlation)
		request.Header.Set("return-client-request-id", "true")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return Result{}, &Error{Code: "execution_unknown", Unknown: true}
	}
	defer response.Body.Close()
	result := Result{Status: response.StatusCode, Headers: map[string]string{}, RequestIDs: []string{}, CorrelationID: correlation}
	for _, name := range []string{"x-ms-request-id", "x-ms-correlation-request-id", "request-id", "x-github-request-id", "x-guploader-uploadid"} {
		if value := response.Header.Get(name); safeResponseHeader(value, token) && len(value) < 1024 {
			result.RequestIDs = append(result.RequestIDs, value)
		}
	}
	for _, name := range []string{"etag", "retry-after", "location", "azure-asyncoperation", "operation-location", "link", "x-ratelimit-remaining", "x-ratelimit-reset"} {
		if value := response.Header.Get(name); safeResponseHeader(value, token) {
			result.Headers[name] = value
		}
	}
	// 304 is a cache result; all redirect responses are returned without following.
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.StatusCode != 304 {
		return result, &Error{Code: "redirect_rejected"}
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(payload) > MaxResponseBytes {
		return result, &Error{Code: "execution_unknown", Unknown: true}
	}
	if len(payload) > 0 {
		contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || !(contentType == "application/json" || strings.HasSuffix(contentType, "+json")) {
			// The provider may have applied a write before returning an unsupported
			// representation. Do not report this as a request rejected before execution.
			return result, &Error{Code: "execution_unknown", Unknown: true}
		}
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.UseNumber()
		if decoder.Decode(&result.Body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			result.Body = nil
			return result, &Error{Code: "execution_unknown", Unknown: true}
		}
	}
	// Do not let an upstream echo disclose the bearer credential, including escaped
	// JSON strings or header values. Resource data otherwise remains intact.
	if containsCredential(result.Body, token) || containsCredential(result.Headers, token) {
		result.Body = nil
		return result, &Error{Code: "execution_unknown", Unknown: true}
	}
	return result, nil
}
func containsCredential(value any, token string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, token)
	case map[string]any:
		for k, x := range v {
			if strings.Contains(k, token) || containsCredential(x, token) {
				return true
			}
		}
	case map[string]string:
		for k, x := range v {
			if strings.Contains(k, token) || strings.Contains(x, token) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if containsCredential(x, token) {
				return true
			}
		}
	}
	return false
}

func safeResponseHeader(value, token string) bool {
	if value == "" || len(value) > 8192 || strings.Contains(value, token) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
