// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package publish writes immutable Team skill releases to the marketplace
// storage and switches each Team's CURRENT release atomically.
package publish

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

const (
	markerFile    = ".cetirun-marketplace"
	markerContent = "cetirun-skill-marketplace v1\n"
	tmpPrefix     = ".tmp-"
	// tmpMaxAge is when an abandoned temporary entry may be removed.
	tmpMaxAge = time.Hour
)

// atomicTemp matches the temporary file of an interrupted writeFileAtomic of
// the file named in its first group.
var atomicTemp = regexp.MustCompile(`^(.+)\.tmp-[0-9a-f]{12}$`)

// File is one file of a published skill, relative to the skill directory.
type File struct {
	RelPath    string
	Executable bool
	Open       func() (io.ReadCloser, error)
}

// SkillContent is one skill version to publish.
type SkillContent struct {
	Name    string
	Version string
	Files   []File
}

// TeamRef identifies a Team.
type TeamRef struct {
	Name string
	UID  string
}

// Publisher writes releases below Root.
type Publisher struct {
	Root string
	Now  func() time.Time
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// ReleaseID identifies the content of a release.
func ReleaseID(selectionDigest string, skills []api.SkillMarketplaceReleaseSkill) string {
	lines := make([]string, 0, len(skills))
	for _, skill := range skills {
		lines = append(lines, skill.Name+"@"+skill.Version)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(selectionDigest + "\n" + strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:20]
}

// Init prepares the storage root, and refuses one that holds other data. The
// temporary file of an interrupted write of the root marker is the
// marketplace's own: Init removes it.
func (p *Publisher) Init() error {
	entries, err := os.ReadDir(p.Root)
	if err != nil {
		return fmt.Errorf("read the storage root: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case name == api.SkillMarketplaceTeamsDir || name == markerFile || name == "lost+found":
		case entry.Type().IsRegular() && isAtomicTemp(name, markerFile):
			if err := os.Remove(filepath.Join(p.Root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove an interrupted write of the storage marker: %w", err)
			}
		default:
			return fmt.Errorf("the storage root %s holds %q, which the marketplace did not write; use an empty container", p.Root, name)
		}
	}
	if err := os.MkdirAll(filepath.Join(p.Root, api.SkillMarketplaceTeamsDir), 0o755); err != nil {
		return err
	}
	marker := filepath.Join(p.Root, markerFile)
	if existing, err := readRegular(marker, int64(len(markerContent))+1); err == nil && string(existing) == markerContent {
		return nil
	}
	return writeFileAtomic(marker, []byte(markerContent))
}

// isAtomicTemp reports whether name is the temporary file of a writeFileAtomic
// of the file target.
func isAtomicTemp(name, target string) bool {
	match := atomicTemp.FindStringSubmatch(name)
	return match != nil && match[1] == target
}

func (p *Publisher) teamDir(uid string) (string, error) {
	if !validUID(uid) {
		return "", fmt.Errorf("invalid Team UID %q", uid)
	}
	return filepath.Join(p.Root, api.SkillMarketplaceTeamsDir, uid), nil
}

func validUID(uid string) bool {
	if len(uid) == 0 || len(uid) > 128 {
		return false
	}
	for i, r := range uid {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || (i > 0 && (r == '-' || r == '_' || r == '.'))) {
			return false
		}
	}
	return !strings.Contains(uid, "..")
}

// EnsureTeam creates a Team's directory, so a read-only mount of it exists
// before the first release.
func (p *Publisher) EnsureTeam(uid string) error {
	dir, err := p.teamDir(uid)
	if err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(dir, api.SkillMarketplaceReleasesDir), 0o755)
}

// readCurrentID returns the release that a Team's CURRENT names, or an empty
// ID when the Team has no CURRENT yet.
func readCurrentID(dir, uid string) (string, error) {
	raw, err := readRegular(filepath.Join(dir, api.SkillMarketplaceCurrentFile), 64)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the CURRENT release of Team %s: %w", uid, err)
	}
	id := strings.TrimSpace(string(raw))
	if !api.ValidSkillMarketplaceReleaseID(id) {
		return "", fmt.Errorf("Team %s has an invalid CURRENT release", uid)
	}
	return id, nil
}

// ReadCurrent returns a Team's current release, or an empty ID when the Team
// has no CURRENT yet. Any other failure is an error, also when CURRENT names a
// release whose manifest cannot be read.
func (p *Publisher) ReadCurrent(uid string) (string, *api.SkillMarketplaceRelease, error) {
	dir, err := p.teamDir(uid)
	if err != nil {
		return "", nil, err
	}
	id, err := readCurrentID(dir, uid)
	if err != nil || id == "" {
		return "", nil, err
	}
	manifest, err := readRegular(filepath.Join(dir, api.SkillMarketplaceReleasesDir, id, api.SkillMarketplaceReleaseManifest), 1<<20)
	if err != nil {
		return id, nil, fmt.Errorf("read the manifest of Team %s release %s: %w", uid, id, err)
	}
	var release api.SkillMarketplaceRelease
	if err := json.Unmarshal(manifest, &release); err != nil || release.ID != id {
		return id, nil, fmt.Errorf("Team %s release %s has an invalid manifest", uid, id)
	}
	return id, &release, nil
}

