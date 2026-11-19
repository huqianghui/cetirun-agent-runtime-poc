// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package publish

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func content(name, version string, files map[string]string) SkillContent {
	skill := SkillContent{Name: name, Version: version}
	for rel, data := range files {
		data := data
		skill.Files = append(skill.Files, File{RelPath: rel, Executable: strings.HasSuffix(rel, ".sh"),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data)), nil }})
	}
	return skill
}

const uid = "00000000-0000-4000-8000-000000000001"

func TestReleaseIDIsContentAddressed(t *testing.T) {
	a := ReleaseID("sha256:x", []api.SkillMarketplaceReleaseSkill{{Name: "a", Version: "1"}, {Name: "b", Version: "2"}})
	b := ReleaseID("sha256:x", []api.SkillMarketplaceReleaseSkill{{Name: "b", Version: "2"}, {Name: "a", Version: "1"}})
	c := ReleaseID("sha256:y", []api.SkillMarketplaceReleaseSkill{{Name: "a", Version: "1"}, {Name: "b", Version: "2"}})
	if a != b || a == c || !api.ValidSkillMarketplaceReleaseID(a) {
		t.Fatalf("release IDs %q %q %q", a, b, c)
	}
}

func TestPublishLayoutAndIdempotence(t *testing.T) {
	root := t.TempDir()
	p := &Publisher{Root: root}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	digest := api.SkillMarketplaceSelectionDigest([]string{"pdf-tools"})
	release, changed, err := p.Publish(TeamRef{Name: "research", UID: uid}, digest,
		[]SkillContent{content("pdf-tools", "0123456789ab", map[string]string{"SKILL.md": "x", "scripts/run.sh": "#!/bin/sh"})}, "c1")
	if err != nil || !changed {
		t.Fatalf("publish: %v %v", changed, err)
	}
	team := filepath.Join(root, "teams", uid)
	current, err := os.ReadFile(filepath.Join(team, "CURRENT"))
	if err != nil || strings.TrimSpace(string(current)) != release.ID {
		t.Fatalf("CURRENT %q, release %q, err %v", current, release.ID, err)
	}
	selection, _ := os.ReadFile(filepath.Join(team, "releases", release.ID, ".cetirun-selection"))
	if string(selection) != digest+"\n" {
		t.Fatalf("selection file %q", selection)
	}
	info, err := os.Stat(filepath.Join(team, "releases", release.ID, "pdf-tools", "scripts", "run.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("executable file: %v %v", info, err)
	}
	if _, changed, err := p.Publish(TeamRef{Name: "research", UID: uid}, digest,
		[]SkillContent{content("pdf-tools", "0123456789ab", map[string]string{"SKILL.md": "x"})}, "c2"); err != nil || changed {
		t.Fatalf("same content must not publish again: %v %v", changed, err)
	}
	id, read, err := p.ReadCurrent(uid)
	if err != nil || id != release.ID || read == nil || len(read.Skills) != 1 {
		t.Fatalf("ReadCurrent %q %+v %v", id, read, err)
	}
	files, err := p.ReleaseSkillFiles(uid, release.ID, "pdf-tools")
	if err != nil || len(files) != 2 {
		t.Fatalf("release files %+v %v", files, err)
	}
	digest2 := api.SkillMarketplaceSelectionDigest([]string{"pdf-tools", "web"})
	second, changed, err := p.Publish(TeamRef{Name: "research", UID: uid}, digest2,
		[]SkillContent{{Name: "pdf-tools", Version: "0123456789ab", Files: files}, content("web", "ba9876543210", map[string]string{"SKILL.md": "w"})}, "c3")
	if err != nil || !changed || second.ID == release.ID {
		t.Fatalf("new selection: %v %v %v", second.ID, changed, err)
	}
	current, _ = os.ReadFile(filepath.Join(team, "CURRENT"))
	if strings.TrimSpace(string(current)) != second.ID {
		t.Fatalf("CURRENT must move to the new release")
	}
	if data, _ := os.ReadFile(filepath.Join(team, "releases", second.ID, "pdf-tools", "SKILL.md")); string(data) != "x" {
		t.Fatalf("retained skill copied from the previous release: %q", data)
	}
}

func TestGC(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	clock := now
	p := &Publisher{Root: root, Now: func() time.Time { return clock }}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	// One release an hour: each replaces the one before.
	var ids []string
	for i, name := range []string{"a", "b", "c", "d"} {
		clock = now.Add(-time.Duration(4-i) * time.Hour)
		release, _, err := p.Publish(TeamRef{Name: "t", UID: uid}, "sha256:"+name, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, release.ID)
		if err := os.Chtimes(filepath.Join(root, "teams", uid, "releases", release.ID), clock, clock); err != nil {
			t.Fatal(err)
		}
	}
	clock = now
	tmp := filepath.Join(root, "teams", uid, "releases", ".tmp-abandoned")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	_ = os.Chtimes(tmp, old, old)
	// Interrupted writes of CURRENT: an old one is abandoned, a new one may
	// still be in progress.
	abandoned := filepath.Join(root, "teams", uid, "CURRENT.tmp-0123456789ab")
	recent := filepath.Join(root, "teams", uid, "CURRENT.tmp-ba9876543210")
	for _, name := range []string{abandoned, recent} {
		if err := os.WriteFile(name, []byte(ids[0]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chtimes(abandoned, old, old)
	_ = os.Chtimes(recent, now, now)
	if err := p.GC(uid, 2, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		_, err := os.Stat(filepath.Join(root, "teams", uid, "releases", id))
		if exists := err == nil; exists != (i >= 1) {
			t.Errorf("release %d exists=%v", i, exists)
		}
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("abandoned temporary release must be removed")
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("an abandoned write of CURRENT must be removed")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("a recent write of CURRENT must stay: %v", err)
	}
}

// GC removes nothing while it cannot tell which release is current: CURRENT
// may name an older release than the newest ones.
func TestGCKeepsEveryReleaseWhileCURRENTCannotBeRead(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	p := &Publisher{Root: root, Now: func() time.Time { return now }}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, name := range []string{"a", "b", "c", "d"} {
		release, _, err := p.Publish(TeamRef{Name: "t", UID: uid}, "sha256:"+name, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, release.ID)
		old := now.Add(-time.Duration(4-i) * time.Hour)
		_ = os.Chtimes(filepath.Join(root, "teams", uid, "releases", release.ID), old, old)
	}
	current := filepath.Join(root, "teams", uid, "CURRENT")
	// Each case replaces CURRENT, so that no case depends on the one before.
	for _, damage := range []struct {
		name  string
		apply func() error
	}{
		{"invalid", func() error { return os.WriteFile(current, []byte("not a release\n"), 0o644) }},
		{"directory", func() error { return os.Mkdir(current, 0o755) }},
	} {
		if err := os.RemoveAll(current); err != nil {
			t.Fatal(err)
		}
		if err := damage.apply(); err != nil {
			t.Fatal(err)
		}
		if err := p.GC(uid, 2, 10*time.Minute); err == nil {
			t.Fatalf("%s: GC must fail while CURRENT cannot be read", damage.name)
		}
		for i, id := range ids {
			if _, err := os.Stat(filepath.Join(root, "teams", uid, "releases", id)); err != nil {
				t.Fatalf("%s: release %d must stay: %v", damage.name, i, err)
			}
		}
	}
}

func TestInitRefusesForeignRoot(t *testing.T) {
	for _, foreign := range []string{"other", ".cetirun-marketplace.tmp-xyz", "other.tmp-0123456789ab", ".cetirun-marketplace.tmp-0123456789abc"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, foreign), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := (&Publisher{Root: root}).Init(); err == nil {
			t.Fatalf("a root with %q must be refused", foreign)
		}
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".cetirun-marketplace.tmp-0123456789ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&Publisher{Root: root}).Init(); err == nil {
		t.Fatal("a directory with the name of a temporary marker file must be refused")
	}
}

// A process that stops while it writes the root marker leaves the marker's
// temporary file: the next start removes it, and keeps an unchanged marker.
func TestInitRecoversAnInterruptedMarkerWrite(t *testing.T) {
	root := t.TempDir()
	p := &Publisher{Root: root}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ".cetirun-marketplace")
	old := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	interrupted := filepath.Join(root, ".cetirun-marketplace.tmp-0123456789ab")
	if err := os.WriteFile(interrupted, []byte("cetirun-skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("the marker's own temporary file must not stop the start: %v", err)
	}
	if _, err := os.Stat(interrupted); !os.IsNotExist(err) {
		t.Fatalf("the interrupted write must be removed: %v", err)
	}
	info, err := os.Stat(marker)
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("an unchanged marker must not be written again: %v %v", info.ModTime(), err)
	}
	// A damaged marker is written again.
	if err := os.WriteFile(marker, []byte("cetirun"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "cetirun-skill-marketplace v1\n" {
		t.Fatalf("marker %q %v", data, err)
	}
}

func TestReleaseSkillFilesNeedsTheWholeSkill(t *testing.T) {
	root := t.TempDir()
	p := &Publisher{Root: root}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	release, _, err := p.Publish(TeamRef{Name: "research", UID: uid}, "sha256:x",
		[]SkillContent{content("pdf-tools", "0123456789ab", map[string]string{"SKILL.md": "x", "notes.md": "n"})}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReleaseSkillFiles(uid, release.ID, "web"); err == nil {
		t.Fatal("a skill that the release does not hold must be an error")
	}
	if err := os.Remove(filepath.Join(root, "teams", uid, "releases", release.ID, "pdf-tools", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReleaseSkillFiles(uid, release.ID, "pdf-tools"); err == nil {
		t.Fatal("a skill without SKILL.md must be an error")
	}
}

func TestInvalidUIDIsRefused(t *testing.T) {
	p := &Publisher{Root: t.TempDir()}
	for _, bad := range []string{"", "../x", "a/b", ".hidden"} {
		if err := p.EnsureTeam(bad); err == nil {
			t.Errorf("UID %q must be refused", bad)
		}
	}
}

// A file name that only starts with dots stays in its skill directory, and
// publishes. A path that leaves the directory never does.
func TestPublishWritesNamesThatStartWithDots(t *testing.T) {
	for _, name := range []string{"..notes.md", "...", "..data/x.txt", "docs/..notes.md", ".hidden"} {
		p := &Publisher{Root: t.TempDir()}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		release, _, err := p.Publish(TeamRef{Name: "research", UID: uid}, "sha256:x",
			[]SkillContent{content("pdf-tools", "0123456789ab", map[string]string{"SKILL.md": "x", name: "y"})}, "c1")
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		data, err := os.ReadFile(filepath.Join(p.Root, "teams", uid, "releases", release.ID, "pdf-tools", filepath.FromSlash(name)))
		if err != nil || string(data) != "y" {
			t.Fatalf("%q: %q %v", name, data, err)
		}
	}
	for _, name := range []string{"../escape.md", "a/../../escape.md", "/abs.md", "..", ""} {
		p := &Publisher{Root: t.TempDir()}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Publish(TeamRef{Name: "research", UID: uid}, "sha256:x",
			[]SkillContent{content("pdf-tools", "0123456789ab", map[string]string{"SKILL.md": "x", name: "y"})}, "c1"); err == nil {
			t.Fatalf("%q must be refused", name)
		}
	}
}

// A removal of a release that stopped part way can leave its selection file
// without all of its skills. When the same release is needed again, Publish
// writes it again instead of making the incomplete directory CURRENT.
func TestPublishRewritesAnIncompleteRelease(t *testing.T) {
	root := t.TempDir()
	p := &Publisher{Root: root}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	team := TeamRef{Name: "research", UID: uid}
	pdf := func() []SkillContent {
		return []SkillContent{content("pdf-tools", "0123456789ab", map[string]string{"SKILL.md": "pdf", "scripts/run.sh": "#!/bin/sh"})}
	}
	digest := api.SkillMarketplaceSelectionDigest([]string{"pdf-tools"})
	first, _, err := p.Publish(team, digest, pdf(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Publish(team, "sha256:other", nil, "c1"); err != nil {
		t.Fatal(err)
	}
	released := filepath.Join(root, "teams", uid, "releases", first.ID)
	if err := os.RemoveAll(filepath.Join(released, "pdf-tools", "scripts")); err != nil {
		t.Fatal(err)
	}
	again, changed, err := p.Publish(team, digest, pdf(), "c1")
	if err != nil || !changed || again.ID != first.ID {
		t.Fatalf("publish again: %+v %v %v", again, changed, err)
	}
	if data, err := os.ReadFile(filepath.Join(released, "pdf-tools", "scripts", "run.sh")); err != nil || string(data) != "#!/bin/sh" {
		t.Fatalf("the release must be complete again: %q %v", data, err)
	}
	if id, _, err := p.ReadCurrent(uid); err != nil || id != first.ID {
		t.Fatalf("CURRENT %q %v", id, err)
	}
}

// An Agent can start just before CURRENT moves on, and copy the release that
// CURRENT named: GC counts the age of a release from when it stopped being
// CURRENT, not from when it was written.
func TestGCCountsTheAgeOfAReleaseFromItsReplacement(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	clock := now.Add(-24 * time.Hour)
	p := &Publisher{Root: root, Now: func() time.Time { return clock }}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	team := TeamRef{Name: "t", UID: uid}
	old, _, err := p.Publish(team, "sha256:old", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	releases := filepath.Join(root, "teams", uid, "releases")
	if err := os.Chtimes(filepath.Join(releases, old.ID), clock, clock); err != nil {
		t.Fatal(err)
	}
	// A day later, three releases replace it within a minute.
	for i, name := range []string{"one", "two", "three"} {
		clock = now.Add(time.Duration(i-3) * 20 * time.Second)
		release, _, err := p.Publish(team, "sha256:"+name, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(filepath.Join(releases, release.ID), clock, clock)
	}
	clock = now
	if err := p.GC(uid, 0, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(releases, old.ID)); err != nil {
		t.Fatalf("a release that was CURRENT a minute ago must stay: %v", err)
	}
	clock = now.Add(11 * time.Minute)
	if err := p.GC(uid, 0, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(releases, old.ID)); !os.IsNotExist(err) {
		t.Fatalf("after the minimum age, the release must go: %v", err)
	}
}

// GC and RemoveTeam first move a directory to a temporary name, so a removal
// that stops part way never leaves a release or Team directory of its own
// name. A later pass removes what such a removal left.
func TestRemovalLeavesNoPartialReleaseOrTeam(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	p := &Publisher{Root: root, Now: func() time.Time { return now }}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := removeDir(filepath.Join(root, "teams"), "absent"); err != nil {
		t.Fatalf("removing an absent directory is no error: %v", err)
	}
	releases := filepath.Join(root, "teams", uid, "releases")
	if err := os.MkdirAll(releases, 0o755); err != nil {
		t.Fatal(err)
	}
	// What an interrupted removal of a release and of a Team left.
	stale := filepath.Join(releases, ".tmp-removed-0123456789abcdef0123-0123456789ab")
	if err := os.MkdirAll(filepath.Join(stale, "pdf-tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	staleTeam := filepath.Join(root, "teams", ".tmp-removed-other-0123456789ab")
	if err := os.MkdirAll(filepath.Join(staleTeam, "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Publish(TeamRef{Name: "t", UID: uid}, "sha256:a", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.GC(uid, 2, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("an old interrupted release removal must be cleaned up: %v", err)
	}
	if teams, err := p.ListTeams(); err != nil || len(teams) != 1 || teams[0] != uid {
		t.Fatalf("an interrupted Team removal is no Team: %v %v", teams, err)
	}
	if err := p.RemoveTeam(uid); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "teams"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("the Team and the earlier interrupted removal must be gone: %v %v", entries, err)
	}
}
