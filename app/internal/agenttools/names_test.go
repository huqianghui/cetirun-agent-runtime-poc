// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools_test

import (
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func TestValidDNSSubdomainName(t *testing.T) {
	for _, name := range []string{
		"a", "0", "approved.identity", strings.Repeat("a", 64),
		strings.Repeat("a", 63) + "-0", strings.Repeat("a", 253),
		strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61),
	} {
		if !agenttools.ValidDNSSubdomainName(name) {
			t.Errorf("valid Kubernetes subdomain rejected: %q", name)
		}
	}
	for _, name := range []string{
		"", ".a", "a.", "-a", "a-", "a..b", "a.-b", "a-.b", "Uppercase",
		"a_b", "a/b", "a:b", "a b", "a\n", " a", "a%2fb", "é", strings.Repeat("a", 254),
	} {
		if agenttools.ValidDNSSubdomainName(name) {
			t.Errorf("invalid Kubernetes subdomain accepted: %q", name)
		}
	}
}