// ReleaseSkillFiles lists the files of one skill of an existing release, so
// a retained skill can be copied into the next release. A skill directory
// that cannot be read completely, or that has no SKILL.md, is an error.
func (p *Publisher) ReleaseSkillFiles(uid, id, skill string) ([]File, error) {
	dir, err := p.teamDir(uid)
	if err != nil {
		return nil, err
	}
	if !api.ValidSkillMarketplaceReleaseID(id) || !api.ValidSkillMarketplaceName(skill) {
		return nil, fmt.Errorf("invalid release or skill name")
	}
	base := filepath.Join(dir, api.SkillMarketplaceReleasesDir, id, skill)
	var files []File
	err = filepath.WalkDir(base, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("release %s skill %s contains a link or special file", id, skill)
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, current)
		if err != nil {
			return err
		}
		name := current
		files = append(files, File{RelPath: filepath.ToSlash(rel), Executable: info.Mode()&0o111 != 0,
			Open: func() (io.ReadCloser, error) { return os.Open(name) }})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(files, func(file File) bool { return file.RelPath == "SKILL.md" }) {
		return nil, fmt.Errorf("release %s skill %s has no SKILL.md", id, skill)
	}
	return files, nil
}

// Publish writes a release for the selection and makes it CURRENT. It does
// nothing when CURRENT already holds the same content.
func (p *Publisher) Publish(team TeamRef, selectionDigest string, content []SkillContent, commit string) (api.SkillMarketplaceRelease, bool, error) {
	dir, err := p.teamDir(team.UID)
	if err != nil {
		return api.SkillMarketplaceRelease{}, false, err
	}
	skills := make([]api.SkillMarketplaceReleaseSkill, 0, len(content))
	for _, skill := range content {
		if !api.ValidSkillMarketplaceName(skill.Name) {
			return api.SkillMarketplaceRelease{}, false, fmt.Errorf("invalid skill name %q", skill.Name)
		}
		skills = append(skills, api.SkillMarketplaceReleaseSkill{Name: skill.Name, Version: skill.Version})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	id := ReleaseID(selectionDigest, skills)
	releases := filepath.Join(dir, api.SkillMarketplaceReleasesDir)
	if err := os.MkdirAll(releases, 0o755); err != nil {
		return api.SkillMarketplaceRelease{}, false, err
	}
	current, currentRelease, err := p.ReadCurrent(team.UID)
	if err == nil && current == id && currentRelease != nil && selectionMatches(filepath.Join(releases, id), selectionDigest) {
		return *currentRelease, false, nil
	}
	release := api.SkillMarketplaceRelease{
		Version: api.SkillMarketplaceCatalogVersion, ID: id, TeamName: team.Name, TeamUID: team.UID,
		SelectionDigest: selectionDigest, Commit: commit, CreatedAt: p.now().UTC().Format(time.RFC3339), Skills: skills,
	}
	final := filepath.Join(releases, id)
	if !selectionMatches(final, selectionDigest) || !releaseComplete(final, content) {
		if err := p.writeRelease(releases, id, release, content); err != nil {
			return api.SkillMarketplaceRelease{}, false, err
		}
	} else if manifest, err := readRegular(filepath.Join(final, api.SkillMarketplaceReleaseManifest), 1<<20); err == nil {
		var existing api.SkillMarketplaceRelease
		if json.Unmarshal(manifest, &existing) == nil && existing.ID == id {
			release = existing
		}
	}
	if err := writeFileAtomic(filepath.Join(dir, api.SkillMarketplaceCurrentFile), []byte(id+"\n")); err != nil {
		return api.SkillMarketplaceRelease{}, false, err
	}
	if current != "" && current != id {
		// An Agent that started just before the switch can still copy the
		// replaced release: GC counts its minimum age from now.
		now := p.now()
		_ = os.Chtimes(filepath.Join(releases, current), now, now)
	}
	return release, true, nil
}

// releaseComplete reports whether a release directory has its manifest and
// every file of the content. An interrupted removal can leave the selection
// file of a release without all of its skills.
func releaseComplete(release string, content []SkillContent) bool {
	if info, err := os.Lstat(filepath.Join(release, api.SkillMarketplaceReleaseManifest)); err != nil || !info.Mode().IsRegular() {
		return false
	}
	for _, skill := range content {
		for _, file := range skill.Files {
			info, err := os.Lstat(filepath.Join(release, skill.Name, filepath.FromSlash(file.RelPath)))
			if err != nil || !info.Mode().IsRegular() {
				return false
			}
		}
	}
	return true
}

// removeDir moves a release or Team directory to a temporary name next to it,
// and then deletes it. A removal that stops part way leaves only a temporary
// entry, which a later pass removes, and never an incomplete release under
// its own name.
func removeDir(parent, name string) error {
	suffix, err := randomHex()
	if err != nil {
		return err
	}
	trash := filepath.Join(parent, tmpPrefix+"removed-"+name+"-"+suffix)
	if err := os.Rename(filepath.Join(parent, name), trash); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.RemoveAll(trash)
}

func (p *Publisher) writeRelease(releases, id string, release api.SkillMarketplaceRelease, content []SkillContent) error {
	suffix, err := randomHex()
	if err != nil {
		return err
	}
	tmp := filepath.Join(releases, tmpPrefix+id+"-"+suffix)
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	for _, skill := range content {
		for _, file := range skill.Files {
			if err := writeSkillFile(filepath.Join(tmp, skill.Name), file); err != nil {
				return fmt.Errorf("write %s/%s: %w", skill.Name, file.RelPath, err)
			}
		}
		if err := os.MkdirAll(filepath.Join(tmp, skill.Name), 0o755); err != nil {
			return err
		}
	}
	if err := writeFileSync(filepath.Join(tmp, api.SkillMarketplaceSelectionFile), []byte(release.SelectionDigest+"\n"), 0o644); err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(tmp, api.SkillMarketplaceReleaseManifest), append(manifest, '\n'), 0o644); err != nil {
		return err
	}
	final := filepath.Join(releases, id)
	if _, err := os.Lstat(final); err == nil {
		// An earlier attempt left an incomplete release under the final name.
		old := filepath.Join(releases, tmpPrefix+"old-"+id+"-"+suffix)
		if err := os.Rename(final, old); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(old) }()
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	cleanup = false
	return syncDir(releases)
}

