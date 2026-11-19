// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package brokerclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const (
	testCallerProof = "caller-proof-must-not-leak"
	testResponseKey = "broker-response-must-not-leak"
)

func TestNewMutualTLSLoadsFilesAndRequiresClientCertificate(t *testing.T) {
	fixture := newTLSFixture(t)
	var handled atomic.Int32
	server := newMutualTLSServer(t, fixture, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handled.Add(1)
		if request.Header.Get("X-Correlation-ID") != "mcp-correlation" {
			t.Error("MCP correlation did not reach Broker")
		}
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			t.Error("Broker accepted a request without a client certificate")
		}
		if request.URL.Path != "/internal/v1/service-authorizations/list" {
			t.Errorf("unexpected Broker path %q", request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		var input Request
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.CallerProof != testCallerProof {
			t.Error("unexpected Broker request")
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(ListResponse{InstanceUID: "instance-uid", ContractVersion: ContractVersion})
	}))
	defer server.Close()

	baseURL := brokerURL(t, server.URL)
	client, err := NewMutualTLS(baseURL, fixture.caFile, fixture.clientCertFile, fixture.clientKeyFile)
	if err != nil {
		t.Fatal("NewMutualTLS rejected generated certificate files")
	}
	response, err := client.List(agenttools.WithCorrelation(context.Background(), "mcp-correlation"), testCallerProof)
	if err != nil || response.InstanceUID != "instance-uid" {
		t.Fatal("mutual TLS Broker request failed")
	}
	if handled.Load() != 1 {
		t.Fatal("Broker did not receive the authenticated request")
	}

	t.Run("missing client certificate is rejected without its path", func(t *testing.T) {
		missingCertificate := filepath.Join(t.TempDir(), "missing-client.pem")
		_, err := NewMutualTLS(baseURL, fixture.caFile, missingCertificate, fixture.clientKeyFile)
		assertSanitizedError(t, err, missingCertificate, fixture.clientKeyFile)
		if err == nil || err.Error() != "Broker client identity is unavailable" {
			t.Fatal("missing client certificate was not rejected")
		}
	})

	t.Run("server rejects a client without a certificate", func(t *testing.T) {
		unauthenticated, err := New(baseURL)
		if err != nil {
			t.Fatal("New rejected HTTPS origin")
		}
		roots := x509.NewCertPool()
		roots.AddCert(fixture.ca.certificate)
		unauthenticated.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}
		_, err = unauthenticated.List(context.Background(), testCallerProof)
		assertErrorCode(t, err, "dependency_unavailable", testCallerProof)
		if handled.Load() != 1 {
			t.Fatal("Broker accepted a client without a certificate")
		}
	})

	t.Run("untrusted Broker certificate is rejected", func(t *testing.T) {
		untrusted, err := NewMutualTLS(baseURL, fixture.untrustedCAFile, fixture.clientCertFile, fixture.clientKeyFile)
		if err != nil {
			t.Fatal("NewMutualTLS rejected a syntactically valid CA")
		}
		_, err = untrusted.List(context.Background(), testCallerProof)
		assertErrorCode(t, err, "dependency_unavailable", testCallerProof, testResponseKey)
		if handled.Load() != 1 {
			t.Fatal("Broker received a request from a client that rejected its certificate")
		}
	})
}

