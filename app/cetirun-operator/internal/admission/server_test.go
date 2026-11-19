// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type servingCertificate struct {
	ca          []byte
	certificate []byte
	key         []byte
	serial      *big.Int
}

func newServingCertificate(t *testing.T, serial int64) servingCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "CetiRun local test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	ca, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(serial + 100), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return servingCertificate{
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}), leaf.SerialNumber,
	}
}

func (certificate servingCertificate) write(t *testing.T, directory string) {
	t.Helper()
	for name, content := range map[string][]byte{"tls.crt": certificate.certificate, "tls.key": certificate.key} {
		temporary := filepath.Join(directory, name+".next")
		if err := os.WriteFile(temporary, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServingCertificateRequiresValidPlatformMaterial(t *testing.T) {
	directory := t.TempDir()
	cache := &servingCertificateCache{directory: directory}
	now := time.Now()
	if _, err := cache.load(now); err == nil {
		t.Fatal("missing certificates must not cause an implicit self-signed fallback")
	}
	certificate := newServingCertificate(t, 1)
	certificate.write(t, directory)
	first, err := cache.load(now)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := cache.load(now)
	if err != nil || reused != first {
		t.Fatalf("unchanged material was not reused: %v", err)
	}
	var expiry dto.Metric
	if err := servingCertificateExpiry.Write(&expiry); err != nil {
		t.Fatal(err)
	}
	if expiry.GetGauge().GetValue() != float64(first.Leaf.NotAfter.Unix()) {
		t.Fatal("serving expiry metric did not reflect the loaded certificate")
	}
	for _, invalidTime := range []time.Time{now.Add(-2 * time.Hour), now.Add(2 * time.Hour)} {
		if _, err := cache.load(invalidTime); err == nil {
			t.Fatal("not-yet-valid/expired serving material was accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "tls.key"), []byte("not a private key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.load(now); err == nil {
		t.Fatal("invalid serving material was accepted")
	}
	if err := servingCertificateExpiry.Write(&expiry); err != nil {
		t.Fatal(err)
	}
	if expiry.GetGauge().GetValue() != 0 {
		t.Fatal("invalid serving material must not leave a healthy expiry metric")
	}
	certificate.write(t, directory)
	if _, err := cache.load(now); err != nil {
		t.Fatal("repaired certificate was not accepted:", err)
	}
}

func TestCertificateCacheDetectsContentReplacementWithUnchangedTimestamps(t *testing.T) {
	directory := t.TempDir()
	first, second := newServingCertificate(t, 1), newServingCertificate(t, 2)
	first.write(t, directory)
	cache := &servingCertificateCache{directory: directory}
	before, err := cache.load(time.Now())
	if err != nil {
		t.Fatal(err)
	}

	timestamp := time.Now().Add(-time.Hour)
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Chtimes(filepath.Join(directory, name), timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
	second.write(t, directory)
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Chtimes(filepath.Join(directory, name), timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
	after, err := cache.load(time.Now())
	if err != nil || before == after || after.Leaf.SerialNumber.Cmp(second.serial) != 0 {
		t.Fatalf("replacement was not detected: %v", err)
	}
	if err := os.Remove(filepath.Join(directory, "tls.crt")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.load(time.Now()); err == nil {
		t.Fatal("missing certificate fell back to cached material")
	}
}

func TestAdmissionMetricsDistinguishHTTP200Denials(t *testing.T) {
	for _, test := range []struct {
		outcome  string
		response admission.Response
	}{
		{"allowed", admission.Allowed("")},
		{"denied", admission.Denied("synthetic policy denial")},
		{"invalid_request", admission.Errored(400, fmt.Errorf("synthetic malformed object"))},
		{"dependency_error", admission.Errored(503, fmt.Errorf("synthetic unavailable dependency"))},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			counter := admissionDecisions.WithLabelValues("validate", test.outcome)
			var before, after dto.Metric
			if err := counter.Write(&before); err != nil {
				t.Fatal(err)
			}
			handler := instrumentAdmission("validate", func(context.Context, admission.Request) admission.Response {
				return test.response
			})
			response := handler(context.Background(), admission.Request{})
			if response.Allowed != test.response.Allowed {
				t.Fatal("instrumentation changed the admission decision")
			}
			if err := counter.Write(&after); err != nil {
				t.Fatal(err)
			}
			if after.GetCounter().GetValue() != before.GetCounter().GetValue()+1 || len(after.Label) != 2 {
				t.Fatal("decision metric must increment exactly once with bounded handler/outcome labels")
			}
		})
	}
}
