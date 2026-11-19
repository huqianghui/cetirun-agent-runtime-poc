// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingressmtls

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ValidateServingMaterial checks public material against the active Envoy leaf.
// The Gateway performs chain verification during the actual mTLS handshake.
func ValidateServingMaterial(serverName string, chainPEM, rootsPEM, activeJSON []byte, now time.Time) error {
	parse := func(data []byte, roots bool) ([]*x509.Certificate, error) {
		var result []*x509.Certificate
		for len(strings.TrimSpace(string(data))) != 0 {
			block, remaining := pem.Decode(data)
			if block == nil || block.Type != "CERTIFICATE" {
				return nil, fmt.Errorf("invalid public ingress certificate")
			}
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil || roots && !certificate.IsCA || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
				return nil, fmt.Errorf("ingress certificate or trust is not current")
			}
			result, data = append(result, certificate), remaining
		}
		if len(result) == 0 {
			return nil, fmt.Errorf("missing public ingress certificate")
		}
		return result, nil
	}
	chain, err := parse(chainPEM, false)
	if err != nil {
		return err
	}
	if _, err := parse(rootsPEM, true); err != nil {
		return err
	}
	leaf := chain[0]
	if serverName == "" || !slices.Contains(leaf.DNSNames, serverName) || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return fmt.Errorf("ingress serving identity does not match its approval")
	}
	var active struct {
		Certificates []struct {
			Chain []struct {
				Serial string `json:"serial_number"`
			} `json:"cert_chain"`
		} `json:"certificates"`
	}
	if len(activeJSON) > 64<<10 || json.Unmarshal(activeJSON, &active) != nil {
		return fmt.Errorf("active ingress certificate observation is invalid")
	}
	serial := func(value string) string {
		return strings.TrimLeft(strings.ToLower(strings.ReplaceAll(value, ":", "")), "0")
	}
	for _, entry := range active.Certificates {
		for _, certificate := range entry.Chain {
			if serial(certificate.Serial) == serial(leaf.SerialNumber.Text(16)) {
				return nil
			}
		}
	}
	return fmt.Errorf("active ingress certificate differs from approved public material")
}
