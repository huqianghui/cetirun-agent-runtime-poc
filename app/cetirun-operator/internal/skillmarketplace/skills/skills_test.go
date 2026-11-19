// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package skills

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"
)

// fakeTree is an in-memory Tree. Directories are implied by file paths.
type fakeTree struct {
	files map[string]string
	modes map[string]Mode
}

func newTree(files map[string]string) *fakeTree {
	return &fakeTree{files: files, modes: map[string]Mode{}}
}

func (t *fakeTree) Walk(root string, fn func(Entry) error) error {
	dirs := map[string]bool{}
	var names []string
	for name := range t.files {
		if root != "" && !strings.HasPrefix(name, root+"/") {
			continue
		}
		names = append(names, name)
		for dir := path.Dir(name); dir != "." && dir != root; dir = path.Dir(dir) {
			dirs[dir] = true
		}
	}
	for dir := range dirs {
		names = append(names, dir+"/")
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			if err := fn(Entry{Path: strings.TrimSuffix(name, "/"), Mode: Dir}); err != nil {
				return err
			}
			continue
		}
		content := t.files[name]
		mode, ok := t.modes[name]
		if !ok {
			mode = Regular
		}
		if err := fn(Entry{Path: name, Mode: mode, Size: int64(len(content)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }}); err != nil {
			return err
		}
	}
	return nil
}

