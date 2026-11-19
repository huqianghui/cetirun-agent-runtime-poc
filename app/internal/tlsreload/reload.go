// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package tlsreload reloads mounted TLS material without changing process identity.
package tlsreload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const PollInterval = 5 * time.Second
const maxFileBytes = 1 << 20

type Files struct{ Certificate, Key, CA string }
type snapshot struct {
	certificate tls.Certificate
	roots       *x509.CertPool
	digest      [32]byte
}
type Reloader struct {
	now     func() time.Time
	files   Files
	mu      sync.Mutex
	current atomic.Pointer[snapshot]
}

func New(files Files) (*Reloader, error) {
	r := &Reloader{files: files, now: time.Now}
	if _, err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// readFiles checks both resolved paths and contents twice. In particular, a
// Kubernetes ..data symlink change cannot install a mixed Secret generation.
func readFiles(files Files) ([][]byte, error) {
	paths := []string{files.Certificate, files.Key}
	if files.CA != "" {
		paths = append(paths, files.CA)
	}
	resolved := make([]string, len(paths))
	contents := make([][]byte, len(paths))
	read := func(path string) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, errors.New("TLS material is unavailable")
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		if err != nil || len(b) == 0 || len(b) > maxFileBytes {
			return nil, errors.New("TLS material has invalid size")
		}
		return b, nil
	}
	for i, path := range paths {
		var err error
		resolved[i], err = filepath.EvalSymlinks(path)
		if err != nil {
			return nil, errors.New("TLS material is unavailable")
		}
		contents[i], err = read(resolved[i])
		if err != nil {
			return nil, err
		}
	}
	for i, path := range paths {
		next, err := filepath.EvalSymlinks(path)
		if err != nil || next != resolved[i] {
			return nil, errors.New("TLS material changed while loading")
		}
		again, err := read(next)
		if err != nil || !bytes.Equal(again, contents[i]) {
			return nil, errors.New("TLS material changed while loading")
		}
		// Files projected from one directory must resolve into the same generation.
		for j := 0; j < i; j++ {
			if filepath.Dir(path) == filepath.Dir(paths[j]) && filepath.Dir(resolved[i]) != filepath.Dir(resolved[j]) {
				return nil, errors.New("TLS material spans Secret generations")
			}
		}
	}
	return contents, nil
}

func validCertificate(cert tls.Certificate, now time.Time) error {
	for _, raw := range cert.Certificate {
		parsed, err := x509.ParseCertificate(raw)
		if err != nil || now.Before(parsed.NotBefore) || !now.Before(parsed.NotAfter) {
			return errors.New("TLS certificate is outside its validity period")
		}
	}
	return nil
}

// Reload publishes all material together, retaining the previous snapshot on error.
func (r *Reloader) Reload() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := readFiles(r.files)
	if err != nil {
		return false, err
	}
	hash := sha256.New()
	for _, b := range data {
		hash.Write(b)
		hash.Write([]byte{0})
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	if old := r.current.Load(); old != nil && old.digest == digest {
		return false, validCertificate(old.certificate, r.now())
	}
	cert, err := tls.X509KeyPair(data[0], data[1])
	if err != nil {
		return false, errors.New("TLS certificate and key are invalid")
	}
	if err := validCertificate(cert, r.now()); err != nil {
		return false, err
	}
	roots := x509.NewCertPool()
	if len(data) == 3 {
		rest := data[2]
		count := 0
		for len(bytes.TrimSpace(rest)) > 0 {
			block, tail := pem.Decode(rest)
			if block == nil || block.Type != "CERTIFICATE" {
				return false, errors.New("TLS CA bundle is invalid")
			}
			ca, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !ca.IsCA {
				return false, errors.New("TLS CA bundle contains an invalid CA")
			}
			roots.AddCert(ca)
			count++
			rest = tail
		}
		if count == 0 {
			return false, errors.New("TLS CA bundle is empty")
		}
	}
	r.current.Store(&snapshot{certificate: cert, roots: roots, digest: digest})
	return true, nil
}

func (r *Reloader) Run(ctx context.Context, changed func()) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			updated, err := r.Reload()
			if err != nil {
				slog.Warn("TLS material reload failed", "error", err)
			} else if updated {
				if changed != nil {
					changed()
				}
				slog.Info("TLS material reloaded")
			}
		}
	}
}

// ServerConfig selects a fresh immutable snapshot for each new handshake.
func (r *Reloader) ServerConfig(requireClient bool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		s := r.current.Load()
		if err := validCertificate(s.certificate, r.now()); err != nil {
			return nil, err
		}
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{s.certificate}, SessionTicketsDisabled: true}
		if requireClient {
			if r.files.CA == "" {
				return nil, errors.New("TLS client CA is required")
			}
			cfg.ClientAuth, cfg.ClientCAs = tls.RequireAndVerifyClientCert, s.roots
		}
		return cfg, nil
	}}
}

// Transport replaces the connection pool when either identity or trust changes.
// Existing in-flight requests finish normally; no requests are replayed here.
type Transport struct {
	source    *Reloader
	mu        sync.Mutex
	snapshot  *snapshot
	transport *http.Transport
}

func (r *Reloader) ClientTransport() *Transport { return &Transport{source: r} }
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	s := t.source.current.Load()
	if err := validCertificate(s.certificate, t.source.now()); err != nil {
		t.mu.Unlock()
		return nil, err
	}
	if t.source.files.CA == "" {
		t.mu.Unlock()
		return nil, errors.New("TLS server CA is required")
	}
	if t.snapshot != s {
		if t.transport != nil {
			t.transport.CloseIdleConnections()
		}
		t.transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: s.roots, Certificates: []tls.Certificate{s.certificate}}, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 60 * time.Second}
		t.snapshot = s
	}
	transport := t.transport
	t.mu.Unlock()
	return transport.RoundTrip(req)
}
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.transport != nil {
		t.transport.CloseIdleConnections()
	}
}
