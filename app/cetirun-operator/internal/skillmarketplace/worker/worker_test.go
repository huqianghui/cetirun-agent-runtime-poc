// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/publish"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const namespace = "cetirun-system"

func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func selection(name, uid string, skills ...string) skillmarketplace.TeamSelection {
	return skillmarketplace.TeamSelection{Name: name, UID: uid, Skills: skills}
}

func repoSkill(name, version, body string) skills.Skill {
	return skills.Skill{Name: name, Version: version, Description: name, Files: []skills.File{{RelPath: "SKILL.md",
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }}}}
}

// fixture is a worker with an initialized storage and the given Teams.
type fixture struct {
	publisher *publish.Publisher
	store     *Store
	catalog   *Catalog
	teams     *TeamsSource
	worker    *TeamPublisher
}

func newFixture(t *testing.T, teams ...skillmarketplace.TeamSelection) *fixture {
	t.Helper()
	f := &fixture{publisher: &publish.Publisher{Root: t.TempDir()}, store: &Store{}, teams: &TeamsSource{}}
	if err := f.publisher.Init(); err != nil {
		t.Fatal(err)
	}
	kube := newClient(t)
	f.catalog = NewCatalog(kube, kube, namespace, api.SkillMarketplaceRepository{URL: "https://git.example.invalid/x.git"})
	f.worker = &TeamPublisher{Publisher: f.publisher, Store: f.store, Catalog: f.catalog, Teams: f.teams, GCGrace: time.Hour}
	f.setTeams(teams...)
	return f
}

func (f *fixture) setTeams(teams ...skillmarketplace.TeamSelection) {
	f.teams.Set(skillmarketplace.TeamsDocument{Version: skillmarketplace.TeamsVersion, Teams: append([]skillmarketplace.TeamSelection{}, teams...)})
}

func (f *fixture) publish(t *testing.T) bool {
	t.Helper()
	return f.worker.PublishAll(context.Background())
}

// catalogTeams returns the Team entries of the catalog document.
func catalogTeams(t *testing.T, catalog *Catalog) []api.SkillMarketplaceTeam {
	t.Helper()
	raw, err := catalog.Document()
	if err != nil {
		t.Fatal(err)
	}
	var document api.SkillMarketplaceCatalog
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document.Teams
}

func TestTeamPublicationRetainsAndReportsMissing(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	team := selection("research", uid, "pdf-tools", "web")
	f := newFixture(t, team)
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf"), repoSkill("web", "ba9876543210", "web")})
	if retry := f.publish(t); retry {
		t.Fatal("a published Team needs no retry")
	}
	id, release, err := f.publisher.ReadCurrent(uid)
	if err != nil || release == nil || len(release.Skills) != 2 || release.SelectionDigest != api.SkillMarketplaceSelectionDigest(team.Skills) {
		t.Fatalf("first release %q %+v %v", id, release, err)
	}
	// The repository drops web; the Team keeps its last published version.
	f.store.set("c2", []skills.Skill{repoSkill("pdf-tools", "0123456789ac", "pdf2")})
	f.setTeams(selection("research", uid, "pdf-tools", "web", "never-published"))
	f.publish(t)
	_, release, _ = f.publisher.ReadCurrent(uid)
	versions := map[string]string{}
	for _, skill := range release.Skills {
		versions[skill.Name] = skill.Version
	}
	if versions["web"] != "ba9876543210" || versions["pdf-tools"] != "0123456789ac" || len(versions) != 2 {
		t.Fatalf("second release %+v", release.Skills)
	}
	teams := catalogTeams(t, f.catalog)
	if len(teams) != 1 {
		t.Fatalf("catalog Teams %+v", teams)
	}
	state := teams[0]
	if state.ReleaseSelectionDigest != state.SelectionDigest || len(state.Retained) != 1 || state.Retained[0] != "web" ||
		len(state.Missing) != 1 || state.Missing[0] != "never-published" {
		t.Fatalf("Team state %+v", state)
	}
}