func (t *fakeTree) TreeHash(dir string) (string, error) {
	hash := sha1.New()
	var names []string
	for name := range t.files {
		if strings.HasPrefix(name, dir+"/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		hash.Write([]byte(name + "\x00" + t.files[name] + "\x00"))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func skillMD(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\n"
}

func TestDiscover(t *testing.T) {
	tree := newTree(map[string]string{
		"skills/pdf-tools/SKILL.md":            skillMD("pdf-tools", "Read and write PDF files"),
		"skills/pdf-tools/scripts/convert.sh":  "#!/bin/sh\n",
		"skills/pdf-tools/nested/SKILL.md":     skillMD("nested", "Inner skill"),
		"skills/wrong/SKILL.md":                skillMD("other", "Name differs"),
		"skills/no-front/SKILL.md":             "# no frontmatter\n",
		"skills/linked/SKILL.md":               skillMD("linked", "Has a link"),
		"skills/linked/link":                   "target",
		"skills/cetirun-memory/SKILL.md":       skillMD("cetirun-memory", "Reserved"),
		"skills/a/dup/SKILL.md":                skillMD("dup", "First"),
		"skills/b/dup/SKILL.md":                skillMD("dup", "Second"),
		"skills/.hidden/secret/SKILL.md":       skillMD("secret", "Hidden"),
		"skills/Upper/SKILL.md":                skillMD("Upper", "Invalid name"),
		"skills/no-description/SKILL.md":       "---\nname: no-description\n---\n",
		"README.md":                            "outside",
		"skills/pdf-tools/references/notes.md": "notes",
	})
	tree.modes["skills/linked/link"] = Symlink
	tree.modes["skills/pdf-tools/scripts/convert.sh"] = Executable
	found, problems, err := Discover(tree, "skills", DefaultLimits, DefaultReserved)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "pdf-tools" || found[0].Description != "Read and write PDF files" || len(found[0].Version) != 12 {
		t.Fatalf("unexpected skills: %+v", found)
	}
	var executable bool
	for _, file := range found[0].Files {
		if file.RelPath == "scripts/convert.sh" {
			executable = file.Executable
		}
	}
	if !executable || len(found[0].Files) != 4 {
		t.Fatalf("files: %+v", found[0].Files)
	}
	reasons := map[string]string{}
	for _, problem := range problems {
		reasons[problem.Path] = problem.Reason
	}
	for dir, want := range map[string]string{
		"skills/pdf-tools/nested": "nested inside skill pdf-tools",
		"skills/wrong":            "differs from the directory name",
		"skills/no-front":         "frontmatter",
		"skills/linked":           "symbolic link",
		"skills/cetirun-memory":   "reserved",
		"skills/a/dup":            "duplicate",
		"skills/b/dup":            "duplicate",
		"skills/Upper":            "not a valid skill name",
		"skills/no-description":   "description",
	} {
		if !strings.Contains(reasons[dir], want) {
			t.Errorf("%s: reason %q, want %q", dir, reasons[dir], want)
		}
	}
	if _, ok := reasons["skills/.hidden/secret"]; ok {
		t.Fatalf("hidden directories must not be searched")
	}
}

func TestDiscoverLimits(t *testing.T) {
	files := map[string]string{"big/SKILL.md": skillMD("big", "Too many files")}
	for i := 0; i < 3; i++ {
		files["big/f"+string(rune('a'+i))] = "x"
	}
	limits := DefaultLimits
	limits.MaxFiles = 3
	_, problems, err := Discover(newTree(files), "", limits, nil)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0].Reason, "files") {
		t.Fatalf("file limit: %v %+v", err, problems)
	}
	limits = DefaultLimits
	limits.MaxDepth = 2
	_, problems, _ = Discover(newTree(map[string]string{"deep/SKILL.md": skillMD("deep", "d"), "deep/a/b/c": "x"}), "", limits, nil)
	if len(problems) != 1 || !strings.Contains(problems[0].Reason, "deeper") {
		t.Fatalf("depth limit: %+v", problems)
	}
	// Each configured limit decides; one byte or character over is refused.
	sized := map[string]string{"sized/SKILL.md": skillMD("sized", "Ten chars."), "sized/data": strings.Repeat("x", 100)}
	total := int64(len(sized["sized/SKILL.md"]) + 100)
	for name, tc := range map[string]struct {
		change func(*Limits)
		reason string
	}{
		"size":             {func(l *Limits) { l.MaxBytes = total - 1 }, "bytes; at most"},
		"size fits":        {func(l *Limits) { l.MaxBytes = total }, ""},
		"SKILL.md":         {func(l *Limits) { l.MaxSkillMD = int64(len(sized["sized/SKILL.md"])) - 1 }, "SKILL.md has"},
		"description":      {func(l *Limits) { l.MaxDescription = 9 }, "longer than 9 characters"},
		"description fits": {func(l *Limits) { l.MaxDescription = 10 }, ""},
	} {
		limits := DefaultLimits
		tc.change(&limits)
		found, problems, err := Discover(newTree(sized), "", limits, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.reason == "" {
			if len(found) != 1 || len(problems) != 0 {
				t.Errorf("%s: %+v %+v", name, found, problems)
			}
		} else if len(found) != 0 || len(problems) != 1 || !strings.Contains(problems[0].Reason, tc.reason) {
			t.Errorf("%s: %+v %+v", name, found, problems)
		}
	}
}

func TestDiscoverRootSkillIsRejected(t *testing.T) {
	_, problems, _ := Discover(newTree(map[string]string{"skills/SKILL.md": skillMD("skills", "root")}), "skills", DefaultLimits, nil)
	if len(problems) != 1 || !strings.Contains(problems[0].Reason, "below the repository path") {
		t.Fatalf("root SKILL.md: %+v", problems)
	}
}

// A directory whose SKILL.md is not a regular file, for example a symbolic
// link to the SKILL.md of another skill, is a rejected skill directory with a
// reason. It is not left out of the catalog without one. The skill that holds
// the target of the link stays valid.
func TestDiscoverRejectsASkillWhoseSKILLmdIsNotARegularFile(t *testing.T) {
	tree := newTree(map[string]string{
		"skills/cve-detect/SKILL.md": skillMD("cve-detect", "Detect CVEs"),
		"skills/cve-check/SKILL.md":  "../cve-detect/SKILL.md",
		"skills/cve-fix/SKILL.md":    "../cve-detect/SKILL.md",
		"skills/sub/SKILL.md":        "",
		// Inside a valid outer skill, a linked SKILL.md is a nested skill
		// directory, and the link also rejects the outer skill.
		"skills/outer/SKILL.md":       skillMD("outer", "Outer skill"),
		"skills/outer/inner/SKILL.md": "../SKILL.md",
	})
	tree.modes["skills/cve-check/SKILL.md"] = Symlink
	tree.modes["skills/cve-fix/SKILL.md"] = Symlink
	tree.modes["skills/sub/SKILL.md"] = Submodule
	tree.modes["skills/outer/inner/SKILL.md"] = Symlink
	found, problems, err := Discover(tree, "skills", DefaultLimits, DefaultReserved)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "cve-detect" {
		t.Fatalf("the target of the links must stay the only valid skill: %+v", found)
	}
	reasons := map[string]string{}
	for _, problem := range problems {
		reasons[problem.Path] = problem.Reason
	}
	for dir, want := range map[string]string{
		"skills/cve-check":   "SKILL.md is a symbolic link",
		"skills/cve-fix":     "SKILL.md is a symbolic link",
		"skills/sub":         "SKILL.md is a Git submodule",
		"skills/outer":       "inner/SKILL.md is a symbolic link",
		"skills/outer/inner": "nested inside skill outer",
	} {
		if !strings.Contains(reasons[dir], want) {
			t.Errorf("%s: reason %q, want %q", dir, reasons[dir], want)
		}
	}
	if len(problems) != 5 {
		t.Errorf("problems: %+v", problems)
	}
}

// generatedTree yields directories with paths of a given length, without
// keeping them, and counts the entries that it yields.
type generatedTree struct {
	entries, pathBytes, visited int
}

func (t *generatedTree) Walk(root string, fn func(Entry) error) error {
	for i := 0; i < t.entries; i++ {
		t.visited++
		name := fmt.Sprintf("%s/d%07d", root, i)
		if pad := t.pathBytes - len(name); pad > 0 {
			name += strings.Repeat("x", pad)
		}
		if err := fn(Entry{Path: name, Mode: Dir}); err != nil {
			return err
		}
	}
	return nil
}

func (t *generatedTree) TreeHash(string) (string, error) { return "", errors.New("not used") }

// A Git commit can hold one directory at many paths, so a small commit can
// expand into a large tree. The walk stops at the first entry over a tree
// limit, before Discover keeps it.
func TestDiscoverStopsAtTheTreeLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		tree    generatedTree
		change  func(*Limits)
		visited int
		message string
	}{
		"entries":          {generatedTree{entries: 1000}, func(l *Limits) { l.MaxTreeEntries = 50 }, 51, "more than 50 files and directories"},
		"entries fit":      {generatedTree{entries: 50}, func(l *Limits) { l.MaxTreeEntries = 50 }, 50, ""},
		"path bytes":       {generatedTree{entries: 1000, pathBytes: 200}, func(l *Limits) { l.MaxTreePathBytes = 10000 }, 51, "add up to more than 10000 bytes"},
		"path bytes fit":   {generatedTree{entries: 50, pathBytes: 200}, func(l *Limits) { l.MaxTreePathBytes = 10000 }, 50, ""},
		"default entries":  {generatedTree{entries: 200000}, func(l *Limits) { l.MaxTreeEntries = 0 }, 100001, "more than 100000 files and directories"},
		"root of the repo": {generatedTree{entries: 10}, func(l *Limits) { l.MaxTreeEntries = 5 }, 6, "the repository path / holds"},
	} {
		t.Run(name, func(t *testing.T) {
			limits := DefaultLimits
			tc.change(&limits)
			tree := tc.tree
			root := "skills"
			if name == "root of the repo" {
				root = ""
			}
			_, _, err := Discover(&tree, root, limits, nil)
			if tc.message == "" {
				if err != nil {
					t.Fatalf("a tree within the limits: %v", err)
				}
			} else if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error %v, want %q", err, tc.message)
			}
			if tree.visited != tc.visited {
				t.Fatalf("the walk visited %d entries, want %d", tree.visited, tc.visited)
			}
		})
	}
}

