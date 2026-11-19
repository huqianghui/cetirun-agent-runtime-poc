// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingressmtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestCustomServingMaterialRequiresCurrentExactIdentity(t *testing.T) {
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	root, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root})
	for _, scenario := range []string{"current", "expired", "not-yet-valid", "wildcard", "wrong-name", "client-only", "stale-active", "invalid-active", "missing-root", "leaf-as-root"} {
		t.Run(scenario, func(t *testing.T) {
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"agent.example.com"},
				ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
			active := []byte(`{"certificates":[{"cert_chain":[{"serial_number":"00:02"}]}]}`)
			roots := rootPEM
			switch scenario {
			case "expired":
				leaf.NotAfter = now.Add(-time.Second)
			case "not-yet-valid":
				leaf.NotBefore = now.Add(time.Minute)
			case "wildcard":
				leaf.DNSNames = []string{"*.example.com"}
			case "wrong-name":
				leaf.DNSNames = []string{"other.example.com"}
			case "client-only":
				leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			case "stale-active":
				active = []byte(`{"certificates":[{"cert_chain":[{"serial_number":"03"}]}]}`)
			case "invalid-active":
				active = []byte(`{`)
			case "missing-root":
				roots = nil
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
			if scenario == "leaf-as-root" {
				roots = chain
			}
			err = ValidateServingMaterial("agent.example.com", chain, roots, active, now)
			if (err == nil) != (scenario == "current") {
				t.Fatalf("unexpected serving material result: %v", err)
			}
		})
	}
}