func TestTeamWaitsForTheFirstSync(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	f := newFixture(t, selection("research", uid, "pdf-tools"))
	if retry := f.publish(t); !retry {
		t.Fatal("must retry until synchronized")
	}
	if id, _, _ := f.publisher.ReadCurrent(uid); id != "" {
		t.Fatalf("nothing may be published before the first sync")
	}
	if _, err := os.Stat(filepath.Join(f.publisher.Root, "teams", uid, "releases")); err != nil {
		t.Fatalf("the Team directory must exist before the Agents mount it: %v", err)
	}
	if teams := catalogTeams(t, f.catalog); len(teams) != 1 || !strings.Contains(teams[0].Error, "not been synchronized") {
		t.Fatalf("catalog Teams %+v", teams)
	}
}

// catalogDocument returns the catalog document that the worker would write.
func catalogDocument(t *testing.T, catalog *Catalog) api.SkillMarketplaceCatalog {
	t.Helper()
	raw, err := catalog.Document()
	if err != nil {
		t.Fatal(err)
	}
	var document api.SkillMarketplaceCatalog
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// A restarted worker keeps the catalog of the earlier one: the Teams, and for
// the same repository the commit with its skills. Before its first
// synchronization, a Team with the same selection and release keeps its
// entry. A Team with another selection, a Team whose release changed after
// the earlier worker wrote the catalog, and a new Team wait for the
// synchronization; a Team that left the Teams document leaves the catalog.
func TestARestartedWorkerKeepsThePublishedCatalog(t *testing.T) {
	const (
		researchUID = "00000000-0000-4000-8000-000000000001"
		supportUID  = "00000000-0000-4000-8000-000000000002"
		retiredUID  = "00000000-0000-4000-8000-000000000003"
		writingUID  = "00000000-0000-4000-8000-000000000004"
		movedUID    = "00000000-0000-4000-8000-000000000005"
	)
	f := newFixture(t, selection("research", researchUID, "pdf-tools"), selection("support", supportUID, "pdf-tools"),
		selection("retired", retiredUID, "zh-notes"), selection("moved", movedUID, "zh-notes"))
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "aaaaaaaaaaa1", "pdf"), repoSkill("zh-notes", "bbbbbbbbbbb1", "zh")})
	f.catalog.SetSync("c1", []api.SkillMarketplaceSkill{
		{Name: "pdf-tools", Description: "PDF", Version: "aaaaaaaaaaa1", Path: "skills/pdf-tools", UpdatedAt: "2026-11-01T00:00:00Z"},
		{Name: "zh-notes", Description: "Notes", Version: "bbbbbbbbbbb1", Path: "skills/zh-notes", UpdatedAt: "2026-11-02T00:00:00Z"},
	}, []api.SkillMarketplaceProblem{{Path: "skills/broken", Name: "broken", Reason: "SKILL.md has no description"}})
	f.publish(t)
	raw, err := f.catalog.Document()
	if err != nil {
		t.Fatal(err)
	}
	previous := catalogDocument(t, f.catalog)
	previousTeams := map[string]api.SkillMarketplaceTeam{}
	for _, team := range previous.Teams {
		previousTeams[team.Name] = team
	}
	if len(previousTeams) != 4 || previous.Commit != "c1" || len(previous.Skills) != 2 || len(previous.Invalid) != 1 {
		t.Fatalf("test setup: catalog %+v", previous)
	}
	// The earlier worker publishes a new zh-notes, and stops before it
	// writes the catalog: the storage has releases that the catalog lacks.
	f.store.set("c2", []skills.Skill{repoSkill("pdf-tools", "aaaaaaaaaaa1", "pdf"), repoSkill("zh-notes", "bbbbbbbbbbb2", "zh2")})
	f.publish(t)
	current := func(uid string) string {
		t.Helper()
		id, _, err := f.publisher.ReadCurrent(uid)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if current(movedUID) == previousTeams["moved"].Release || current(researchUID) != previousTeams["research"].Release ||
		current(supportUID) != previousTeams["support"].Release {
		t.Fatalf("test setup: releases moved %q, research %q, support %q", current(movedUID), current(researchUID), current(supportUID))
	}

	// The worker restarts. The storage and the catalog ConfigMap stay.
	restart := func(repository api.SkillMarketplaceRepository) (*Catalog, *TeamsSource, *TeamPublisher) {
		t.Helper()
		kube := newClient(t, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: api.SkillMarketplaceCatalogConfigMap},
			Data:       map[string]string{api.SkillMarketplaceCatalogKey: string(raw)},
		})
		catalog := NewCatalog(kube, kube, namespace, repository)
		if err := catalog.Seed(context.Background()); err != nil {
			t.Fatal(err)
		}
		teams := &TeamsSource{}
		return catalog, teams, &TeamPublisher{Publisher: f.publisher, Store: &Store{}, Catalog: catalog, Teams: teams, GCGrace: time.Hour}
	}
	catalog, teams, restarted := restart(previous.Repository)
	keeps := func(stage string) {
		t.Helper()
		got := catalogDocument(t, catalog)
		if got.Commit != previous.Commit || got.SyncedAt != previous.SyncedAt || !reflect.DeepEqual(got.Skills, previous.Skills) ||
			!reflect.DeepEqual(got.Invalid, previous.Invalid) || !reflect.DeepEqual(got.Teams, previous.Teams) {
			t.Fatalf("%s: catalog %+v, want the earlier %+v", stage, got, previous)
		}
	}
	keeps("after the restart")
	if retry := restarted.PublishAll(context.Background()); !retry {
		t.Fatal("must retry until a Teams document is read")
	}
	keeps("before the Teams document")

	// research and moved keep their selections, support selects one skill
	// more with the same release, writing is new, and retired left the
	// document.
	teams.Set(skillmarketplace.TeamsDocument{Version: skillmarketplace.TeamsVersion, Teams: []skillmarketplace.TeamSelection{
		selection("research", researchUID, "pdf-tools"), selection("support", supportUID, "pdf-tools", "zh-notes"),
		selection("writing", writingUID, "pdf-tools"), selection("moved", movedUID, "zh-notes"),
	}})
	if retry := restarted.PublishAll(context.Background()); !retry {
		t.Fatal("must retry until the first synchronization")
	}
	got := map[string]api.SkillMarketplaceTeam{}
	for _, team := range catalogDocument(t, catalog).Teams {
		got[team.Name] = team
	}
	if !reflect.DeepEqual(got["research"], previousTeams["research"]) {
		t.Fatalf("an unchanged Team must keep its entry before the first synchronization: %+v, want %+v",
			got["research"], previousTeams["research"])
	}
	if support := got["support"]; support.Error != errNotSynced.Error() ||
		support.SelectionDigest != api.SkillMarketplaceSelectionDigest([]string{"pdf-tools", "zh-notes"}) || support.Release != current(supportUID) {
		t.Fatalf("a Team with another selection must wait with its current release: %+v", support)
	}
	if moved := got["moved"]; moved.Error != errNotSynced.Error() || moved.Release != current(movedUID) {
		t.Fatalf("a Team whose release changed must wait with the release on the storage: %+v", moved)
	}
	if writing := got["writing"]; writing.Error != errNotSynced.Error() || writing.Release != "" {
		t.Fatalf("a new Team must wait for the first synchronization: %+v", writing)
	}
	if _, ok := got["retired"]; ok || len(got) != 4 {
		t.Fatalf("a Team that left the Teams document must leave the catalog: %+v", got)
	}

	// With another repository, the commit and its skills go; the Teams stay.
	other, _, _ := restart(api.SkillMarketplaceRepository{URL: "https://git.example.invalid/other.git"})
	if document := catalogDocument(t, other); document.Commit != "" || len(document.Skills) != 0 || len(document.Invalid) != 0 ||
		!reflect.DeepEqual(document.Teams, previous.Teams) {
		t.Fatalf("catalog for another repository %+v", document)
	}
}

