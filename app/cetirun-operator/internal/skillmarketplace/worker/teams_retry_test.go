// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// unreadableSkill is a repository skill whose file cannot be read, so its
// publication fails.
func unreadableSkill(name, version string) skills.Skill {
	return skills.Skill{Name: name, Version: version, Description: name, Files: []skills.File{{RelPath: "SKILL.md",
		Open: func() (io.ReadCloser, error) { return nil, errors.New("synthetic read failure") }}}}
}

// A publication that keeps failing logs its error once, and one short line
// for each further attempt. While the input stays the same, the worker tries
// again after a growing wait, at most five minutes. A full pass, after a
// change, tries at once; the end of the failures is logged once.
func TestARepeatedPublicationFailureIsLoggedOnceAndTriedLessOften(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	f := newFixture(t, selection("research", uid, "pdf-tools"))
	clock := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	f.worker.Now = func() time.Time { return clock }
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	f.store.set("c1", []skills.Skill{unreadableSkill("pdf-tools", "0123456789ab")})

	if retry := f.worker.PublishAll(ctx); !retry {
		t.Fatal("a failed Team must be tried again")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"msg"="publish Team skills"`) || !strings.Contains(lines[0], "synthetic read failure") {
		t.Fatalf("the failure must be logged once, with its error: %q", lines)
	}
	if state := catalogTeams(t, f.catalog)[0]; !strings.Contains(state.Error, "publication failed") {
		t.Fatalf("Team state %+v", state)
	}
	try := func(after time.Duration, want bool) {
		t.Helper()
		clock = clock.Add(after)
		before := len(lines)
		f.worker.publishPending(ctx)
		if tried := len(lines) > before; tried != want {
			t.Fatalf("after %v: tried=%v, want %v (%q)", after, tried, want, lines[before:])
		}
		if want {
			line := lines[len(lines)-1]
			if !strings.Contains(line, `"msg"="publishing the Team skills failed again with the same error"`) || strings.Contains(line, `"error"=`) {
				t.Fatalf("a repeated failure must log one short line: %q", line)
			}
		}
	}
	// 10 s, then 20 s, 40 s, 80 s, 160 s, and at most 5 minutes.
	try(5*time.Second, false)
	try(5*time.Second, true)
	try(10*time.Second, false)
	try(10*time.Second, true)
	try(39*time.Second, false)
	try(time.Second, true)
	try(80*time.Second, true)
	try(160*time.Second, true)
	try(299*time.Second, false)
	try(time.Second, true)
	try(5*time.Minute, true)
	if wait := f.worker.nextRetry(); wait != 5*time.Minute {
		t.Fatalf("the next try waits %v, want at most five minutes", wait)
	}

	// A new commit is a change: the full pass tries at once.
	f.store.set("c2", []skills.Skill{repoSkill("pdf-tools", "0123456789ac", "pdf")})
	before := len(lines)
	if retry := f.worker.PublishAll(ctx); retry {
		t.Fatal("a published Team needs no retry")
	}
	if got := lines[before:]; len(got) != 2 || !strings.Contains(got[0], `"msg"="published Team skills"`) ||
		!strings.Contains(got[1], `"msg"="publishing the Team skills succeeds again"`) || !strings.Contains(got[1], `"failedAttempts"=`) {
		t.Fatalf("the end of the failures must be logged once: %q", got)
	}
	if state := catalogTeams(t, f.catalog)[0]; state.Error != "" || state.Release == "" {
		t.Fatalf("Team state %+v", state)
	}
	try(time.Hour, false)
}

// The error of a failed write names the random temporary file of its attempt,
// for example when CURRENT cannot be replaced. Such failures are the same
// failure: the log has the error once.
func TestAFailureThatNamesATemporaryFileIsStillTheSameFailure(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	f := newFixture(t, selection("research", uid, "pdf-tools"))
	clock := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	f.worker.Now = func() time.Time { return clock }
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf")})
	// CURRENT is a directory: each attempt fails to rename its temporary file onto it.
	current := filepath.Join(f.publisher.Root, "teams", uid, "CURRENT")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	f.worker.PublishAll(ctx)
	for range 3 {
		clock = clock.Add(maxRetryInterval)
		f.worker.publishPending(ctx)
	}
	errorsLogged := 0
	for _, line := range lines {
		if strings.Contains(line, `"msg"="publish Team skills"`) {
			errorsLogged++
		}
	}
	if len(lines) != 4 || errorsLogged != 1 || !strings.Contains(lines[0], "CURRENT.tmp-") {
		t.Fatalf("the failure must be logged once, and then with short lines: %q", lines)
	}
}

// The grace period of a Team that left the Teams document starts when the
// worker finds it gone. A Team that comes back within it keeps its releases,
// and with them a retained skill that the repository no longer has.
func TestATeamThatComesBackWithinTheGracePeriodKeepsItsRetainedSkills(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	team := selection("research", uid, "pdf-tools", "web")
	f := newFixture(t, team)
	f.store.set("c1", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf"), repoSkill("web", "ba9876543210", "web")})
	f.publish(t)
	f.store.set("c2", []skills.Skill{repoSkill("pdf-tools", "0123456789ab", "pdf")})
	f.publish(t)
	if state := catalogTeams(t, f.catalog)[0]; len(state.Retained) != 1 {
		t.Fatalf("test setup: web must be retained: %+v", state)
	}
	// The last publication was long ago when the Team leaves the document.
	clock := time.Now().Add(48 * time.Hour)
	f.worker.Now = func() time.Time { return clock }
	f.setTeams()
	f.publish(t)
	if _, err := os.Stat(filepath.Join(f.publisher.Root, "teams", uid)); err != nil {
		t.Fatalf("the Team's releases must stay for the grace period after it left: %v", err)
	}
	clock = clock.Add(5 * time.Minute)
	f.setTeams(team)
	f.publish(t)
	state := catalogTeams(t, f.catalog)[0]
	if len(state.Retained) != 1 || state.Retained[0] != "web" || len(state.Missing) != 0 {
		t.Fatalf("a Team that came back must keep its retained skill: %+v", state)
	}
	// Gone for longer than the grace period, its releases go.
	f.setTeams()
	f.publish(t)
	clock = clock.Add(61 * time.Minute)
	f.publish(t)
	if _, err := os.Stat(filepath.Join(f.publisher.Root, "teams", uid)); !os.IsNotExist(err) {
		t.Fatalf("after the grace period, the releases must go: %v", err)
	}
}
