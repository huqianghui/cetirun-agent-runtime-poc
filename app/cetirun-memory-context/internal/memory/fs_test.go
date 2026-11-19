// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicCrashCopyIsIncludedInForget(t *testing.T) {
	const marker = "SYNTHETIC-ATOMIC-CRASH-COPY"
	if rootPath := os.Getenv("CETIRUN_TEST_ATOMIC_CRASH_ROOT"); rootPath != "" {
		root, err := openRoot(rootPath, false)
		if err != nil {
			t.Fatal(err)
		}
		root.fault = func(string) error { os.Exit(77); return nil }
		_ = root.atomic("knowledge/crash-copy.md", []byte(marker))
		t.Fatal("crash hook was not reached")
	}
	f := newFixture(t)
	note := testNote("atomic crash")
	note.Body = marker
	ref := f.note(t, note)
	child := exec.Command(os.Args[0], "-test.run=^TestAtomicCrashCopyIsIncludedInForget$")
	child.Env = append(os.Environ(), "CETIRUN_TEST_ATOMIC_CRASH_ROOT="+f.store.Config.Root)
	var exited *exec.ExitError
	if err := child.Run(); !errors.As(err, &exited) || exited.ExitCode() != 77 {
		t.Fatalf("child did not crash at the durable temporary copy: %v", err)
	}
	plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{ref}}).Data.(ForgetPlan)
	found := false
	for _, file := range plan.Files {
		found = found || atomicTemporary(file)
	}
	if !found {
		t.Fatal("actual interrupted atomic copy is absent from the forget plan")
	}
	if result := f.call(t, "forget-apply", forgetApplyInput{Plan: plan}).Data.(writeResult); result.PurgePending {
		t.Fatal("purge did not complete")
	}
	for _, file := range plan.Files {
		if _, err := os.Stat(filepath.Join(f.store.Config.Root, file)); !os.IsNotExist(err) {
			t.Fatalf("forgotten file survives: %s: %v", file, err)
		}
	}
}

func TestAtomicFailureRemovesItsTemporaryCopy(t *testing.T) {
	root, err := openRoot(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	root.fault = func(string) error { return errors.New("synthetic failure") }
	if err := root.atomic("knowledge/fixture.md", []byte("temporary")); err == nil {
		t.Fatal("injected failure did not fail the write")
	}
	files, _, err := root.inventory()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.Contains(file.Name, ".tmp-") {
			t.Fatal("recoverable error left a temporary copy")
		}
	}
}

func TestNoLinksOrEscapes(t *testing.T) {
	dir := t.TempDir()
	root, err := openRoot(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	if err := root.create("objects/note/a", []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "objects/note/a"), filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.read("hard", 100); err == nil {
		t.Fatal("accepted hard link")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "outside/new", "/absolute"} {
		if err := root.create(name, []byte("bad")); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := os.Symlink("objects/note/a", filepath.Join(dir, "soft")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.read("soft", 100); err == nil {
		t.Fatal("accepted symlink")
	}
}