// Before the worker read a Teams document, it knows no Team: it publishes
// nothing and removes nothing.
func TestNothingChangesBeforeTheFirstTeamsDocument(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	f := newFixture(t, selection("research", uid, "pdf-tools"))
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf")})
	f.publish(t)
	f.teams = &TeamsSource{}
	f.worker.Teams = f.teams
	f.worker.Now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if retry := f.publish(t); !retry {
		t.Fatal("must retry until a Teams document is read")
	}
	if id, _, _ := f.publisher.ReadCurrent(uid); id == "" {
		t.Fatal("the releases must stay while no Teams document is known")
	}
	// An empty document is a known document: the Team left it. The grace
	// period starts now.
	f.setTeams()
	f.publish(t)
	if _, err := os.Stat(filepath.Join(f.publisher.Root, "teams", uid)); err != nil {
		t.Fatalf("the releases of a Team that left the document stay for the grace period: %v", err)
	}
	f.worker.Now = func() time.Time { return time.Now().Add(26 * time.Hour) }
	f.publish(t)
	if _, err := os.Stat(filepath.Join(f.publisher.Root, "teams", uid)); !os.IsNotExist(err) {
		t.Fatalf("the releases of a Team that left the document must go after the grace period: %v", err)
	}
}

func TestATeamThatLeavesTheDocumentLeavesTheCatalogAtOnce(t *testing.T) {
	research := selection("research", "00000000-0000-4000-8000-000000000001", "pdf-tools")
	support := selection("support", "00000000-0000-4000-8000-000000000002", "pdf-tools")
	f := newFixture(t, research, support)
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf")})
	f.publish(t)
	f.setTeams(support)
	f.publish(t)
	if teams := catalogTeams(t, f.catalog); len(teams) != 1 || teams[0].Name != "support" {
		t.Fatalf("the Team must leave the catalog: %+v", teams)
	}
	// Its releases stay until the grace period ends.
	if id, _, _ := f.publisher.ReadCurrent(research.UID); id == "" {
		t.Fatalf("the releases must stay for the grace period")
	}
}

