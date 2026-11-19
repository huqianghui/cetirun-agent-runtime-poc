// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"testing"
)

type memoryWrapper struct{ key []byte }

func (m memoryWrapper) Wrap(_ context.Context, value []byte) ([]byte, string, error) {
	result := append([]byte(nil), value...)
	for i := range result {
		result[i] ^= m.key[i%len(m.key)]
	}
	return result, "https://example.vault.azure.net/keys/cache/key-version", nil
}

func (m memoryWrapper) Unwrap(_ context.Context, value []byte, _ string) ([]byte, error) {
	result := append([]byte(nil), value...)
	for i := range result {
		result[i] ^= m.key[i%len(m.key)]
	}
	return result, nil
}

func TestEnvelopeRoundTripAndAADBinding(t *testing.T) {
	wrapper := memoryWrapper{key: []byte("unit-test-key")}
	encoded, err := seal(context.Background(), wrapper, []byte("sensitive token cache"), []byte("instance-a"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := openEnvelope(context.Background(), wrapper, encoded, []byte("instance-a"))
	if err != nil || string(plaintext) != "sensitive token cache" {
		t.Fatalf("round trip failed: %q %v", plaintext, err)
	}
	if _, err := openEnvelope(context.Background(), wrapper, encoded, []byte("instance-b")); err == nil {
		t.Fatal("expected AAD mismatch to fail")
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := openEnvelope(context.Background(), wrapper, encoded, []byte("instance-a")); err == nil {
		t.Fatal("expected tampering to fail")
	}
}

func TestSameKeyAllowsOnlyExactVaultKey(t *testing.T) {
	tests := []struct {
		configured, returned string
		want                 bool
	}{
		{"https://v.vault.azure.net/keys/cache", "https://v.vault.azure.net/keys/cache/123", true},
		{"https://v.vault.azure.net/keys/cache/123", "https://v.vault.azure.net/keys/cache/123", true},
		{"https://v.vault.azure.net/keys/cache", "https://v.vault.azure.net/keys/cache-evil/123", false},
		{"https://v.vault.azure.net/keys/cache", "https://other.vault.azure.net/keys/cache/123", false},
		{"https://v.vault.azure.net/keys/cache?ignored=true", "https://v.vault.azure.net/keys/cache/123", false},
		{"https://user@v.vault.azure.net/keys/cache", "https://v.vault.azure.net/keys/cache/123", false},
		{"https://v.vault.azure.net/keys/cache", "https://v.vault.azure.net/keys/cache", false},
		{"https://v.vault.azure.net/keys/cache", "https://v.vault.azure.net/keys/cache/123/extra", false},
		{"https://v.vault.azure.net/keys/cache", "https://v.vault.azure.net/keys/cache/123#fragment", false},
		{"https://v.vault.azure.net/not-keys/cache", "https://v.vault.azure.net/keys/cache/123", false},
	}
	for _, tc := range tests {
		if got := sameKey(tc.configured, tc.returned); got != tc.want {
			t.Errorf("sameKey(%q, %q)=%v", tc.configured, tc.returned, got)
		}
	}
}

func TestEnvelopeRejectsInvalidDEK(t *testing.T) {
	bad := failingWrapper{}
	if _, err := seal(context.Background(), bad, []byte("x"), nil); !errors.Is(err, errWrap) {
		t.Fatalf("got %v", err)
	}
}

var errWrap = errors.New("wrap failed")

type failingWrapper struct{}

func (failingWrapper) Wrap(context.Context, []byte) ([]byte, string, error)   { return nil, "", errWrap }
func (failingWrapper) Unwrap(context.Context, []byte, string) ([]byte, error) { return nil, errWrap }
