// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gitsync

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func commitFiles(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err == git.ErrRepositoryNotExists {
		repo, err = git.PlainInit(dir, false)
	}
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := tree.Commit("update", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func TestResolveAndFetchLocalRepository(t *testing.T) {
	dir := t.TempDir()
	first := commitFiles(t, dir, map[string]string{
		"skills/pdf-tools/SKILL.md": "---\nname: pdf-tools\ndescription: PDF\n---\n",
		"README.md":                 "x",
	})
	source := Source{URL: dir, Auth: AuthNone}
	ctx := context.Background()
	ref, hash, err := source.Resolve(ctx)
	if err != nil || hash.String() != first {
		t.Fatalf("resolve: %v %v %v", ref, hash, err)
	}
	snapshot, err := source.Fetch(ctx, ref, testLimits)
	if err != nil || snapshot.Commit != first {
		t.Fatalf("fetch: %v", err)
	}
	found, problems, err := skills.Discover(snapshot, "skills", skills.DefaultLimits, nil)
	if err != nil || len(found) != 1 || len(problems) != 0 || found[0].Name != "pdf-tools" {
		t.Fatalf("discover: %+v %+v %v", found, problems, err)
	}
	second := commitFiles(t, dir, map[string]string{"skills/pdf-tools/notes.md": "more"})
	_, hash, err = source.Resolve(ctx)
	if err != nil || hash.String() != second {
		t.Fatalf("a new commit must resolve: %v %v", hash, err)
	}
	if _, err := source.Fetch(ctx, ref, Limits{MaxBytes: 4, MaxObjects: 100}); err == nil {
		t.Fatalf("a commit larger than the limit must be refused")
	}
	if _, _, err := (Source{URL: dir, Ref: "no-such-branch", Auth: AuthNone}).Resolve(ctx); err == nil {
		t.Fatalf("an unknown ref must fail")
	}
}

func TestValidateAndRedact(t *testing.T) {
	for _, tc := range []struct {
		source Source
		ok     bool
	}{
		{Source{URL: "https://git.example.invalid/skills.git", Auth: AuthToken}, true},
		{Source{URL: "https://user:pass@git.example.invalid/skills.git", Auth: AuthToken}, false},
		{Source{URL: "http://git.example.invalid/skills.git", Auth: AuthNone}, false},
		{Source{URL: "http://git.example.invalid/skills.git", Auth: AuthNone, AllowInsecureHTTP: true}, true},
		{Source{URL: "git@github.com:org/skills.git", Auth: AuthSSH}, true},
		{Source{URL: "ssh://git@github.com/org/skills.git", Auth: AuthSSH}, true},
		{Source{URL: "https://git.example.invalid/skills.git", Auth: "basic"}, false},
		{Source{URL: "https://git.example.invalid/skills.git", Auth: AuthNone, Ref: "a..b"}, false},
		// The worker logs the URL and the catalog shows it: no query or fragment.
		{Source{URL: "https://git.example.invalid/skills.git?token=synthetic", Auth: AuthToken}, false},
		{Source{URL: "https://git.example.invalid/skills.git?", Auth: AuthNone}, false},
		{Source{URL: "https://git.example.invalid/skills.git#synthetic", Auth: AuthNone}, false},
		{Source{URL: "https://git.example.invalid/skills.git#", Auth: AuthNone}, false},
		{Source{URL: "ssh://git@github.com/org/skills.git?x=1", Auth: AuthSSH}, false},
	} {
		if err := tc.source.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.source, err)
		}
	}
	for raw, want := range map[string]string{
		"https://user:pass@git.example.invalid/x.git":         "https://git.example.invalid/x.git",
		"git@github.com:org/skills.git":                       "github.com:org/skills.git",
		"https://git.example.invalid/x.git?token=synthetic":   "https://git.example.invalid/x.git",
		"https://git.example.invalid/x.git#synthetic":         "https://git.example.invalid/x.git",
		"https://u:p@git.example.invalid/x.git?a=b#synthetic": "https://git.example.invalid/x.git",
	} {
		if got := Redacted(raw); got != want {
			t.Errorf("Redacted(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The remote advertises an annotated tag as the tag object, and its commit
// under the tag's name with "^{}". Resolve returns the commit, which the
// worker compares with the commit that it read.
func TestResolveReturnsTheCommitOfAnAnnotatedTag(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills")
	commit := commitFiles(t, dir, map[string]string{"skills/pdf-tools/SKILL.md": "---\nname: pdf-tools\ndescription: PDF\n---\n"})
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := repo.CreateTag("approved", plumbing.NewHash(commit), &git.CreateTagOptions{
		Message: "approved skills", Tagger: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if tag.Hash().String() == commit {
		t.Fatal("test setup: the tag must be an annotated tag object")
	}
	if _, err := repo.CreateTag("light", plumbing.NewHash(commit), nil); err != nil {
		t.Fatal(err)
	}
	server, _ := gitHTTPServer(t, root, false)
	ctx := context.Background()
	for _, ref := range []string{"approved", "refs/tags/approved", "light"} {
		source := Source{URL: server.URL + "/skills", Ref: ref, Auth: AuthNone, AllowInsecureHTTP: true}
		name, hash, err := source.Resolve(ctx)
		if err != nil || hash.String() != commit {
			t.Fatalf("%s: resolved %s %s %v, want the commit %s", ref, name, hash, err, commit)
		}
		snapshot, err := source.Fetch(ctx, name, testLimits)
		if err != nil || snapshot.Commit != commit {
			t.Fatalf("%s: fetch: %v", ref, err)
		}
	}
}

// azureDevOpsLikeServer serves the repositories below root over smart HTTP
// with git http-backend. Like Azure DevOps, it refuses an upload-pack request
// whose first want line does not ask for multi_ack (HTTP 400). It counts the
// upload-pack requests that it passes on.
func azureDevOpsLikeServer(t *testing.T, root string) (*httptest.Server, *atomic.Int32) {
	return gitHTTPServer(t, root, true)
}

// gitHTTPServer serves the repositories below root over smart HTTP with git
// http-backend, and counts the upload-pack requests that it passes on.
// Unlike a local path, which go-git serves with a git-upload-pack child
// process, a server connection can be closed while the server still sends.
func gitHTTPServer(t *testing.T, root string, requireMultiACK bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not installed")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	cgiHandler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	served := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// The first pkt-line: four hexadecimal digits of length, then the
			// want line with the requested capabilities.
			first := ""
			if size, err := strconv.ParseUint(string(body[:min(4, len(body))]), 16, 16); err == nil && int(size) <= len(body) && size >= 4 {
				first = string(body[4:size])
			}
			if requireMultiACK && !strings.Contains(first, " multi_ack") {
				http.Error(w, "multi_ack or multi_ack_detailed is required", http.StatusBadRequest)
				return
			}
			served.Add(1)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, served
}

func TestFetchWorksWithAServerThatRequiresMultiACK(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills")
	first := commitFiles(t, dir, map[string]string{"skills/pdf-tools/SKILL.md": "---\nname: pdf-tools\ndescription: PDF\n---\n"})
	commitFiles(t, dir, map[string]string{"skills/pdf-tools/notes.md": "more"})
	server, served := azureDevOpsLikeServer(t, root)
	source := Source{URL: server.URL + "/skills", Auth: AuthNone, AllowInsecureHTTP: true}
	ctx := context.Background()
	ref, hash, err := source.Resolve(ctx)
	if err != nil || hash.String() == first {
		t.Fatalf("resolve: %v %v %v", ref, hash, err)
	}
	snapshot, err := source.Fetch(ctx, ref, testLimits)
	if err != nil || snapshot.Commit != hash.String() || served.Load() != 1 {
		t.Fatalf("fetch through a server that requires multi_ack: %v (upload-pack requests: %d)", err, served.Load())
	}
	found, problems, err := skills.Discover(snapshot, "skills", skills.DefaultLimits, nil)
	if err != nil || len(found) != 1 || len(problems) != 0 || len(found[0].Files) != 2 {
		t.Fatalf("discover: %+v %+v %v", found, problems, err)
	}
	// go-git's own default leaves multi_ack out, and such a server refuses it.
	saved := transport.UnsupportedCapabilities
	transport.UnsupportedCapabilities = []capability.Capability{capability.MultiACK, capability.MultiACKDetailed, capability.ThinPack}
	defer func() { transport.UnsupportedCapabilities = saved }()
	if _, err := source.Fetch(ctx, ref, testLimits); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("the test server must refuse a request without multi_ack: %v", err)
	}
}

func TestTokenIsReadFromTheCredentialsDirectory(t *testing.T) {
	dir := t.TempDir()
	source := Source{URL: "https://git.example.invalid/x.git", Auth: AuthToken, CredentialsDir: dir}
	if _, err := source.auth(); err == nil {
		t.Fatal("a missing token must fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := source.auth()
	if err != nil || auth.String() == "" {
		t.Fatalf("token auth: %v", err)
	}
	if _, err := (Source{URL: "git@h:x", Auth: AuthSSH, CredentialsDir: dir}).auth(); err == nil {
		t.Fatal("SSH without a key and known_hosts must fail")
	}
}

// commitRawTree commits files without go-git's worktree, which refuses the
// unsafe names that a crafted repository can still contain.
func commitRawTree(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	store := func(object interface {
		Encode(plumbing.EncodedObject) error
	}, kind plumbing.ObjectType) plumbing.Hash {
		encoded := repo.Storer.NewEncodedObject()
		encoded.SetType(kind)
		if err := object.Encode(encoded); err != nil {
			t.Fatal(err)
		}
		hash, err := repo.Storer.SetEncodedObject(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	var build func(prefix string) plumbing.Hash
	build = func(prefix string) plumbing.Hash {
		entries := map[string]object.TreeEntry{}
		for name, content := range files {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			part, _, nested := strings.Cut(strings.TrimPrefix(name, prefix), "/")
			if nested {
				if _, ok := entries[part]; !ok {
					entries[part] = object.TreeEntry{Name: part, Mode: filemode.Dir, Hash: build(prefix + part + "/")}
				}
				continue
			}
			blob := repo.Storer.NewEncodedObject()
			blob.SetType(plumbing.BlobObject)
			writer, _ := blob.Writer()
			_, _ = writer.Write([]byte(content))
			_ = writer.Close()
			hash, err := repo.Storer.SetEncodedObject(blob)
			if err != nil {
				t.Fatal(err)
			}
			entries[part] = object.TreeEntry{Name: part, Mode: filemode.Regular, Hash: hash}
		}
		tree := &object.Tree{}
		for _, entry := range entries {
			tree.Entries = append(tree.Entries, entry)
		}
		sort.Slice(tree.Entries, func(i, j int) bool {
			key := func(e object.TreeEntry) string {
				if e.Mode == filemode.Dir {
					return e.Name + "/"
				}
				return e.Name
			}
			return key(tree.Entries[i]) < key(tree.Entries[j])
		})
		return store(tree, plumbing.TreeObject)
	}
	signature := object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}
	commit := store(&object.Commit{Author: signature, Committer: signature, Message: "raw", TreeHash: build("")}, plumbing.CommitObject)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.Master, commit)); err != nil {
		t.Fatal(err)
	}
	return commit.String()
}

func TestAnUnsafeNameRejectsOnlyItsSkill(t *testing.T) {
	dir := t.TempDir()
	commit := commitRawTree(t, dir, map[string]string{
		"skills/good/SKILL.md":       "---\nname: good\ndescription: Valid\n---\n",
		"skills/control/SKILL.md":    "---\nname: control\ndescription: Unsafe file name\n---\n",
		"skills/control/bad\x01name": "x",
		"skills/dot-git/SKILL.md":    "---\nname: dot-git\ndescription: Holds a .git directory\n---\n",
		"skills/dot-git/.git/config": "[core]\n",
		"docs/bad\x02name.md":        "outside every skill",
	})
	source := Source{URL: dir, Auth: AuthNone}
	ctx := context.Background()
	ref, _, err := source.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Fetch(ctx, ref, testLimits)
	if err != nil || snapshot.Commit != commit {
		t.Fatalf("an unsafe name must not stop the sync: %v", err)
	}
	found, problems, err := skills.Discover(snapshot, "skills", skills.DefaultLimits, nil)
	if err != nil || len(found) != 1 || found[0].Name != "good" {
		t.Fatalf("discover: %+v %v", found, err)
	}
	rejected := map[string]string{}
	for _, problem := range problems {
		rejected[problem.Path] = problem.Reason
	}
	if len(rejected) != 2 || !strings.Contains(rejected["skills/control"], "not allowed") ||
		!strings.Contains(rejected["skills/dot-git"], "not allowed") {
		t.Fatalf("problems: %+v", problems)
	}
}
