// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpDoesNotContactAzure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "inspect is the default") ||
		!strings.Contains(stdout.String(), "not a CheckQuiesced provider") || stderr.Len() != 0 {
		t.Fatalf("unexpected help: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestInvalidCLIArgumentsFailBeforeAuthentication(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown"}, {"unexpected"}, {"--timeout=0"}, {"--timeout=6m"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 || stderr.Len() == 0 {
			t.Fatalf("arguments %#v: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestMissingRegistrationDoesNotReturnSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, &stdout, &stderr); code == 0 ||
		!strings.Contains(stdout.String(), `"error":`) || strings.Contains(stdout.String(), `"completion":`) {
		t.Fatalf("missing registration became success: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