// A selected skill that the repository dropped keeps its last published
// version. While that version or the current release cannot be read, the Team
// keeps its current release: a new release would silently leave the skill out.
func TestARetainedSkillWaitsForItsRelease(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	f := newFixture(t, selection("research", uid, "pdf-tools", "web"))
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf"), repoSkill("web", "ba9876543210", "web")})
	f.publish(t)
	first, _, err := f.publisher.ReadCurrent(uid)
	if err != nil || first == "" {
		t.Fatalf("first release %q %v", first, err)
	}
	// The repository drops web, whose published files cannot be read.
	f.store.set("c2", []skills.Skill{repoSkill("pdf-tools", "0123456789ac", "pdf2")})
	skillMD := filepath.Join(f.publisher.Root, "teams", uid, "releases", first, "web", "SKILL.md")
	if err := os.Rename(skillMD, skillMD+".away"); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(f.publisher.Root, "teams", uid, "CURRENT")
	blocked := func(step, wantCurrent string) {
		t.Helper()
		if retry := f.publish(t); !retry {
			t.Fatalf("%s: the publication must be tried again", step)
		}
		if data, err := os.ReadFile(current); err != nil || string(data) != wantCurrent {
			t.Fatalf("%s: CURRENT must stay: %q %v", step, data, err)
		}
		if state := catalogTeams(t, f.catalog)[0]; !strings.Contains(state.Error, "could not be read") || len(state.Retained) != 0 {
			t.Fatalf("%s: Team state %+v", step, state)
		}
	}
	blocked("retained files", first+"\n")
	// The current release itself cannot be read.
	if err := os.WriteFile(current, []byte("damaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked("CURRENT", "damaged\n")
	// Once storage is readable again, the next release keeps web.
	if err := os.WriteFile(current, []byte(first+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(skillMD+".away", skillMD); err != nil {
		t.Fatal(err)
	}
	if retry := f.publish(t); retry {
		t.Fatal("a readable release needs no retry")
	}
	_, release, err := f.publisher.ReadCurrent(uid)
	if err != nil || release == nil || release.ID == first {
		t.Fatalf("a new release must follow: %+v %v", release, err)
	}
	versions := map[string]string{}
	for _, skill := range release.Skills {
		versions[skill.Name] = skill.Version
	}
	if versions["web"] != "ba9876543210" || versions["pdf-tools"] != "0123456789ac" {
		t.Fatalf("release %+v", release.Skills)
	}
	if state := catalogTeams(t, f.catalog)[0]; state.Error != "" || len(state.Retained) != 1 {
		t.Fatalf("Team state %+v", state)
	}
}

// A Team whose selected skills are all in the repository needs nothing from
// its current release: a damaged CURRENT does not stop its next release.
func TestACompleteSelectionReplacesADamagedCURRENT(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	f := newFixture(t, selection("research", uid, "pdf-tools"))
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf")})
	f.publish(t)
	if err := os.WriteFile(filepath.Join(f.publisher.Root, "teams", uid, "CURRENT"), []byte("damaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.store.set("c2", []skills.Skill{repoSkill("pdf-tools", "0123456789ac", "pdf2")})
	if retry := f.publish(t); retry {
		t.Fatal("a complete selection must publish")
	}
	if _, release, err := f.publisher.ReadCurrent(uid); err != nil || release == nil || release.Skills[0].Version != "0123456789ac" {
		t.Fatalf("release %+v %v", release, err)
	}
}

// teamRelease is what an Agent of a Team copies: the current release, and the
// skill directories in it.
type teamRelease struct {
	id, published string
	versions      map[string]string
	dirs          []string
}

func readTeamRelease(t *testing.T, publisher *publish.Publisher, uid string) teamRelease {
	t.Helper()
	id, release, err := publisher.ReadCurrent(uid)
	if err != nil || release == nil {
		t.Fatalf("Team %s has no current release: %q %v", uid, id, err)
	}
	state := teamRelease{id: id, published: release.CreatedAt, versions: map[string]string{}}
	for _, skill := range release.Skills {
		state.versions[skill.Name] = skill.Version
	}
	entries, err := os.ReadDir(filepath.Join(publisher.Root, "teams", uid, "releases", id))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			state.dirs = append(state.dirs, entry.Name())
		}
	}
	return state
}

// Teams share the repository, not their releases: each Team's subtree holds
// only the skills that the Team selects, and a repository change publishes
// again only for the Teams that select a changed skill.
func TestTeamsArePublishedIndependently(t *testing.T) {
	clock := time.Now()
	research := selection("research", "00000000-0000-4000-8000-000000000001", "pdf-tools", "zh-notes")
	support := selection("support", "00000000-0000-4000-8000-000000000002", "zh-notes")
	f := newFixture(t, research, support)
	f.publisher.Now = func() time.Time { return clock }
	newCommit := func(commit, pdf, zh string) {
		t.Helper()
		f.store.set(commit, []skills.Skill{repoSkill("pdf-tools", pdf, "pdf "+pdf), repoSkill("zh-notes", zh, "zh "+zh),
			repoSkill("unselected", "cccccccccccc", "unselected")})
		clock = clock.Add(time.Minute)
		f.publish(t)
	}
	releaseCount := func(team skillmarketplace.TeamSelection) int {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(f.publisher.Root, "teams", team.UID, "releases"))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}

	newCommit("c1", "aaaaaaaaaaa1", "bbbbbbbbbbb1")
	first := map[string]teamRelease{"research": readTeamRelease(t, f.publisher, research.UID),
		"support": readTeamRelease(t, f.publisher, support.UID)}
	if !reflect.DeepEqual(first["research"].dirs, []string{"pdf-tools", "zh-notes"}) ||
		!reflect.DeepEqual(first["support"].dirs, []string{"zh-notes"}) {
		t.Fatalf("each release must hold only its Team's selection: %+v", first)
	}

	// Only research selects pdf-tools.
	newCommit("c2", "aaaaaaaaaaa2", "bbbbbbbbbbb1")
	second := map[string]teamRelease{"research": readTeamRelease(t, f.publisher, research.UID),
		"support": readTeamRelease(t, f.publisher, support.UID)}
	if second["research"].id == first["research"].id || second["research"].versions["pdf-tools"] != "aaaaaaaaaaa2" {
		t.Fatalf("research must get a release with the new pdf-tools: %+v", second["research"])
	}
	if !reflect.DeepEqual(second["support"], first["support"]) || releaseCount(support) != 1 {
		t.Fatalf("support must keep its release: %+v, want %+v", second["support"], first["support"])
	}

	// Both select zh-notes.
	newCommit("c3", "aaaaaaaaaaa2", "bbbbbbbbbbb2")
	third := map[string]teamRelease{"research": readTeamRelease(t, f.publisher, research.UID),
		"support": readTeamRelease(t, f.publisher, support.UID)}
	for name, release := range third {
		if release.id == second[name].id || release.versions["zh-notes"] != "bbbbbbbbbbb2" {
			t.Fatalf("%s must get a release with the new zh-notes: %+v", name, release)
		}
	}
	if !reflect.DeepEqual(third["support"].dirs, []string{"zh-notes"}) {
		t.Fatalf("support must still hold only zh-notes: %+v", third["support"])
	}

	// The catalog lists each Team once, with its own release.
	teams := catalogTeams(t, f.catalog)
	if len(teams) != 2 || teams[0].Name != "research" || teams[0].Release != third["research"].id ||
		teams[1].Name != "support" || teams[1].Release != third["support"].id {
		t.Fatalf("catalog Teams %+v", teams)
	}
}

// A Team created again under the name of a deleted Team is a new Team: it gets
// its own subtree, and the catalog lists it once. The deleted Team's releases
// stay until the grace period ends, and other Teams keep theirs.
func TestATeamCreatedAgainUnderItsNameIsANewTeam(t *testing.T) {
	const (
		researchUID = "00000000-0000-4000-8000-000000000001"
		deletedUID  = "00000000-0000-4000-8000-000000000002"
		createdUID  = "00000000-0000-4000-8000-000000000003"
	)
	f := newFixture(t, selection("research", researchUID, "pdf-tools"), selection("support", deletedUID, "zh-notes"))
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "aaaaaaaaaaa1", "pdf"), repoSkill("zh-notes", "bbbbbbbbbbb1", "zh")})
	f.publish(t)
	research := readTeamRelease(t, f.publisher, researchUID)
	f.setTeams(selection("research", researchUID, "pdf-tools"), selection("support", createdUID, "pdf-tools"))
	f.publish(t)
	teams := catalogTeams(t, f.catalog)
	if len(teams) != 2 || teams[1].Name != "support" || teams[1].UID != createdUID {
		t.Fatalf("the catalog must list the new Team once, without its predecessor: %+v", teams)
	}
	if created := readTeamRelease(t, f.publisher, createdUID); !reflect.DeepEqual(created.dirs, []string{"pdf-tools"}) {
		t.Fatalf("the new Team must get its own release: %+v", created)
	}
	if old := readTeamRelease(t, f.publisher, deletedUID); !reflect.DeepEqual(old.dirs, []string{"zh-notes"}) {
		t.Fatalf("the deleted Team's release must stay for the grace period: %+v", old)
	}

	// After the grace period, a pass removes only the deleted Team's subtree.
	f.worker.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	f.publish(t)
	if _, err := os.Stat(filepath.Join(f.publisher.Root, "teams", deletedUID)); !os.IsNotExist(err) {
		t.Fatalf("the deleted Team's subtree must be removed after the grace period: %v", err)
	}
	if got := readTeamRelease(t, f.publisher, researchUID); !reflect.DeepEqual(got, research) {
		t.Fatalf("another Team must keep its release: %+v, want %+v", got, research)
	}
	readTeamRelease(t, f.publisher, createdUID)
	if teams := catalogTeams(t, f.catalog); len(teams) != 2 || teams[1].UID != createdUID {
		t.Fatalf("catalog Teams after the grace period %+v", teams)
	}
}