func writeSkillFile(skillDir string, file File) error {
	// Validation refuses "." and ".." as names; a name that only starts with
	// dots, such as "..notes.md", stays in the skill directory.
	rel := filepath.FromSlash(file.RelPath)
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("unsafe path")
	}
	target := filepath.Join(skillDir, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	mode := os.FileMode(0o644)
	if file.Executable {
		mode = 0o755
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, reader); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

func selectionMatches(release, digest string) bool {
	info, err := os.Lstat(release)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	raw, err := readRegular(filepath.Join(release, api.SkillMarketplaceSelectionFile), 256)
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	return first == digest
}

// GC keeps the CURRENT release and the keep most recent others, and removes
// older releases after minAge and abandoned temporary entries after an hour.
// A release's age counts from when it stopped being CURRENT. GC removes
// nothing while CURRENT cannot be read.
func (p *Publisher) GC(uid string, keep int, minAge time.Duration) error {
	dir, err := p.teamDir(uid)
	if err != nil {
		return err
	}
	current, err := readCurrentID(dir, uid)
	if err != nil {
		return fmt.Errorf("keep every release: %w", err)
	}
	now := p.now()
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !isAtomicTemp(entry.Name(), api.SkillMarketplaceCurrentFile) {
				continue
			}
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > tmpMaxAge {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	releases := filepath.Join(dir, api.SkillMarketplaceReleasesDir)
	entries, err := os.ReadDir(releases)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		name string
		mod  time.Time
	}
	var others []candidate
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, tmpPrefix) {
			if now.Sub(info.ModTime()) > tmpMaxAge {
				_ = os.RemoveAll(filepath.Join(releases, name))
			}
			continue
		}
		if name == current || !api.ValidSkillMarketplaceReleaseID(name) {
			continue
		}
		others = append(others, candidate{name, info.ModTime()})
	}
	sort.Slice(others, func(i, j int) bool { return others[i].mod.After(others[j].mod) })
	for i, other := range others {
		if i < keep || now.Sub(other.mod) < minAge {
			continue
		}
		if err := removeDir(releases, other.name); err != nil {
			return err
		}
	}
	return nil
}

// ListTeams returns the Team UIDs that have a directory.
func (p *Publisher) ListTeams() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(p.Root, api.SkillMarketplaceTeamsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var uids []string
	for _, entry := range entries {
		if entry.IsDir() && validUID(entry.Name()) {
			uids = append(uids, entry.Name())
		}
	}
	sort.Strings(uids)
	return uids, nil
}

// RemoveTeam deletes a Team's releases. It also removes what an earlier
// interrupted removal of a Team left.
func (p *Publisher) RemoveTeam(uid string) error {
	if _, err := p.teamDir(uid); err != nil {
		return err
	}
	teams := filepath.Join(p.Root, api.SkillMarketplaceTeamsDir)
	if entries, err := os.ReadDir(teams); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), tmpPrefix) {
				_ = os.RemoveAll(filepath.Join(teams, entry.Name()))
			}
		}
	}
	return removeDir(teams, uid)
}

func readRegular(name string, limit int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(name))
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, limit))
}

func writeFileSync(name string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(name, mode)
}

func writeFileAtomic(name string, data []byte) error {
	suffix, err := randomHex()
	if err != nil {
		return err
	}
	tmp := name + ".tmp-" + suffix
	if err := writeFileSync(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(name))
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		// Some file systems (for example NFS on certain clients) reject
		// directory fsync; the rename is still durable on the server.
		return nil
	}
	return nil
}

func randomHex() (string, error) {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
