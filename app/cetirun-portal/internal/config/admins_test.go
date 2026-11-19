// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadAdminAliases_MissingPath(t *testing.T) {
	got, err := LoadAdminAliases("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
	got, err = LoadAdminAliases("/nonexistent/path/aliases")
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for missing file", got)
	}
}

func TestLoadAdminAliases_FileContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aliases")
	contents := "alice\n# comment\n\n  bob  \n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	got, err := LoadAdminAliases(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