// Unset tree limits use the defaults; they never mean "no limit".
func TestTreeLimitsDefaultToTheRepositoryDefaults(t *testing.T) {
	for _, limits := range []Limits{{}, {MaxTreeEntries: -1, MaxTreePathBytes: -1}} {
		if entries, pathBytes := limits.treeLimits(); entries != 100000 || pathBytes != 256<<20 {
			t.Fatalf("%+v: %d entries, %d path bytes", limits, entries, pathBytes)
		}
	}
	if entries, pathBytes := (Limits{MaxTreeEntries: 7, MaxTreePathBytes: 9}).treeLimits(); entries != 7 || pathBytes != 9 {
		t.Fatalf("configured limits: %d entries, %d path bytes", entries, pathBytes)
	}
}

// Each skill gets exactly the files below its own directory, also next to
// directories whose names start with the same characters, and a skill below
// another skill is nested at any depth.
func TestDiscoverAssignsFilesToTheirSkill(t *testing.T) {
	files := map[string]string{}
	for name, count := range map[string]int{"tool": 1, "tool-kit": 2, "tool2": 3, "toolbox": 4} {
		files["skills/"+name+"/SKILL.md"] = skillMD(name, "A tool")
		for i := 0; i < count; i++ {
			files[fmt.Sprintf("skills/%s/f%d", name, i)] = "x"
		}
	}
	files["skills/tool/a/b/inner/SKILL.md"] = skillMD("inner", "Two levels below tool")
	files["skills/tool-kit/x/SKILL.md"] = skillMD("x", "Below tool-kit")
	found, problems, err := Discover(newTree(files), "skills", DefaultLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, skill := range found {
		counts[skill.Name] = len(skill.Files)
	}
	// SKILL.md and its own files; the nested SKILL.md files belong to the outer skills.
	want := map[string]int{"tool": 3, "tool-kit": 4, "tool2": 4, "toolbox": 5}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("files per skill %v, want %v", counts, want)
	}
	reasons := map[string]string{}
	for _, problem := range problems {
		reasons[problem.Path] = problem.Reason
	}
	if len(problems) != 2 || !strings.Contains(reasons["skills/tool/a/b/inner"], "nested inside skill tool") ||
		!strings.Contains(reasons["skills/tool-kit/x"], "nested inside skill tool-kit") {
		t.Fatalf("problems %+v", problems)
	}
}

