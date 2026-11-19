// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package skills finds and validates the skills of a repository tree. It
// reads content only; it never executes anything from the repository.
package skills

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// Mode is the kind of a tree entry.
type Mode int

const (
	Regular Mode = iota
	Executable
	Dir
	Symlink
	Submodule
)

// Entry is one entry of a repository tree. Path is slash-separated and
// relative to the repository root.
type Entry struct {
	Path string
	Mode Mode
	Size int64
	Open func() (io.ReadCloser, error)
}

// Tree is a read-only repository tree.
type Tree interface {
	// Walk visits every entry below root (root itself excluded), parents first.
	Walk(root string, fn func(Entry) error) error
	// TreeHash returns the content hash of a directory.
	TreeHash(dir string) (string, error)
}

// Limits bound what a single skill may contain, and the tree that Discover
// reads.
type Limits struct {
	MaxFiles   int
	MaxBytes   int64
	MaxSkillMD int64
	MaxDepth   int
	// MaxDescription is the longest SKILL.md description, in characters.
	MaxDescription int
	// MaxTreeEntries and MaxTreePathBytes bound the tree below the
	// repository path, as Discover reads it: the number of its files and
	// directories, and the bytes of their paths together. A Git commit can
	// hold one directory at many paths, so each path counts. Zero uses the
	// default.
	MaxTreeEntries   int
	MaxTreePathBytes int64
}

// DefaultLimits are the limits used unless configured otherwise. The
// description limit is the one of the Agent Skills specification; the others
// keep a skill small enough to copy into the Pod of each Agent. The tree
// limits equal the default repository limits of the worker.
var DefaultLimits = Limits{MaxFiles: 500, MaxBytes: 5 << 20, MaxSkillMD: 256 << 10, MaxDepth: 8, MaxDescription: 1024,
	MaxTreeEntries: 100000, MaxTreePathBytes: 256 << 20}

// ErrLimit reports a repository commit that exceeds the limits of the Skill
// Marketplace.
var ErrLimit = errors.New("the repository commit exceeds the limits of the Skill Marketplace")

// DefaultReserved are the names of skills that CetiRun or its images supply.
var DefaultReserved = []string{"cetirun-memory", "connected-services"}

// File is one file of a skill, relative to the skill directory.
type File struct {
	RelPath    string
	Executable bool
	Size       int64
	Open       func() (io.ReadCloser, error)
}

// Skill is one valid skill.
type Skill struct {
	Name        string
	Description string
	Path        string
	Version     string
	Files       []File
	Bytes       int64
}

const skillFile = "SKILL.md"