func TestClientBoundsResponsesAndSanitizesRedirectFailures(t *testing.T) {
	fixture := newTLSFixture(t)
	var redirectTargetHits atomic.Int32
	server := newMutualTLSServer(t, fixture, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/internal/v1/service-authorizations/check":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"response":"` + testResponseKey + strings.Repeat("x", 1<<20) + `"}`))
		case "/internal/v1/service-credentials/issue":
			writer.Header().Set("Location", "/redirect-target?credential="+testResponseKey)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusFound)
			_, _ = writer.Write([]byte(`{"error":{"code":"unexpected_redirect","message":"` + testResponseKey + `"}}`))
		case "/redirect-target":
			redirectTargetHits.Add(1)
			http.Error(writer, "redirect target must not be requested", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewMutualTLS(brokerURL(t, server.URL), fixture.caFile, fixture.clientCertFile, fixture.clientKeyFile)
	if err != nil {
		t.Fatal("NewMutualTLS rejected generated certificate files")
	}

	_, err = client.Check(context.Background(), NewRequest(testCallerProof, "tool", "connection", nil, nil))
	assertErrorCode(t, err, "dependency_unavailable", testCallerProof, testResponseKey)

	_, err = client.Issue(context.Background(), NewRequest(testCallerProof, "tool", "connection", nil, nil))
	assertErrorCode(t, err, "dependency_unavailable", testCallerProof, testResponseKey)
	if redirectTargetHits.Load() != 0 {
		t.Fatal("Broker client followed a redirect")
	}
}

type tlsFixture struct {
	ca              certificateAuthority
	caFile          string
	untrustedCAFile string
	clientCertFile  string
	clientKeyFile   string
	server          tls.Certificate
}

type certificateAuthority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

type leafCertificate struct {
	tlsCertificate tls.Certificate
	certificatePEM []byte
	keyPEM         []byte
}

func newTLSFixture(t *testing.T) tlsFixture {
	t.Helper()
	directory := t.TempDir()
	ca := newCertificateAuthority(t, "test Broker CA")
	server := newLeafCertificate(t, ca, "test Broker", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	client := newLeafCertificate(t, ca, "test client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, nil)
	untrustedCA := newCertificateAuthority(t, "test untrusted CA")
	caFile := writeTestFile(t, directory, "ca.pem", ca.pem)
	return tlsFixture{
		ca:              ca,
		caFile:          caFile,
		untrustedCAFile: writeTestFile(t, directory, "untrusted-ca.pem", untrustedCA.pem),
		clientCertFile:  writeTestFile(t, directory, "client.pem", client.certificatePEM),
		clientKeyFile:   writeTestFile(t, directory, "client-key.pem", client.keyPEM),
		server:          server.tlsCertificate,
	}
}

func newCertificateAuthority(t *testing.T, commonName string) certificateAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("generate CA key")
	}
	template := &x509.Certificate{
		SerialNumber:          certificateSerial(t),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal("create CA certificate")
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal("parse CA certificate")
	}
	return certificateAuthority{certificate: certificate, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func newLeafCertificate(t *testing.T, ca certificateAuthority, commonName string, usages []x509.ExtKeyUsage, dnsNames []string, ipAddresses []net.IP) leafCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("generate leaf key")
	}
	template := &x509.Certificate{
		SerialNumber:          certificateSerial(t),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
		ExtKeyUsage:           usages,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal("create leaf certificate")
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal("marshal leaf key")
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tlsCertificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal("load generated leaf certificate")
	}
	return leafCertificate{tlsCertificate: tlsCertificate, certificatePEM: certificatePEM, keyPEM: keyPEM}
}

func certificateSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal("generate certificate serial")
	}
	return serial
}

func writeTestFile(t *testing.T, directory, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal("write generated certificate file")
	}
	return path
}

func newMutualTLSServer(t *testing.T, fixture tlsFixture, handler http.Handler) *httptest.Server {
	t.Helper()
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(fixture.ca.certificate)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{fixture.server},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
	server.StartTLS()
	return server
}

func brokerURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal("parse test Broker URL")
	}
	return baseURL
}

func assertErrorCode(t *testing.T, err error, want string, sensitiveValues ...string) {
	t.Helper()
	assertSanitizedError(t, err, sensitiveValues...)
	var brokerError *Error
	if !errors.As(err, &brokerError) || brokerError.Code != want {
		t.Fatal("unexpected sanitized Broker error code")
	}
}

func assertSanitizedError(t *testing.T, err error, sensitiveValues ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected Broker request to fail")
	}
	for _, value := range sensitiveValues {
		if strings.Contains(err.Error(), value) {
			t.Fatal("Broker error exposed sensitive data")
		}
	}
}