func TestDiscoverRejectsOnlyTheSkillWithAnUnsafeName(t *testing.T) {
	files := map[string]string{
		"skills/good/SKILL.md":    skillMD("good", "Valid"),
		"docs/bad\x01name.md":     "outside every skill",
		"docs/.git/config":        "outside every skill",
		"skills/good/.github/x":   "a dot directory that is not .git",
		"skills/good/notes\u00e9": "a non-ASCII name",
	}
	bad := map[string]string{
		"control":   "bad\x01name.txt",
		"delete":    "bad\x7fname.txt",
		"c1":        "bad\u0085name.txt",
		"dot-git":   ".git/config",
		"upper-git": ".GIT/config",
		"git-dots":  ".git. /hooks/post-checkout",
		"short-git": "GIT~1/config",
		"backslash": "a\\b.txt",
	}
	for name, file := range bad {
		files["skills/"+name+"/SKILL.md"] = skillMD(name, "Has an unsafe name")
		files["skills/"+name+"/"+file] = "x"
	}
	found, problems, err := Discover(newTree(files), "skills", DefaultLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "good" || len(found[0].Files) != 3 {
		t.Fatalf("only the valid skill must be found: %+v", found)
	}
	reasons := map[string]string{}
	for _, problem := range problems {
		reasons[problem.Path] = problem.Reason
	}
	for name := range bad {
		if !strings.Contains(reasons["skills/"+name], "is not allowed") {
			t.Errorf("%s: reason %q", name, reasons["skills/"+name])
		}
	}
	if len(problems) != len(bad) {
		t.Fatalf("names outside every skill must not be problems: %+v", problems)
	}
}

// The storage and the Agents' file systems take names of at most 255 bytes.
// A longer name rejects its skill, which the worker could not publish; names
// that only start with dots stay valid.
func TestDiscoverRejectsANameLongerThan255Bytes(t *testing.T) {
	cjk := strings.Repeat("\u6d4b", 85) // 255 bytes
	files := map[string]string{
		"skills/fits/SKILL.md":                  skillMD("fits", "A name of 255 bytes"),
		"skills/fits/" + cjk:                    "x",
		"skills/dots/SKILL.md":                  skillMD("dots", "Names that start with dots"),
		"skills/dots/..notes.md":                "x",
		"skills/dots/...":                       "x",
		"skills/dots/..data/x.txt":              "x",
		"skills/long-file/SKILL.md":             skillMD("long-file", "A file name of 258 bytes"),
		"skills/long-file/" + cjk + ".md":       "x",
		"skills/long-dir/SKILL.md":              skillMD("long-dir", "A directory name of 256 bytes"),
		"skills/long-dir/" + cjk + "x/notes.md": "x",
	}
	found, problems, err := Discover(newTree(files), "skills", DefaultLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, skill := range found {
		names = append(names, skill.Name)
	}
	if !slices.Equal(names, []string{"dots", "fits"}) {
		t.Fatalf("valid skills %v", names)
	}
	reasons := map[string]string{}
	for _, problem := range problems {
		reasons[problem.Path] = problem.Reason
	}
	for _, name := range []string{"long-file", "long-dir"} {
		if !strings.Contains(reasons["skills/"+name], "longer than 255 bytes") {
			t.Errorf("%s: reason %q", name, reasons["skills/"+name])
		}
	}
}

func TestFrontmatter(t *testing.T) {
	for _, tc := range []struct{ content, reason string }{
		{"---\r\nname: a\r\ndescription: b\r\n---\r\n", ""},
		{"\ufeff---\nname: a\ndescription: b\n---\n", ""},
		{"---\nname: a\ndescription: b\n", "not closed"},
		{"---\nname: [a\n---\n", "not valid YAML"},
		{"---\nname: a\ndescription: " + strings.Repeat("x", 1024) + "\n---\n", ""},
		{"---\nname: a\ndescription: " + strings.Repeat("x", 1025) + "\n---\n", "longer than 1024 characters"},
		// Characters, not bytes, count.
		{"---\nname: a\ndescription: " + strings.Repeat("\u6587", 1024) + "\n---\n", ""},
		{"---\nname: a\ndescription: \"bad\\u0007\"\n---\n", "control"},
	} {
		_, _, reason := frontmatter([]byte(tc.content), DefaultLimits.MaxDescription)
		if (tc.reason == "") != (reason == "") || !strings.Contains(reason, tc.reason) {
			t.Errorf("%q: reason %q, want %q", tc.content, reason, tc.reason)
		}
	}
}