func teamsConfigMap(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: skillmarketplace.TeamsConfigMap},
		Data: map[string]string{skillmarketplace.TeamsKey: data}}
}

func TestTeamsSourceReadsTheOperatorsDocument(t *testing.T) {
	ctx := context.Background()
	changed := make(chan struct{}, 1)
	kube := newClient(t)
	source := &TeamsSource{Reader: kube, Namespace: namespace, Changed: changed}
	if err := source.Poll(ctx); err == nil || !strings.Contains(err.Error(), "has not written") {
		t.Fatalf("a missing document: %v", err)
	}
	if _, known := source.Document(); known {
		t.Fatal("no document is known before the first read")
	}
	configMap := teamsConfigMap(`{"version":"v1","teams":[{"name":"research","uid":"u1","skills":["pdf-tools"]}]}`)
	if err := kube.Create(ctx, configMap); err != nil {
		t.Fatal(err)
	}
	if err := source.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	document, known := source.Document()
	if !known || len(document.Teams) != 1 || document.Teams[0].Skills[0] != "pdf-tools" || len(changed) != 1 {
		t.Fatalf("document %+v known %v, %d signals", document, known, len(changed))
	}
	<-changed
	// An unchanged ConfigMap is not read again.
	if err := source.Poll(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("unchanged: %v, %d signals", err, len(changed))
	}
	// A damaged document keeps the last one.
	configMap.Data[skillmarketplace.TeamsKey] = `{"version":"v1","teams":[{"name":"research"}]}`
	if err := kube.Update(ctx, configMap); err != nil {
		t.Fatal(err)
	}
	if err := source.Poll(ctx); err == nil {
		t.Fatal("a damaged document must be an error")
	}
	if document, _ := source.Document(); len(document.Teams) != 1 || document.Teams[0].UID != "u1" {
		t.Fatalf("the last document must stay: %+v", document)
	}
}