// Discover returns the valid skills below root and the rejected directories.
// A tree that exceeds the tree limits is an ErrLimit error.
func Discover(tree Tree, root string, limits Limits, reserved []string) ([]Skill, []api.SkillMarketplaceProblem, error) {
	root = strings.Trim(root, "/")
	all, err := inventory(tree, root, limits)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })

	// Skill directories: directories that hold SKILL.md, not below a hidden
	// directory, outermost first. A SKILL.md that is not a regular file, such
	// as a symbolic link, still marks a skill directory, so that validate
	// rejects it with a reason instead of leaving it out of the catalog.
	var candidates []string
	for _, entry := range all {
		if path.Base(entry.Path) != skillFile || entry.Mode == Dir {
			continue
		}
		dir := path.Dir(entry.Path)
		if dir == "." {
			dir = ""
		}
		if hiddenBelow(root, dir) {
			continue
		}
		candidates = append(candidates, dir)
	}
	sort.Strings(candidates)

	var problems []api.SkillMarketplaceProblem
	var outer []string
	isOuter := map[string]bool{}
	for _, dir := range candidates {
		if parent := enclosing(isOuter, dir); parent != "" {
			problems = append(problems, api.SkillMarketplaceProblem{Path: dir, Name: path.Base(dir),
				Reason: fmt.Sprintf("nested inside skill %s; only the outer skill is published", path.Base(parent))})
			continue
		}
		if dir == root {
			problems = append(problems, api.SkillMarketplaceProblem{Path: displayPath(dir),
				Reason: "SKILL.md must be in a skill directory below the repository path, not at the path itself"})
			continue
		}
		outer = append(outer, dir)
		isOuter[dir] = true
	}

	byName := map[string][]int{}
	var found []Skill
	for _, dir := range outer {
		skill, reason := validate(tree, below(all, dir), dir, limits, reserved)
		if reason != "" {
			problems = append(problems, api.SkillMarketplaceProblem{Path: dir, Name: path.Base(dir), Reason: reason})
			continue
		}
		byName[skill.Name] = append(byName[skill.Name], len(found))
		found = append(found, skill)
	}
	var skills []Skill
	for _, skill := range found {
		if len(byName[skill.Name]) > 1 {
			problems = append(problems, api.SkillMarketplaceProblem{Path: skill.Path, Name: skill.Name,
				Reason: "duplicate skill name; every directory with this name is rejected"})
			continue
		}
		skills = append(skills, skill)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	sort.Slice(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
	return skills, problems, nil
}

func displayPath(dir string) string {
	if dir == "" {
		return "/"
	}
	return dir
}

// treeLimits returns the tree limits, with the defaults for zero.
func (l Limits) treeLimits() (int, int64) {
	maxEntries, maxPathBytes := l.MaxTreeEntries, l.MaxTreePathBytes
	if maxEntries <= 0 {
		maxEntries = DefaultLimits.MaxTreeEntries
	}
	if maxPathBytes <= 0 {
		maxPathBytes = DefaultLimits.MaxTreePathBytes
	}
	return maxEntries, maxPathBytes
}

// inventory reads the tree below root. It counts each entry and the bytes of
// its path when the walk visits it, and stops with ErrLimit before it keeps
// more than the tree limits allow: the pack limits count each object once,
// but a commit can hold one directory at many paths.
func inventory(tree Tree, root string, limits Limits) ([]Entry, error) {
	maxEntries, maxPathBytes := limits.treeLimits()
	var all []Entry
	var pathBytes int64
	err := tree.Walk(root, func(entry Entry) error {
		if len(all) >= maxEntries {
			return fmt.Errorf("%w: the repository path %s holds more than %d files and directories; each path counts",
				ErrLimit, displayPath(root), maxEntries)
		}
		if pathBytes += int64(len(entry.Path)); pathBytes > maxPathBytes {
			return fmt.Errorf("%w: the paths of the files and directories below the repository path %s add up to more than %d bytes",
				ErrLimit, displayPath(root), maxPathBytes)
		}
		all = append(all, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// below returns the entries below dir. all is sorted by path, so the paths
// that start with dir/ are one run of it.
func below(all []Entry, dir string) []Entry {
	prefix := dir + "/"
	start := sort.Search(len(all), func(i int) bool { return all[i].Path >= prefix })
	end := start
	for end < len(all) && strings.HasPrefix(all[end].Path, prefix) {
		end++
	}
	return all[start:end]
}

// hiddenBelow reports whether dir has a hidden segment below root.
func hiddenBelow(root, dir string) bool {
	rel := strings.TrimPrefix(strings.TrimPrefix(dir, root), "/")
	if rel == "" {
		return false
	}
	for _, segment := range strings.Split(rel, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

// enclosing returns the outer skill directory that holds dir, if any. The
// candidates are processed in path order, so an outer directory is known
// before the directories below it.
func enclosing(isOuter map[string]bool, dir string) string {
	for parent := path.Dir(dir); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if isOuter[parent] {
			return parent
		}
	}
	return ""
}

// validate checks the skill in dir. entries are the entries below dir.
func validate(tree Tree, entries []Entry, dir string, limits Limits, reserved []string) (Skill, string) {
	name := path.Base(dir)
	if !api.ValidSkillMarketplaceName(name) {
		return Skill{}, "the directory name is not a valid skill name: use at most 64 lowercase letters and digits, in words joined by single hyphens"
	}
	for _, value := range reserved {
		if value == name {
			return Skill{}, "the skill name is reserved by CetiRun"
		}
	}
	skill := Skill{Name: name, Path: dir}
	prefix := dir + "/"
	var skillMD *Entry
	for i := range entries {
		entry := entries[i]
		rel := strings.TrimPrefix(entry.Path, prefix)
		for _, segment := range strings.Split(rel, "/") {
			if unsafeName(segment) {
				return Skill{}, fmt.Sprintf("the file name %q is not allowed", rel)
			}
			if len(segment) > maxNameBytes {
				return Skill{}, fmt.Sprintf("a file or directory name in %q is longer than %d bytes", rel, maxNameBytes)
			}
		}
		if strings.Count(rel, "/")+1 > limits.MaxDepth {
			return Skill{}, fmt.Sprintf("%s is nested deeper than %d levels", rel, limits.MaxDepth)
		}
		switch entry.Mode {
		case Symlink:
			return Skill{}, fmt.Sprintf("%s is a symbolic link; skills cannot contain links", rel)
		case Submodule:
			return Skill{}, fmt.Sprintf("%s is a Git submodule; skills cannot contain submodules", rel)
		case Dir:
			continue
		}
		skill.Files = append(skill.Files, File{RelPath: rel, Executable: entry.Mode == Executable, Size: entry.Size, Open: entry.Open})
		skill.Bytes += entry.Size
		if rel == skillFile {
			skillMD = &entries[i]
		}
	}
	if len(skill.Files) > limits.MaxFiles {
		return Skill{}, fmt.Sprintf("the skill has %d files; at most %d are allowed", len(skill.Files), limits.MaxFiles)
	}
	if skill.Bytes > limits.MaxBytes {
		return Skill{}, fmt.Sprintf("the skill has %d bytes; at most %d are allowed", skill.Bytes, limits.MaxBytes)
	}
	if skillMD == nil {
		return Skill{}, "SKILL.md is missing"
	}
	if skillMD.Size > limits.MaxSkillMD {
		return Skill{}, fmt.Sprintf("SKILL.md has %d bytes; at most %d are allowed", skillMD.Size, limits.MaxSkillMD)
	}
	content, err := readAll(skillMD.Open, limits.MaxSkillMD)
	if err != nil {
		return Skill{}, "SKILL.md cannot be read"
	}
	frontName, description, reason := frontmatter(content, limits.MaxDescription)
	if reason != "" {
		return Skill{}, reason
	}
	if frontName != name {
		return Skill{}, fmt.Sprintf("the SKILL.md name %q differs from the directory name", frontName)
	}
	skill.Description = description
	hash, err := tree.TreeHash(dir)
	if err != nil || len(hash) < 12 {
		return Skill{}, "the skill version cannot be computed"
	}
	skill.Version = hash[:12]
	sort.Slice(skill.Files, func(i, j int) bool { return skill.Files[i].RelPath < skill.Files[j].RelPath })
	return skill, ""
}

// maxNameBytes is the longest file or directory name that the release
// storage and the Agents' file systems take (NAME_MAX). Git allows longer
// names, which the worker could not publish.
const maxNameBytes = 255

// unsafeName reports whether a file or directory name of a skill must not be
// published: it is empty, a dot name, not valid UTF-8, contains a control
// character or a backslash, or Git would treat it as its own directory
// (.git, also in other case or with trailing dots or spaces, and git~1).
func unsafeName(name string) bool {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 || strings.ContainsRune(name, '\\') {
		return true
	}
	git := strings.ToLower(strings.TrimRight(name, ". "))
	return git == ".git" || git == "git~1"
}

func readAll(open func() (io.ReadCloser, error), limit int64) ([]byte, error) {
	reader, err := open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(io.LimitReader(reader, limit+1))
}

// frontmatter returns the name and description of a SKILL.md, or a reason. A
// description may have at most maxDescription characters.
func frontmatter(content []byte, maxDescription int) (string, string, string) {
	content = bytes.TrimPrefix(content, []byte("\ufeff"))
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", "", "SKILL.md must start with YAML frontmatter between --- lines"
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return "", "", "SKILL.md frontmatter is not closed with a --- line"
	}
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(text[4:4+end]), &fields); err != nil {
		return "", "", "SKILL.md frontmatter is not valid YAML"
	}
	name, ok := fields["name"].(string)
	if !ok || name == "" {
		return "", "", "SKILL.md frontmatter needs a name"
	}
	description, ok := fields["description"].(string)
	description = strings.TrimSpace(description)
	if !ok || description == "" {
		return "", "", "SKILL.md frontmatter needs a description"
	}
	if utf8.RuneCountInString(description) > maxDescription {
		return "", "", fmt.Sprintf("the description is longer than %d characters", maxDescription)
	}
	if strings.IndexFunc(description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return "", "", "the description contains control characters"
	}
	return name, description, ""
}
