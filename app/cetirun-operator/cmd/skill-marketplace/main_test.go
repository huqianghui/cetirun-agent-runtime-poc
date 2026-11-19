// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/gitsync"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
)

// required are the flags without a default, with synthetic values.
var required = []string{
	"--repository-url=https://git.example.invalid/example/skills.git", "--namespace=example-system",
}

func TestParseNeedsTheOperatorNamespace(t *testing.T) {
	for _, args := range [][]string{
		{"--repository-url=https://git.example.invalid/example/skills.git"},
		{"--repository-url=https://git.example.invalid/example/skills.git", "--namespace=Example_System"},
	} {
		if _, err := parse(args); err == nil || !strings.Contains(err.Error(), "--namespace must name the operator namespace") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if o, err := parse(required); err != nil || o.namespace != "example-system" {
		t.Fatalf("namespace %q, %v", o.namespace, err)
	}
}

func TestParseLimitsDefaultToTheBuiltInOnes(t *testing.T) {
	o, err := parse(required)
	if err != nil {
		t.Fatal(err)
	}
	if o.limits != skills.DefaultLimits {
		t.Fatalf("limits = %+v, want %+v", o.limits, skills.DefaultLimits)
	}
	if o.maxRepository != 256<<20 || o.maxObjects != 100000 {
		t.Fatalf("repository limits = %d bytes, %d objects", o.maxRepository, o.maxObjects)
	}
}

func TestParseLimits(t *testing.T) {
	o, err := parse(append(required, "--max-repository-size=1Gi", "--max-repository-objects=5000", "--max-skill-files=1000",
		"--max-skill-size=20Mi", "--max-skill-md-size=512Ki", "--max-skill-depth=12", "--max-description-length=2048"))
	if err != nil {
		t.Fatal(err)
	}
	// The repository limits bound the tree that the synchronizer walks too.
	want := skills.Limits{MaxFiles: 1000, MaxBytes: 20 << 20, MaxSkillMD: 512 << 10, MaxDepth: 12, MaxDescription: 2048,
		MaxTreeEntries: 5000, MaxTreePathBytes: 1 << 30}
	if o.limits != want || o.maxRepository != 1<<30 || o.maxObjects != 5000 {
		t.Fatalf("limits = %+v, repository %d bytes, %d objects", o.limits, o.maxRepository, o.maxObjects)
	}
	// The synchronizer validates the repository with these limits.
	if syncer := newSyncer(o, nil, nil, nil, nil); syncer.Limits != want ||
		syncer.Fetch != (gitsync.Limits{MaxBytes: 1 << 30, MaxObjects: 5000}) {
		t.Fatalf("synchronizer limits = %+v, fetch %+v", syncer.Limits, syncer.Fetch)
	}
}

func TestParseRefusesUnusableLimits(t *testing.T) {
	for flag, want := range map[string]string{
		"--max-repository-size=0":      "--max-repository-size must be a positive size",
		"--max-repository-objects=0":   "--max-repository-objects must be at least 1",
		"--max-skill-size=-1Mi":        "--max-skill-size must be a positive size",
		"--max-skill-md-size=large":    "--max-skill-md-size must be a positive size",
		"--max-skill-files=0":          "--max-skill-files must be at least 1",
		"--max-skill-depth=-2":         "--max-skill-depth must be at least 1",
		"--max-description-length=0":   "--max-description-length must be at least 1",
		"--max-skill-md-size=6Mi":      "--max-skill-md-size cannot be larger than --max-skill-size",
		"--max-skill-files=five":       "invalid value",
		"--max-description-length=1.5": "invalid value",
	} {
		t.Run(flag, func(t *testing.T) {
			if _, err := parse(append(append([]string{}, required...), flag)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error %v, want %q", err, want)
			}
		})
	}
}

func TestSetMemoryLimitFollowsTheContainerLimit(t *testing.T) {
	previous := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(previous)
	dir := t.TempDir()
	write := func(content string) string {
		t.Helper()
		name := filepath.Join(dir, "memory.max")
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	t.Setenv("GOMEMLIMIT", "")
	if got := setMemoryLimit(write("1073741824\n")); got != 966367638 || debug.SetMemoryLimit(-1) != got {
		t.Fatalf("limit %d, runtime %d", got, debug.SetMemoryLimit(-1))
	}
	debug.SetMemoryLimit(previous)
	for _, content := range []string{"max\n", "", "-1"} {
		if got := setMemoryLimit(write(content)); got != 0 || debug.SetMemoryLimit(-1) != previous {
			t.Fatalf("%q: limit %d", content, got)
		}
	}
	if got := setMemoryLimit(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("a missing cgroup file: limit %d", got)
	}
	t.Setenv("GOMEMLIMIT", "512MiB")
	if got := setMemoryLimit(write("1073741824\n")); got != 0 || debug.SetMemoryLimit(-1) != previous {
		t.Fatalf("GOMEMLIMIT must win: limit %d", got)
	}
}