func TestCatalogIsWrittenOnlyWhenChanged(t *testing.T) {
	ctx := context.Background()
	kube := newClient(t)
	catalog := NewCatalog(kube, kube, namespace, api.SkillMarketplaceRepository{URL: "https://git.example.invalid/x.git"})
	catalog.SetSync("c1", []api.SkillMarketplaceSkill{{Name: "pdf-tools", Version: "0123456789ab"}}, nil)
	// The worker cannot create the catalog: the operator does.
	if err := catalog.Write(ctx); err == nil || !strings.Contains(err.Error(), "has not created the catalog") {
		t.Fatalf("a missing catalog ConfigMap: %v", err)
	}
	if err := kube.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace,
		Name: api.SkillMarketplaceCatalogConfigMap}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Write(ctx); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: api.SkillMarketplaceCatalogConfigMap}, stored); err != nil {
		t.Fatal(err)
	}
	version := stored.ResourceVersion
	if err := catalog.Write(ctx); err != nil {
		t.Fatal(err)
	}
	_ = kube.Get(ctx, client.ObjectKeyFromObject(stored), stored)
	if stored.ResourceVersion != version {
		t.Fatalf("an unchanged catalog must not be written again")
	}
	var document api.SkillMarketplaceCatalog
	if err := json.Unmarshal([]byte(stored.Data[api.SkillMarketplaceCatalogKey]), &document); err != nil || document.Commit != "c1" {
		t.Fatalf("catalog %+v %v", document, err)
	}
}

func TestCatalogFitsItsConfigMap(t *testing.T) {
	document := api.SkillMarketplaceCatalog{Version: "v1"}
	for i := 0; i < 2000; i++ {
		document.Skills = append(document.Skills, api.SkillMarketplaceSkill{Name: "s", Description: strings.Repeat("d", 600)})
	}
	raw, err := fitDocument(document)
	if err != nil || len(raw) > MaxCatalogBytes || !strings.Contains(string(raw), `"truncated":true`) {
		t.Fatalf("fitDocument: %d bytes, %v", len(raw), err)
	}
}
