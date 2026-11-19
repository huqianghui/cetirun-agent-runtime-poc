// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestAssetInstallIsVerifiedAndDoesNotOverwriteForeignFiles(t *testing.T) {
	dir := t.TempDir()
	source := fstest.MapFS{"AGENTS.md": {Data: []byte("fixed contract")}, "skills/cetirun-memory/SKILL.md": {Data: []byte("fixed skill")}}
	if err := InstallAssets(dir, source, []byte("fixed executable")); err != nil {
		t.Fatal(err)
	}
	if err := InstallAssets(dir, source, []byte("fixed executable")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "bin/cetirun-memory"))
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatal("binary permissions", err)
	}
	info, err = os.Stat(filepath.Join(dir, "skills/cetirun-memory"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("asset directory permissions", err)
	}
	if err := InstallAssets(dir, source, []byte("different executable")); err == nil {
		t.Fatal("replaced immutable asset")
	}
	foreign := t.TempDir()
	os.WriteFile(filepath.Join(foreign, "owner-file"), []byte("keep"), 0600)
	if err := InstallAssets(foreign, source, []byte("binary")); err == nil {
		t.Fatal("adopted foreign directory")
	}
}
